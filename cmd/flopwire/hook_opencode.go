package main

// The opencode side of `flopwire hook` (issue #62). opencode has no
// command hooks: the Flopwire plugin (plugins/opencode/flopwire.js) runs
// `flopwire hook` itself with Claude-shaped hook JSON that names the
// harness ("harness": "opencode"). Delivery differs from the other
// harnesses in one way: the plugin, not the hook, puts the text into the
// session, with client.session.promptAsync({noReply: true}), which stores a
// user message (metadata.flopwire on its part) without starting a turn.
// So the hook prints what to deliver and does not confirm it; the plugin
// confirms with a Confirm event once opencode reports the message's part
// stored (message.part.updated), not when promptAsync answers: it answers
// before opencode stores the message, and a later failure is only a
// session.error event. A message opencode never stores is never
// confirmed: it stays leased and is offered again, marked, when the lease
// ends, like a hook killed before it confirmed.
//
// Events the plugin sends, besides the hook events hookCmd knows:
//
//	Hello    what the plugin needs at load: the standing instruction and
//	         the MCP instructions for its system prompt, the tool
//	         definitions it serves, and the registry directory
//	Confirm  the delivered message ids (ids) and whether the standing
//	         instruction counts as delivered (instruction)
//
// The standing instruction is in the session's system prompt on every
// model call (experimental.chat.system.transform), so an instruction the
// bus leases with messages is confirmed with them and never printed into
// the conversation.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busrender"
)

// Plugin events.
const (
	evHello   = "Hello"
	evConfirm = "Confirm"
)

// opencodeOutput is what `flopwire hook` prints for the opencode plugin
// on a delivery event: the messages to deliver and whether the standing
// instruction was leased with them.
//
// The delivery format: each message goes into the session as its own
// text part whose text is the message's wrapper (it starts with
// `<flopwire-message id="ID"`) and whose metadata is {"flopwire": {"id":
// ID}}. The parser counts a part as delivered hook context only when the
// two ids agree (transcript/opencode), so a read receipt names only the
// message the plugin delivered there.
type opencodeOutput struct {
	Messages    []opencodeMessage `json:"messages,omitempty"`
	Instruction bool              `json:"instruction,omitempty"`
}

type opencodeMessage struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// opencodeHello is the Hello answer.
type opencodeHello struct {
	Version     string `json:"version"`
	Instruction string `json:"instruction"`
	// Tools are the MCP tools (retrieval and messaging) the plugin serves
	// as plugin tools, so each call knows its session; MCP is their
	// server instructions.
	Tools    []any  `json:"tools"`
	MCP      string `json:"mcp_instructions"`
	Registry string `json:"registry"`
}

// opencodeRegistryDir is where the plugin keeps one file per opencode
// process (<pid>.json): the sessions it holds, for presence.
func opencodeRegistryDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "opencode"), nil
}

// opencodeHook handles the plugin's own events, Hello and Confirm.
func opencodeHook(ctx context.Context, in hookInput, socket string, stdout io.Writer, warn func(string, ...any)) {
	switch in.Event {
	case evHello:
		reg, err := opencodeRegistryDir()
		if err != nil {
			warn("no config directory: %v", err)
		}
		writeJSONLine(stdout, opencodeHello{Version: version, Instruction: busrender.StandingInstruction, Tools: mcpTools(),
			MCP: mcpInstructions + "\n\n" + mcpBusInstructions, Registry: reg})
	case evConfirm:
		if in.SessionID == "" || len(in.IDs) == 0 && !in.Instruction {
			return
		}
		// The plugin confirms after opencode stored the message: it is in
		// the session whatever the time, so no hookLate check applies.
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if _, err := agent.Call(cctx, socket, agent.Request{Op: "confirm", Session: in.SessionID, IDs: in.IDs, Instruction: in.Instruction}); err != nil {
			warn("could not confirm the delivery (%s); %d messages will be offered again", hookReason(err), len(in.IDs))
		}
	}
}

// opencodeDeliver takes the session's pending messages for the plugin to
// deliver; it prints them and confirms nothing (see the file comment).
func opencodeDeliver(ctx context.Context, in hookInput, socket string, started time.Time, stdout io.Writer, warn func(string, ...any)) {
	pctx, cancel := context.WithTimeout(ctx, hookPendingBudget)
	defer cancel()
	resp, err := agent.Call(pctx, socket, agent.Request{Op: "pending", Session: in.SessionID,
		Limit: busrender.HookMessages, MaxBytes: busrender.HookBytes, HookStart: started.UnixMilli()})
	if err != nil {
		warn("%s; nothing delivered", hookReason(err))
		return
	}
	out := opencodeOutput{Instruction: resp.Instruct}
	for _, m := range resp.Messages {
		out.Messages = append(out.Messages, opencodeMessage{ID: m.ID, Text: busrender.Render(m, resp.Excerpts, busrender.HookBytes)})
	}
	if len(out.Messages) == 0 && !out.Instruction {
		return
	}
	writeJSONLine(stdout, out)
}

func writeJSONLine(w io.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "%s\n", b)
}
