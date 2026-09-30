package store

import (
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolHeadroom is how many connections the server keeps beyond its
// bounded users (retrieval slots and parse workers): flushes, which hold a
// connection for their whole body, logins, audit writes, the deletion
// worker and the chunk reconciler.
const PoolHeadroom = 24

// StatementBackstop stops any statement of the server's pool that runs
// longer than this. Retrieval sets its own, much shorter, budget per
// transaction (internal/retrieval); migrations lift it.
const StatementBackstop = 5 * time.Minute

// PoolConfig parses a database URL into the server's pool settings:
// MaxConns set explicitly to bounded (the retrieval concurrency plus the
// parse workers) plus PoolHeadroom, and the statement backstop. pgxpool's
// default of max(4, CPUs) connections lets a few slow searches starve
// sync and login. A pool_max_conns in the URL still wins.
func PoolConfig(url string, bounded int) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(url, "pool_max_conns=") {
		cfg.MaxConns = int32(bounded + PoolHeadroom)
	}
	if _, set := cfg.ConnConfig.RuntimeParams["statement_timeout"]; !set {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(StatementBackstop.Milliseconds(), 10)
	}
	return cfg, nil
}
