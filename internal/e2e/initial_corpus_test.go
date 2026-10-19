package e2e

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// The corpus files are static until the initial sync check completes. Prepare
// their oracle before launching the full-mode agent, outside its time budget.
type corpusSession struct {
	agent, session, label string
	keys, content         map[string]int
	server                bool
}

type corpusOracle struct {
	sessions []corpusSession
	prep     time.Duration
	files    int
	rows     int
}

func (h *harness) prepareCorpusOracle(t *testing.T, d *device) *corpusOracle {
	t.Helper()
	start := time.Now()
	oracle := &corpusOracle{}
	files, err := d.transcripts()
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		agent := files[path]
		keys, err := parseAll(path, agent)
		if err != nil {
			t.Fatal(err)
		}
		content, err := h.expectedContent(path, agent, "")
		if err != nil {
			t.Fatal(err)
		}
		for session, want := range keys {
			oracle.sessions = append(oracle.sessions, corpusSession{string(agent), session, filepath.Base(path), want, content[session], true})
			oracle.rows += len(want)
		}
	}
	oracle.files = len(paths)
	devinSessions, err := devin.ListSessions(h.ctx, d.devinDB())
	if err != nil {
		t.Fatal(err)
	}
	placed, err := d.devinPlaced()
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range devinSessions {
		keys, err := expectedDevinKeys(h.ctx, h.cfg.root, d.devinDB(), session)
		if err != nil {
			t.Fatal(err)
		}
		content, err := h.expectedContent(d.devinDB(), transcript.AgentDevin, session)
		if err != nil {
			t.Fatal(err)
		}
		oracle.sessions = append(oracle.sessions, corpusSession{string(transcript.AgentDevin), session, "Devin", keys, content[session], placed[session]})
		oracle.rows += len(keys)
	}
	oracle.prep = time.Since(start)
	return oracle
}

// Derive Devin identity from the source export. Using the local index as the
// expected state would miss rows absent from both local and server indexes.
func expectedDevinKeys(ctx context.Context, root, dbPath, session string) (map[string]int, error) {
	data, err := devin.Export(ctx, dbPath, session)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, "keys-devin-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	r := redact.NewReaderAt(strings.NewReader(string(data)), redact.Lines)
	path, err := devin.LoadExport(ctx, io.NewSectionReader(r, 0, int64(len(data))), session, dir)
	if err != nil {
		return nil, err
	}
	parser := &devin.Parser{}
	src := &transcript.Source{Agent: transcript.AgentDevin, Path: path, StorageKind: transcript.StorageSQLite, Parser: parser.Name()}
	var c transcript.Collector
	if _, err := transcript.Reparse(ctx, parser, transcript.Input{Source: src}, &c); err != nil {
		return nil, err
	}
	keys := map[string]int{}
	for _, m := range c.Messages {
		if m.Superseded {
			continue
		}
		k := m.NativeID
		if k == "" {
			k = fmt.Sprintf("@%d", m.ByteOffset)
		}
		keys[fmt.Sprintf("%s#%d", k, m.Part)]++
	}
	return keys, nil
}

type corpusSnapshot struct {
	pg      pgx.Tx
	localDB *sql.DB
	local   *sql.Tx
	pgAt    time.Time
	localAt time.Time
}

func (s *corpusSnapshot) close(ctx context.Context) {
	if s.local != nil {
		_ = s.local.Rollback()
	}
	if s.localDB != nil {
		_ = s.localDB.Close()
	}
	if s.pg != nil {
		_ = s.pg.Rollback(ctx)
	}
}

// PostgreSQL assigns a repeatable-read snapshot on its first SELECT; SQLite
// establishes its WAL read snapshot on its first SELECT inside the transaction.
func (h *harness) captureCorpus(d *device) (s corpusSnapshot, err error) {
	defer func() {
		if err != nil {
			s.close(h.ctx)
		}
	}()
	s.pg, err = h.pg.BeginTx(h.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return s, err
	}
	var marker int
	err = s.pg.QueryRow(h.ctx, "SELECT 1 FROM messages LIMIT 1").Scan(&marker)
	if err != nil && err != pgx.ErrNoRows {
		return s, err
	}
	s.pgAt = time.Now()
	s.localDB, err = sql.Open("sqlite", "file:"+d.index+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return s, err
	}
	s.local, err = s.localDB.BeginTx(h.ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s, err
	}
	err = s.local.QueryRowContext(h.ctx, "SELECT count(*) FROM sqlite_master").Scan(&marker)
	if err != nil {
		return s, err
	}
	s.localAt = time.Now()
	return s, nil
}

func snapshotWithinDeadline(start time.Time, s corpusSnapshot, limit time.Duration) bool {
	return !s.pgAt.IsZero() && !s.localAt.IsZero() && !s.pgAt.Before(start) && !s.localAt.Before(start) && s.pgAt.Sub(start) <= limit && s.localAt.Sub(start) <= limit
}

