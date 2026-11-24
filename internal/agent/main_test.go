package agent

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Synthetic agent tests must never discover the developer's conversations.
	os.Setenv("FLOPWIRE_ANTIGRAVITY_ROOT", "-")
	os.Exit(m.Run())
}
