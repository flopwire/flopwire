package synthcorpus

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Line-size model (reference: p50 732B, p99 13KB, max 1.2MB, 77% of bytes
// in lines over 100KB). Conversation lines draw a content size from a
// lognormal; a few tool results are huge (base64 images, whole command
// output), log-uniform between 100KB and 1.2MB.
const (
	contentMedian = 640
	contentSigma  = 1.1
	hugeChance    = 0.042 // of tool results
	hugeMin       = 200 << 10
	hugeMax       = 1200 << 10
	persistChance = 0.04 // of other tool results: written to tool-results/
	nonConvChance = 0.75 // a metadata line after a conversation line
)

type writer struct {
	r        rng
	f        *fileSpec
	root     string
	bw       *bufio.Writer
	n        int64
	t        time.Time
	res      fileResult
	plants   []plant
	parent   string // Claude parentUuid of the next line
	ord      int64  // Codex ordinal
	nPlanted int
	buf      []byte
}

func writeFile(seed uint64, f *fileSpec, root string) (fileResult, error) {
	w := &writer{r: newRNG(seed, uint64(f.idx)+16), f: f, root: root, t: f.start, plants: f.plants,
		res: fileResult{planted: map[string]int{}}}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return w.res, err
	}
	out, err := os.Create(f.path)
	if err != nil {
		return w.res, err
	}
	w.bw = bufio.NewWriterSize(out, 1<<20)
	if f.agent == agentCodex {
		err = w.codex()
	} else {
		err = w.claude()
	}
	if err == nil {
		err = w.bw.Flush()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return w.res, err
	}
	w.res.bytes = w.n
	// The file's mtime is its last line, as for a real session.
	return w.res, os.Chtimes(f.path, w.t, w.t)
}

func (w *writer) emit(line []byte) error {
	line = append(line, '\n')
	if len(line) > 100<<10 {
		w.res.hugeBytes += int64(len(line))
	}
	w.res.lineSizes = append(w.res.lineSizes, len(line))
	w.n += int64(len(line))
	_, err := w.bw.Write(line)
	return err
}

func (w *writer) tick() {
	w.t = w.t.Add(time.Duration(float64(w.f.gap) * (0.2 + 1.6*w.r.Float64())))
}

func (w *writer) done() bool { return w.n >= w.f.size && len(w.plants) == 0 }

// due returns the next plant if it is due at this point of the file and
// fits the slot (tool output or prose), and consumes it.
func (w *writer) due(tool bool) (string, bool) {
	if len(w.plants) == 0 {
		return "", false
	}
	p := w.plants[0]
	if p.tool != tool || float64(w.n) < p.at*float64(w.f.size) {
		return "", false
	}
	w.plants = w.plants[1:]
	w.res.planted[p.needle]++
	w.nPlanted++
	return p.text, true
}

// huge draws the size of a huge tool result, if this one is huge and fits
// in what is left of the file.
func (w *writer) huge() (int, bool) {
	if w.r.Float64() >= hugeChance {
		return 0, false
	}
	n := int(w.r.logUniform(hugeMin, hugeMax))
	if w.n+int64(n) > w.f.size {
		return 0, false
	}
	return n, true
}

func (w *writer) contentSize() int {
	return int(min(48<<10, max(8, w.r.lognormal(contentMedian, contentSigma))))
}

// prose appends JSON-safe prose of about n bytes, led by a due plant.
func (w *writer) proseSlot(dst []byte, n int) []byte {
	if text, ok := w.due(false); ok {
		dst = append(dst, text...)
		dst = append(dst, ' ')
	}
	return w.r.prose(dst, n)
}

// --- Claude Code ---

