package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/upgrade"
)

const upgradeUsage = `usage: flopwire upgrade [--check] [--version vX.Y.Z] [--dir DIR]

Installs the latest flopwire release over the binary the plugins' hooks
run (the recorded binary, else this one), after verifying the archive
against the release's SHA256SUMS. On macOS a launchd device agent that
runs that binary is restarted. Running hooks keep the old binary until
they exit.

  --check            print this version and the latest release; install nothing
  --version vX.Y.Z   install that release instead of the latest
  --dir DIR          install into DIR instead of the recorded binary's directory`

func upgradeCommand(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "")
	want := fs.String("version", "", "")
	dir := fs.String("dir", "", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(w, upgradeUsage)
			return nil
		}
		return fmt.Errorf("%w\n%s", err, upgradeUsage)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), upgradeUsage)
	}
	if *check {
		return upgradeCheck(ctx, w)
	}
	target := *dir
	if target == "" {
		target = upgradeTargetDir()
	}
	fmt.Fprintf(w, "flopwire %s; installing %s into %s\n", version, orLatest(*want), target)
	res, err := upgrade.Install(ctx, upgrade.Options{Version: *want, Dir: target, Restart: upgrade.RestartLaunchd})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "installed flopwire %s at %s\n", res.Version, res.Binary)
	if res.Restart != "" {
		fmt.Fprintln(w, res.Restart)
	}
	if self, err := selfPath(); err == nil && self != res.Binary {
		fmt.Fprintf(w, "note: this command ran %s, which is unchanged\n", self)
	}
	return nil
}

func orLatest(v string) string {
	if v == "" {
		return "the latest release"
	}
	return v
}

// upgradeTargetDir is where the upgrade goes: the recorded binary's
// directory (the one the plugins' hooks run), else this binary's.
func upgradeTargetDir() string {
	if file, err := recordedBinaryPath(); err == nil {
		if r := readRecordedBinary(file); r != "" && isExecFile(r) {
			return filepath.Dir(r)
		}
	}
	if self, err := selfPath(); err == nil {
		return filepath.Dir(self)
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "bin")
}

func upgradeCheck(ctx context.Context, w io.Writer) error {
	fmt.Fprintf(w, "this binary: %s\n", version)
	latest, err := upgrade.Latest(ctx, nil, "")
	if err != nil {
		return fmt.Errorf("latest release: %w", err)
	}
	fmt.Fprintf(w, "latest release: %s\n", latest)
	if server := client.ServerVersion(); server != "" {
		fmt.Fprintf(w, "server: %s\n", server)
	}
	if server := client.ServerVersion(); upgrade.Newer(latest, server) {
		latest = server
	}
	switch {
	case upgrade.Newer(version, latest):
		fmt.Fprintf(w, "upgrade available: run %s\n", upgrade.InstallCommand)
	case strings.TrimSpace(version) == latest:
		fmt.Fprintln(w, "up to date")
	default:
		fmt.Fprintln(w, "this build is not a release version; upgrade installs the latest release over it")
	}
	return nil
}
