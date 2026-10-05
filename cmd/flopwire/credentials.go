package main

// Device credential upkeep (rotation, re-login state, the sync transport
// that follows a rotated token) and minted tokens (`flopwire token mint`).
// SECURITY.md, Credentials, describes the model.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/syncproto"
)

// reloginError is the server refusing the device credential: it was
// rotated by another holder, revoked, or expired. Only an interactive
// `flopwire login` fixes it, so it is permanent for sync.
type reloginError struct{ code string }

func (e *reloginError) Error() string {
	return "re-login required (" + e.code + "): run flopwire login"
}
func (e *reloginError) Permanent() bool { return true }

const reloginPrefix = "re-login required"

// markRelogin records in the config that the device credential was
// refused, for `flopwire agent status`. The caller holds the config lock.
func markRelogin(cfg client.Config, code string) error {
	if code == "" {
		code = "credential_invalid"
	}
	if !cfg.FromEnv && cfg.ReloginRequired != code {
		cfg.ReloginRequired = code
		if err := client.Save(cfg); err != nil {
			return fmt.Errorf("record re-login required: %w", err)
		}
	}
	return &reloginError{code: code}
}

func unauthorized(err error) (code string, ok bool) {
	var he *syncproto.HTTPError
	if errors.As(err, &he) && he.Status == http.StatusUnauthorized {
		return he.Body.Code, true
	}
	var ae *client.APIError
	if errors.As(err, &ae) && ae.StatusCode == http.StatusUnauthorized {
		return ae.Code, true
	}
	return "", false
}

// rotateCredentialLocked replaces the device credential in two steps
// (prepare, save, commit, save). The caller holds the config lock. A
// refused credential (401 on prepare) is recorded and returned as a
// reloginError: another holder rotated it first, or it was revoked or
// expired.
func rotateCredentialLocked(ctx context.Context, now func() time.Time) (map[string]any, error) {
	cfg, err := client.LoadFile()
	if err != nil {
		return nil, err
	}
	if cfg.DeviceID == "" {
		return nil, errors.New("config has no enrolled device")
	}
	httpClient := cfg.API(cfg.Token)
	for attempt := 0; attempt < 2; attempt++ {
		if cfg.PendingRotation != nil && (cfg.PendingRotation.State != "prepared" || cfg.PendingRotation.ExpiresAt.IsZero()) {
			cfg.PendingRotation = nil
			if err = client.Save(cfg); err != nil {
				return nil, fmt.Errorf("clear unusable rotation: %w", err)
			}
		}
		if cfg.PendingRotation == nil {
			var prepared struct {
				RotationID          string    `json:"rotation_id"`
				Token               string    `json:"token"`
				CommitToken         string    `json:"commit_token"`
				ExpiresAt           time.Time `json:"expires_at"`
				CredentialExpiresAt time.Time `json:"credential_expires_at"`
				IdleExpiresAt       time.Time `json:"idle_expires_at"`
			}
			if err = httpClient.JSON(ctx, "POST", "/v1/devices/"+cfg.DeviceID+"/rotation/prepare", map[string]string{}, &prepared); err != nil {
				if code, ok := unauthorized(err); ok {
					return nil, markRelogin(cfg, code)
				}
				return nil, err
			}
			if prepared.RotationID == "" || prepared.Token == "" || prepared.CommitToken == "" || prepared.ExpiresAt.IsZero() || !now().Before(prepared.ExpiresAt) {
				return nil, errors.New("server returned an incomplete rotation")
			}
			cfg.PendingRotation = &client.PendingRotation{ID: prepared.RotationID, NewToken: prepared.Token, CommitToken: prepared.CommitToken, State: "prepared", ExpiresAt: prepared.ExpiresAt,
				CredentialExpiresAt: prepared.CredentialExpiresAt, IdleExpiresAt: prepared.IdleExpiresAt}
			if err = client.Save(cfg); err != nil {
				return nil, fmt.Errorf("save prepared credential before commit: %w", err)
			}
		}
		var committed map[string]any
		if err = httpClient.JSON(ctx, "POST", "/v1/device-rotations/"+cfg.PendingRotation.ID+"/commit", map[string]string{"commit_token": cfg.PendingRotation.CommitToken}, &committed); err != nil {
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.Code == "rotation_invalid_or_expired" {
				cfg.PendingRotation = nil
				if saveErr := client.Save(cfg); saveErr != nil {
					return nil, fmt.Errorf("clear invalid rotation: %w", saveErr)
				}
				continue
			}
			return nil, fmt.Errorf("rotation prepared locally; rerun rotate-device to commit: %w", err)
		}
		pending := cfg.PendingRotation
		cfg.Token = pending.NewToken
		cfg.PendingRotation = nil
		cfg.RotatedAt, cfg.ReloginRequired = now().UTC(), ""
		if !pending.CredentialExpiresAt.IsZero() {
			cfg.CredentialExpiresAt = pending.CredentialExpiresAt
		}
		if !pending.IdleExpiresAt.IsZero() {
			cfg.IdleExpiresAt = pending.IdleExpiresAt
		}
		if err = client.Save(cfg); err != nil {
			return nil, fmt.Errorf("rotation committed; rerun rotate-device to finalize local config: %w", err)
		}
		return map[string]any{"rotation_id": pending.ID, "device_id": cfg.DeviceID, "state": "committed", "config": "saved with mode 0600"}, nil
	}
	return nil, errors.New("rotation could not be recovered")
}

