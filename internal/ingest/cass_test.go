package ingest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/cassimport"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestCASSRecoverySyncReparse(t *testing.T) {
	for _, agent := range []transcript.Agent{transcript.AgentClaude, transcript.AgentCodex, transcript.AgentDevin} {
		t.Run(string(agent), func(t *testing.T) {
			e := newEnv(t)
			sy := e.syncer(devicesync.Config{})
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			enc.Encode(cassimport.Record{Version: 1, Conversation: &transcript.Conversation{Agent: agent, SessionID: "original", Cwd: "/work/recovery", Extra: map[string]any{"recovered_history": true, "cass_source_path": "/gone/source"}}})
			enc.Encode(cassimport.Record{Version: 1, Message: &transcript.Message{SessionID: "original", NativeID: "cass:mac:1", Kind: transcript.KindUser, Role: "user", Text: "recovered searchable history", Enrichment: map[string]any{"recovered_history": true}}})
			p := filepath.Join(t.TempDir(), "recovery.jsonl")
			if err := os.WriteFile(p, b.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			sp := devicesync.SourceSpec{Path: p, Agent: agent, StorageKind: cassimport.StorageKind, Parser: cassimport.Name, SessionKey: "original"}
			sync1(t, sy, sp)
			e.drain()
			if n := e.count(`SELECT count(*) FROM messages WHERE text='recovered searchable history' AND parser='cass@1' AND NOT superseded`); n != 1 {
				t.Fatalf("recovered messages: %d", n)
			}
			if n := e.count(`SELECT count(*) FROM conversations WHERE session_id='original' AND agent=$1 AND extra->>'cass_source_path'='/gone/source'`, string(agent)); n != 1 {
				t.Fatalf("provenance: %d", n)
			}
			if n := e.count(`SELECT count(*) FROM source_parse_state p JOIN sources s ON s.id=p.source_id WHERE `+staleParseSQL, reparseArgs()...); n != 0 {
				t.Fatal("recovery repeatedly scheduled as native format")
			}
			e.exec(`UPDATE source_parse_state SET reparse=true,requested_seq=requested_seq+1`)
			e.drain()
			sync1(t, sy, sp)
			e.drain()
			if n := e.count(`SELECT count(*) FROM messages WHERE NOT superseded`); n != 1 {
				t.Fatalf("replay duplicated messages: %d", n)
			}
		})
	}
}
