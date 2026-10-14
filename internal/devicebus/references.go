package devicebus

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// ReferenceReader is optional: older servers and bus-only credentials still
// deliver messages. Implementations must authorize reads as the receiving
// device and honor cancellation. A failed read never falls back to local text.
type ReferenceReader interface {
	ReadReference(context.Context, string) (string, error)
}

const (
	referenceWait    = time.Second
	referenceAge     = 30 * time.Second
	referenceEntries = 128
)

type referenceKey struct{ message, address string }
type referenceSnapshot struct {
	text, credential string
	expires          time.Time
}

// InvalidateReferences also fences reads already in flight. No excerpt text is
// persisted: restarting the agent keeps its inbox, but drops these snapshots.
func (b *Bus) InvalidateReferences() {
	b.mu.Lock()
	b.referenceEpoch++
	b.references = nil
	b.mu.Unlock()
}

// prepareReferences runs on the delivery loop, before newly received messages
// enter the inbox. One deadline covers the entire batch, not each reference.
// Existing queued messages remain deliverable while enrichment runs.
func (b *Bus) prepareReferences(ctx context.Context, msgs []busproto.Envelope) {
	if b.Local() || len(msgs) == 0 {
		return
	}
	srv, credential := b.cfg.Connect()
	reader, ok := srv.(ReferenceReader)
	if !ok {
		return
	}
	started := b.cfg.Now()
	b.mu.Lock()
	epoch := b.referenceEpoch
	for k, v := range b.references {
		if !started.Before(v.expires) || v.credential != credential {
			delete(b.references, k)
		}
	}
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, referenceWait)
	defer cancel()
	ready := make(map[referenceKey]referenceSnapshot)
	// The batch is bounded independently of the server's inbox limit.
	reads := 0
	for _, m := range msgs {
		for i, ref := range m.Refs {
			if i >= busrender.MaxRefs || reads >= referenceEntries || ctx.Err() != nil {
				break
			}
			addr, err := format.ParseAddress(ref)
			if err != nil || addr.Kind != format.AddrMessage {
				continue
			}
			reads++
			text, err := reader.ReadReference(ctx, ref)
			var denied *client.APIError
			if errors.As(err, &denied) && (denied.StatusCode == http.StatusUnauthorized || denied.StatusCode == http.StatusForbidden) {
				b.InvalidateReferences()
				return
			}
			if err != nil || text == "" || ctx.Err() != nil {
				continue
			}
			text = format.Cut(text, 1, 4*busrender.MaxExcerptBytes).Text
			ready[referenceKey{m.ID, ref}] = referenceSnapshot{text, credential, started.Add(referenceAge)}
		}
	}
	_, current := b.cfg.Connect()
	b.mu.Lock()
	defer b.mu.Unlock()
	if epoch != b.referenceEpoch || current != credential || !b.cfg.Now().Before(started.Add(referenceAge)) {
		return
	}
	// Keep valid snapshots usable during refresh; replace them atomically
	// when the read completes, including removing denied/missing results.
	for _, m := range msgs {
		for _, ref := range m.Refs {
			delete(b.references, referenceKey{m.ID, ref})
		}
	}
	if b.references == nil {
		b.references = make(map[referenceKey]referenceSnapshot)
	}
	for k, v := range ready {
		if len(b.references) >= referenceEntries {
			// Discard an old snapshot, never a durable message or receipt.
			var oldest referenceKey
			var at time.Time
			for key, old := range b.references {
				if at.IsZero() || old.expires.Before(at) {
					oldest, at = key, old.expires
				}
			}
			delete(b.references, oldest)
		}
		b.references[k] = v
	}
}

// ReferenceExcerpts is a local-only hook read. Cache entries are scoped to a
// delivered envelope and the receiving credential, never just a ref string.
func (b *Bus) ReferenceExcerpts(msgs []busproto.Envelope) map[string]string {
	if b.Local() {
		return nil
	}
	if b.cfg.NoDevice != nil && b.cfg.NoDevice() != "" {
		b.InvalidateReferences()
		return nil
	}
	_, credential := b.cfg.Connect()
	now := b.cfg.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out map[string]string
	for _, m := range msgs {
		for _, ref := range m.Refs {
			k := referenceKey{m.ID, ref}
			v, ok := b.references[k]
			if !ok {
				continue
			}
			if v.credential != credential || !now.Before(v.expires) {
				delete(b.references, k)
				continue
			}
			if out == nil {
				out = make(map[string]string)
			}
			out[ref] = v.text
		}
	}
	return out
}
