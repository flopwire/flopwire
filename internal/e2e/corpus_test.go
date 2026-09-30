package e2e

import (
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// Real-corpus sample (FLOPWIRE_E2E_CORPUS=N): device 1 also carries copies of
// the N newest Claude sessions (with their subagents and tool-results), the
// N newest Codex rollouts and the N newest Devin sessions of this machine,
// so real transcripts go through upload, server parse and retrieval, and
// scenario z compares them row for row. The harness directories are only
// read; the copies live in the test's work directory, which the script
// deletes with the Compose volumes.

const corpusMaxFile = 64 << 20

type corpusStats struct {
	claude, codex, devin int
	bytes                int64
}

func (s corpusStats) String() string {
	return fmt.Sprintf("real corpus sample: %d Claude sessions, %d Codex rollouts, %d Devin sessions, %.1f MB", s.claude, s.codex, s.devin, float64(s.bytes)/(1<<20))
}

func (d *device) seedCorpus(t *testing.T, n int) corpusStats {
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	var st corpusStats

	// Claude: newest main transcripts, each with its session directory.
	sessions, err := claude.Discover(filepath.Join(realHome, ".claude", "projects"))
	if err != nil {
		t.Fatal(err)
	}
	type item struct {
		path string
		mod  int64
		size int64
		s    *claude.Session
	}
	var items []item
	for _, s := range sessions {
		if s.Transcript == "" {
			continue
		}
		if fi, err := os.Stat(s.Transcript); err == nil && fi.Size() <= corpusMaxFile {
			items = append(items, item{s.Transcript, fi.ModTime().UnixNano(), fi.Size(), s})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod > items[j].mod })
	for _, it := range items[:min(n, len(items))] {
		rel, _ := filepath.Rel(filepath.Join(realHome, ".claude", "projects"), it.path)
		// Keep the corpus apart from the fixtures' projects.
		dst := filepath.Join(d.home, ".claude", "projects", "corpus"+strings.TrimPrefix(filepath.Dir(rel), "."), filepath.Base(rel))
		st.bytes += copyFileCapped(t, it.path, dst)
		sessDir := strings.TrimSuffix(it.path, ".jsonl")
		if fi, err := os.Stat(sessDir); err == nil && fi.IsDir() {
			_ = filepath.WalkDir(sessDir, func(p string, e fs.DirEntry, err error) error {
				if err != nil || e.IsDir() {
					return nil
				}
				r, _ := filepath.Rel(sessDir, p)
				st.bytes += copyFileCapped(t, p, filepath.Join(strings.TrimSuffix(dst, ".jsonl"), r))
				return nil
			})
		}
		st.claude++
	}

	// Codex: newest rollouts, kept at their dated paths.
	srcs, err := codex.Discover(filepath.Join(realHome, ".codex"))
	if err != nil {
		t.Fatal(err)
	}
	var cx []item
	for _, s := range srcs {
		if fi, err := os.Stat(s.Path); err == nil && fi.Size() <= corpusMaxFile {
			cx = append(cx, item{path: s.Path, mod: fi.ModTime().UnixNano(), size: fi.Size()})
		}
	}
	sort.Slice(cx, func(i, j int) bool { return cx[i].mod > cx[j].mod })
	for _, it := range cx[:min(n, len(cx))] {
		rel, _ := filepath.Rel(filepath.Join(realHome, ".codex"), it.path)
		st.bytes += copyFileCapped(t, it.path, filepath.Join(d.home, ".codex", rel))
		st.codex++
	}

	// Devin: a consistent copy of the store (VACUUM INTO from a read-only
	// connection), cut down to the newest sessions, merged into the
	// device's oracle store is not possible (one store per device), so the
	// device's store becomes the sample plus the oracle sessions.
	realDevin := filepath.Join(realHome, ".local", "share", "devin", "cli", "sessions.db")
	if _, err := os.Stat(realDevin); err == nil && n > 0 {
		st.devin = d.mergeDevinSample(t, realDevin, n)
	}
	return st
}

// mergeDevinSample copies the n newest real Devin sessions into the
// device's Devin store.
func (d *device) mergeDevinSample(t *testing.T, realDB string, n int) int {
	tmp := filepath.Join(d.dir, "devin-sample.db")
	src, err := sql.Open("sqlite", "file:"+realDB+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`VACUUM INTO ?`, tmp); err != nil {
		src.Close()
		t.Fatalf("devin sample: %v", err)
	}
	src.Close()
	defer os.Remove(tmp)
	db, err := sql.Open("sqlite", "file:"+d.devinDB()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	stmts := []string{
		`ATTACH DATABASE '` + strings.ReplaceAll(tmp, "'", "''") + `' AS real`,
		`CREATE TEMP TABLE pick AS SELECT id FROM real.sessions ORDER BY last_activity_at DESC LIMIT ` + fmt.Sprint(n),
		`INSERT INTO sessions (id, working_directory, backend_type, model, agent_mode, created_at, last_activity_at, title, main_chain_id, hidden)
			SELECT id, working_directory, backend_type, model, agent_mode, created_at, last_activity_at, title, main_chain_id, hidden FROM real.sessions WHERE id IN (SELECT id FROM pick)`,
		`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at, metadata)
			SELECT session_id, node_id, parent_node_id, chat_message, created_at, metadata FROM real.message_nodes WHERE session_id IN (SELECT id FROM pick)`,
		`INSERT INTO tool_call_state (session_id, tool_call_id, tool_call_json, tool_call_update_json)
			SELECT session_id, tool_call_id, tool_call_json, tool_call_update_json FROM real.tool_call_state WHERE session_id IN (SELECT id FROM pick)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("devin sample: %v", err)
		}
	}
	var got int
	_ = db.QueryRow(`SELECT count(*) FROM pick`).Scan(&got)
	return got
}

// copyFileCapped copies src to dst (creating directories) unless src is
// larger than corpusMaxFile, and returns the bytes copied.
func copyFileCapped(t *testing.T, src, dst string) int64 {
	in, err := os.Open(src)
	if err != nil {
		return 0
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil || fi.Size() > corpusMaxFile || !fi.Mode().IsRegular() {
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	n, err := io.Copy(out, in)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
