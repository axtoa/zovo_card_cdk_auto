package db

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// AccountStore carries immutable ownership through each operation. Zero never
// means "all accounts": unknown public codes must not access privileged data.
type AccountStore struct{ AccountID int64 }

func ForAccount(id int64) AccountStore { return AccountStore{AccountID: id} }
func (s AccountStore) sqlID() string   { return strconv.FormatInt(s.AccountID, 10) }

type CardAccount struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	APIKey        string `json:"-"`
	WebhookSecret string `json:"-"`
	CreatedAt     string `json:"created_at"`
}

func ListCardAccounts() ([]CardAccount, error) {
	rows, err := DB.Query(`SELECT id,name,api_key,webhook_secret,created_at FROM card_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CardAccount{}
	for rows.Next() {
		var a CardAccount
		if err := rows.Scan(&a.ID, &a.Name, &a.APIKey, &a.WebhookSecret, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func GetCardAccount(id int64) (CardAccount, error) {
	var a CardAccount
	err := DB.QueryRow(`SELECT id,name,api_key,webhook_secret,created_at FROM card_accounts WHERE id=?`, id).Scan(&a.ID, &a.Name, &a.APIKey, &a.WebhookSecret, &a.CreatedAt)
	return a, err
}

// Keys identify issuer applications. Replacing a configured key could strand
// historical orders even when the new key belongs to the same owner.
func SaveCardAccount(a CardAccount) (int64, error) {
	a.Name = strings.TrimSpace(a.Name)
	a.APIKey = strings.TrimSpace(a.APIKey)
	a.WebhookSecret = strings.TrimSpace(a.WebhookSecret)
	if a.Name == "" || len([]rune(a.Name)) > 80 {
		return 0, fmt.Errorf("账号名称须为 1–80 字")
	}
	tx, err := DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if a.ID > 0 {
		var oldKey, oldSecret string
		if err = tx.QueryRow(`SELECT api_key,webhook_secret FROM card_accounts WHERE id=?`, a.ID).Scan(&oldKey, &oldSecret); err != nil {
			return 0, err
		}
		if a.APIKey == "" {
			a.APIKey = oldKey
		}
		if oldKey != "" && a.APIKey != oldKey {
			return 0, fmt.Errorf("已配置的 API Key 不可替换；请新增账号，保留旧码归属")
		}
		if a.WebhookSecret == "" {
			a.WebhookSecret = oldSecret
		}

	}
	if a.APIKey == "" {
		return 0, fmt.Errorf("请填写 API Key")
	}
	var n int
	if err = tx.QueryRow(`SELECT count(*) FROM card_accounts WHERE id<>? AND (api_key=? OR (webhook_secret<>'' AND webhook_secret=?))`, a.ID, a.APIKey, a.WebhookSecret).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, fmt.Errorf("API Key 或回调密钥已被其他账号使用")
	}
	if a.WebhookSecret != "" {
		if err = tx.QueryRow(`SELECT count(*) FROM account_webhook_secrets WHERE secret=? AND account_id<>?`, a.WebhookSecret, a.ID).Scan(&n); err != nil {
			return 0, err
		}
		if n > 0 {
			return 0, fmt.Errorf("该回调密钥属于其他账号（包括历史密钥）")
		}
	}
	if a.ID > 0 {
		_, err = tx.Exec(`UPDATE card_accounts SET name=?,api_key=?,webhook_secret=? WHERE id=?`, a.Name, a.APIKey, a.WebhookSecret, a.ID)
	} else {
		var res sql.Result
		res, err = tx.Exec(`INSERT INTO card_accounts(name,api_key,webhook_secret) VALUES(?,?,?)`, a.Name, a.APIKey, a.WebhookSecret)
		if err == nil {
			a.ID, err = res.LastInsertId()
		}
	}
	if err != nil {
		return 0, err
	}
	if a.WebhookSecret != "" {
		// Give in-flight callbacks a bounded overlap; retired secrets must not
		// remain valid indefinitely. Rotating again never extends old overlap.
		if _, err = tx.Exec(`UPDATE account_webhook_secrets SET expires_at=datetime('now','+24 hours') WHERE account_id=? AND secret<>? AND expires_at IS NULL`, a.ID, a.WebhookSecret); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(`INSERT INTO account_webhook_secrets(account_id,secret) VALUES(?,?) ON CONFLICT(secret) DO UPDATE SET expires_at=NULL`, a.ID, a.WebhookSecret); err != nil {
			return 0, err
		}
	}
	return a.ID, tx.Commit()
}

func AccountIDForCode(code string) int64 {
	if DB == nil || strings.TrimSpace(code) == "" {
		return 0
	}
	var id int64
	_ = DB.QueryRow(`SELECT account_id FROM cardplatform_cdk_codes WHERE code=? COLLATE NOCASE`, strings.TrimSpace(code)).Scan(&id)
	return id
}

// MigrateCardAccounts is one transaction: old inventory and account-specific
// keys move together, or none of them do. Safe to rerun after restart.
func MigrateCardAccounts() error {
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS card_accounts(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,api_key TEXT NOT NULL DEFAULT '',webhook_secret TEXT NOT NULL DEFAULT '',created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return err
	}
	var done int
	if err = tx.QueryRow(`SELECT count(*) FROM site_settings WHERE key='card_accounts_migrated'`).Scan(&done); err != nil {
		return err
	}
	if done > 0 {
		return tx.Commit()
	}
	get := func(key, env string) string {
		var v string
		_ = tx.QueryRow(`SELECT value FROM site_settings WHERE key=?`, key).Scan(&v)
		if strings.TrimSpace(v) == "" {
			v = os.Getenv(env)
		}
		return strings.TrimSpace(v)
	}
	key, secret := get("card_api_key", "CARD_API_KEY"), get("webhook_secret", "")
	base := strings.TrimRight(get("card_api_base", "CARD_API_BASE"), "/")
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/openapi/v1"), "/openapi")
	if base == "" {
		base = "https://zovocard.com"
	}
	if _, err = tx.Exec(`INSERT INTO site_settings(key,value) VALUES('card_api_base',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, base); err != nil {
		return err
	}

	if _, err = tx.Exec(`INSERT INTO card_accounts(id,name,api_key,webhook_secret) VALUES(1,'原账号',?,?)`, key, secret); err != nil {
		return err
	}
	// Rebuild only tables whose former primary/unique key needs account scope.
	replacements := map[string][2]string{
		"cardplatform_cdk_notes": {"upstream_id INTEGER PRIMARY KEY", "upstream_id INTEGER, PRIMARY KEY(account_id, upstream_id)"},
		"plan_status_cache":      {"plan_key TEXT PRIMARY KEY", "plan_key TEXT, PRIMARY KEY(account_id, plan_key)"},
		"card_product_cache":     {"product_code TEXT PRIMARY KEY", "product_code TEXT, PRIMARY KEY(account_id, product_code)"},
		"card_blocklist":         {"card_id INTEGER PRIMARY KEY", "card_id INTEGER, PRIMARY KEY(account_id, card_id)"},
		"card_fail_events":       {"UNIQUE(order_id, card_id)", "UNIQUE(account_id, order_id, card_id)"},
	}
	tables := []string{"cardplatform_cdk_codes", "cardplatform_cdk_notes", "plan_status_cache", "card_product_cache", "card_blocklist", "card_fail_events", "agent_cdk_exchanges", "webhook_events"}
	for _, table := range tables {
		var ddl string
		if err = tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl); err != nil {
			return fmt.Errorf("migrate %s: %w", table, err)
		}
		if pair, ok := replacements[table]; ok {
			// Append table constraints at the end, after every column.
			replacement := pair[1]
			constraint := ""
			if i := strings.Index(replacement, ", PRIMARY KEY"); i >= 0 {
				constraint = replacement[i:]
				replacement = replacement[:i]
			}
			if !strings.Contains(ddl, pair[0]) {
				return fmt.Errorf("unexpected schema for %s", table)
			}
			ddl = strings.Replace(ddl, pair[0], replacement, 1)
			i := strings.LastIndex(ddl, ")")
			ddl = ddl[:i] + constraint + ddl[i:]
			i = strings.Index(ddl, "(")
			ddl = ddl[:i+1] + "account_id INTEGER NOT NULL DEFAULT 1, " + ddl[i+1:]
			ddl = strings.Replace(ddl, table, table+"_accounts_new", 1)
			var indexes []string
			rows, e := tx.Query(`SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name=? AND sql IS NOT NULL`, table)
			if e != nil {
				return e
			}
			for rows.Next() {
				var q string
				if e = rows.Scan(&q); e != nil {
					rows.Close()
					return e
				}
				indexes = append(indexes, q)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			for _, q := range []string{ddl, "INSERT INTO " + table + "_accounts_new SELECT 1,* FROM " + table, "DROP TABLE " + table, "ALTER TABLE " + table + "_accounts_new RENAME TO " + table} {
				if _, err = tx.Exec(q); err != nil {
					return fmt.Errorf("migrate %s: %w", table, err)
				}
			}
			for _, q := range indexes {
				if _, err = tx.Exec(q); err != nil {
					return err
				}
			}
		} else {
			if _, err = tx.Exec("ALTER TABLE " + table + " ADD COLUMN account_id INTEGER NOT NULL DEFAULT 1"); err != nil {
				return err
			}
		}
	}
	for _, q := range []string{
		`ALTER TABLE webhook_events ADD COLUMN processed_at DATETIME`,
		`UPDATE webhook_events SET idem_key='1|'||idem_key,processed_at=created_at`,
		`CREATE UNIQUE INDEX card_accounts_key ON card_accounts(api_key) WHERE api_key<>''`,
		`CREATE UNIQUE INDEX card_accounts_secret ON card_accounts(webhook_secret) WHERE webhook_secret<>''`,
		`CREATE UNIQUE INDEX cp_cdk_code_nocase ON cardplatform_cdk_codes(code COLLATE NOCASE)`,
		`CREATE INDEX cp_cdk_account_id ON cardplatform_cdk_codes(account_id,upstream_id)`,
		`CREATE TABLE account_webhook_secrets(account_id INTEGER NOT NULL,secret TEXT PRIMARY KEY,expires_at DATETIME)`,
		`INSERT INTO account_webhook_secrets(account_id,secret) SELECT id,webhook_secret FROM card_accounts WHERE webhook_secret<>''`,
		`CREATE TABLE cdk_token_origins(token TEXT PRIMARY KEY,code TEXT NOT NULL)`,
		`INSERT OR IGNORE INTO cdk_token_origins SELECT redemption_token,cdk_code FROM cdk_session_bindings WHERE redemption_token IS NOT NULL AND redemption_token<>''`,
		`INSERT INTO site_settings(key,value) VALUES('card_accounts_migrated','1')`,
		`DELETE FROM site_settings WHERE key IN ('card_api_key','webhook_secret')`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WebhookSecrets includes prior secrets so in-flight callbacks survive rotation.
func WebhookSecrets() (map[string]int64, error) {
	rows, err := DB.Query(`SELECT secret,account_id FROM account_webhook_secrets WHERE expires_at IS NULL OR expires_at>CURRENT_TIMESTAMP`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var secret string
		var id int64
		if err = rows.Scan(&secret, &id); err != nil {
			return nil, err
		}
		out[secret] = id
	}
	return out, rows.Err()
}