func (h *harness) checkInitialCorpus(t *testing.T, d *device, oracle *corpusOracle, started time.Time, r *result) {
	t.Helper()
	const limit = 5 * time.Minute
	const margin = 5 * time.Second
	r.Timings["corpus_oracle_prep"] = ms(oracle.prep)
	r.Notes = append(r.Notes, fmt.Sprintf("oracle: %d files, %d sessions, %d keys", oracle.files, len(oracle.sessions), oracle.rows))
	if wait := time.Until(started.Add(limit - margin)); wait > 0 {
		time.Sleep(wait)
	}
	snap, err := h.captureCorpus(d)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.close(h.ctx)
	r.Timings["corpus_pg_capture"] = ms(snap.pgAt.Sub(started))
	r.Timings["corpus_local_capture"] = ms(snap.localAt.Sub(started))
	if !snapshotWithinDeadline(started, snap, limit) {
		t.Fatalf("corpus snapshot acquired after %s deadline: pg=%s local=%s", limit, r.Timings["corpus_pg_capture"], r.Timings["corpus_local_capture"])
	}
	readStart := time.Now()
	problems, err := h.compareCorpusSnapshot(d, oracle, snap)
	r.Timings["corpus_snapshot_read_compare"] = ms(time.Since(readStart))
	r.Timings["corpus_post_capture_wall"] = ms(time.Since(snap.localAt))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		path := filepath.Join(h.cfg.root, "initial-corpus-mismatches.txt")
		if err := os.WriteFile(path, []byte(strings.Join(problems, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("corpus not converged at pg=%s local=%s: %d mismatches, first: %s (all: %s)", r.Timings["corpus_pg_capture"], r.Timings["corpus_local_capture"], len(problems), strings.Join(head(problems), "; "), path)
	}
	r.Timings["corpus_synced"] = r.Timings["corpus_local_capture"]
}

func (h *harness) compareCorpusSnapshot(d *device, oracle *corpusOracle, s corpusSnapshot) ([]string, error) {
	var problems []string
	for _, want := range oracle.sessions {
		serverKeys, serverContent, err := h.corpusServerRows(s.pg, d.id, want.agent, want.session)
		if err != nil {
			return nil, err
		}
		serverWantKeys, serverWantContent := want.keys, want.content
		if !want.server {
			serverWantKeys, serverWantContent = nil, nil
		}
		localKeys, err := corpusLocalKeys(h.ctx, s.local, want.agent, want.session)
		if err != nil {
			return nil, err
		}
		problems = append(problems, compareCorpusSession(want.label+" "+want.session, want.keys, serverWantKeys, serverWantContent, localKeys, serverKeys, serverContent)...)
	}
	return problems, nil
}

func compareCorpusSession(label string, localWant, serverWantKeys, serverWantContent, localKeys, serverKeys, serverContent map[string]int) []string {
	var problems []string
	if diff := diffKeys(serverWantKeys, serverKeys); diff != "" {
		problems = append(problems, "server "+label+": "+diff)
	}
	if diff := contentDiff(serverWantContent, serverContent); diff != "" {
		problems = append(problems, "server "+label+": "+diff)
	}
	if diff := diffKeys(localWant, localKeys); diff != "" {
		problems = append(problems, "local "+label+": "+diff)
	}
	return problems
}

func corpusLocalKeys(ctx context.Context, tx *sql.Tx, agent, session string) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(m.native_id, '@'||m.byte_offset), m.part FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.agent=? AND c.session_id=? AND m.superseded=0`, agent, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		var part int
		if err := rows.Scan(&key, &part); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%s#%d", key, part)]++
	}
	return out, rows.Err()
}

func (h *harness) corpusServerRows(tx pgx.Tx, deviceID, agent, session string) (map[string]int, map[string]int, error) {
	rows, err := tx.Query(h.ctx, `SELECT COALESCE(m.native_id, '@'||m.byte_offset::text), m.part, count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.device_id=$1 AND c.agent=$2 AND c.session_id=$3 AND NOT m.superseded GROUP BY 1,2`, deviceID, agent, session)
	if err != nil {
		return nil, nil, err
	}
	keys := map[string]int{}
	for rows.Next() {
		var key string
		var part, count int
		if err := rows.Scan(&key, &part, &count); err != nil {
			rows.Close()
			return nil, nil, err
		}
		keys[fmt.Sprintf("%s#%d", key, part)] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	contentRows, err := tx.Query(h.ctx, `SELECT COALESCE(m.native_id,''),COALESCE(m.parent_native_id,''),m.kind,
		COALESCE(m.role,''),COALESCE(m.tool_name,''),COALESCE(m.tool_call_id,''),COALESCE(m.locator,''),m.parser,
		m.ordinal,COALESCE(m.line_no,0),COALESCE(m.byte_offset,0),COALESCE(m.byte_len,0),m.part,m.text_len,
		COALESCE(m.is_error,false),m.text,encode(m.content_sha,'hex')
		FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.device_id=$1 AND c.agent=$2 AND c.session_id=$3 AND NOT m.superseded`, deviceID, agent, session)
	if err != nil {
		return nil, nil, err
	}
	content := map[string]int{}
	for contentRows.Next() {
		var row contentRow
		var text string
		if err := contentRows.Scan(&row.Native, &row.Parent, &row.Kind, &row.Role, &row.Tool, &row.Call, &row.Locator, &row.Parser,
			&row.Ordinal, &row.Line, &row.Offset, &row.Length, &row.Part, &row.FullLen, &row.IsError, &text, &row.FullSHA); err != nil {
			contentRows.Close()
			return nil, nil, err
		}
		row.TextSHA = fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
		content[row.fingerprint()]++
	}
	err = contentRows.Err()
	contentRows.Close()
	return keys, content, err
}