// rotateLoop rotates the device credential every DeviceRotateEvery,
// checking each interval.
func rotateLoop(ctx context.Context, tr *syncTransport, interval time.Duration, now func() time.Time, log *slog.Logger) {
	rotateIfDue(ctx, tr, now, log)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rotateIfDue(ctx, tr, now, log)
		}
	}
}

// rotateIfDue rotates the saved device credential when its last rotation
// is DeviceRotateEvery old, and hands the new token to the sync transport.
// It does nothing once the server refused the credential: sync has
// stopped too, and `flopwire agent status` says a re-login is required. It
// reports whether it rotated.
func rotateIfDue(ctx context.Context, tr *syncTransport, now func() time.Time, log *slog.Logger) bool {
	cfg, err := client.LoadFile()
	if err != nil || cfg.DeviceID == "" || cfg.ReloginRequired != "" {
		return false
	}
	if !cfg.RotatedAt.IsZero() && now().Sub(cfg.RotatedAt) < domain.DeviceRotateEvery {
		return false
	}
	err = client.WithConfigLock(ctx, func() error {
		_, err := rotateCredentialLocked(ctx, now)
		return err
	})
	var relogin *reloginError
	switch {
	case err == nil:
		log.Info("agent: device credential rotated")
		if tr != nil {
			tr.reload(ctx)
		}
		return true
	case errors.As(err, &relogin):
		log.Error("agent: the server refused this device's credential; uploads stop until you run flopwire login", "err", err)
	case ctx.Err() == nil:
		log.Warn("agent: device credential rotation failed; retrying later", "err", err)
	}
	return false
}

// syncTransport is the agent's sync client. It follows the saved config:
// a rotated token or a new server pin is installed without a restart. A
// 401 re-reads the config under its lock (a rotation holds the lock
// through its commit and save), so a request that raced a rotation
// retries with the new token; a 401 with the saved token is a refused
// credential: sync stops (a permanent error) and the config records that a
// re-login is required. `flopwire login` saves a new token and asks the
// agent to recheck (Repin).
type syncTransport struct {
	mu     sync.Mutex
	cl     syncproto.Client
	pin    string // cl.HTTP's pin
	server string // normalized
	load   func() (client.Config, error)
	env    bool
	log    *slog.Logger
}

func newSyncTransport(cfg client.Config, load func() (client.Config, error), log *slog.Logger) *syncTransport {
	server, _ := client.NormalizeServer(cfg.Server)
	return &syncTransport{cl: syncproto.Client{Server: cfg.Server, Token: cfg.Token, HTTP: cfg.HTTPClient()}, pin: cfg.TLSFingerprint, server: server, load: load, env: cfg.FromEnv, log: log}
}

func (t *syncTransport) current() syncproto.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cl
}

func (t *syncTransport) Has(ctx context.Context, hashes []syncproto.Hash) ([]syncproto.Hash, error) {
	c := t.current()
	missing, err := c.Has(ctx, hashes)
	code, denied := unauthorized(err)
	if !denied {
		return missing, err
	}
	if t.refresh(ctx, c.Token) {
		c = t.current()
		return c.Has(ctx, hashes)
	}
	return nil, t.refused(ctx, c.Token, code)
}

func (t *syncTransport) Flush(ctx context.Context, req *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	c := t.current()
	resp, err := c.Flush(ctx, req)
	code, denied := unauthorized(err)
	if !denied {
		return resp, err
	}
	if t.refresh(ctx, c.Token) {
		// The payload was consumed: let the scheduler retry the flush.
		return nil, &syncproto.HTTPError{Status: http.StatusServiceUnavailable, Body: syncproto.ErrorResponse{Code: "credential_refreshed", Message: "the device credential changed; retrying"}}
	}
	return nil, t.refused(ctx, c.Token, code)
}

