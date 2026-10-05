package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/transcript"
)

// lineRef locates a physical line of the rollout.
type lineRef struct {
	No  int64 `json:"n"`
	Off int64 `json:"o"`
	Len int64 `json:"l"` // physical length, terminator included
}

func (r *run) lineRef() lineRef { return lineRef{No: r.l.No, Off: r.l.Offset, Len: r.l.Len} }

// readLine reads a line of this rollout back, without its terminator. The
// bytes before the cursor never change within a generation.
func (r *run) readLine(ref lineRef) ([]byte, error) {
	if ref.Len <= 0 || ref.Len > maxOversized {
		return nil, fmt.Errorf("codex: line %d: length %d", ref.No, ref.Len)
	}
	b := make([]byte, ref.Len)
	n, err := r.in.R.ReadAt(b, ref.Off)
	if int64(n) != ref.Len {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("codex: read line %d: %w", ref.No, err)
	}
	return bytes.TrimRight(b, "\r\n"), nil
}

// callRow renders a function_call or custom_tool_call item.
func (r *run) callRow(ts time.Time, ord *int64, it *item, ref lineRef) row {
	name := it.Name
	text := rawString(it.Arguments)
	if it.Type == "custom_tool_call" || text == "" {
		text = rawString(it.Input)
	}
	w := newRowAt(kindToolCall, ts, ord, 0, ref)
	w.NativeID, w.Role, w.ToolName, w.ToolCallID = it.id(), "assistant", name, it.CallID
	w.Paths = patchPaths(callPatchText(name, text))
	r.setText(&w, text)
	return w
}

// callCache holds the rows and decoded events of the calls tracked in
// cursor state that this Parse call has seen, so a re-emission reads the
// rollout back only for calls from an earlier call. Keyed by line offset.
type callCache struct {
	rows   map[int64]row
	events map[int64]*event
}

func newCallCache() callCache {
	return callCache{rows: map[int64]row{}, events: map[int64]*event{}}
}

// forget drops a call that is no longer tracked.
func (c callCache) forget(oc *openCall) {
	delete(c.rows, oc.Line.Off)
	for _, e := range oc.Events {
		delete(c.events, e.Off)
	}
}

// readCall returns a call's row as first emitted, from the cache or read
// back from the rollout. ok is false when the line no longer decodes as a
// call.
func (r *run) readCall(ref lineRef) (row, bool, error) {
	if w, ok := r.cache.rows[ref.Off]; ok {
		return w, true, nil
	}
	w, ok, err := r.rereadCall(ref)
	if ok {
		r.cache.rows[ref.Off] = w
	}
	return w, ok, err
}

func (r *run) rereadCall(ref lineRef) (row, bool, error) {
	b, err := r.readLine(ref)
	if err != nil {
		return row{}, false, err
	}
	var ts time.Time
	var ord *int64
	var it item
	if typ, _ := transcript.PeekString(bytes.NewReader(b), "type"); typ == "response_item" {
		var ln line[item]
		if !lenient(json.Unmarshal(b, &ln)) {
			return row{}, false, nil
		}
		ts, ord, it = parseTS(ln.Timestamp), ln.Ordinal, ln.Payload
	} else {
		if !lenient(json.Unmarshal(b, &it)) {
			return row{}, false, nil
		}
		ts = parseTS(it.Timestamp)
	}
	if it.Type != "function_call" && it.Type != "custom_tool_call" {
		return row{}, false, nil
	}
	return r.callRow(ts, ord, &it, ref), true, nil
}

func lenient(err error) bool {
	var te *json.UnmarshalTypeError
	return err == nil || errors.As(err, &te)
}

// enrichedCall reads a call and its attached events back and returns the
// call row with the events applied (commands, changed paths, failure).
func (r *run) enrichedCall(oc *openCall) (row, bool, error) {
	w, ok, err := r.readCall(oc.Line)
	if err != nil || !ok {
		return w, ok, err
	}
	var failed bool
	var paths []string
	w.Commands = nil
	for _, ref := range oc.Events {
		e, err := r.readEvent(ref)
		if err != nil {
			return row{}, false, err
		}
		if e == nil {
			continue
		}
		if e.Command != nil {
			w.Commands = append(w.Commands, *e.Command)
		}
		paths = appendUnique(paths, e.Paths...)
		failed = failed || e.Failed
	}
	w.Paths = appendUnique(append([]string(nil), w.Paths...), paths...)
	w.IsError = w.IsError || failed
	w.Enriched = EventsVersion
	return w, true, nil
}

// readEvent returns an attached event, from the cache or read back from
// the rollout; nil when it no longer decodes.
func (r *run) readEvent(ref lineRef) (*event, error) {
	if e, ok := r.cache.events[ref.Off]; ok {
		return e, nil
	}
	b, err := r.readLine(ref)
	if err != nil {
		return nil, err
	}
	var ln line[struct {
		Item *eventItem `json:"item"`
	}]
	if json.Unmarshal(b, &ln) != nil {
		return nil, nil
	}
	e, err := parseEvent(ln.Payload.Item)
	if err != nil {
		return nil, nil
	}
	r.cache.events[ref.Off] = e
	return e, nil
}

