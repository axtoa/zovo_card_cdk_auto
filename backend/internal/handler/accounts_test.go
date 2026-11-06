package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tuzi/cdk-recharge-system/internal/cardplatform"
	"github.com/tuzi/cdk-recharge-system/internal/config"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func multiAccountFixture(t *testing.T) {
	t.Helper()
	old := db.DB
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "accounts.db"))
	t.Setenv("CARD_API_KEY", "")
	t.Setenv("ADMIN_PASSWORD", "Fixture-password-92841")
	if err := db.Init(&config.DatabaseConfig{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DB.Close(); db.DB = old })
	for _, a := range []db.CardAccount{{ID: 1, Name: "A", APIKey: "key-a", WebhookSecret: "secret-a"}, {Name: "B", APIKey: "key-b", WebhookSecret: "secret-b"}} {
		if _, err := db.SaveCardAccount(a); err != nil {
			t.Fatal(err)
		}
	}
	gin.SetMode(gin.TestMode)
}
func accountRequest(router http.Handler, method, path, body, account string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Card-Account-ID", account)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}
func TestAccountRequestsStayIndependentAndFailClosed(t *testing.T) {
	multiAccountFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key != "key-a" && key != "key-b" {
			t.Error("wrong credentials")
			w.WriteHeader(401)
			return
		}
		balance := 110
		if key == "key-b" {
			balance = 220
		}
		fmt.Fprintf(w, `{"code":0,"data":{"balance":%d,"spendable_balance":%d,"account_reserve_amount":20}}`, balance, balance-20)
	}))
	defer upstream.Close()
	_ = db.SetSetting("card_api_base", upstream.URL)
	router := gin.New()
	router.GET("/balance", RequireCardAccount, CardPlatformBalance)
	for _, id := range []string{"", "0", "999", "1 OR 1=1"} {
		if w := accountRequest(router, "GET", "/balance", "", id); w.Code != 400 {
			t.Fatalf("invalid account accepted %q %d", id, w.Code)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "1"
			want := 90.
			if i%2 == 1 {
				id = "2"
				want = 200
			}
			w := accountRequest(router, "GET", "/balance", "", id)
			var result map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &result)
			if w.Code != 200 || result["spendable_balance"] != want {
				t.Errorf("mixed concurrent balance account %s: %s", id, w.Body.String())
			}
		}(i)
	}
	wg.Wait()
	accounts := gin.New()
	accounts.GET("/accounts", AdminListCardAccounts)
	w := accountRequest(accounts, "GET", "/accounts", "", "")
	if strings.Contains(w.Body.String(), "key-a") || strings.Contains(w.Body.String(), "secret-a") {
		t.Fatal("raw credentials leaked")
	}
}
func TestSharedWebhookRoutesRetriesAndKeepsTerminalStatus(t *testing.T) {
	multiAccountFixture(t)
	for _, id := range []int64{1, 2} {
		if err := db.ForAccount(id).SaveCardplatformCDKCode(42, fmt.Sprintf("CODE-%d", id), "SHARED", "plus", 15); err != nil {
			t.Fatal(err)
		}
	}
	router := gin.New()
	router.POST("/hook", CardPlatformWebhook)
	send := func(secret, body string) int {
		req := httptest.NewRequest("POST", "/hook", strings.NewReader(body))
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	completed := `{"event":"gpt_direct.completed","event_id":"same-upstream-event","cdk_id":42,"order_id":7}`
	if send("wrong", completed) != 401 {
		t.Fatal("bad signature accepted")
	}
	if send("secret-a", `{broken`) != 400 {
		t.Fatal("bad JSON accepted")
	}
	if send("secret-a", completed) != 200 || send("secret-a", completed) != 200 {
		t.Fatal("valid/repeated callback failed")
	}
	if db.ForAccount(1).GetCardplatformCDKStatus(42) != "consumed" || db.ForAccount(2).GetCardplatformCDKStatus(42) != "unused" {
		t.Fatal("webhook status crossed account")
	}
	if send("secret-a", `{"event":"gpt_direct.failed","event_id":"late","cdk_id":42,"cdk_status":"unused"}`) != 200 {
		t.Fatal("late callback failed")
	}
	if db.ForAccount(1).GetCardplatformCDKStatus(42) != "consumed" {
		t.Fatal("late failure downgraded consumed")
	}
	if send("secret-b", completed) != 200 {
		t.Fatal("same upstream event ID collided across accounts")
	}
	var count int
	_ = db.DB.QueryRow(`SELECT count(*) FROM webhook_events`).Scan(&count)
	if count != 3 {
		t.Fatalf("duplicate event stored %d", count)
	}
	// Rotate without breaking old callbacks.
	if _, err := db.SaveCardAccount(db.CardAccount{ID: 1, Name: "A", WebhookSecret: "rotated-a"}); err != nil {
		t.Fatal(err)
	}
	if send("secret-a", completed) != 200 || send("rotated-a", completed) != 200 {
		t.Fatal("rotation lost in-flight callback")
	}
	_, _ = db.DB.Exec(`UPDATE account_webhook_secrets SET expires_at=datetime('now','-1 second') WHERE secret='secret-a'`)
	if send("secret-a", completed) != 401 {
		t.Fatal("expired secret still accepted")
	}
	// Durable pending event: a local write failure must not acknowledge success.
	_, err := db.DB.Exec(`CREATE TRIGGER fail_status BEFORE UPDATE ON cardplatform_cdk_codes BEGIN SELECT RAISE(FAIL,'fixture failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	pending := `{"event":"gpt_direct.completed","event_id":"retry-me","cdk_id":42}`
	if send("secret-b", pending) != 503 {
		t.Fatal("DB failure acknowledged")
	}
	_, _ = db.DB.Exec(`DROP TRIGGER fail_status`)
	if err := db.ForAccount(2).SaveCardplatformCDKCode(43, "UNRELATED-CODE", "UNRELATED", "plus", 15); err != nil {
		t.Fatal(err)
	}
	altered := strings.Replace(pending, `"cdk_id":42`, `"cdk_id":43`, 1)
	if send("secret-b", altered) != 200 {
		t.Fatal("pending event not retryable")
	}
	if db.ForAccount(2).GetCardplatformCDKStatus(43) != "unused" {
		t.Fatal("reused event ID substituted a different CDK")
	}
}
func TestPublicRedemptionUsesTokenOriginAndNeverSendsAPIKey(t *testing.T) {
	multiAccountFixture(t)
	for _, id := range []int64{1, 2} {
		_ = db.ForAccount(id).SaveCardplatformCDKCode(id, fmt.Sprintf("CODE-%d", id), "", "plus", 15)
		_ = db.ForAccount(id).UpsertCardBlock(db.CardBlockEntry{CardID: id * 100})
	}
	if err := db.BindCDKRedemptionToken("CODE-1", "token-a"); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			t.Error("public request sent privileged key")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["redemption_token"] == "token-a" {
			ids, _ := body["exclude_card_ids"].([]any)
			if len(ids) != 1 || ids[0] != float64(100) {
				t.Errorf("wrong origin blacklist: %v", body)
			}
		} else if _, ok := body["exclude_card_ids"]; ok {
			t.Error("unknown token borrowed an account")
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	_ = db.SetSetting("card_api_base", upstream.URL)
	router := gin.New()
	router.POST("/redeem", PublicCDKRedeem)
	for _, body := range []string{`{"redemption_token":"token-a","code":"CODE-2","account_id":2}`, `{"redemption_token":"unknown","code":"CODE-1"}`} {
		if w := accountRequest(router, "POST", "/redeem", body, "2"); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
}
func TestReplacementUsesOriginalIssuerAndRegion(t *testing.T) {
	multiAccountFixture(t)
	_ = db.ForAccount(1).SaveCardplatformCDKCode(42, "A-OLD-CODE-123456789", "A-OLD", "plus", 15, "CL")
	if err := setAgentSwapPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	var issueKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "key-a" {
			t.Error("replacement used another account")
		}
		switch r.URL.Path {
		case "/openapi/v1/gpt-direct/cdk-orders":
			fmt.Fprint(w, `{"code":0,"data":{"total":1,"list":[{"id":7,"status":"failed_precharge","final_amount_minor":0}]}}`)
		case "/openapi/v1/gpt-direct/cdk-orders/7":
			fmt.Fprint(w, `{"code":0,"data":{"order":{"id":7,"status":"failed_precharge","final_amount_minor":0},"events":[]}}`)
		case "/openapi/v1/gpt-direct/cdks":
			issueKey = r.Header.Get("Idempotency-Key")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["payment_country"] != "CL" {
				t.Error("replacement changed region")
			}
			fmt.Fprint(w, `{"code":0,"data":{"requested":1,"issued":[{"id":43,"code":"A-NEW-CODE-123456789","plan":"plus","payment_country":"CL"}]}}`)
		case "/openapi/v1/gpt-direct/cdks/42/disable":
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	_ = db.SetSetting("card_api_base", upstream.URL)
	router := gin.New()
	router.POST("/exchange", PublicAgentCDKExchange)
	w := accountRequest(router, "POST", "/exchange", `{"password":"fixture-password","code":"A-OLD-CODE-123456789"}`, "2")
	if w.Code != 200 {
		t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
	}
	if issueKey == "" {
		t.Fatal("replacement missing stable idempotency")
	}
	if db.AccountIDForCode("A-NEW-CODE-123456789") != 1 {
		t.Fatal("replacement stored under wrong account")
	}
	if db.ForAccount(1).GetCardplatformCDKStatus(42) != "disabled" {
		t.Fatal("old code not disabled")
	}
}
func TestSharedRuleSyncReportsEachAccountFailure(t *testing.T) {
	multiAccountFixture(t)
	seen := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		seen[key]++
		if key == "key-b" {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"code":503,"msg":"fixture unavailable"}`)
			return
		}
		if r.Method == "GET" {
			fmt.Fprint(w, `{"code":0,"data":{"list":[]}}`)
		} else {
			fmt.Fprint(w, `{"code":0,"data":{"product":"gpt"}}`)
		}
	}))
	defer upstream.Close()
	_ = db.SetSetting("card_api_base", upstream.URL)
	if err := SyncOwnerDirectCardRules(context.Background()); err == nil {
		t.Fatal("partial sync reported success")
	}
	if seen["key-a"] != 4 || seen["key-b"] != 1 {
		t.Fatalf("fanout missed account: %v", seen)
	}
	raw, _ := db.GetSetting("card_rule_sync_results")
	var results []map[string]any
	_ = json.Unmarshal([]byte(raw), &results)
	if len(results) != 2 || results[0]["ok"] != true || results[1]["ok"] != false {
		t.Fatalf("wrong account results %s", raw)
	}
	// Public client remains credential-free even with two configured accounts.
	if cardplatform.LoadConfig().APIKey != "" {
		t.Fatal("implicit active account exists")
	}
}

