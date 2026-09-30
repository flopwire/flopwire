// Package pgtest gives each integration test its own Postgres database and,
// when an S3 endpoint is configured, its own bucket.
//
// Tests run when FLOPWIRE_TEST_DATABASE_URL names a role that may create
// databases; otherwise they skip. Packages run in parallel under `go test
// ./...`, so no test may share or reset a common schema.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnvURL is the environment variable naming the admin connection URL.
const EnvURL = "FLOPWIRE_TEST_DATABASE_URL"

// NewDatabase creates an empty database, drops it when the test ends, and
// returns its connection URL. It skips the test when EnvURL is unset.
func NewDatabase(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(EnvURL)
	if admin == "" {
		t.Skip(EnvURL + " is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("pgtest: connect admin: %v", err)
	}
	defer conn.Close(context.Background())
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "flopwire_test_" + hex.EncodeToString(b[:])
	if _, err = conn.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Logf("pgtest: drop %s: %v", name, err)
			return
		}
		defer c.Close(context.Background())
		if _, err = c.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
			t.Logf("pgtest: drop %s: %v", name, err)
		}
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("pgtest: parse %s: %v", EnvURL, err)
	}
	u.Path = "/" + name
	return u.String()
}
