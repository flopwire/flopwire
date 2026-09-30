package store

// Identity and audit transactions are ported from the CASS-era hardening
// stack (#6: 031cf6f, 4d2e741; #8: 62d5a64), minus the segment paths.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
)

type ObjectClient interface {
	PutObject(context.Context, string, string, io.Reader, int64, minio.PutObjectOptions) (minio.UploadInfo, error)
	GetObject(context.Context, string, string, minio.GetObjectOptions) (*minio.Object, error)
	RemoveObject(context.Context, string, string, minio.RemoveObjectOptions) error
	BucketExists(context.Context, string) (bool, error)
}

type Postgres struct {
	pool    *pgxpool.Pool
	objects ObjectClient
	bucket  string
}

func NewPostgres(pool *pgxpool.Pool, objects ObjectClient, bucket string) *Postgres {
	return &Postgres{pool: pool, objects: objects, bucket: bucket}
}

// bootstrapLockID serializes concurrent bootstrap attempts.
const bootstrapLockID int64 = 748326551

func (p *Postgres) BootstrapIdentity(ctx context.Context, u domain.User, c domain.Credential, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, bootstrapLockID); err != nil {
			return err
		}
		var admins int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE identity_type='human' AND role='admin'`).Scan(&admins); err != nil {
			return err
		}
		if admins != 0 {
			return ErrConflict
		}
		return insertAll(ctx, tx, insertUser(u), insertCredential(c), insertAudit(a))
	})
}

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.IdentityType, &u.PasswordHash, &u.Disabled, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return u, err
}

const userCols = `id,email,name,role,identity_type,password_hash,disabled,created_at`

func (p *Postgres) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	return scanUser(p.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE lower(email)=lower($1)`, email))
}
func (p *Postgres) UserByID(ctx context.Context, id string) (domain.User, error) {
	return scanUser(p.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}
func (p *Postgres) ListUsers(ctx context.Context) ([]domain.User, error) {
	return collect[domain.User](p.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at`))(scanUser)
}

func (p *Postgres) CreateInviteWithAudit(ctx context.Context, authorizer string, v domain.Invite, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockActiveCredential(ctx, tx, authorizer, time.Now().UTC()); err != nil {
			return err
		}
		return insertAll(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO invites(id,email,role,code_hash,created_by,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, v.ID, v.Email, v.Role, v.CodeHash, v.CreatedBy, v.ExpiresAt)
			return err
		}, insertAudit(a))
	})
}
func (p *Postgres) InviteByCodeHash(ctx context.Context, h string) (domain.Invite, error) {
	var v domain.Invite
	var claimed *time.Time
	err := p.pool.QueryRow(ctx, `SELECT id,email,role,code_hash,created_by,expires_at,claimed_at FROM invites WHERE code_hash=$1`, h).Scan(&v.ID, &v.Email, &v.Role, &v.CodeHash, &v.CreatedBy, &v.ExpiresAt, &claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if claimed != nil {
		v.ClaimedAt = *claimed
	}
	return v, err
}
func (p *Postgres) ClaimInviteIdentity(ctx context.Context, h string, now time.Time, u domain.User, c domain.Credential, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		var email string
		var role domain.Role
		var expires time.Time
		var claimed *time.Time
		err := tx.QueryRow(ctx, `SELECT email,role,expires_at,claimed_at FROM invites WHERE code_hash=$1 FOR UPDATE`, h).Scan(&email, &role, &expires, &claimed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if claimed != nil || !now.Before(expires) || !strings.EqualFold(email, u.Email) || role != u.Role {
			return ErrConflict
		}
		return insertAll(ctx, tx, insertUser(u), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE invites SET claimed_at=$2 WHERE code_hash=$1`, h, now)
			return err
		}, insertCredential(c), insertAudit(a))
	})
}
func (p *Postgres) CreateCredentialWithAudit(ctx context.Context, c domain.Credential, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error { return insertAll(ctx, tx, insertCredential(c), insertAudit(a)) })
}

const credentialCols = `id,user_id,device_id::text,kind,token_hash,scopes,label,minted_from_device::text,expires_at,revoked_at,revoke_reason,created_at,active`

