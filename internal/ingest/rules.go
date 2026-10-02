package ingest

// Server-side path rules (D18). The device agent is the primary enforcer:
// it resolves each session's git worktree, main checkout and remote and
// its symlinks on the device, and never uploads a session a rule keeps
// local. The server applies the same admin rules again at parse time, as
// a floor against an old, modified or buggy agent, with the same
// pathpolicy matching, on what it can see without the device's file
// system:
//
//   - the working directories the transcript recorded (Claude "cwd", Codex
//     session_meta cwd or an older rollout's <cwd> tag, Devin's session
//     cwd), and every other one the session named later (Claude's
//     per-record cwd, Codex turn_context cwd and <cwd> tags; stored in
//     conversations.other_cwds): the most restrictive verdict across
//     them wins;
//   - the git remote the transcript recorded (Codex git.repository_url);
//   - for a Claude session that names no directory, its project folder
//     name, matched as the agent matches it;
//   - "~" in a rule is the device user's home as the device reports it
//     with every flush (syncproto.DeviceDirs, devices.home); until a
//     device has reported one, the home is inferred from the harness
//     directory in the source path (/Users/me/.claude/... is /Users/me),
//     which is wrong when CLAUDE_CONFIG_DIR or CODEX_HOME lies outside it.
//
// It cannot see worktree and main checkout roots, the remote git would
// report, or symlinks, so it can miss what the agent catches: a rule on a
// main checkout does not cover a session in a linked worktree here.
//
// A new session a deny or local rule covers, or an unplaceable one under
// an unplaceable=local or exclude floor, is never stored: the sink refuses
// it before writing its conversation or any message row, and the source
// is tombstoned with the rule (refuseSource). A session already stored
// that the rules now cover is hidden instead (hidden.go): retrieval leaves
// it out, and it is purged through the normal deletion machinery when an
// administrator confirms or once it has been hidden for HiddenPurgeAfter,
// unless a rule change restores it first.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// serverRules is the admin policy as the server enforces it.
type serverRules struct {
	admin       []pathpolicy.Rule // "~" not yet expanded
	unplaceable pathpolicy.Mode   // Allow when the admin set no floor
	version     int64             // collection_policy.rules_version
	swept       int64             // collection_policy.rules_swept_version
	updatedBy   string
}

