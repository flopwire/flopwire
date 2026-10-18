package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

func (r *recorder) notice(path string) devicesync.Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.notices[path]
}

func TestSyncNoticeColdThenAppendAndRefresh(t *testing.T) {
	f := newFixture(t, "-")
	p := f.path(alphaRel)
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	f.once()
	for path, n := range f.rec.notices {
		if n.Kind != devicesync.NoticeHistorical {
			t.Fatalf("cold source %s classified changed", path)
		}
	}
	if n := f.rec.notice(p); !n.ActivityAt.Equal(old) {
		t.Fatalf("cold activity=%v expected=%v", n.ActivityAt, old)
	}
	appendFile(t, p, claudeUser("c1000000-0000-4000-8000-0000000000ee", "actual changed notice"))
	f.once()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	changed := f.rec.notice(p)
	if changed.Kind != devicesync.NoticeChanged || !changed.ActivityAt.Equal(fi.ModTime()) {
		t.Fatalf("append notice=%+v mtime=%v", changed, fi.ModTime())
	}
	if len(f.rec.flushed) != 0 {
		t.Fatal("ordinary append used interactive hook")
	}
	// A placement/policy refresh cannot promote the same past write again.
	if !f.a.notify(f.a.lookup(p, "")) {
		t.Fatal("source unexpectedly withheld")
	}
	refreshed := f.rec.notice(p)
	if refreshed.Kind != devicesync.NoticeHistorical || !refreshed.ActivityAt.Equal(changed.ActivityAt) {
		t.Fatalf("refresh notice=%+v", refreshed)
	}
	// A real extraction-version reparse is historical, even though it
	// writes a replacement local generation.
	target := f.a.lookup(p, "")
	target.parser = &versioned{Parser: target.parser, name: "claude@notice-next"}
	if written, err := f.a.indexTranscript(ctx, target); err != nil || !written {
		t.Fatalf("reparse written=%v error=%v", written, err)
	}
	if n := f.rec.notice(p); n.Kind != devicesync.NoticeHistorical || !n.ActivityAt.Equal(changed.ActivityAt) {
		t.Fatalf("reparse notice=%+v", n)
	}
}

func TestSyncNoticeDirectoryPeersAndNewSource(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	p := f.path(alphaRel)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// A cold peer first enumerated by an urgent directory scan is historical.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	cold := &target{path: p, src: transcript.Source{Agent: transcript.AgentClaude}, notice: devicesync.Notice{}}
	fi, err = os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	f.a.gateID(cold, transcript.IdentityOf(fi), time.Now(), true, fi.ModTime())
	f.a.mu.Lock()
	n := cold.notice
	f.a.mu.Unlock()
	if n.Kind != devicesync.NoticeHistorical || !n.ActivityAt.Equal(old) {
		t.Fatalf("cold directory peer=%+v", n)
	}
	// Re-statting an initial cold queue before indexing catches up is not
	// a new change merely because its saved watermark has fewer bytes.
	f.a.mu.Lock()
	cold.seen = transcript.IdentityOf(fi)
	cold.seen.Size--
	cold.seenAt = f.a.initializedAt.Add(-time.Hour).UnixNano()
	f.a.mu.Unlock()
	f.a.gateID(cold, transcript.IdentityOf(fi), time.Now(), true, fi.ModTime())
	f.a.mu.Lock()
	again := cold.notice
	f.a.mu.Unlock()
	if again.Kind != devicesync.NoticeHistorical {
		t.Fatalf("cold queued restat=%+v", again)
	}
	// A newly written source discovered after startup has actual write evidence.
	appendFile(t, p, "\n")
	fi, err = os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	fresh := &target{path: p, src: transcript.Source{Agent: transcript.AgentClaude}}
	f.a.gateID(fresh, transcript.IdentityOf(fi), time.Now(), false, fi.ModTime())
	// This target has not yet been handed to sync.
	f.a.mu.Lock()
	freshNotice := fresh.notice
	f.a.mu.Unlock()
	if n := freshNotice; n.Kind != devicesync.NoticeChanged || !n.ActivityAt.Equal(fi.ModTime()) {
		t.Fatalf("new source notice=%+v", n)
	}
}

