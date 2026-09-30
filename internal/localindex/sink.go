package localindex

import (
	"context"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Sink adapts parser output to ApplyBatch: it buffers conversations and
// messages for one source and applies them in batches of BatchSize
// messages. It implements transcript.SessionSuperseder, so multi-session
// parsers (Devin) can use it: a SupersedeSession call applies the pending
// batch first, so ordering against earlier rows is kept, and rows the
// parser re-emits later in the same parse are revived by the upsert.
//
// Call Flush after Parse returns, with the watermark and cursor state to
// save in the same transaction as the last rows.
type Sink struct {
	Store      *Store
	Ctx        context.Context
	SourceID   int64
	Generation int64
	BatchSize  int // default 1000

	// Result accumulates the BatchResult of every applied batch.
	Result BatchResult

	// SupersedeAbsent and RetireSources apply with the final flush (the
	// one given a watermark), in its transaction; see Batch.
	SupersedeAbsent bool
	RetireSources   []int64
	Extraction      *transcript.ExtractionCheckpoint
	AppliedParser   string

	convs []*transcript.Conversation
	msgs  []*transcript.Message
}

var _ transcript.SessionSuperseder = (*Sink)(nil)

// NewSink returns a Sink writing rows of source sourceID stamped with
// generation gen.
func (s *Store) NewSink(ctx context.Context, sourceID, gen int64) *Sink {
	return &Sink{Store: s, Ctx: ctx, SourceID: sourceID, Generation: gen}
}

func (k *Sink) Conversation(c *transcript.Conversation) error {
	k.convs = append(k.convs, c)
	return nil
}

func (k *Sink) Message(m *transcript.Message) error {
	if k.Store.opts.SyncOnly {
		return nil // no message rows: skip buffering and text preparation
	}
	k.msgs = append(k.msgs, m)
	n := k.BatchSize
	if n <= 0 {
		n = 1000
	}
	if len(k.msgs) >= n {
		return k.Flush(nil, nil)
	}
	return nil
}

// SupersedeSession applies the pending batch, then marks every live row of
// the session superseded in the sink's generation.
func (k *Sink) SupersedeSession(agent transcript.Agent, sessionID string) error {
	if err := k.Flush(nil, nil); err != nil {
		return err
	}
	_, err := k.Store.SupersedeSession(k.Ctx, agent, sessionID, k.Generation)
	return err
}

// Flush applies the pending rows, saving wm and cursorState with them when
// wm is set (after SupersedeAbsent and RetireSources). It writes even when
// nothing is pending if wm is set.
func (k *Sink) Flush(wm *transcript.Watermark, cursorState []byte) error {
	if len(k.convs) == 0 && len(k.msgs) == 0 && wm == nil {
		return nil
	}
	k.Store.tombs.mask(k.msgs) // messages the owner redacted (notes/redaction.md)
	k.Store.tombs.maskTitles(k.convs)
	b := Batch{SourceID: k.SourceID, Generation: k.Generation,
		Conversations: k.convs, Messages: k.msgs, Watermark: wm, CursorState: cursorState,
		prep: prepareAll(k.msgs)}
	if wm != nil {
		b.SupersedeAbsent, b.RetireSources = k.SupersedeAbsent, k.RetireSources
		b.Extraction, b.AppliedParser = k.Extraction, k.AppliedParser
	}
	r, err := k.Store.ApplyBatch(k.Ctx, b)
	if err != nil {
		return err
	}
	k.Result.Inserted += r.Inserted
	k.Result.Grown += r.Grown
	k.Result.Touched += r.Touched
	k.Result.Versioned += r.Versioned
	k.Result.Conversations += r.Conversations
	k.convs, k.msgs = nil, k.msgs[:0]
	return nil
}