// loadRules reads the admin path rules and unplaceable floor.
func loadRules(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (serverRules, error) {
	var r serverRules
	var raw []byte
	var floor string
	var by *string
	err := q.QueryRow(ctx, `SELECT path_rules,unplaceable,rules_version,rules_swept_version,updated_by::text FROM collection_policy WHERE singleton`).
		Scan(&raw, &floor, &r.version, &r.swept, &by)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	var lines []string
	if err := json.Unmarshal(raw, &lines); err != nil {
		return r, err
	}
	// Stored rules are normalized on update; one that no longer parses is
	// skipped here as the agent skips it.
	r.admin, _ = pathpolicy.ParseRules(lines)
	if m, ok := pathpolicy.ParseUnplaceable(floor); ok {
		r.unplaceable = m
	}
	if by != nil {
		r.updatedBy = *by
	}
	return r, nil
}

// empty reports whether the rules refuse nothing.
func (r serverRules) empty() bool { return len(r.admin) == 0 && r.unplaceable == pathpolicy.Allow }

// policy is the admin rules with "~" as home, as a device builds them.
func (r serverRules) policy(home string) pathpolicy.Policy {
	pol := pathpolicy.Policy{Unplaceable: r.unplaceable, UnplaceableAdmin: true}
	for _, rule := range r.admin {
		pol.Admin = append(pol.Admin, rule.ExpandHome(home))
	}
	return pol
}

// deviceDirs is what a device reported of its file system; empty fields
// were not reported.
type deviceDirs struct {
	home, claudeProjects string
}

// decide applies the admin rules to a session of the agent whose
// transcript is at sourcePath (the device's path), which recorded cwd and
// remote (either may be empty), on the device that reported dev.
func (r serverRules) decide(dev deviceDirs, agent, sourcePath, cwd, remote string) pathpolicy.Decision {
	home := dev.home
	if home == "" {
		home = homeOf(sourcePath)
	}
	d := r.decideHome(home, dev.claudeProjects, agent, sourcePath, cwd, remote)
	// Report the rule as the administrator wrote it ("~", not this home).
	for _, rule := range r.admin {
		if !d.Unplaceable && d.Mode != pathpolicy.Allow && rule.ExpandHome(home) == d.Rule {
			d.Rule = rule
			break
		}
	}
	return d
}

// decideAll is decide across a session's directories, cwd and others:
// the most restrictive verdict wins.
func (r serverRules) decideAll(dev deviceDirs, agent, sourcePath, cwd string, others []string, remote string) pathpolicy.Decision {
	d := r.decide(dev, agent, sourcePath, cwd, remote)
	for _, o := range others {
		if !absPath(o) {
			continue
		}
		if d2 := r.decide(dev, agent, sourcePath, o, remote); d2.Mode > d.Mode {
			d = d2
		}
	}
	return d
}

func (r serverRules) decideHome(home, projects, agent, sourcePath, cwd, remote string) pathpolicy.Decision {
	pol := r.policy(home)
	if !absPath(cwd) {
		cwd = ""
	}
	remote = normalizeRemote(remote)
	if cwd == "" && agent == string(transcript.AgentClaude) {
		if folder := claudeFolder(projects, sourcePath); folder != "" {
			// The agent places it by its project folder: placed, never
			// unplaceable.
			d := pathpolicy.Decision{}
			if remote != "" {
				d = pol.Decide(pathpolicy.Placement{Remote: remote})
			}
			return pol.DecideFolder(d, folder)
		}
	}
	return pol.Decide(pathpolicy.Placement{Cwd: cwd, Remote: remote})
}

// harnessDirs are the directories under a user's home that hold harness
// transcripts; the path before one is the home.
var harnessDirs = []string{"/.claude/", "/.codex/", "/.local/share/devin/", "/.pi/", "/.gemini/"}

// homeOf is the device user's home, from a source path under a harness
// directory, or "".
func homeOf(sourcePath string) string {
	p := strings.ReplaceAll(sourcePath, `\`, "/")
	best := -1
	for _, d := range harnessDirs {
		if i := strings.Index(p, d); i > 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return p[:best]
}

// claudeFolder is the Claude project folder name holding a transcript
// (<projects>/<folder>/...), or "". projects is the Claude projects
// directory the device reported; without one, the folder is what follows
// the last "/projects/" in the path.
func claudeFolder(projects, sourcePath string) string {
	p := strings.ReplaceAll(sourcePath, `\`, "/")
	var rel string
	if projects != "" {
		root := strings.TrimSuffix(strings.ReplaceAll(projects, `\`, "/"), "/") + "/"
		var ok bool
		if rel, ok = strings.CutPrefix(p, root); !ok {
			return ""
		}
	} else {
		i := strings.LastIndex(p, "/projects/")
		if i < 0 {
			return ""
		}
		rel = p[i+len("/projects/"):]
	}
	folder, rest, ok := strings.Cut(rel, "/")
	if !ok || folder == "" || rest == "" {
		return ""
	}
	return folder
}

var drivePath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// absPath reports whether a recorded working directory is absolute on the
// device (Unix, or a Windows drive or UNC path). "." names nothing.
func absPath(p string) bool {
	return strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\\`) || drivePath.MatchString(p)
}

// normalizeRemote is a recorded remote as host/owner/name, as the agent
// normalizes it.
func normalizeRemote(raw string) string { return pathpolicy.NormalizeRepo(raw) }

// remoteOf is the git remote a conversation record carries (Codex
// session_meta git.repository_url).
func remoteOf(extra map[string]any) string {
	switch g := extra["git"].(type) {
	case codex.Git:
		return g.RepositoryURL
	case *codex.Git:
		if g != nil {
			return g.RepositoryURL
		}
	case map[string]any:
		s, _ := g["repository_url"].(string)
		return s
	}
	return ""
}

// cwdTag is the directory an older Codex rollout names only in the
// <environment_context> it injects (sink.placeTagged).
var cwdTag = regexp.MustCompile(`<cwd>([^<]+)</cwd>`)

// refusal is a session the admin rules keep off the server.
type refusal struct {
	d       pathpolicy.Decision
	session string
}

func (r *refusal) Error() string {
	return fmt.Sprintf("ingest: session %s refused by %s", r.session, r.d.Reason())
}

// detail is the audit metadata of a refusal beyond its rule.
func (r *refusal) detail() map[string]any {
	return map[string]any{"mode": r.d.Mode.String(), "unplaceable": r.d.Unplaceable, "session": r.session}
}

// sessions are the sessions the gate has seen in this parse.
func (g *gate) sessions() []string {
	out := make([]string, 0, len(g.seen))
	for id := range g.seen {
		out = append(out, id)
	}
	return out
}

// ruleName is how a refusal's rule is stored and reported.
func ruleName(d pathpolicy.Decision) string {
	if d.Unplaceable {
		return "unplaceable=" + pathpolicy.UnplaceableName(d.Mode)
	}
	return d.Rule.String()
}

// gate applies the admin rules to a parse's sessions before the sink
// writes them.
type gate struct {
	rules serverRules
	src   source
	path  string
	dev   deviceDirs
	pool  *pgxpool.Pool
	seen  map[string]*placeHint
	// hadStored: the source had conversations stored before this parse,
	// so a session the rules cover is hidden, not refused.
	hadStored bool
	// hide holds the sessions the rules cover that are (or will be) stored
	// all the same: they are written hidden.
	hide map[string]pathpolicy.Decision
}

func newGate(rules serverRules, src source, path string, dev deviceDirs, pool *pgxpool.Pool, hadStored bool) *gate {
	return &gate{rules: rules, src: src, path: path, dev: dev, pool: pool, seen: map[string]*placeHint{}, hadStored: hadStored,
		hide: map[string]pathpolicy.Decision{}}
}

// placeHint is what a parse has learned of a session's place.
type placeHint struct {
	cwd, remote string
	others      []string // other directories the session named
	looked      bool     // the stored conversation was consulted
	stored      *bool    // the session has a stored conversation on this device
}

// check refuses the sink's pending sessions that the rules cover. It runs
// before every write: a session's record (and its cwd) comes with or
// before its first messages, and a pending batch holds up to sinkBatch
// messages, so a session that names its directory at all names it before
// anything is written. One that has named nothing by then is decided as
// unplaceable, as the agent decides after its first lines. Likewise a
// directory named later: the parsers re-emit the record, with it in
// OtherCwds, before the rows of the line that names it.
//
// A covered session is refused only when nothing of it or its source is
// stored yet (a new upload). One already stored (an append, a new
// generation, another file of the session) is written hidden, as a rule
// change hides it, so a rule removed within the window restores it whole.
func (g *gate) check(ctx context.Context, s *sink) error {
	sessions := map[string]bool{}
	for id := range s.convs {
		sessions[id] = true
	}
	for _, m := range s.msgs {
		sessions[m.SessionID] = true
	}
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		h := g.hint(ctx, s, id)
		d := g.rules.decideAll(g.dev, g.src.agent, g.path, h.cwd, h.others, h.remote)
		if d.Mode == pathpolicy.Allow {
			delete(g.hide, id)
			continue
		}
		stored, err := g.stored(ctx, h, id)
		if err != nil {
			return err
		}
		if !g.hadStored && !stored {
			return &refusal{d: d, session: id}
		}
		g.hide[id] = d
	}
	return nil
}

