package transcript

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func readAll(t *testing.T, data []byte, start, lineNo int64, opts LineReaderOptions) ([]Line, *LineReader) {
	t.Helper()
	lr := NewLineReader(bytes.NewReader(data), start, lineNo, int64(len(data)), opts)
	var out []Line
	for {
		l, err := lr.Next()
		if errors.Is(err, io.EOF) {
			return out, lr
		}
		if err != nil {
			t.Fatal(err)
		}
		c := *l
		c.Data = bytes.Clone(l.Data)
		c.Head = bytes.Clone(l.Head)
		out = append(out, c)
	}
}

func TestLineReaderOffsetsCRLFAndPartialTail(t *testing.T) {
	data := []byte("{\"a\":1}\n\r\n{\"b\":2}\r\n\n{\"c\":")
	lines, lr := readAll(t, data, 0, 0, LineReaderOptions{})
	want := []struct {
		off, n int64
		data   string
	}{{0, 8, `{"a":1}`}, {8, 2, ""}, {10, 9, `{"b":2}`}, {19, 1, ""}}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d", len(lines), len(want))
	}
	for i, w := range want {
		l := lines[i]
		if l.No != int64(i+1) || l.Offset != w.off || l.Len != w.n || string(l.Data) != w.data {
			t.Errorf("line %d = {No:%d Off:%d Len:%d %q}, want {%d %d %q}", i, l.No, l.Offset, l.Len, l.Data, w.off, w.n, w.data)
		}
		if l.ContentLen != int64(len(w.data)) {
			t.Errorf("line %d ContentLen = %d", i, l.ContentLen)
		}
	}
	if lr.Offset() != 20 || lr.LineNo() != 4 || lr.Pending() != 5 {
		t.Fatalf("cursor = (%d, %d, pending %d), want (20, 4, 5)", lr.Offset(), lr.LineNo(), lr.Pending())
	}

	// The writer finishes the partial line; resume from the cursor.
	data = append(data, []byte("3}\n")...)
	lines, lr = readAll(t, data, 20, 4, LineReaderOptions{})
	if len(lines) != 1 || lines[0].No != 5 || lines[0].Offset != 20 || string(lines[0].Data) != `{"c":3}` {
		t.Fatalf("resumed lines = %+v", lines)
	}
	if lr.Offset() != int64(len(data)) || lr.Pending() != 0 {
		t.Fatalf("cursor after resume = %d pending %d", lr.Offset(), lr.Pending())
	}
}

func TestLineReaderLongLinesMaterialized(t *testing.T) {
	// 1.2MB line spans many 64KB bufio reads but stays under MaxLine.
	big := `{"type":"tool_result","text":"` + strings.Repeat("x", 1200<<10) + `"}`
	data := []byte("{}\n" + big + "\r\n{}\n")
	lines, _ := readAll(t, data, 0, 0, LineReaderOptions{})
	if len(lines) != 3 || string(lines[1].Data) != big || lines[1].Oversized {
		t.Fatalf("big line not materialized intact: %d lines", len(lines))
	}
	if lines[2].Offset != int64(3+len(big)+2) {
		t.Fatalf("offset after big line = %d", lines[2].Offset)
	}
}

func TestLineReaderOversizedPeekAndStream(t *testing.T) {
	payload := strings.Repeat("QUJD", 300<<10) // 1.2MB of base64
	big := `{"type":"image","source":{"data":"` + payload + `"},"uuid":"u-1"}`
	data := []byte(big + "\r\n{\"type\":\"user\"}\n")
	lines, lr := readAll(t, data, 0, 0, LineReaderOptions{MaxLine: 256 << 10, HeadSize: 1024})
	if len(lines) != 2 {
		t.Fatalf("got %d lines", len(lines))
	}
	l := lines[0]
	if !l.Oversized || l.Data != nil || len(l.Head) != 1024 || l.ContentLen != int64(len(big)) || l.Len != int64(len(big)+2) {
		t.Fatalf("oversized line = {Oversized:%v Head:%d ContentLen:%d Len:%d}", l.Oversized, len(l.Head), l.ContentLen, l.Len)
	}
	if typ, ok := PeekLine(&l, "type"); !ok || typ != "image" {
		t.Fatalf("PeekLine type = %q, %v", typ, ok)
	}
	if _, ok := PeekLine(&l, "uuid"); ok {
		t.Fatal("uuid lies past the head; PeekLine must report false")
	}
	if id, ok := PeekString(lr.Open(&l), "uuid"); !ok || id != "u-1" {
		t.Fatalf("streamed uuid = %q, %v", id, ok)
	}
	full, _ := io.ReadAll(lr.Open(&l))
	if string(full) != big {
		t.Fatal("Open did not stream the whole line")
	}
	if lines[1].Offset != int64(len(big)+2) || string(lines[1].Data) != `{"type":"user"}` {
		t.Fatalf("line after oversized = %+v", lines[1])
	}
}

