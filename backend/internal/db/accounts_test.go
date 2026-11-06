package db

import (
	"database/sql"
	"github.com/tuzi/cdk-recharge-system/internal/config"
	"os"
	"path/filepath"
	"testing"
)

func accountTestDB(t *testing.T) {
	t.Helper()
	old := DB
	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "accounts.db"))
	t.Setenv("ADMIN_USERNAME", "fixtureadmin")
	t.Setenv("ADMIN_PASSWORD", "Fixture-only-password-92841")
	t.Setenv("CARD_API_KEY", "")
	if err := Init(&config.DatabaseConfig{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { DB.Close(); DB = old })
}
func TestAccountMigrationPreservesOriginalInventory(t *testing.T) {
	old := DB
	path := filepath.Join(t.TempDir(), "legacy.db")
	conn, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE site_settings(key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO site_settings(key,value) VALUES('card_api_key','original-key'),('webhook_secret','original-secret')`,
		`CREATE TABLE cardplatform_cdk_codes(upstream_id INTEGER,code TEXT NOT NULL UNIQUE,code_prefix TEXT,plan TEXT,fee_amount_minor INTEGER DEFAULT 0,status TEXT DEFAULT '',payment_country TEXT,created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO cardplatform_cdk_codes(upstream_id,code,code_prefix,plan,fee_amount_minor,status,payment_country) VALUES(7,'OLD-FULL-CODE','OLD','plus',15,'unused','CL')`,
		`CREATE TABLE cardplatform_cdk_notes(upstream_id INTEGER PRIMARY KEY,note TEXT NOT NULL DEFAULT '',updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO cardplatform_cdk_notes(upstream_id,note) VALUES(7,'历史库存')`,
	} {
		if _, err = conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	t.Setenv("DB_PATH", path)
	t.Setenv("ADMIN_PASSWORD", "Fixture-only-password-92841")
	if err = Init(&config.DatabaseConfig{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { DB.Close(); DB = old })
	a, err := GetCardAccount(1)
	if err != nil || a.APIKey != "original-key" || a.WebhookSecret != "original-secret" {
		t.Fatalf("credentials not migrated: %v", err)
	}
	if err = MigrateCardAccounts(); err != nil {
		t.Fatal("migration not idempotent", err)
	}
	if AccountIDForCode("old-full-code") != 1 {
		t.Fatal("old code lost origin")
	}
	list, total, err := ForAccount(1).ListCardplatformStoredCDKCodesPage("", "", "", 1, 10)
	if err != nil || total != 1 || list[0].FeeAmountMinor != 15 || list[0].PaymentCountry == nil || *list[0].PaymentCountry != "CL" {
		t.Fatalf("legacy metadata lost: %+v %v", list, err)
	}
	if ForAccount(1).GetCardplatformCDKNote(7) != "历史库存" {
		t.Fatal("legacy note lost")
	}
	secrets, err := WebhookSecrets()
	if err != nil || secrets["original-secret"] != 1 {
		t.Fatal("legacy callback secret lost")
	}
	var key string
	_ = DB.QueryRow(`SELECT value FROM site_settings WHERE key='card_api_key'`).Scan(&key)
	if key != "" {
		t.Fatal("stale global credentials retained")
	}
}
func TestAccountsIsolateCollidingIDsAndKeepCodeOwnership(t *testing.T) {
	accountTestDB(t)
	a, b := ForAccount(1), ForAccount(2)
	for _, s := range []AccountStore{a, b} {
		code := "CODE-A"
		if s.AccountID == 2 {
			code = "CODE-B"
		}
		if err := s.SaveCardplatformCDKCode(42, code, "SHARED", "plus", 15, "CL"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetCardplatformCDKNote(42, code); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertPlanStatus("plus", code, true, s.AccountID*100); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertCardProduct(CardProductCache{ProductCode: "P1", Issuer: code, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertCardFailEvent(CardFailEvent{CardID: 99, OrderID: 7, AccountEmailNorm: code + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.SaveCardplatformCDKCode(44, "CODE-A", "SHARED", "plus", 15); err == nil {
		t.Fatal("code reassigned across accounts")
	}
	if err := b.SaveCardplatformCDKCode(44, "code-a", "SHARED", "plus", 15); err == nil {
		t.Fatal("case variant reassigned code")
	}
	if err := a.UpdateCardplatformCDKMetadata(42, "disabled", "JP"); err != nil {
		t.Fatal(err)
	}
	if b.GetCardplatformCDKStatus(42) != "unused" {
		t.Fatal("status crossed account")
	}
	if code, _ := b.LookupCardplatformCDKCode(42, "SHARED"); code != "CODE-B" {
		t.Fatal("full-code lookup crossed account")
	}
	if note := b.GetCardplatformCDKNote(42); note != "CODE-B" {
		t.Fatal("note crossed account")
	}
	if _, total, err := a.ListCardplatformStoredCDKCodesPage("", "CODE-B", "", 1, 10); err != nil || total != 0 {
		t.Fatalf("note search leaked another account %d %v", total, err)
	}
	if err := a.UpsertCardBlock(CardBlockEntry{CardID: 99}); err != nil {
		t.Fatal(err)
	}
	if yes, _ := b.IsCardBlocked(99); yes {
		t.Fatal("block crossed account")
	}
	if stats, err := a.GetCardFailStats(99); err != nil || stats.FailCount != 1 {
		t.Fatal("health stats mixed", err)
	}
	if err := a.UnblockCard(99, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ListCardBlocklist(false, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ListCardFailEvents(10); err != nil {
		t.Fatal(err)
	}
	if plans, err := b.GetPlanStatusCache(); err != nil || len(plans) != 1 || plans[0].ServiceFeeUsdMinor != 200 {
		t.Fatalf("plan cache crossed account %v %v", plans, err)
	}
	if products, err := b.GetCardProducts(); err != nil || len(products) != 1 || products[0].Issuer != "CODE-B" {
		t.Fatalf("product cache crossed account %v %v", products, err)
	}
	if _, err := a.PruneCardProductsExcept(map[string]bool{"P2": true}); err != nil {
		t.Fatal(err)
	}
	if products, _ := b.GetCardProducts(); len(products) != 1 {
		t.Fatal("product prune crossed account")
	}
	if _, err := a.ClearCardplatformCDKNotes([]int64{42}); err != nil {
		t.Fatal(err)
	}
	if b.MapCardplatformCDKNotes([]int64{42})[42] != "CODE-B" {
		t.Fatal("note delete crossed account")
	}
}
func TestAccountCredentialsAndHistoricalTokens(t *testing.T) {
	accountTestDB(t)
	id, err := SaveCardAccount(CardAccount{ID: 1, Name: "A", APIKey: "key-a", WebhookSecret: "secret-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = SaveCardAccount(CardAccount{ID: id, Name: "B", APIKey: "key-b"}); err == nil {
		t.Fatal("changed issuer key")
	}
	if _, err = SaveCardAccount(CardAccount{Name: "duplicate", APIKey: "key-a"}); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if _, err = SaveCardAccount(CardAccount{ID: id, Name: "A renamed", WebhookSecret: "rotated-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err = SaveCardAccount(CardAccount{Name: "B", APIKey: "key-b", WebhookSecret: "secret-a"}); err == nil {
		t.Fatal("historical secret reused")
	}
	if err = BindCDKRedemptionToken("CODE-A", "token-1"); err != nil {
		t.Fatal(err)
	}
	if err = BindCDKRedemptionToken("CODE-A", "token-2"); err != nil {
		t.Fatal(err)
	}
	if code, err := FindCodeByRedemptionToken("token-1"); err != nil || code != "CODE-A" {
		t.Fatal("older valid token lost", err)
	}
	if err = BindCDKRedemptionToken("CODE-B", "token-1"); err == nil {
		t.Fatal("token rebound to another code")
	}
}

func TestAccountMigrationRollsBackOnIncompleteSchema(t *testing.T) {
	old := DB
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	DB = conn
	t.Cleanup(func() { conn.Close(); DB = old })
	for _, q := range []string{
		`CREATE TABLE site_settings(key TEXT PRIMARY KEY,value TEXT)`,
		`INSERT INTO site_settings VALUES('card_api_key','old-key')`,
		`CREATE TABLE cardplatform_cdk_codes(code TEXT)`,
	} {
		if _, err = conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err = MigrateCardAccounts(); err == nil {
		t.Fatal("incomplete migration unexpectedly succeeded")
	}
	var n int
	if err = conn.QueryRow(`SELECT count(*) FROM pragma_table_info('cardplatform_cdk_codes') WHERE name='account_id'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("partial table migration remained", err)
	}
	var key string
	_ = conn.QueryRow(`SELECT value FROM site_settings WHERE key='card_api_key'`).Scan(&key)
	if key != "old-key" {
		t.Fatal("failed migration erased old credentials")
	}
	_ = conn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='card_accounts'`).Scan(&n)
	if n != 0 {
		t.Fatal("partial accounts remained")
	}
}

func TestExchangeClaimSurvivesRestartAndSeparatesAccounts(t *testing.T) {
	accountTestDB(t)
	a, b := ForAccount(1), ForAccount(2)
	hash := HashCDKCode("OLD-CODE-A")
	ok, err := a.ClaimAgentExchange(hash)
	if err != nil || !ok {
		t.Fatal("claim failed", err)
	}
	if ok, err = b.ClaimAgentExchange(hash); err != nil || ok {
		t.Fatal("another account reclaimed old code", err)
	}
	if ok, err = b.ClaimAgentExchange(HashCDKCode("OLD-CODE-B")); err != nil || !ok {
		t.Fatal("unrelated account blocked", err)
	}
	if err = a.SaveAgentExchangeIssued(hash, 123, "NEW-CODE-A"); err != nil {
		t.Fatal(err)
	}
	// Re-open the file to ensure this is a durable claim, not a process mutex.
	if err = DB.Close(); err != nil {
		t.Fatal(err)
	}
	DB, err = sql.Open("sqlite3", os.Getenv("DB_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := AgentExchangePendingOrDone(hash); err != nil || !pending {
		t.Fatal("restart lost pending request", err)
	}
	if ok, err = a.ClaimAgentExchange(hash); err != nil || ok {
		t.Fatal("restart allowed second purchase", err)
	}
	var code string
	if err = DB.QueryRow(`SELECT new_code FROM agent_exchange_attempts WHERE old_code_hash=?`, hash).Scan(&code); err != nil || code != "NEW-CODE-A" {
		t.Fatal("issued result lost", err)
	}
}

func TestCDKLookupNeverGuessesFromAmbiguousPrefix(t *testing.T) {
	accountTestDB(t)
	store := ForAccount(1)
	for i, code := range []string{"SHARED-A", "SHARED-B"} {
		if err := store.SaveCardplatformCDKCode(int64(i+1), code, "SHARED", "plus", 15); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := store.LookupCardplatformCDKCode(99, "SHARED"); ok {
		t.Fatal("missing ID fell back to unrelated prefix")
	}
	if _, ok := store.LookupCardplatformCDKCode(0, "SHARED"); ok {
		t.Fatal("ambiguous prefix guessed a code")
	}
	if _, ok := store.LookupCardplatformCDKCode(0, "%"); ok {
		t.Fatal("prefix treated as SQL wildcard")
	}
	if code, ok := store.LookupCardplatformCDKCode(1, "SHARED"); !ok || code != "SHARED-A" {
		t.Fatal("exact ID failed")
	}
}
