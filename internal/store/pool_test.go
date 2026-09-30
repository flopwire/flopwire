package store

import (
	"testing"
)

// V1: the server's pool is sized above its bounded users, and every
// connection carries a statement backstop.
func TestPoolConfig(t *testing.T) {
	cfg, err := PoolConfig("postgres://u:p@127.0.0.1:5432/db", 20)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 20+PoolHeadroom {
		t.Fatalf("MaxConns %d", cfg.MaxConns)
	}
	if got := cfg.ConnConfig.RuntimeParams["statement_timeout"]; got != "300000" {
		t.Fatalf("statement_timeout %q", got)
	}
	cfg, err = PoolConfig("postgres://u:p@127.0.0.1:5432/db?pool_max_conns=7&statement_timeout=1000", 20)
	if err != nil || cfg.MaxConns != 7 || cfg.ConnConfig.RuntimeParams["statement_timeout"] != "1000" {
		t.Fatalf("explicit settings lost: %v %d %q", err, cfg.MaxConns, cfg.ConnConfig.RuntimeParams["statement_timeout"])
	}
}
