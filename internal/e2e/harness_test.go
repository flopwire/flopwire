package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	_ "modernc.org/sqlite"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// config is what scripts/e2e-sync.sh hands the test.
type config struct {
	bin      string   // flopwire binary built from this tree
	server   string   // server URL as devices reach it (LAN IP, or loopback)
	pin      string   // the server's self-signed certificate fingerprint
	dbURL    string   // Postgres from the host (compose.dev.yaml port)
	compose  []string // docker compose invocation for this project
	s3       string   // MinIO from the host
	s3User   string
	s3Pass   string
	bucket   string
	root     string // work directory
	report   string // where to write the scenario table
	repoRoot string
}

func loadConfig(t *testing.T) *config {
	if os.Getenv("FLOPWIRE_E2E") != "1" {
		t.Skip("set FLOPWIRE_E2E=1 and run scripts/e2e-sync.sh")
	}
	get := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			t.Fatalf("%s is not set; run scripts/e2e-sync.sh", k)
		}
		return v
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return &config{bin: get("FLOPWIRE_E2E_BIN"), server: get("FLOPWIRE_E2E_SERVER"), pin: get("FLOPWIRE_E2E_FINGERPRINT"), dbURL: get("FLOPWIRE_E2E_DATABASE_URL"),
		compose: strings.Fields(get("FLOPWIRE_E2E_COMPOSE")), s3: get("FLOPWIRE_E2E_S3_ENDPOINT"), s3User: get("FLOPWIRE_E2E_S3_USER"),
		s3Pass: get("FLOPWIRE_E2E_S3_PASSWORD"), bucket: os.Getenv("FLOPWIRE_E2E_S3_BUCKET"), root: get("FLOPWIRE_E2E_ROOT"),
		report: os.Getenv("FLOPWIRE_E2E_REPORT"), repoRoot: repo}
}

// harness holds the server side and the two devices.
type harness struct {
	t        *testing.T
	cfg      *config
	ctx      context.Context
	pg       *pgxpool.Pool
	s3       *minio.Client
	admin    string // admin config path
	adminTok string // admin session token
	userID   string
	devs     []*device
	sockDir  string

	// nonce makes every generated needle unique to the run: a real-corpus
	// sample may hold earlier runs' output (and this test's source).
	nonce string

	mu      sync.Mutex
	results []result
}

type result struct {
	Scenario string            `json:"scenario"`
	Pass     bool              `json:"pass"`
	Wall     time.Duration     `json:"wall"`
	Timings  map[string]string `json:"timings,omitempty"`
	Notes    []string          `json:"notes,omitempty"`
}

func newHarness(t *testing.T) *harness {
	cfg := loadConfig(t)
	if cfg.bucket == "" {
		cfg.bucket = "flopwire"
	}
	ctx := context.Background()
	pg, err := pgxpool.New(ctx, cfg.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	mc, err := minio.New(cfg.s3, &minio.Options{Creds: credentials.NewStaticV4(cfg.s3User, cfg.s3Pass, "")})
	if err != nil {
		t.Fatal(err)
	}
	// Unix socket paths are limited to 104 bytes on macOS and the work
	// directory is usually deeper than that.
	sockDir, err := os.MkdirTemp("/tmp", "tme2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	return &harness{t: t, cfg: cfg, ctx: ctx, pg: pg, s3: mc, sockDir: sockDir, nonce: strconv.FormatInt(time.Now().UnixNano(), 36)}
}

// record adds a scenario's outcome to the report.
func (h *harness) record(r result) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.results = append(h.results, r)
}

// scenario runs fn as a subtest and records pass/fail and wall time.
func (h *harness) scenario(name string, fn func(t *testing.T, r *result)) {
	r := result{Scenario: name, Timings: map[string]string{}}
	start := time.Now()
	skipped := false
	ok := h.t.Run(name, func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		fn(t, &r)
	})
	r.Pass, r.Wall = ok && !skipped, time.Since(start).Round(time.Millisecond)
	if skipped {
		r.Notes = append(r.Notes, "skipped")
	}
	h.record(r)
}

