package redact

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/redact/redacttest"
)

func TestPlantedSecretsAreMasked(t *testing.T) {
	needles := redacttest.Needles()
	for _, p := range redacttest.PlantedSecrets() {
		for _, wrap := range []string{"%s", "x %s y", `{"text":"see %s now"}`, "\\n%s\\n", "'%s'", "`%s`"} {
			in := strings.Replace(wrap, "%s", p.Value, 1)
			out, ms := Redact([]byte(in))
			if len(out) != len(in) {
				t.Fatalf("%s: length changed %d -> %d", p.Name, len(in), len(out))
			}
			if bytes.Contains(out, []byte(needles[p.Name])) {
				t.Errorf("%s in %q: secret survived", p.Name, wrap)
				continue
			}
			found := false
			for _, m := range ms {
				found = found || m.Rule == p.Rule
			}
			if !found {
				t.Errorf("%s in %q: want rule %s, got %v", p.Name, wrap, p.Rule, ms)
			}
		}
	}
}

func TestMarkers(t *testing.T) {
	tok := "gh" + "p_" + strings.Repeat("aB3dE5", 6)
	out, _ := Redact([]byte("x " + tok + " y"))
	got := string(out[2 : 2+len(tok)])
	if !strings.HasPrefix(got, "[REDACTED:github-token:") || len(got) != len(tok) {
		t.Fatalf("marker %q", got)
	}
	// Same token, same marker; the marker itself is not re-redacted.
	again, ms := Redact(out)
	if len(ms) != 0 || !bytes.Equal(again, out) {
		t.Fatalf("redacting a marker changed it: %v", ms)
	}
	out2, _ := Redact([]byte(tok))
	if string(out2) != got {
		t.Fatalf("markers differ: %q vs %q", out2, got)
	}
	// Password-class rules carry no hash.
	out3, _ := Redact([]byte("DB_PASSWORD=Xk29!pq_77aa-Lm3Q8zR4t"))
	if !strings.Contains(string(out3), "[REDACTED:assignment]") || strings.Contains(string(out3), "Xk29") {
		t.Fatalf("assignment marker %q", out3)
	}
	// Too short for any marker: stars.
	dst := make([]byte, 5)
	marker(dst, []byte("abcde"), "url-password", false)
	if string(dst) != "*****" {
		t.Fatalf("short marker %q", dst)
	}
}

func TestAssignments(t *testing.T) {
	cases := []struct {
		in   string
		mask bool
	}{
		{`GITHUB_TOKEN=Zq81mPx0Lw2e`, true},
		{`export API_KEY="k8Hq2-Lx09-Pp77"`, true},
		{`{"client_secret": "Q9w8E7r6T5y4U3i2"}`, true},
		{`{\"password\":\"Hunter2Mouse!9\"}`, true},
		{`--api-key=Ab12Cd34Ef56Gh78`, true},
		{`STRIPE_SIGNING_KEY=Ab12Cd34Ef56Gh78`, true},
		{`"input_tokens":123456789`, false},
		{`max_tokens: 4096`, false},
		{`token = self._get_token()`, false},
		{`password = os.environ["DB_PASSWORD"]`, false},
		{`token: ${{ secrets.GITHUB_TOKEN }}`, false},
		{`TOKEN=$(gh auth token)`, false},
		{`password: string`, false},
		{`token: CancellationToken`, false},
		{`"tokenizer": "cl100k_base"`, false},
		{`password_hash = "abc123def456ghi"`, false},
		{`cache_key: "user1234profile"`, false},
		{`SSH_PUBLIC_KEY=ssh-ed25519AAAAC3Nza`, false},
		{`PWD=/Users/someone/code`, false},
		{`secret: <your-secret-here>`, false},
		{`if token == "abc123xyz789":`, false},
	}
	for _, c := range cases {
		out, ms := Redact([]byte(c.in))
		if got := len(ms) > 0; got != c.mask {
			t.Errorf("%q: masked=%v want %v (%q)", c.in, got, c.mask, out)
		}
	}
}