func (w *writer) claudeStart(typ string) []byte {
	f := w.f
	w.tick()
	b := w.buf[:0]
	b = append(b, `{"parentUuid":`...)
	if w.parent == "" {
		b = append(b, "null"...)
	} else {
		b = appendJSON(b, w.parent)
	}
	b = append(b, `,"isSidechain":`...)
	b = strconv.AppendBool(b, f.isSub())
	b = append(b, `,"userType":"external","cwd":`...)
	b = appendJSON(b, f.cwd)
	b = append(b, `,"sessionId":"`...)
	b = append(b, f.id...)
	b = append(b, `","version":"2.1.0","gitBranch":"main",`...)
	if f.isSub() {
		b = append(b, `"agentId":"`...)
		b = append(b, f.agentID...)
		b = append(b, `",`...)
	}
	b = append(b, `"type":"`...)
	b = append(b, typ...)
	b = append(b, `",`...)
	return b
}

func (w *writer) claudeEnd(b []byte) error {
	u := w.r.uuid4()
	b = append(b, `,"uuid":"`...)
	b = append(b, u...)
	b = append(b, `","timestamp":`...)
	b = appendTS(b, w.t)
	b = append(b, '}')
	w.parent = u
	w.buf = b
	return w.emit(b)
}

func (w *writer) usage(b []byte) []byte {
	b = append(b, `"usage":{"input_tokens":`...)
	b = strconv.AppendInt(b, int64(w.r.IntN(5000)), 10)
	b = append(b, `,"output_tokens":`...)
	b = strconv.AppendInt(b, int64(w.r.IntN(2000)), 10)
	b = append(b, `,"cache_read_input_tokens":`...)
	b = strconv.AppendInt(b, int64(w.r.IntN(200000)), 10)
	b = append(b, `,"cache_creation_input_tokens":`...)
	b = strconv.AppendInt(b, int64(w.r.IntN(5000)), 10)
	return append(b, '}')
}

// assistant writes one assistant line holding one content block.
func (w *writer) assistant(msgID string, block []byte, stop string) error {
	b := w.claudeStart("assistant")
	b = append(b, `"message":{"id":"`...)
	b = append(b, msgID...)
	b = append(b, `","type":"message","role":"assistant","model":"claude-opus-4-1","content":[`...)
	b = append(b, block...)
	b = append(b, `],"stop_reason":`...)
	b = append(b, stop...)
	b = append(b, ',')
	b = w.usage(b)
	b = append(b, `},"requestId":"req_`...)
	b = append(b, msgID[4:]...)
	b = append(b, '"')
	return w.claudeEnd(b)
}

func (w *writer) userText(text []byte) error {
	b := w.claudeStart("user")
	b = append(b, `"message":{"role":"user","content":"`...)
	b = append(b, text...)
	b = append(b, `"}`...)
	return w.claudeEnd(b)
}

// metaLine writes a line that is not conversation (about 45% of real lines).
func (w *writer) metaLine() error {
	f := w.f
	var b []byte
	switch k := w.r.IntN(6); k {
	case 0, 1:
		b = w.claudeStart("attachment")
		b = append(b, `"attachment":{"type":"total_tokens_reminder","used":`...)
		b = strconv.AppendInt(b, int64(w.r.IntN(900000)), 10)
		b = append(b, '}')
		return w.claudeEnd(b)
	case 2:
		b = w.claudeStart("system")
		b = append(b, `"subtype":"turn_duration","durationMs":`...)
		b = strconv.AppendInt(b, int64(w.r.IntN(600000)), 10)
		b = append(b, `,"messageCount":`...)
		b = strconv.AppendInt(b, int64(1+w.r.IntN(40)), 10)
		b = append(b, `,"isMeta":false`...)
		return w.claudeEnd(b)
	case 3:
		w.tick()
		b = append(w.buf[:0], `{"type":"queue-operation","operation":"enqueue","content":"`...)
		b = w.r.prose(b, 20+w.r.IntN(120))
		b = append(b, `","sessionId":"`...)
		b = append(b, f.id...)
		b = append(b, `","timestamp":`...)
		b = appendTS(b, w.t)
		b = append(b, '}')
	case 4:
		b = append(w.buf[:0], `{"type":"last-prompt","lastPrompt":"`...)
		b = w.r.prose(b, 20+w.r.IntN(200))
		b = append(b, `","sessionId":"`...)
		b = append(b, f.id...)
		b = append(b, `"}`...)
	default:
		b = append(w.buf[:0], `{"type":"file-history-snapshot","messageId":"`...)
		b = append(b, w.r.uuid4()...)
		b = append(b, `","snapshot":{"messageId":"`...)
		b = append(b, w.r.uuid4()...)
		b = append(b, `","trackedFileBackups":{},"timestamp":`...)
		b = appendTS(b, w.t)
		b = append(b, `},"isSnapshotUpdate":false}`...)
	}
	w.buf = b
	return w.emit(b)
}

