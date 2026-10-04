package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/auth"
	backupsvc "github.com/flopwire/flopwire/internal/backup"
	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/retrieval"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/webapp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/term"
)

var version = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		if !errors.Is(err, errReported) { // already written (JSON on stderr)
			fmt.Fprintln(os.Stderr, "flopwire:", err)
		}
		os.Exit(1)
	}
}
func run(parent context.Context, args []string) error {
	if len(args) == 0 {
		return usage()
	}
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch args[0] {
	case "diagnostics":
		return diagnosticsCommand(ctx, args[1:], os.Stdout)
	case "version":
		fmt.Println(version)
		return nil
	case "serve":
		return serve(ctx, args[1:])
	case "healthcheck":
		return healthcheck(ctx, args[1:])
	case "fingerprint":
		return fingerprintCmd(args[1:])
	case "backup":
		return backupCommand(ctx, args[1:])
	case "backup-verify":
		return backupVerifyCommand(args[1:])
	case "restore":
		return restoreCommand(ctx, args[1:])
	case "bootstrap":
		return bootstrap(ctx, args[1:])
	case "login":
		return login(ctx, args[1:])
	case "invite":
		return invite(ctx, args[1:])
	case "service-account":
		return serviceAccount(ctx, args[1:])
	case "revoke-device":
		return revokeDevice(ctx, args[1:])
	case "admin":
		return adminCmd(ctx, args[1:])
	case "rotate-device":
		return rotateDevice(ctx, args[1:])
	case "token":
		return tokenCmd(ctx, args[1:])
	case "claim":
		return claim(ctx, args[1:])
	case "enroll":
		return enroll(ctx, args[1:])
	case "grep", "find":
		return toolCmd(ctx, "grep", args[1:])
	case "search", "sessions", "read":
		return toolCmd(ctx, args[0], args[1:])
	case "peers", "send", "inbox":
		return busMain(ctx, args[0], args[1:])
	case "accept", "revoke", "accepts":
		return acceptMain(ctx, args[0], args[1:])
	case "raw":
		return raw(ctx, args[1:])
	case "mcp":
		return mcp(ctx, args[1:])
	case "agent":
		return agentCmd(ctx, args[1:])
	case "hook":
		return hookMain(ctx, args[1:])
	case "setup":
		return setupMain(ctx, args[1:])
	case "probe":
		return probeMain(ctx, args[1:])
	case "bench":
		return benchCmd(ctx, args[1:])
	case "redact":
		return redactCmd(ctx, args[1:])
	default:
		return usage()
	}
}

// usageText lists every command; the plugin test checks hook commands
// against it.
const usageText = `Usage: flopwire <command>

  serve       run the API and admin service
  healthcheck probe a server readiness endpoint
  fingerprint print the server's self-signed certificate pin (on the server host)
  backup      create a coordinated Postgres and object backup
  backup-verify  verify every file in a backup
  restore     restore a verified backup into empty targets
  bootstrap   create the first administrator (needs DATABASE_URL)
  login       authenticate and save a local credential
  invite      create a one-time member invitation
  service-account  create an upload-only service identity
  revoke-device    revoke a device and its credentials
  admin       admin devices: list devices; admin revoke-device ID;
              admin revoke-user ID: revoke every credential of a user or
              service identity; admin token-ttl [DURATION]: the minted
              token cap; admin policy preview: sessions a path-rule change
              hid; admin policy purge --yes: delete them now
  rotate-device    replace this device's credential without a gap
  token       token mint --label NAME --ttl 2h --scope upload,read: a
              short-lived token for a sandbox or CI job (FLOPWIRE_TOKEN)
  claim       claim an invitation
  enroll      bind this installation as a device
  grep        regex search over agent transcripts, like rg (find is an alias)
  search      ranked search for fuzzy questions
  sessions    list sessions, newest first
  read        read a message or session at an address that grep, search or sessions print
  peers       list live agent sessions you can message
  send        message another agent session, or @user's next session
  inbox       this session's messages, received and sent
  accepts     who may message your agents, and messages held until you accept
              their sender (a person at a terminal only, like accept and revoke)
  accept      accept messages from a person's agents (asks for your password)
  revoke      stop accepting a person's messages; undelivered ones are held again
  mcp         serve grep, search, sessions, read, peers, send and inbox over
              MCP stdio (the first four read the local index; --server
              queries the team server; the last three go through the device
              agent; flopwire <tool> --help shows examples)
  diagnostics inspect extraction reports (--server, --source ID, --json)
  raw         plumbing: archived bytes of a source by provenance
  redact      hide a message (or some of its lines) on the server and in the local index
  hook        what harness hooks run: prints messages for this session into it,
              and asks the device agent to index the transcript now
  setup       install Flopwire into Claude Code, Codex, Devin CLI and opencode
              through each one's own plugin mechanism (--check reports, --remove uninstalls)
  probe       re-run the message-bus delivery tests against the installed
              harnesses (--local, --json, --notes; docs/probe.md)
  agent       run the device agent (agent run) or signal it from a hook (agent flush)
  bench       bench acceptance: the local-track acceptance checks on this device's transcripts
  version     print version`

