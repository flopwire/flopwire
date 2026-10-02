package ingest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/transcript"
)

func persistedExtraction(t *testing.T, e *env, id string) (*transcript.ExtractionCheckpoint, int64, int64) {
	t.Helper()
	var data []byte
	var off, line int64
	if err := e.pool.QueryRow(e.ctx, "SELECT extraction_report,cursor_offset,cursor_line FROM source_parse_state WHERE source_id=$1", id).Scan(&data, &off, &line); err != nil {
		t.Fatal(err)
	}
	if data == nil {
		return nil, off, line
	}
	var cp transcript.ExtractionCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatal(err)
	}
	return &cp, off, line
}
func TestExtractionReportCheckpointFailureRetryAndContractUpgrade(t *testing.T) {
	e := newEnv(t)
	sp := bulkSpec(t, 1)
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	var id string
	if err := e.pool.QueryRow(e.ctx, "SELECT id::text FROM sources WHERE device_id=$1 AND path=$2", e.deviceID, sp.Path).Scan(&id); err != nil {
		t.Fatal(err)
	}
	first, off, line := persistedExtraction(t, e, id)
	if first == nil || len(first.Report.Issues) != 0 {
		t.Fatal("clean report not assessed")
	}
	appendFile(t, sp.Path, "{broken\n"+`{"type":"user","uuid":"after","sessionId":"0b7e2c1a-0000-4000-8000-0000000000b1","message":{"content":"after bad line"}}`+"\n")
	sync1(t, sy, sp)
	e.exec(`CREATE FUNCTION reject_report() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'blocked extraction checkpoint'; END $$`)
	e.exec(`CREATE TRIGGER reject_report BEFORE UPDATE ON source_parse_state FOR EACH ROW WHEN (NEW.extraction_report IS DISTINCT FROM OLD.extraction_report) EXECUTE FUNCTION reject_report()`)
	if err := e.queue.ParseSource(e.ctx, id); err == nil || !strings.Contains(err.Error(), "blocked extraction checkpoint") {
		t.Fatalf("unexpected failure %v", err)
	}
	failed, failedOff, failedLine := persistedExtraction(t, e, id)
	if !reflect.DeepEqual(failed, first) || failedOff != off || failedLine != line {
		t.Fatal("failed report commit advanced checkpoint")
	}
	if e.count("SELECT count(*) FROM messages WHERE source_id=$1", id) != 2 {
		t.Fatal("test did not exercise replay after committed message batch")
	}
	e.exec(`UPDATE messages SET native_id='failed-attempt-native-id' WHERE source_id=$1 AND native_id='after#0'`, id)
	e.exec("DROP TRIGGER reject_report ON source_parse_state")
	e.exec(`UPDATE source_parse_state SET extraction_report=jsonb_set(extraction_report,'{contract}','"old-contract"') WHERE source_id=$1`, id)
	e.drain()
	saved, nextOff, nextLine := persistedExtraction(t, e, id)
	if saved.Report.Issues[0].Count != 1 || nextOff <= off || nextLine != line+2 {
		t.Fatal("retry lost report or counted twice")
	}
	if e.count("SELECT count(*) FROM messages WHERE source_id=$1 AND NOT superseded", id) != 2 {
		t.Fatal("retry retained obsolete failed-attempt row")
	}
	e.start()
	e.drain()
	same, _, _ := persistedExtraction(t, e, id)
	if !reflect.DeepEqual(saved, same) {
		t.Fatal("restart changed report")
	}
	// Simulate an older contract emitting a different identity. Its row must
	// remain live on checkpoint failure, then retire on successful replacement.
	e.exec(`UPDATE messages SET native_id='old-contract-native-id' WHERE source_id=$1 AND native_id='after#0'`, id)
	e.exec(`UPDATE source_parse_state SET extraction_report=jsonb_set(extraction_report,'{contract}','"old-contract"'),requested_seq=requested_seq+1 WHERE source_id=$1`, id)
	e.exec(`CREATE TRIGGER reject_report BEFORE UPDATE ON source_parse_state FOR EACH ROW WHEN (NEW.extraction_report IS DISTINCT FROM OLD.extraction_report) EXECUTE FUNCTION reject_report()`)
	if err := e.queue.ParseSource(e.ctx, id); err == nil {
		t.Fatal("replacement checkpoint unexpectedly committed")
	}
	if e.count(`SELECT count(*) FROM messages WHERE source_id=$1 AND native_id='old-contract-native-id' AND NOT superseded`, id) != 1 {
		t.Fatal("failed replacement retired old contract row")
	}
	e.exec("DROP TRIGGER reject_report ON source_parse_state")
	e.drain()
	if e.count("SELECT count(*) FROM messages WHERE source_id=$1 AND NOT superseded", id) != 2 {
		t.Fatal("same-generation reparse left absent rows live")
	}
	var liveRows, digestRows int
	if err := e.pool.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM messages WHERE source_id=$1 AND NOT superseded),(SELECT COALESCE(sum(v::int),0) FROM conversation_activity c,jsonb_each_text(c.digest->'messages') x(k,v) WHERE c.conversation_id IN (SELECT conversation_id FROM messages WHERE source_id=$1))`, id).Scan(&liveRows, &digestRows); err != nil || digestRows != liveRows {
		t.Fatalf("replacement digest %d vs live rows %d: %v", digestRows, liveRows, err)
	}
	// A changed contract schedules a full reparse even without another upload.
	e.exec(`UPDATE source_parse_state SET extraction_report=jsonb_set(extraction_report,'{contract}','"old-contract"') WHERE source_id=$1`, id)
	e.drain()
	upgraded, _, _ := persistedExtraction(t, e, id)
	if upgraded.Contract != serverExtractionContract("claude") || upgraded.Report.Issues[0].Count != 1 {
		t.Fatal("contract reparse merged old observations")
	}
	// Legacy NULL checkpoints receive the same full assessment.
	e.exec("UPDATE source_parse_state SET extraction_report=NULL WHERE source_id=$1", id)
	e.drain()
	assessed, _, _ := persistedExtraction(t, e, id)
	if assessed == nil || assessed.Report.Issues[0].Count != 1 {
		t.Fatal("legacy cursor was not assessed")
	}
}
func TestReportAcknowledgesOnlyCapturedParseRequest(t *testing.T) {
	e := newEnv(t)
	sp := bulkSpec(t, 1)
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	var id string
	var seq, gen int64
	var cursor transcript.Cursor
	if err := e.pool.QueryRow(e.ctx, `SELECT p.source_id::text,p.parsed_seq,p.generation,p.cursor_offset,p.cursor_line,p.cursor_state FROM source_parse_state p JOIN sources s ON s.id=p.source_id WHERE s.path=$1`, sp.Path).Scan(&id, &seq, &gen, &cursor.Offset, &cursor.LineNo, &cursor.State); err != nil {
		t.Fatal(err)
	}
	cp, _, _ := persistedExtraction(t, e, id)
	e.exec("UPDATE source_parse_state SET requested_seq=requested_seq+1 WHERE source_id=$1", id)
	if err := e.queue.done(e.ctx, &job{src: source{id: id}, seq: seq, cursor: cursor, extraction: cp}, gen); err != nil {
		t.Fatal(err)
	}
	if e.count("SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND requested_seq>parsed_seq", id) != 1 {
		t.Fatal("new request swallowed by checkpoint commit")
	}
}
