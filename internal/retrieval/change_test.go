package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type changeFixture struct {
	*findFixture
	source  string
	ordinal int64
}

func newChangeFixture(t *testing.T) *changeFixture {
	f := &changeFixture{findFixture: newFindFixture(t), source: uuid.NewString()}
	_, err := f.s.Pool.Exec(context.Background(), `INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at,checkout)
   SELECT $1,device_id,'codex','/synthetic/transcript','synthetic','jsonl_append','test',now(),'/src/flopwire' FROM conversations WHERE id=$2`, f.source, f.conv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Pool.Exec(context.Background(), `UPDATE conversations SET source_id=$1 WHERE id=$2`, f.source, f.conv); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *changeFixture) row(source, call, kind, text string) string {
	f.t.Helper()
	f.ordinal++
	id := uuid.NewString()
	sum := sha256.Sum256([]byte(text))
	_, err := f.s.Pool.Exec(context.Background(), `INSERT INTO messages(id,conversation_id,source_id,native_id,ordinal,kind,tool_name,tool_call_id,text,text_len,content_sha,source_generation,parser)
		VALUES($1::uuid,$2,$3,$1::uuid::text,$4,$5,'exec_command',$6,$7,$8,$9,0,'test')`, id, f.conv, source, f.ordinal, kind, call, text, len(text), sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f *changeFixture) commit(hash string) (string, string) {
	call := uuid.NewString()
	body, _ := json.Marshal(map[string]string{"cmd": "git commit -m synthetic", "workdir": "/src/flopwire"})
	command := f.row(f.source, call, "tool_call", string(body))
	result := f.row(f.source, call, "tool_result", "[topic "+hash+"] synthetic")
	return command, result
}
func (f *changeFixture) lookup(commits []string, limit int, cursor string) *format.Changes {
	f.t.Helper()
	out, err := f.s.Change(context.Background(), format.ChangeQuery{Commits: commits, Limit: limit, Cursor: cursor}, format.Filters{Repo: "/src/flopwire"})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestChangeServerEvidenceAndStablePages(t *testing.T) {
	f := newChangeFixture(t)
	h := strings.Repeat("a", 40)
	c1, r1 := f.commit(h[:7])
	c2, r2 := f.commit(h[:7])
	first := f.lookup([]string{h}, 1, "")
	if len(first.Matches) != 1 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page: %+v", first)
	}
	if first.Matches[0].CommandAddress != c1 || first.Matches[0].ResultAddress != r1 || first.Matches[0].Match != "prefix" || first.Matches[0].Session.Address != "sess-1" {
		t.Fatalf("evidence: %+v", first.Matches[0])
	}
	f.commit(h[:7])
	second := f.lookup([]string{h}, 1, first.NextCursor)
	if second.RestartRequired || second.HasMore || len(second.Matches) != 1 || second.Matches[0].CommandAddress != c2 || second.Matches[0].ResultAddress != r2 {
		t.Fatalf("stable append page: %+v", second)
	}
	// A redaction removes the old projection in the same SQL transaction.
	if _, err := f.s.Pool.Exec(context.Background(), `UPDATE messages SET text='[redacted]' WHERE id=$1`, r2); err != nil {
		t.Fatal(err)
	}
	resumed := f.lookup([]string{h}, 1, first.NextCursor)
	if !resumed.RestartRequired || len(resumed.Matches) != 0 {
		t.Fatalf("redaction did not invalidate: %+v", resumed)
	}
	fresh := f.lookup([]string{h}, 10, "")
	if len(fresh.Matches) != 2 {
		t.Fatalf("redacted evidence survived: %+v", fresh)
	}
}

func TestChangeServerLifecycleAndCursorBinding(t *testing.T) {
	f := newChangeFixture(t)
	h := strings.Repeat("b", 40)
	f.commit(h)
	f.commit(h)
	first := f.lookup([]string{h}, 1, "")
	if first.Matches[0].Match != "exact" {
		t.Fatal("full observation marked prefix")
	}
	_, err := f.s.Change(context.Background(), format.ChangeQuery{Commits: []string{strings.Repeat("c", 40)}, Cursor: first.NextCursor}, format.Filters{Repo: "/src/flopwire"})
	if err == nil {
		t.Fatal("query cursor reused")
	}
	cursor, err := format.ParseChangeCursor(strings.Split(first.NextCursor, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	cursor.Cutoff++
	_, err = f.s.Change(context.Background(), format.ChangeQuery{Commits: []string{h}, Cursor: format.EncodeChangeCursor(cursor)}, format.Filters{Repo: "/src/flopwire"})
	if err == nil {
		t.Fatal("unsigned altered cutoff accepted")
	}
	if _, err = f.s.Pool.Exec(context.Background(), `UPDATE conversations SET hidden_at=now() WHERE id=$1`, f.conv); err != nil {
		t.Fatal(err)
	}
	if !f.lookup([]string{h}, 1, first.NextCursor).RestartRequired {
		t.Fatal("hidden session cursor not invalidated")
	}
	if len(f.lookup([]string{h}, 10, "").Matches) != 0 {
		t.Fatal("hidden evidence visible")
	}
	if _, err = f.s.Pool.Exec(context.Background(), `UPDATE conversations SET hidden_at=NULL WHERE id=$1`, f.conv); err != nil {
		t.Fatal(err)
	}
	if len(f.lookup([]string{h}, 10, "").Matches) != 2 {
		t.Fatal("restored session missing")
	}
	if _, err = f.s.Pool.Exec(context.Background(), `UPDATE sources SET tombstoned_at=now() WHERE id=$1`, f.source); err != nil {
		t.Fatal(err)
	}
	if len(f.lookup([]string{h}, 10, "").Matches) != 0 {
		t.Fatal("tombstoned source visible")
	}
	if _, err = f.s.Pool.Exec(context.Background(), `DELETE FROM conversations WHERE id=$1`, f.conv); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM commit_evidence`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("deleted conversation retained evidence")
	}
}

func TestChangeServerPrefixAmbiguityAndAtomicBackfill(t *testing.T) {
	f := newChangeFixture(t)
	h := "abcdef0" + strings.Repeat("a", 33)
	other := "abcdef0" + strings.Repeat("b", 33)
	f.commit("abcdef0")
	ctx := context.Background()
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ingest.RefreshCommitEvidence(ctx, tx, f.conv); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM commit_evidence`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("rolled-back projection published")
	}
	out := f.lookup([]string{other, h}, 10, "")
	if len(out.Matches) != 1 || len(out.Matches[0].InputCommits) != 2 || out.Coverage.HistoryComplete || out.Coverage.IndexingLag {
		t.Fatalf("ambiguity/backfill: %+v", out)
	}
	// Rebuild after marking dirty preserves the observation ID and revision.
	var before, after int64
	if err := f.s.Pool.QueryRow(ctx, `SELECT id FROM commit_evidence`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `INSERT INTO commit_evidence_dirty(conversation_id) VALUES($1)`, f.conv); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, f.s.Pool, func(tx pgx.Tx) error { return ingest.RefreshCommitEvidence(ctx, tx, f.conv) }); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Pool.QueryRow(ctx, `SELECT id FROM commit_evidence`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("unchanged observation replaced")
	}
}

func TestChangeServerSourceIsolationAndRevealReferences(t *testing.T) {
	f := newChangeFixture(t)
	ctx := context.Background()
	h := strings.Repeat("d", 40)
	other := uuid.NewString()
	if _, err := f.s.Pool.Exec(ctx, `INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at)
	 SELECT $1,device_id,agent,'/synthetic/other','other',storage_kind,parser,now() FROM sources WHERE id=$2`, other, f.source); err != nil {
		t.Fatal(err)
	}
	cmd, _ := json.Marshal(map[string]string{"cmd": "git commit -m synthetic", "workdir": "/src/flopwire"})
	f.row(f.source, "same", "tool_call", string(cmd))
	f.row(other, "same", "tool_result", "[topic "+h+"] synthetic")
	if len(f.lookup([]string{h}, 10, "").Matches) != 0 {
		t.Fatal("paired across source identities")
	}
	quiet, _ := json.Marshal(map[string]string{"cmd": "git commit -qm synthetic", "workdir": "/src/flopwire"})
	command := f.row(f.source, "quiet", "tool_call", string(quiet))
	result := f.row(f.source, "quiet", "tool_result", "")
	reveal, _ := json.Marshal(map[string]string{"cmd": "git rev-parse HEAD", "workdir": "/src/flopwire"})
	revealCommand := f.row(f.source, "head", "tool_call", string(reveal))
	revealResult := f.row(f.source, "head", "tool_result", h)
	out := f.lookup([]string{h}, 10, "")
	if len(out.Matches) != 1 {
		t.Fatalf("quiet result: %+v", out)
	}
	m := out.Matches[0]
	if m.CommandAddress != command || m.ResultAddress != result || m.RevealCommandAddress != revealCommand || m.RevealResultAddress != revealResult {
		t.Fatalf("quiet references: %+v", m)
	}
	if _, err := f.s.Pool.Exec(ctx, `DELETE FROM messages WHERE id=$1`, revealResult); err != nil {
		t.Fatal(err)
	}
	if len(f.lookup([]string{h}, 10, "").Matches) != 0 {
		t.Fatal("deleted reveal retained evidence")
	}
}

func TestChangeServerGenerationIsolation(t *testing.T) {
	f := newChangeFixture(t)
	ctx := context.Background()
	h := strings.Repeat("1", 40)
	command, result := f.commit(h)
	if _, err := f.s.Pool.Exec(ctx, `UPDATE messages SET source_generation=1 WHERE id=$1`, result); err != nil {
		t.Fatal(err)
	}
	if len(f.lookup([]string{h}, 10, "").Matches) != 0 {
		t.Fatal("paired across replacement generations")
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE messages SET source_generation=1 WHERE id=$1`, command); err != nil {
		t.Fatal(err)
	}
	if len(f.lookup([]string{h}, 10, "").Matches) != 1 {
		t.Fatal("completed replacement not indexed")
	}
}

func TestChangeServerRetainedTextCoverage(t *testing.T) {
	f := newChangeFixture(t)
	h := strings.Repeat("3", 40)
	_, result := f.commit(h)
	if _, err := f.s.Pool.Exec(context.Background(), `UPDATE messages SET text_len=text_len+100 WHERE id=$1`, result); err != nil {
		t.Fatal(err)
	}
	out := f.lookup([]string{h}, 20, "")
	if !out.Coverage.SourceTextTruncated || out.Coverage.HistoryComplete {
		t.Fatalf("capped source coverage: %+v", out.Coverage)
	}
}

func TestChangeServerRebuiltIndexRestarts(t *testing.T) {
	f := newChangeFixture(t)
	h := strings.Repeat("4", 40)
	f.commit(h)
	f.commit(h)
	first := f.lookup([]string{h}, 1, "")
	if _, err := f.s.Pool.Exec(context.Background(), `UPDATE commit_evidence_state SET index_id=gen_random_uuid(),cursor_secret=decode(replace(gen_random_uuid()::text,'-','')||replace(gen_random_uuid()::text,'-',''),'hex') WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	out := f.lookup([]string{h}, 1, first.NextCursor)
	if !out.RestartRequired || len(out.Matches) != 0 {
		t.Fatalf("rebuilt index cursor: %+v", out)
	}
}

func TestChangeServerIndexedScale(t *testing.T) {
	f := newChangeFixture(t)
	ctx := context.Background()
	command, result := f.commit(strings.Repeat("2", 40))
	f.lookup([]string{strings.Repeat("2", 40)}, 20, "")
	if _, err := f.s.Pool.Exec(ctx, `INSERT INTO commit_evidence(conversation_id,source_id,source_generation,fingerprint,observed_hash,command_id,result_id,evidence)
	 SELECT $1,$2,0,'scale-'||i,lpad(to_hex(i),40,'0'),$3,$4,'{"operation":"commit","outcome":"succeeded","directory_known":true}'::jsonb FROM generate_series(1,10000)i`, f.conv, f.source, command, result); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `ANALYZE commit_evidence`); err != nil {
		t.Fatal(err)
	}
	commits := make([]string, 100)
	for i := range commits {
		commits[i] = fmt.Sprintf("%040x", 9901+i)
	}
	rows, err := f.s.Pool.Query(ctx, `EXPLAIN (ANALYZE,BUFFERS) SELECT hit.id FROM unnest($1::text[]) p(hash) CROSS JOIN LATERAL
	 (SELECT id FROM commit_evidence WHERE observed_hash=p.hash ORDER BY id LIMIT 21) hit ORDER BY hit.id LIMIT 21`, commits)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "commit_evidence_hash_idx") {
		t.Fatalf("selective lookup missed hash index:\n%s", joined)
	}
	start := time.Now()
	out := f.lookup(commits, 20, "")
	elapsed := time.Since(start)
	data, _ := json.Marshal(out)
	if len(out.Matches) != 20 || !out.HasMore {
		t.Fatalf("scale result: %+v", out)
	}
	t.Logf("10000 indexed observations,100 inputs,20-event page: %s, %d bytes; plan:\n%s", elapsed, len(data), joined)
}

func TestChangeServerOutOfOrderPublicationRestarts(t *testing.T) {
	f := newChangeFixture(t)
	ctx := context.Background()
	h := strings.Repeat("e", 40)
	c1, r1 := f.commit(h)
	c2, r2 := f.commit(h)
	c3, r3 := f.commit(h)
	if _, err := f.s.Pool.Exec(ctx, `DELETE FROM commit_evidence_dirty WHERE conversation_id=$1`, f.conv); err != nil {
		t.Fatal(err)
	}
	older, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer older.Rollback(ctx)
	newer, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Rollback(ctx)
	insert := func(tx pgx.Tx, key, command, result string) {
		t.Helper()
		_, err := tx.Exec(ctx, `INSERT INTO commit_evidence(conversation_id,source_id,source_generation,fingerprint,observed_hash,command_id,result_id,evidence)
		 VALUES($1,$2,0,$3,$4,$5,$6,'{"operation":"commit","outcome":"succeeded"}')`, f.conv, f.source, key, h, command, result)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(older, "old", c1, r1)
	insert(newer, "new1", c2, r2)
	insert(newer, "new2", c3, r3)
	if err := newer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	first := f.lookup([]string{h}, 1, "")
	if !first.HasMore {
		t.Fatal("missing cursor")
	}
	if err := older.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.lookup([]string{h}, 1, first.NextCursor).RestartRequired {
		t.Fatal("older ID publication entered stable cutoff")
	}
}

func TestChangeServerOversizedProjectionCoverageAndProgress(t *testing.T) {
	f := newChangeFixture(t)
	ctx := context.Background()
	h := strings.Repeat("f", 40)
	f.row(f.source, "large", "tool_result", "synthetic")
	if _, err := f.s.Pool.Exec(ctx, `UPDATE messages SET text=repeat('x',$2) WHERE conversation_id=$1`, f.conv, ingest.CommitEvidenceTextLimit+1); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, f.s.Pool, func(tx pgx.Tx) error { return ingest.RefreshCommitEvidence(ctx, tx, f.conv) }); err != nil {
		t.Fatal(err)
	}
	out := f.lookup([]string{h}, 10, "")
	if !out.Coverage.Truncated || out.Coverage.IndexingLag {
		t.Fatalf("bounded projection coverage: %+v", out.Coverage)
	}
	// The limited first conversation must not prevent a smaller one rebuilding.
	second := uuid.NewString()
	if _, err := f.s.Pool.Exec(ctx, `INSERT INTO conversations(id,agent,session_id,device_id,user_id,repo_root,source_id)
	 SELECT $1,agent,'second',device_id,user_id,repo_root,source_id FROM conversations WHERE id=$2`, second, f.conv); err != nil {
		t.Fatal(err)
	}
	f.conv = second
	f.commit(h)
	out = f.lookup([]string{h}, 10, "")
	if len(out.Matches) != 1 || !out.Coverage.Truncated {
		t.Fatalf("bounded source starved next source: %+v", out)
	}
}
