package handler

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

// 允许写入 site_settings 的公开安全键（无密钥）
var publicSettingKeys = map[string]bool{
	"brand_name": true,
	"brand_sub":  true,
	"skin":       true,
	"theme_mode": true,
}

// 密钥类键：只接受写入，读出脱敏
var secretSettingKeys = map[string]bool{
	"card_api_base":    false,
	"telegram_token":   true,
	"telegram_chat_id": false,
	// agent_swap handled specially (hash)
}

// 站点未设置时的默认外观：卡台新版（zovo）+ 夜间，与前端 theme.ts / index.html 一致。
// 已在后台选过皮肤的站点保持原样（存了 skin 设置项就不走默认）。
const (
	defaultSiteSkin  = "zovo"
	defaultThemeMode = "dark"
)

// PublicSiteConfig GET /api/v1/public/site — 用户端拉品牌/皮肤（无鉴权）
func PublicSiteConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"installed":  db.IsInstalled(),
		"brand_name": settingOr("brand_name", "Best AI"),
		"brand_sub":  settingOr("brand_sub", "Account Upgrade Service"),
		"skin":       settingOr("skin", defaultSiteSkin),
		"theme_mode": settingOr("theme_mode", defaultThemeMode),
	})
}

// AdminGetSettings GET /api/v1/admin/settings
func AdminGetSettings(c *gin.Context) {
	out := gin.H{
		"brand_name": settingOr("brand_name", "Best AI"),
		"brand_sub":  settingOr("brand_sub", "Account Upgrade Service"),
		"skin":       settingOr("skin", defaultSiteSkin),
		"theme_mode": settingOr("theme_mode", defaultThemeMode),
	}
	// 非密钥可读
	for k, isSecret := range secretSettingKeys {
		v, _ := db.GetSetting(k)
		if isSecret {
			out[k+"_configured"] = strings.TrimSpace(v) != ""
			out[k+"_hint"] = maskSecret(v)
		} else {
			out[k] = v
		}
	}
	out["agent_swap_password_configured"] = agentSwapPasswordConfigured()
	c.JSON(http.StatusOK, out)
}

type adminSettingsBody struct {
	BrandName      *string `json:"brand_name"`
	BrandSub       *string `json:"brand_sub"`
	Skin           *string `json:"skin"`
	ThemeMode      *string `json:"theme_mode"`
	CardAPIBase    *string `json:"card_api_base"`
	CardAPIKey     *string `json:"card_api_key"`
	TelegramToken  *string `json:"telegram_token"`
	TelegramChatID *string `json:"telegram_chat_id"`
	WebhookSecret  *string `json:"webhook_secret"`
	// 代理失败换码密码（明文一次写入，存 bcrypt；空=不改）
	AgentSwapPassword *string `json:"agent_swap_password"`
}

// AdminPutSettings PUT /api/v1/admin/settings
func AdminPutSettings(c *gin.Context) {
	var body adminSettingsBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	if (body.CardAPIKey != nil && strings.TrimSpace(*body.CardAPIKey) != "") || (body.WebhookSecret != nil && strings.TrimSpace(*body.WebhookSecret) != "") {
		c.JSON(400, gin.H{"error": "请在卡台接入的账号列表中配置密钥"})
		return
	}

	if body.CardAPIBase != nil {
		cardAccountConfigMu.Lock()
		defer cardAccountConfigMu.Unlock()
		v := strings.TrimRight(strings.TrimSpace(*body.CardAPIBase), "/")
		v = strings.TrimSuffix(strings.TrimSuffix(v, "/openapi/v1"), "/openapi")
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) {
			c.JSON(400, gin.H{"error": "请填写有效的 HTTPS 卡台根地址"})
			return
		}
		accounts, err := db.ListCardAccounts()
		if err != nil {
			c.JSON(500, gin.H{"error": "读取账号失败"})
			return
		}
		for _, a := range accounts {
			if a.APIKey != "" && v != cardplatform.LoadConfig().SiteBase {
				c.JSON(409, gin.H{"error": "已有账号，不能更换卡台站点以免旧码失效"})
				return
			}
		}
		body.CardAPIBase = &v
	}

	setIf := func(key string, p *string, max int) error {
		if p == nil {
			return nil
		}
		v := strings.TrimSpace(*p)
		if max > 0 && len(v) > max {
			v = v[:max]
		}
		// 空字符串对密钥类表示「不修改」；若要清空用 " " 不支持清空密钥防误操作
		if v == "" && secretSettingKeys[key] {
			return nil
		}
		return db.SetSetting(key, v)
	}

	allowedSkins := map[string]bool{
		"best-ai": true,
		"zovo":    true, "terracotta": true, "ocean": true, "cyber": true, "forest": true, "violet": true,
		"slate": true, "rose": true, "ember": true, "noir": true, "paper": true,
	}
	if body.Skin != nil {
		s := strings.TrimSpace(*body.Skin)
		if !allowedSkins[s] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skin"})
			return
		}
		_ = db.SetSetting("skin", s)
	}
	if body.ThemeMode != nil {
		m := strings.TrimSpace(*body.ThemeMode)
		if m != "light" && m != "dark" && m != "auto" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid theme_mode"})
			return
		}
		_ = db.SetSetting("theme_mode", m)
	}
	_ = setIf("brand_name", body.BrandName, 40)
	_ = setIf("brand_sub", body.BrandSub, 80)
	if err := setIf("card_api_base", body.CardAPIBase, 0); err != nil {
		c.JSON(500, gin.H{"error": "卡台站点保存失败"})
		return
	}
	_ = setIf("telegram_token", body.TelegramToken, 200)
	_ = setIf("telegram_chat_id", body.TelegramChatID, 64)
	if body.AgentSwapPassword != nil {
		pw := strings.TrimSpace(*body.AgentSwapPassword)
		if pw != "" {
			if err := setAgentSwapPassword(pw); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
	}

	auditAdmin(c, "update_settings", "site settings")
	AdminGetSettings(c)
}

func settingOr(key, def string) string {
	v, _ := db.GetSetting(key)
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func maskSecret(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) <= 4 {
		return "****"
	}
	return "****" + v[len(v)-4:]
}
