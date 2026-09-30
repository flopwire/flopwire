package domain

import "time"

type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

type IdentityType string

const (
	IdentityHuman   IdentityType = "human"
	IdentityService IdentityType = "service"
)

type User struct {
	ID           string       `json:"id"`
	Email        string       `json:"email,omitempty"`
	Name         string       `json:"name"`
	Role         Role         `json:"role"`
	IdentityType IdentityType `json:"identity_type"`
	PasswordHash string       `json:"-"`
	Disabled     bool         `json:"disabled"`
	CreatedAt    time.Time    `json:"created_at"`
}

type Invite struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      Role      `json:"role"`
	CodeHash  string    `json:"-"`
	CreatedBy string    `json:"created_by"`
	ExpiresAt time.Time `json:"expires_at"`
	ClaimedAt time.Time `json:"claimed_at,omitempty"`
}

type CredentialKind string

const (
	CredentialSession CredentialKind = "session"
	CredentialDevice  CredentialKind = "device"
	// CredentialMinted is a short-lived scoped token minted by a device or
	// login session (`flopwire token mint`) for a sandbox or CI job.
	CredentialMinted CredentialKind = "minted"
)

// Scopes a credential grants. A minted token names its scopes; the other
// kinds default by kind (DefaultScopes).
const (
	ScopeUpload = "upload"
	ScopeRead   = "read"
)

// Credential lifetimes (SECURITY.md, Credentials).
const (
	// DeviceReauthAfter is how long an enrolled device credential lives
	// from the interactive login that issued it. Rotation keeps the
	// deadline; only `flopwire login` resets it.
	DeviceReauthAfter = 90 * 24 * time.Hour
	// DeviceIdleAfter: a device unseen this long must log in again.
	DeviceIdleAfter = 30 * 24 * time.Hour
	// DeviceRotateEvery is how often the agent rotates its credential.
	DeviceRotateEvery = 24 * time.Hour
	// CredentialWarnBefore: the agent warns this long before a deadline.
	CredentialWarnBefore = 7 * 24 * time.Hour
	// MintDefaultTTL is a minted token's TTL when none is asked for;
	// MintMaxTTLDefault caps it unless the policy sets another cap.
	MintDefaultTTL    = time.Hour
	MintMaxTTLDefault = 24 * time.Hour
	// EphemeralSweepGrace: an ephemeral device leaves the device list this
	// long after its last credential ended.
	EphemeralSweepGrace = 24 * time.Hour
)

// Revocation reasons (credentials.revoke_reason).
const (
	RevokeRotated   = "rotated"
	RevokeRevoked   = "revoked"
	RevokeReauth    = "reauth"
	RevokeExpired   = "expired"
	RevokeIdle      = "idle"
	RevokePrincipal = "principal_revoked"
)

type Credential struct {
	ID        string         `json:"id"`
	UserID    string         `json:"user_id"`
	DeviceID  string         `json:"device_id,omitempty"`
	Kind      CredentialKind `json:"kind"`
	TokenHash string         `json:"-"`
	// Scopes is empty for the kind's default (DefaultScopes).
	Scopes []string `json:"scopes,omitempty"`
	Label  string   `json:"label,omitempty"`
	// MintedFromDevice is the device whose credential minted this token;
	// revoking that device revokes it.
	MintedFromDevice string    `json:"minted_from_device,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	RevokedAt        time.Time `json:"revoked_at,omitempty"`
	RevokeReason     string    `json:"revoke_reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	// Active is false for a device credential issued by a prepared rotation
	// until the rotation commits.
	Active bool `json:"active"`
}

// DefaultScopes is what a credential of kind grants when it names no
// scopes. A session reads (and administers, for an admin); it never
// uploads: sync needs a device.
func DefaultScopes(kind CredentialKind, identity IdentityType) []string {
	switch {
	case kind == CredentialSession:
		return []string{ScopeRead}
	case identity == IdentityService:
		return []string{ScopeUpload}
	case kind == CredentialDevice:
		return []string{ScopeUpload, ScopeRead}
	}
	return nil
}

