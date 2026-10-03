package local

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

// TestRepoFilterLatency times --repo on a 10k-conversation index (20
// messages each) where one repository, acme/web, has 2,000 sessions
// spread over N checkouts (linked worktrees, placed as the agent places
// them), for N = 1, 100 and 1,024 (#102 review). It reports sessions and
// grep wall time and checks every message of the repository is found.
//
//	FLOPWIRE_BENCH=1 go test -run TestRepoFilterLatency -timeout 30m -v ./internal/retrieval/local/
func TestRepoFilterLatency(t *testing.T) {
	if os.Getenv("FLOPWIRE_BENCH") == "" {
		t.Skip("set FLOPWIRE_BENCH=1")
	}
	for _, n := range []int{1, 100, 1024} {
		s, err := localindex.Open(filepath.Join(t.TempDir(), "bench.db"), localindex.Options{RepoRoot: func(cwd string) string { return cwd }})
		if err != nil {
			t.Fatal(err)
		}
		const convs, per, inRepo = 10_000, 20, 2_000
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for src := 0; src < convs/100; src++ {
			st, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: fmt.Sprintf("/synthetic/%d.jsonl", src),
				FileID: transcript.FileID{Dev: 1, Ino: uint64(src + 1)}, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
			if err != nil {
				t.Fatal(err)
			}
			batch := localindex.Batch{SourceID: st.ID, Generation: 1}
			for c := range 100 {
				i := src*100 + c
				sess := fmt.Sprintf("sess-%05d", i)
				var pl pathpolicy.Placement
				if i < inRepo {
					wt := fmt.Sprintf("/w/web-wt-%04d", i%n)
					if i%n == 0 && n == 1 {
						wt = "/w/web"
					}
					pl = pathpolicy.Placement{Cwd: wt, Worktree: wt, Main: "/w/web", Remote: "github.com/acme/web"}
				} else {
					d := fmt.Sprintf("/w/other-%03d", i%300)
					pl = pathpolicy.Placement{Cwd: d, Worktree: d, Main: d, Remote: fmt.Sprintf("github.com/acme/other-%03d", i%300)}
				}
				if err := s.SavePlacement(ctx, localindex.Placement{Agent: transcript.AgentClaude, SessionID: sess, How: localindex.PlacedByWorktree, Placement: pl}); err != nil {
					t.Fatal(err)
				}
				batch.Conversations = append(batch.Conversations, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: sess, Cwd: pl.Cwd})
				for j := range per {
					text := fmt.Sprintf("retry the upload %d of %s after a connection reset", j, sess)
					m := &transcript.Message{SessionID: sess, NativeID: fmt.Sprintf("m%d-%d", i, j), Ordinal: int64(j), Kind: transcript.KindAssistant,
						TS: base.Add(time.Duration(i*per+j) * time.Second), LineNo: int64(j + 1), ByteOffset: int64(j) * 100, ByteLen: 100,
						Parser: "claude@1", Text: text, FullLen: len(text)}
					m.ContentSHA = sha256.Sum256([]byte(text))
					batch.Messages = append(batch.Messages, m)
				}
			}
			if _, err := s.ApplyBatch(ctx, batch); err != nil {
				t.Fatal(err)
			}
		}
		b := &Backend{Store: s}
		best := func(fn func() error) time.Duration {
			var min time.Duration
			for range 3 {
				t0 := time.Now()
				if err := fn(); err != nil {
					t.Fatal(err)
				}
				if d := time.Since(t0); min == 0 || d < min {
					min = d
				}
			}
			return min
		}
		var ss *format.Sessions
		var p *format.Page
		dSess := best(func() (err error) {
			ss, err = b.Sessions(ctx, "", "", format.Filters{Repo: "github.com/acme/web", Limit: 500})
			return err
		})
		dGrep := best(func() (err error) {
			p, err = b.Grep(ctx, format.GrepQuery{Pattern: "connection reset", Fixed: true, Mode: format.ModeCount}, format.Filters{Repo: "github.com/acme/web"})
			return err
		})
		dAll := best(func() error {
			_, err := b.Grep(ctx, format.GrepQuery{Pattern: "connection reset", Fixed: true, Mode: format.ModeCount}, format.Filters{})
			return err
		})
		t.Logf("checkouts %4d: sessions %d in %s; grep %d hits in %s (no repo filter: %s); best of 3", n, len(ss.Sessions), dSess.Round(time.Millisecond), p.Total, dGrep.Round(time.Millisecond), dAll.Round(time.Millisecond))
		if p.Total != inRepo*per {
			t.Errorf("checkouts %d: %d hits, want %d", n, p.Total, inRepo*per)
		}
		s.Close()
	}
}
