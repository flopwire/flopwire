package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

func codexRec(ord int, typ, payload string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-15T10:00:%02d.000Z","type":%q,"payload":%s}`, ord%60, typ, payload)
}

// D11 on the server: a Codex fork without a history marker skips the
// parent history it copied, reading the parent's rollout from the archive.
func TestServerForkSkipsCopiedHistory(t *testing.T) { testServerFork(t, false) }

// A transient error reading the fork parent (object storage down) is not
// "parent missing": the fork's parse fails and retries, and once the
// parent reads again the copied history is still skipped.
func TestServerForkParentReadErrorRetries(t *testing.T) { testServerFork(t, true) }

// flakyObjects fails Gets of the keys Put while recording, while failing.
type flakyObjects struct {
	Objects
	mu              sync.Mutex
	record, failing bool
	keys            map[string]bool
}

func (f *flakyObjects) Put(ctx context.Context, key string, data []byte) error {
	f.mu.Lock()
	if f.record {
		f.keys[key] = true
	}
	f.mu.Unlock()
	return f.Objects.Put(ctx, key, data)
}

func (f *flakyObjects) Get(ctx context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	fail := f.failing && f.keys[key]
	f.mu.Unlock()
	if fail {
		return nil, errors.New("object storage unavailable")
	}
	return f.Objects.Get(ctx, key)
}