// usageCommands are the commands a usage text lists: lines indented two
// spaces whose first word is the command. It reads any flopwire's usage,
// so setup can tell which commands an installed binary knows.
func usageCommands(text string) map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(text, "\n") {
		if f := strings.Fields(l); len(f) > 1 && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "    ") {
			m[f[0]] = true
		}
	}
	return m
}

func usage() error {
	fmt.Fprintln(os.Stderr, usageText)
	return errors.New("command required")
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", env("FLOPWIRE_ADDR", ":8080"), "listen address")
	tlsf := addTLSFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	transport, err := resolveServerTLS(tlsf, *addr)
	if err != nil {
		return err
	}
	database := mustEnv("DATABASE_URL")
	endpoint := mustEnv("S3_ENDPOINT")
	access := mustEnv("S3_ACCESS_KEY")
	secret := mustEnv("S3_SECRET_KEY")
	bucket := env("S3_BUCKET", "flopwire")
	secure := envBool("S3_SECURE", false)
	workers := envInt("FLOPWIRE_PARSE_WORKERS", 4)
	poolCfg, err := store.PoolConfig(database, api.RetrievalConcurrency+workers+1)
	if err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	if err = store.Migrate(ctx, pool); err != nil {
		return err
	}
	mc, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: secure, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}})
	if err != nil {
		return err
	}
	exists, err := mc.BucketExists(ctx, bucket)
	if err != nil {
		return err
	}
	if !exists {
		if err = mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return err
		}
	}
	reg := prometheus.NewRegistry()
	durableStore := store.NewPostgres(pool, mc, bucket)
	objects := ingest.MinIO{Client: mc, Bucket: bucket}
	parser := &ingest.Queue{Pool: pool, Objects: objects, Log: slog.Default(), Workers: workers, RefreshInterval: envDuration("FLOPWIRE_REPARSE_INTERVAL", 2*time.Second)}
	go parser.Run(ctx)
	// The server's ctx ends at shutdown: a waiting poll answers then,
	// inside the shutdown grace, instead of being cut.
	messageBus := &bus.Store{Pool: pool, Stopping: ctx.Done()}
	app := api.New(durableStore, api.Config{Registry: reg, Logger: slog.Default(),
		Sync: &ingest.Server{Pool: pool, Objects: objects, Log: slog.Default(), Queue: parser}, Parse: parser,
		Retrieval:         &retrieval.Store{Pool: pool, Objects: objects, RefreshSession: parser.RefreshSession},
		Bus:               messageBus,
		TrustedProxyCIDRs: envList("FLOPWIRE_TRUSTED_PROXY_CIDRS"),
		AuthRate:          api.Rate{Burst: envInt("FLOPWIRE_AUTH_RATE_BURST", 10), Refill: envDuration("FLOPWIRE_AUTH_RATE_REFILL", time.Minute)}})
	go runDeletionWorker(ctx, durableStore, slog.Default())
	go runCredentialSweeper(ctx, durableStore, slog.Default())
	go runChunkReconciler(ctx, durableStore, slog.Default())
	go runBusSweeper(ctx, messageBus, slog.Default())
	apiHandler := app.Handler(reg)
	root := http.NewServeMux()
	root.Handle("/v1/", apiHandler)
	root.Handle("/healthz", apiHandler)
	root.Handle("/livez", apiHandler)
	root.Handle("/readyz", apiHandler)
	root.Handle("/metrics", apiHandler)
	root.Handle("/", webapp.Handler())
	server := &http.Server{Addr: *addr, Handler: api.SecurityHeaders(root), TLSConfig: transport.config, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second}
	transport.announce(os.Stderr, *addr)
	slog.Info("flopwire server listening", "addr", *addr, "tls", transport.mode, "version", version)
	return serveUntilDone(ctx, server, shutdownGrace, func() error {
		if transport.config != nil {
			return server.ListenAndServeTLS("", "")
		}
		return server.ListenAndServe()
	})
}