// EffectiveScopes is what the credential grants a user of identity. A
// service identity never reads, whatever its credential names.
func (c Credential) EffectiveScopes(identity IdentityType) []string {
	scopes := c.Scopes
	if len(scopes) == 0 {
		scopes = DefaultScopes(c.Kind, identity)
	}
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if s == ScopeRead && identity == IdentityService {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Allows reports whether the credential grants scope to a user of identity.
func (c Credential) Allows(scope string, identity IdentityType) bool {
	for _, s := range c.EffectiveScopes(identity) {
		if s == scope {
			return true
		}
	}
	return false
}

type DeviceRotationState string

const (
	DeviceRotationPrepared  DeviceRotationState = "prepared"
	DeviceRotationCommitted DeviceRotationState = "committed"
)

// DeviceRotation replaces a device credential in two steps so a client that
// crashes between steps keeps a working credential.
type DeviceRotation struct {
	ID              string              `json:"id"`
	DeviceID        string              `json:"device_id"`
	OldCredentialID string              `json:"old_credential_id"`
	NewCredentialID string              `json:"new_credential_id"`
	CommitTokenHash string              `json:"-"`
	State           DeviceRotationState `json:"state"`
	ExpiresAt       time.Time           `json:"expires_at"`
	CreatedAt       time.Time           `json:"created_at"`
	CommittedAt     time.Time           `json:"committed_at,omitempty"`
}

type DeviceKind string

const (
	// DeviceEnrolled is a laptop enrolled with `flopwire enroll`.
	DeviceEnrolled DeviceKind = "device"
	// DeviceEphemeral registers itself on the first use of a minted token.
	DeviceEphemeral DeviceKind = "ephemeral"
	// DeviceService is a service identity's upload device.
	DeviceService DeviceKind = "service"
)

type Device struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Name      string     `json:"name"`
	Platform  string     `json:"platform"`
	Kind      DeviceKind `json:"kind"`
	Label     string     `json:"label,omitempty"`
	LastSeen  time.Time  `json:"last_seen_at,omitempty"`
	LastIP    string     `json:"last_ip,omitempty"`
	RevokedAt time.Time  `json:"revoked_at,omitempty"`
	SweptAt   time.Time  `json:"swept_at,omitzero"`
	CreatedAt time.Time  `json:"created_at"`
	// The admin device list fills these: the owner's email, and the
	// device's live credential's expiry and scopes.
	UserEmail string    `json:"user_email,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	Scopes    []string  `json:"scopes,omitempty"`
}

// CredentialSweep is what one sweep of expired credentials did.
type CredentialSweep struct {
	Expired int `json:"expired"`
	Idle    int `json:"idle"`
	Swept   int `json:"swept"`
}

type AuditEvent struct {
	ID         string         `json:"id"`
	ActorID    string         `json:"actor_id,omitempty"`
	DeviceID   string         `json:"device_id,omitempty"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type,omitempty"`
	TargetID   string         `json:"target_id,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

type Policy struct {
	MaxStorageBytes int64    `json:"max_storage_bytes"`
	MaxUserBytes    int64    `json:"max_user_bytes"`
	PathRules       []string `json:"path_rules"`
	// Unplaceable is the floor for sessions with no working directory,
	// repo root or remote: "" (no floor), "local", "upload" or "exclude".
	Unplaceable string `json:"unplaceable"`
	// MaxTokenTTLSeconds caps `flopwire token mint`; 0 means
	// MintMaxTTLDefault.
	MaxTokenTTLSeconds int64     `json:"max_token_ttl_seconds"`
	UpdatedBy          string    `json:"updated_by,omitempty"`
	UpdatedAt          time.Time `json:"updated_at,omitempty"`
}

// MaxTokenTTL is the policy's cap on a minted token's TTL.
func (p Policy) MaxTokenTTL() time.Duration {
	if p.MaxTokenTTLSeconds <= 0 {
		return MintMaxTTLDefault
	}
	return time.Duration(p.MaxTokenTTLSeconds) * time.Second
}

type DeletionState string

const (
	DeletionQueued    DeletionState = "queued"
	DeletionPurging   DeletionState = "purging"
	DeletionRetryWait DeletionState = "retry_wait"
	DeletionComplete  DeletionState = "complete"
	DeletionFailed    DeletionState = "failed"
)

// DeletionJob purges the raw evidence of one deleted conversation. The
// conversation's rows are gone when the job is created; the job removes
// sources nothing else references and the chunks they alone used.
type DeletionJob struct {
	ID             string        `json:"id"`
	ConversationID string        `json:"conversation_id"`
	DeviceID       string        `json:"device_id"`
	Agent          string        `json:"agent"`
	SessionID      string        `json:"session_id"`
	RequestedBy    string        `json:"requested_by"`
	State          DeletionState `json:"state"`
	Attempts       int           `json:"attempts"`
	NextAttemptAt  time.Time     `json:"next_attempt_at,omitempty"`
	LastError      string        `json:"last_error,omitempty"`
	RequestedAt    time.Time     `json:"requested_at"`
	CompletedAt    time.Time     `json:"completed_at,omitempty"`
}

// QuarantinedSource is a source whose parse failed too many times in a row
// and is no longer retried until an administrator releases it. Its raw
// evidence is stored; only its message rows are behind.
// RedactionStatus is the deployment's redaction record for the admin
// status (notes/redaction.md): counts only, never a matched value.
type RedactionStatus struct {
	// Device: secrets devices masked before upload, per rule, summed over
	// every source's latest generation.
	Device map[string]int64 `json:"device"`
	// Server: secrets the server's own pass masked at parse, per rule.
	// Nonzero means bytes arrived unredacted.
	Server map[string]int64 `json:"server"`
	// SourcesRedacted: sources whose latest generation had a secret masked
	// on the device.
	SourcesRedacted int64 `json:"sources_redacted"`
	// SourcesUnredacted: sources whose latest generation was uploaded by a
	// device that did not redact.
	SourcesUnredacted int64 `json:"sources_unredacted"`
	// Rules: the rule sets devices reported, with source counts.
	Rules map[string]int64 `json:"rules"`
}

type QuarantinedSource struct {
	SourceID      string    `json:"source_id"`
	Path          string    `json:"path"`
	Agent         string    `json:"agent"`
	DeviceID      string    `json:"device_id"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error"`
	QuarantinedAt time.Time `json:"quarantined_at"`
}

// HiddenSummary previews what a path-rule change hid (D18): stored
// sessions the rules now cover, kept out of retrieval until an
// administrator confirms their purge, the hide expires into one, or a rule
// change restores them. A session counts once, by the conversation the
// rule matched.
type HiddenSummary struct {
	Total  int64         `json:"total"`
	ByUser []HiddenCount `json:"by_user"`
	ByRule []HiddenCount `json:"by_rule"`
	// OldestHiddenAt is when the longest-hidden session was hidden; it is
	// purged PurgeAfter later unless a rule change restores it first.
	OldestHiddenAt *time.Time `json:"oldest_hidden_at,omitempty"`
	PurgeAfter     string     `json:"purge_after"`
}

// HiddenCount is how many sessions one user has hidden, or one rule hid.
type HiddenCount struct {
	UserID   string `json:"user_id,omitempty"`
	Email    string `json:"email,omitempty"`
	Rule     string `json:"rule,omitempty"`
	Sessions int64  `json:"sessions"`
}