func TestFalsePositiveGuards(t *testing.T) {
	for _, in := range []string{
		"AK" + "IAIOSFODNN7EXAMPLE",                                     // AWS documentation key
		"-----BEGIN PRIVATE KEY-----\\n...\\n-----END PRIVATE KEY-----", // doc example
		"https://example.com:8443/path",
		"git@github.com:org/repo.git",
		"postgres://app:Dev9Pass-x@localhost:5432/app",
		"postgres://postgres:postgres@db.internal:5432/app",
		"sha256:" + strings.Repeat("ab12", 16),
		"commit 3f2a9c1e0b7d4a6f8e2c1b0a9d8e7f6a5b4c3d2e",
	} {
		if _, ms := Redact([]byte(in)); len(ms) != 0 {
			t.Errorf("%q: unexpected %v", in, ms)
		}
	}
}

// A redacted JSON line still parses, even when a match edge falls on a
// JSON escape.
func TestJSONStaysValid(t *testing.T) {
	tok := "gh" + "p_" + strings.Repeat("aB3dE5", 6)
	for _, s := range []string{
		"line\n" + tok + "\nnext",
		"PASSWORD=\"Q9w8E7r6T5y4U3i2\"",
		"quote\"" + tok + "\\",
		redacttest.Fill("{{PEM}}", 0),
		"Authorization: Bearer " + redacttest.Needles()["OPENAI"][8:],
	} {
		line, _ := json.Marshal(map[string]string{"text": s})
		out, ms := Redact(line)
		if len(ms) == 0 {
			t.Errorf("%q: nothing masked", line)
		}
		var v map[string]string
		if err := json.Unmarshal(out, &v); err != nil {
			t.Errorf("%q -> %q: %v", line, out, err)
		}
	}
	// Escape widening: a match starting right after "\u" is widened.
	b := []byte("x\\u0041y")
	if s, e := widenEscapes(b, 3, 5); s != 1 || e != 7 {
		t.Fatalf("widen start: %d,%d", s, e)
	}
	if s, e := widenEscapes(b, 0, 3); s != 0 || e != 7 {
		t.Fatalf("widen end: %d,%d", s, e)
	}
}

// ReaderAt serves the same bytes as redacting each line whole, whatever
// offsets and lengths the reads use, including lines longer than SegMax
// with a secret across a cut.
func TestReaderAtMatchesWholeLines(t *testing.T) {
	var src bytes.Buffer
	secrets := redacttest.PlantedSecrets()
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 400 {
		p := secrets[i%len(secrets)]
		fmtLine, _ := json.Marshal(map[string]string{"text": strings.Repeat("filler ", rng.IntN(40)) + p.Value + " tail"})
		src.Write(fmtLine)
		src.WriteByte('\n')
	}
	// A long line with a token straddling the SegMax cut.
	tok := "gh" + "p_" + strings.Repeat("Zy9Xw8", 6)
	long := bytes.Repeat([]byte("a "), SegMax/2)
	long = append(long[:SegMax-10], append([]byte(" "+tok+" "), bytes.Repeat([]byte("b "), 1000)...)...)
	lineStart := src.Len()
	src.Write(long)
	src.WriteString("\nlast line without newline " + secrets[1].Value)
	raw := src.Bytes()

	var want []byte
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		out, _ := Redact(line)
		want = append(want, out...)
	}
	// Whole-line Redact of the long line is the reference only if the token
	// is caught; ReaderAt must mask it too.
	if bytes.Contains(want, []byte(tok)) {
		t.Fatal("reference missed the long-line token")
	}

	x := NewReaderAt(bytes.NewReader(raw), Lines)
	got := make([]byte, len(raw))
	for off := 0; off < len(raw); {
		n := 1 + rng.IntN(300000)
		if off+n > len(raw) {
			n = len(raw) - off
		}
		m, err := x.ReadAt(got[off:off+n], int64(off))
		if err != nil && err != io.EOF || m != n {
			t.Fatalf("ReadAt(%d,%d) = %d, %v", off, n, m, err)
		}
		off += n
	}
	if !bytes.Equal(got, want) {
		i := 0
		for i < len(got) && got[i] == want[i] {
			i++
		}
		t.Fatalf("differs at %d (long line starts at %d)", i, lineStart)
	}
	// Random-access reads agree too.
	y := NewReaderAt(bytes.NewReader(raw), Lines)
	for range 200 {
		off := rng.IntN(len(raw))
		n := min(1+rng.IntN(5000), len(raw)-off)
		buf := make([]byte, n)
		if _, err := y.ReadAt(buf, int64(off)); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, want[off:off+n]) {
			t.Fatalf("random read at %d differs", off)
		}
	}
	// Reading past the end reports EOF.
	if n, err := y.ReadAt(make([]byte, 10), int64(len(raw)-3)); n != 3 || err != io.EOF {
		t.Fatalf("tail read = %d, %v", n, err)
	}
}

