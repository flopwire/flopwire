package retrieval

// SetBeforeRedactTx sets the hook that runs after a redaction picks its
// targets and before its transaction.
func SetBeforeRedactTx(f func()) { beforeRedactTx = f }
