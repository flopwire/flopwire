package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func incompletePage() format.Page {
	return format.Page{Hits: []format.Hit{{Address: "session/1", SessionID: "session", MessageID: "m1", Agent: "codex", Kind: "tool_result", Snippet: "retained partial hit", Lines: []format.Line{{N: 1, Text: "retained partial hit", Match: true}}}}, Total: 1, Truncated: true, Reason: "timed out after 1s: checked 3 candidates, newest first"}
}

func incompleteServer(t *testing.T, page format.Page, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/grep" && r.URL.Path != "/v1/search" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "missing.db"))
	t.Setenv(client.EnvToken, "test-token")
	t.Setenv(client.EnvServer, srv.URL)
}

func assertIncompleteJSON(t *testing.T, raw []byte, want format.Page) {
	t.Helper()
	var got format.Page
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("page JSON: %v: %s", err, raw)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("expected a single JSON object: %v: %s", err, raw)
	}
	if !got.Truncated || got.Reason != want.Reason || len(got.Hits) != len(want.Hits) {
		t.Fatalf("partial page lost: %+v", got)
	}
	if len(got.Hits) > 0 && got.Hits[0].MessageID != want.Hits[0].MessageID {
		t.Fatalf("partial hit lost: %+v", got.Hits)
	}
}

func TestRetrievalIncompleteCLI(t *testing.T) {
	zero := format.Page{Hits: []format.Hit{}, Truncated: true, Reason: "timed out after 1s before the candidate query returned its first page (checked 0 candidates)"}
	verify := incompletePage()
	verify.Reason = "stopped after verifying 3 candidates, newest first; narrow the pattern or the filters"
	scan := incompletePage()
	scan.Reason = "unindexed: only the newest 3 messages passing the filters were checked"
	for _, verb := range []string{"grep", "search"} {
		for name, page := range map[string]format.Page{"partial": incompletePage(), "zero-checked": zero, "verification-budget": verify, "scan-budget": scan} {
			for _, asJSON := range []bool{false, true} {
				t.Run(verb+"/"+name+map[bool]string{false: "/text", true: "/json"}[asJSON], func(t *testing.T) {
					incompleteServer(t, page, http.StatusOK)
					args := []string{"--server", "needle", "--include-self"}
					if asJSON {
						args = append(args, "--json")
					}
					var out, stderr bytes.Buffer
					err := toolCmdIO(t.Context(), verb, args, &out, &stderr)
					var incomplete *incompleteRetrieval
					if !errors.As(err, &incomplete) || errors.Is(err, errReported) {
						t.Fatalf("expected incomplete status: %v", err)
					}
					warning := stderr.String()
					if !strings.Contains(warning, "results are incomplete") || !strings.Contains(warning, "no absence conclusion is justified") || !strings.Contains(warning, page.Reason) || strings.Count(warning, "flopwire: warning:") != 1 {
						t.Fatalf("warning: %q", warning)
					}
					if asJSON {
						assertIncompleteJSON(t, out.Bytes(), page)
					} else if len(page.Hits) > 0 && !strings.Contains(out.String(), "retained partial hit") {
						t.Fatalf("partial hit missing: %s", &out)
					}
				})
			}
		}
	}
}

func TestRetrievalCompleteNotIncomplete(t *testing.T) {
	for _, verb := range []string{"grep", "search"} {
		for name, page := range map[string]format.Page{
			"empty":                   {Hits: []format.Hit{}, Exact: true},
			"paginated":               {Hits: incompletePage().Hits, Total: 20, Next: 10, Exact: true},
			"rank-capped-lower-bound": {Hits: incompletePage().Hits, Total: 1, Exact: false, Notes: []string{"rank cap reached"}},
		} {
			t.Run(verb+"/"+name, func(t *testing.T) {
				incompleteServer(t, page, http.StatusOK)
				for _, options := range [][]string{{"--json"}, {"--max-bytes", "80"}} {
					var out, stderr bytes.Buffer
					args := append([]string{"--server", "needle", "--include-self"}, options...)
					if err := toolCmdIO(t.Context(), verb, args, &out, &stderr); err != nil || stderr.Len() != 0 {
						t.Fatalf("complete status: %v stderr=%s", err, &stderr)
					}
				}
			})
		}
	}
}

