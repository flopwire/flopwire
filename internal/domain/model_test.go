package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSecretHashesAreRedactedFromJSON(t *testing.T) {
	values := []any{Credential{TokenHash: "credential-secret-hash"}, Invite{CodeHash: "invite-secret-hash"}, DeviceRotation{CommitTokenHash: "rotation-secret-hash"}}
	for _, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "secret-hash") {
			t.Fatalf("secret leaked from %T: %s", value, raw)
		}
	}
}
