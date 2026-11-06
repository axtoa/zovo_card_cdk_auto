package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/db"
	"golang.org/x/crypto/bcrypt"
)

const agentSwapPasswordKey = "agent_swap_password_hash"

// 简易 IP 限流：每 IP 每小时最多 20 次换码尝试
var (
	swapRateMu   sync.Mutex
	swapRateHits = map[string][]time.Time{}
)

func swapRateAllow(ip string) bool {
	swapRateMu.Lock()
	defer swapRateMu.Unlock()
	now := time.Now()
	cut := now.Add(-time.Hour)
	hits := swapRateHits[ip]
	kept := hits[:0]
	for _, t := range hits {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= 20 {
		swapRateHits[ip] = kept
		return false
	}
	swapRateHits[ip] = append(kept, now)
	return true
}

func setAgentSwapPassword(plain string) error {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return db.DeleteSetting(agentSwapPasswordKey)
	}
	if len(plain) < 6 {
		return errPasswordTooShort
	}
	hash, err := db.HashAdminPassword(plain)
	if err != nil {
		return err
	}
	return db.SetSetting(agentSwapPasswordKey, hash)
}

var errPasswordTooShort = &simpleError{"代理换码密码至少 6 位"}

type simpleError struct{ s string }

func (e *simpleError) Error() string { return e.s }

func agentSwapPasswordConfigured() bool {
	v, _ := db.GetSetting(agentSwapPasswordKey)
	return strings.TrimSpace(v) != ""
}

func verifyAgentSwapPassword(plain string) bool {
	stored, _ := db.GetSetting(agentSwapPasswordKey)
	stored = strings.TrimSpace(stored)
	if stored == "" || strings.TrimSpace(plain) == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(stored), []byte(plain)) == nil
}

// orderLooksUnpaid 订单终态失败且无扣款证据。
func orderLooksUnpaid(status, stage string, finalMinor int64, events []map[string]any) bool {
	st := strings.ToLower(strings.TrimSpace(status))
	// 成功/进行中不可换
	switch st {
	case "completed", "success", "paid":
		return false
	case "running", "queued", "dispatching", "pending", "awaiting_card", "funding_pending", "card_open_review":
		return false
	}
	// 允许失败类终态
	okFail := st == "failed_precharge" || st == "declined" || st == "cancelled" || st == "failed"
	if !okFail {
		// 有些上游用 stage 表达失败
		sg := strings.ToLower(stage)
		if strings.Contains(sg, "failed") || strings.Contains(sg, "decline") || strings.Contains(sg, "unpaid") {
			okFail = true
		}
	}
	if !okFail {
		return false
	}
	if finalMinor > 0 {
		return false
	}
	for _, ev := range events {
		pf := strings.ToLower(strAny(ev["payment_fact"]))
		if pf == "true" || pf == "1" || pf == "charged" || pf == "paid" {
			return false
		}
		cat := strings.ToLower(strAny(ev["category"]))
		if cat == "charged" || cat == "payment" {
			// 再看 message
			msg := strings.ToLower(strAny(ev["public_message"]) + " " + strAny(ev["event"]))
			if strings.Contains(msg, "paid") || strings.Contains(msg, "扣款") || strings.Contains(msg, "charged") {
				return false
			}
		}
	}
	return true
}

// PublicAgentCDKExchange POST /api/v1/public/cdk/exchange
// body: { password, code }  校验代理密码后，未扣款失败的 CDK 换一张同套餐新码。

