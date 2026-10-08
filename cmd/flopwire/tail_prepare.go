package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"path/filepath"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
)

func agentPrepareLegacyTails(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent prepare-legacy-tails", flag.ContinueOnError)
	dbPath := fs.String("db", "", "existing collector index (default $FLOPWIRE_INDEX or user cache index)")
	spoolPath := fs.String("spool", "", "existing sync spool (default client config directory/spool)")
	asJSON := fs.Bool("json", false, "print aggregate conversion counts as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: flopwire agent prepare-legacy-tails [--db index] [--spool directory] [--json]")
	}
	var err error
	if *dbPath == "" {
		if *dbPath, err = indexPath(); err != nil {
			return err
		}
	}
	if *spoolPath == "" {
		dir, err := configDir()
		if err != nil {
			return err
		}
		*spoolPath = filepath.Join(dir, "spool")
	}
	lock, err := localindex.AcquireMaintenanceLock(*dbPath)
	if err != nil {
		return errors.New("tail preparation requires a stopped collector and exclusive ownership of an existing index")
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := filepath.Abs(*dbPath)
	if err != nil {
		return errors.New("cannot resolve existing collector index")
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return errors.New("cannot open existing collector state read-only")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	result, err := devicesync.PrepareLegacyTails(ctx, db, *spoolPath)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(out).Encode(result)
	}
	_, err = fmt.Fprintf(out, "legacy tails prepared: needed %d; converted %d; canonical %d; intentionally unspooled %d; obsolete versions removed %d\nTail format only; older binary policy and schema compatibility require separate qualification.\n",
		result.Needed, result.Converted, result.Canonical, result.Unspooled, result.RemovedVersions)
	return err
}