// parentHistory is what a fork's parent rollout holds: its response item
// payload ids and its turn ids. A nil *parentHistory means the parent could
// not be read.
type parentHistory struct {
	ids, turns map[string]struct{}
	missing    bool
}

func (ph *parentHistory) has(m map[string]struct{}, k string) bool {
	_, ok := m[k]
	return ok
}

// parentHistory loads the fork parent's ids once per Parse call. Only a
// parent that is not there (fs.ErrNotExist) reads as missing, which keeps
// the fork's copied history. Any other error (object storage down, a
// parent whose bytes cannot be read yet) fails the parse so it is retried:
// treating it as missing would index the copied history for good.
func (r *run) parentHistory() (*parentHistory, error) {
	if r.parent == nil {
		ph, err := r.loadParent()
		if err != nil {
			return nil, fmt.Errorf("codex: read fork parent %s: %w", r.st.Meta.ForkedFrom, err)
		}
		r.parent = ph
	}
	if r.parent.missing {
		return nil, nil
	}
	return r.parent, r.ctx.Err()
}

func (r *run) loadParent() (*parentHistory, error) {
	open := r.p.OpenRollout
	if open == nil {
		open = openLocalRollout
	}
	f, err := open(r.path, r.st.Meta.ForkedFrom)
	if errors.Is(err, fs.ErrNotExist) {
		return &parentHistory{missing: true}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ph := &parentHistory{ids: map[string]struct{}{}, turns: map[string]struct{}{}}
	// No LineBudget: this scan runs while the child's reader may hold a
	// reservation. Oversized lines are read from their head.
	in := transcript.Input{R: f, Size: f.Size()}
	_, err = transcript.ScanJSONL(r.ctx, in, transcript.Cursor{}, transcript.LineReaderOptions{}, func(_ *transcript.LineReader, l *transcript.Line) error {
		var typ, id, turn string
		if l.Oversized {
			typ, _ = transcript.PeekLine(l, "type")
			id, _ = transcript.PeekLine(l, "payload", "id")
			turn, _ = transcript.PeekLine(l, "payload", "turn_id")
		} else {
			var v struct {
				Type    string `json:"type"`
				Payload struct {
					ID     *string `json:"id"`
					TurnID string  `json:"turn_id"`
				} `json:"payload"`
			}
			if json.Unmarshal(l.Data, &v) != nil {
				return nil
			}
			typ, turn = v.Type, v.Payload.TurnID
			if v.Payload.ID != nil {
				id = *v.Payload.ID
			}
		}
		if typ == "response_item" && id != "" {
			ph.ids[id] = struct{}{}
		}
		if turn != "" {
			ph.turns[turn] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ph, nil
}

// FindRollout returns the path of the rollout of sessionID under the Codex
// home that holds childPath (sessions/YYYY/MM/DD or archived_sessions), or
// "" when there is none. The name must hold the whole id after the
// rollout's timestamp (rollout-2006-01-02T15-04-05-ID.jsonl): a tail of
// an id names no rollout.
func FindRollout(childPath, sessionID string) string {
	if sessionID == "" || strings.ContainsAny(sessionID, `*?[\/`) {
		return ""
	}
	home := ""
	for d := filepath.Dir(childPath); d != filepath.Dir(d); d = filepath.Dir(d) {
		if b := filepath.Base(d); b == SessionsDir || b == ArchivedDir {
			home = filepath.Dir(d)
			break
		}
	}
	if home == "" {
		return ""
	}
	name := "rollout-*-" + sessionID + ".jsonl"
	for _, pat := range []string{
		filepath.Join(home, SessionsDir, "*", "*", "*", name),
		filepath.Join(home, ArchivedDir, name),
		filepath.Join(home, ArchivedDir, "*", "*", "*", name),
	} {
		m, _ := fsprobe.Glob(pat)
		for _, p := range m {
			ts := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "rollout-"), "-"+sessionID+".jsonl")
			if _, err := time.Parse("2006-01-02T15-04-05", ts); err == nil {
				return p
			}
		}
	}
	return ""
}

// findRollout is FindRollout; tests replace it.
var findRollout = FindRollout

// openLocalRollout opens the rollout FindRollout locates. Codex may move
// it into archived_sessions between the locate and the open; a file gone
// by the open is located once more before it counts as missing.
func openLocalRollout(childPath, sessionID string) (File, error) {
	var f *os.File
	err := error(os.ErrNotExist)
	for try := 0; try < 2 && errors.Is(err, fs.ErrNotExist); try++ {
		p := findRollout(childPath, sessionID)
		if p == "" {
			return nil, os.ErrNotExist
		}
		f, err = fsprobe.Open(p)
	}
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return localFile{f, fi.Size()}, nil
}

type localFile struct {
	*os.File
	size int64
}

func (f localFile) Size() int64 { return f.size }
