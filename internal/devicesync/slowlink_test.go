package devicesync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// slowLink stands for a link of rate bytes per second in front of a server
// that must read a whole request within deadline. It keeps time on a
// virtual clock: a request whose body would take longer than deadline to
// cross the link fails the way the server's read deadline fails it, and
// one that fits is passed on at full speed. Real sleeps and a real
// ReadTimeout made the outcome depend on scheduler load.
type slowLink struct {
	rate     int64 // bytes per second
	deadline time.Duration
	longest  atomic.Int64 // virtual nanoseconds of the slowest request
}

func (l *slowLink) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return nil, err
		}
		took := time.Duration(int64(len(body)) * int64(time.Second) / l.rate)
		for {
			old := l.longest.Load()
			if int64(took) <= old || l.longest.CompareAndSwap(old, int64(took)) {
				break
			}
		}
		if took > l.deadline {
			return nil, fmt.Errorf("slow link: a %d-byte request takes %v, past the server's %v read deadline: i/o timeout", len(body), took, l.deadline)
		}
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	return http.DefaultTransport.RoundTrip(r)
}

// S9: the server reads a whole request within its ReadTimeout (30s in
// production). A first sync of a large file on a slow link must be split
// into requests that each fit. Scaled down: an 8MB/s link and a 1s read
// timeout stand for 1.1Mbit/s and 30s, on a virtual clock. Bodies travel
// zstd-compressed (this text to about 63%), so the ~20MB file is ~12.6MB
// on the wire: the old 32MB cap sends it in one 1.5s request and must
// fail; the default 4MB cap sends 0.5s requests and must pass.
func TestSlowLinkRequestsFitReadTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	data := jsonlLines(70, 20000, 1000) // ~20MB, ~12.6MB compressed
	appendFile(t, path, data)
	sp := SourceSpec{Path: path, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: "codex@1"}

	for _, tc := range []struct {
		name   string
		cap    int64
		wantOK bool
	}{
		{"old 32MB cap", 32 << 20, false},
		{"default cap", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := synctest.New("tok")
			hs := httptest.NewServer(srv)
			defer hs.Close()
			sdir := t.TempDir()
			st, err := OpenStore(filepath.Join(sdir, "s.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			spool, _ := OpenSpool(filepath.Join(sdir, "spool"), 1<<30)
			link := &slowLink{rate: 8 << 20, deadline: time.Second}
			cl := &syncproto.Client{Server: hs.URL, Token: "tok", HTTP: &http.Client{Transport: link}}
			sy, err := NewSyncer(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), SealAfter: -1, MaxRequestBytes: tc.cap}, st, spool, cl)
			if err != nil {
				t.Fatal(err)
			}
			defer sy.Close()
			err = sy.Sync(context.Background(), sp)
			longest := time.Duration(link.longest.Load())
			if !tc.wantOK {
				if err == nil || longest <= time.Second {
					t.Fatalf("want a request past the read deadline; slowest took %v, sync error %v", longest, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("sync over a slow link: %v", err)
			}
			if longest > time.Second {
				t.Fatalf("slowest request took %v on the link", longest)
			}
			got, err := srv.Reconstruct(path, fileIDOf(t, path), 0)
			if err != nil || len(got) != len(data) {
				t.Fatalf("server holds %d of %d bytes: %v", len(got), len(data), err)
			}
		})
	}
}

// S9: with the default 4MB request cap, a device that does not know what
// the server holds (a fresh or rebuilt sync state) must still ask /has
// before sending bodies, not re-upload everything.
func TestDefaultConfigAsksHasBeforeResending(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 64<<20)
	data := jsonlLines(23, 20000, 300) // several MB
	a := e.spec("a.jsonl", transcript.StorageJSONLAppend)
	if err := os.WriteFile(a.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	e.sync(a)

	// A rebuilt sync state: same server, nothing known.
	st, err := OpenStore(e.path("sync2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sp, err := OpenSpool(e.path("spool2"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	sy, err := NewSyncer(Config{Chunk: small, SealAfter: -1, Logger: e.sy.cfg.Logger}, st, sp, e.client)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	e.srv.Lock()
	before := e.srv.BodyBytes
	e.srv.Unlock()
	if err := sy.Sync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	e.srv.Lock()
	sent := e.srv.BodyBytes - before
	e.srv.Unlock()
	if sent > int64(len(data))/10 {
		t.Fatalf("re-sent %d of %d bytes the server already held", sent, len(data))
	}
}