// shutdownGrace is how long a shutdown waits for requests in flight.
const shutdownGrace = 10 * time.Second

// serveUntilDone runs listen until ctx is done, then shuts server down:
// it stops accepting connections and waits up to grace for requests in
// flight before it returns, so the deferred pool close and the process
// exit do not cut them.
func serveUntilDone(ctx context.Context, server *http.Server, grace time.Duration, listen func() error) error {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err := listen()
	if errors.Is(err, http.ErrServerClosed) {
		// Listen returns as Shutdown starts; Shutdown returns when the
		// requests in flight have finished or grace has passed.
		<-stopped
		return nil
	}
	return err
}
func backupCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	output := fs.String("output", "", "new or empty backup directory")
	encrypted := fs.Bool("encrypted-destination", false, "acknowledge the backup destination is encrypted")
	allowRepair := fs.Bool("allow-pending-redaction-repair", false, "take the backup even while message redactions still repair archived bytes (the backup may hold redacted text)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := backupsvc.ValidateOutput(*output); err != nil {
		return err
	}
	objects, err := newObjectClient()
	if err != nil {
		return err
	}
	manifest, createErr := backupsvc.Create(ctx, mustEnv("DATABASE_URL"), objects, env("S3_BUCKET", "flopwire"), *output, backupsvc.Options{EncryptedDestination: *encrypted, AllowPendingRedactionRepair: *allowRepair})
	pool, err := pgxpool.New(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("record backup status: %w", err)
	}
	defer pool.Close()
	action := "backup.complete"
	metadata := map[string]any{"path": *output, "objects": len(manifest.Objects), "manifest_created_at": manifest.CreatedAt}
	if createErr != nil {
		action, metadata["error_class"] = "backup.failed", "backup_failed"
	}
	record := domain.AuditEvent{ID: uuid.NewString(), Action: action, TargetType: "backup", TargetID: filepath.Base(*output), CreatedAt: time.Now().UTC(), Metadata: metadata}
	if err = appendBackupAudit(ctx, pool, record); err != nil {
		if createErr != nil {
			return errors.Join(createErr, fmt.Errorf("record backup status: %w", err))
		}
		return fmt.Errorf("record backup status: %w", err)
	}
	if createErr != nil {
		return createErr
	}
	return printJSON(manifest)
}

func appendBackupAudit(ctx context.Context, pool *pgxpool.Pool, record domain.AuditEvent) error {
	var initialized bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.audit_events') IS NOT NULL`).Scan(&initialized); err != nil {
		return err
	}
	if !initialized {
		return errors.New("flopwire schema is not initialized; run flopwire serve or flopwire bootstrap before backup")
	}
	return store.NewPostgres(pool, nil, "").AppendAudit(ctx, record)
}