func PublicAgentCDKExchange(c *gin.Context) {
	ip := c.ClientIP()
	if !swapRateAllow(ip) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后再试"})
		return
	}
	if !agentSwapPasswordConfigured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "管理员尚未启用代理换码功能"})
		return
	}
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if !verifyAgentSwapPassword(req.Password) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "密码错误"})
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) < 10 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入完整卡密"})
		return
	}
	codeHash := db.HashCDKCode(code)
	pending, pendingErr := db.AgentExchangePendingOrDone(codeHash)
	if pendingErr != nil {
		c.JSON(503, gin.H{"error": "无法核对补发记录，未执行补发"})
		return
	}
	if pending {
		c.JSON(http.StatusConflict, gin.H{"error": "该卡密已提交过补发，不能重复购买。若未收到新码，请联系管理员核对原账号的补发记录"})
		return
	}

	upstreamID, plan, prefix, localStatus, ok := db.LookupStoredCDKByCode(code)
	if !ok || upstreamID <= 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "本站未找到该完整卡密（仅支持本站发过的码）"})
		return
	}
	plan = strings.ToLower(strings.TrimSpace(plan))
	if plan == "" {
		c.JSON(409, gin.H{"error": "无法确认原套餐，未执行补发，请联系管理员"})
		return
	}

	accountID := db.AccountIDForCode(code)
	account, accountErr := db.GetCardAccount(accountID)
	if accountErr != nil || accountID <= 0 {
		c.JSON(409, gin.H{"error": "无法确认卡密所属账号，请联系管理员"})
		return
	}
	c.Request = c.Request.WithContext(cardplatform.WithAccount(c.Request.Context(), account))
	cli := cardplatform.NewForAccount(account)
	// 查该 CDK 的订单（卡台 OpenAPI）
	raw, err := cli.ListCDKOrdersQuery(c.Request.Context(), cardplatform.CDKOrderListQuery{
		Page: 1, PageSize: 20, CDKID: upstreamID,
	})
	if err != nil {
		writeCardErr(c, err)
		return
	}
	list, total := parseCDKOrderList(raw)
	if total == 0 || len(list) == 0 {
		// 从未进兑换：不允许「白嫖」新码
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "该卡密尚无兑换失败记录，不能换新码",
			"hint":  "仅限充值失败且未扣款的卡密",
		})
		return
	}

	if total > len(list) {
		c.JSON(409, gin.H{"error": "原卡密订单较多，无法完整确认是否有成功扣款，请联系管理员核对"})
		return
	}
	// 取最近一单
	latest := list[0]
	orderID := int64(anyToInt64(latest["order_id"]))
	if orderID == 0 {
		orderID = int64(anyToInt64(latest["id"]))
	}
	status := strings.ToLower(strAny(latest["status"]))
	stage := strings.ToLower(strAny(latest["stage"]))
	finalMinor := anyToInt64(latest["final_amount_minor"])

	if orderID <= 0 {
		c.JSON(409, gin.H{"error": "无法确认原订单编号，未执行补发"})
		return
	}
	// 拉详情看 events 的 payment_fact
	var events []map[string]any
	if orderID > 0 {
		detailRaw, detailErr := cli.GetCDKOrder(c.Request.Context(), strconv.FormatInt(orderID, 10))
		if detailErr != nil {
			c.JSON(503, gin.H{"error": "暂时无法核对原订单扣款记录，未执行补发，请稍后重试"})
			return
		}
		{
			if evs := extractOrderEvents(detailRaw); len(evs) > 0 {
				events = evs
			}
			// 详情里的 order 覆盖
			detail := extractOrderMap(detailRaw)
			if detail == nil || strings.TrimSpace(strAny(detail["status"])) == "" {
				c.JSON(503, gin.H{"error": "卡台订单详情不完整，未执行补发"})
				return
			}
			detailID := anyToInt64(detail["id"])
			if detailID == 0 {
				detailID = anyToInt64(detail["order_id"])
			}
			if detailID != orderID || (anyToInt64(detail["cdk_id"]) > 0 && anyToInt64(detail["cdk_id"]) != upstreamID) {
				c.JSON(503, gin.H{"error": "卡台订单详情与原卡密不匹配，未执行补发"})
				return
			}
			if m := detail; m != nil {
				if s := strAny(m["status"]); s != "" {
					status = strings.ToLower(s)
				}
				if s := strAny(m["stage"]); s != "" {
					stage = strings.ToLower(s)
				}
				if n := anyToInt64(m["final_amount_minor"]); n > 0 {
					finalMinor = n
				}
			}
		}
	}

	// 任一成功单则拒
	for _, it := range list {
		st := strings.ToLower(strAny(it["status"]))
		if st == "completed" || st == "success" || st == "paid" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "该卡密已有成功充值记录，不能换新码"})
			return
		}
	}

	if !orderLooksUnpaid(status, stage, finalMinor, events) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":        "不符合换码条件（需充值失败且未扣款）",
			"order_status": status,
			"order_stage":  stage,
			"final_amount": finalMinor,
			"local_status": localStatus,
		})
		return
	}

	// ★新码必须与旧码同一付款地区★：补发原来只带套餐，智利/日本等地区码补发出来全成了默认菲律宾区，
	// 用户兑换时按 PHP 走、和原单完全不是一回事（2026-10-10 代理反馈）。地区以本站缓存为准，
	// 老数据没记过地区就去卡台查；两边都拿不到就拒绝补发——宁可让代理找管理员，也不发错区的码。
	country, known := db.ForAccount(cli.AccountID()).LookupStoredCDKRegion(upstreamID)
	if !known {
		country, known = upstreamCDKRegion(c, cli, upstreamID)
	}
	if !known {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "无法确认原卡密的付款地区，暂不能补发，请联系管理员"})
		return
	}
	// 发一张同套餐、同地区的新码（带本站选卡偏好；跳过未启动卡头）
	sitePref, hasSitePref := issuePrefFromSite(cli.AccountID())
	issuePrefs := agentSwapIssuePrefs(sitePref, hasSitePref, country)

	claimed, claimErr := db.ForAccount(accountID).ClaimAgentExchange(codeHash)
	if claimErr != nil {
		c.JSON(503, gin.H{"error": "无法保存补发请求，未执行购买"})
		return
	}
	if !claimed {
		c.JSON(409, gin.H{"error": "该卡密已有补发请求，请勿重复操作"})
		return
	}
	idem := "agent-swap-" + codeHash
	var res *cardplatform.IssueCDKResult
	if len(issuePrefs) > 0 {
		res, err = cli.IssueCDKs(c.Request.Context(), plan, 1, idem, issuePrefs[0])
	} else {
		res, err = cli.IssueCDKs(c.Request.Context(), plan, 1, idem)
	}
	if err != nil {
		log.Printf("[agent-exchange] issue uncertain account=%d old=%d: %v", accountID, upstreamID, err)
		c.JSON(502, gin.H{"error": "补发请求结果未确认，已锁定重复购买，请联系管理员核对卡台记录"})
		return
	}
	if res == nil || len(res.Issued) != 1 || strings.TrimSpace(res.Issued[0].Code) == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "卡台未返回完整新码，已锁定重复购买，请联系管理员核对"})
		return
	}
	newIt := res.Issued[0]
	newCode := strings.TrimSpace(newIt.Code)
	newPrefix := strings.TrimSpace(newIt.CodePrefix)
	if newPrefix == "" && len(newCode) >= 14 {
		newPrefix = newCode[:14]
	}
	store := db.ForAccount(accountID)
	manual := func(message string) {
		db.WriteAudit("agent-swap", "agent_cdk_exchange.manual_review", fmt.Sprintf("account_id=%d old=%d new=%d", accountID, upstreamID, newIt.ID), ip)
		c.JSON(409, gin.H{"error": message + "；请联系管理员核对，已阻止重复购买", "new_cdk_id": newIt.ID})
	}
	if err := store.SaveAgentExchangeIssued(codeHash, newIt.ID, newCode); err != nil {
		manual("新码已生成，但补发记录保存失败")
		return
	}
	if newIt.ID <= 0 || newIt.ID == upstreamID || strings.EqualFold(newCode, code) {
		manual("卡台返回的新码编号或卡密异常")
		return
	}
	if got := strings.ToUpper(strings.TrimSpace(newIt.PaymentCountry)); got != "" && got != country {
		manual("卡台返回的新码付款地区与原码不一致")
		return
	}
	if err := store.SaveCardplatformCDKCode(newIt.ID, newCode, newPrefix, plan, newIt.FeeAmountMinor, country); err != nil {
		manual("新码已生成，但库存保存失败")
		return
	}
	if err := cli.DisableCDK(c.Request.Context(), upstreamID); err != nil {
		manual("新码已生成，但旧码未能禁用，暂不交付新码")
		return
	}
	if err := store.UpdateCardplatformCDKStatus(upstreamID, "disabled"); err != nil {
		manual("旧码已禁用，但本地状态保存失败")
		return
	}
	if err := store.RecordAgentCDKExchange(codeHash, upstreamID, newIt.ID, prefix, newPrefix, plan, orderID, status, ip); err != nil {
		manual("补发结果保存失败")
		return
	}
	if err := store.CompleteAgentExchange(codeHash); err != nil {
		manual("补发完成状态保存失败")
		return
	}

	db.WriteAudit("agent-swap", "agent_cdk_exchange",
		"account_id="+strconv.FormatInt(accountID, 10)+" old="+strconv.FormatInt(upstreamID, 10)+" new="+strconv.FormatInt(newIt.ID, 10)+" plan="+plan+" region="+country+" order="+strconv.FormatInt(orderID, 10),
		ip)

	c.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"plan":            plan,
		"old_cdk_id":      upstreamID,
		"old_code_prefix": prefix,
		"new_cdk_id":      newIt.ID,
		"new_code":        newCode,
		"new_code_prefix": newPrefix,
		"payment_country": country,
		"order_status":    status,
		"message":         "已换发全新卡密，请妥善保存（仅显示一次）",
	})
}