func TestLineReaderRandomMatchesSplit(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var buf bytes.Buffer
	var want []string
	for i := 0; i < 400; i++ {
		n := rng.Intn(4)
		switch n {
		case 0:
			n = 0
		case 1:
			n = rng.Intn(100)
		case 2:
			n = rng.Intn(70 << 10)
		default:
			n = rng.Intn(300 << 10)
		}
		s := strings.Repeat(string(rune('a'+i%26)), n)
		want = append(want, s)
		buf.WriteString(s)
		if i%3 == 0 {
			buf.WriteString("\r\n")
		} else {
			buf.WriteString("\n")
		}
	}
	data := buf.Bytes()
	lines, lr := readAll(t, data, 0, 0, LineReaderOptions{MaxLine: 128 << 10, HeadSize: 100})
	if len(lines) != len(want) || lr.Offset() != int64(len(data)) {
		t.Fatalf("got %d lines ending %d, want %d ending %d", len(lines), lr.Offset(), len(want), len(data))
	}
	var off int64
	for i, l := range lines {
		if l.Offset != off {
			t.Fatalf("line %d offset %d, want %d", i+1, l.Offset, off)
		}
		if l.ContentLen != int64(len(want[i])) {
			t.Fatalf("line %d content len %d, want %d", i+1, l.ContentLen, len(want[i]))
		}
		got := string(l.Data)
		if l.Oversized {
			got = string(l.Head)
			if !strings.HasPrefix(want[i], got) || len(got) != 100 {
				t.Fatalf("line %d head mismatch", i+1)
			}
		} else if got != want[i] {
			t.Fatalf("line %d content mismatch", i+1)
		}
		off += l.Len
	}
}

func TestPeekStringNested(t *testing.T) {
	doc := `{"timestamp":"t","payload":{"content":[{"type":"x"}],"type":"function_call"},"type":"response_item"}`
	if v, ok := PeekString(strings.NewReader(doc), "payload", "type"); !ok || v != "function_call" {
		t.Fatalf("payload.type = %q, %v", v, ok)
	}
	if v, ok := PeekString(strings.NewReader(doc), "type"); !ok || v != "response_item" {
		t.Fatalf("type = %q, %v", v, ok)
	}
	for _, bad := range []string{`[]`, `{"type":1}`, `{"payload":"s"}`, `{"typ`} {
		if _, ok := PeekString(strings.NewReader(bad), "payload", "type"); ok {
			t.Errorf("PeekString(%s) reported a match", bad)
		}
	}
}

// Readers sharing a LineBudget hold at most its capacity in large lines at
// once (one line alone may exceed it), and give it back line by line.
func TestLineBudgetBoundsLargeLines(t *testing.T) {
	big := strings.Repeat("x", BigLine+10) + "\n"
	data := []byte(big + big + "small\n")
	b := NewLineBudget(int64(BigLine) + 100)
	opts := LineReaderOptions{MaxLine: 4 * BigLine, Budget: b}
	r1 := NewLineReader(bytes.NewReader(data), 0, 0, int64(len(data)), opts)
	if _, err := r1.Next(); err != nil {
		t.Fatal(err)
	}
	if usedOf(b) != int64(len(big)) {
		t.Fatalf("reserved %d after one line, want its size %d", usedOf(b), len(big))
	}
	got := make(chan error, 1)
	go func() {
		r2 := NewLineReader(bytes.NewReader(data), 0, 0, int64(len(data)), opts)
		_, err := r2.Next() // waits: the budget cannot hold a second large line
		r2.Release()
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("second reader did not wait for the budget")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := r1.Next(); err != nil { // releases the first line, reserves the second
		t.Fatal(err)
	}
	if l, err := r1.Next(); err != nil || string(l.Data) != "small" {
		t.Fatalf("small line: %v", err)
	}
	if r1.held != 0 {
		t.Fatalf("small line holds %d of the budget", r1.held)
	}
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if n := usedOf(b); n != 0 {
		t.Fatalf("budget holds %d after both readers finished", n)
	}
}

// usedOf reads the budget's reservation under its lock.
func usedOf(b *LineBudget) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// An oversized line a parser decodes whole reserves its full size, so two
// readers never hold two such lines beyond the budget at once (P5).
func TestLineBudgetCoversMaterializedOversizedLines(t *testing.T) {
	big := strings.Repeat("y", 3*BigLine) + "\n"
	data := []byte(big + "small\n")
	b := NewLineBudget(int64(2 * BigLine))
	opts := LineReaderOptions{MaxLine: BigLine + BigLine/2, Budget: b}
	r1 := NewLineReader(bytes.NewReader(data), 0, 0, int64(len(data)), opts)
	l, err := r1.Next()
	if err != nil || !l.Oversized {
		t.Fatalf("want an oversized line, got %+v %v", l, err)
	}
	buf, err := r1.Materialize(l, nil)
	if err != nil || len(buf) != 3*BigLine {
		t.Fatalf("materialized %d bytes: %v", len(buf), err)
	}
	if b.used != int64(3*BigLine) {
		t.Fatalf("reserved %d while materialized, want the line's size %d", b.used, 3*BigLine)
	}
	got := make(chan error, 1)
	go func() {
		r2 := NewLineReader(bytes.NewReader(data), 0, 0, int64(len(data)), opts)
		l2, err := r2.Next() // waits: the first reader holds more than the cap
		if err == nil {
			_, err = r2.Materialize(l2, nil)
		}
		r2.Release()
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("second reader did not wait for the budget")
	case <-time.After(50 * time.Millisecond):
	}
	if l, err := r1.Next(); err != nil || string(l.Data) != "small" {
		t.Fatalf("small line: %v", err)
	}
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if n := usedOf(b); n != 0 {
		t.Fatalf("budget holds %d after both readers finished", n)
	}
}