func runDeletionWorker(ctx context.Context, s interface {
	ProcessDeletionJobs(context.Context) (int, error)
}, log *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if _, err := s.ProcessDeletionJobs(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("deletion worker pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runChunkReconciler removes orphaned chunk uploads every 30 seconds.
// runCredentialSweeper ends expired and idle credentials and hides expired
// ephemeral devices, every few minutes.
func runCredentialSweeper(ctx context.Context, s interface {
	SweepCredentials(context.Context, time.Time) (domain.CredentialSweep, error)
}, log *slog.Logger) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		out, err := s.SweepCredentials(ctx, time.Now().UTC())
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("credential sweep", "err", err)
		case out != (domain.CredentialSweep{}):
			log.Info("credential sweep", "expired", out.Expired, "idle", out.Idle, "ephemeral_devices_swept", out.Swept)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// runBusSweeper expires undelivered messages and drops stale presence.
func runBusSweeper(ctx context.Context, s *bus.Store, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		n, err := s.Sweep(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("message bus sweep", "err", err)
		case n > 0:
			log.Info("message bus sweep", "expired", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func runChunkReconciler(ctx context.Context, s interface {
	ReconcileOrphanChunks(context.Context, int) (int, error)
}, log *slog.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		passCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if _, err := s.ReconcileOrphanChunks(passCtx, 100); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("chunk reconciliation failed", "error_class", fmt.Sprintf("%T", err))
		}
		cancel()
	}
}

func healthcheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", defaultHealthURL(), "readiness URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		return err
	}
	// A readiness probe sends no credential and reads only a status, so on
	// loopback it skips certificate checks: the self-signed or ACME
	// certificate does not name 127.0.0.1 for every mode.
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: client.IsLoopbackHost(req.URL.Hostname())}
	// An ACME listener picks its certificate by SNI, and a probe of an IP
	// sends none, so name the served domain.
	if domains := splitList(os.Getenv("FLOPWIRE_DOMAIN")); tlsCfg.InsecureSkipVerify && len(domains) > 0 {
		tlsCfg.ServerName = domains[0]
	}
	hc := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness returned %s", res.Status)
	}
	return nil
}

// defaultHealthURL probes this container's own listener with the scheme
// its TLS mode serves.
func defaultHealthURL() string {
	switch strings.ToLower(os.Getenv("FLOPWIRE_TLS")) {
	case tlsProxy, tlsOff:
		return "http://127.0.0.1:8080/readyz"
	}
	return "https://127.0.0.1:8080/readyz"
}
func backupVerifyCommand(args []string) error {
	fs := flag.NewFlagSet("backup-verify", flag.ContinueOnError)
	input := fs.String("input", "", "backup directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := backupsvc.ValidateOutput(*input); err != nil {
		return err
	}
	manifest, err := backupsvc.Verify(*input)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"verified": true, "created_at": manifest.CreatedAt, "objects": len(manifest.Objects)})
}
func restoreCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	input := fs.String("input", "", "verified backup directory")
	stateOutput := fs.String("state-output", "", "restore operation state file (default: <input>/restore-state.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := backupsvc.ValidateOutput(*input); err != nil {
		return err
	}
	if *stateOutput == "" {
		*stateOutput = filepath.Join(*input, "restore-state.json")
	}
	if err := backupsvc.ValidateOutput(*stateOutput); err != nil {
		return err
	}
	objects, err := newObjectClient()
	if err != nil {
		return err
	}
	restoreErr := backupsvc.Restore(ctx, mustEnv("DATABASE_URL"), objects, env("S3_BUCKET", "flopwire"), *input, *stateOutput)
	pool, err := pgxpool.New(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		if restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("record restore status: %w", err)
	}
	defer pool.Close()
	action := "data_restore.complete"
	metadata := map[string]any{}
	if restoreErr != nil {
		action, metadata["error_class"] = "restore.failed", "restore_failed"
	}
	var auditTable *string
	if tableErr := pool.QueryRow(ctx, `SELECT to_regclass('public.audit_events')::text`).Scan(&auditTable); tableErr == nil && auditTable != nil {
		err = store.NewPostgres(pool, nil, "").AppendAudit(ctx, domain.AuditEvent{ID: uuid.NewString(), Action: action, TargetType: "backup", TargetID: filepath.Base(*input), Metadata: metadata, CreatedAt: time.Now().UTC()})
		if err != nil && restoreErr == nil {
			return fmt.Errorf("record restore status: %w", err)
		}
	}
	if restoreErr != nil {
		return restoreErr
	}
	return printJSON(map[string]any{"data_restored": true})
}
func newObjectClient() (*minio.Client, error) {
	return minio.New(mustEnv("S3_ENDPOINT"), &minio.Options{Creds: credentials.NewStaticV4(mustEnv("S3_ACCESS_KEY"), mustEnv("S3_SECRET_KEY"), ""), Secure: envBool("S3_SECURE", false), Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}})
}