// Whole mode catches a PEM block across real newlines; Lines mode is
// per line; Off passes through.
func TestModes(t *testing.T) {
	body := strings.Repeat("MIIEowIBAAKCAQEA7x9", 4)
	pem := "-----BEGIN PRIVATE KEY-----\n" + body + "\n" + body + "\n-----END PRIVATE KEY-----\n"
	read := func(m Mode) string {
		out, _ := io.ReadAll(io.NewSectionReader(NewReaderAt(strings.NewReader(pem), m), 0, int64(len(pem))))
		return string(out)
	}
	if strings.Contains(read(Whole), body) {
		t.Error("Whole missed a multi-line PEM block")
	}
	if read(Off) != pem {
		t.Error("Off changed bytes")
	}
	if ModeFor("jsonl_append", "/x/a.jsonl") != Lines || ModeFor("companion", "/x/tool-results/a.png") != Off ||
		ModeFor("companion", "/x/tool-results/a.txt") != Whole || ModeFor("sqlite", "/x/sessions.db#s1") != Lines {
		t.Error("ModeFor")
	}
}

func TestCounts(t *testing.T) {
	line := "TOKEN=Zq81mPx0Lw2eQ7\n"
	src := strings.Repeat(line, 10)
	x := NewReaderAt(strings.NewReader(src), Lines)
	x.CountFrom(int64(5 * len(line)))
	buf := make([]byte, len(src))
	for range 3 { // re-reads do not double count
		x.ReadAt(buf, 0)
	}
	if got := x.Counts()[assignRule]; got != 5 {
		t.Fatalf("counted %d, want 5", got)
	}
}

// Redacting redacted bytes changes nothing: the server's pass over a
// redacting device's upload must count zero.
func TestIdempotentOnFixtures(t *testing.T) {
	for _, f := range []string{"claude.jsonl", "codex.jsonl", "devin-export.jsonl", "tool-result.txt"} {
		raw, err := os.ReadFile("../../testdata/redaction/" + f)
		if err != nil {
			t.Fatal(err)
		}
		depth := 1
		if strings.HasSuffix(f, ".txt") {
			depth = 0
		}
		once, ms := Redact([]byte(redacttest.Fill(string(raw), depth)))
		if len(ms) == 0 {
			t.Fatalf("%s: nothing masked", f)
		}
		if _, again := Redact(once); len(again) != 0 {
			t.Errorf("%s: %d matches in redacted output, first %s", f, len(again), again[0].Rule)
		}
	}
	// Every rule's marker, in every context a key could precede it.
	for _, r := range append(rules, rule{id: assignRule}) {
		for _, ctx := range []string{"x %s y", "token %s", "TOKEN=%s", `"api_key": "%s"`, "Bearer %s", "cookie: a=%s"} {
			m := strings.Replace(ctx, "%s", "[REDACTED:"+r.id+":1a2b3c4d]*****", 1)
			if _, ms := Redact([]byte(m)); len(ms) != 0 {
				t.Errorf("marker of %s re-matched as %s in %q", r.id, ms[0].Rule, m)
			}
		}
	}
}

