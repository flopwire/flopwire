package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// redactCmd redacts a message after the fact (notes/redaction.md): on the
// server (rows, archived chunks, raw reads; old chunks purged) and in this
// device's local index. The harness's own transcript file is not touched.
func redactCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("redact", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `Usage: flopwire redact [flags] ADDRESS[:L1-L2]

Hide a message (or lines L1-L2 of its text, as flopwire read numbers them)
everywhere Flopwire keeps it: server rows, the archive, raw reads, and this
device's local index. Your own messages; with --admin, anyone's. On the
server, byte-identical copies of the record are redacted too: yours (the
same session archived from another device) always, another user's when
you uploaded the record first (others are listed; --admin redacts them).
--all-copies adds copies with the same text (subagents, forks, other
agents).

  flopwire redact 3f2a9c1e/42          the whole message
  flopwire redact 3f2a9c1e/42:7-9      lines 7 to 9
  flopwire redact --all-copies 3f2a9c1e/42:8-8

Flags:`)
		fs.PrintDefaults()
	}
	all := fs.Bool("all-copies", false, "also redact every identical copy (the +N copies group: subagents, forks, other devices)")
	admin := fs.Bool("admin", false, "act as an admin on another member's message (needs flopwire login)")
	localOnly := fs.Bool("local", false, "only this device's local index")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("one ADDRESS[:L1-L2] is required")
	}
	address := fs.Arg(0)
	if _, _, _, err := format.SplitLineRange(address); err != nil {
		return err
	}
	if !*localOnly {
		cfg, err := client.Load()
		switch {
		case err == nil && cfg.Server != "":
			token, path := cfg.Token, "/v1/redactions"
			if *admin {
				if token, err = cfg.SessionCredential(); err != nil {
					return err
				}
				path = "/v1/admin/redactions"
			}
			var res format.RedactResult
			if err := cfg.API(token).JSON(ctx, "POST", path, format.RedactRequest{Address: address, AllCopies: *all}, &res); err != nil {
				return client.TrustHint(err)
			}
			fmt.Printf("server: %d messages redacted, %d archived chunks and %d provisional tails rewritten", res.Messages, res.Chunks, res.Tails)
			if res.Fallbacks > 0 {
				fmt.Printf(" (%d records masked whole: their text was not found as written)", res.Fallbacks)
			}
			if res.JobID != "" {
				fmt.Printf("; old chunks purge in job %s", res.JobID)
			}
			fmt.Println()
			printSkipped(res.Skipped)
		case *admin:
			return errors.New("--admin needs a server: run flopwire login")
		default:
			fmt.Println("server: none configured; redacting the local index only")
		}
	}
	n, err := redactLocal(ctx, address, *all)
	if err != nil {
		return fmt.Errorf("local index: %w", err)
	}
	fmt.Printf("local index: %d rows redacted (your transcript file on disk is unchanged)\n", n)
	return nil
}

// redactLocal asks the running agent to apply the redaction to its index,
// or opens the index itself when no agent runs.
func redactLocal(ctx context.Context, address string, all bool) (int, error) {
	dir, err := configDir()
	if err != nil {
		return 0, err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := agent.Call(cctx, filepath.Join(dir, "agent.sock"), agent.Request{Op: "redact", Address: address, AllCopies: all})
	if err == nil {
		return resp.Redacted, nil
	}
	var nerr interface{ Timeout() bool }
	if resp.Error != "" || errors.As(err, &nerr) && nerr.Timeout() {
		return 0, err
	}
	// No agent: take the index ourselves.
	p, perr := indexPath()
	if perr != nil {
		return 0, perr
	}
	if _, serr := os.Stat(p); serr != nil {
		return 0, fmt.Errorf("no local index at %s", p)
	}
	s, oerr := localindex.Open(p, localindex.Options{})
	if oerr != nil {
		return 0, oerr
	}
	defer s.Close()
	return agent.RedactLocal(ctx, s, address, all)
}

// printSkipped lists other users' byte-identical copies a redaction left
// alone because they uploaded the record first: who and where, no text.
func printSkipped(skipped []format.SkippedCopies) {
	if len(skipped) == 0 {
		return
	}
	n := 0
	for _, sk := range skipped {
		n += sk.Messages
	}
	fmt.Printf("server: %d copies in %d sources of other users not redacted (they uploaded the record first):\n", n, len(skipped))
	for _, sk := range skipped {
		fmt.Printf("  %s on %s: %d messages (source %s)\n", sk.User, sk.Device, sk.Messages, sk.SourceID)
	}
	fmt.Println("  An admin can redact them with flopwire redact --admin.")
}