func bootstrap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	server := fs.String("server", "https://localhost:8080", "API URL")
	fingerprint := fs.String("fingerprint", "", "pin the server's self-signed certificate (from flopwire fingerprint)")
	name := fs.String("name", "", "administrator name")
	email := fs.String("email", "", "administrator email")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pin, err := client.ParseFingerprint(*fingerprint)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*email) == "" {
		return errors.New("--name and --email are required")
	}
	if normalized, err := client.NormalizeServer(*server); err != nil {
		return err
	} else if pin != "" && !strings.HasPrefix(normalized, "https://") {
		return errors.New("--fingerprint pins a TLS certificate; the server URL must be https://")
	}
	password, err := readHiddenPassword("Password: ", true)
	if err != nil {
		return err
	}
	out, err := bootstrapIdentity(ctx, mustEnv("DATABASE_URL"), *name, *email, password)
	if err != nil {
		return err
	}
	return saveAuth(ctx, *server, pin, out)
}

func login(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	server := fs.String("server", "https://localhost:8080", "API URL")
	fingerprint := fs.String("fingerprint", "", "pin the server's self-signed certificate (default: the pin already saved for this server)")
	email := fs.String("email", "", "email")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pin, err := client.ParseFingerprint(*fingerprint)
	if err != nil {
		return err
	}
	password, err := readHiddenPassword("Password: ", false)
	if err != nil {
		return err
	}
	return loginAndSave(ctx, *server, pin, *email, password)
}

