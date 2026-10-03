// Package cassimport recovers CASS's normalized history. Its evidence is an
// export of CASS rows, never a reconstruction of missing native transcripts.
package cassimport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/flopwire/flopwire/internal/transcript"
)

const Name = "cass@1"
const StorageKind transcript.StorageKind = "cass_export"

// Record is the versioned recovery format. The first record describes the
// conversation; every subsequent record contains exactly one stored message.
type Record struct {
	Version      int                      `json:"version"`
	Conversation *transcript.Conversation `json:"conversation,omitempty"`
	Message      *transcript.Message      `json:"message,omitempty"`
}

// Parse streams a complete immutable export. A malformed record fails the
// source instead of silently losing recovered history.
func Parse(ctx context.Context, in transcript.Input, sink transcript.Sink) (transcript.Cursor, error) {
	var session string
	next, err := transcript.ScanJSONL(ctx, in, transcript.Cursor{}, transcript.LineReaderOptions{}, func(lr *transcript.LineReader, l *transcript.Line) error {
		var rec Record
		var r io.Reader
		if l.Oversized {
			r = lr.Open(l)
		} else {
			r = bytes.NewReader(l.Data)
		}
		dec := json.NewDecoder(r)
		dec.UseNumber()
		if err := dec.Decode(&rec); err != nil {
			return fmt.Errorf("cass: record %d: %w", l.No, err)
		}
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("cass: trailing data at record %d", l.No)
		}
		if rec.Version != 1 || (rec.Conversation == nil) == (rec.Message == nil) {
			return fmt.Errorf("cass: invalid record %d", l.No)
		}
		if rec.Conversation != nil {
			c := rec.Conversation
			if l.No != 1 || c.SessionID == "" || c.Agent != in.Source.Agent {
				return fmt.Errorf("cass: invalid conversation")
			}
			session = c.SessionID
			return sink.Conversation(c)
		}
		if session == "" || rec.Message.SessionID != session {
			return fmt.Errorf("cass: message without matching conversation")
		}
		m := rec.Message
		m.Parser = Name
		m.LineNo, m.ByteOffset, m.ByteLen = l.No, l.Offset, l.Len
		m.SetText(m.Text, transcript.CapConfig{})
		return sink.Message(m)
	})
	if err == nil && (session == "" || next.Offset != in.Size) {
		err = fmt.Errorf("cass: incomplete recovery evidence")
	}
	return next, err
}
