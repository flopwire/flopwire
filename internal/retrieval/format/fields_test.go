package format

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/digest"
)

// parseFields splits a labeled line the way the help documents it:
// "key: value" fields two spaces apart, each value bare (no space, quote,
// backslash or control character) or a JSON string. It fails on anything
// else, so a test that passes proves the line splits unambiguously.
func parseFields(t *testing.T, line string) ([]string, map[string]string) {
	t.Helper()
	key := regexp.MustCompile(`^([a-z_]+): `)
	var keys []string
	vals := map[string]string{}
	rest := line
	for rest != "" {
		m := key.FindStringSubmatch(rest)
		if m == nil {
			t.Fatalf("no key at %q in %q", rest, line)
		}
		rest = rest[len(m[0]):]
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := 1
			for ; end < len(rest) && rest[end] != '"'; end++ {
				if rest[end] == '\\' {
					end++
				}
			}
			if end >= len(rest) {
				t.Fatalf("unterminated value in %q", line)
			}
			if err := json.Unmarshal([]byte(rest[:end+1]), &v); err != nil {
				t.Fatalf("value %s is not a JSON string: %v", rest[:end+1], err)
			}
			rest = rest[end+1:]
		} else {
			end := strings.Index(rest, "  ")
			if end < 0 {
				end = len(rest)
			}
			v, rest = rest[:end], rest[end:]
			if v == "" || strings.ContainsAny(v, " \"\\\t\n") {
				t.Fatalf("bad bare value %q in %q", v, line)
			}
		}
		if _, dup := vals[m[1]]; dup {
			t.Fatalf("key %s twice in %q", m[1], line)
		}
		keys, vals[m[1]] = append(keys, m[1]), v
		if rest != "" && !strings.HasPrefix(rest, "  ") {
			t.Fatalf("fields not two spaces apart at %q in %q", rest, line)
		}
		rest = strings.TrimPrefix(rest, "  ")
	}
	return keys, vals
}

// Headers are labeled fields that split the same way whatever the
// transcript holds: titles and intents with quotes, double spaces, text
// that looks like another field, newlines, bidi controls and Unicode;
// repo paths and names with spaces or a trailing colon; a user name with a
// space. Text fields are JSON strings and come last.
func TestLabeledHeadersSplitUnambiguously(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Hour)
	hostile := "say \"hi\"  agent: codex  repo: evil\nnext ☕ café x ‮RTL \\ end"
	c := ConversationInfo{Address: "aaaa1111", SessionID: "aaaa1111-0000-4000-8000-000000000001", Agent: "claude", User: "a b@x.test", Device: "lap top",
		Repo: "/tmp/my repo: x", Branches: []string{"feat/ünï"}, StartedAt: &start, LastActivityAt: &now, Messages: 7, ParentSession: "pppp",
		Title: hostile, Hits: 3, Digest: &digest.Digest{Intent: hostile, Commits: []string{"abc1234"}, Failed: 1}}
	wantText := Clean(oneLine(hostile))
	if !strings.Contains(wantText, `say "hi"  agent: codex  repo: evil⏎next ☕ café`) || !strings.Contains(wantText, "�RTL") {
		t.Fatalf("cleaned text %q", wantText)
	}
	check := func(name, line string, keys []string, want map[string]string) {
		t.Helper()
		if strings.ContainsAny(line, "\n\r‮") {
			t.Fatalf("%s: raw newline or bidi control: %q", name, line)
		}
		gotKeys, vals := parseFields(t, line)
		if !slices.Equal(gotKeys, keys) {
			t.Errorf("%s: keys %v, want %v\n%s", name, gotKeys, keys, line)
		}
		for k, v := range want {
			if vals[k] != v {
				t.Errorf("%s: %s = %q, want %q\n%s", name, k, vals[k], v, line)
			}
		}
	}

	h := header(&c, now)
	if !strings.HasPrefix(h, "## ") {
		t.Fatalf("header: %q", h)
	}
	check("grep header", strings.TrimPrefix(h, "## "), []string{"session", "who", "agent", "ended", "repo", "branch", "commits", "failed", "intent"},
		map[string]string{"session": "aaaa1111", "who": "a b@x.test@lap top", "repo": "my repo: x", "branch": "feat/ünï", "ended": "2026-09-29", "intent": wantText})

	var b strings.Builder
	if err := WriteSessions(&b, &Sessions{Sessions: []ConversationInfo{c}}, Style{Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	check("sessions --text", strings.SplitN(b.String(), "\n", 2)[0], []string{"session", "who", "agent", "ended", "repo", "branch", "msgs", "parent", "commits", "failed", "intent"},
		map[string]string{"msgs": "7", "parent": "pppp", "intent": wantText})

	check("grep -l", sessionLine(&c), []string{"session", "agent", "active", "repo", "branch", "hits", "title"},
		map[string]string{"active": "2026-09-29T12:00Z", "hits": "3", "title": wantText})

	b.Reset()
	if err := WriteRead(&b, &Context{Conversation: c, Focus: "m", Messages: []Message{{ID: "m", Address: "aaaa1111/1", Kind: "user", Text: "x"}}}, Style{}); err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(b.String(), "\n")
	if !strings.HasPrefix(head, "# ") {
		t.Fatalf("read header: %q", head)
	}
	check("read header", strings.TrimPrefix(head, "# "), []string{"session", "agent", "repo", "branch", "device", "user", "parent", "start", "active", "msgs", "title"},
		map[string]string{"session": c.SessionID, "repo": "/tmp/my repo: x", "device": "lap top", "user": "a b@x.test", "start": "2026-09-29T11:00Z", "active": "2026-09-29T12:00Z", "title": wantText})

	// A time a header prints is accepted back by --since.
	if got, err := ParseTime("2026-09-29T11:00Z", now); err != nil || !got.Equal(start) {
		t.Fatalf("ParseTime of a header time: %v %v", got, err)
	}
	// A value ending in a colon would read as a key: it is quoted. One
	// without a space stays bare, even with a colon inside.
	for v, want := range map[string]string{"x:": `k: "x:"`, "a:b": "k: a:b", "": `k: ""`, "it's": `k: "it's"`, "tab\there": `k: "tab\there"`} {
		if got := field("k", v); got != want {
			t.Errorf("field(%q) = %s, want %s", v, got, want)
		}
	}
	// A cwd stands in for a missing repo, under its own key.
	c.Repo, c.Cwd = "", "/home/me/scratch dir"
	if _, vals := parseFields(t, readHeader(&c)); vals["cwd"] != "/home/me/scratch dir" || vals["repo"] != "" {
		t.Fatalf("cwd: %v", vals)
	}
}
