package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// toyParser exercises the Parser contract: a header line sets the session
// id, carried across calls in Cursor.State; every other line is a message.
type toyParser struct{}

func (toyParser) Name() string             { return "toy@1" }
func (toyParser) Agent() Agent             { return "toy" }
func (toyParser) StorageKind() StorageKind { return StorageJSONLAppend }

func (p toyParser) Parse(ctx context.Context, in Input, cur Cursor, sink Sink) (Cursor, error) {
	session := string(cur.State)
	next, err := ScanJSONL(ctx, in, cur, LineReaderOptions{}, func(_ *LineReader, l *Line) error {
		var rec struct{ Session, ID, Text string }
		if json.Unmarshal(l.Data, &rec) != nil {
			return nil // unknown or malformed: ignore, never fail
		}
		if rec.Session != "" {
			session = rec.Session
			return sink.Conversation(&Conversation{Agent: p.Agent(), SessionID: session})
		}
		m := &Message{SessionID: session, NativeID: rec.ID, Kind: KindUser, Ordinal: OrdinalAt(l.Offset, 0),
			LineNo: l.No, ByteOffset: l.Offset, ByteLen: l.Len, Parser: p.Name()}
		m.SetText(rec.Text, Uncapped)
		return sink.Message(m)
	})
	next.State = []byte(session)
	return next, err
}

func TestIncrementalParseMatchesFullReparse(t *testing.T) {
	data := []byte("{\"session\":\"s1\"}\n{\"id\":\"a\",\"text\":\"one\"}\nnot json\n{\"id\":\"b\",\"text\":\"two\"}\n{\"id\":\"c\",")
	ctx := context.Background()

	var full Collector
	cur, err := Reparse(ctx, toyParser{}, Input{R: bytes.NewReader(data), Size: int64(len(data))}, &full)
	if err != nil || cur.Offset != int64(len(data)-len(`{"id":"c",`)) || cur.LineNo != 4 {
		t.Fatalf("cursor %+v, err %v", cur, err)
	}
	if len(full.Messages) != 2 {
		t.Fatalf("partial final line must not be emitted: %d messages", len(full.Messages))
	}

	// Complete the tail and resume, then compare with a fresh full parse.
	data = append(data, []byte("\"text\":\"three\"}\n")...)
	in := Input{R: bytes.NewReader(data), Size: int64(len(data))}
	var tail Collector
	p := toyParser{}
	if _, err := p.Parse(ctx, in, cur, &tail); err != nil {
		t.Fatal(err)
	}
	var again Collector
	if _, err := Reparse(ctx, toyParser{}, in, &again); err != nil {
		t.Fatal(err)
	}
	incremental := append(full.Messages, tail.Messages...)
	if !reflect.DeepEqual(incremental, again.Messages) {
		t.Fatalf("incremental %+v\nfull %+v", incremental, again.Messages)
	}
	if got := again.MessagesFor("s1"); len(got) != 3 || got[2].LineNo != 5 {
		t.Fatalf("MessagesFor = %+v", got)
	}
}

func TestKindText(t *testing.T) {
	for k := KindUnknown; k <= KindSystem; k++ {
		b, _ := k.MarshalText()
		var back Kind
		if err := back.UnmarshalText(b); err != nil || back != k {
			t.Fatalf("round trip %v: %v %v", k, back, err)
		}
	}
	if _, err := ParseKind("tool"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}