// Flopwire's own credentials are recognized when they land in a transcript.
func TestFlopwireTokensAreMasked(t *testing.T) {
	tok, _, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, ms := Redact([]byte("login with " + tok + " now")); len(ms) != 1 || ms[0].Rule != "flopwire-token" {
		t.Fatalf("token not caught: %v", ms)
	}
}

// A quoted value that holds an escaped quote or backslash, a JSON
// \uXXXX escape, or a shell separator is masked whole, at every JSON
// nesting depth, and every level still parses.
func TestQuotedAssignmentWithEscapes(t *testing.T) {
	for _, pw := range []string{`Xk9#mQ2$"vL8p\Zq7w`, `Tr0ub4dor&3xyzQ|`, `pa5\5word;Qz81vL`} {
		inner, _ := json.Marshal(map[string]string{"password": pw})
		outer, _ := json.Marshal(map[string]string{"arguments": string(inner)})
		outer2, _ := json.Marshal(map[string]string{"x": string(outer)})
		for depth, in := range []string{string(inner), string(outer), string(outer2), "DB_PASSWORD='" + pw + "'"} {
			out, _ := Redact([]byte(in))
			for _, part := range []string{"vL8p", "Zq7w", "3xyzQ", "Qz81"} {
				if strings.Contains(pw, part) && strings.Contains(string(out), part) {
					t.Errorf("depth %d: %q survives in %s", depth, part, out)
				}
			}
			if depth == 3 {
				continue
			}
			v := out
			for d := depth; ; d-- {
				var m map[string]string
				if err := json.Unmarshal(v, &m); err != nil {
					t.Fatalf("depth %d level %d: %v: %s", depth, d, err, v)
				}
				if d == 0 {
					break
				}
				for _, s := range m {
					v = []byte(s)
				}
			}
		}
	}
}

type countingReaderAt struct {
	r *bytes.Reader
	n int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

// A read deep inside a long line walks back to the line start reading
// each byte about once, not a 2*SegMax block per 64KB step (on the
// server every block is fetched from the archive).
func TestReaderAtLineStartReadsLinearly(t *testing.T) {
	raw := append([]byte("first\n"), bytes.Repeat([]byte("a "), 3*SegMax)...)
	c := &countingReaderAt{r: bytes.NewReader(raw)}
	buf := make([]byte, 100)
	if _, err := NewReaderAt(c, Lines).ReadAt(buf, int64(len(raw)-200)); err != nil {
		t.Fatal(err)
	}
	if c.n > 4*int64(len(raw)) {
		t.Fatalf("read %d underlying bytes for one 100-byte read of a %d-byte file", c.n, len(raw))
	}
}

func TestReaderAtPastEOF(t *testing.T) {
	x := NewReaderAt(strings.NewReader("a\nbb"), Lines)
	if n, err := x.ReadAt(make([]byte, 4), 10); n != 0 || err != io.EOF {
		t.Fatalf("read past EOF = %d, %v", n, err)
	}
}

// Password-class markers never carry a hash, however random the value
// looks: a human-chosen passphrase plus a 32-bit unsalted hash confirms a
// dictionary guess (notes/redaction.md Q2).
func TestAssignmentMarkerHasNoHash(t *testing.T) {
	out, ms := Redact([]byte(`DB_PASSWORD="Winter2024!Jackson#Blue.Mustang7"`))
	if len(ms) != 1 || ms[0].Rule != "assignment" {
		t.Fatalf("matches %v", ms)
	}
	if !strings.Contains(string(out), "[REDACTED:assignment]*") {
		t.Fatalf("marker %s", out)
	}
}
