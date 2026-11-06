package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

// CardPlatformWebhook POST /api/v1/webhooks/cardplatform
// 卡台开发者页配置的回调地址指向此处。
// 验签：X-Signature = hex(HMAC-SHA256(webhook_secret, raw body))
// 文档：zovocard-openapi-zh.md §7
func CardPlatformWebhook(c *gin.Context) {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if err != nil {
		c.Status(http.StatusRequestEntityTooLarge)
		return
	}
	secrets, err := db.WebhookSecrets()
	if err != nil {
		c.Status(503)
		return
	}
	got, err := hex.DecodeString(strings.TrimSpace(c.GetHeader("X-Signature")))
	if err != nil || len(got) != sha256.Size {
		c.Status(401)
		return
	}
	var accountID int64
	matches := 0
	for secret, id := range secrets {
		if secret == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(raw)
		if subtle.ConstantTimeCompare(got, mac.Sum(nil)) == 1 {
			accountID = id
			matches++
		}
	}
	if matches != 1 {
		c.Status(401)
		return
	}
	var payload map[string]interface{}
	if json.Unmarshal(raw, &payload) != nil || payload == nil {
		c.Status(400)
		return
	}
	eventType := strAny(payload["event"])
	if eventType == "" {
		eventType = strAny(payload["type"])
	}
	if eventType == "" {
		c.Status(400)
		return
	}
	idem := fmt.Sprintf("%d|%s", accountID, webhookIdemKey(payload, eventType))
	// Persist first; failed effects remain pending and upstream retries replay them.
	_, err = db.DB.Exec(`INSERT INTO webhook_events(account_id,event_type,idem_key,payload) VALUES(?,?,?,?) ON CONFLICT(idem_key) DO NOTHING`, accountID, eventType, idem, string(raw))
	if err != nil {
		log.Printf("webhook store: %v", err)
		c.Status(503)
		return
	}
	var processed bool
	var storedPayload string
	if err = db.DB.QueryRow(`SELECT processed_at IS NOT NULL,payload FROM webhook_events WHERE idem_key=?`, idem).Scan(&processed, &storedPayload); err != nil {
		c.Status(503)
		return
	}
	// Replays must use the payload that was durably accepted first. A reused
	// event ID cannot substitute a different CDK/order after a failed attempt.
	payload = nil
	if json.Unmarshal([]byte(storedPayload), &payload) != nil {
		c.Status(503)
		return
	}
	if processed {
		c.Status(200)
		return
	}
	if strings.HasPrefix(strings.ToLower(eventType), "gpt_direct.") {
		if err = applyCDKStatusFromWebhook(accountID, payload, eventType); err == nil {
			err = observeFromWebhookPayload(accountID, payload)
		}
		if err != nil {
			log.Printf("webhook effects account=%d: %v", accountID, err)
			c.Status(503)
			return
		}
	}
	if _, err = db.DB.Exec(`UPDATE webhook_events SET processed_at=CURRENT_TIMESTAMP WHERE idem_key=?`, idem); err != nil {
		c.Status(503)
		return
	}
	c.Status(200)
}

func applyCDKStatusFromWebhook(accountID int64, payload map[string]interface{}, eventType string) error {
	cdkID := anyToInt64(payload["cdk_id"])
	if cdkID <= 0 {
		return nil
	}
	st := strings.ToLower(strings.TrimSpace(strAny(payload["cdk_status"])))
	if st == "" && strings.Contains(strings.ToLower(eventType), "completed") {
		st = "consumed"
	}
	switch st {
	case "unused", "reserved", "consumed", "disabled", "frozen":
	default:
		return nil
	}
	// A delayed reservation/failure must never undo a terminal status. Predicate
	// and update are one SQLite operation, including under concurrent callbacks.
	_, err := db.DB.Exec(`UPDATE cardplatform_cdk_codes SET status=? WHERE account_id=? AND upstream_id=? AND NOT (? IN ('unused','reserved') AND status IN ('consumed','disabled'))`, st, accountID, cdkID, st)
	return err
}

func webhookIdemKey(p map[string]interface{}, eventType string) string {
	str := func(k string) string {
		if v, ok := p[k]; ok {
			switch t := v.(type) {
			case string:
				return t
			case float64:
				return strings.TrimSuffix(strings.TrimSuffix(jsonNumber(t), ".0"), ".")
			}
		}
		return ""
	}
	if id := str("event_id"); id != "" {
		return eventType + "|event|" + id
	}
	switch eventType {
	case "card_transaction":
		if str("auth_id") == "" {
			break
		}
		return strings.Join([]string{eventType, str("auth_id"), str("type"), str("status")}, "|")
	case "card_operation":
		if str("operation_id") == "" {
			break
		}
		return strings.Join([]string{eventType, str("operation"), str("operation_id"), str("status")}, "|")
	case "gpt_direct.completed":
		if id := str("order_id"); id != "" {
			return "gpt_direct.completed|order|" + id
		}
		if id := str("client_request_id"); id != "" {
			return "gpt_direct.completed|client|" + id
		}
	}
	// fallback
	canonical, _ := json.Marshal(p)
	h := sha256.Sum256(canonical)
	return hex.EncodeToString(h[:16])
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// AdminListWebhooks GET /api/v1/admin/webhooks/events
func AdminListWebhooks(c *gin.Context) {
	limit := 50
	rows, err := db.ListWebhookEvents(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	// 脱敏：不返回完整 card_number
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"id":         r.ID,
			"account_id": r.AccountID,
			"event_type": r.EventType,
			"idem_key":   r.IdemKey,
			"created_at": r.CreatedAt,
			"payload":    sanitizeWebhookPayload(r.Payload),
		})
	}
	urlHint := ""
	if host := c.Request.Host; host != "" {
		scheme := "https"
		if c.Request.TLS == nil && !strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
			// 生产经 Caddy 会带 proto
			if p := c.GetHeader("X-Forwarded-Proto"); p != "" {
				scheme = p
			}
		} else if strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		urlHint = scheme + "://" + host + "/api/v1/webhooks/cardplatform"
	}
	c.JSON(http.StatusOK, gin.H{
		"events":      out,
		"webhook_url": urlHint,
	})
}

func sanitizeWebhookPayload(raw string) interface{} {
	var m map[string]interface{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return raw
	}
	if cn, ok := m["card_number"].(string); ok && len(cn) > 4 {
		m["card_number"] = "****" + cn[len(cn)-4:]
	}
	return m
}