func (h *harness) writeReport() {
	var b strings.Builder
	fmt.Fprintf(&b, "| Scenario | Result | Wall | Timings | Notes |\n|---|---|---|---|---|\n")
	for _, r := range h.results {
		var ts []string
		for k, v := range r.Timings {
			ts = append(ts, k+"="+v)
		}
		sort.Strings(ts)
		res := "pass"
		if !r.Pass {
			res = "FAIL"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.Scenario, res, r.Wall, strings.Join(ts, ", "), strings.Join(r.Notes, "; "))
	}
	h.t.Log("\n" + b.String())
	if h.cfg.report != "" {
		_ = os.WriteFile(h.cfg.report, []byte(b.String()), 0o644)
		raw, _ := json.MarshalIndent(h.results, "", "  ")
		_ = os.WriteFile(h.cfg.report+".json", raw, 0o644)
	}
}

// run executes the flopwire binary with env and returns stdout.
func (h *harness) run(env []string, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(h.ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.cfg.bin, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("flopwire %s: %w: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

// compose runs docker compose for the test project.
func (h *harness) compose(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(h.ctx, 4*time.Minute)
	defer cancel()
	all := append(append([]string{}, h.cfg.compose[1:]...), args...)
	cmd := exec.CommandContext(ctx, h.cfg.compose[0], all...)
	cmd.Dir = h.cfg.repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", h.cfg.compose[0], strings.Join(all, " "), err, out)
	}
	return string(out), nil
}

// baseEnv is the environment without anything that would point a flopwire
// process at the real harness data or config of this machine.
func baseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "FLOPWIRE_DEVIN_DB", "FLOPWIRE_OPENCODE_DB", "OPENCODE_DB", "XDG_DATA_HOME", "FLOPWIRE_INDEX", "FLOPWIRE_CONFIG", "XDG_CONFIG_HOME", "XDG_CACHE_HOME":
			continue
		}
		env = append(env, kv)
	}
	return env
}

func (h *harness) adminEnv() []string {
	return append(baseEnv(), "HOME="+h.cfg.root, "FLOPWIRE_CONFIG="+h.admin, "DATABASE_URL="+h.cfg.dbURL)
}

// setupIdentity bootstraps the admin, invites the user and enrolls both
// devices: device 1 claims the invite, device 2 logs in as the same user.
func (h *harness) setupIdentity(names ...string) {
	t := h.t
	h.admin = filepath.Join(h.cfg.root, "admin", "config.json")
	if err := os.MkdirAll(filepath.Dir(h.admin), 0o700); err != nil {
		t.Fatal(err)
	}
	const adminPW, userPW = "e2e admin correct horse", "e2e member battery staple"
	if _, err := h.run(h.adminEnv(), adminPW+"\n"+adminPW+"\n", "bootstrap", "--server", h.cfg.server, "--fingerprint", h.cfg.pin, "--name", "Admin", "--email", "admin@e2e.test"); err != nil {
		t.Fatal(err)
	}
	out, err := h.run(h.adminEnv(), "", "invite", "--email", "gary@e2e.test")
	if err != nil {
		t.Fatal(err)
	}
	var inv struct{ Code, Invite string }
	if err := json.Unmarshal([]byte(out), &inv); err != nil || inv.Code == "" {
		t.Fatalf("invite: %v %q", err, out)
	}
	// The invite carries the server and its certificate pin (D13).
	if p, err := client.ParseInvite(inv.Invite); err != nil || p.Fingerprint != h.cfg.pin || p.Code != inv.Code {
		t.Fatalf("invite string %q: %+v %v", inv.Invite, p, err)
	}
	var adminCfg struct {
		SessionToken string `json:"session_token"`
	}
	raw, _ := os.ReadFile(h.admin)
	_ = json.Unmarshal(raw, &adminCfg)
	h.adminTok = adminCfg.SessionToken
	for i, name := range names {
		d := h.newDevice(i+1, name)
		if i == 0 {
			if _, err := h.run(d.env(), userPW+"\n"+userPW+"\n", "claim", "--invite", inv.Invite, "--name", "Gary"); err != nil {
				t.Fatal(err)
			}
		} else {
			// A wrong pin is refused before the password is sent.
			wrong := client.FingerprintPrefix + strings.Repeat("0", 64)
			if out, err := h.run(d.env(), userPW+"\n", "login", "--server", h.cfg.server, "--fingerprint", wrong, "--email", "gary@e2e.test"); err == nil || !strings.Contains(err.Error(), "does not match the pinned fingerprint") {
				t.Fatalf("login with a wrong pin: %v %q", err, out)
			}
			if _, err := h.run(d.env(), userPW+"\n", "login", "--server", h.cfg.server, "--fingerprint", h.cfg.pin, "--email", "gary@e2e.test"); err != nil {
				t.Fatal(err)
			}
		}
		out, err := h.run(d.env(), "", "enroll", "--name", name)
		if err != nil {
			t.Fatal(err)
		}
		var dev struct {
			Device struct {
				ID     string `json:"id"`
				UserID string `json:"user_id"`
			} `json:"device"`
		}
		if err := json.Unmarshal([]byte(out), &dev); err != nil {
			t.Fatal(err)
		}
		d.id, h.userID = dev.Device.ID, dev.Device.UserID
		if d.id == "" {
			t.Fatalf("enroll %s: no device id in %q", name, out)
		}
		h.devs = append(h.devs, d)
	}
}

