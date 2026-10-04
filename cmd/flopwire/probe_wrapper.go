package main

// What reached the model: the probe reads the recipient's transcript as the
// harness stored it, and finds the message's wrapper in the rows the
// harness records as hook context (transcript.HookContext): Claude Code's
// hook_additional_context attachment, Codex's hooks.additional_context
// developer message, Devin's system node that is the hook's output, and
// the opencode part the Flopwire plugin delivered. A verdict that rests on
// this does not depend on a cheap model quoting the message back: the
// model's quote is corroboration only (issue #131).

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/flopwire/flopwire/internal/transcript/opencode"
)

const (
	wrapperOpen  = `<flopwire-message id="`
	wrapperClose = "</flopwire-message>"
)

// wrapper is one message's wrapper as the transcript holds it.
type wrapper struct {
	attrs  map[string]string // the opening tag's attributes, unescaped
	body   string            // the text between the opening and closing tags
	closed bool              // the closing tag follows
	// cut is the harness's mark that the model did not get the hook
	// context whole, when its row holds one: Claude Code's
	// <persisted-output> stub (past 10,000 characters the model sees a
	// 2 KB preview) or Codex's truncation (past 10,000 bytes it keeps the
	// head and the tail). See notes/message-bus/hook-caps-2026-10-03.md.
	cut string
	// answered is whether the model replied after the row: a row after
	// the turn's last model call was recorded but never read.
	answered bool
}

// delivery is what the recipient's transcript shows of one message.
type delivery struct {
	where string   // the transcript read, for evidence
	err   error    // the transcript could not be read
	w     *wrapper // the message's first wrapper in hook context; nil when none
	n     int      // how many wrappers of the message hook context holds
}

// hookText is one hook context row of the transcript.
type hookText struct {
	text     string
	answered bool // an assistant row of the session follows it
}

// cutMarks are what a harness puts in a hook context it did not give the
// model whole.
var cutMarks = []string{"<persisted-output>", "Warning: truncated output", "tokens truncated…"}

// whole checks that the model got the wrapper whole and answered it: closed,
// in a row the harness did not cut, and followed by a reply.
func (w *wrapper) whole(id string) error {
	switch {
	case w.cut != "":
		return fmt.Errorf("the hook context holding %s's wrapper was cut by the harness (%s): the model did not get it whole", id, w.cut)
	case !w.closed:
		return fmt.Errorf("the wrapper of %s is not closed", id)
	case !w.answered:
		return fmt.Errorf("no model reply follows the hook context holding %s: it came after the turn's last model call", id)
	}
	return nil
}

var wrapperAttr = regexp.MustCompile(`\s([a-z][a-z-]*)="([^"]*)"`)

// findWrapper finds message id's first wrapper in hook context rows, and
// counts them: a line that starts with its opening tag, as `flopwire hook`
// prints it (busrender.Render). A wrapper's attributes are escaped, so the
// first ">" ends the opening tag.
func findWrapper(rows []hookText, id string) (first *wrapper, n int) {
	open := wrapperOpen + id + `"`
	for _, row := range rows {
		text := row.text
		for rest, at := text, 0; ; {
			i := strings.Index(rest, open)
			if i < 0 {
				break
			}
			start := at + i
			rest, at = rest[i+len(open):], start+len(open)
			if start > 0 && text[start-1] != '\n' {
				continue
			}
			end := strings.IndexByte(text[start:], '>')
			if end < 0 {
				continue
			}
			w := &wrapper{attrs: map[string]string{}, answered: row.answered}
			for _, c := range cutMarks {
				if strings.Contains(text, c) {
					w.cut = c
					break
				}
			}
			for _, m := range wrapperAttr.FindAllStringSubmatch(text[start:start+end], -1) {
				w.attrs[m[1]] = html.UnescapeString(m[2])
			}
			body := text[start+end+1:]
			if j := strings.Index(body, wrapperClose); j >= 0 {
				body, w.closed = body[:j], true
			}
			w.body = body
			if n++; first == nil {
				first = w
			}
		}
	}
	return first, n
}

// hookContexts returns the text of every row of session's transcript at
// path that the harness records as hook context, parsed by the harness's
// own parser. path is a JSONL transcript (Claude Code, Codex) or a store
// (Devin, opencode).
func hookContexts(ctx context.Context, h transcript.Agent, path, session string) ([]hookText, error) {
	var p transcript.Parser
	switch h {
	case transcript.AgentClaude:
		p = &claude.Parser{}
	case transcript.AgentCodex:
		p = &codex.Parser{}
	case transcript.AgentDevin:
		p = &devin.Parser{}
	case transcript.AgentOpencode:
		p = &opencode.Parser{}
	default:
		return nil, fmt.Errorf("no transcript parser for %s", h)
	}
	in := transcript.Input{Source: &transcript.Source{Agent: h, Path: path, StorageKind: p.StorageKind(), Parser: p.Name()}}
	if p.StorageKind() == transcript.StorageJSONLAppend {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return nil, err
		}
		in.R, in.Size = f, st.Size()
	} else if _, err := os.Stat(path); err != nil {
		return nil, err // the SQLite parsers would create an empty store
	}
	var c transcript.Collector
	if _, err := p.Parse(ctx, in, transcript.Cursor{}, &c); err != nil {
		return nil, err
	}
	var out []hookText
	for _, m := range c.MessagesFor(session) {
		if m.Kind == transcript.KindAssistant {
			for i := range out {
				out[i].answered = true
			}
			continue
		}
		if !transcript.HookContext(h, m) {
			continue
		}
		// Devin stores its own system prompt parts as system rows too: only
		// one that is the hook's output counts (internal/agent/reads.go).
		if h == transcript.AgentDevin && !strings.HasPrefix(m.Text, wrapperOpen) && !strings.HasPrefix(m.Text, "<flopwire-instructions>") {
			continue
		}
		out = append(out, hookText{text: m.Text})
	}
	return out, nil
}

// transcriptPath is where the session's transcript is: the path its hooks
// were given (Claude Code, Codex), or the harness's store (Devin,
// opencode).
func (r *harnessRun) transcriptPath(entries []tapEntry, session string) string {
	switch r.name {
	case transcript.AgentDevin:
		return filepath.Join(r.home, ".local", "share", "devin", "cli", "sessions.db")
	case transcript.AgentOpencode:
		return r.p.opencodeDB()
	}
	for _, e := range entries {
		if e.Session == session && e.AgentID == "" && e.Transcript != "" {
			return e.Transcript
		}
	}
	return ""
}

// delivered reads the session's transcript for message id's wrapper. The
// harness may write the transcript a moment after the turn ends, so it
// reads again for a while before it reports no wrapper.
func (r *harnessRun) delivered(ctx context.Context, session, id string) delivery {
	deadline := time.Now().Add(20 * time.Second)
	for {
		all, _ := readTap(r.tap)
		d := delivery{where: r.transcriptPath(all, session)}
		if d.where == "" {
			d.err = fmt.Errorf("no hook named the session's transcript")
		} else {
			rows, err := hookContexts(ctx, r.name, d.where, session)
			d.err = err
			d.w, d.n = findWrapper(rows, id)
		}
		if (d.w != nil && d.w.answered) || time.Now().After(deadline) || ctx.Err() != nil {
			return d
		}
		select {
		case <-ctx.Done():
			return d
		case <-time.After(500 * time.Millisecond):
		}
	}
}