func testServerFork(t *testing.T, flakyParent bool) {
	e := newEnv(t)
	flaky := &flakyObjects{Objects: e.objects, keys: map[string]bool{}}
	if flakyParent {
		e.objects = flaky
		e.start()
	}
	dir := filepath.Join(t.TempDir(), ".codex", codex.SessionsDir, "2026", "09", "15")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	parentID, childID := "019f0000-0000-7000-8000-00000000f101", "019f0000-0000-7000-8000-00000000f102"
	parent := []string{
		codexRec(0, "session_meta", `{"id":"`+parentID+`","cwd":"/x","source":"cli"}`),
		codexRec(1, "event_msg", `{"type":"task_started","turn_id":"pt1"}`),
		codexRec(2, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"parent prompt"}]}`),
		codexRec(3, "event_msg", `{"type":"user_message","message":"parent prompt","turn_id":"pt1"}`),
		codexRec(4, "response_item", `{"type":"message","id":"msg_p1","role":"assistant","content":[{"type":"output_text","text":"parent answer"}]}`),
		codexRec(5, "event_msg", `{"type":"task_complete","turn_id":"pt1"}`),
	}
	child := append([]string{codexRec(0, "session_meta", `{"id":"`+childID+`","forked_from_id":"`+parentID+`","cwd":"/x","source":"cli"}`)}, parent[1:]...)
	child = append(child,
		codexRec(8, "event_msg", `{"type":"task_started","turn_id":"ct1"}`),
		codexRec(9, "turn_context", `{"turn_id":"ct1"}`),
		codexRec(10, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"child prompt"}]}`),
		codexRec(11, "response_item", `{"type":"message","id":"msg_c1","role":"assistant","content":[{"type":"output_text","text":"child answer"}]}`),
	)
	spec := func(id string, lines []string) devicesync.SourceSpec {
		p := filepath.Join(dir, "rollout-2026-09-15T10-00-00-"+id+".jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return devicesync.SourceSpec{Path: p, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name}
	}
	// The flaky variant seals the parent's tail into a chunk object, so
	// reading it goes through object storage.
	sealAfter := time.Duration(-1)
	if flakyParent {
		sealAfter = time.Millisecond
	}
	sy := e.syncer(devicesync.Config{SealAfter: sealAfter})
	ps := spec(parentID, parent)
	time.Sleep(10 * time.Millisecond)
	flaky.record = true
	sync1(t, sy, ps)
	flaky.record = false
	if flakyParent && len(flaky.keys) == 0 {
		t.Fatal("the parent was not sealed into a chunk object")
	}
	e.drain()
	// The server reads the parent from its archive, not the device's disk.
	if err := os.Remove(ps.Path); err != nil {
		t.Fatal(err)
	}
	sync1(t, sy, spec(childID, child))
	if flakyParent {
		flaky.mu.Lock()
		flaky.failing = true
		flaky.mu.Unlock()
		if err := e.queue.Drain(e.ctx); err == nil {
			t.Fatal("the fork parsed although its parent could not be read")
		}
		flaky.mu.Lock()
		flaky.failing = false
		flaky.mu.Unlock()
	}
	e.drain()
	rows := func(session string) []string {
		r, err := e.pool.Query(e.ctx, `SELECT m.text FROM messages m JOIN conversations c ON c.id=m.conversation_id
			WHERE c.session_id=$1 AND NOT m.superseded ORDER BY m.ordinal`, session)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var out []string
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	t.Logf("child %q parent %q", rows(childID), rows(parentID))
	if got := strings.Join(rows(childID), "|"); got != "child prompt|child answer" {
		t.Fatalf("fork rows %q", got)
	}
	if got := strings.Join(rows(parentID), "|"); !strings.Contains(got, "parent prompt") {
		t.Fatalf("parent rows %q", got)
	}
}

// D20: a moved file's source names its old path as Previous. When the old
// path's source reaches the server after the new one, the link is still
// made and the old copy's rows are superseded, not left live twice.
func TestPreviousLinkResolvesWhenOldSourceArrivesLate(t *testing.T) {
	e := newEnv(t)
	sid := "019f0000-0000-7000-8000-00000000f201"
	data := []byte(strings.Join([]string{
		codexRec(0, "session_meta", `{"id":"`+sid+`","cwd":"/x","source":"cli"}`),
		codexRec(1, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"moved prompt"}]}`),
		codexRec(2, "response_item", `{"type":"message","id":"msg_m1","role":"assistant","content":[{"type":"output_text","text":"moved answer"}]}`),
	}, "\n") + "\n")
	oldPath := "/h/.codex/sessions/2026/09/15/rollout-2026-09-15T10-00-00-" + sid + ".jsonl"
	newPath := "/h/.codex/archived_sessions/rollout-2026-09-15T10-00-00-" + sid + ".jsonl"
	flush := func(path string, prev *syncproto.SourceRef) {
		t.Helper()
		h := syncproto.Sum(data)
		resp, err := e.client.Flush(e.ctx, &syncproto.FlushRequest{
			Header: syncproto.FlushHeader{Version: syncproto.Version, CapturedAt: time.Now(),
				Source:  syncproto.Source{Path: path, FileID: "1:7", Agent: "codex", StorageKind: "jsonl_append", Parser: codex.Name, SessionKey: sid, Previous: prev},
				Entries: []syncproto.Entry{{Ordinal: 0, Hash: h, Offset: 0, Size: int64(len(data))}},
				Bodies:  bodyOf(data)},
			Payload: zpayload(data)})
		if err != nil || resp.Status != syncproto.StatusOK {
			t.Fatalf("flush %s: %+v %v", path, resp, err)
		}
	}
	flush(newPath, &syncproto.SourceRef{Path: oldPath, FileID: "1:7"})
	e.drain()
	flush(oldPath, nil)
	e.drain()
	if n := e.count(`SELECT count(*) FROM sources n JOIN sources o ON o.id=n.previous_source_id WHERE n.path=$1 AND o.path=$2`, newPath, oldPath); n != 1 {
		t.Fatal("previous link not made when the old source arrived")
	}
	live := func(path string) int {
		return e.count(`SELECT count(*) FROM messages m JOIN sources s ON s.id=m.source_id WHERE s.path=$1 AND NOT m.superseded`, path)
	}
	if o, n := live(oldPath), live(newPath); o != 0 || n == 0 {
		t.Fatalf("live rows: old %d, new %d", o, n)
	}
}

// A path whose file identity goes 1:7 -> 1:8 -> 1:7 (inode reuse): each new
// identity names the previous one. The current file's rows must stay live.
func TestPreviousCycleKeepsCurrentRowsLive(t *testing.T) {
	e := newEnv(t)
	sid := "019f0000-0000-7000-8000-00000000f301"
	path := "/h/.codex/sessions/2026/09/15/rollout-2026-09-15T10-00-00-" + sid + ".jsonl"
	body := func(text string) []byte {
		return []byte(strings.Join([]string{
			codexRec(0, "session_meta", `{"id":"`+sid+`","cwd":"/x","source":"cli"}`),
			codexRec(1, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"`+text+`"}]}`),
		}, "\n") + "\n")
	}
	flush := func(gen int64, fid string, prev *syncproto.SourceRef, data []byte) {
		t.Helper()
		h := syncproto.Sum(data)
		resp, err := e.client.Flush(e.ctx, &syncproto.FlushRequest{
			Header: syncproto.FlushHeader{Version: syncproto.Version, CapturedAt: time.Now(), Generation: gen,
				Source:  syncproto.Source{Path: path, FileID: fid, Agent: "codex", StorageKind: "jsonl_append", Parser: codex.Name, SessionKey: sid, Previous: prev},
				Entries: []syncproto.Entry{{Ordinal: 0, Hash: h, Offset: 0, Size: int64(len(data))}},
				Bodies:  bodyOf(data)},
			Payload: zpayload(data)})
		if err != nil || resp.Status != syncproto.StatusOK {
			t.Fatalf("flush gen %d: %+v %v", gen, resp, err)
		}
		e.drain()
	}
	flush(1, "1:7", nil, body("first"))
	flush(2, "1:8", &syncproto.SourceRef{Path: path, FileID: "1:7"}, body("second"))
	flush(3, "1:7", &syncproto.SourceRef{Path: path, FileID: "1:8"}, body("third"))
	if n := e.count(`SELECT count(*) FROM messages m WHERE NOT m.superseded AND m.text LIKE '%third%'`); n == 0 {
		t.Fatalf("current file's rows are superseded; live=%d", e.count(`SELECT count(*) FROM messages WHERE NOT superseded`))
	}
}