func TestSyncNoticeExportInitialIncrementalReparseAndRestart(t *testing.T) {
	path, db := buildDevin(t)
	f := newFixture(t, path)
	f.once()
	sessions := []string{"devin-oracle-001", "devin-oracle-002"}
	initial, err := f.store.SessionActivities(ctx, transcript.AgentDevin, sessions)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		n := f.rec.notice(devin.ExportPath(path, session))
		if n.Kind != devicesync.NoticeHistorical || !n.ActivityAt.Equal(initial[session]) {
			t.Fatalf("initial %s notice=%+v indexed=%v", session, n, initial[session])
		}
	}
	if initial[sessions[0]].Equal(initial[sessions[1]]) {
		t.Fatal("fixture does not distinguish individual session activity")
	}
	const at = int64(1790157900)
	if _, err := db.Exec(`INSERT INTO message_nodes(session_id,node_id,parent_node_id,chat_message,created_at) VALUES('devin-oracle-001',8,7,'{"message_id":"notice-8","role":"user","content":"fresh notice","metadata":{"is_user_input":true}}',?)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET main_chain_id=8,last_activity_at=? WHERE id='devin-oracle-001'`, at); err != nil {
		t.Fatal(err)
	}
	id, err := transcript.StatIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.a.parseStore(ctx, &f.a.devin, id, false); err != nil {
		t.Fatal(err)
	}
	n := f.rec.notice(devin.ExportPath(path, sessions[0]))
	if n.Kind != devicesync.NoticeChanged || !n.ActivityAt.Equal(time.Unix(at, 0)) {
		t.Fatalf("incremental session notice=%+v", n)
	}
	if other := f.rec.notice(devin.ExportPath(path, sessions[1])); other.Kind != devicesync.NoticeHistorical || !other.ActivityAt.Equal(initial[sessions[1]]) {
		t.Fatalf("other session promoted: %+v", other)
	}
	if err := f.a.parseStore(ctx, &f.a.devin, id, true); err != nil {
		t.Fatal(err)
	}
	if reparsed := f.rec.notice(devin.ExportPath(path, sessions[0])); reparsed.Kind != devicesync.NoticeHistorical || !reparsed.ActivityAt.Equal(n.ActivityAt) {
		t.Fatalf("reparse=%+v", reparsed)
	}
	// The incremental cursor has no new rows on restart. Initial listing
	// must nevertheless restore the original per-session metadata in batches.
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	f.rec = newRecorder()
	f.cfg.Sync = f.rec
	f.restart()
	f.once()
	for _, session := range sessions {
		expected := initial[session]
		if session == sessions[0] {
			expected = n.ActivityAt
		}
		restored := f.rec.notice(devin.ExportPath(path, session))
		if restored.Kind != devicesync.NoticeHistorical || !restored.ActivityAt.Equal(expected) {
			t.Fatalf("restart %s=%+v expected=%v", session, restored, expected)
		}
	}
}

func TestStoreSinkKeepsMaxIndividualActivity(t *testing.T) {
	sink := &storeSink{}
	a, b := time.Unix(1000, 0), time.Unix(2000, 0)
	sink.noteActivity("a", b)
	sink.noteActivity("a", a)
	sink.noteActivity("b", a)
	sink.noteActivity("unknown", time.Time{})
	if !sink.touched["a"].Equal(b) || !sink.touched["b"].Equal(a) {
		t.Fatalf("activity=%+v", sink.touched)
	}
	if at, ok := sink.touched["unknown"]; !ok || !at.IsZero() {
		t.Fatal("undated new message lost its changed provenance")
	}
}

