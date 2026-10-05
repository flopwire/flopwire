package ingest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnrichmentNULPreservesLiteralEscapes(t *testing.T) {
	for _, text := range []string{`\u0000`, `literal \u0000 followed by a quote "`, `\\u0000`, `\\\u0000`, "actual\x00nul", "\\\x00", "before\x00after\\u0000"} {
		t.Run(text, func(t *testing.T) {
			b := enrichmentJSON(map[string]any{"command": text})
			var got map[string]string
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("invalid JSON: %s: %v", b, err)
			}
			if want := strings.ReplaceAll(text, "\x00", ""); got["command"] != want {
				t.Fatalf("got %q, want %q", got["command"], want)
			}
		})
	}
}

func TestEnrichmentLiteralNULStoredInPostgres(t *testing.T) {
	e := newEnv(t)
	text := `replace("\u0000", "")`
	var got string
	if err := e.pool.QueryRow(e.ctx, `SELECT $1::jsonb->>'command'`, enrichmentJSON(map[string]any{"command": text})).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != text {
		t.Fatalf("stored %q, want %q", got, text)
	}
}

func TestEnrichmentNULNestedRawJSON(t *testing.T) {
	e := newEnv(t)
	b := enrichmentJSON(map[string]any{
		"args":    json.RawMessage(`{"command":"\\u0000","nul":"\u0000","number":9007199254740993}`),
		"key\x00": []string{"\x00", `\u0000`},
	})
	var literal, nul, number, array string
	if err := e.pool.QueryRow(e.ctx, `SELECT $1::jsonb->'args'->>'command',$1::jsonb->'args'->>'nul',$1::jsonb->'args'->>'number',$1::jsonb->'key'->>1`, b).
		Scan(&literal, &nul, &number, &array); err != nil {
		t.Fatal(err)
	}
	if literal != `\u0000` || nul != "" || number != "9007199254740993" || array != `\u0000` {
		t.Fatalf("literal=%q nul=%q number=%q array=%q", literal, nul, number, array)
	}
}

func FuzzEnrichmentNULRoundTrip(f *testing.F) {
	for _, text := range []string{`\u0000`, "\x00", "\\\x00", `quoted "\u0000"`, "invalid\xffUTF8", "\xc2\x00\x80"} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		b := enrichmentJSON(map[string]any{"value": text})
		var got map[string]string
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		// encoding/json replaces invalid UTF-8. NUL is the only additional
		// removal PostgreSQL storage requires from these string values.
		wantJSON, _ := json.Marshal(text)
		var want string
		if err := json.Unmarshal(wantJSON, &want); err != nil {
			t.Fatal(err)
		}
		want = strings.ReplaceAll(want, "\x00", "")
		if got["value"] != want {
			t.Fatalf("got %q, want %q", got["value"], want)
		}
	})
}