// agentSwapIssuePrefs 补发的发码偏好：地区与选卡偏好是两件独立的事，
// 没有本站选卡配置时也要把非默认地区带上（与正常发码 PublicIssue 同一规则）。
func agentSwapIssuePrefs(sitePref cardplatform.IssueCardPref, hasSitePref bool, country string) []cardplatform.IssueCardPref {
	country = strings.ToUpper(strings.TrimSpace(country))
	if !hasSitePref && country == "" {
		return nil
	}
	sitePref.PaymentCountry = country
	return []cardplatform.IssueCardPref{sitePref}
}

// upstreamCDKRegion 本站没记过地区的老码，从卡台按 id 查权威地区并回填缓存。
func upstreamCDKRegion(c *gin.Context, cli *cardplatform.Client, upstreamID int64) (string, bool) {
	res, err := cli.ListCDKsQuery(c.Request.Context(), cardplatform.CDKListQuery{Page: 1, PageSize: 50, Query: strconv.FormatInt(upstreamID, 10)})
	if err != nil || res == nil {
		return "", false
	}
	for _, it := range res.List {
		if it.ID == upstreamID {
			country := strings.ToUpper(strings.TrimSpace(it.PaymentCountry))
			_ = db.ForAccount(cli.AccountID()).UpdateCardplatformCDKRegion(upstreamID, country)
			return country, true
		}
	}
	return "", false
}

func parseCDKOrderList(raw json.RawMessage) (list []map[string]any, total int) {
	if len(raw) == 0 {
		return nil, 0
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, 0
	}
	// data.list or list
	data := envelope
	if d, ok := envelope["data"].(map[string]any); ok {
		data = d
	}
	total = int(anyToInt64(data["total"]))
	arr, _ := data["list"].([]any)
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			list = append(list, m)
		}
	}
	if total == 0 {
		total = len(list)
	}
	return list, total
}

func extractOrderMap(raw json.RawMessage) map[string]any {
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	if d, ok := envelope["data"].(map[string]any); ok {
		if o, ok := d["order"].(map[string]any); ok {
			return o
		}
		// data itself is order
		if _, ok := d["status"]; ok {
			return d
		}
	}
	if o, ok := envelope["order"].(map[string]any); ok {
		return o
	}
	return nil
}

func extractOrderEvents(raw json.RawMessage) []map[string]any {
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	var arr []any
	if d, ok := envelope["data"].(map[string]any); ok {
		arr, _ = d["events"].([]any)
	}
	if arr == nil {
		arr, _ = envelope["events"].([]any)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func anyToInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	default:
		return 0
	}
}
