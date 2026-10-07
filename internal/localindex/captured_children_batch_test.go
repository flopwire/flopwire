package localindex

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/transcript"
)

func batchNativeEvidence(t *testing.T, s *Store, path, key string, foreign bool) {
	t.Helper()
	src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: path, SessionKey: key, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveWatermark(ctx, src.ID, transcript.Watermark{Offset: 9}, nil); err != nil {
		t.Fatal(err)
	}
	if foreign {
		if err = s.write(ctx, func(w *writeTx) error {
			_, err := w.exec(`UPDATE sources SET device_id='foreign' WHERE id=?`, src.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCapturedClaudeChildrenBatchKeepsPerParentRoots(t *testing.T) {
	s := openEvidenceTest(t)
	const a = "12345678-abcd-4321-9876-123456789abc"
	const b = "12345678-abcd-4321-9876-123456789abd"
	const c = "12345678-abcd-4321-9876-123456789abe"
	root := "/synthetic/projects"
	nested := root + "/project/" + a + "/subagents"
	batchNativeEvidence(t, s, root+"/project/"+a+"/subagents/agent-native.jsonl", "agent-native", false)
	batchNativeEvidence(t, s, nested+"/project/"+b+"/subagents/agent-overlap.jsonl", "", false)
	batchNativeEvidence(t, s, root+"/project/"+b+"/subagents/agent-wrong-root.jsonl", "", false)
	batchNativeEvidence(t, s, root+"-other/project/"+a+"/subagents/agent-prefix-other.jsonl", "", false)
	batchNativeEvidence(t, s, nested+"/project/"+b+"/subagents/agent-foreign.jsonl", "", true)
	batchNativeEvidence(t, s, nested+"/project/"+b+"/subagents/agent-conflict.jsonl", "agent-other", false)
	// An acknowledged older scheduler companion can outlive its parent file.
	err := s.write(ctx, func(w *writeTx) error {
		for _, stmt := range []string{`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,path TEXT,spec TEXT,generation INTEGER)`, `CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER,closed INTEGER,acked INTEGER)`} {
			if _, err := w.exec(stmt); err != nil {
				return err
			}
		}
		spec, _ := json.Marshal(map[string]any{"Agent": "claude", "StorageKind": "companion", "Parser": "claude@1", "Parent": nested + "/project/" + b + "/subagents/agent-archived.jsonl", "SessionKey": ""})
		if _, err := w.exec(`INSERT INTO devsync_sources VALUES(1,'/synthetic/companion.txt',?,3)`, string(spec)); err != nil {
			return err
		}
		_, err := w.exec(`INSERT INTO devsync_gens VALUES(1,1,9,1,1)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	uppercase := strings.ToUpper(a)
	parents := map[string][]string{a: {root}, uppercase: {root}, b: {nested}, c: {"/same-name/projects"}, "not-a-uuid": {root}}
	got, err := s.CapturedClaudeChildrenForParents(ctx, parents)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{a: {"agent-archived", "agent-native", "agent-overlap"}, uppercase: {"agent-archived", "agent-native", "agent-overlap"}, b: {"agent-archived", "agent-overlap"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("relations=%v want=%v", got, want)
	}
	// Existing single-parent compatibility includes uppercase UUID spelling.
	one, err := s.CapturedClaudeChildren(ctx, parents[uppercase], uppercase)
	if err != nil || !reflect.DeepEqual(one, want[uppercase]) {
		t.Fatalf("single=%v error=%v", one, err)
	}
}

func TestCapturedClaudeChildrenBatchScansOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	counter := perfguard.CountSQLite(t, "file:"+path+"?")
	s, err := Open(path, Options{DeviceID: "local-device", DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	parents := map[string][]string{}
	for i := range 32 {
		parents[fmt.Sprintf("12345678-abcd-4321-9876-%012x", i+1)] = []string{"/synthetic/projects"}
	}
	for i := range 2048 {
		parent := fmt.Sprintf("12345678-abcd-4321-9876-%012x", i%32+1)
		child := fmt.Sprintf("agent-%04x", i)
		batchNativeEvidence(t, s, "/synthetic/projects/project/"+parent+"/subagents/"+child+".jsonl", child, false)
	}
	if err = s.write(ctx, func(w *writeTx) error {
		for _, stmt := range []string{`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,path TEXT,spec TEXT,generation INTEGER)`, `CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER,closed INTEGER,acked INTEGER)`} {
			if _, err := w.exec(stmt); err != nil {
				return err
			}
		}
		for i := range 2048 {
			parent := fmt.Sprintf("12345678-abcd-4321-9876-%012x", i%32+1)
			child := fmt.Sprintf("agent-%04x", i)
			path := "/synthetic/projects/project/" + parent + "/subagents/" + child + ".jsonl"
			spec, _ := json.Marshal(map[string]any{"Agent": "claude", "StorageKind": "jsonl_append", "Parser": "claude@1", "SessionKey": child})
			if _, err := w.exec(`INSERT INTO devsync_sources VALUES(?,?,?,3)`, i+1, path, string(spec)); err != nil {
				return err
			}
			if _, err := w.exec(`INSERT INTO devsync_gens VALUES(?,1,9,1,1)`, i+1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	counter.Reset()
	start := time.Now()
	legacy := map[string][]string{}
	for parent, roots := range parents {
		children, err := s.CapturedClaudeChildren(ctx, roots, parent)
		if err != nil {
			t.Fatal(err)
		}
		legacy[parent] = children
	}
	legacyElapsed := time.Since(start)
	legacyScans, legacySyncScans := int64(0), int64(0)
	for sql, n := range counter.BySQL() {
		if strings.Contains(sql, "FROM sources src") {
			legacyScans += n
		}
		if strings.Contains(sql, "FROM devsync_sources src JOIN devsync_gens") {
			legacySyncScans += n
		}
	}
	if err = s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	counter.Reset()
	start = time.Now()
	batch, err := s.CapturedClaudeChildrenForParents(ctx, parents)
	batchElapsed := time.Since(start)
	if err != nil || !reflect.DeepEqual(batch, legacy) {
		t.Fatalf("batch differs: error=%v", err)
	}
	batchScans, batchSyncScans := int64(0), int64(0)
	for sql, n := range counter.BySQL() {
		if strings.Contains(sql, "FROM sources src") {
			batchScans += n
		}
		if strings.Contains(sql, "FROM devsync_sources src JOIN devsync_gens") {
			batchSyncScans += n
		}
	}
	if legacyScans != 32 || batchScans != 1 || legacySyncScans != 32 || batchSyncScans != 1 {
		t.Fatalf("evidence scans local legacy=%d batch=%d, scheduler legacy=%d batch=%d", legacyScans, batchScans, legacySyncScans, batchSyncScans)
	}
	t.Logf("2048 local sources +2048 retained scheduler generations,32 parents: repeated scans=%d time=%v; batch scans=%d time=%v", legacyScans, legacyElapsed, batchScans, batchElapsed)
}

func TestCapturedClaudeChildrenBatchSchemaErrorRetainsVerifiedRestrictions(t *testing.T) {
	s := openEvidenceTest(t)
	const parent = "12345678-abcd-4321-9876-123456789abc"
	root := "/synthetic/projects"
	batchNativeEvidence(t, s, root+"/project/"+parent+"/subagents/agent-local.jsonl", "", false)
	if err := s.write(ctx, func(w *writeTx) error {
		_, err := w.exec(`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,path TEXT,spec TEXT,generation INTEGER)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.CapturedClaudeChildrenForParents(ctx, map[string][]string{parent: {root}})
	if err == nil || !reflect.DeepEqual(got[parent], []string{"agent-local"}) {
		t.Fatalf("partial restrictions=%v error=%v", got, err)
	}
}
