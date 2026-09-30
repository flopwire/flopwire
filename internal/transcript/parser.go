package transcript

import (
	"context"
	"errors"
	"io"
)

// Cursor is where a parser stopped. For JSONL sources Offset is the byte
// offset just past the last complete line and LineNo is the number of
// physical lines before Offset; the next line is LineNo+1. For other
// storage kinds Offset carries the watermark (rowid, array index) and LineNo
// is zero. State is opaque per-parser state (pending tool calls, the session
// id read from a header line, ...) that the caller persists verbatim next to
// the offset. The zero Cursor means "from the start".
type Cursor struct {
	Offset int64
	LineNo int64
	State  []byte
}

// IsStart reports whether c is the start of a source.
func (c Cursor) IsStart() bool { return c.Offset == 0 && c.LineNo == 0 && len(c.State) == 0 }

// Sink receives parser output in source order. A parser allocates a fresh
// value for every call, so the sink may retain it. Returning an error stops
// the parse; the parser returns that error unchanged.
type Sink interface {
	Conversation(*Conversation) error
	Message(*Message) error
}

// Input is the source a parser reads. Size bounds the read (the size the
// caller sampled when it decided to parse), so bytes appended mid-parse are
// left for the next pass. R is typically the open *os.File; the caller keeps
// it open while tailing so an unlinked file stays readable until drained.
type Input struct {
	Source *Source
	R      io.ReaderAt
	Size   int64
}

// Parser extracts conversations and messages from one storage kind of one
// harness.
//
// Parse resumes at cur, reads up to in.Size, emits every complete record to
// sink and returns the cursor after the last complete record. An incomplete
// trailing record (a JSONL line without its newline) is neither emitted nor
// consumed: the returned cursor stops before it and the next call re-reads
// it. Parse must not buffer more than one record, so memory stays bounded by
// the largest record the parser chooses to materialize.
//
// Parsers ignore record types they do not know and never fail on them; they
// return an error only for I/O failures or a sink error.
type Parser interface {
	// Name is the parser name and version recorded on every row, e.g. "claude@1".
	Name() string
	Agent() Agent
	StorageKind() StorageKind
	Parse(ctx context.Context, in Input, cur Cursor, sink Sink) (Cursor, error)
}

// Reparse is the full-reparse path used after Decide returns Rewrite: it
// parses the whole source from a zero cursor, discarding any saved parser
// state. The caller starts a new generation, upserts the emitted rows by
// native id (or locator) and marks rows absent from the new generation
// superseded (spec §4.2).
func Reparse(ctx context.Context, p Parser, in Input, sink Sink) (Cursor, error) {
	return p.Parse(ctx, in, Cursor{}, sink)
}

// Collector is a Sink that keeps everything in memory. For tests and small
// sources only.
type Collector struct {
	Conversations      []*Conversation
	Messages           []*Message
	SupersededSessions []string // SupersedeSession calls, in order
}

func (c *Collector) Conversation(v *Conversation) error {
	c.Conversations = append(c.Conversations, v)
	return nil
}

func (c *Collector) Message(v *Message) error {
	c.Messages = append(c.Messages, v)
	return nil
}

// MessagesFor returns the collected messages of one session, in emission order.
func (c *Collector) MessagesFor(sessionID string) []*Message {
	var out []*Message
	for _, m := range c.Messages {
		if m.SessionID == sessionID {
			out = append(out, m)
		}
	}
	return out
}

// ScanJSONL drives a JSONL parser: it reads complete lines of in from cur
// and calls fn for each. It returns the cursor past the last line fn
// accepted, with State left for the caller to fill. An fn error stops the
// scan before that line, so the line is re-read on the next call.
func ScanJSONL(ctx context.Context, in Input, cur Cursor, opts LineReaderOptions, fn func(lr *LineReader, l *Line) error) (Cursor, error) {
	lr := NewLineReader(in.R, cur.Offset, cur.LineNo, in.Size, opts)
	defer lr.Release()
	next := Cursor{Offset: cur.Offset, LineNo: cur.LineNo, State: cur.State}
	for i := 0; ; i++ {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return next, err
			}
		}
		l, err := lr.Next()
		if errors.Is(err, io.EOF) {
			return next, nil
		}
		if err != nil {
			return next, err
		}
		if err := fn(lr, l); err != nil {
			return next, err
		}
		next.Offset, next.LineNo = lr.Offset(), lr.LineNo()
	}
}
