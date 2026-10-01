package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func apply(rec string, spans []Span) string {
	b := []byte(rec)
	FillSpans(b, spans, MessageRule)
	return string(b)
}

func TestMaskRecordNeedles(t *testing.T) {
	secret := "db pass is Tr0ub4dor&3 ok"
	inner, _ := json.Marshal(map[string]any{"message_id": "m1", "role": "tool", "content": "line one\n" + secret + "\nline é three"})
	rec, _ := json.Marshal(map[string]any{"t": "node", "row_id": 3, "chat_message": string(inner)})
	spans, found := MaskRecord(rec, []string{secret}, false)
	if !found || len(spans) != 1 {
		t.Fatalf("spans %v found %v", spans, found)
	}
	out := apply(string(rec), spans)
	if len(out) != len(rec) || strings.Contains(out, "Tr0ub4dor") {
		t.Fatalf("masked %q", out)
	}
	var outer map[string]any
	if err := json.Unmarshal([]byte(out), &outer); err != nil {
		t.Fatal(err)
	}
	var in map[string]any
	if err := json.Unmarshal([]byte(outer["chat_message"].(string)), &in); err != nil {
		t.Fatalf("nested JSON broken: %v", err)
	}
	if in["message_id"] != "m1" || !strings.Contains(in["content"].(string), "line é three") || !strings.Contains(in["content"].(string), "[REDACTED:message]") {
		t.Fatalf("inner %v", in)
	}
	if _, found := MaskRecord(rec, []string{"not there"}, false); found {
		t.Fatal("found a missing needle")
	}
}

func TestMaskRecordWhole(t *testing.T) {
	rec := `{"type":"user","uuid":"u1","timestamp":"2026-09-30T12:00:00Z","message":{"role":"user","content":[{"type":"text","text":"my key is abc\"def"}]},"cwd":"/w"}`
	spans, _ := MaskRecord([]byte(rec), nil, true)
	out := apply(rec, spans)
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	if v["uuid"] != "u1" || v["type"] != "user" || v["timestamp"] != "2026-09-30T12:00:00Z" || strings.Contains(out, "abc") || strings.Contains(out, `"/w"`) {
		t.Fatalf("whole mask: %s", out)
	}
	// Not JSON: plain text search, or everything.
	txt := "a\nsecret line here\nb\n"
	if s, ok := MaskRecord([]byte(txt), []string{"secret line here"}, false); !ok || apply(txt, s) != "a\n[REDACTED]******\nb\n" {
		t.Fatalf("text mask %v", s)
	}
}

func TestMaskText(t *testing.T) {
	got, hidden := MaskText("one\ntwo secret\n\nfour", 2, 3)
	if got != "one\n[REDACTED]\n\nfour" || len(hidden) != 1 || hidden[0] != "two secret" {
		t.Fatalf("%q %q", got, hidden)
	}
	if got, _ := MaskText("abcdefghijklmnopqrstuvwxyz", 0, 0); got != "[REDACTED:message]********" {
		t.Fatalf("%q", got)
	}
}

func TestReaderAtLineMasks(t *testing.T) {
	src := "{\"a\":\"keep\"}\n{\"a\":\"hide me please\"}\n{\"a\":\"keep\"}"
	lines := strings.SplitAfter(src, "\n")
	sum := LineSum([]byte(lines[1]))
	x := NewReaderAt(strings.NewReader(src), Lines)
	x.SetLineMasks(NewLineCatalog([]LineMask{{SHA: sum, Spans: []Span{{6, 20}}}}))
	out := make([]byte, len(src))
	x.ReadAt(out, 0)
	want := lines[0] + "{\"a\":\"[REDACTED]****\"}\n" + lines[2]
	if string(out) != want {
		t.Fatalf("%q", out)
	}
}
