package main

// The private diagnostic is separate from the legacy probe tap. Its scoped
// directory is supplied explicitly; it never records bodies, paths, raw input,
// arbitrary errors, or unaliased identifiers.
import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const envHookDiagnostic = "FLOPWIRE_PRIVATE_HOOK_DIAGNOSTIC"

// Reserve 4 KiB for the bounded configuration so the run stays below 256 KiB.
const diagnosticMaxBytes = (256 << 10) - 4096
const diagnosticMaxEvents = 256

type hookDiagnosticConfig struct {
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Salt    string    `json:"salt"` // 32 random bytes, hex encoded; one salt per run
}

type hookObservation struct {
	AtUTC         time.Time `json:"at_utc"`
	Event         string    `json:"event"`
	Session       string    `json:"session,omitempty"`
	Messages      []string  `json:"messages,omitempty"`
	Reason        string    `json:"reason"`
	PendingUS     int64     `json:"pending_us,omitempty"`
	OutputBytes   int       `json:"output_bytes,omitempty"`
	OutputResult  string    `json:"output_result,omitempty"`
	ConfirmUS     int64     `json:"confirm_us,omitempty"`
	ConfirmResult string    `json:"confirm_result,omitempty"`
	DiagnosticUS  int64     `json:"diagnostic_us"`
	ElapsedUS     int64     `json:"elapsed_us"`
}

type hookDiagnostic struct {
	dir      string
	cfg      hookDiagnosticConfig
	salt     []byte
	entry    hookObservation
	overhead time.Duration
}

// openHookDiagnostic runs after the hook entry timestamp is captured. Invalid
// configuration silently disables observation and never changes delivery.
func openHookDiagnostic(dir string, now time.Time) *hookDiagnostic {
	setupStarted := time.Now()
	if dir == "" {
		return nil
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 || !diagnosticOwned(st) {
		return nil
	}
	p := filepath.Join(dir, "config.json")
	st, err = os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || !diagnosticOwned(st) || st.Size() > 4096 {
		(&hookDiagnostic{dir: dir}).unavailable()
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		(&hookDiagnostic{dir: dir}).unavailable()
		return nil
	}
	var cfg hookDiagnosticConfig
	if json.Unmarshal(b, &cfg) != nil || cfg.Created.After(now) || !cfg.Expires.After(now) || cfg.Expires.Sub(cfg.Created) > 10*time.Minute {
		(&hookDiagnostic{dir: dir}).unavailable()
		return nil
	}
	salt, err := hex.DecodeString(cfg.Salt)
	if err != nil || len(salt) != 32 {
		(&hookDiagnostic{dir: dir}).unavailable()
		return nil
	}
	if _, err := os.Lstat(filepath.Join(dir, "unavailable")); !os.IsNotExist(err) {
		(&hookDiagnostic{dir: dir}).unavailable()
		return nil
	}
	d := &hookDiagnostic{dir: dir, cfg: cfg, salt: salt, overhead: time.Since(setupStarted), entry: hookObservation{AtUTC: now.UTC(), Event: "unknown", Reason: "internal_error"}}
	return d
}

func diagnosticOwned(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Getuid())
}

func (d *hookDiagnostic) alias(kind, id string) string {
	if id == "" {
		return ""
	}
	h := hmac.New(sha256.New, d.salt)
	h.Write([]byte(kind + "\x00" + id))
	return hex.EncodeToString(h.Sum(nil)[:12])
}
func (d *hookDiagnostic) input(in hookInput) {
	if d == nil {
		return
	}
	d.entry.Session = d.alias("session", in.SessionID)
	switch in.Event {
	case evSessionStart, evUserPromptSubmit, evPostToolUse, evSessionEnd, "Stop":
		d.entry.Event = in.Event
	default:
		d.entry.Event = "other"
	}
}
func (d *hookDiagnostic) reason(reason string) {
	if d != nil {
		switch reason {
		case "invalid_arguments", "invalid_input", "config_error", "encode_error", "unsupported_harness", "non_delivery_event", "no_session", "subagent", "held_elsewhere", "late", "pending_error", "pending_timeout", "empty", "output_error", "printed", "confirm_error", "confirm_timeout", "confirm_late":
			d.entry.Reason = reason
		default:
			d.entry.Reason = "internal_error"
		}
	}
}
func (d *hookDiagnostic) unavailable() {
	f, err := os.OpenFile(filepath.Join(d.dir, "unavailable"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		f.Close()
	}
}

// finish attempts one append. It never waits for another diagnostic writer.
// A cap or error disables this run through an empty content-free sentinel.
func (d *hookDiagnostic) finish(start time.Time) {
	if d == nil {
		return
	}
	workStarted := time.Now()
	defer func() {
		if recover() != nil {
			d.unavailable()
		}
	}()
	d.entry.ElapsedUS = time.Since(start).Microseconds()
	if _, err := os.Lstat(filepath.Join(d.dir, "unavailable")); !os.IsNotExist(err) {
		return
	}
	if !d.cfg.Expires.After(time.Now()) {
		d.unavailable()
		return
	}
	f, err := os.OpenFile(filepath.Join(d.dir, "events.jsonl"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		d.unavailable()
		return
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		d.unavailable()
		return
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || !diagnosticOwned(st) || st.Size() >= diagnosticMaxBytes {
		d.unavailable()
		return
	}
	// The entire run is bounded, so counting lines requires at most 256 KiB.
	b := make([]byte, st.Size())
	if _, err = f.ReadAt(b, 0); err != nil && len(b) > 0 {
		d.unavailable()
		return
	}
	count := 0
	for _, c := range b {
		if c == '\n' {
			count++
		}
	}
	d.entry.AtUTC = start.UTC()
	d.entry.DiagnosticUS = (d.overhead + time.Since(workStarted)).Microseconds()
	line, err := json.Marshal(d.entry)
	line = append(line, '\n')
	if err != nil || count >= diagnosticMaxEvents || len(b)+len(line) > diagnosticMaxBytes {
		d.unavailable()
		return
	}
	if n, writeErr := f.WriteAt(line, st.Size()); writeErr != nil || n != len(line) {
		d.unavailable()
	}
}
