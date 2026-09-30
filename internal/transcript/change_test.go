package transcript

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failReader fails the test if Decide reads anything.
type failReader struct{ t *testing.T }

func (f failReader) ReadAt([]byte, int64) (int, error) {
	f.t.Fatal("Decide read the file although the tuple was unchanged")
	return 0, errors.New("unreachable")
}

// indexFile parses path to its last complete line and returns the watermark,
// as the device agent does after a parse.
func indexFile(t *testing.T, path string) Watermark {
	t.Helper()
	id, err := StatIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	sampled := time.Now()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lr := NewLineReader(f, 0, 0, id.Size, LineReaderOptions{})
	for {
		if _, err := lr.Next(); err != nil {
			break
		}
	}
	w, err := NewWatermark(f, id, sampled, Cursor{Offset: lr.Offset(), LineNo: lr.LineNo()})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func decide(t *testing.T, w *Watermark, path string) Change {
	t.Helper()
	id, err := StatIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := Decide(w, id, f)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeAt overwrites bytes in place without changing the inode.
func writeAt(t *testing.T, path string, off int64, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte(s), off); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// tick waits past the coarsest ctime granularity CI kernels use.
func tick() { time.Sleep(30 * time.Millisecond) }

// lines builds n JSONL lines of roughly width bytes each.
func lines(prefix string, n, width int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(`{"id":"` + prefix + `","pad":"` + strings.Repeat("p", width) + "\"}\n")
	}
	return b.String()
}

func TestDecideNoWatermarkIsRewrite(t *testing.T) {
	c, err := Decide(nil, Identity{Size: 10}, bytes.NewReader(nil))
	if err != nil || c.Decision != Rewrite {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestDecideUnchangedTupleReadsNothing(t *testing.T) {
	w := &Watermark{Identity: Identity{ID: FileID{1, 2}, Size: 100, CTime: 1_000}, SampledAt: 1_000 + int64(time.Hour), Offset: 100}
	c, err := Decide(w, w.Identity, failReader{t})
	if err != nil || c.Decision != Unchanged || c.Verified {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestDecideRacyUnchangedIsVerified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, lines("a", 3, 10))
	w := indexFile(t, path) // just written: ctime is within RacyWindow
	if !w.Racy() {
		t.Skip("filesystem ctime not racy relative to wall clock")
	}
	c := decide(t, &w, path)
	if c.Decision != Unchanged || !c.Verified {
		t.Fatalf("got %+v", c)
	}
}

func TestDecideAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, lines("a", 3, 10)+`{"partial":`)
	w := indexFile(t, path)
	tick()
	appendFile(t, path, "1}\n"+lines("b", 2, 10))
	if c := decide(t, &w, path); c.Decision != Append {
		t.Fatalf("got %+v", c)
	}
	// Resuming at the watermark yields the completed partial line first.
	f, _ := os.Open(path)
	defer f.Close()
	fi, _ := f.Stat()
	lr := NewLineReader(f, w.Offset, w.LineNo, fi.Size(), LineReaderOptions{})
	l, err := lr.Next()
	if err != nil || string(l.Data) != `{"partial":1}` || l.No != 4 {
		t.Fatalf("first resumed line = %+v, %v", l, err)
	}
}

func TestDecideSameSizeRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := lines("a", 2000, 60) // > head + anchor windows
	writeFile(t, path, body)
	w := indexFile(t, path)
	tick()
	mid := int64(len(body) / 2)
	writeAt(t, path, mid, "Z") // middle byte, outside head and anchor
	fi, _ := os.Stat(path)
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil { // restore mtime: must not matter
		t.Fatal(err)
	}
	c := decide(t, &w, path)
	if c.Decision != Rewrite || !strings.Contains(c.Reason, "without growing") {
		t.Fatalf("same-size rewrite: got %+v", c)
	}
}

func TestDecideEditLastLineThenAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := lines("a", 500, 60)
	writeFile(t, path, body)
	w := indexFile(t, path)
	tick()
	writeAt(t, path, int64(len(body))-5, "X") // failed streaming line rewritten in place
	appendFile(t, path, lines("b", 3, 10))    // then the file grows
	c := decide(t, &w, path)
	if c.Decision != Rewrite || !strings.Contains(c.Reason, "before offset") {
		t.Fatalf("got %+v", c)
	}
}

func TestDecideHeadEditThenAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := lines("a", 2000, 60)
	writeFile(t, path, body)
	w := indexFile(t, path)
	tick()
	writeAt(t, path, 10, "Q") // compaction cleanup rewrites the head
	appendFile(t, path, lines("b", 3, 10))
	c := decide(t, &w, path)
	if c.Decision != Rewrite || !strings.Contains(c.Reason, "4KB") {
		t.Fatalf("got %+v", c)
	}
}

func TestDecideTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, lines("a", 10, 10))
	w := indexFile(t, path)
	tick()
	if err := os.Truncate(path, 20); err != nil {
		t.Fatal(err)
	}
	if c := decide(t, &w, path); c.Decision != Rewrite {
		t.Fatalf("got %+v", c)
	}
}

func TestDecideNewInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	body := lines("a", 10, 10)
	writeFile(t, path, body)
	w := indexFile(t, path)
	if w.Identity.ID.IsZero() {
		t.Skip("no file identity on this platform")
	}
	// Atomic replace with identical content plus an append: only the inode tells.
	tmp := filepath.Join(dir, "s.tmp")
	writeFile(t, tmp, body+lines("b", 1, 10))
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if c := decide(t, &w, path); c.Decision != Rewrite || !strings.Contains(c.Reason, "identity") {
		t.Fatalf("got %+v", c)
	}
}

// mutableFile is an in-memory file a test can rewrite between reads.
type mutableFile struct{ b []byte }

func (m *mutableFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func scanAll(t *testing.T, r io.ReaderAt, cur Cursor, size int64) Cursor {
	t.Helper()
	next, err := ScanJSONL(context.Background(), Input{R: r, Size: size}, cur, LineReaderOptions{}, func(*LineReader, *Line) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return next
}

// A rewrite that lands after the parse read the bytes but before the
// watermark is taken must still be detected (P2): the watermark hashes the
// bytes the parse consumed, not the file as it is afterwards.
func TestReadRecorderWatermarkSeesRewriteDuringParse(t *testing.T) {
	var lines []string
	for i := range 3000 {
		lines = append(lines, fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, strings.Repeat("p", 40)))
	}
	orig := []byte(strings.Join(lines, "\n") + "\n")
	for _, start := range []int{0, len(orig) / 2} {
		f := &mutableFile{b: bytes.Clone(orig)}
		id := Identity{ID: FileID{1, 1}, Size: int64(len(orig)), CTime: 1}
		cur := Cursor{}
		if start > 0 {
			cur = scanAll(t, f, Cursor{}, int64(start)) // an earlier pass
		}
		rec, err := NewReadRecorder(f, cur.Offset)
		if err != nil {
			t.Fatal(err)
		}
		next := scanAll(t, rec, cur, id.Size)
		if next.Offset != int64(len(orig)) {
			t.Fatalf("parsed to %d of %d", next.Offset, len(orig))
		}
		// The harness rewrites the tail in place (same size), then appends.
		rewritten := bytes.Clone(orig)
		copy(rewritten[len(rewritten)-30:], strings.Repeat("Z", 29))
		f.b = append(rewritten, []byte(`{"n":"new"}`+"\n")...)

		stale, err := NewWatermark(f, id, time.Unix(0, 1e12), next)
		if err != nil {
			t.Fatal(err)
		}
		grown := Identity{ID: id.ID, Size: int64(len(f.b)), CTime: 2}
		if c, _ := Decide(&stale, grown, f); c.Decision != Append {
			t.Fatalf("start %d: post-parse watermark decided %v, the bug this test guards against is gone?", start, c.Decision)
		}
		wm, err := rec.Watermark(id, time.Unix(0, 1e12), next)
		if err != nil {
			t.Fatal(err)
		}
		if c, _ := Decide(&wm, grown, f); c.Decision != Rewrite {
			t.Fatalf("start %d: recorded watermark decided %v (%s), want rewrite", start, c.Decision, c.Reason)
		}
		// Without a rewrite the recorded watermark equals the file's.
		f.b = orig
		clean, _ := NewWatermark(f, id, time.Unix(0, 1e12), next)
		if wm2, _ := rec.Watermark(id, time.Unix(0, 1e12), next); wm2 != clean {
			t.Fatalf("start %d: recorded watermark differs from the file's on an unchanged file", start)
		}
		// Nothing new to read: the primed window still covers the anchor.
		rec2, _ := NewReadRecorder(f, next.Offset)
		again := scanAll(t, rec2, next, id.Size)
		if wm3, _ := rec2.Watermark(id, time.Unix(0, 1e12), again); wm3 != clean {
			t.Fatalf("start %d: primed-only watermark differs", start)
		}
	}
}