func TestReplacementPartialFailuresDoNotPurchaseAgain(t *testing.T) {
	for _, scenario := range []string{"issue_timeout", "store_failure", "disable_failure", "record_failure", "wrong_region"} {
		t.Run(scenario, func(t *testing.T) {
			multiAccountFixture(t)
			code := "PARTIAL-OLD-CODE-123456789"
			if err := db.ForAccount(1).SaveCardplatformCDKCode(42, code, "PARTIAL", "plus", 15, "CL"); err != nil {
				t.Fatal(err)
			}
			if err := setAgentSwapPassword("fixture-password"); err != nil {
				t.Fatal(err)
			}
			if scenario == "store_failure" {
				if _, err := db.DB.Exec(`CREATE TRIGGER reject_new BEFORE INSERT ON cardplatform_cdk_codes WHEN NEW.upstream_id=43 BEGIN SELECT RAISE(FAIL,'fixture full disk'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "record_failure" {
				if _, err := db.DB.Exec(`CREATE TRIGGER reject_record BEFORE INSERT ON agent_cdk_exchanges BEGIN SELECT RAISE(FAIL,'fixture record error'); END`); err != nil {
					t.Fatal(err)
				}
			}
			issues, disables := 0, 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-Key") != "key-a" {
					t.Error("wrong issuer")
				}
				switch r.URL.Path {
				case "/openapi/v1/gpt-direct/cdk-orders":
					fmt.Fprint(w, `{"code":0,"data":{"total":1,"list":[{"id":7,"status":"failed_precharge","final_amount_minor":0}]}}`)
				case "/openapi/v1/gpt-direct/cdk-orders/7":
					fmt.Fprint(w, `{"code":0,"data":{"order":{"id":7,"status":"failed_precharge","final_amount_minor":0},"events":[]}}`)
				case "/openapi/v1/gpt-direct/cdks":
					issues++
					if scenario == "issue_timeout" {
						w.WriteHeader(504)
						fmt.Fprint(w, `{"code":504,"msg":"uncertain"}`)
						return
					}
					country := "CL"
					if scenario == "wrong_region" {
						country = "JP"
					}
					fmt.Fprintf(w, `{"code":0,"data":{"requested":1,"issued":[{"id":43,"code":"PARTIAL-NEW-CODE-123456789","plan":"plus","payment_country":%q}]}}`, country)
				case "/openapi/v1/gpt-direct/cdks/42/disable":
					disables++
					if scenario == "disable_failure" {
						w.WriteHeader(503)
						fmt.Fprint(w, `{"code":503,"msg":"disable failed"}`)
						return
					}
					fmt.Fprint(w, `{"code":0,"data":{}}`)
				default:
					t.Errorf("unexpected %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer upstream.Close()
			_ = db.SetSetting("card_api_base", upstream.URL)
			// Each fixture simulates an independent process's rate-limit state.
			swapRateMu.Lock()
			swapRateHits = map[string][]time.Time{}
			swapRateMu.Unlock()
			router := gin.New()
			router.POST("/exchange", PublicAgentCDKExchange)
			body := fmt.Sprintf(`{"password":"fixture-password","code":%q}`, code)
			first := accountRequest(router, "POST", "/exchange", body, "2")
			if first.Code == 200 || strings.Contains(first.Body.String(), "PARTIAL-NEW-CODE") {
				t.Fatalf("unsafe delivery %d %s", first.Code, first.Body.String())
			}
			// Clear failure triggers: the durable claim must still prevent a second
			// paid request, even when the original transient failure has disappeared.
			_, _ = db.DB.Exec(`DROP TRIGGER IF EXISTS reject_new`)
			_, _ = db.DB.Exec(`DROP TRIGGER IF EXISTS reject_record`)
			again := accountRequest(router, "POST", "/exchange", body, "2")
			if again.Code != 409 || issues != 1 {
				t.Fatalf("duplicate purchase: status=%d issues=%d", again.Code, issues)
			}
			if (scenario == "store_failure" || scenario == "wrong_region") && disables != 0 {
				t.Fatal("old code disabled before safe persistence/validation")
			}
		})
	}
}

func TestWebhookMissingIdentifiersDoNotMergeDifferentEvents(t *testing.T) {
	for _, kind := range []string{"card_operation", "card_transaction"} {
		a := map[string]any{"event": kind, "status": "failed", "card_id": float64(1)}
		b := map[string]any{"event": kind, "status": "failed", "card_id": float64(2)}
		if webhookIdemKey(a, kind) == webhookIdemKey(b, kind) {
			t.Fatal("different anonymous events merged")
		}
		if webhookIdemKey(a, kind) != webhookIdemKey(a, kind) {
			t.Fatal("retry not stable")
		}
	}
}