func scanCredential(row pgx.Row) (domain.Credential, error) {
	var c domain.Credential
	var device, mintedFrom *string
	var expires, revoked *time.Time
	err := row.Scan(&c.ID, &c.UserID, &device, &c.Kind, &c.TokenHash, &c.Scopes, &c.Label, &mintedFrom, &expires, &revoked, &c.RevokeReason, &c.CreatedAt, &c.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if device != nil {
		c.DeviceID = *device
	}
	if mintedFrom != nil {
		c.MintedFromDevice = *mintedFrom
	}
	if expires != nil {
		c.ExpiresAt = *expires
	}
	if revoked != nil {
		c.RevokedAt = *revoked
	}
	if len(c.Scopes) == 0 {
		c.Scopes = nil
	}
	return c, err
}

func (p *Postgres) CredentialByTokenHash(ctx context.Context, h string) (domain.Credential, error) {
	return scanCredential(p.pool.QueryRow(ctx, `SELECT `+credentialCols+` FROM credentials WHERE token_hash=$1`, h))
}
func (p *Postgres) CreateDeviceWithCredential(ctx context.Context, authorizer string, d domain.Device, c domain.Credential, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockActiveCredential(ctx, tx, authorizer, time.Now().UTC()); err != nil {
			return err
		}
		return insertAll(ctx, tx, insertDevice(d), insertCredential(c), insertAudit(a))
	})
}
func (p *Postgres) CreateServiceIdentity(ctx context.Context, authorizer string, u domain.User, d domain.Device, c domain.Credential, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockActiveCredential(ctx, tx, authorizer, time.Now().UTC()); err != nil {
			return err
		}
		return insertAll(ctx, tx, insertUser(u), insertDevice(d), insertCredential(c), insertAudit(a))
	})
}

