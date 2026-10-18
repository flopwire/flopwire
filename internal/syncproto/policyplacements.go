package syncproto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	PathCapabilities         = "/v1/sync/capabilities"
	PathPolicyPlacements     = "/v1/sync/policyplacements"
	MaxPolicyPlacementsBytes = 1 << 20

	// Version 1 enforces durable device-bound policy placements for protected sources.
	PolicyPlacementsVersion = 1

	EvidenceNone     = "none"
	EvidenceMapped   = "mapped"
	EvidenceUnmapped = "unmapped"
	ClientModeAllow  = "allow"
	ClientModeLocal  = "local"
	ClientModeDeny   = "deny"
	ScopeLimitHeld   = "limit-held"
)

// CapabilitiesResponse describes enforced server features, not just accepted fields.
type CapabilitiesResponse struct {
	Version                 int `json:"version"`
	PolicyPlacementsVersion int `json:"policyplacements_version"`
	MaxConcurrentFlushes    int `json:"max_concurrent_flushes"`
}

// PolicyPlacement describes a verified host folder associated with a session.
type PolicyPlacement struct {
	CWD          string `json:"cwd"`
	WorktreeRoot string `json:"worktree_root"`
	MainRoot     string `json:"main_root"`
	Remote       string `json:"remote"`
}

// PolicySource identifies captured evidence on the authenticated device.
type PolicySource struct {
	Path       string `json:"path"`
	FileID     string `json:"file_id"`
	Generation int64  `json:"generation"`
}

// PolicyRecoverySource identifies an existing recovered copy, solely to add
// restrictions. Stored same-device native/path provenance must match; these
// references cannot establish historical folder grants or permit sharing.
type PolicyRecoverySource struct {
	Source       PolicySource `json:"source"`
	OriginalPath string       `json:"original_path"`
}

// PolicyPlacementsRequest adds session restrictions without uploading content.
// The server derives the user and device exclusively from the credential.
type PolicyPlacementsRequest struct {
	Version             int                    `json:"version"`
	Agent               string                 `json:"agent"`
	ParentSessionID     string                 `json:"parent_session_id,omitempty"`
	SessionID           string                 `json:"session_id"`
	CurrentMappingKnown bool                   `json:"current_mapping_known"`
	EvidenceScope       string                 `json:"evidence_scope"`
	Placements          []PolicyPlacement      `json:"placements"`
	Sources             []PolicySource         `json:"sources,omitempty"`
	RecoverySources     []PolicyRecoverySource `json:"recovery_sources,omitempty"`
	// ScopeStatus may request a compact restriction when full scope exceeds
	// transport limits. It cannot clear a previously committed limit hold.
	ScopeStatus string      `json:"scope_status,omitempty"`
	ClientMode  string      `json:"client_mode"`
	Device      *DeviceDirs `json:"device,omitempty"`
}

// PolicyPlacementsResponse acknowledges the durably reconciled restriction union.
// Allowed does not grant permission to send evidence omitted from the request.
type PolicyPlacementsResponse struct {
	Version       int    `json:"version"`
	Revision      int64  `json:"revision"`
	EvidenceScope string `json:"evidence_scope"`
	Allowed       bool   `json:"allowed"`
	RequestDigest string `json:"request_digest"`
}

// PolicyPlacementsDigest binds an acknowledgement to the exact submitted batch.
func PolicyPlacementsDigest(req *PolicyPlacementsRequest) (string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