// refresh installs the saved token when it differs from used.
func (t *syncTransport) refresh(ctx context.Context, used string) bool {
	if t.env {
		return false
	}
	var cc client.Config
	var same bool
	if err := client.WithConfigLock(ctx, func() error { cc, same = savedConfig(t.server, t.load); return nil }); err != nil {
		return false
	}
	if !same || cc.Token == used {
		return false
	}
	t.install(cc)
	return true
}

// refused records that the saved token (used) was refused.
func (t *syncTransport) refused(ctx context.Context, used, code string) error {
	if t.env {
		if code == "" {
			code = "credential_invalid"
		}
		return &reloginError{code: code + "; FLOPWIRE_TOKEN was refused (expired or revoked): mint a new token"}
	}
	var out error = &reloginError{code: code}
	_ = client.WithConfigLock(ctx, func() error {
		if cc, same := savedConfig(t.server, t.load); same && cc.Token == used {
			out = markRelogin(cc, code)
		}
		return nil
	})
	return out
}

func (t *syncTransport) install(cc client.Config) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cl.Token = cc.Token
	if cc.TLSFingerprint != t.pin {
		t.cl.HTTP.CloseIdleConnections()
		t.cl.HTTP, t.pin = cc.HTTPClient(), cc.TLSFingerprint
	}
}

// reload installs the saved token after the agent rotated it.
func (t *syncTransport) reload(ctx context.Context) {
	_ = client.WithConfigLock(ctx, func() error {
		if cc, same := savedConfig(t.server, t.load); same && cc.Token != t.current().Token {
			t.install(cc)
		}
		return nil
	})
}

// repin is the scheduler's Repin: while sync is stopped, it re-reads the
// saved config and, when the pin or the token of the same server changed
// (`flopwire login` saved a new one), installs it and resumes.
func (t *syncTransport) repin() bool {
	cc, same := savedConfig(t.server, t.load)
	if cc.Server == "" {
		return false
	}
	if !same {
		t.log.Warn("agent: the client config names another server; restart the agent to sync with it", "server", cc.Server)
		return false
	}
	t.mu.Lock()
	unchanged := cc.Token == t.cl.Token && cc.TLSFingerprint == t.pin
	t.mu.Unlock()
	if unchanged {
		return false
	}
	t.install(cc)
	return true
}

// Credential sources, as setup --check and agent status name them.
const (
	credDevice = "device login"
	credEnv    = client.EnvToken
	credLegacy = "legacy login"
	credNone   = "none"
)

// credentialSource names the credential client.Load gives this process,
// says why it cannot use the server's message bus (with the fix), and
// warns when FLOPWIRE_TOKEN hides a saved device login. The precedence
// is not changed here: FLOPWIRE_TOKEN in the environment wins over the
// config file (flags > env > file, as gh with GH_TOKEN). A config that
// cannot be read is none.
func credentialSource(load, loadFile func() (client.Config, error)) agent.Credential {
	cc, err := load()
	switch {
	case err != nil || cc.Token == "":
		return agent.Credential{Source: credNone}
	case cc.FromEnv:
		c := agent.Credential{Source: credEnv, MessagingOff: "FLOPWIRE_TOKEN is a minted token, and messaging needs an enrolled device credential"}
		if f, ferr := loadFile(); ferr == nil && f.DeviceID != "" {
			c.Warning = "FLOPWIRE_TOKEN in the environment hides the device login saved for " + f.Server + "; unset FLOPWIRE_TOKEN to use it"
			c.MessagingOff += ": unset FLOPWIRE_TOKEN to use the saved device login"
		} else {
			c.MessagingOff += ": unset FLOPWIRE_TOKEN, then run flopwire login and flopwire enroll"
		}
		return c
	case cc.DeviceID == "":
		return agent.Credential{Source: credLegacy, MessagingOff: "this login has no device credential, and messaging needs one: run flopwire login, then flopwire enroll"}
	}
	return agent.Credential{Source: credDevice}
}