// stored reports whether the session has a stored conversation on this
// device.
func (g *gate) stored(ctx context.Context, h *placeHint, id string) (bool, error) {
	if h.stored == nil {
		var ok bool
		if err := g.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE device_id=$1 AND agent=$2 AND session_id=$3)`,
			g.src.deviceID, g.src.agent, id).Scan(&ok); err != nil {
			return false, err
		}
		h.stored = &ok
	}
	return *h.stored, nil
}

func (g *gate) hint(ctx context.Context, s *sink, id string) *placeHint {
	h := g.seen[id]
	if h == nil {
		h = &placeHint{}
		g.seen[id] = h
	}
	if c := s.convs[id]; c != nil {
		if c.Cwd != "" {
			if h.cwd != "" && h.cwd != c.Cwd {
				h.addOther(h.cwd)
			}
			h.cwd = c.Cwd
		}
		for _, o := range c.OtherCwds {
			h.addOther(o)
		}
		if r := remoteOf(c.Extra); r != "" {
			h.remote = r
		}
	}
	if !h.looked {
		// An incremental parse may not repeat the record, nor every
		// directory named before: add the stored ones.
		h.looked = true
		var cwd, remote *string
		var others []string
		err := g.pool.QueryRow(ctx, `SELECT cwd,extra->'git'->>'repository_url',other_cwds FROM conversations WHERE device_id=$1 AND agent=$2 AND session_id=$3`,
			g.src.deviceID, g.src.agent, id).Scan(&cwd, &remote, &others)
		if err == nil {
			if h.cwd == "" {
				h.cwd = deref(cwd)
			} else if c := deref(cwd); c != "" && c != h.cwd {
				h.addOther(c)
			}
			if h.remote == "" {
				h.remote = deref(remote)
			}
			for _, o := range others {
				h.addOther(o)
			}
		}
	}
	return h
}

func (h *placeHint) addOther(d string) {
	if d != "" && d != h.cwd && !slices.Contains(h.others, d) {
		h.others = append(h.others, d)
	}
}

// refuseSource's deletes: the conversations of the refused sources ($1)
// or of their sessions on the device, then any message rows still naming
// the sources (superseded ones included).
//
// refuseLockSQL first locks those conversations in the order a parse flush
// and a checkpoint take them (store.LockConversationsSQL): the DELETE alone
// would lock them in plan order and could deadlock with a checkpoint. It
// locks their linked subagents too: the DELETE clears their
// parent_conversation_id (ON DELETE SET NULL), after the parent, while a
// hide of the parent's tree locks both in session order.
const (
	refuseLockSQL = `SELECT 1 FROM conversations WHERE id IN (
		SELECT id FROM conversations WHERE source_id=ANY($1::uuid[]) OR (device_id=$2 AND agent=$3 AND session_id=ANY($4))
		UNION SELECT k.id FROM conversations p JOIN conversations k ON k.parent_conversation_id=p.id
		WHERE p.source_id=ANY($1::uuid[]) OR (p.device_id=$2 AND p.agent=$3 AND p.session_id=ANY($4)))
	ORDER BY session_id COLLATE "C",id FOR UPDATE`
	refuseConversationsSQL = `DELETE FROM conversations WHERE source_id=ANY($1::uuid[]) OR (device_id=$2 AND agent=$3 AND session_id=ANY($4))`
	refuseMessagesSQL      = `DELETE FROM messages WHERE source_id=ANY($1::uuid[])`
)

// refuseSource drops everything the server holds of a source the admin
// rules refuse: the conversations and messages of its sessions on this
// device, its raw evidence (generations; chunks nothing else references
// go to the orphan reconciler), and that of its companions and subagent
// files. The source row stays, tombstoned with the rule, so later uploads
// are acknowledged, discarded and reported to the device. Like a deletion
// purge it holds the purge lock (tried, not waited for), and it is
// audited with the user, device and rule.
func refuseSource(ctx context.Context, pool *pgxpool.Pool, src source, path string, sessions []string, rule string, detail map[string]any) error {
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, store.PurgeLockID).Scan(&got); err != nil {
			return err
		}
		if !got {
			return errPurgeBusy
		}
		slices.Sort(sessions)
		for _, s := range slices.Compact(sessions) {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, store.ConversationLockKey(src.userID, src.agent, s)); err != nil {
				return err
			}
		}
		ids, released, err := purgeSourceTx(ctx, tx, src.id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, refuseLockSQL, ids, src.deviceID, src.agent, sessions); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, refuseConversationsSQL,
			ids, src.deviceID, src.agent, sessions)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, refuseMessagesSQL, ids); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sources SET refused_rule=COALESCE(refused_rule,$2) WHERE id=ANY($1::uuid[])`, ids, rule); err != nil {
			return err
		}
		meta := map[string]any{"rule": rule, "agent": src.agent, "path": path, "sessions": sessions, "sources": len(ids),
			"conversations": tag.RowsAffected(), "chunks_released": released}
		for k, v := range detail {
			meta[k] = v
		}
		return store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), ActorID: src.userID, DeviceID: src.deviceID,
			Action: "source.refused", TargetType: "source", TargetID: src.id,
			Metadata: meta, CreatedAt: time.Now().UTC()})
	})
}