func TestSyncNoticeVerifiedRewriteWithPreservedMtime(t *testing.T) {
	f := newFixture(t, "-")
	p := f.path(alphaRel)
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	f.once()
	// Pure ctime changes must not promote a cold session.
	if err := os.Chmod(p, 0600); err != nil {
		t.Fatal(err)
	}
	f.once()
	if n := f.rec.notice(p); n.Kind != devicesync.NoticeHistorical {
		t.Fatalf("chmod classified changed: %+v", n)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(raw, []byte("why does the login test flake?"), []byte("why does the logon test flake?"), 1)
	if bytes.Equal(raw, changed) || len(raw) != len(changed) {
		t.Fatal("fixture lacks same-size replacement")
	}
	before, err := transcript.StatIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	after, err := transcript.StatIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	if before.ID != after.ID || before.Size != after.Size {
		t.Fatal("rewrite changed identity or size")
	}
	f.once()
	n := f.rec.notice(p)
	if n.Kind != devicesync.NoticeChanged || !n.ActivityAt.Equal(old) {
		t.Fatalf("verified rewrite notice=%+v", n)
	}
}

type noticeDuringHandoff struct {
	*recorder
	during func()
}

func (r *noticeDuringHandoff) NotifyWithNotice(spec devicesync.SourceSpec, notice devicesync.Notice) {
	r.recorder.NotifyWithNotice(spec, notice)
	r.during()
}

func TestSyncNoticePreservesNewChangeDuringHandoffWithSameActivity(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	target := f.a.lookup(f.path(alphaRel), "")
	f.a.mu.Lock()
	target.notice.Kind = devicesync.NoticeChanged
	activity := target.notice.ActivityAt
	identity := target.seen
	identity.Size++
	f.a.mu.Unlock()
	// A new write is gated while the previous nonblocking notification is
	// handed to the scheduler, even if its mtime has the same timestamp.
	f.a.cfg.Sync = &noticeDuringHandoff{recorder: f.rec, during: func() { f.a.gateID(target, identity, time.Now(), true, activity) }}
	if !f.a.notify(target) {
		t.Fatal("source unexpectedly withheld")
	}
	f.a.mu.Lock()
	pending := target.notice
	f.a.mu.Unlock()
	if pending.Kind != devicesync.NoticeChanged || !pending.ActivityAt.Equal(activity) {
		t.Fatalf("new gate erased during handoff: %+v", pending)
	}
}

type observedExportNotice struct {
	*recorder
	delivered func()
}

func (r *observedExportNotice) NotifyExportFuncWithNotice(spec devicesync.SourceSpec, fn devicesync.ExportFunc, notice devicesync.Notice) {
	r.recorder.NotifyExportFuncWithNotice(spec, fn, notice)
	r.delivered()
}

func TestSyncNoticeExportSurvivesActivityReadFailureAfterCursorCommit(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		cancelRead, deleteSession bool
	}{
		{name: "transient metadata read"},
		{name: "canceled metadata read", cancelRead: true},
		{name: "unknown deletion activity", deleteSession: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, db := buildDevin(t)
			f := newFixture(t, path)
			realRead := f.a.sessionActivities
			reads := 0
			var beforeCursor []byte
			pollCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			f.a.sessionActivities = func(readCtx context.Context, agent transcript.Agent, sessions []string) (map[string]time.Time, error) {
				reads++
				if reads == 2 {
					committed, err := f.store.Source(ctx, f.a.devin.sourceID)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Equal(committed.CursorState, beforeCursor) {
						t.Fatal("fault did not occur after incremental cursor advanced")
					}
					if tc.cancelRead {
						cancel()
						return realRead(readCtx, agent, sessions)
					}
					return nil, errors.New("synthetic transient metadata read failure")
				}
				return realRead(readCtx, agent, sessions)
			}
			f.once() // the initial activity lookup succeeds normally
			before, err := f.store.Source(ctx, f.a.devin.sourceID)
			if err != nil {
				t.Fatal(err)
			}
			beforeCursor = append([]byte(nil), before.CursorState...)
			f.rec = newRecorder()
			notifications := 0
			f.a.cfg.Sync = &observedExportNotice{recorder: f.rec, delivered: func() { notifications++ }}
			session := "devin-oracle-001"
			const at = int64(1790157900)
			if tc.deleteSession {
				session = "devin-oracle-002"
				if _, err := db.Exec(`DELETE FROM message_nodes WHERE session_id=?`, session); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`DELETE FROM sessions WHERE id=?`, session); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.Exec(`INSERT INTO message_nodes(session_id,node_id,parent_node_id,chat_message,created_at) VALUES('devin-oracle-001',8,7,'{"message_id":"metadata-failure-8","role":"user","content":"committed export obligation","metadata":{"is_user_input":true}}',?)`, at); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE sessions SET main_chain_id=8,last_activity_at=? WHERE id='devin-oracle-001'`, at); err != nil {
					t.Fatal(err)
				}
			}
			id, err := transcript.StatIdentity(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.a.parseStore(pollCtx, &f.a.devin, id, false); err != nil {
				t.Fatalf("optional activity lookup blocked committed notification: %v", err)
			}
			exportPath := devin.ExportPath(path, session)
			queued, ok := f.rec.spec(exportPath)
			if !ok || queued.SessionKey != session {
				t.Fatalf("committed export not queued: %+v", queued)
			}
			notice := f.rec.notice(exportPath)
			expected := time.Unix(at, 0)
			if tc.deleteSession {
				expected = time.Time{}
			}
			if notice.Kind != devicesync.NoticeChanged || !notice.ActivityAt.Equal(expected) {
				t.Fatalf("fallback notice=%+v expected=%v", notice, expected)
			}
			f.rec.mu.Lock()
			export := f.rec.exports[exportPath]
			f.rec.mu.Unlock()
			if export == nil {
				t.Fatal("queued export has no producer")
			}
			payload, err := export(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			needle := []byte("committed export obligation")
			if tc.deleteSession {
				needle = []byte(`"t":"gone"`)
			}
			if !bytes.Contains(payload.Data, needle) {
				t.Fatalf("queued producer missed original committed change: %s", payload.Data)
			}
			if notifications != 1 {
				t.Fatalf("committed export notifications=%d", notifications)
			}
			harnessID, err := transcript.StatIdentity(path)
			if err != nil {
				t.Fatal(err)
			}
			// Retry with an unchanged harness and a healthy metadata reader. Its
			// advanced cursor has no touched sessions; the original export must
			// already be queued rather than depending on a new write or restart.
			if err := f.a.parseStore(ctx, &f.a.devin, id, false); err != nil {
				t.Fatal(err)
			}
			afterID, err := transcript.StatIdentity(path)
			if err != nil {
				t.Fatal(err)
			}
			if afterID != harnessID {
				t.Fatal("retry required another harness change")
			}
			if notifications != 1 {
				t.Fatalf("unchanged retry emitted additional touched sessions: %d", notifications)
			}
			if reads != 3 {
				t.Fatalf("metadata reads=%d", reads)
			}
			if preserved := f.rec.notice(exportPath); preserved != notice {
				t.Fatalf("unchanged retry replaced original queued notice: %+v", preserved)
			}
		})
	}
}
