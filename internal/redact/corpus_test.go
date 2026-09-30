package redact

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// FLOPWIRE_CORPUS=1 go test -run Corpus -v ./internal/redact
//
// Runs the redactor over the real Claude and Codex transcript corpus
// (read-only) and reports matches per rule, throughput, and a
// false-positive review of what the matches look like. It never prints a
// matched value: only rule names, the key name of an assignment, and the
// value's shape (length, character classes, entropy).

func corpusFiles(t *testing.T) []string {
	if os.Getenv("FLOPWIRE_CORPUS") != "1" {
		t.Skip("set FLOPWIRE_CORPUS=1 to run against the real corpus")
	}
	home, _ := os.UserHomeDir()
	var files []string
	for _, root := range []string{
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".codex", "sessions"),
		filepath.Join(home, ".codex", "archived_sessions"),
	} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				files = append(files, p)
			}
			return nil
		})
	}
	return files
}

func corpusMode(p string) Mode {
	if strings.HasSuffix(p, ".jsonl") {
		return Lines
	}
	return ModeFor("companion", p)
}

func TestCorpusRedaction(t *testing.T) {
	files := corpusFiles(t)
	counts := map[string]int64{}
	var bytesIn, withHits int64
	files = sampleEvery(files, os.Getenv("FLOPWIRE_CORPUS_SAMPLE"))
	start := time.Now()
	buf := make([]byte, 4<<20)
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		mode := corpusMode(p)
		x := NewReaderAt(f, mode)
		x.CountFrom(0)
		for off := int64(0); ; {
			n, err := x.ReadAt(buf, off)
			off += int64(n)
			bytesIn += int64(n)
			if err != nil {
				if err != io.EOF {
					t.Logf("read error in a file: %v", err)
				}
				break
			}
		}
		f.Close()
		c := x.Counts()
		if len(c) > 0 {
			withHits++
		}
		for k, v := range c {
			counts[k] += v
		}
	}
	el := time.Since(start)
	t.Logf("files %d, bytes %.2f GB, %s, %.0f MB/s (one core); files with a match %d",
		len(files), float64(bytesIn)/1e9, el.Round(time.Second), float64(bytesIn)/1e6/el.Seconds(), withHits)
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return int(counts[b] - counts[a]) })
	for _, k := range keys {
		t.Logf("  %-24s %d", k, counts[k])
	}
}

func sampleEvery(files []string, every string) []string {
	var n int
	if _, err := fmt.Sscan(every, &n); err != nil || n <= 1 {
		return files
	}
	var out []string
	for i := 0; i < len(files); i += n {
		out = append(out, files[i])
	}
	return out
}

var keyNameRe = regexp.MustCompile(`[A-Za-z0-9_.\-]{1,64}$`)

// TestCorpusFalsePositiveReview groups the matches of a sample of the
// corpus by rule, key name (assignments) and value shape, so the kinds of
// things each rule catches can be reviewed without seeing any value.
func TestCorpusFalsePositiveReview(t *testing.T) {
	files := sampleEvery(corpusFiles(t), envOr("FLOPWIRE_CORPUS_SAMPLE", "10"))
	groups := map[string]int{}
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil || corpusMode(p) == Off {
			continue
		}
		segs := [][]byte{data}
		if corpusMode(p) == Lines {
			segs = splitLines(data)
		}
		for _, seg := range segs {
			for _, m := range Find(seg) {
				groups[describe(seg, m)]++
			}
		}
	}
	type kv struct {
		k string
		n int
	}
	var all []kv
	for k, n := range groups {
		all = append(all, kv{k, n})
	}
	slices.SortFunc(all, func(a, b kv) int { return b.n - a.n })
	t.Logf("sampled %d files; %d distinct (rule, key, shape) groups", len(files), len(all))
	for i, g := range all {
		if i == 150 {
			break
		}
		t.Logf("  %6d  %s", g.n, g.k)
	}
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := slices.Index(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

// describe names a match without its value.
func describe(seg []byte, m Match) string {
	v := seg[m.Start:m.End]
	key := ""
	switch m.Rule {
	case assignRule:
		pre := seg[max(0, m.Start-80):m.Start]
		pre = []byte(strings.TrimRight(string(pre), `"'\: =`+"`"))
		key = keyNameRe.FindString(string(pre))
	case "url-password":
		pre := string(seg[max(0, m.Start-80):m.Start])
		if i := strings.LastIndex(pre, "://"); i >= 0 {
			j := i - 1
			for j >= 0 && (isAlnum(pre[j]) || pre[j] == '+' || pre[j] == '.' || pre[j] == '-') {
				j--
			}
			key = pre[j+1:i] + "://"
		}
	}
	return fmt.Sprintf("%-18s key=%-28s %s", m.Rule, key, shape(v))
}

func shape(v []byte) string {
	var lower, upper, digit, hexOnly, sym = 0, 0, 0, true, map[byte]bool{}
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z':
			lower++
			if c > 'f' {
				hexOnly = false
			}
		case c >= 'A' && c <= 'Z':
			upper++
			hexOnly = false
		case c >= '0' && c <= '9':
			digit++
		default:
			sym[c] = true
			hexOnly = false
		}
	}
	class := "mixed"
	switch {
	case digit == len(v):
		class = "digits"
	case hexOnly:
		class = "hex"
	case len(sym) == 0:
		class = "alnum"
	}
	var syms []string
	for c := range sym {
		syms = append(syms, string(c))
	}
	slices.Sort(syms)
	lb := len(v)
	switch {
	case lb < 16:
		lb = lb / 4 * 4
	case lb < 64:
		lb = lb / 8 * 8
	default:
		lb = lb / 64 * 64
	}
	return fmt.Sprintf("len~%d %s syms=%q H~%.1f", lb, class, strings.Join(syms, ""), float64(int(entropy(v)*2))/2)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
