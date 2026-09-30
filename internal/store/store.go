package store

import (
	"context"
	"errors"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Store is the identity, audit, and policy boundary the API needs.
//
// Every mutation that an administrator or device performs commits together
// with its audit event. Methods that take an authorizer credential ID lock
// that credential and fail with ErrConflict when it was revoked, expired, or
// deactivated after the request authenticated, so a revocation cannot race a
// privileged write.
type Store interface {
	// BootstrapIdentity creates the first human administrator; ErrConflict
	// when one already exists.
	BootstrapIdentity(context.Context, domain.User, domain.Credential, domain.AuditEvent) error
	UserByEmail(context.Context, string) (domain.User, error)
	UserByID(context.Context, string) (domain.User, error)
	ListUsers(context.Context) ([]domain.User, error)
	CreateInviteWithAudit(ctx context.Context, authorizer string, v domain.Invite, a domain.AuditEvent) error
	InviteByCodeHash(context.Context, string) (domain.Invite, error)
	// ClaimInviteIdentity claims the invite, creates the user and its first
	// session credential; ErrConflict when claimed, expired, or mismatched.
	ClaimInviteIdentity(ctx context.Context, codeHash string, now time.Time, u domain.User, c domain.Credential, a domain.AuditEvent) error
	CreateCredentialWithAudit(context.Context, domain.Credential, domain.AuditEvent) error
	CredentialByTokenHash(context.Context, string) (domain.Credential, error)
	CreateDeviceWithCredential(ctx context.Context, authorizer string, d domain.Device, c domain.Credential, a domain.AuditEvent) error
	CreateServiceIdentity(ctx context.Context, authorizer string, u domain.User, d domain.Device, c domain.Credential, a domain.AuditEvent) error
	DeviceByID(context.Context, string) (domain.Device, error)
	// ListDevices lists the devices a sweep has not hidden, with the
	// owner's email and the live credential's expiry and scopes.
	ListDevices(context.Context) ([]domain.Device, error)
	// TouchDevice records that the device was seen at t from ip.
	TouchDevice(ctx context.Context, id string, t time.Time, ip string) error
	// RevokeDeviceWithAudit revokes the device, its credentials, the tokens
	// minted from it and their ephemeral devices.
	RevokeDeviceWithAudit(ctx context.Context, authorizer, deviceID string, at time.Time, a domain.AuditEvent) error
	// RevokeUserWithAudit revokes every credential and device of the user
	// and returns how many of each it revoked; ErrNotFound for no user.
	RevokeUserWithAudit(ctx context.Context, authorizer, userID string, at time.Time, a domain.AuditEvent) (credentials, devices int, err error)
	// ReauthDevice gives the user's enrolled device c.DeviceID the new
	// credential c after an interactive login (authorizer), revoking the
	// device's other credentials. ErrNotFound: not this user's device;
	// ErrForbidden: revoked, or not an enrolled device.
	ReauthDevice(ctx context.Context, authorizer string, c domain.Credential, a domain.AuditEvent) error
	// MintCredential stores a minted token (authorizer minted it).
	MintCredential(ctx context.Context, authorizer string, c domain.Credential, a domain.AuditEvent) error
	// RegisterEphemeralDevice binds a minted credential to the new device
	// d on first use and returns the bound device id (d's, or the one an
	// earlier first use registered).
	RegisterEphemeralDevice(ctx context.Context, credentialID string, d domain.Device, a domain.AuditEvent) (string, error)
	// SweepCredentials ends expired and idle credentials and hides
	// ephemeral devices whose credentials ended a grace period ago, each
	// with an audit event.
	SweepCredentials(ctx context.Context, now time.Time) (domain.CredentialSweep, error)
	// PrepareDeviceRotation stores an inactive replacement credential,
	// replacing any earlier prepared rotation of the same device.
	PrepareDeviceRotation(context.Context, domain.DeviceRotation, domain.Credential, domain.AuditEvent) error
	// CommitDeviceRotation activates the new credential and revokes the old
	// one. Committing an already committed rotation returns it unchanged.
	CommitDeviceRotation(ctx context.Context, id, commitHash string, now time.Time, a domain.AuditEvent) (domain.DeviceRotation, error)
	AppendAudit(context.Context, domain.AuditEvent) error
	ListAudit(context.Context, int) ([]domain.AuditEvent, error)
	CountAudit(context.Context) (int64, error)
	GetPolicy(context.Context) (domain.Policy, error)
	UpdatePolicyWithAudit(ctx context.Context, authorizer string, p domain.Policy, a domain.AuditEvent) error
	// Usage reports stored raw bytes: every chunk and provisional tail, and
	// the given user's share (chunks their devices reserved first, and
	// their sources' tails).
	Usage(context.Context, string) (totalBytes int64, userBytes int64, err error)
}
