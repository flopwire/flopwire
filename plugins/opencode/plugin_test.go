package opencodeplugin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The plugin's delivery after a tool call, driven by node with a fake
// opencode client and a fake flopwire binary. promptAsync answers 204
// before opencode stores the message, and a failure after that answer is
// only a session.error event, so a message is confirmed only when
// opencode reports its part stored (message.part.updated), and never on
// promptAsync's answer alone.
func TestPluginConfirmsOnlyStoredParts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the fake flopwire is a shell script")
	}
	dir := t.TempDir()
	write := func(name, s string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("flopwire.js", string(Source), 0o644)
	write("package.json", `{"type":"module"}`, 0o644)
	write("node_modules/@opencode-ai/plugin/package.json", `{"name":"@opencode-ai/plugin","type":"module","main":"index.js"}`, 0o644)
	write("node_modules/@opencode-ai/plugin/index.js", "export const tool = (t) => t\ntool.schema = {}\n", 0o644)
	// The fake flopwire: Hello, one message on PostToolUse, and every
	// Confirm appended to $CONFIRM_LOG.
	write("flopwire", `#!/bin/sh
in=$(cat)
case "$in" in
*'"Hello"'*) echo '{"instruction":"I","tools":[],"registry":""}' ;;
*'"PostToolUse"'*) echo '{"messages":[{"id":"m1","text":"<flopwire-message id=\"m1\">hi</flopwire-message>"}],"instruction":true}' ;;
*'"Confirm"'*) printf '%s\n' "$in" >> "$CONFIRM_LOG" ;;
esac
`, 0o755)
	// mode: stored (opencode stores the parts after answering), lost (it
	// answers 204 and then refuses the message), error (the call throws).
	write("driver.mjs", `import { Flopwire } from "./flopwire.js"
const mode = process.argv[2]
const sent = []
const client = { session: {
  get: async ({ path }) => ({ data: { id: path.id } }),
  promptAsync: async ({ path, body }) => {
    sent.push(body)
    if (mode === "error") throw new Error("boom")
    return { data: undefined }
  },
} }
const hooks = await Flopwire({ client })
await hooks["tool.execute.after"]({ sessionID: "ses_A", tool: "bash", callID: "c1" })
if (mode === "stored")
  for (const p of sent[0].parts) await hooks.event({ event: { type: "message.part.updated", properties: { part: { ...p, sessionID: "ses_A" } } } })
console.log(JSON.stringify(sent))
`, 0o644)

	for _, mode := range []string{"stored", "lost", "error"} {
		t.Run(mode, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "confirm.log")
			cmd := exec.Command(node, "driver.mjs", mode)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "FLOPWIRE_BIN="+filepath.Join(dir, "flopwire"), "FLOPWIRE_HOOK_ARGS=", "FLOPWIRE_SOCKET=", "CONFIRM_LOG="+log)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("node: %v\n%s", err, out)
			}
			var sent []struct {
				NoReply bool `json:"noReply"`
				Parts   []struct {
					ID       string `json:"id"`
					Text     string `json:"text"`
					Metadata struct {
						Flopwire struct {
							ID string `json:"id"`
						} `json:"flopwire"`
					} `json:"metadata"`
				} `json:"parts"`
			}
			if err := json.Unmarshal(out, &sent); err != nil {
				t.Fatalf("driver output %q: %v", out, err)
			}
			if len(sent) != 1 || !sent[0].NoReply || len(sent[0].Parts) != 1 {
				t.Fatalf("promptAsync calls: %+v", sent)
			}
			b, _ := os.ReadFile(log)
			confirms := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(b) == 0 {
				confirms = nil
			}
			if mode != "stored" {
				if len(confirms) != 0 {
					t.Fatalf("confirmed a message opencode never stored: %q", confirms)
				}
			} else if len(confirms) != 1 {
				t.Fatalf("confirms: %q", confirms)
			}
			var c struct {
				Session     string   `json:"session_id"`
				IDs         []string `json:"ids"`
				Instruction bool     `json:"instruction"`
			}
			if mode == "stored" {
				if err := json.Unmarshal([]byte(confirms[0]), &c); err != nil || c.Session != "ses_A" || strings.Join(c.IDs, ",") != "m1" || !c.Instruction {
					t.Fatalf("confirm %q: %+v %v", confirms[0], c, err)
				}
			}
			// The part opencode reports stored is the one the plugin sent:
			// it carries its own id, and the message's wrapper and id.
			if p := sent[0].Parts[0]; !strings.HasPrefix(p.ID, "prt_") || p.Metadata.Flopwire.ID != "m1" || !strings.HasPrefix(p.Text, `<flopwire-message id="m1"`) {
				t.Fatalf("part: %+v", p)
			}
		})
	}
}