// credentialStatus is what `flopwire agent status` says about the
// credential: its source, then for a device login a refused credential, a
// deadline within CredentialWarnBefore, or ok; a messaging: off line when
// the source cannot use the bus; and the FLOPWIRE_TOKEN warning. cfg is
// the config the source came from.
func credentialStatus(w io.Writer, src agent.Credential, cfg client.Config, now time.Time) {
	line := "credential: " + src.Source
	switch {
	case src.Source == credEnv:
		line += " (a minted token: not rotated; mint a new one when it expires)"
	case src.Source != credDevice:
	case cfg.ReloginRequired != "":
		line += fmt.Sprintf("; re-login required (%s): the server refused this device's credential; uploads stopped. Run flopwire login", cfg.ReloginRequired)
	default:
		var due []string
		if d := cfg.CredentialExpiresAt; !d.IsZero() && d.Sub(now) <= domain.CredentialWarnBefore {
			due = append(due, "interactive re-login due by "+d.Local().Format(time.DateTime))
		}
		if d := cfg.IdleExpiresAt; !d.IsZero() && d.Sub(now) <= domain.CredentialWarnBefore {
			due = append(due, "idle limit reached at "+d.Local().Format(time.DateTime)+" unless the agent reaches the server")
		}
		if len(due) > 0 {
			line += fmt.Sprintf("; warning: %s (flopwire login)", strings.Join(due, "; "))
			break
		}
		line += "; ok"
		if !cfg.RotatedAt.IsZero() {
			line += "; rotated " + cfg.RotatedAt.Local().Format(time.DateTime)
		}
		if !cfg.CredentialExpiresAt.IsZero() {
			line += "; re-login due " + cfg.CredentialExpiresAt.Local().Format(time.DateOnly)
		}
	}
	fmt.Fprintln(w, line)
	if src.MessagingOff != "" {
		fmt.Fprintf(w, "messaging: off: %s\n", src.MessagingOff)
	}
	if src.Warning != "" {
		fmt.Fprintf(w, "warning: %s\n", src.Warning)
	}
}

// tokenCmd is `flopwire token mint`: a short-lived scoped token for a
// sandbox or CI job, minted by this device (or login session). It prints
// the token alone on stdout, so `export FLOPWIRE_TOKEN=$(flopwire token mint
// ...)` works, and the other variables the sandbox needs on stderr.
func tokenCmd(ctx context.Context, args []string) error {
	const use = "usage: flopwire token mint --label NAME [--ttl 1h] [--scope upload,read] [--json]"
	if len(args) == 0 || args[0] != "mint" {
		return errors.New(use)
	}
	fs := flag.NewFlagSet("token mint", flag.ContinueOnError)
	ttl := fs.Duration("ttl", domain.MintDefaultTTL, "lifetime (at most the policy's cap, 24h unless an admin changed it)")
	scope := fs.String("scope", "upload", "scopes: upload, read, or upload,read")
	label := fs.String("label", "", "name for the sandbox or job (shown in flopwire admin devices)")
	asJSON := fs.Bool("json", false, "print JSON, with the server and pin")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*label) == "" {
		return errors.New("--label is required: " + use)
	}
	if *ttl <= 0 {
		return errors.New("--ttl must be positive")
	}
	cfg, err := client.Load()
	if err != nil {
		return err
	}
	if cfg.FromEnv {
		return errors.New("a minted token (FLOPWIRE_TOKEN) cannot mint another; mint from an enrolled device")
	}
	var scopes []string
	for _, s := range strings.Split(*scope, ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopes = append(scopes, s)
		}
	}
	var out struct {
		Token        string    `json:"token"`
		CredentialID string    `json:"credential_id"`
		Label        string    `json:"label"`
		Scopes       []string  `json:"scopes"`
		ExpiresAt    time.Time `json:"expires_at"`
	}
	body := map[string]any{"ttl_seconds": int64(ttl.Seconds()), "scopes": scopes, "label": *label}
	if err := cfg.API(cfg.Token).JSON(ctx, "POST", "/v1/tokens", body, &out); err != nil {
		return client.TrustHint(err)
	}
	if *asJSON {
		res := map[string]any{"token": out.Token, "credential_id": out.CredentialID, "label": out.Label, "scopes": out.Scopes, "expires_at": out.ExpiresAt, "server": cfg.Server}
		if cfg.TLSFingerprint != "" {
			res["tls_fingerprint"] = cfg.TLSFingerprint
		}
		return printJSON(res)
	}
	fmt.Println(out.Token)
	fmt.Fprintf(os.Stderr, "minted %q (%s) until %s. In the sandbox set:\n  %s=<the token above>\n  %s=%s\n", out.Label, strings.Join(out.Scopes, ","), out.ExpiresAt.Local().Format(time.DateTime), client.EnvToken, client.EnvServer, cfg.Server)
	if cfg.TLSFingerprint != "" {
		fmt.Fprintf(os.Stderr, "  %s=%s\n", client.EnvFingerprint, cfg.TLSFingerprint)
	}
	return nil
}