func invite(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	email := fs.String("email", "", "email")
	role := fs.String("role", "member", "admin or member")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := client.Load()
	if err != nil {
		return err
	}
	sessionToken, err := cfg.SessionCredential()
	if err != nil {
		return err
	}
	var out map[string]any
	if err = cfg.API(sessionToken).JSON(ctx, "POST", "/v1/admin/invites", map[string]string{"email": *email, "role": *role}, &out); err != nil {
		return client.TrustHint(err)
	}
	// The invite string carries the server and this admin's certificate
	// pin, so the member's first connection is already verified.
	if code, _ := out["code"].(string); code != "" {
		out["invite"] = client.Invite{Server: cfg.Server, Code: code, Fingerprint: cfg.TLSFingerprint}.String()
	}
	return printJSON(out)
}
func serviceAccount(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("service-account", flag.ContinueOnError)
	name := fs.String("name", "", "service account name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := client.Load()
	if err != nil {
		return err
	}
	sessionToken, err := cfg.SessionCredential()
	if err != nil {
		return err
	}
	var out map[string]any
	if err = cfg.API(sessionToken).JSON(ctx, "POST", "/v1/admin/service-accounts", map[string]string{"name": *name}, &out); err != nil {
		return err
	}
	return printJSON(out)
}
func revokeDevice(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("revoke-device", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("device ID is required")
	}
	cfg, err := client.Load()
	if err != nil {
		return err
	}
	sessionToken, err := cfg.SessionCredential()
	if err != nil {
		return err
	}
	var out map[string]any
	if err = cfg.API(sessionToken).JSON(ctx, "POST", "/v1/admin/devices/"+fs.Arg(0)+"/revoke", nil, &out); err != nil {
		return err
	}
	return printJSON(out)
}
func claim(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	inviteStr := fs.String("invite", "", "the invite string from flopwire invite (server, code and certificate pin)")
	server := fs.String("server", "", "API URL (with --code, instead of --invite)")
	code := fs.String("code", "", "invite code (with --server, instead of --invite)")
	fingerprint := fs.String("fingerprint", "", "pin the server's self-signed certificate (with --server)")
	name := fs.String("name", "", "name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	inv, err := claimInvite(*inviteStr, *server, *code, *fingerprint)
	if err != nil {
		return err
	}
	password, err := readHiddenPassword("Password: ", true)
	if err != nil {
		return err
	}
	return claimAndSave(ctx, inv.Server, inv.Fingerprint, inv.Code, *name, password)
}

// claimInvite takes either --invite or --server with --code (and an
// optional --fingerprint).
func claimInvite(inviteStr, server, code, fingerprint string) (client.Invite, error) {
	if inviteStr != "" {
		if server != "" || code != "" || fingerprint != "" {
			return client.Invite{}, errors.New("--invite already carries the server, code and pin; drop --server, --code and --fingerprint")
		}
		return client.ParseInvite(inviteStr)
	}
	if server == "" || code == "" {
		return client.Invite{}, errors.New("pass --invite, or --server and --code")
	}
	pin, err := client.ParseFingerprint(fingerprint)
	if err != nil {
		return client.Invite{}, err
	}
	return client.Invite{Server: server, Code: code, Fingerprint: pin}, nil
}

func enroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	name := fs.String("name", hostname(), "device name")
	platform := fs.String("platform", runtime.GOOS+"-"+runtime.GOARCH, "platform")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return client.WithConfigLock(ctx, func() error {
		cfg, err := client.LoadFile()
		if err != nil {
			return err
		}
		sessionToken, err := cfg.SessionCredential()
		if err != nil {
			return err
		}
		var out struct {
			Token         string         `json:"token"`
			Device        map[string]any `json:"device"`
			ExpiresAt     time.Time      `json:"expires_at"`
			IdleExpiresAt time.Time      `json:"idle_expires_at"`
		}
		if err = cfg.API(sessionToken).JSON(ctx, "POST", "/v1/devices", map[string]string{"name": *name, "platform": *platform}, &out); err != nil {
			return client.TrustHint(err)
		}
		cfg.Token = out.Token
		cfg.DeviceID, _ = out.Device["id"].(string)
		cfg.PendingRotation = nil
		cfg.CredentialExpiresAt, cfg.IdleExpiresAt, cfg.RotatedAt, cfg.ReloginRequired = out.ExpiresAt, out.IdleExpiresAt, time.Now().UTC(), ""
		if err = client.Save(cfg); err != nil {
			return err
		}
		res := map[string]any{"device": out.Device, "config": "saved with mode 0600"}
		if cfg.TLSFingerprint != "" {
			res["tls_fingerprint"] = cfg.TLSFingerprint
		}
		return printJSON(res)
	})
}
func rawJSON(ctx context.Context, hc *http.Client, server, token, method, path string, in any, headers map[string]string, out any) error {
	raw, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(server, "/")+path, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := hc.Do(req)
	if err != nil {
		return client.TrustHint(err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		return fmt.Errorf("flopwire API %d %s", res.StatusCode, http.StatusText(res.StatusCode))
	}
	return json.NewDecoder(res.Body).Decode(out)
}
func saveAuth(ctx context.Context, server, pin string, out map[string]any) error {
	return client.WithConfigLock(ctx, func() error { return saveAuthLocked(server, pin, out) })
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		panic(k + " is required")
	}
	return v
}
func envBool(k string, fallback bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	b, _ := strconv.ParseBool(v)
	return b
}
func bootstrapIdentity(ctx context.Context, database, name, email, password string) (map[string]any, error) {
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, database)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	if err = store.Migrate(ctx, pool); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	u := domain.User{ID: uuid.NewString(), Name: strings.TrimSpace(name), Email: strings.ToLower(strings.TrimSpace(email)), Role: domain.RoleAdmin, IdentityType: domain.IdentityHuman, PasswordHash: passwordHash, CreatedAt: now}
	plain, tokenHash, err := auth.NewToken()
	if err != nil {
		return nil, err
	}
	c := domain.Credential{ID: uuid.NewString(), UserID: u.ID, Kind: domain.CredentialSession, TokenHash: tokenHash, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, Active: true}
	event := domain.AuditEvent{ID: uuid.NewString(), ActorID: u.ID, Action: "bootstrap", TargetType: "user", TargetID: u.ID, Metadata: map[string]any{"method": "local_cli"}, CreatedAt: now}
	durable := store.NewPostgres(pool, nil, "")
	if err = durable.BootstrapIdentity(ctx, u, c, event); err != nil {
		if errors.Is(err, store.ErrConflict) {
			_ = durable.AppendAudit(ctx, domain.AuditEvent{ID: uuid.NewString(), Action: "bootstrap.failed", TargetType: "deployment", Metadata: map[string]any{"reason": "human_admin_exists"}, CreatedAt: time.Now().UTC()})
			return nil, errors.New("deployment already has an administrator")
		}
		return nil, fmt.Errorf("create administrator: %w", err)
	}
	return map[string]any{"user": u, "token": plain, "expires_at": c.ExpiresAt}, nil
}
func loginAndSave(ctx context.Context, server, pin, email, password string) error {
	normalized, err := client.NormalizeServer(server)
	if err != nil {
		return err
	}
	return client.WithConfigLock(ctx, func() error {
		pin := savedPin(normalized, pin)
		var out map[string]any
		if err := rawJSON(ctx, client.Config{TLSFingerprint: pin}.HTTPClient(), normalized, "", "POST", "/v1/login", map[string]string{"email": email, "password": password}, nil, &out); err != nil {
			return err
		}
		if err := saveAuthLocked(normalized, pin, out); err != nil {
			return err
		}
		return reauthDeviceLocked(ctx, time.Now)
	})
}