type incompleteBackend struct {
	backend
	page format.Page
}

func (b *incompleteBackend) Grep(context.Context, format.GrepQuery, format.Filters) (*format.Page, error) {
	p := b.page
	return &p, nil
}
func (b *incompleteBackend) Search(context.Context, format.SearchQuery, format.Filters) (*format.Page, error) {
	p := b.page
	return &p, nil
}

func TestRetrievalIncompleteMCP(t *testing.T) {
	for _, verb := range []string{"grep", "search"} {
		for _, mode := range []string{"text", "json"} {
			t.Run(verb+"/"+mode, func(t *testing.T) {
				page := incompletePage()
				r := &retriever{backend: &incompleteBackend{page: page}}
				key := map[string]string{"grep": "pattern", "search": "query"}[verb]
				request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flopwire_` + verb + `","arguments":{"` + key + `":"needle","format":"` + mode + `"}}}`
				answer, isErr, _ := mcpContent(t, mcpRoundTrip(t, r, request))
				if isErr {
					t.Fatalf("partial page became an MCP error: %s", answer)
				}
				if mode == "json" {
					assertIncompleteJSON(t, []byte(answer), page)
				} else if !strings.Contains(answer, "retained partial hit") || !strings.Contains(answer, page.Reason) {
					t.Fatalf("partial text lost: %s", answer)
				}
			})
		}
	}
}

func TestRetrievalIncompleteProcessHelper(t *testing.T) {
	if os.Getenv("FLOPWIRE_INCOMPLETE_PROCESS_HELPER") != "1" {
		return
	}
	os.Args = append([]string{"flopwire"}, strings.Split(os.Getenv("FLOPWIRE_INCOMPLETE_PROCESS_ARGS"), " ")...)
	main()
	os.Exit(0)
}

func TestRetrievalIncompleteProcessExit(t *testing.T) {
	for _, verb := range []string{"grep", "search"} {
		for _, status := range []struct {
			name             string
			page             format.Page
			httpStatus, exit int
		}{
			{"incomplete", incompletePage(), http.StatusOK, 2},
			{"empty", format.Page{Hits: []format.Hit{}, Exact: true}, http.StatusOK, 0},
			{"operational", format.Page{}, http.StatusServiceUnavailable, 1},
		} {
			t.Run(verb+"/"+status.name, func(t *testing.T) {
				incompleteServer(t, status.page, status.httpStatus)
				cmd := exec.Command(os.Args[0], "-test.run=^TestRetrievalIncompleteProcessHelper$")
				cmd.Env = append(os.Environ(), "FLOPWIRE_INCOMPLETE_PROCESS_HELPER=1", "FLOPWIRE_INCOMPLETE_PROCESS_ARGS="+verb+" --server needle --json --include-self")
				var out, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &stderr
				err := cmd.Run()
				code := 0
				if err != nil {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) {
						t.Fatal(err)
					}
					code = exitErr.ExitCode()
				}
				if code != status.exit {
					t.Fatalf("exit=%d want=%d stderr=%s", code, status.exit, &stderr)
				}
				switch status.exit {
				case 2:
					assertIncompleteJSON(t, out.Bytes(), status.page)
					if strings.Count(stderr.String(), "flopwire: warning:") != 1 || !strings.Contains(stderr.String(), status.page.Reason) {
						t.Fatalf("warning=%s", &stderr)
					}
				case 0:
					if stderr.Len() != 0 || !json.Valid(out.Bytes()) {
						t.Fatalf("stdout=%s stderr=%s", &out, &stderr)
					}
				case 1:
					var got errorJSON
					if out.Len() != 0 || json.Unmarshal(stderr.Bytes(), &got) != nil || got.Kind != "error" || got.Error.Code == "" {
						t.Fatalf("operational error changed: stdout=%s stderr=%s", &out, &stderr)
					}
				}
			})
		}
	}
}