func (w *writer) maybeMeta() error {
	for w.r.Float64() < nonConvChance*0.6 {
		if err := w.metaLine(); err != nil {
			return err
		}
	}
	return nil
}

var claudeTools = []string{"Bash", "Read", "Grep", "Edit", "Glob"}

func (w *writer) toolUse(id, name string) []byte {
	b := append([]byte(nil), `{"type":"tool_use","id":"`...)
	b = append(b, id...)
	b = append(b, `","name":"`...)
	b = append(b, name...)
	b = append(b, `","input":{`...)
	switch name {
	case "Bash":
		b = append(b, `"command":"`...)
		b = w.r.prose(b, 10+w.r.IntN(120))
		b = append(b, `","description":"`...)
		b = w.r.prose(b, 10+w.r.IntN(40))
		b = append(b, '"')
	case "Read", "Glob":
		b = append(b, `"file_path":`...)
		b = appendJSON(b, w.f.cwd+"/src/"+w.r.word()+"/"+w.r.word()+".go")
	case "Grep":
		b = append(b, `"pattern":"`...)
		b = append(b, w.r.word()...)
		b = append(b, `","path":"src"`...)
	case "Edit":
		b = append(b, `"file_path":`...)
		b = appendJSON(b, w.f.cwd+"/src/"+w.r.word()+".go")
		b = append(b, `,"old_string":"`...)
		b = w.r.prose(b, w.contentSize()/2)
		b = append(b, `","new_string":"`...)
		b = w.r.prose(b, w.contentSize()/2)
		b = append(b, '"')
	}
	return append(b, `}}`...)
}

// toolResult writes the tool_result line of call id, with the
// toolUseResult mirror real Claude Code writes beside it.
func (w *writer) toolResult(id string) error {
	b := w.claudeStart("user")
	b = append(b, `"message":{"role":"user","content":[{"tool_use_id":"`...)
	b = append(b, id...)
	b = append(b, `","type":"tool_result","content":`...)
	plantText, planted := w.due(true)
	if !planted {
		if n, ok := w.huge(); ok && w.r.Float64() < 0.4 {
			// An image: base64 in the result and again in the mirror.
			const head = `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`
			start := len(b) + len(head)
			b = append(b, head...)
			b = w.r.blob(b, n/2)
			end := len(b)
			b = append(b, `"}}]}]},"toolUseResult":{"type":"image","file":{"type":"image/png","base64":"`...)
			b = append(b, b[start:end]...)
			b = append(b, `"}}`...)
			return w.claudeEnd(b)
		} else if ok {
			return w.textResult(b, n/2, "")
		}
		if w.r.Float64() < persistChance {
			return w.persisted(b)
		}
	}
	return w.textResult(b, w.contentSize(), plantText)
}

func (w *writer) textResult(b []byte, n int, lead string) error {
	start := len(b) + 1
	b = append(b, '"')
	if lead != "" {
		b = append(b, lead...)
		b = append(b, ' ')
	}
	b = w.r.output(b, n, false)
	out := len(b)
	b = append(b, `"}]},"toolUseResult":{"stdout":"`...)
	b = append(b, b[start:out]...)
	b = append(b, `","stderr":"","interrupted":false}`...)
	return w.claudeEnd(b)
}

