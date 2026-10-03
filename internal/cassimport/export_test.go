package cassimport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestRecoverStoredHistory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database := filepath.Join(root, "cass.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE agents(id INTEGER,slug TEXT);CREATE TABLE workspaces(id INTEGER,path TEXT);CREATE TABLE conversations(id INTEGER,agent_id INTEGER,workspace_id INTEGER,external_id TEXT,title TEXT,source_path TEXT,started_at INTEGER,ended_at INTEGER,source_id TEXT,origin_host TEXT,metadata_json TEXT,metadata_bin BLOB);CREATE TABLE messages(id INTEGER,conversation_id INTEGER,idx INTEGER,role TEXT,author TEXT,created_at INTEGER,content TEXT,extra_json TEXT,extra_bin BLOB);
 INSERT INTO agents VALUES(1,'claude_code');INSERT INTO workspaces VALUES(1,'/work/recovered');INSERT INTO conversations VALUES(42,1,1,'original-session','Original title','/gone/transcript.jsonl',1700000000000,1700000001000,'local','mac-original','{"number":9007199254740993}',X'010203');
 INSERT INTO messages VALUES(4,42,7,'user',NULL,1700000000001,'recover me',NULL,X'040506');INSERT INTO messages VALUES(5,42,8,'tool','shell',NULL,'opaque tool text','{"original_id":"tool-uuid"}',NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO conversations SELECT 43,agent_id,workspace_id,'/capture-a/original-session',title,source_path,started_at,ended_at,source_id,origin_host,metadata_json,metadata_bin FROM conversations WHERE id=42;
 INSERT INTO conversations SELECT 44,agent_id,workspace_id,'/capture-b/original-session',title,source_path,started_at,ended_at,source_id,origin_host,metadata_json,metadata_bin FROM conversations WHERE id=42;`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "export")
	m, err := Export(ctx, database, out, "mac-original", []int64{42})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Messages != 2 {
		t.Fatalf("manifest %#v", m)
	}
	b, err := os.ReadFile(filepath.Join(out, m.Entries[0].File))
	if err != nil {
		t.Fatal(err)
	}
	c := &transcript.Collector{}
	cur, err := Parse(ctx, transcript.Input{Source: &transcript.Source{Agent: transcript.AgentClaude}, R: bytes.NewReader(b), Size: int64(len(b))}, c)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Offset != int64(len(b)) || len(c.Conversations) != 1 || len(c.Messages) != 2 {
		t.Fatalf("incomplete recovery")
	}
	conv := c.Conversations[0]
	if conv.SessionID != "original-session" || conv.Cwd != "/work/recovered" || conv.Extra["cass_source_path"] != "/gone/transcript.jsonl" || conv.Extra["cass_metadata_json"] != `{"number":9007199254740993}` {
		t.Fatalf("conversation metadata changed: %#v", conv)
	}
	if c.Messages[0].Text != "recover me" || c.Messages[0].TS.UnixMilli() != 1700000000001 || c.Messages[1].Kind != transcript.KindUnknown || c.Messages[1].Role != "tool" || c.Messages[1].ToolName != "" || c.Messages[1].Enrichment["cass_extra_json"] != `{"original_id":"tool-uuid"}` {
		t.Fatalf("message evidence changed")
	}
	after, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("CASS database changed")
	}
	aliases, err := Export(ctx, database, filepath.Join(root, "aliases"), "mac-original", []int64{43, 44})
	if err != nil {
		t.Fatal(err)
	}
	if aliases.Entries[0].SessionID == aliases.Entries[1].SessionID || strings.Contains(aliases.Entries[0].SessionID, "/") {
		t.Fatal("path-shaped CASS IDs produced duplicate or unreadable addresses")
	}
	if _, err = Export(ctx, database, out, "mac-original", []int64{42}); err == nil {
		t.Fatal("overwrote existing recovery evidence")
	}
}

func TestRecoveryRejectsBrokenEvidence(t *testing.T) {
	var header bytes.Buffer
	json.NewEncoder(&header).Encode(Record{Version: 1, Conversation: &transcript.Conversation{Agent: transcript.AgentCodex, SessionID: "s"}})
	for _, tail := range []string{`{"version":2,"message":{}}`, `{"version":1,"message":{"SessionID":"other"}}`, `{"version":1,"message":{}} trailing`, `{"version":1,"message":`} {
		b := append(append([]byte{}, header.Bytes()...), []byte(tail+"\n")...)
		_, err := Parse(context.Background(), transcript.Input{Source: &transcript.Source{Agent: transcript.AgentCodex}, R: bytes.NewReader(b), Size: int64(len(b))}, &transcript.Collector{})
		if err == nil {
			t.Fatalf("accepted broken evidence %s", tail)
		}
	}
}

func TestRecoveryPreservesOversizedMessage(t *testing.T) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.Encode(Record{Version: 1, Conversation: &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "s"}})
	text := strings.Repeat("X", transcript.DefaultMaxLine+1024)
	enc.Encode(Record{Version: 1, Message: &transcript.Message{SessionID: "s", Text: text, Role: "user", Kind: transcript.KindUser}})
	c := &transcript.Collector{}
	_, err := Parse(context.Background(), transcript.Input{Source: &transcript.Source{Agent: transcript.AgentClaude}, R: bytes.NewReader(b.Bytes()), Size: int64(b.Len())}, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Messages) != 1 || c.Messages[0].Text != text || c.Messages[0].FullLen != len(text) {
		t.Fatal("oversized message lost or truncated")
	}
}

func TestRecoveryRejectsIncompleteTail(t *testing.T) {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(Record{Version: 1, Conversation: &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "s"}})
	b.WriteString(`{"version":1,"message":{"SessionID":"s","Text":"unsealed"}}`)
	_, err := Parse(context.Background(), transcript.Input{Source: &transcript.Source{Agent: transcript.AgentClaude}, R: bytes.NewReader(b.Bytes()), Size: int64(b.Len())}, &transcript.Collector{})
	if err == nil {
		t.Fatal("accepted incomplete recovery tail")
	}
}