// reauthDeviceLocked, after a login, gives this config's enrolled device a
// fresh credential with the new session: the 90-day deadline restarts, a
// refused credential (re-login required) works again, and a token another
// holder rotated to is revoked. The device keeps its id. A revoked device
// is not an error here: login still succeeded, and the user is told to
// enroll again. The caller holds the config lock.
func reauthDeviceLocked(ctx context.Context, now func() time.Time) error {
	cfg, err := client.LoadFile()
	if err != nil || cfg.DeviceID == "" || cfg.SessionToken == "" {
		return err
	}
	var out struct {
		Token         string    `json:"token"`
		ExpiresAt     time.Time `json:"expires_at"`
		IdleExpiresAt time.Time `json:"idle_expires_at"`
	}
	if err := cfg.API(cfg.SessionToken).JSON(ctx, "POST", "/v1/devices/"+cfg.DeviceID+"/reauth", map[string]string{}, &out); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == 404 || apiErr.StatusCode == 409) {
			fmt.Fprintln(os.Stderr, "flopwire: this device was revoked or is unknown to the server; run flopwire enroll to enroll it again")
			return nil
		}
		return fmt.Errorf("logged in, but could not renew this device's credential (run flopwire login again): %w", err)
	}
	if out.Token == "" {
		return errors.New("server returned no device credential")
	}
	cfg.Token, cfg.PendingRotation, cfg.ReloginRequired = out.Token, nil, ""
	cfg.CredentialExpiresAt, cfg.IdleExpiresAt, cfg.RotatedAt = out.ExpiresAt, out.IdleExpiresAt, now().UTC()
	if err := client.Save(cfg); err != nil {
		return err
	}
	notifyRepin(ctx) // a sync stopped by a refused credential resumes
	fmt.Fprintln(os.Stderr, "flopwire: device credential renewed")
	return nil
}
func rotateDevice(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rotate-device", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("rotate-device takes no arguments")
	}
	return client.WithConfigLock(ctx, func() error { return rotateDeviceLocked(ctx, time.Now) })
}
func rotateDeviceLocked(ctx context.Context, now func() time.Time) error {
	res, err := rotateCredentialLocked(ctx, now)
	if err != nil {
		return err
	}
	notifyRepin(ctx) // a running agent picks up the new token
	return printJSON(res)
}
func claimAndSave(ctx context.Context, server, pin, code, name, password string) error {
	normalized, err := client.NormalizeServer(server)
	if err != nil {
		return err
	}
	return client.WithConfigLock(ctx, func() error {
		pin := savedPin(normalized, pin)
		var out map[string]any
		if err := rawJSON(ctx, client.Config{TLSFingerprint: pin}.HTTPClient(), normalized, "", "POST", "/v1/invites/claim", map[string]string{"code": code, "name": name, "password": password}, nil, &out); err != nil {
			return err
		}
		return saveAuthLocked(normalized, pin, out)
	})
}

