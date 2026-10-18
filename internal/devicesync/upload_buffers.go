package devicesync

// uploadBuffers owns one request's compressed frames and complete tail backing.
// The transmitted tail suffix and payload.cur borrow these roots. Deferred
// frames belong to syncScratch until pack moves them into a request.
// Production access remains serialized by Syncer.mu.
type uploadBuffers struct {
	parts    []part
	tail     []byte
	tailFrom int64
}

// capacities derives selected retained application-buffer capacities. It does
// not include temporary raw reads, codec/redactor state, metadata, allocator
// overhead or capture/export work, and is neither peak memory nor admission.
func (b *uploadBuffers) capacities() (compressed, tail int64) {
	for _, p := range b.parts {
		compressed += int64(cap(p.z))
	}
	return compressed, int64(cap(b.tail))
}

func (b *uploadBuffers) clear() { *b = uploadBuffers{} }