// device is one simulated laptop: its own HOME with Claude, Codex and Devin
// data, its own config (credential), local index, sync state and socket.
type device struct {
	h     *harness
	n     int
	name  string
	id    string // server device id
	dir   string
	home  string
	cfg   string
	index string
	sock  string
	log   string

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan error
	rss  int64 // peak resident set of the agent, sampled

	claudeSID, claudePath, claudeLast string
	codexSID, codexPath               string
	codexNeedle                       string // the codex growth session's first prompt
	codexOrd                          int
	seq                               int
}

func (h *harness) newDevice(n int, name string) *device {
	dir := filepath.Join(h.cfg.root, fmt.Sprintf("dev%d", n))
	d := &device{h: h, n: n, name: name, dir: dir, home: filepath.Join(dir, "home"), cfg: filepath.Join(dir, "config", "config.json"),
		index: filepath.Join(dir, "cache", "index.db"), sock: filepath.Join(h.sockDir, fmt.Sprintf("d%d.sock", n)), log: filepath.Join(dir, "agent.log")}
	for _, p := range []string{d.home, filepath.Dir(d.cfg), filepath.Dir(d.index)} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			h.t.Fatal(err)
		}
	}
	return d
}

func (d *device) env() []string {
	return append(baseEnv(), "HOME="+d.home, "FLOPWIRE_CONFIG="+d.cfg, "FLOPWIRE_INDEX="+d.index)
}

// cli runs a flopwire command as this device.
func (d *device) cli(args ...string) (string, error) { return d.h.run(d.env(), "", args...) }

// seed builds the device's harness data: the oracle home (Claude with
// subagents, workflows and tool-results; Codex live and archived), the
// oracle Devin store, and two growth sessions (Claude, Codex) unique to the
// device that the scenarios append to.
func (d *device) seed() {
	t := d.h.t
	copyTree(t, filepath.Join(d.h.cfg.repoRoot, "testdata", "oracle", "home"), d.home)
	seed, err := os.ReadFile(filepath.Join(d.h.cfg.repoRoot, "testdata", "oracle", "seeds", "devin.sql"))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := d.devinDB()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	d.devinExec(string(seed))

	d.claudeSID = fmt.Sprintf("e2e0000%d-0000-4000-8000-000000000001", d.n)
	proj := filepath.Join(d.home, ".claude", "projects", "-tmp-e2e-growth")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	d.claudePath = filepath.Join(proj, d.claudeSID+".jsonl")
	writeFile(t, d.claudePath, d.claudeUser(fmt.Sprintf("growth session on %s starts here %s", d.name, d.h.nonce)))

	d.codexSID = fmt.Sprintf("019a0000-0000-7000-8000-00000000e10%d", d.n)
	cdir := filepath.Join(d.home, ".codex", "sessions", "2026", "09", "29")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatal(err)
	}
	d.codexPath = filepath.Join(cdir, "rollout-2026-09-29T10-00-00-"+d.codexSID+".jsonl")
	meta := fmt.Sprintf(`{"timestamp":%q,"ordinal":0,"type":"session_meta","payload":{"session_id":%q,"id":%q,"timestamp":%q,"cwd":"/tmp/e2e-growth","originator":"codex-tui","cli_version":"0.154.0","source":"cli","model_provider":"openai"}}`+"\n",
		now(), d.codexSID, d.codexSID, now())
	d.codexNeedle = fmt.Sprintf("codex growth session on %s %s", d.name, d.h.nonce)
	writeFile(t, d.codexPath, meta+d.codexMsg("user", d.codexNeedle))
}

func (d *device) devinDB() string {
	return filepath.Join(d.home, ".local", "share", "devin", "cli", "sessions.db")
}

