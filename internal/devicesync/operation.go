package devicesync

// syncScratch belongs to a fixed Syncer workspace, not a logical operation.
// Its admitted operation retains it through network waits and recapture.
// Capture and materialization still borrow it only under Syncer.mu.
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
	scratch       *syncScratch // fixed by operation admission; reads/materialization use Syncer.mu
}

func (s *Syncer) operation(frozen *CaptureAuthorization) *syncOperation {
	return s.operationWithScratch(frozen, &s.serialScratch)
}

// ordinaryOperation explicitly selects ordinary capture for private fixtures
// and ordinary entry points. It never consults ambient authorization state.
func (s *Syncer) ordinaryOperation() *syncOperation { return s.operation(nil) }

func (s *Syncer) operationWithScratch(frozen *CaptureAuthorization, scratch *syncScratch) *syncOperation {
	return &syncOperation{syncer: s, authorization: frozen, scratch: scratch}
}
