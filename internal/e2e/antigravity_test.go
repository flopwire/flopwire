package e2e

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

type antigravityFixture struct {
	session, path, auxiliaryPath              string
	prompt, response, continuation, auxiliary string
}

func antigravityRecord(step int, kind, source, content string) string {
	return fmt.Sprintf(`{"step_index":%d,"created_at":%q,"type":%q,"source":%q,"status":"DONE","content":%s}`+"\n",
		step, now(), kind, source, jsonStr(content))
}

func (h *harness) seedAntigravity(d *device) antigravityFixture {
	t := h.t
	t.Helper()
	const session = "a9700000-0000-4000-8000-000000000001"
	root := filepath.Join(d.home, ".gemini", "antigravity-cli")
	logs := filepath.Join(root, "brain", session, ".system_generated", "logs")
	if err := os.MkdirAll(logs, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := antigravityFixture{
		session:       session,
		path:          filepath.Join(logs, "transcript.jsonl"),
		auxiliaryPath: filepath.Join(logs, "transcript_full.jsonl"),
		prompt:        "Antigravity fixture prompt " + h.nonce,
		response:      "Antigravity fixture response " + h.nonce,
		continuation:  "Antigravity fixture continuation " + h.nonce,
		auxiliary:     "Antigravity fixture auxiliary " + h.nonce,
	}
	writeFile(t, fixture.path, antigravityRecord(0, "USER_INPUT", "USER_EXPLICIT", "<USER_REQUEST>\n"+fixture.prompt+"\n</USER_REQUEST>"))
	writeFile(t, fixture.auxiliaryPath, antigravityRecord(0, "USER_INPUT", "USER_EXPLICIT", fixture.auxiliary))

	db, err := sql.Open("sqlite", filepath.Join(root, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT PRIMARY KEY, workspace_uris TEXT, title TEXT,
		parent_conversation_id TEXT, nesting_depth INTEGER, agent_name TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversation_summaries
		(conversation_id, workspace_uris, title, parent_conversation_id, nesting_depth) VALUES (?, ?, ?, '', 0)`,
		session, `["file:///tmp/e2e-growth"]`, "Antigravity synthetic conversation"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (h *harness) checkAntigravitySync(t *testing.T, origin, reader *device, f antigravityFixture) {
	t.Helper()
	checkRow := func(needle, kind string) {
		t.Helper()
		eventually(t, time.Minute, 250*time.Millisecond, "Antigravity local and shared search: "+needle, func() (bool, error) {
			rows, err := origin.localFind(needle)
			if err != nil {
				return false, err
			}
			if len(rows) != 1 || rows[0].Session != f.session || rows[0].Agent != "antigravity" || rows[0].Superseded {
				return false, fmt.Errorf("unexpected local rows: %+v", rows)
			}
			hits, err := reader.serverFind(needle, "--agent", "antigravity", "--device", origin.name)
			if err != nil {
				return false, err
			}
			if len(hits) != 1 || hits[0].SessionID != f.session || hits[0].Kind != kind {
				return false, fmt.Errorf("unexpected shared hits: %+v", hits)
			}
			return true, nil
		})
	}
	checkRow(f.prompt, "user")
	h.checkRaw(t, origin, f.path)

	appendFile(t, f.path, antigravityRecord(1, "PLANNER_RESPONSE", "MODEL", f.response)+
		antigravityRecord(2, "USER_INPUT", "USER_EXPLICIT", f.continuation))
	checkRow(f.response, "assistant")
	checkRow(f.continuation, "user")
	h.checkRaw(t, origin, f.path)
	checkRow(f.prompt, "user")

	out, err := reader.cli("search", "--server", "--json", "--agent", "antigravity", "--device", origin.name, f.continuation)
	if err != nil {
		t.Fatal(err)
	}
	var ranked struct{ Hits []format.Hit }
	if err := json.Unmarshal([]byte(out), &ranked); err != nil {
		t.Fatalf("ranked search output: %v (%s)", err, out)
	}
	hits := inSession(ranked.Hits, f.session)
	if len(hits) == 0 || hits[0].Agent != "antigravity" {
		t.Fatalf("ranked search missed Antigravity session: %s", out)
	}

	if rows, err := origin.localFind(f.auxiliary); err != nil || len(rows) != 0 {
		t.Fatalf("auxiliary transcript in local search: %+v, %v", rows, err)
	}
	if hits, err := reader.serverFind(f.auxiliary, "--agent", "antigravity"); err != nil || len(hits) != 0 {
		t.Fatalf("auxiliary transcript in shared search: %+v, %v", hits, err)
	}
	var auxiliarySources int
	if err := h.pg.QueryRow(h.ctx, "SELECT count(*) FROM sources WHERE device_id=$1 AND path=$2",
		origin.id, f.auxiliaryPath).Scan(&auxiliarySources); err != nil || auxiliarySources != 0 {
		t.Fatalf("auxiliary transcript archive sources: %d, %v", auxiliarySources, err)
	}
}
