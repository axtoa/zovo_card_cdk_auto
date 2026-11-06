package db

import "fmt"

// A durable claim is written BEFORE the paid upstream request. Its uncertainty
// survives process restarts and upstream idempotency expiry. Ambiguous attempts
// require reconciliation, never an automatic second purchase.
func migrateAgentExchangeAttempts() error {
	_, err := DB.Exec(`CREATE TABLE IF NOT EXISTS agent_exchange_attempts (
 old_code_hash TEXT PRIMARY KEY,
 account_id INTEGER NOT NULL,
 state TEXT NOT NULL DEFAULT 'requested',
 new_upstream_id INTEGER,
 new_code TEXT,
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
 )`)
	return err
}
func AgentExchangePendingOrDone(hash string) (bool, error) {
	var n int
	err := DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_exchange_attempts WHERE old_code_hash=?) OR EXISTS(SELECT 1 FROM agent_cdk_exchanges WHERE old_code_hash=?)`, hash, hash).Scan(&n)
	return n != 0, err
}
func (s AccountStore) ClaimAgentExchange(hash string) (bool, error) {
	if s.AccountID <= 0 || hash == "" {
		return false, fmt.Errorf("account and old code required")
	}
	res, err := DB.Exec(`INSERT INTO agent_exchange_attempts(old_code_hash,account_id) SELECT ?,? WHERE NOT EXISTS(SELECT 1 FROM agent_cdk_exchanges WHERE old_code_hash=?) ON CONFLICT(old_code_hash) DO NOTHING`, hash, s.AccountID, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
func (s AccountStore) SaveAgentExchangeIssued(hash string, id int64, code string) error {
	res, err := DB.Exec(`UPDATE agent_exchange_attempts SET state='issued',new_upstream_id=?,new_code=?,updated_at=CURRENT_TIMESTAMP WHERE old_code_hash=? AND account_id=? AND state='requested'`, id, code, hash, s.AccountID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("exchange claim not found")
	}
	return nil
}
func (s AccountStore) CompleteAgentExchange(hash string) error {
	_, err := DB.Exec(`UPDATE agent_exchange_attempts SET state='completed',updated_at=CURRENT_TIMESTAMP WHERE old_code_hash=? AND account_id=?`, hash, s.AccountID)
	return err
}