func scanDeviceInto(d *domain.Device, row pgx.Row, extra ...any) error {
	var seen, revoked, swept *time.Time
	err := row.Scan(append([]any{&d.ID, &d.UserID, &d.Name, &d.Platform, &d.Kind, &d.Label, &seen, &d.LastIP, &revoked, &swept, &d.CreatedAt}, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if seen != nil {
		d.LastSeen = *seen
	}
	if revoked != nil {
		d.RevokedAt = *revoked
	}
	if swept != nil {
		d.SweptAt = *swept
	}
	return err
}

func scanDevice(row pgx.Row) (domain.Device, error) {
	var d domain.Device
	err := scanDeviceInto(&d, row)
	return d, err
}

const deviceCols = `id,user_id,name,platform,kind,label,last_seen_at,last_ip,revoked_at,swept_at,created_at`

func (p *Postgres) DeviceByID(ctx context.Context, id string) (domain.Device, error) {
	return scanDevice(p.pool.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices WHERE id=$1`, id))
}

// ListDevices lists every device the sweep has not hidden, with its
// owner's email and its live credential's expiry and scopes.
func (p *Postgres) ListDevices(ctx context.Context) ([]domain.Device, error) {
	return collect[domain.Device](p.pool.Query(ctx, `SELECT d.id,d.user_id,d.name,d.platform,d.kind,d.label,d.last_seen_at,d.last_ip,d.revoked_at,d.swept_at,d.created_at,
			u.email,u.identity_type,c.kind,c.scopes,c.expires_at
		FROM devices d JOIN users u ON u.id=d.user_id
		LEFT JOIN LATERAL (SELECT kind,scopes,expires_at FROM credentials
			WHERE device_id=d.id AND active AND revoked_at IS NULL ORDER BY created_at DESC LIMIT 1) c ON true
		WHERE d.swept_at IS NULL ORDER BY d.created_at`))(func(row pgx.Row) (domain.Device, error) {
		var d domain.Device
		var identity domain.IdentityType
		var kind *domain.CredentialKind
		var scopes []string
		var expires *time.Time
		if err := scanDeviceInto(&d, row, &d.UserEmail, &identity, &kind, &scopes, &expires); err != nil {
			return d, err
		}
		if kind != nil {
			c := domain.Credential{Kind: *kind, Scopes: scopes}
			if expires != nil {
				c.ExpiresAt = *expires
			}
			listing(&d, c, identity)
		}
		return d, nil
	})
}

// listing fills a listed device's expiry and scopes from its live
// credential c. An enrolled device also expires when it has been idle for
// DeviceIdleAfter.
func listing(d *domain.Device, c domain.Credential, identity domain.IdentityType) {
	d.Scopes = c.EffectiveScopes(identity)
	d.ExpiresAt = c.ExpiresAt
	if d.Kind == domain.DeviceEnrolled {
		seen := d.LastSeen
		if seen.IsZero() {
			seen = d.CreatedAt
		}
		if idle := seen.Add(domain.DeviceIdleAfter); d.ExpiresAt.IsZero() || idle.Before(d.ExpiresAt) {
			d.ExpiresAt = idle
		}
	}
}

func (p *Postgres) TouchDevice(ctx context.Context, id string, t time.Time, ip string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE devices SET last_seen_at=GREATEST(last_seen_at,$2),last_ip=$3 WHERE id=$1`, id, t, ip)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// RevokeDeviceWithAudit revokes the device, its credentials, the tokens
// minted from it, and those tokens' ephemeral devices.
func (p *Postgres) RevokeDeviceWithAudit(ctx context.Context, authorizer, id string, t time.Time, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockActiveCredential(ctx, tx, authorizer, t); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE devices SET revoked_at=$2 WHERE id=$1 AND revoked_at IS NULL`, id, t)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err = tx.Exec(ctx, `UPDATE devices SET revoked_at=$2 WHERE revoked_at IS NULL AND id IN
			(SELECT device_id FROM credentials WHERE minted_from_device=$1 AND device_id IS NOT NULL)`, id, t); err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `UPDATE credentials SET revoked_at=$2,revoke_reason=$3 WHERE (device_id=$1 OR minted_from_device=$1) AND revoked_at IS NULL`, id, t, domain.RevokeRevoked)
		if err != nil {
			return err
		}
		a.Metadata = withMeta(a.Metadata, "credentials", tag.RowsAffected())
		return insertAudit(a)(ctx, tx)
	})
}

// RevokeUserWithAudit revokes every credential of the user (sessions,
// device credentials, minted tokens) and every device the user owns.
// The account stays: a human can log in again.
func (p *Postgres) RevokeUserWithAudit(ctx context.Context, authorizer, userID string, t time.Time, a domain.AuditEvent) (int, int, error) {
	var creds, devices int64
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockActiveCredential(ctx, tx, authorizer, t); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, userID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		tag, err := tx.Exec(ctx, `UPDATE credentials SET revoked_at=$2,revoke_reason=$3 WHERE user_id=$1 AND revoked_at IS NULL`, userID, t, domain.RevokePrincipal)
		if err != nil {
			return err
		}
		creds = tag.RowsAffected()
		if tag, err = tx.Exec(ctx, `UPDATE devices SET revoked_at=$2 WHERE user_id=$1 AND revoked_at IS NULL`, userID, t); err != nil {
			return err
		}
		devices = tag.RowsAffected()
		a.Metadata = withMeta(withMeta(a.Metadata, "credentials", creds), "devices", devices)
		return insertAudit(a)(ctx, tx)
	})
	return int(creds), int(devices), err
}

// ReauthDevice replaces the credentials of the user's enrolled device with
// c after an interactive login (authorizer is that login session). Every
// other credential of the device and every token minted from it is
// revoked, a prepared rotation is
// dropped, and the device counts as seen now. ErrNotFound: no such device
// of this user; ErrForbidden: the device was revoked or is not an enrolled
// device.
func (p *Postgres) ReauthDevice(ctx context.Context, authorizer string, c domain.Credential, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockActiveCredential(ctx, tx, authorizer, c.CreatedAt); err != nil {
			return err
		}
		var owner string
		var kind domain.DeviceKind
		var revoked *time.Time
		err := tx.QueryRow(ctx, `SELECT user_id::text,kind,revoked_at FROM devices WHERE id=$1 FOR UPDATE`, c.DeviceID).Scan(&owner, &kind, &revoked)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && owner != c.UserID {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if revoked != nil || kind != domain.DeviceEnrolled {
			return ErrForbidden
		}
		var previousNew *string
		err = tx.QueryRow(ctx, `DELETE FROM device_rotations WHERE device_id=$1 AND state='prepared' RETURNING new_credential_id::text`, c.DeviceID).Scan(&previousNew)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if previousNew != nil {
			if _, err = tx.Exec(ctx, `DELETE FROM credentials WHERE id=$1`, *previousNew); err != nil {
				return err
			}
		}
		// Tokens minted from the device die too: a thief who held the old
		// credential may have minted them.
		if _, err = tx.Exec(ctx, `UPDATE credentials SET revoked_at=$2,revoke_reason=$3 WHERE (device_id=$1 OR minted_from_device=$1) AND revoked_at IS NULL`, c.DeviceID, c.CreatedAt, domain.RevokeReauth); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE devices SET last_seen_at=$2 WHERE id=$1`, c.DeviceID, c.CreatedAt); err != nil {
			return err
		}
		return insertAll(ctx, tx, insertCredential(c), insertAudit(a))
	})
}

