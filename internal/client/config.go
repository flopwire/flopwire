package client

// Credential transitions (session vs device token, two-step rotation, a
// config lock around every transition) and server URL normalization are
// ported from the CASS-era stack (#6: 031cf6f, 4d2e741; #8: 62d5a64,
// 4bbf5fe, cb16d94).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type Config struct {
	Server       string `json:"server"`
	Token        string `json:"token"`
	SessionToken string `json:"session_token,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	// TLSFingerprint pins the server's certificate ("sha256:<hex>"); empty
	// means Web PKI verification. See NewHTTPClient.
	TLSFingerprint string   `json:"tls_fingerprint,omitempty"`
	Denylist       []string `json:"denylist,omitempty"`
	// Unplaceable is what the agent does with a session that has no
	// working directory, repo root or remote: "local" (the default: index,
	// never upload), "upload" or "exclude". The server's setting is a floor.
	Unplaceable string `json:"unplaceable,omitempty"`
	// Mode is the device agent's index mode: "full" (the default: a local
	// index to search, and upload when a server is configured) or
	// "sync-only" (upload only; local search needs --server).
	Mode            string           `json:"mode,omitempty"`
	PendingRotation *PendingRotation `json:"pending_rotation,omitempty"`
	// The device credential's deadlines as the server last reported them
	// (enroll, login, rotation): CredentialExpiresAt is the 90-day
	// interactive re-login, IdleExpiresAt the 30-day idle limit.
	// RotatedAt is the last rotation (or enrollment or login); the agent
	// rotates DeviceRotateEvery after it. ReloginRequired, when set, is
	// why the server refused the device credential (rotated elsewhere,
	// revoked, expired): the agent stopped and `flopwire login` is needed.
	CredentialExpiresAt time.Time `json:"credential_expires_at,omitzero"`
	IdleExpiresAt       time.Time `json:"idle_expires_at,omitzero"`
	RotatedAt           time.Time `json:"rotated_at,omitzero"`
	ReloginRequired     string    `json:"relogin_required,omitempty"`
	// FromEnv marks a config built from FLOPWIRE_TOKEN (a minted token in a
	// sandbox): it is never saved and never rotates.
	FromEnv bool `json:"-"`
}

// Environment for a sandbox or CI job that has a minted token and no
// config file: FLOPWIRE_TOKEN (the token), FLOPWIRE_SERVER (the server URL)
// and, for a self-signed server, FLOPWIRE_FINGERPRINT (its pin).
const (
	EnvToken       = "FLOPWIRE_TOKEN"
	EnvServer      = "FLOPWIRE_SERVER"
	EnvFingerprint = "FLOPWIRE_FINGERPRINT"
)

// SessionCredential returns the short-lived human login credential used for
// enrollment and administration. A pre-scope config without a device ID is a
// legacy login config; once a device ID exists, Token must be treated only as
// a device credential and never promoted back to session authority.
func (c Config) SessionCredential() (string, error) {
	if c.FromEnv {
		return "", errors.New("FLOPWIRE_TOKEN is a minted token; administrative commands need a login session (unset FLOPWIRE_TOKEN and run flopwire login)")
	}
	if c.SessionToken != "" {
		return c.SessionToken, nil
	}
	if c.DeviceID == "" && c.Token != "" {
		return c.Token, nil
	}
	return "", errors.New("a login session is required; run flopwire login")
}

type PendingRotation struct {
	ID          string    `json:"id"`
	NewToken    string    `json:"new_token"`
	CommitToken string    `json:"commit_token"`
	State       string    `json:"state"`
	ExpiresAt   time.Time `json:"expires_at"`
	// The deadlines the server reported with the rotation, saved at
	// commit.
	CredentialExpiresAt time.Time `json:"credential_expires_at,omitzero"`
	IdleExpiresAt       time.Time `json:"idle_expires_at,omitzero"`
}

func NormalizeServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("server must be an absolute http or https URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("server must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("server URL cannot contain credentials, query, or fragment")
	}
	serverPath := strings.TrimRight(u.Path, "/")
	if u.RawPath != "" || strings.Contains(serverPath, "\\") || (serverPath != "" && pathpkg.Clean(serverPath) != serverPath) {
		return "", errors.New("server base path cannot contain escapes, backslashes, repeated separators, or dot segments")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if u.Scheme == "http" && !IsLoopbackHost(host) {
		return "", errors.New("server must use https; plain http works only on loopback")
	}
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path = serverPath
	return u.String(), nil
}

func WithConfigLock(ctx context.Context, fn func() error) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return fn()
}

func Path() (string, error) {
	if p := os.Getenv("FLOPWIRE_CONFIG"); p != "" {
		return p, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "flopwire", "config.json"), nil
}

// Load returns the client config: from FLOPWIRE_TOKEN and FLOPWIRE_SERVER when
// FLOPWIRE_TOKEN is set (no file is read), else from the config file.
func Load() (Config, error) {
	if token := strings.TrimSpace(os.Getenv(EnvToken)); token != "" {
		return envConfig(token)
	}
	return LoadFile()
}

func envConfig(token string) (Config, error) {
	server := strings.TrimSpace(os.Getenv(EnvServer))
	if server == "" {
		return Config{}, errors.New("FLOPWIRE_TOKEN is set but FLOPWIRE_SERVER is not")
	}
	normalized, err := NormalizeServer(server)
	if err != nil {
		return Config{}, fmt.Errorf("FLOPWIRE_SERVER: %w", err)
	}
	pin, err := ParseFingerprint(os.Getenv(EnvFingerprint))
	if err != nil {
		return Config{}, fmt.Errorf("FLOPWIRE_FINGERPRINT: %w", err)
	}
	return Config{Server: normalized, Token: token, TLSFingerprint: pin, FromEnv: true}, nil
}

// ErrNoCredential: the config file names no server or no token.
var ErrNoCredential = errors.New("config is missing server or token")

// LoadFile reads the config file, ignoring FLOPWIRE_TOKEN: what login,
// enroll and rotation change.
func LoadFile() (Config, error) {
	p, err := Path()
	if err != nil {
		return Config{}, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err = json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.Server == "" || c.Token == "" {
		return c, ErrNoCredential
	}
	return c, nil
}
func Save(c Config) error {
	if c.FromEnv {
		return errors.New("a credential from FLOPWIRE_TOKEN is never saved")
	}
	p, err := Path()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(p), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmpName, p); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(p))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
