package localindex

import (
	"crypto/sha256"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// BenchmarkChangeProjectionReplay measures retained-evidence replay independently
// of parsing and FTS insertion. Replaying must preserve evidence and ingestion IDs.
func BenchmarkChangeProjectionReplay(b *testing.B) {
	p := newPerfIndex(b)
	var msgs []*transcript.Message
	for i := 0; i < 128; i++ {
		pair := changePair("projection-bench", i, "abcdef0")
		pair[1].Text += strings.Repeat(" safe output", 512)
		pair[1].ContentSHA = sha256.Sum256([]byte(pair[1].Text))
		pair[1].FullLen = len(pair[1].Text)
		msgs = append(msgs, pair...)
	}
	_, err := p.s.ApplyBatch(ctx, Batch{SourceID: p.id, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "projection-bench", Cwd: "/repo"}}, Messages: msgs})
	if err != nil {
		b.Fatal(err)
	}
	var conv int64
	if err = p.s.DB().QueryRow(`SELECT id FROM conversations WHERE session_id='projection-bench'`).Scan(&conv); err != nil {
		b.Fatal(err)
	}
	snapshot := func() []string {
		rows, e := p.s.DB().Query(`SELECT json_array(id,event_key,evidence,source_id,source_generation,command_id,result_id,reveal_command_id,reveal_result_id) FROM change_events ORDER BY id`)
		if e != nil {
			b.Fatal(e)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if e = rows.Scan(&s); e != nil {
				b.Fatal(e)
			}
			out = append(out, s)
		}
		if e = rows.Err(); e != nil {
			b.Fatal(e)
		}
		return out
	}
	expected := snapshot()
	if len(expected) != 128 {
		b.Fatalf("events %d", len(expected))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.s.write(ctx, func(w *writeTx) error {
			return w.refreshChangeEvents(conv)
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if !reflect.DeepEqual(expected, snapshot()) {
		b.Fatal("projection changed stored evidence or identities")
	}
}