// MintCredential stores a minted token; authorizer is the minting
// credential, which must still be usable.
func (p *Postgres) MintCredential(ctx context.Context, authorizer string, c domain.Credential, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockActiveCredential(ctx, tx, authorizer, c.CreatedAt); err != nil {
			return err
		}
		return insertAll(ctx, tx, insertCredential(c), insertAudit(a))
	})
}

// RegisterEphemeralDevice binds a minted credential to a new ephemeral
// device d on its first use and returns the device id: d's, or the one an
// earlier (or concurrent) first use already registered.
func (p *Postgres) RegisterEphemeralDevice(ctx context.Context, credentialID string, d domain.Device, a domain.AuditEvent) (string, error) {
	id := d.ID
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		var device *string
		var kind domain.CredentialKind
		err := tx.QueryRow(ctx, `SELECT device_id::text,kind FROM credentials WHERE id=$1 FOR UPDATE`, credentialID).Scan(&device, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if kind != domain.CredentialMinted {
			return ErrConflict
		}
		if device != nil {
			id = *device
			return nil
		}
		return insertAll(ctx, tx, insertDevice(d), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE credentials SET device_id=$2 WHERE id=$1`, credentialID, d.ID)
			return err
		}, insertAudit(a))
	})
	return id, err
}

// SweepCredentials ends credentials that outlived their deadline, as of
// now: a device or minted credential past expires_at, and an enrolled
// device's credential after DeviceIdleAfter unseen. Each gets a
// credential.expire audit event. Then it hides (swept_at) ephemeral
// devices whose last credential ended EphemeralSweepGrace ago; the device
// row, its uploads and its audit events stay.
func (p *Postgres) SweepCredentials(ctx context.Context, now time.Time) (domain.CredentialSweep, error) {
	var out domain.CredentialSweep
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		out = domain.CredentialSweep{}
		var events []domain.AuditEvent
		ended := func(reason string, sql string, args ...any) (int, error) {
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return 0, err
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				var id, user string
				var device *string
				var kind domain.CredentialKind
				var label string
				if err := rows.Scan(&id, &user, &device, &kind, &label); err != nil {
					return 0, err
				}
				ev := expireEvent(now, id, user, kind, label, reason)
				if device != nil {
					ev.DeviceID = *device
				}
				events = append(events, ev)
				n++
			}
			return n, rows.Err()
		}
		var err error
		if out.Expired, err = ended(domain.RevokeExpired, `UPDATE credentials SET revoked_at=$1,revoke_reason=$2
			WHERE revoked_at IS NULL AND kind<>'session' AND expires_at<=$1
			RETURNING id::text,user_id::text,device_id::text,kind,label`, now, domain.RevokeExpired); err != nil {
			return err
		}
		if out.Idle, err = ended(domain.RevokeIdle, `UPDATE credentials c SET revoked_at=$1,revoke_reason=$2 FROM devices d
			WHERE c.device_id=d.id AND d.kind='device' AND c.kind='device' AND c.revoked_at IS NULL
			  AND COALESCE(d.last_seen_at,d.created_at)<=$3
			RETURNING c.id::text,c.user_id::text,c.device_id::text,c.kind,c.label`, now, domain.RevokeIdle, now.Add(-domain.DeviceIdleAfter)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE devices d SET swept_at=$1
			WHERE d.kind='ephemeral' AND d.swept_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM credentials c WHERE c.device_id=d.id AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at>$1))
			  AND COALESCE((SELECT max(LEAST(c.revoked_at,c.expires_at)) FROM credentials c WHERE c.device_id=d.id), d.revoked_at, d.created_at)<=$2
			RETURNING d.id::text,d.user_id::text,d.label`, now, now.Add(-domain.EphemeralSweepGrace))
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, user, label string
			if err := rows.Scan(&id, &user, &label); err != nil {
				rows.Close()
				return err
			}
			events = append(events, sweepEvent(now, id, user, label))
			out.Swept++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, ev := range events {
			if err := insertAudit(ev)(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// expireEvent and sweepEvent are the audit events a sweep writes. The
// server is the actor; the owner is in the metadata.
func expireEvent(now time.Time, credentialID, userID string, kind domain.CredentialKind, label, reason string) domain.AuditEvent {
	return domain.AuditEvent{ID: uuid.NewString(), Action: "credential.expire", TargetType: "credential", TargetID: credentialID,
		Metadata: map[string]any{"user_id": userID, "kind": kind, "label": label, "reason": reason}, CreatedAt: now}
}

func sweepEvent(now time.Time, deviceID, userID, label string) domain.AuditEvent {
	return domain.AuditEvent{ID: uuid.NewString(), Action: "device.sweep", TargetType: "device", TargetID: deviceID,
		Metadata: map[string]any{"user_id": userID, "label": label}, CreatedAt: now}
}

func withMeta(m map[string]any, k string, v any) map[string]any {
	if m == nil {
		m = map[string]any{}
	}
	m[k] = v
	return m
}

func (p *Postgres) PrepareDeviceRotation(ctx context.Context, rotation domain.DeviceRotation, c domain.Credential, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		var revoked *time.Time
		err := tx.QueryRow(ctx, `SELECT revoked_at FROM credentials WHERE id=$1 AND device_id=$2 AND active FOR UPDATE`, rotation.OldCredentialID, rotation.DeviceID).Scan(&revoked)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && revoked != nil {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		var previousNew *string
		err = tx.QueryRow(ctx, `DELETE FROM device_rotations WHERE device_id=$1 AND state='prepared' RETURNING new_credential_id::text`, rotation.DeviceID).Scan(&previousNew)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if previousNew != nil {
			if _, err = tx.Exec(ctx, `DELETE FROM credentials WHERE id=$1`, *previousNew); err != nil {
				return err
			}
		}
		return insertAll(ctx, tx, insertCredential(c), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO device_rotations(id,device_id,old_credential_id,new_credential_id,commit_token_hash,state,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, rotation.ID, rotation.DeviceID, rotation.OldCredentialID, rotation.NewCredentialID, rotation.CommitTokenHash, rotation.State, rotation.ExpiresAt, rotation.CreatedAt)
			return err
		}, insertAudit(a))
	})
}

func (p *Postgres) CommitDeviceRotation(ctx context.Context, id, commitHash string, now time.Time, a domain.AuditEvent) (domain.DeviceRotation, error) {
	var r domain.DeviceRotation
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		var userID string
		var committed, deviceRevoked, oldRevoked *time.Time
		err := tx.QueryRow(ctx, `SELECT r.id,r.device_id,r.old_credential_id,r.new_credential_id,r.commit_token_hash,r.state,r.expires_at,r.created_at,r.committed_at,d.user_id::text,d.revoked_at,c.revoked_at
			FROM device_rotations r JOIN devices d ON d.id=r.device_id JOIN credentials c ON c.id=r.old_credential_id
			WHERE r.id=$1 FOR UPDATE OF r,d,c`, id).Scan(&r.ID, &r.DeviceID, &r.OldCredentialID, &r.NewCredentialID, &r.CommitTokenHash, &r.State, &r.ExpiresAt, &r.CreatedAt, &committed, &userID, &deviceRevoked, &oldRevoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if r.CommitTokenHash != commitHash {
			return ErrNotFound
		}
		if r.State == domain.DeviceRotationCommitted {
			if committed != nil {
				r.CommittedAt = *committed
			}
			return nil
		}
		if deviceRevoked != nil || oldRevoked != nil || !now.Before(r.ExpiresAt) {
			return ErrConflict
		}
		for _, stmt := range []struct {
			sql  string
			args []any
		}{
			{`UPDATE credentials SET active=true WHERE id=$1`, []any{r.NewCredentialID}},
			{`UPDATE credentials SET revoked_at=$2,revoke_reason='rotated' WHERE id=$1`, []any{r.OldCredentialID, now}},
			{`UPDATE device_rotations SET state='committed',committed_at=$2 WHERE id=$1`, []any{id, now}},
		} {
			if _, err = tx.Exec(ctx, stmt.sql, stmt.args...); err != nil {
				return err
			}
		}
		r.State, r.CommittedAt = domain.DeviceRotationCommitted, now
		a.ActorID, a.DeviceID, a.TargetID = userID, r.DeviceID, r.DeviceID
		return insertAudit(a)(ctx, tx)
	})
	return r, err
}

func (p *Postgres) AppendAudit(ctx context.Context, a domain.AuditEvent) error {
	return p.inTx(ctx, func(tx pgx.Tx) error { return insertAudit(a)(ctx, tx) })
}
func (p *Postgres) ListAudit(ctx context.Context, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return collect[domain.AuditEvent](p.pool.Query(ctx, `SELECT id,actor_id::text,device_id::text,action,target_type,target_id,metadata,created_at FROM audit_events ORDER BY created_at DESC LIMIT $1`, limit))(func(row pgx.Row) (domain.AuditEvent, error) {
		var a domain.AuditEvent
		var actor, device *string
		var meta []byte
		if err := row.Scan(&a.ID, &actor, &device, &a.Action, &a.TargetType, &a.TargetID, &meta, &a.CreatedAt); err != nil {
			return a, err
		}
		if actor != nil {
			a.ActorID = *actor
		}
		if device != nil {
			a.DeviceID = *device
		}
		_ = json.Unmarshal(meta, &a.Metadata)
		return a, nil
	})
}
func (p *Postgres) CountAudit(ctx context.Context) (int64, error) {
	var count int64
	return count, p.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&count)
}
func (p *Postgres) GetPolicy(ctx context.Context) (domain.Policy, error) {
	var policy domain.Policy
	var raw []byte
	var updatedBy *string
	var updatedAt *time.Time
	err := p.pool.QueryRow(ctx, `SELECT max_storage_bytes,max_user_bytes,path_rules,unplaceable,max_token_ttl_seconds,updated_by::text,updated_at FROM collection_policy WHERE singleton`).Scan(&policy.MaxStorageBytes, &policy.MaxUserBytes, &raw, &policy.Unplaceable, &policy.MaxTokenTTLSeconds, &updatedBy, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy, nil
	}
	if err != nil {
		return policy, err
	}
	if updatedBy != nil {
		policy.UpdatedBy = *updatedBy
	}
	if updatedAt != nil {
		policy.UpdatedAt = *updatedAt
	}
	return policy, json.Unmarshal(raw, &policy.PathRules)
}
func (p *Postgres) UpdatePolicyWithAudit(ctx context.Context, authorizer string, policy domain.Policy, a domain.AuditEvent) error {
	raw, err := json.Marshal(policy.PathRules)
	if err != nil {
		return err
	}
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockActiveCredential(ctx, tx, authorizer, time.Now().UTC()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO collection_policy(singleton,max_storage_bytes,max_user_bytes,path_rules,unplaceable,updated_by,updated_at,max_token_ttl_seconds) VALUES(true,$1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(singleton) DO UPDATE SET max_storage_bytes=excluded.max_storage_bytes,max_user_bytes=excluded.max_user_bytes,path_rules=excluded.path_rules,unplaceable=excluded.unplaceable,updated_by=excluded.updated_by,updated_at=excluded.updated_at,max_token_ttl_seconds=excluded.max_token_ttl_seconds,
				rules_version=collection_policy.rules_version+CASE WHEN collection_policy.path_rules IS DISTINCT FROM excluded.path_rules OR collection_policy.unplaceable<>excluded.unplaceable THEN 1 ELSE 0 END`,
			policy.MaxStorageBytes, policy.MaxUserBytes, raw, policy.Unplaceable, nullable(policy.UpdatedBy), policy.UpdatedAt, policy.MaxTokenTTLSeconds)
		if err != nil {
			return err
		}
		return insertAudit(a)(ctx, tx)
	})
}
func (p *Postgres) Usage(ctx context.Context, userID string) (int64, int64, error) {
	var total, user int64
	err := p.pool.QueryRow(ctx, `SELECT (SELECT COALESCE(sum(bytes),0) FROM storage_usage)::bigint,
		COALESCE((SELECT bytes FROM storage_usage WHERE owner=$1),0)`, userID).Scan(&total, &user)
	return total, user, err
}

// inTx runs fn in a transaction and maps unique violations to ErrConflict.
func (p *Postgres) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err = fn(tx); err != nil {
		return classify(err)
	}
	return tx.Commit(ctx)
}

type txStep func(context.Context, pgx.Tx) error

func insertAll(ctx context.Context, tx pgx.Tx, steps ...txStep) error {
	for _, step := range steps {
		if err := step(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

func insertUser(u domain.User) txStep {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO users(id,email,name,role,identity_type,password_hash,disabled,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, u.ID, u.Email, u.Name, u.Role, u.IdentityType, u.PasswordHash, u.Disabled, u.CreatedAt)
		return err
	}
}
func insertDevice(d domain.Device) txStep {
	return func(ctx context.Context, tx pgx.Tx) error {
		kind := d.Kind
		if kind == "" {
			kind = domain.DeviceEnrolled
		}
		_, err := tx.Exec(ctx, `INSERT INTO devices(id,user_id,name,platform,kind,label,last_seen_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, d.ID, d.UserID, d.Name, d.Platform, kind, d.Label, nullTime(d.LastSeen), d.CreatedAt)
		return err
	}
}
func insertCredential(c domain.Credential) txStep {
	return func(ctx context.Context, tx pgx.Tx) error {
		var expires any
		if !c.ExpiresAt.IsZero() {
			expires = c.ExpiresAt
		}
		scopes := c.Scopes
		if scopes == nil {
			scopes = []string{}
		}
		_, err := tx.Exec(ctx, `INSERT INTO credentials(id,user_id,device_id,kind,token_hash,expires_at,created_at,active,scopes,label,minted_from_device) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, c.ID, c.UserID, nullable(c.DeviceID), c.Kind, c.TokenHash, expires, c.CreatedAt, c.Active, scopes, c.Label, nullable(c.MintedFromDevice))
		return err
	}
}
func insertAudit(a domain.AuditEvent) txStep {
	return func(ctx context.Context, tx pgx.Tx) error {
		meta, err := json.Marshal(a.Metadata)
		if err != nil {
			return err
		}
		if string(meta) == "null" {
			meta = []byte("{}")
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(id,actor_id,device_id,action,target_type,target_id,metadata,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, a.ID, nullable(a.ActorID), nullable(a.DeviceID), a.Action, a.TargetType, a.TargetID, meta, a.CreatedAt)
		return err
	}
}

// lockActiveCredential holds the authorizer's credential row for the rest of
// the transaction and rejects one that is no longer usable.
func lockActiveCredential(ctx context.Context, tx pgx.Tx, id string, now time.Time) error {
	var active bool
	var revoked, expires *time.Time
	err := tx.QueryRow(ctx, `SELECT active,revoked_at,expires_at FROM credentials WHERE id=$1 FOR UPDATE`, id).Scan(&active, &revoked, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if !active || revoked != nil || (expires != nil && !now.Before(*expires)) {
		return ErrConflict
	}
	return nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func collect[T any](rows pgx.Rows, err error) func(func(pgx.Row) (T, error)) ([]T, error) {
	return func(scan func(pgx.Row) (T, error)) ([]T, error) {
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []T
		for rows.Next() {
			v, err := scan(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "duplicate key") {
		return ErrConflict
	}
	return err
}
