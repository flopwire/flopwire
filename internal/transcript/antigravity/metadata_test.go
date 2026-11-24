package antigravity

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadMetadata(t *testing.T) {
	dir := t.TempDir()
	catalog := filepath.Join(dir, "conversation_summaries.db")
	cache := filepath.Join(dir, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	const cacheJSON = `{"conversations":{"database":{"is_internal":false,"summary":{"Title":"Old","WorkspaceURIs":["file:///old"]}},"cached":{"is_internal":false,"summary":{"Title":"Cache","WorkspaceURIs":["file:///cached"]}},"invalid":{"is_internal":false,"summary":{"WorkspaceURIs":["file:///stale"]}}}}`
	if err := os.WriteFile(filepath.Join(cache, "conversation_metadata.json"), []byte(cacheJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	fallback, err := ReadAllMetadata(t.Context(), catalog)
	if err != nil || fallback["cached"].Cwd != "/cached" {
		t.Fatalf("cache fallback before DB exists = %+v, %v", fallback, err)
	}
	if _, err := os.Stat(catalog); !os.IsNotExist(err) {
		t.Fatalf("read created the SQLite file: %v", err)
	}

	db, err := sql.Open("sqlite", catalog)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		PRAGMA journal_mode=WAL;
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY, workspace_uris TEXT,
			title TEXT, parent_conversation_id TEXT, nesting_depth INTEGER
		);
		INSERT INTO conversation_summaries VALUES
			('database', '["file:///workspace%20one","file://localhost/workspace/two","file:///workspace/two"]', 'Current', 'parent', 2),
			('invalid', '["file:///workspace", "ssh://remote/example"]', 'Invalid', '', 0),
			('null-title', '["file:///workspace"]', NULL, '', 0),
			('null-workspace', NULL, NULL, '', 0),
			('null-parent', '["file:///workspace"]', NULL, NULL, 0),
			('null-depth', '["file:///workspace"]', NULL, '', NULL),
			('broken-depth', '["file:///workspace"]', NULL, '', 'broken'),
			('oversized-workspace', '[]' || printf('%*s', 1048575, ''), NULL, '', 0);
	`)
	if err != nil {
		t.Fatal(err)
	}

	want := Metadata{Cwd: "/workspace one", OtherCwds: []string{"/workspace/two"}, ParentSessionID: "parent", Depth: 2, Title: "Current"}
	all, err := ReadAllMetadata(t.Context(), catalog)
	if err != nil || len(all) != 9 || !all["invalid"].Invalid || !reflect.DeepEqual(all["database"], want) || !reflect.DeepEqual(all["cached"], fallback["cached"]) {
		t.Fatalf("catalog snapshot = %+v, %v", all, err)
	}
	if row := all["null-title"]; row.Invalid || row.Cwd != "/workspace" || row.Title != "" {
		t.Fatalf("row with no title = %+v", row)
	}
	for _, id := range []string{"null-workspace", "null-parent", "null-depth", "broken-depth", "oversized-workspace"} {
		if row := all[id]; !row.Invalid || row.Cwd != "" {
			t.Errorf("%s row = %+v", id, row)
		}
	}
}

func TestCachedMetadataRequiresKnownRoot(t *testing.T) {
	dir := t.TempDir()
	catalog := filepath.Join(dir, "conversation_summaries.db")
	cache := filepath.Join(dir, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	const cacheJSON = `{"conversations":{
		"external":{"is_internal":false,"summary":{"Internal":false,"WorkspaceURIs":["file:///workspace"]}},
		"internal":{"is_internal":true},
		"internal-with-workspace":{"is_internal":true,"summary":{"Internal":false,"WorkspaceURIs":["file:///workspace"]}},
		"internal-summary":{"is_internal":false,"summary":{"Internal":true,"WorkspaceURIs":["file:///workspace"]}},
		"missing-summary":{"is_internal":false},
		"unknown":{"summary":{"WorkspaceURIs":["file:///workspace"]}}
	}}`
	if err := os.WriteFile(filepath.Join(cache, "conversation_metadata.json"), []byte(cacheJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	all, err := ReadAllMetadata(t.Context(), catalog)
	if err != nil || len(all) != 6 {
		t.Fatalf("catalog snapshot = %+v, %v", all, err)
	}
	for id, row := range all {
		if id == "external" {
			if row.Invalid || row.Cwd != "/workspace" {
				t.Errorf("external root = %+v", row)
			}
		} else if !row.Invalid || row.Cwd != "" {
			t.Errorf("%s has unverified lineage: %+v", id, row)
		}
	}
}