// devinPlaced returns the Devin sessions that have a working directory,
// which places them; the others get the unplaceable setting.
func (d *device) devinPlaced() (map[string]bool, error) {
	db, err := sql.Open("sqlite", "file:"+d.devinDB()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id FROM sessions WHERE working_directory LIKE '/%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// devinExec writes to the device's Devin store the way Devin does: a
// separate connection in WAL mode.
func (d *device) devinExec(q string, args ...any) {
	db, err := sql.Open("sqlite", "file:"+d.devinDB()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		d.h.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		d.h.t.Fatalf("devin: %v", err)
	}
}

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func (d *device) nextUUID() string {
	d.seq++
	return fmt.Sprintf("e2e%05d-%04d-4000-8000-%012d", d.n, d.seq%10000, d.seq)
}

// claudeUser returns a Claude user line chained to the previous line.
func (d *device) claudeUser(text string) string {
	return d.claudeLine(`"type":"user","message":{"role":"user","content":` + jsonStr(text) + `}`)
}

func (d *device) claudeAssistant(text string) string {
	return d.claudeLine(`"type":"assistant","message":{"id":"msg_` + d.nextUUID()[:8] + `","type":"message","role":"assistant","model":"claude-e2e","content":[{"type":"text","text":` + jsonStr(text) + `}]}`)
}

func (d *device) claudeLine(body string) string {
	parent := "null"
	if d.claudeLast != "" {
		parent = jsonStr(d.claudeLast)
	}
	u := d.nextUUID()
	d.claudeLast = u
	return `{"parentUuid":` + parent + `,"isSidechain":false,"userType":"external","cwd":"/tmp/e2e-growth","sessionId":"` + d.claudeSID +
		`","version":"2.1.0",` + body + `,"uuid":"` + u + `","timestamp":"` + now() + `"}` + "\n"
}

func (d *device) codexMsg(role, text string) string {
	d.codexOrd++
	typ := "input_text"
	if role == "assistant" {
		typ = "output_text"
	}
	return fmt.Sprintf(`{"timestamp":%q,"ordinal":%d,"type":"response_item","payload":{"type":"message","id":"msg_e2e_%d_%d","role":%q,"content":[{"type":%q,"text":%s}]}}`+"\n",
		now(), d.codexOrd, d.n, d.codexOrd, role, typ, jsonStr(text))
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// start launches `flopwire agent run` for the device.
func (d *device) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	logf, err := os.OpenFile(d.log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		d.h.t.Fatal(err)
	}
	fmt.Fprintf(logf, "=== start %s\n", time.Now().Format(time.RFC3339Nano))
	cmd := exec.Command(d.h.cfg.bin, "agent", "run", "-v", "--socket", d.sock)
	cmd.Env = d.env()
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		d.h.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); logf.Close() }()
	d.cmd, d.done = cmd, done
	go d.sampleRSS(cmd.Process.Pid)
}

// sampleRSS records the agent's peak resident set every 500ms until it
// exits (ps reports kilobytes).
func (d *device) sampleRSS(pid int) {
	for {
		out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return
		}
		if kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			d.mu.Lock()
			d.rss = max(d.rss, kb<<10)
			d.mu.Unlock()
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (d *device) peakRSS() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rss
}

// stop ends the agent: SIGTERM, or SIGKILL when kill is set.
func (d *device) stop(kill bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cmd == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = d.cmd.Process.Signal(sig)
	select {
	case <-d.done:
	case <-time.After(20 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.done
	}
	d.cmd = nil
}

func (d *device) running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cmd == nil {
		return false
	}
	select {
	case err := <-d.done:
		d.h.t.Logf("%s agent exited: %v", d.name, err)
		d.cmd = nil
		return false
	default:
		return true
	}
}

// ---- local index (read-only) ----

type localRow struct {
	ID         int64
	Superseded bool
	Session    string
	Agent      string
	OnPath     sql.NullInt64
}