// parentRefused is the rule that refused the source's parent (the
// transcript a companion or subagent file belongs to), or "".
func parentRefused(ctx context.Context, pool *pgxpool.Pool, parentID *string) (string, error) {
	if parentID == nil {
		return "", nil
	}
	var rule *string
	err := pool.QueryRow(ctx, `SELECT refused_rule FROM sources WHERE id=$1`, *parentID).Scan(&rule)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return deref(rule), err
}

// recheckStored applies rules to the conversations a source's parse left
// stored. A parse that decided under older rules re-checks with the
// current ones, so a rule change that the stored-session sweep ran before
// this parse committed still covers it.
func recheckStored(ctx context.Context, pool *pgxpool.Pool, r serverRules, src source, path string, dev deviceDirs) (*refusal, []string, error) {
	rows, err := pool.Query(ctx, `SELECT session_id,COALESCE(cwd,''),COALESCE(extra->'git'->>'repository_url',''),other_cwds FROM conversations
		WHERE source_id=$1 ORDER BY session_id`, src.id)
	if err != nil {
		return nil, nil, err
	}
	type conv struct {
		session, cwd, remote string
		others               []string
	}
	convs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (conv, error) {
		var c conv
		return c, row.Scan(&c.session, &c.cwd, &c.remote, &c.others)
	})
	if err != nil {
		return nil, nil, err
	}
	var sessions []string
	var ref *refusal
	for _, c := range convs {
		sessions = append(sessions, c.session)
		if d := r.decideAll(dev, src.agent, path, c.cwd, c.others, c.remote); d.Mode != pathpolicy.Allow && ref == nil {
			ref = &refusal{d: d, session: c.session}
		}
	}
	return ref, sessions, nil
}

// Refused counts the sources the server refused or purged by an admin
// path rule.
func (q *Queue) Refused(ctx context.Context) (int64, error) {
	var n int64
	err := q.Pool.QueryRow(ctx, `SELECT count(*) FROM sources WHERE refused_rule IS NOT NULL`).Scan(&n)
	return n, err
}

// AdminDecision is the server's verdict on a session under the admin path
// rules and unplaceable floor, as a parse decides it: home is the home the
// device reported ("" infers it from sourcePath), agent the session's
// harness, sourcePath its transcript's device path, cwd and remote what
// the transcript recorded (either may be empty).
func AdminDecision(rules []string, unplaceable, home, agent, sourcePath, cwd, remote string) pathpolicy.Decision {
	r := serverRules{}
	r.admin, _ = pathpolicy.ParseRules(rules)
	if m, ok := pathpolicy.ParseUnplaceable(unplaceable); ok {
		r.unplaceable = m
	}
	return r.decide(deviceDirs{home: home}, agent, sourcePath, cwd, remote)
}