// persisted writes a large output to <session>/tool-results/ and a 2KB
// <persisted-output> preview in the transcript.
func (w *writer) persisted(b []byte) error {
	f := w.f
	full := w.r.output(nil, int(w.r.logUniform(10<<10, 200<<10)), true)
	name := "t" + hex(w.r.Uint64(), 12) + ".txt"
	dir := filepath.Join(filepath.Dir(f.path), f.id, "tool-results")
	if f.isSub() {
		dir = filepath.Join(filepath.Dir(filepath.Dir(f.path)), "tool-results")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name), full, 0o644); err != nil {
		return err
	}
	w.res.companions++
	preview := string(full[:min(len(full), 2048)])
	shown := "/home/synth/.claude/projects/" + projectDir(f.cwd) + "/" + f.id + "/tool-results/" + name
	content := fmt.Sprintf("<persisted-output>\nOutput too large (%.1fKB). Full output saved to: %s\n\nPreview (first 2KB):\n%s\n...\n</persisted-output>", float64(len(full))/1024, shown, preview)
	b = appendJSON(b, content)
	b = append(b, `}]},"toolUseResult":{"stdout":`...)
	b = appendJSON(b, preview)
	b = append(b, `,"stderr":"","interrupted":false}`...)
	return w.claudeEnd(b)
}

// spawn writes the Agent tool call that starts subagent s and its result.
func (w *writer) spawn(s *fileSpec) error {
	msgID := "msg_" + hex(w.r.Uint64(), 16)
	block := append([]byte(nil), `{"type":"tool_use","id":"`...)
	block = append(block, s.toolUseID...)
	block = append(block, `","name":"Agent","input":{"description":"`...)
	block = w.r.prose(block, 20)
	block = append(block, `","prompt":"`...)
	block = w.r.prose(block, 100+w.r.IntN(400))
	block = append(block, `","subagent_type":"Explore"}}`...)
	if err := w.assistant(msgID, block, `"tool_use"`); err != nil {
		return err
	}
	text := w.r.prose(nil, 100+w.r.IntN(600))
	b := w.claudeStart("user")
	b = append(b, `"message":{"role":"user","content":[{"tool_use_id":"`...)
	b = append(b, s.toolUseID...)
	b = append(b, `","type":"tool_result","content":[{"type":"text","text":"`...)
	b = append(b, text...)
	b = append(b, `"}]}]},"toolUseResult":{"status":"completed","agentId":"`...)
	b = append(b, s.agentID...)
	b = append(b, `","content":[{"type":"text","text":"`...)
	b = append(b, text...)
	b = append(b, `"}]}`...)
	return w.claudeEnd(b)
}

