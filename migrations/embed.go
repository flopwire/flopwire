package migrations

import "embed"

// Files contains the ordered PostgreSQL migration scripts.
//
//go:embed *.sql
var Files embed.FS
