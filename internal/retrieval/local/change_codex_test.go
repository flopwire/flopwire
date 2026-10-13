package local

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// Exercise the real parser and persisted normalized rows, rather than constructing
// commit evidence directly. All rollout records here are synthetic.
func TestChangeCodexCompletionRoundTrip(t *testing.T) {
	store, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	backend := &Backend{Store: store}
	repo := "/tmp/synthetic-change-repo"
	session := "019a0000-0000-7000-8000-000000000091"
	crossSession := "019a0000-0000-7000-8000-000000000092"
	hash := "abc1234" + strings.Repeat("0", 33)
	wrongHash := "def5678" + strings.Repeat("0", 33)
	output := "[topic abc1234] synthetic completion"
	type record map[string]any
	meta := func(id string) record {
		return record{"type": "session_meta", "payload": record{"id": id, "cwd": repo}}
	}
	call := func(id, native, command string) record {
		return record{"type": "response_item", "payload": record{"type": "custom_tool_call", "id": native, "call_id": id, "name": "functions.exec", "input": command}}
	}
	result := func(id, native, text string) record {
		return record{"type": "response_item", "payload": record{"type": "custom_tool_call_output", "id": native, "call_id": id, "output": text}}
	}
	completion := record{"type": "event_msg", "payload": record{"type": "item_completed", "turn_id": "synthetic-turn", "item": record{"type": "CommandExecution", "id": "synthetic-completion", "command": []string{"zsh", "-lc", "git commit -m synthetic"}, "cwd": repo, "exit_code": 0, "aggregated_output": output}}}
	paths := []string{}
	ingest := func(name string, records ...record) {
		t.Helper()
		var raw bytes.Buffer
		enc := json.NewEncoder(&raw)
		enc.SetEscapeHTML(false)
		for _, r := range records {
			if err := enc.Encode(r); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.Join(t.TempDir(), name+".jsonl")
		paths = append(paths, path)
		if err := os.WriteFile(path, raw.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		src := transcript.Source{Agent: transcript.AgentCodex, Path: path, Parser: codex.Name, StorageKind: transcript.StorageJSONLAppend}
		state, err := store.EnsureSource(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		var normalized transcript.Collector
		parser := &codex.Parser{}
		if _, err := parser.Parse(ctx, transcript.Input{Source: &src, R: bytes.NewReader(raw.Bytes()), Size: int64(raw.Len())}, transcript.Cursor{}, &normalized); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ApplyBatch(ctx, localindex.Batch{SourceID: state.ID, Generation: 1, NewGeneration: &transcript.Generation{Generation: 1, Size: int64(raw.Len()), Complete: true}, Conversations: normalized.Conversations, Messages: normalized.Messages, AppliedParser: codex.Name}); err != nil {
			t.Fatal(err)
		}
	}
	command := `text(await tools.exec_command({cmd:"git commit -m synthetic",workdir:"/tmp/synthetic-change-repo"}));`
	ingest("completion", meta(session), call("duplicate-call", "synthetic-call", command), result("duplicate-call", "synthetic-original-result", output), completion)
	// The exec script output has no exact provenance for its shell execution.
	// Only the retained self-contained completion supplies commit evidence.
	page, err := backend.Change(ctx, format.ChangeQuery{Commits: []string{hash}}, format.Filters{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Matches) != 1 {
		t.Fatalf("matches=%+v, want only exact self-contained completion evidence", page)
	}
	sawCompletion := false
	for _, match := range page.Matches {
		if match.ObservedHash != "abc1234" || match.Operation != "commit" || match.Session.SessionID != session {
			t.Fatalf("unexpected match %+v", match)
		}
		commandRead, err := backend.Read(ctx, format.ReadQuery{Address: match.CommandAddress}, format.Filters{})
		if err != nil {
			t.Fatal(err)
		}
		resultRead, err := backend.Read(ctx, format.ReadQuery{Address: match.ResultAddress}, format.Filters{})
		if err != nil {
			t.Fatal(err)
		}
		if len(commandRead.Messages) != 1 || len(resultRead.Messages) != 1 {
			t.Fatal("evidence address did not resolve one record")
		}
		cm, rm := commandRead.Messages[0], resultRead.Messages[0]
		if rm.Text != output || rm.Provenance.Path != paths[0] {
			t.Fatalf("result evidence %+v", rm)
		}
		if match.CommandAddress == match.ResultAddress {
			sawCompletion = true
			encodedMatch, err := json.Marshal(match)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(encodedMatch, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["command_raw"] != true {
				t.Fatalf("completion does not instruct raw command read: %s", encodedMatch)
			}
			rawCommand, err := backend.RawAt(ctx, match.CommandAddress)
			if err != nil {
				t.Fatal(err)
			}
			rawFile, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			captured := strings.SplitAfter(string(rawFile), "\n")[3]
			if string(rawCommand) != captured || !strings.Contains(string(rawCommand), "git commit -m synthetic") || !strings.Contains(string(rawCommand), output) {
				t.Fatalf("raw command does not recover exact completion record: %q", rawCommand)
			}

			if cm.Text != output || cm.Provenance.LineNo != 4 || cm.Provenance.Path != paths[0] || !match.DirectoryKnown || match.CommandDirectory != repo || match.Outcome != "succeeded" {
				t.Fatalf("completion lost exact provenance: match=%+v row=%+v", match, cm)
			}
		} else {
			t.Fatalf("exec script invented a command/result association: %+v", match)
		}
	}
	if !sawCompletion {
		t.Fatal("completion evidence missing")
	}
	// Two retained sources in one conversation reuse a tool ID. An output
	// in the second source cannot supply evidence for the first source's call.
	args, err := json.Marshal(map[string]string{"cmd": "git commit -m synthetic", "workdir": repo})
	if err != nil {
		t.Fatal(err)
	}
	orphanCall := record{"type": "response_item", "payload": record{"type": "function_call", "id": "orphan-call", "call_id": "reused-id", "name": "exec_command", "arguments": string(args)}}
	ingest("orphan-call", meta(crossSession), orphanCall)
	controlCall := record{"type": "response_item", "payload": record{"type": "function_call", "id": "control-call", "call_id": "control-id", "name": "exec_command", "arguments": string(args)}}
	ingest("orphan-result", meta(crossSession), result("reused-id", "orphan-result", "[topic def5678] unrelated source"), controlCall, result("control-id", "control-result", "[topic cafe987] correctly paired"))
	cross, err := backend.Change(ctx, format.ChangeQuery{Commits: []string{wrongHash}}, format.Filters{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(cross.Matches) != 0 {
		t.Fatalf("cross-source tool ID invented provenance: %+v", cross)
	}
	control, err := backend.Change(ctx, format.ChangeQuery{Commits: []string{"cafe987" + strings.Repeat("0", 33)}}, format.Filters{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(control.Matches) != 1 || control.Matches[0].CommandAddress == control.Matches[0].ResultAddress {
		t.Fatalf("same-source direct call/result control failed: %+v", control)
	}
	// A quiet commit can reveal its full ID in the same completed command.
	quietHash := strings.Repeat("e", 40)
	quietCommand := "git commit -qm quiet && git rev-parse HEAD"
	quiet := record{"type": "event_msg", "payload": record{"type": "item_completed", "turn_id": "quiet-turn", "item": record{"type": "CommandExecution", "id": "quiet-completion", "command": quietCommand, "cwd": repo, "exit_code": 0, "aggregated_output": quietHash}}}
	ingest("quiet", meta("019a0000-0000-7000-8000-000000000093"), quiet)
	quietPage, err := backend.Change(ctx, format.ChangeQuery{Commits: []string{quietHash}}, format.Filters{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(quietPage.Matches) != 1 {
		t.Fatalf("quiet self-reveal missing: %+v", quietPage)
	}
	qm := quietPage.Matches[0]
	if qm.ObservedHash != quietHash || qm.Match != "exact" || qm.CommandAddress != qm.ResultAddress || !qm.CommandRaw {
		t.Fatalf("quiet evidence %+v", qm)
	}
	quietRaw, err := backend.RawAt(ctx, qm.CommandAddress)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(quietRaw), quietCommand) || !strings.Contains(string(quietRaw), quietHash) {
		t.Fatalf("quiet evidence raw %q", quietRaw)
	}

}