func (w *writer) claude() error {
	f := w.f
	if f.isSub() {
		meta := fmt.Sprintf(`{"agentType":"Explore","description":"synthetic subagent","toolUseId":%q,"spawnDepth":1,"parentAgentId":null}`, f.toolUseID)
		if err := os.WriteFile(strings.TrimSuffix(f.path, ".jsonl")+".meta.json", []byte(meta+"\n"), 0o644); err != nil {
			return err
		}
	} else {
		b := append(w.buf[:0], `{"type":"permission-mode","permissionMode":"default","sessionId":"`...)
		b = append(b, f.id...)
		b = append(b, `"}`...)
		w.buf = b
		if err := w.emit(b); err != nil {
			return err
		}
	}
	subs := f.subs
	for turn := 0; !w.done() || len(subs) > 0; turn++ {
		var text []byte
		if turn == 0 && !f.isSub() {
			// The fresh check finds a session by its id prefix.
			text = append(text, "Working in session "+f.id+". "...)
		}
		if err := w.userText(w.proseSlot(text, w.contentSize()/2)); err != nil {
			return err
		}
		if err := w.maybeMeta(); err != nil {
			return err
		}
		steps := 1 + w.r.IntN(6)
		for range steps {
			msgID := "msg_" + hex(w.r.Uint64(), 16)
			if w.r.Float64() < 0.35 {
				block := append([]byte(nil), `{"type":"thinking","thinking":"`...)
				block = w.r.prose(block, w.contentSize())
				block = append(block, `","signature":"`...)
				block = w.r.blob(block, 200+w.r.IntN(1200))
				block = append(block, `"}`...)
				if err := w.assistant(msgID, block, "null"); err != nil {
					return err
				}
			}
			if w.r.Float64() < 0.4 {
				block := append([]byte(nil), `{"type":"text","text":"`...)
				block = w.proseSlot(block, w.contentSize()/2)
				block = append(block, `"}`...)
				if err := w.assistant(msgID, block, "null"); err != nil {
					return err
				}
			}
			if len(subs) > 0 && float64(w.n) >= float64(f.size)*float64(len(f.subs)-len(subs)+1)/float64(len(f.subs)+1) {
				if err := w.spawn(subs[0]); err != nil {
					return err
				}
				subs = subs[1:]
				continue
			}
			id := "toolu_" + hex(w.r.Uint64(), 16)
			if err := w.assistant(msgID, w.toolUse(id, claudeTools[w.r.IntN(len(claudeTools))]), `"tool_use"`); err != nil {
				return err
			}
			if err := w.maybeMeta(); err != nil {
				return err
			}
			if err := w.toolResult(id); err != nil {
				return err
			}
			if err := w.maybeMeta(); err != nil {
				return err
			}
		}
		block := append([]byte(nil), `{"type":"text","text":"`...)
		block = w.proseSlot(block, w.contentSize())
		block = append(block, `"}`...)
		if err := w.assistant("msg_"+hex(w.r.Uint64(), 16), block, `"end_turn"`); err != nil {
			return err
		}
		if err := w.maybeMeta(); err != nil {
			return err
		}
		if turn == 0 && !f.isSub() {
			b := append(w.buf[:0], `{"type":"ai-title","aiTitle":"`...)
			b = w.r.prose(b, 30)
			b = append(b, `","sessionId":"`...)
			b = append(b, f.id...)
			b = append(b, `"}`...)
			w.buf = b
			if err := w.emit(b); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- Codex ---

func (w *writer) codexStart(typ string) []byte {
	w.tick()
	b := append(w.buf[:0], `{"timestamp":`...)
	b = appendTS(b, w.t)
	b = append(b, `,"ordinal":`...)
	b = strconv.AppendInt(b, w.ord, 10)
	w.ord++
	b = append(b, `,"type":"`...)
	b = append(b, typ...)
	b = append(b, `","payload":{`...)
	return b
}

func (w *writer) codexEnd(b []byte) error {
	b = append(b, `}}`...)
	w.buf = b
	return w.emit(b)
}

func (w *writer) turnMeta(b []byte, turn string) []byte {
	b = append(b, `,"internal_chat_message_metadata_passthrough":{"turn_id":"`...)
	b = append(b, turn...)
	return append(b, `"}`...)
}

type codexItem struct{ id, role, text string }

func (w *writer) codex() error {
	f := w.f
	b := w.codexStart("session_meta")
	b = append(b, `"session_id":"`...)
	b = append(b, f.id...)
	b = append(b, `","id":"`...)
	b = append(b, f.id...)
	b = append(b, `","timestamp":`...)
	b = appendTS(b, w.t)
	b = append(b, `,"cwd":`...)
	b = appendJSON(b, f.cwd)
	b = append(b, `,"originator":"codex-tui","cli_version":"0.154.0","source":"cli","thread_source":"user","model_provider":"openai","history_mode":"paginated","git":{"commit_hash":"`...)
	b = append(b, hex(w.r.Uint64(), 16)+hex(w.r.Uint64(), 16)+hex(w.r.Uint64(), 8)...)
	b = append(b, `","branch":"main","repository_url":`...)
	b = appendJSON(b, remoteOf(f.cwd))
	b = append(b, `},"base_instructions":{"text":"`...)
	b = w.r.prose(b, 2000+w.r.IntN(6000))
	b = append(b, `"}`...)
	if err := w.codexEnd(b); err != nil {
		return err
	}
	var recent []codexItem // replayed by compactions
	remember := func(it codexItem) {
		recent = append(recent, it)
		if len(recent) > 30 {
			recent = recent[1:]
		}
	}
	for turnN := 0; !w.done(); turnN++ {
		turn := w.r.uuid7()
		b := w.codexStart("event_msg")
		b = append(b, `"type":"task_started","turn_id":"`...)
		b = append(b, turn...)
		b = append(b, '"')
		if err := w.codexEnd(b); err != nil {
			return err
		}
		b = w.codexStart("turn_context")
		b = append(b, `"turn_id":"`...)
		b = append(b, turn...)
		b = append(b, `","cwd":`...)
		b = appendJSON(b, f.cwd)
		b = append(b, `,"model":"gpt-5-codex","approval_policy":"on-request","user_instructions":"`...)
		b = w.r.prose(b, 200+w.r.IntN(1500))
		b = append(b, '"')
		if err := w.codexEnd(b); err != nil {
			return err
		}
		prompt := string(w.r.prose(nil, w.contentSize()/2))
		id := "msg_" + hex(w.r.Uint64(), 16)
		b = w.codexStart("response_item")
		b = append(b, `"type":"message","id":"`...)
		b = append(b, id...)
		b = append(b, `","role":"user","content":[{"type":"input_text","text":"`...)
		b = append(b, prompt...)
		b = append(b, `"}]`...)
		b = w.turnMeta(b, turn)
		if err := w.codexEnd(b); err != nil {
			return err
		}
		remember(codexItem{id, "user", prompt})
		b = w.codexStart("event_msg")
		b = append(b, `"type":"user_message","message":"`...)
		b = append(b, prompt...)
		b = append(b, `","images":[]`...)
		if err := w.codexEnd(b); err != nil {
			return err
		}
		for range 1 + w.r.IntN(6) {
			b = w.codexStart("response_item")
			b = append(b, `"type":"reasoning","id":"rs_`...)
			b = append(b, hex(w.r.Uint64(), 16)...)
			b = append(b, `","summary":[{"type":"summary_text","text":"`...)
			b = w.r.prose(b, 20+w.r.IntN(200))
			b = append(b, `"}],"content":null,"encrypted_content":"`...)
			b = w.r.blob(b, w.contentSize())
			b = append(b, '"')
			if err := w.codexEnd(b); err != nil {
				return err
			}
			call := "call_" + hex(w.r.Uint64(), 16)
			cmd := string(w.r.prose(nil, 10+w.r.IntN(60)))
			b = w.codexStart("response_item")
			b = append(b, `"type":"custom_tool_call","id":"ctc_`...)
			b = append(b, hex(w.r.Uint64(), 16)...)
			b = append(b, `","status":"completed","call_id":"`...)
			b = append(b, call...)
			b = append(b, `","name":"exec","input":"text(await tools.exec_command({cmd:\"`...)
			b = append(b, cmd...)
			b = append(b, `\"}));\n"`...)
			b = w.turnMeta(b, turn)
			if err := w.codexEnd(b); err != nil {
				return err
			}
			n := w.contentSize()
			if h, ok := w.huge(); ok {
				n = h / 2
			}
			out := w.r.output(nil, n, false)
			b = w.codexStart("event_msg")
			b = append(b, `"type":"item_completed","thread_id":"`...)
			b = append(b, f.id...)
			b = append(b, `","turn_id":"`...)
			b = append(b, turn...)
			b = append(b, `","item":{"type":"CommandExecution","id":"exec-`...)
			b = append(b, hex(w.r.Uint64(), 12)...)
			b = append(b, `","process_id":"4242","command":["/bin/zsh","-lc","`...)
			b = append(b, cmd...)
			b = append(b, `"],"cwd":`...)
			b = appendJSON(b, "file://"+f.cwd)
			b = append(b, `,"parsed_cmd":[{"type":"unknown","cmd":"`...)
			b = append(b, cmd...)
			b = append(b, `"}],"source":"unified_exec_startup","status":"completed","stdout":"`...)
			b = append(b, out...)
			b = append(b, `","stderr":"","aggregated_output":"`...)
			b = append(b, out...)
			b = append(b, `","exit_code":0,"duration":{"secs":1,"nanos":0}}`...)
			if err := w.codexEnd(b); err != nil {
				return err
			}
			b = w.codexStart("response_item")
			b = append(b, `"type":"custom_tool_call_output","id":"ctco_`...)
			b = append(b, hex(w.r.Uint64(), 16)...)
			b = append(b, `","call_id":"`...)
			b = append(b, call...)
			b = append(b, `","output":"Script completed\nOutput:\n`...)
			b = append(b, out...)
			b = append(b, '"')
			b = w.turnMeta(b, turn)
			if err := w.codexEnd(b); err != nil {
				return err
			}
			b = w.codexStart("event_msg")
			b = append(b, `"type":"token_count","info":{"total_token_usage":{"input_tokens":`...)
			b = strconv.AppendInt(b, int64(w.r.IntN(400000)), 10)
			b = append(b, `,"output_tokens":`...)
			b = strconv.AppendInt(b, int64(w.r.IntN(40000)), 10)
			b = append(b, `}},"rate_limits":null`...)
			if err := w.codexEnd(b); err != nil {
				return err
			}
		}
		before := w.nPlanted
		answer := string(w.proseSlot(nil, w.contentSize()))
		planted := w.nPlanted != before
		id = "msg_" + hex(w.r.Uint64(), 16)
		b = w.codexStart("response_item")
		b = append(b, `"type":"message","id":"`...)
		b = append(b, id...)
		b = append(b, `","role":"assistant","content":[{"type":"output_text","text":"`...)
		b = append(b, answer...)
		b = append(b, `"}]`...)
		b = w.turnMeta(b, turn)
		if err := w.codexEnd(b); err != nil {
			return err
		}
		if !planted {
			remember(codexItem{id, "assistant", answer})
		}
		b = w.codexStart("event_msg")
		b = append(b, `"type":"agent_message","message":"`...)
		b = append(b, answer...)
		b = append(b, '"')
		if err := w.codexEnd(b); err != nil {
			return err
		}
		b = w.codexStart("event_msg")
		b = append(b, `"type":"task_complete","turn_id":"`...)
		b = append(b, turn...)
		b = append(b, '"')
		if err := w.codexEnd(b); err != nil {
			return err
		}
		// Compaction: replays recent items plus an encrypted summary,
		// 70-100KB per line.
		if turnN%12 == 11 && w.f.size > 1<<20 {
			b = w.codexStart("compacted")
			b = append(b, `"message":"","replacement_history":[`...)
			for _, it := range recent {
				b = append(b, `{"type":"message","id":"`...)
				b = append(b, it.id...)
				b = append(b, `","role":"`...)
				b = append(b, it.role...)
				if it.role == "user" {
					b = append(b, `","content":[{"type":"input_text","text":"`...)
				} else {
					b = append(b, `","content":[{"type":"output_text","text":"`...)
				}
				b = append(b, it.text...)
				b = append(b, `"}]},`...)
			}
			b = append(b, `{"type":"compaction","id":"cmp_`...)
			b = append(b, hex(w.r.Uint64(), 12)...)
			b = append(b, `","encrypted_content":"`...)
			b = w.r.blob(b, 70<<10+w.r.IntN(30<<10))
			b = append(b, `"}],"retained_context":null,"window_id":"win-`...)
			b = append(b, hex(w.r.Uint64(), 8)...)
			b = append(b, `","window_number":`...)
			b = strconv.AppendInt(b, int64(turnN/12+1), 10)
			if err := w.codexEnd(b); err != nil {
				return err
			}
			recent = recent[:0]
		}
	}
	return nil
}
