package store

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
)

var _ Store = (*Memory)(nil)
var _ Store = (*Postgres)(nil)

// Memory is an in-process Store for API tests. It mirrors the Postgres
// transaction semantics: each method checks and mutates under one lock.
type Memory struct {
	mu          sync.RWMutex
	users       map[string]domain.User
	invites     map[string]domain.Invite
	credentials map[string]domain.Credential // by token hash
	devices     map[string]domain.Device
	rotations   map[string]domain.DeviceRotation
	audit       []domain.AuditEvent
	policy      domain.Policy
}

func NewMemory() *Memory {
	return &Memory{
		users: map[string]domain.User{}, invites: map[string]domain.Invite{},
		credentials: map[string]domain.Credential{}, devices: map[string]domain.Device{},
		rotations: map[string]domain.DeviceRotation{},
	}
}

func (m *Memory) BootstrapIdentity(_ context.Context, u domain.User, c domain.Credential, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.users {
		if existing.Role == domain.RoleAdmin && existing.IdentityType == domain.IdentityHuman {
			return ErrConflict
		}
	}
	if err := m.addUserLocked(u); err != nil {
		return err
	}
	m.credentials[c.TokenHash] = c
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) addUserLocked(u domain.User) error {
	for _, existing := range m.users {
		if u.Email != "" && strings.EqualFold(existing.Email, u.Email) {
			return ErrConflict
		}
	}
	m.users[u.ID] = u
	return nil
}
func (m *Memory) UserByEmail(_ context.Context, email string) (domain.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return domain.User{}, ErrNotFound
}
func (m *Memory) UserByID(_ context.Context, id string) (domain.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return domain.User{}, ErrNotFound
	}
	return u, nil
}
func (m *Memory) ListUsers(context.Context) ([]domain.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.User, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
func (m *Memory) CreateInviteWithAudit(_ context.Context, authorizer string, v domain.Invite, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.activeCredentialLocked(authorizer, time.Now().UTC()); err != nil {
		return err
	}
	m.invites[v.CodeHash] = v
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) InviteByCodeHash(_ context.Context, h string) (domain.Invite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.invites[h]
	if !ok {
		return domain.Invite{}, ErrNotFound
	}
	return v, nil
}
func (m *Memory) ClaimInviteIdentity(_ context.Context, h string, now time.Time, u domain.User, c domain.Credential, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.invites[h]
	if !ok {
		return ErrNotFound
	}
	if !v.ClaimedAt.IsZero() || !now.Before(v.ExpiresAt) || !strings.EqualFold(v.Email, u.Email) || v.Role != u.Role {
		return ErrConflict
	}
	if err := m.addUserLocked(u); err != nil {
		return err
	}
	v.ClaimedAt = now
	m.invites[h] = v
	m.credentials[c.TokenHash] = c
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) CreateCredentialWithAudit(_ context.Context, c domain.Credential, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.credentials[c.TokenHash] = c
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) CredentialByTokenHash(_ context.Context, h string) (domain.Credential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.credentials[h]
	if !ok {
		return domain.Credential{}, ErrNotFound
	}
	return c, nil
}
func (m *Memory) CreateDeviceWithCredential(_ context.Context, authorizer string, d domain.Device, c domain.Credential, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.activeCredentialLocked(authorizer, time.Now().UTC()); err != nil {
		return err
	}
	if d.Kind == "" {
		d.Kind = domain.DeviceEnrolled
	}
	m.devices[d.ID] = d
	m.credentials[c.TokenHash] = c
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) CreateServiceIdentity(_ context.Context, authorizer string, u domain.User, d domain.Device, c domain.Credential, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.activeCredentialLocked(authorizer, time.Now().UTC()); err != nil {
		return err
	}
	if err := m.addUserLocked(u); err != nil {
		return err
	}
	m.devices[d.ID] = d
	m.credentials[c.TokenHash] = c
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) DeviceByID(_ context.Context, id string) (domain.Device, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.devices[id]
	if !ok {
		return domain.Device{}, ErrNotFound
	}
	return d, nil
}
func (m *Memory) ListDevices(context.Context) ([]domain.Device, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.Device, 0, len(m.devices))
	for _, d := range m.devices {
		if !d.SweptAt.IsZero() {
			continue
		}
		u := m.users[d.UserID]
		d.UserEmail = u.Email
		var live *domain.Credential
		for _, c := range m.credentials {
			if c.DeviceID == d.ID && c.Active && c.RevokedAt.IsZero() && (live == nil || c.CreatedAt.After(live.CreatedAt)) {
				live = &c
			}
		}
		if live != nil {
			listing(&d, *live, u.IdentityType)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
func (m *Memory) TouchDevice(_ context.Context, id string, t time.Time, ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	if !ok {
		return ErrNotFound
	}
	if t.After(d.LastSeen) {
		d.LastSeen = t
	}
	d.LastIP = ip
	m.devices[id] = d
	return nil
}
func (m *Memory) RevokeDeviceWithAudit(_ context.Context, authorizer, id string, t time.Time, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.activeCredentialLocked(authorizer, t); err != nil {
		return err
	}
	d, ok := m.devices[id]
	if !ok || !d.RevokedAt.IsZero() {
		return ErrNotFound
	}
	d.RevokedAt = t
	m.devices[id] = d
	n := 0
	for hash, c := range m.credentials {
		if (c.DeviceID == id || c.MintedFromDevice == id) && c.RevokedAt.IsZero() {
			c.RevokedAt, c.RevokeReason = t, domain.RevokeRevoked
			m.credentials[hash] = c
			n++
		}
		if c.MintedFromDevice == id && c.DeviceID != "" {
			if e := m.devices[c.DeviceID]; e.RevokedAt.IsZero() {
				e.RevokedAt = t
				m.devices[c.DeviceID] = e
			}
		}
	}
	a.Metadata = withMeta(a.Metadata, "credentials", int64(n))
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) RevokeUserWithAudit(_ context.Context, authorizer, userID string, t time.Time, a domain.AuditEvent) (int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.activeCredentialLocked(authorizer, t); err != nil {
		return 0, 0, err
	}
	if _, ok := m.users[userID]; !ok {
		return 0, 0, ErrNotFound
	}
	creds, devices := 0, 0
	for hash, c := range m.credentials {
		if c.UserID == userID && c.RevokedAt.IsZero() {
			c.RevokedAt, c.RevokeReason = t, domain.RevokePrincipal
			m.credentials[hash] = c
			creds++
		}
	}
	for id, d := range m.devices {
		if d.UserID == userID && d.RevokedAt.IsZero() {
			d.RevokedAt = t
			m.devices[id] = d
			devices++
		}
	}
	a.Metadata = withMeta(withMeta(a.Metadata, "credentials", int64(creds)), "devices", int64(devices))
	m.audit = append(m.audit, a)
	return creds, devices, nil
}
func (m *Memory) ReauthDevice(_ context.Context, authorizer string, c domain.Credential, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.activeCredentialLocked(authorizer, c.CreatedAt); err != nil {
		return err
	}
	d, ok := m.devices[c.DeviceID]
	if !ok || d.UserID != c.UserID {
		return ErrNotFound
	}
	if !d.RevokedAt.IsZero() || (d.Kind != "" && d.Kind != domain.DeviceEnrolled) {
		return ErrForbidden
	}
	for id, r := range m.rotations {
		if r.DeviceID == c.DeviceID && r.State == domain.DeviceRotationPrepared {
			if stale, ok := m.credentialByIDLocked(r.NewCredentialID); ok {
				delete(m.credentials, stale.TokenHash)
			}
			delete(m.rotations, id)
		}
	}
	for hash, old := range m.credentials {
		if (old.DeviceID == c.DeviceID || old.MintedFromDevice == c.DeviceID) && old.RevokedAt.IsZero() {
			old.RevokedAt, old.RevokeReason = c.CreatedAt, domain.RevokeReauth
			m.credentials[hash] = old
		}
	}
	d.LastSeen = c.CreatedAt
	m.devices[d.ID] = d
	m.credentials[c.TokenHash] = c
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) MintCredential(_ context.Context, authorizer string, c domain.Credential, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.activeCredentialLocked(authorizer, c.CreatedAt); err != nil {
		return err
	}
	m.credentials[c.TokenHash] = c
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) RegisterEphemeralDevice(_ context.Context, credentialID string, d domain.Device, a domain.AuditEvent) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.credentialByIDLocked(credentialID)
	if !ok {
		return "", ErrNotFound
	}
	if c.Kind != domain.CredentialMinted {
		return "", ErrConflict
	}
	if c.DeviceID != "" {
		return c.DeviceID, nil
	}
	m.devices[d.ID] = d
	c.DeviceID = d.ID
	m.credentials[c.TokenHash] = c
	m.audit = append(m.audit, a)
	return d.ID, nil
}
func (m *Memory) SweepCredentials(_ context.Context, now time.Time) (domain.CredentialSweep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out domain.CredentialSweep
	for hash, c := range m.credentials {
		if !c.RevokedAt.IsZero() || c.Kind == domain.CredentialSession {
			continue
		}
		reason := ""
		if !c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now) {
			reason = domain.RevokeExpired
			out.Expired++
		} else if d, ok := m.devices[c.DeviceID]; ok && c.Kind == domain.CredentialDevice && (d.Kind == "" || d.Kind == domain.DeviceEnrolled) {
			seen := d.LastSeen
			if seen.IsZero() {
				seen = d.CreatedAt
			}
			if !seen.After(now.Add(-domain.DeviceIdleAfter)) {
				reason = domain.RevokeIdle
				out.Idle++
			}
		}
		if reason == "" {
			continue
		}
		c.RevokedAt, c.RevokeReason = now, reason
		m.credentials[hash] = c
		ev := expireEvent(now, c.ID, c.UserID, c.Kind, c.Label, reason)
		ev.DeviceID = c.DeviceID
		m.audit = append(m.audit, ev)
	}
	for id, d := range m.devices {
		if d.Kind != domain.DeviceEphemeral || !d.SweptAt.IsZero() {
			continue
		}
		var ended time.Time
		live := false
		for _, c := range m.credentials {
			if c.DeviceID != id {
				continue
			}
			end := c.RevokedAt
			if end.IsZero() || (!c.ExpiresAt.IsZero() && c.ExpiresAt.Before(end)) {
				end = c.ExpiresAt
			}
			if c.RevokedAt.IsZero() && (c.ExpiresAt.IsZero() || c.ExpiresAt.After(now)) {
				live = true
			}
			if end.After(ended) {
				ended = end
			}
		}
		if ended.IsZero() {
			ended = d.CreatedAt
		}
		if live || ended.After(now.Add(-domain.EphemeralSweepGrace)) {
			continue
		}
		d.SweptAt = now
		m.devices[id] = d
		m.audit = append(m.audit, sweepEvent(now, id, d.UserID, d.Label))
		out.Swept++
	}
	return out, nil
}
func (m *Memory) PrepareDeviceRotation(_ context.Context, r domain.DeviceRotation, c domain.Credential, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.credentialByIDLocked(r.OldCredentialID)
	if !ok || old.DeviceID != r.DeviceID || !old.Active || !old.RevokedAt.IsZero() {
		return ErrConflict
	}
	for id, prior := range m.rotations {
		if prior.DeviceID == r.DeviceID && prior.State == domain.DeviceRotationPrepared {
			if stale, ok := m.credentialByIDLocked(prior.NewCredentialID); ok {
				delete(m.credentials, stale.TokenHash)
			}
			delete(m.rotations, id)
		}
	}
	m.credentials[c.TokenHash] = c
	m.rotations[r.ID] = r
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) CommitDeviceRotation(_ context.Context, id, commitHash string, now time.Time, a domain.AuditEvent) (domain.DeviceRotation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rotations[id]
	if !ok || r.CommitTokenHash != commitHash {
		return domain.DeviceRotation{}, ErrNotFound
	}
	if r.State == domain.DeviceRotationCommitted {
		return r, nil
	}
	old, _ := m.credentialByIDLocked(r.OldCredentialID)
	next, _ := m.credentialByIDLocked(r.NewCredentialID)
	device := m.devices[r.DeviceID]
	if !device.RevokedAt.IsZero() || !old.RevokedAt.IsZero() || !now.Before(r.ExpiresAt) {
		return r, ErrConflict
	}
	next.Active = true
	m.credentials[next.TokenHash] = next
	old.RevokedAt, old.RevokeReason = now, domain.RevokeRotated
	m.credentials[old.TokenHash] = old
	r.State, r.CommittedAt = domain.DeviceRotationCommitted, now
	m.rotations[id] = r
	a.ActorID, a.DeviceID, a.TargetID = device.UserID, r.DeviceID, r.DeviceID
	m.audit = append(m.audit, a)
	return r, nil
}
func (m *Memory) credentialByIDLocked(id string) (domain.Credential, bool) {
	for _, c := range m.credentials {
		if c.ID == id {
			return c, true
		}
	}
	return domain.Credential{}, false
}
func (m *Memory) activeCredentialLocked(id string, now time.Time) error {
	c, ok := m.credentialByIDLocked(id)
	if !ok || !c.Active || !c.RevokedAt.IsZero() || (!c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt)) {
		return ErrConflict
	}
	return nil
}
func (m *Memory) AppendAudit(_ context.Context, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, a)
	return nil
}
func (m *Memory) ListAudit(_ context.Context, limit int) ([]domain.AuditEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := append([]domain.AuditEvent(nil), m.audit...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (m *Memory) CountAudit(context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.audit)), nil
}
func (m *Memory) GetPolicy(context.Context) (domain.Policy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.policy, nil
}
func (m *Memory) UpdatePolicyWithAudit(_ context.Context, authorizer string, p domain.Policy, a domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.activeCredentialLocked(authorizer, time.Now().UTC()); err != nil {
		return err
	}
	m.policy = p
	m.audit = append(m.audit, a)
	return nil
}

// Usage is always zero: the memory store holds identity and audit only. Raw
// evidence (chunks, tails) lives in Postgres and S3.
func (m *Memory) Usage(context.Context, string) (int64, int64, error) { return 0, 0, nil }
