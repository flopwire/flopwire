package ingest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/redact/redacttest"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/antigravity"
	"github.com/google/uuid"
)

func TestAntigravityFileIngest(t *testing.T) {
	e := newEnv(t)
	const sessionID = "20000000-0000-4000-8000-0000000000a1"
	path := filepath.Join(t.TempDir(), ".gemini", "antigravity-cli", "brain", sessionID, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := devicesync.SourceSpec{Path: path, Agent: transcript.AgentAntigravity, StorageKind: transcript.StorageJSONLAppend, SessionKey: sessionID, Parser: antigravity.ParserName}
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	secret := redacttest.Needles()["GITHUB_PAT"]
	if secret == "" {
		t.Fatal("missing GitHub token fixture")
	}
	prompt := func(index int, message string) []byte {
		t.Helper()
		content, err := json.Marshal("<USER_REQUEST>\n" + message + "\n</USER_REQUEST>")
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Appendf(nil, `{"step_index":%d,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-01-01T00:00:00Z","content":%s}`+"\n", index, content)
	}
	sync := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := sy.Sync(e.ctx, spec); err != nil {
			t.Fatal(err)
		}
		e.drain()
	}
	texts := func() []string {
		t.Helper()
		rows, err := e.pool.Query(e.ctx, `SELECT m.text FROM messages m JOIN conversations c ON c.id=m.conversation_id
			WHERE NOT m.superseded AND c.agent=$1 AND c.session_id=$2 ORDER BY m.ordinal`, spec.Agent, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				t.Fatal(err)
			}
			out = append(out, text)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := prompt(0, "first Antigravity prompt")
	sync(first)
	if got := texts(); !slices.Equal(got, []string{"first Antigravity prompt"}) {
		t.Fatalf("initial transcript: %q", got)
	}
	sync(append(slices.Clone(first), prompt(1, "second Antigravity prompt "+secret)...))
	if got := texts(); len(got) != 2 || got[0] != "first Antigravity prompt" || !strings.HasPrefix(got[1], "second Antigravity prompt ") || strings.Contains(got[1], secret) {
		t.Fatalf("appended transcript has %d rows or failed redaction", len(got))
	}
	redactions, err := e.queue.Redactions(e.ctx)
	if err != nil || redactions.Device["github-token"] == 0 {
		t.Fatalf("device redactions: %+v, %v", redactions, err)
	}
	sync(prompt(0, "rewritten Antigravity prompt"))
	if got := texts(); !slices.Equal(got, []string{"rewritten Antigravity prompt"}) {
		t.Fatalf("rewritten transcript: %q", got)
	}
	e.exec(`UPDATE source_parse_state SET applied_parser='antigravity@0'`)
	id, err := e.queue.nextRefresh(e.ctx)
	if err != nil || id == "" {
		t.Fatalf("stale Antigravity parser was not queued: %q, %v", id, err)
	}
	if err := e.queue.refreshSource(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := texts(); !slices.Equal(got, []string{"rewritten Antigravity prompt"}) {
		t.Fatalf("refreshed transcript: %q", got)
	}
}

func TestAntigravityLineageBackfillsAnExistingConversation(t *testing.T) {
	e := newEnv(t)
	const child = "20000000-0000-4000-8000-0000000000a1"
	const parent = "20000000-0000-4000-8000-0000000000a2"
	data := []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-01-01T00:00:00Z","content":"<USER_REQUEST>\nSynthetic Antigravity prompt\n</USER_REQUEST>"}` + "\n")
	captured := time.Now().UTC().Truncate(time.Microsecond)
	send := func(id string, lineage *syncproto.AntigravityLineage, withBody bool, observedAt time.Time) *syncproto.FlushResponse {
		t.Helper()
		req := &syncproto.FlushRequest{Header: syncproto.FlushHeader{
			Version: syncproto.Version, CapturedAt: observedAt,
			Source: syncproto.Source{
				Path: "/synthetic/brain/" + id + "/.system_generated/logs/transcript.jsonl", FileID: "1:1",
				Agent: "antigravity", StorageKind: "jsonl_append", SessionKey: id,
				Parser: antigravity.ParserName, AntigravityLineage: lineage,
			},
		}}
		if lineage != nil {
			req.Header.Source.AntigravityLineageObservedAt = observedAt.UnixMicro()
		}
		if withBody {
			req.Header.Entries = []syncproto.Entry{{Ordinal: 0, Hash: syncproto.Sum(data), Size: int64(len(data))}}
			req.Header.Bodies = bodyOf(data)
			req.Payload = zpayload(data)
		}
		resp, err := e.client.Flush(e.ctx, req)
		if err != nil || resp.Status != syncproto.StatusOK {
			t.Fatalf("flush %s: %+v, %v", id, resp, err)
		}
		e.drain()
		return resp
	}
	check := func(wantParent string, wantDepth int, linked bool) {
		t.Helper()
		var gotParent string
		var gotDepth int
		var linkedParent *string
		err := e.pool.QueryRow(e.ctx, `SELECT COALESCE(c.parent_native_session_id,''),c.depth,p.device_id::text
			FROM conversations c LEFT JOIN conversations p ON p.id=c.parent_conversation_id
			WHERE c.device_id=$1 AND c.session_id=$2`, e.deviceID, child).Scan(&gotParent, &gotDepth, &linkedParent)
		if err != nil || gotParent != wantParent || gotDepth != wantDepth ||
			(linkedParent != nil) != linked || linkedParent != nil && *linkedParent != e.deviceID {
			t.Fatalf("lineage parent=%q depth=%d linked device=%v, err=%v", gotParent, gotDepth, linkedParent, err)
		}
		if n := e.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.device_id=$1 AND c.session_id=$2 AND NOT m.superseded`, e.deviceID, child); n != 1 {
			t.Fatalf("child has %d messages after metadata flush, want 1", n)
		}
	}

	checkParentCount := func(want int) {
		t.Helper()
		if n := e.count(`SELECT COALESCE((digest->>'subagents')::int,0) FROM conversation_activity a JOIN conversations c ON c.id=a.conversation_id WHERE c.device_id=$1 AND c.session_id=$2`, e.deviceID, parent); n != want {
			t.Fatalf("parent digest counts %d children, want %d", n, want)
		}
	}

	if resp := send(child, nil, true, captured); resp.AntigravityLineage != nil {
		t.Fatalf("legacy flush unexpectedly acknowledged lineage: %+v", resp)
	}
	check("", 0, false)

	otherDevice := uuid.NewString()
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'other','darwin',now())`, otherDevice, e.userID)
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,'antigravity',$2,$3,$4)`, uuid.NewString(), parent, otherDevice, e.userID)

	lineage := &syncproto.AntigravityLineage{ParentSessionID: parent, Depth: 1}
	if resp := send(child, lineage, false, captured.Add(time.Second)); resp.AntigravityLineage == nil || *resp.AntigravityLineage != *lineage {
		t.Fatalf("metadata-only flush did not acknowledge lineage: %+v", resp)
	}
	check(parent, 1, false)

	send(parent, nil, true, captured.Add(2*time.Second))
	check(parent, 1, true)
	checkParentCount(1)

	send(child, nil, false, captured.Add(3*time.Second))
	check(parent, 1, true)

	root := &syncproto.AntigravityLineage{}
	if resp := send(child, root, false, captured.Add(4*time.Second)); resp.AntigravityLineage == nil || *resp.AntigravityLineage != *root {
		t.Fatalf("explicit root did not clear lineage: %+v", resp)
	}
	check("", 0, false)
	checkParentCount(0)
	if resp := send(child, lineage, false, captured.Add(time.Second)); resp.AntigravityLineage != nil {
		t.Fatalf("a stale header unexpectedly acknowledged old lineage: %+v", resp)
	}
	check("", 0, false)

	const grandchild = "20000000-0000-4000-8000-0000000000a3"
	send(grandchild, &syncproto.AntigravityLineage{ParentSessionID: child, Depth: 1}, true, captured.Add(5*time.Second))
	e.exec(`UPDATE conversations SET hidden_at=now(),hidden_rule='test',hidden_rules_version=1,hidden_root=id,hidden_scope='device'
		WHERE device_id=$1 AND session_id=$2`, e.deviceID, parent)
	send(child, lineage, false, captured.Add(6*time.Second))
	checkParentCount(0)
	if n := e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND session_id IN ($2,$3) AND hidden_at IS NOT NULL`, e.deviceID, child, grandchild); n != 2 {
		t.Fatalf("hidden parent propagated to %d descendants, want 2", n)
	}

	// An already deleted parent is discovered only after the child was
	// indexed. The child and its descendants must leave the shared index.
	const deletedParent = "20000000-0000-4000-8000-0000000000a4"
	if _, _, err := e.store.WithholdDeviceSession(e.ctx, e.userID, e.deviceID, "antigravity", deletedParent); err != nil {
		t.Fatal(err)
	}
	send(child, &syncproto.AntigravityLineage{ParentSessionID: deletedParent, Depth: 1}, false, captured.Add(7*time.Second))
	if n := e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND session_id IN ($2,$3)`, e.deviceID, child, grandchild); n != 0 {
		t.Fatalf("%d descendants of a deleted late parent still indexed", n)
	}

}

func TestAntigravityReplacementKeepsCurrentLineage(t *testing.T) {
	for _, order := range []string{"older parsed first", "newer parsed first", "both uploaded before parsing"} {
		t.Run(order, func(t *testing.T) {
			e := newEnv(t)
			const session = "20000000-0000-4000-8000-0000000000a1"
			const oldParent = "20000000-0000-4000-8000-0000000000a2"
			const newParent = "20000000-0000-4000-8000-0000000000a3"
			path := "/synthetic/brain/" + session + "/.system_generated/logs/transcript.jsonl"
			body := func(text string) []byte {
				return fmt.Appendf(nil, `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-01-01T00:00:00Z","content":%q}`+"\n", text)
			}
			oldBody, newBody := body("older synthetic prompt"), body("newer synthetic prompt")
			send := func(fileID, parent string, data []byte, observed int64, prev *syncproto.SourceRef) {
				t.Helper()
				req := &syncproto.FlushRequest{
					Header: syncproto.FlushHeader{
						Version: syncproto.Version, CapturedAt: time.UnixMicro(observed),
						Source: syncproto.Source{Path: path, FileID: fileID, Agent: "antigravity", StorageKind: "jsonl_append", SessionKey: session,
							Parser: antigravity.ParserName, Previous: prev, AntigravityLineage: &syncproto.AntigravityLineage{ParentSessionID: parent, Depth: 1},
							AntigravityLineageObservedAt: observed},
						Entries: []syncproto.Entry{{Hash: syncproto.Sum(data), Size: int64(len(data))}},
						Bodies:  bodyOf(data),
					},
					Payload: zpayload(data),
				}
				resp, err := e.client.Flush(e.ctx, req)
				if err != nil || resp.Status != syncproto.StatusOK {
					t.Fatalf("flush %s: %+v, %v", fileID, resp, err)
				}
			}
			check := func(fileID, parent string) {
				t.Helper()
				var gotFile, gotParent string
				var depth int
				err := e.pool.QueryRow(e.ctx, `SELECT s.file_id,COALESCE(c.parent_native_session_id,''),c.depth
					FROM conversations c JOIN sources s ON s.id=c.source_id WHERE c.device_id=$1 AND c.session_id=$2`, e.deviceID, session).
					Scan(&gotFile, &gotParent, &depth)
				if err != nil || gotFile != fileID || gotParent != parent || depth != 1 {
					t.Fatalf("file %q parent %q depth %d, err %v; want file %q and parent %q at depth 1", gotFile, gotParent, depth, err, fileID, parent)
				}
			}
			observed := time.Now().UnixMicro()
			sendOld := func() { send("1:1", oldParent, oldBody, observed, nil) }
			sendNew := func() { send("1:2", newParent, newBody, observed+1, &syncproto.SourceRef{Path: path, FileID: "1:1"}) }
			switch order {
			case "older parsed first":
				sendOld()
				e.drain()
				check("1:1", oldParent)
				sendNew()
				e.drain()
				check("1:2", newParent)

				var oldID, oldMessage, replacedBy string
				if err := e.pool.QueryRow(e.ctx, `SELECT s.id::text,m.id::text,m.superseded_by::text
					FROM sources s JOIN messages m ON m.source_id=s.id
					WHERE s.device_id=$1 AND s.path=$2 AND s.file_id='1:1' AND m.superseded`, e.deviceID, path).
					Scan(&oldID, &oldMessage, &replacedBy); err != nil {
					t.Fatal(err)
				}
				e.exec(`UPDATE source_parse_state SET applied_parser='antigravity@0',applied_redaction_rules=0 WHERE source_id=$1`, oldID)
				e.exec(`UPDATE messages SET redaction_rules=0 WHERE id=$1`, oldMessage)
				if err := e.queue.refreshSource(e.ctx, oldID); err != nil {
					t.Fatal(err)
				}
				if n := e.count(`SELECT count(*) FROM messages WHERE id=$1 AND source_id=$2 AND superseded_by=$3
					AND superseded AND redaction_rules=$4`, oldMessage, oldID, replacedBy, redact.RulesVersion); n != 1 {
					t.Fatal("obsolete refresh lost the historical message, its provenance, or its redaction update")
				}
			case "newer parsed first":
				sendNew()
				e.drain()
				check("1:2", newParent)
				sendOld()
				e.drain()
			case "both uploaded before parsing":
				sendOld()
				sendNew()
				e.drain()
			}
			check("1:2", newParent)
			if n := e.count(`SELECT count(*) FROM messages m JOIN sources s ON s.id=m.source_id
				WHERE s.device_id=$1 AND s.path=$2 AND s.file_id='1:2' AND NOT m.superseded AND m.text='newer synthetic prompt'`, e.deviceID, path); n != 1 {
				t.Fatalf("current source has %d live replacement messages, want 1", n)
			}
			if n := e.count(`SELECT count(*) FROM messages old JOIN sources s ON s.id=old.source_id JOIN messages new ON new.conversation_id=old.conversation_id
				WHERE s.device_id=$1 AND s.path=$2 AND s.file_id='1:1' AND old.superseded AND NOT new.superseded
				AND old.native_id=new.native_id AND old.text='older synthetic prompt' AND new.text='newer synthetic prompt'
				AND ($3 OR old.superseded_by=new.id)`, e.deviceID, path, order == "both uploaded before parsing"); n != 1 {
				t.Fatalf("archive has %d searchable historical versions linked to the replacement, want 1", n)
			}
			for fileID, data := range map[string][]byte{"1:1": oldBody, "1:2": newBody} {
				if got, err := e.reconstruct(path, fileID, 0); err != nil || !slices.Equal(got, data) {
					t.Fatalf("raw source %s unavailable or changed (len %d, err %v)", fileID, len(got), err)
				}
			}
		})
	}
}