// savedPin is pin, or when pin is empty the pin already saved for server.
// Logging in again to the same server keeps its pin.
func savedPin(server, pin string) string {
	if pin != "" {
		return pin
	}
	if current, err := client.LoadFile(); err == nil {
		if cs, err := client.NormalizeServer(current.Server); err == nil && cs == server {
			return current.TLSFingerprint
		}
	}
	return ""
}

func saveAuthLocked(server, pin string, out map[string]any) error {
	token := out["token"].(string)
	normalized, err := client.NormalizeServer(server)
	if err != nil {
		return err
	}
	if pin != "" && !strings.HasPrefix(normalized, "https://") {
		return errors.New("--fingerprint pins a TLS certificate; the server URL must be https://")
	}
	cfg := client.Config{Server: normalized, Token: token, SessionToken: token, TLSFingerprint: pin}
	if current, loadErr := client.LoadFile(); loadErr == nil {
		cfg.Denylist = append([]string(nil), current.Denylist...)
		cfg.Unplaceable = current.Unplaceable
		currentServer, normalizeErr := client.NormalizeServer(current.Server)
		if normalizeErr == nil && currentServer == normalized {
			cfg = current
			cfg.Server = normalized
			cfg.SessionToken = token
			if pin != "" {
				cfg.TLSFingerprint = pin
			}
			if cfg.DeviceID == "" {
				cfg.Token = token
			}
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return fmt.Errorf("load existing config before credential save: %w", loadErr)
	}
	if err = client.Save(cfg); err != nil {
		return err
	}
	notifyRepin(context.Background()) // a sync stopped by a pin mismatch resumes
	return printJSON(map[string]any{"user": out["user"], "config": "saved with mode 0600"})
}
func envInt(k string, fallback int) int {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
func envDuration(k string, fallback time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
func envList(k string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(k), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// readHiddenPassword prompts on a terminal without echo. When stdin is not a
// terminal (scripts, tests) it reads one line per prompt instead, so a
// password never has to appear in argv or the environment.
func readHiddenPassword(prompt string, confirm bool) (string, error) {
	read := func(p string) (string, error) {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Fprint(os.Stderr, p)
			raw, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			return string(raw), err
		}
		line, err := stdinLines.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	password, err := read(prompt)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if confirm {
		confirmation, err := read("Confirm password: ")
		if err != nil {
			return "", fmt.Errorf("read password confirmation: %w", err)
		}
		if password != confirmation {
			return "", errors.New("passwords do not match")
		}
	}
	return password, nil
}

var stdinLines = bufio.NewReader(os.Stdin)

func hostname() string { h, _ := os.Hostname(); return h }
