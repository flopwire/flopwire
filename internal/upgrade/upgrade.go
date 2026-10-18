// Package upgrade compares flopwire versions and installs a release
// binary: what `flopwire upgrade` runs, and what the device agent uses to
// notice that the server runs a newer flopwire than this device.
package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Releases is where releases live. The install script reads the same
// URLs: releases/latest/download/<asset> and releases/download/<tag>/<asset>.
const Releases = "https://github.com/flopwire/flopwire/releases"

// InstallCommand is what a person or agent runs to upgrade.
const InstallCommand = "flopwire upgrade"

// Parse reads a release version, vX.Y.Z with an optional pre-release
// suffix, into its numeric parts. A development version ("dev", a commit
// hash, "v0.2.0-next") is not comparable: ok is false.
func Parse(v string) (parts [3]int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		return parts, false
	}
	fields := strings.Split(v, ".")
	if len(fields) != 3 {
		return parts, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || (len(f) > 1 && f[0] == '0') {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}

// IsRelease reports whether v is a release version (vX.Y.Z).
func IsRelease(v string) bool {
	_, ok := Parse(v)
	return ok
}

// Newer reports whether latest is a release version newer than current.
// It is false when either is not a release version: a development build
// is never told to upgrade, and an unparseable server version never
// triggers a notice.
func Newer(current, latest string) bool {
	c, ok := Parse(current)
	if !ok {
		return false
	}
	l, ok := Parse(latest)
	if !ok {
		return false
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// Notice is the one line a device shows when latest is newer than
// current, or "".
func Notice(current, latest string) string {
	if !Newer(current, latest) {
		return ""
	}
	return fmt.Sprintf("flopwire %s is available; this device runs %s. Upgrade with: %s", latest, current, InstallCommand)
}

// Asset is the release archive for an OS and CPU.
func Asset(goos, goarch string) string {
	return fmt.Sprintf("flopwire_%s_%s.tar.gz", goos, goarch)
}

// Options control Install.
type Options struct {
	// Version is the release tag to install; "" means the latest.
	Version string
	// Dir is the directory to install into; "" means the directory of
	// the running binary.
	Dir string
	// Releases overrides the release URL base (tests).
	Releases string
	// Client is the HTTP client; nil uses a default with a timeout.
	Client *http.Client
	// GOOS and GOARCH override the asset selection (tests).
	GOOS, GOARCH string
	// Restart, when set, restarts the device agent after the binary is
	// replaced (RestartLaunchd on macOS).
	Restart func(ctx context.Context, binary string) (string, error)
}

// Result reports what Install did.
type Result struct {
	Binary  string // the installed path
	Version string // what the installed binary prints
	Restart string // what Restart reported, "" when none ran
}

// Install downloads the release archive for this machine, verifies it
// against the release's SHA256SUMS, checks the binary runs, and replaces
// the target with one rename, so a running agent or hook keeps its old
// binary and none sees a half-written one.
func Install(ctx context.Context, o Options) (Result, error) {
	var res Result
	goos, goarch := o.GOOS, o.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	if goos != "darwin" && goos != "linux" {
		return res, fmt.Errorf("no release for %s: releases cover macOS and Linux", goos)
	}
	if goarch != "amd64" && goarch != "arm64" {
		return res, fmt.Errorf("no release for %s: releases cover arm64 and amd64", goarch)
	}
	base := o.Releases
	if base == "" {
		base = Releases
	}
	base = strings.TrimRight(base, "/")
	var url string
	if o.Version == "" {
		url = base + "/latest/download"
	} else {
		if _, ok := Parse(o.Version); !ok {
			return res, fmt.Errorf("not a release version: %q (expected vX.Y.Z)", o.Version)
		}
		url = base + "/download/" + o.Version
	}
	dir := o.Dir
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return res, err
		}
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		dir = filepath.Dir(exe)
	}
	hc := o.Client
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	name := Asset(goos, goarch)

	tmp, err := os.MkdirTemp("", "flopwire-upgrade-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)

	archive := filepath.Join(tmp, name)
	if err := fetch(ctx, hc, url+"/"+name, archive); err != nil {
		return res, err
	}
	sums := filepath.Join(tmp, "SHA256SUMS")
	if err := fetch(ctx, hc, url+"/SHA256SUMS", sums); err != nil {
		return res, err
	}
	want, err := expectedSum(sums, name)
	if err != nil {
		return res, err
	}
	got, err := fileSum(archive)
	if err != nil {
		return res, err
	}
	if got != want {
		return res, fmt.Errorf("checksum mismatch for %s: got %s, want %s", name, got, want)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	// Staged in the target directory, so the final rename stays on one
	// filesystem.
	staged, err := os.CreateTemp(dir, ".flopwire.*")
	if err != nil {
		return res, err
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	if err := extractBinary(archive, staged); err != nil {
		staged.Close()
		return res, err
	}
	if err := staged.Close(); err != nil {
		return res, err
	}
	if err := os.Chmod(stagedPath, 0o755); err != nil {
		return res, err
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, stagedPath, "version").Output()
	if err != nil {
		return res, fmt.Errorf("the downloaded binary does not run on this machine: %w", err)
	}
	res.Version = strings.TrimSpace(string(out))
	if o.Version != "" && res.Version != o.Version {
		return res, fmt.Errorf("the downloaded binary reports %s, not %s", res.Version, o.Version)
	}
	res.Binary = filepath.Join(dir, "flopwire")
	if err := os.Rename(stagedPath, res.Binary); err != nil {
		return res, err
	}
	if o.Restart != nil {
		res.Restart, err = o.Restart(ctx, res.Binary)
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

func fetch(ctx context.Context, hc *http.Client, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, io.LimitReader(resp.Body, 256<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// expectedSum finds name's digest in a SHA256SUMS file (`<hex>  <name>`).
func expectedSum(path, name string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS has no entry for %s", name)
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractBinary copies the archive's flopwire member into dst.
func extractBinary(archive string, dst io.Writer) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(archive), err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s has no flopwire binary", filepath.Base(archive))
		}
		if err != nil {
			return err
		}
		if filepath.Base(h.Name) == "flopwire" && h.Typeflag == tar.TypeReg {
			_, err = io.Copy(dst, io.LimitReader(tr, 512<<20))
			return err
		}
	}
}

// Latest resolves the newest release tag by following the releases/latest
// redirect, which needs no API token.
func Latest(ctx context.Context, hc *http.Client, releases string) (string, error) {
	if releases == "" {
		releases = Releases
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, strings.TrimRight(releases, "/")+"/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || loc == "" {
		return "", fmt.Errorf("releases/latest: HTTP %d, no redirect", resp.StatusCode)
	}
	tag := loc[strings.LastIndex(loc, "/")+1:]
	if _, ok := Parse(tag); !ok {
		return "", fmt.Errorf("releases/latest points at %q, not a release version", tag)
	}
	return tag, nil
}
