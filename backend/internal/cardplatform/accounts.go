package cardplatform

import (
	"context"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

type accountContextKey struct{}

func WithAccount(ctx context.Context, a db.CardAccount) context.Context {
	return context.WithValue(ctx, accountContextKey{}, a)
}
func AccountID(ctx context.Context) int64 {
	a, _ := ctx.Value(accountContextKey{}).(db.CardAccount)
	return a.ID
}
func ConfigForContext(ctx context.Context) Config {
	cfg := LoadConfig()
	a, ok := ctx.Value(accountContextKey{}).(db.CardAccount)
	if ok {
		cfg.APIKey = a.APIKey
		cfg.AccountID = a.ID
	} else {
		cfg.APIKey = ""
		cfg.AccountID = 0
	}
	return cfg
}
func NewForContext(ctx context.Context) *Client { return New(ConfigForContext(ctx)) }
func NewForAccount(a db.CardAccount) *Client {
	cfg := LoadConfig()
	cfg.APIKey = a.APIKey
	cfg.AccountID = a.ID
	return New(cfg)
}
func (c *Client) AccountID() int64 { return c.cfg.AccountID }
