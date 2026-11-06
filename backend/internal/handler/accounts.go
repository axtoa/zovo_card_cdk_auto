package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

// RequireCardAccount freezes the chosen account in this request. There is no
// process-wide selection, and concurrent tabs cannot change one another's key.
func RequireCardAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.GetHeader("X-Card-Account-ID"), 10, 64)
	if err != nil || id <= 0 {
		c.AbortWithStatusJSON(400, gin.H{"error": "请先选择卡台账号", "error_code": "account_required"})
		return
	}
	a, err := db.GetCardAccount(id)
	if err != nil {
		c.AbortWithStatusJSON(400, gin.H{"error": "卡台账号不存在"})
		return
	}
	c.Request = c.Request.WithContext(cardplatform.WithAccount(c.Request.Context(), a))
	c.Next()
}
func AdminListCardAccounts(c *gin.Context) {
	accounts, err := db.ListCardAccounts()
	if err != nil {
		c.JSON(500, gin.H{"error": "读取账号失败"})
		return
	}
	out := []gin.H{}
	for _, a := range accounts {
		out = append(out, gin.H{"id": a.ID, "name": a.Name, "api_key_configured": a.APIKey != "", "api_key_hint": maskSecret(a.APIKey), "webhook_secret_configured": a.WebhookSecret != "", "webhook_secret_hint": maskSecret(a.WebhookSecret), "created_at": a.CreatedAt})
	}
	c.JSON(200, gin.H{"accounts": out, "site_base": cardplatform.LoadConfig().SiteBase, "webhook_path": "/api/v1/webhooks/cardplatform"})
}

var cardAccountConfigMu sync.Mutex

func AdminSaveCardAccount(c *gin.Context) {
	cardAccountConfigMu.Lock()
	defer cardAccountConfigMu.Unlock()
	var body struct {
		Name          string `json:"name"`
		APIKey        string `json:"api_key"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if c.ShouldBindJSON(&body) != nil {
		c.JSON(400, gin.H{"error": "请求格式错误"})
		return
	}
	var id int64
	if p := c.Param("id"); p != "" {
		var err error
		id, err = strconv.ParseInt(p, 10, 64)
		if err != nil || id <= 0 {
			c.JSON(400, gin.H{"error": "账号编号无效"})
			return
		}
	}
	if len(body.APIKey) > 4096 || len(body.WebhookSecret) > 4096 {
		c.JSON(400, gin.H{"error": "密钥过长"})
		return
	}

	if strings.TrimSpace(body.APIKey) != "" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		_, err := cardplatform.NewForAccount(db.CardAccount{APIKey: strings.TrimSpace(body.APIKey)}).GetBalance(ctx)
		cancel()
		if err != nil {
			writeCardErr(c, err)
			return
		}
	}
	id, err := db.SaveCardAccount(db.CardAccount{ID: id, Name: body.Name, APIKey: body.APIKey, WebhookSecret: body.WebhookSecret})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	auditAdmin(c, "save_card_account", fmt.Sprintf("account_id=%d", id))
	// Initial shared-rule sync is explicit and reports remote failures.
	cardRulesMu.Lock()
	defer cardRulesMu.Unlock()
	a, readErr := db.GetCardAccount(id)
	if readErr != nil {
		c.JSON(200, gin.H{"ok": true, "id": id, "rules_synced": false, "rules_error": "账号已保存，但无法读取账号以同步规则，请重试同步"})
		return
	}
	ctx, cancel := context.WithTimeout(cardplatform.WithAccount(c.Request.Context(), a), 30*time.Second)
	defer cancel()
	syncErr := syncAccountDirectCardRules(ctx)
	note := ""
	if syncErr != nil {
		note = syncErr.Error()
	}
	c.JSON(200, gin.H{"ok": true, "id": id, "rules_synced": syncErr == nil, "rules_error": note})
}
func publicBodyAccountID(body map[string]any) int64 {
	// Redemption token is authoritative; a caller cannot inject another code to
	// borrow that account's blacklist.
	token := strings.TrimSpace(str(body["redemption_token"]))
	if token == "" {
		return 0
	}
	code, err := db.FindCodeByRedemptionToken(token)
	if err != nil {
		return 0
	}
	return db.AccountIDForCode(code)
}
