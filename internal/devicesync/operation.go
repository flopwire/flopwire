package devicesync

// syncScratch belongs to the serial Syncer, not a logical operation. Borrow it
// only while Syncer.mu is held. A later serial call may reuse it between turns.
// Deferred bytes are content addressed; they carry no authorization or cursor.
type syncScratch struct {
	buf      []byte
	deferred part
}

// syncOperation fixes the authorization for one logical call. The entry point
// freezes authorization before construction; callback/lease ownership stays
// with its caller. This type owns neither source state nor a request budget.
type syncOperation struct {
	syncer        *Syncer
	authorization *CaptureAuthorization
	scratch       *syncScratch // association only; borrowing requires Syncer.mu
}

func (s *Syncer) operation(frozen *CaptureAuthorization) *syncOperation {
	return &syncOperation{syncer: s, authorization: frozen, scratch: &s.serialScratch}
}

// ordinaryOperation explicitly selects ordinary capture for private fixtures
// and ordinary entry points. It never consults ambient authorization state.
func (s *Syncer) ordinaryOperation() *syncOperation { return s.operation(nil) }