// localFind returns the local index rows whose text contains needle, read
// through the trigram shards on a read-only connection (the agent owns the
// writers).
func (d *device) localFind(needle string) ([]localRow, error) {
	db, err := sql.Open("sqlite", "file:"+d.index+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(d.h.ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var parts []string
	for i := 0; ; i++ {
		p := fmt.Sprintf("%s-tri%d", d.index, i)
		if _, err := os.Stat(p); err != nil {
			break
		}
		schema := fmt.Sprintf("tri%d", i)
		if _, err := conn.ExecContext(d.h.ctx, `ATTACH DATABASE ? AS `+schema, "file:"+p+"?mode=ro"); err != nil {
			return nil, err
		}
		parts = append(parts, `SELECT rowid FROM `+schema+`.fts_tri t WHERE t.fts_tri MATCH ?1`)
	}
	if len(parts) == 0 {
		return nil, errors.New("no trigram shards yet")
	}
	q := `SELECT m.id, m.superseded, c.session_id, c.agent, m.on_active_path FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.id IN (` + strings.Join(parts, " UNION ") + `)`
	rows, err := conn.QueryContext(d.h.ctx, q, `"`+strings.ReplaceAll(needle, `"`, `""`)+`"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []localRow
	for rows.Next() {
		var r localRow
		if err := rows.Scan(&r.ID, &r.Superseded, &r.Session, &r.Agent, &r.OnPath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// localLive counts rows matching needle that the default view shows.
func (d *device) localLive(needle string) (live, superseded int, err error) {
	rows, err := d.localFind(needle)
	for _, r := range rows {
		if r.Superseded {
			superseded++
		} else if !r.OnPath.Valid || r.OnPath.Int64 != 0 {
			live++
		}
	}
	return live, superseded, err
}

// localLiveIn is localLive restricted to one session.
func (d *device) localLiveIn(needle, session string) (live, superseded int, err error) {
	rows, err := d.localFind(needle)
	for _, r := range rows {
		if r.Session != session {
			continue
		}
		if r.Superseded {
			superseded++
		} else if !r.OnPath.Valid || r.OnPath.Int64 != 0 {
			live++
		}
	}
	return live, superseded, err
}

// inSession keeps the hits of one session.
func inSession(hits []format.Hit, session string) []format.Hit {
	var out []format.Hit
	for _, h := range hits {
		if h.SessionID == session {
			out = append(out, h)
		}
	}
	return out
}

// localKeys returns native-id keys of the live rows of a session in the
// local index, with their multiplicity.
func (d *device) localKeys(agent, session string) (map[string]int, error) {
	db, err := sql.Open("sqlite", "file:"+d.index+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(d.h.ctx, `SELECT COALESCE(m.native_id, '@'||m.byte_offset), m.part FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.agent=? AND c.session_id=? AND m.superseded=0`, agent, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var part int
		if err := rows.Scan(&k, &part); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%s#%d", k, part)]++
	}
	return out, rows.Err()
}

// localConversationExtra returns a local conversation's extra JSON.
func (d *device) localConversationExtra(agent transcript.Agent, session string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+d.index+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var extra sql.NullString
	err = db.QueryRowContext(d.h.ctx, `SELECT extra FROM conversations WHERE agent=? AND session_id=?`, string(agent), session).Scan(&extra)
	return extra.String, err
}

// ---- server ----

// serverFind runs `flopwire grep --server --json -F` as the device.
func (d *device) serverFind(needle string, extra ...string) ([]format.Hit, error) {
	args := append([]string{"grep", "--server", "--json", "-F"}, extra...)
	out, err := d.cli(append(args, needle)...)
	if err != nil {
		return nil, err
	}
	var res struct{ Hits []format.Hit }
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return nil, fmt.Errorf("find output %q: %w", out, err)
	}
	return res.Hits, nil
}

// hitsOn keeps the hits of one device. Hits name the device (its name);
// the device's id is accepted too.
func hitsOn(hits []format.Hit, d *device) []format.Hit {
	var out []format.Hit
	for _, h := range hits {
		if h.Device == d.name || h.Device == d.id {
			out = append(out, h)
		}
	}
	return out
}

// serverKeys returns native-id keys of the live rows of a session on the
// server, with their multiplicity.
func (h *harness) serverKeys(deviceID, agent, session string) (map[string]int, error) {
	rows, err := h.pg.Query(h.ctx, `SELECT COALESCE(m.native_id, '@'||m.byte_offset::text), m.part, count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.device_id=$1 AND c.agent=$2 AND c.session_id=$3 AND NOT m.superseded GROUP BY 1,2`, deviceID, agent, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var part, n int
		if err := rows.Scan(&k, &part, &n); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%s#%d", k, part)] = n
	}
	return out, rows.Err()
}

// parseKeys parses a transcript file on the device the way the agent does
// and returns the native-id keys of the session's rows.
func parseKeys(path string, agent transcript.Agent, session string) (map[string]int, error) {
	all, err := parseAll(path, agent)
	if err != nil {
		return nil, err
	}
	if all[session] == nil {
		return map[string]int{}, nil
	}
	return all[session], nil
}

// parseAll parses a transcript file and returns the native-id keys of its
// rows by session.
func parseAll(path string, agent transcript.Agent) (map[string]map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var p transcript.Parser
	switch agent {
	case transcript.AgentClaude:
		p = &claude.Parser{}
	case transcript.AgentCodex:
		p = &codex.Parser{}
	default:
		return nil, fmt.Errorf("parseAll: %s", agent)
	}
	src := &transcript.Source{Agent: agent, Path: path, StorageKind: transcript.StorageJSONLAppend, Parser: p.Name()}
	var c transcript.Collector
	if _, err := transcript.Reparse(context.Background(), p, transcript.Input{Source: src, R: f, Size: fi.Size()}, &c); err != nil {
		return nil, err
	}
	out := map[string]map[string]int{}
	for _, m := range c.Messages {
		k := m.NativeID
		if k == "" {
			k = fmt.Sprintf("@%d", m.ByteOffset)
		}
		if out[m.SessionID] == nil {
			out[m.SessionID] = map[string]int{}
		}
		out[m.SessionID][fmt.Sprintf("%s#%d", k, m.Part)]++
	}
	return out, nil
}

// transcripts lists every Claude and Codex transcript on the device.
func (d *device) transcripts() (map[string]transcript.Agent, error) {
	out := map[string]transcript.Agent{}
	sessions, err := claude.Discover(filepath.Join(d.home, ".claude", "projects"))
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		for _, src := range s.Sources() {
			out[src.Path] = transcript.AgentClaude
		}
	}
	srcs, err := codex.Discover(filepath.Join(d.home, ".codex"))
	if err != nil {
		return nil, err
	}
	for _, src := range srcs {
		out[src.Path] = transcript.AgentCodex
	}
	return out, nil
}

// diffKeys describes how got differs from want: missing keys (gaps),
// unexpected keys, and duplicates.
func diffKeys(want, got map[string]int) string {
	var missing, extra, dup []string
	for k := range want {
		if got[k] == 0 {
			missing = append(missing, k)
		}
	}
	for k, n := range got {
		if want[k] == 0 {
			extra = append(extra, k)
		}
		if n > 1 {
			dup = append(dup, fmt.Sprintf("%s x%d", k, n))
		}
	}
	if len(missing)+len(extra)+len(dup) == 0 {
		return ""
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return fmt.Sprintf("missing %d %v, unexpected %d %v, duplicated %v", len(missing), head(missing), len(extra), head(extra), dup)
}

func head(s []string) []string {
	if len(s) > 5 {
		return s[:5]
	}
	return s
}

// adminJSON calls the admin API with the admin session.
func (h *harness) adminJSON(method, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(h.ctx, method, strings.TrimRight(h.cfg.server, "/")+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+h.adminTok)
	res, err := client.NewHTTPClient(h.cfg.pin).Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return res.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, raw)
		}
	}
	return res.StatusCode, nil
}

func (h *harness) ready(url string) bool {
	c := client.NewHTTPClient(h.cfg.pin)
	c.Timeout = 2 * time.Second
	res, err := c.Get(strings.TrimRight(url, "/") + "/readyz")
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == 200
}

// ---- waiting ----

// eventually polls cond every step until it holds or d passes, and returns
// how long it took.
func eventually(t *testing.T, d, step time.Duration, what string, cond func() (bool, error)) time.Duration {
	t.Helper()
	start := time.Now()
	var last error
	for {
		ok, err := cond()
		if ok {
			return time.Since(start)
		}
		last = err
		if time.Since(start) > d {
			t.Fatalf("%s: not true after %s (last error: %v)", what, d, last)
		}
		time.Sleep(step)
	}
}

// ---- files ----

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if e.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// noise returns n bytes of printable random text in lines, so chunks of a
// large file are distinct.
func noise(n int) string {
	raw := make([]byte, n*3/4+3)
	_, _ = rand.Read(raw)
	s := base64.StdEncoding.EncodeToString(raw)[:n]
	var b strings.Builder
	for i := 0; i < len(s); i += 120 {
		b.WriteString(s[i:min(i+120, len(s))])
		b.WriteByte('\n')
	}
	return b.String()
}

// logText returns about n bytes of build-log-like text: realistic
// redundancy, but every line unique, so chunks of a large file differ.
func logText(seed, n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "2026-09-29T10:%02d:%02d.%03dZ INFO worker-%d batch %d/%d processed %d records in %dms status=ok\n",
			i/60%60, i%60, (seed*7+i)%1000, i%8, seed, i, (seed*31+i*17)%5000, (i*13)%900)
	}
	return b.String()
}

func ms(d time.Duration) string { return d.Round(time.Millisecond).String() }
