package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.2.1", "v0.2.0", false},
		{"v0.9.9", "v1.0.0", true},
		{"v1.0.0", "v0.99.0", false},
		{"dev", "v0.2.0", false},
		{"abc123def456", "v0.2.0", false},
		{"v0.1.0", "dev", false},
		{"v0.1.0", "v0.2.0-next", false},
		{"v0.1.0", "0.2.0", true},
		{"v01.0.0", "v2.0.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
	if n := Notice("v0.1.0", "v0.2.0"); !strings.Contains(n, "v0.2.0 is available") || !strings.Contains(n, InstallCommand) {
		t.Errorf("Notice = %q", n)
	}
	if n := Notice("v0.2.0", "v0.2.0"); n != "" {
		t.Errorf("Notice same = %q", n)
	}
}

// fakeRelease serves a release whose flopwire is a shell script printing
// version, under both latest/download and download/<tag>.
func fakeRelease(t *testing.T, mux *http.ServeMux, prefix, version string, tamper bool) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := fmt.Sprintf("#!/bin/sh\necho %s\n", version)
	for _, f := range []struct{ name, body string }{{"LICENSE", "license"}, {"flopwire", body}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	if tamper {
		archive = append(append([]byte{}, archive...), 'x')
	}
	name := Asset(runtime.GOOS, runtime.GOARCH)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
	mux.HandleFunc(prefix+"/"+name, func(w http.ResponseWriter, _ *http.Request) { w.Write(archive) })
	mux.HandleFunc(prefix+"/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(sums)) })
}

func TestInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	mux := http.NewServeMux()
	fakeRelease(t, mux, "/latest/download", "v9.9.9", false)
	fakeRelease(t, mux, "/download/v9.9.8", "v9.9.8", false)
	fakeRelease(t, mux, "/download/v9.9.7", "v9.9.7", true)
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://github.com/flopwire/flopwire/releases/tag/v9.9.9", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dir := t.TempDir()
	ctx := context.Background()

	restarted := ""
	res, err := Install(ctx, Options{Dir: dir, Releases: srv.URL, Restart: func(_ context.Context, b string) (string, error) {
		restarted = b
		return "restarted", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "v9.9.9" || res.Binary != filepath.Join(dir, "flopwire") || res.Restart != "restarted" || restarted != res.Binary {
		t.Fatalf("latest: %+v restarted=%q", res, restarted)
	}
	if out, _ := exec.Command(res.Binary, "version").Output(); strings.TrimSpace(string(out)) != "v9.9.9" {
		t.Fatalf("installed binary prints %q", out)
	}

	res, err = Install(ctx, Options{Dir: dir, Releases: srv.URL, Version: "v9.9.8"})
	if err != nil || res.Version != "v9.9.8" {
		t.Fatalf("pinned: %+v %v", res, err)
	}

	_, err = Install(ctx, Options{Dir: dir, Releases: srv.URL, Version: "v9.9.7"})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered: err = %v", err)
	}
	if out, _ := exec.Command(filepath.Join(dir, "flopwire"), "version").Output(); strings.TrimSpace(string(out)) != "v9.9.8" {
		t.Fatalf("tampered install replaced the binary: %q", out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("staging files left behind: %v", entries)
	}

	if _, err := Install(ctx, Options{Dir: dir, Releases: srv.URL, Version: "main"}); err == nil {
		t.Fatal("non-release version accepted")
	}

	tag, err := Latest(ctx, srv.Client(), srv.URL)
	if err != nil || tag != "v9.9.9" {
		t.Fatalf("Latest = %q, %v", tag, err)
	}
}
