// Package retrieval answers the retrieval tools (spec §7.3, §8) from the
// server's Postgres message index and chunk archive, with the semantics
// of the local index (internal/retrieval/grep, internal/retrieval/format):
// grep over pg_trgm verified in Go, ranked search over the tsvector,
// sessions, read by address (SESSION/ORDINAL[:LINE], a session, a message
// id, a transcript path:line), and raw bytes by manifest arithmetic.
//
// Every tool applies the default view unless a filter widens it: rows not
// superseded and not off the active path (§4.2). Deleted conversations
// have no rows left to find; conversations hidden by an admin path rule
// change (D18, internal/ingest hidden.go) are left out of every tool,
// raw bytes included.
package retrieval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/regexq"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultLimit = 20
	MaxLimit     = 500
	// MaxRaw bounds one raw read.
	MaxRaw = 16 << 20
	// maxContext bounds before/after of one context call.
	maxContext = 200
	// maxConversation bounds the messages one conversation call returns.
	maxConversation = 5000
)

// The backend's error kinds are shared with every retrieval backend.
var (
	ErrNotFound   = format.ErrNotFound
	ErrBadRequest = format.ErrBadRequest
)

// Store is the server-side retrieval backend.
type Store struct {
	Pool    *pgxpool.Pool
	Objects ingest.Objects
	// RefreshSession prioritizes background re-derivation after an authorized read.
	RefreshSession func(context.Context, string)
	tx             pgx.Tx // the budgeted transaction, on the copy budgeted hands on
}

// db is where the Store's lookups run: its budgeted transaction, else the
// pool.
func (s *Store) db() interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
} {
	if s.tx != nil {
		return s.tx
	}
	return s.Pool
}

// budgeted runs fn with a copy of s whose lookups share one read-only
// transaction under the query budget's statement_timeout (D8), as grep
// and search do. A spent budget is an error that says so and wraps
// context.DeadlineExceeded.
func (s *Store) budgeted(ctx context.Context, fn func(*Store) error) error {
	b := budget(ctx)
	err := s.read(ctx, b, func(tx pgx.Tx) error {
		c := *s
		c.tx = tx
		return fn(&c)
	})
	return spent(ctx, b, err)
}

// spent turns a query stopped by budget b (its statement_timeout or ctx's
// deadline) into an error that says so and wraps context.DeadlineExceeded.
// It classifies err as observed now, as cutShort does.
func spent(ctx context.Context, b time.Duration, err error) error {
	if timedOut(cutShort(ctx, err)) {
		return fmt.Errorf("timed out after %s: %w", b.Round(100*time.Millisecond), context.DeadlineExceeded)
	}
	return err
}

// query accumulates a WHERE clause and its arguments.
type query struct {
	conds []string
	args  []any
}

func (q *query) arg(v any) string {
	q.args = append(q.args, v)
	return "$" + strconv.Itoa(len(q.args))
}

func (q *query) where(c string) { q.conds = append(q.conds, c) }

func (q *query) sql() string {
	if len(q.conds) == 0 {
		return "true"
	}
	return strings.Join(q.conds, " AND ")
}

// visible is the conversations retrieval may show: not hidden by an admin
// path rule change (D18).
const visible = `(SELECT * FROM conversations WHERE hidden_at IS NULL)`

// listed is visible with each conversation's hot fields
// (conversation_activity: last_activity_at, digest, digest_stale), for
// the queries that list or describe conversations (convCols). aid is the
// id as the activity row holds it: a keyset bound on
// (last_activity_at, aid) is an index condition on
// conversation_activity_idx, one on (last_activity_at, id) is not (the
// planner does not carry a range on id across the join). Visibility is
// NOT a.hidden, which mirrors c.hidden_at (002_ingest.sql): it matches
// that index's predicate, so a page walks visible conversations only.
// Testing c.hidden_at as well would make the planner multiply two
// selectivities that are one, underestimate the visible rows and, with
// many hidden, hash-join a scan of every conversation.
const listed = `(SELECT c.*,a.conversation_id AS aid,a.last_activity_at,a.digest,a.digest_stale FROM conversations c
	JOIN conversation_activity a ON a.conversation_id=c.id WHERE NOT a.hidden)`

// lastActivity is conversation c's last activity where c is a plain
// conversations row (a message's conversation in from).
func lastActivity(c string) string {
	return `(SELECT a.last_activity_at FROM conversation_activity a WHERE a.conversation_id=` + c + `.id)`
}

// from joins a message to what its filters and output need.
const from = `messages m JOIN ` + visible + ` c ON c.id=m.conversation_id JOIN devices d ON d.id=c.device_id
	JOIN users u ON u.id=c.user_id LEFT JOIN sources s ON s.id=m.source_id`

// hitCols are scanned by scanHit, in order.
const hitCols = `m.id::text,c.id::text,c.agent,c.session_id,COALESCE(c.title,''),COALESCE(c.repo_root,c.cwd,''),d.name,u.email,c.branches,
	m.kind,COALESCE(m.tool_name,''),COALESCE(m.is_error,false),m.ts,m.superseded,COALESCE(m.on_active_path,true),
	COALESCE(s.id::text,''),COALESCE(s.path,''),COALESCE(s.file_id,''),m.source_generation,COALESCE(m.line_no,0),m.byte_offset,COALESCE(m.byte_len,0),COALESCE(m.locator,''),
	m.ordinal,CASE WHEN s.storage_kind='cass_export' THEN 'cass_recovery' ELSE '' END,
 CASE WHEN s.storage_kind='cass_export' THEN COALESCE(c.extra->>'cass_source_path','') ELSE '' END`

func scanHit(row pgx.Row, extra ...any) (format.Hit, error) {
	var h format.Hit
	var onPath bool
	dst := []any{&h.MessageID, &h.ConversationID, &h.Agent, &h.SessionID, &h.Title, &h.Repo, &h.Device, &h.User, &h.Branches,
		&h.Kind, &h.ToolName, &h.IsError, &h.TS, &h.Superseded, &onPath,
		&h.Provenance.SourceID, &h.Provenance.Path, &h.Provenance.FileID, &h.Provenance.Generation, &h.Provenance.LineNo, &h.Provenance.ByteOffset, &h.Provenance.ByteLen, &h.Provenance.Locator,
		&h.Ordinal, &h.Provenance.EvidenceKind, &h.Provenance.OriginalPath}
	err := row.Scan(append(dst, extra...)...)
	h.OffPath = !onPath
	if h.Provenance.Locator != "" && strings.HasPrefix(h.Provenance.Locator, "@") {
		h.Provenance.Locator = "" // byte-offset key, already in ByteOffset
	}
	return h, err
}

// repoWhere adds f's repo condition: Repo as format.RepoMatch reads it
// (a name is resolved first, resolveFilterRepo), a directory that is one
// of RepoRoots or lies under one, or an upload placed in a checkout of
// one of RepoRemotes or in one of RepoCheckouts.
func repoWhere(q *query, f format.Filters) {
	var ors []string
	if prefix, like := format.RepoMatch(f.Repo); prefix != "" {
		a := q.arg(prefix)
		ors = append(ors, fmt.Sprintf("c.repo_root=%s OR c.cwd=%s OR c.cwd LIKE %s OR c.repo_root LIKE %s", a, a, q.arg(likeEscape(prefix)+"/%"), q.arg(likeEscape(prefix)+"/%")))
	} else if like != "" {
		ors = append(ors, "COALESCE(c.repo_root,c.cwd) ILIKE "+q.arg(like))
	}
	if len(f.RepoRoots) > 0 {
		roots := make([]string, 0, len(f.RepoRoots))
		likes := make([]string, 0, len(f.RepoRoots))
		for _, r := range f.RepoRoots {
			r = strings.TrimSuffix(r, "/")
			roots, likes = append(roots, r), append(likes, likeEscape(r)+"/%")
		}
		a, l := q.arg(roots), q.arg(likes)
		ors = append(ors, fmt.Sprintf("c.repo_root=ANY(%[1]s) OR c.cwd=ANY(%[1]s) OR c.cwd LIKE ANY(%[2]s) OR c.repo_root LIKE ANY(%[2]s)", a, l))
	}
	if len(f.RepoRemotes) > 0 {
		ors = append(ors, "c.source_id IN (SELECT id FROM sources WHERE remote=ANY("+q.arg(f.RepoRemotes)+"))")
	}
	if len(f.RepoCheckouts) > 0 {
		devs := make([]string, len(f.RepoCheckouts))
		dirs := make([]string, len(f.RepoCheckouts))
		for i, dc := range f.RepoCheckouts {
			devs[i], dirs[i] = dc.Device, dc.Checkout
		}
		ors = append(ors, fmt.Sprintf("c.source_id IN (SELECT s.id FROM sources s JOIN unnest(%s::uuid[],%s::text[]) k(device,checkout) ON s.device_id=k.device AND s.checkout=k.checkout)", q.arg(devs), q.arg(dirs)))
	}
	if len(ors) > 0 {
		q.where("(" + strings.Join(ors, " OR ") + ")")
	}
}

// filters adds the WHERE conditions of f.
func filters(q *query, f format.Filters) error {
	if !f.IncludeSuperseded {
		q.where("NOT m.superseded")
	}
	if !f.IncludeBranches {
		q.where("m.on_active_path IS NOT FALSE")
	}
	if f.ExcludeSubagents {
		q.where("c.depth=0 AND c.parent_native_session_id IS NULL")
	}
	if f.Agent != "" {
		q.where("c.agent=ANY(" + q.arg(format.List(f.Agent)) + ")")
	}
	repoWhere(q, f)
	if f.Device != "" {
		a := q.arg(f.Device)
		q.where(fmt.Sprintf("(d.id::text=%s OR d.name=%s)", a, a))
	}
	if f.User != "" {
		a := q.arg(f.User)
		q.where(fmt.Sprintf("(u.id::text=%s OR lower(u.email)=lower(%s))", a, a))
	}
	if len(f.Kinds) > 0 {
		q.where("m.kind=ANY(" + q.arg(f.Kinds) + ")")
	}
	if len(f.ExcludeKinds) > 0 {
		q.where("m.kind<>ALL(" + q.arg(f.ExcludeKinds) + ")")
	}
	if len(f.Tools) > 0 {
		lower := make([]string, len(f.Tools))
		for i, t := range f.Tools {
			lower[i] = strings.ToLower(t)
		}
		q.where("lower(m.tool_name)=ANY(" + q.arg(lower) + ")")
	}
	if !f.Since.IsZero() {
		q.where("m.ts>=" + q.arg(f.Since))
	}
	if !f.Until.IsZero() {
		q.where("m.ts<" + q.arg(f.Until))
	}
	if f.ExcludeConversation != "" {
		if _, err := uuid.Parse(f.ExcludeConversation); err != nil {
			return fmt.Errorf("%w: exclude_conversation is not a conversation id", ErrBadRequest)
		}
		q.where("c.id<>" + q.arg(f.ExcludeConversation))
	}
	if f.Session != "" {
		q.where("c.session_id IN (" + sessionTree(q, f.Session) + ")")
		if f.Self {
			q.where("c.user_id=" + q.arg(f.Owner) + "::uuid")
		}
	}
	if f.ExcludeSession != "" {
		q.where(excludeSessionTree(q, f.ExcludeSession))
	}
	convFilters(q, f)
	return nil
}

// convFilters adds the conditions on conversation columns that grep,
// search and sessions share: the branch and live sessions.
func convFilters(q *query, f format.Filters) {
	if f.Branch != "" {
		q.where("EXISTS (SELECT 1 FROM unnest(c.branches) b WHERE b ILIKE " + q.arg(format.BranchMatch(f.Branch)) + ")")
	}
	if f.ExcludeLive {
		q.where("COALESCE(" + lastActivity("c") + ",'-infinity') < " + q.arg(time.Now().Add(-format.LiveWindow)))
		// Sessions (and their subagents) the device reports open, while
		// they (or the parent) wrote within the hour, as liveSQL has it.
		q.where("NOT COALESCE(d.live_at>now()-interval '1 hour' AND " + heldOpen("d.live_sessions") + ",false)")
		if len(f.Live) > 0 {
			q.where("NOT COALESCE(" + heldOpen(q.arg(f.Live)) + ",false)")
		}
	}
}

// heldOpen is whether conversation c is held open by its harness per
// the session id list ids: it is named there and wrote within the hour,
// or its parent (on its device) is named and wrote within the hour.
func heldOpen(ids string) string {
	return `(c.session_id=ANY(` + ids + `) AND ` + lastActivity("c") + `>now()-interval '1 hour'
		OR c.parent_native_session_id=ANY(` + ids + `) AND EXISTS (SELECT 1 FROM conversations p WHERE p.device_id=c.device_id
			AND p.session_id=c.parent_native_session_id AND ` + lastActivity("p") + `>now()-interval '1 hour'))`
}

// excludeSessionTree is the condition leaving out a session and its
// subagents at every depth (they name their parent's native session id),
// as the local index does.
func excludeSessionTree(q *query, session string) string {
	return `c.session_id NOT IN (` + sessionTree(q, session) + `)`
}

// sessionTree selects the session ids of session and its subagents.
func sessionTree(q *query, session string) string {
	return `WITH RECURSIVE tree(sid) AS (SELECT ` + q.arg(session) + `::text
		UNION SELECT k.session_id FROM conversations k JOIN tree ON k.parent_native_session_id=tree.sid) SELECT sid FROM tree`
}

// hitFilters is filters for search and find: they also leave out injected
// rows (CLAUDE.md, AGENTS.md, system reminders) unless f names kinds, as
// the local index does (decision D6). Context and conversation show them.
func hitFilters(q *query, f format.Filters) error {
	if len(f.Kinds) == 0 {
		q.where("m.kind<>'injected'")
	}
	return filters(q, f)
}

func limit(f format.Filters) int {
	switch {
	case f.Limit <= 0:
		return DefaultLimit
	case f.Limit > MaxLimit:
		return MaxLimit
	}
	return f.Limit
}

// trigramCond renders a trigram query as conditions on m.text that the
// pg_trgm index answers: every required trigram becomes an ILIKE of it
// (a superset of the matches, since the index folds case). pg_trgm indexes
// only runs of letters and digits, so a trigram holding anything else
// constrains nothing. It returns "" when nothing constrains the text, and
// "false" when nothing can match.
func trigramCond(q *query, n *regexq.Query) string {
	t := indexable(n)
	if t == nil {
		return ""
	}
	return renderCond(q, t)
}

// cond is a trigram query reduced to what the index can answer.
type cond struct {
	op   regexq.QueryOp
	tris []string
	sub  []*cond
}

// indexable reduces n to its index-usable part; nil means unconstrained.
func indexable(n *regexq.Query) *cond {
	switch n.Op {
	case regexq.QAll:
		return nil
	case regexq.QNone:
		return &cond{op: regexq.QNone}
	}
	c := &cond{op: n.Op}
	for _, t := range n.Trigram {
		if !wordTrigram(t) {
			if n.Op == regexq.QOr {
				return nil
			}
			continue
		}
		c.tris = append(c.tris, t)
	}
	for _, s := range n.Sub {
		sc := indexable(s)
		if sc == nil {
			if n.Op == regexq.QOr {
				return nil
			}
			continue
		}
		c.sub = append(c.sub, sc)
	}
	if len(c.tris) == 0 && len(c.sub) == 0 {
		return nil // an AND of nothing constrains nothing
	}
	return c
}

func renderCond(q *query, c *cond) string {
	if c.op == regexq.QNone {
		return "false"
	}
	var parts []string
	for _, t := range c.tris {
		parts = append(parts, "m.text ILIKE "+q.arg("%"+likeEscape(t)+"%"))
	}
	for _, s := range c.sub {
		parts = append(parts, renderCond(q, s))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	sep := " AND "
	if c.op == regexq.QOr {
		sep = " OR "
	}
	return "(" + strings.Join(parts, sep) + ")"
}

// likeIndexable reports whether pg_trgm narrows ILIKE '%s%' with a
// trigram holding at least two letters or digits. pg_trgm takes each run
// of letters and digits in the pattern, pads it with blanks on a side
// that a non-word character bounds (two before, one after; never at the
// pattern's ends, which meet the % wildcards), and indexes its trigrams:
// "go to" gives "go " and " to", while "a b" gives only "a " and "  b".
func likeIndexable(s string) bool {
	rs := []rune(s)
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	for i := 0; i < len(rs); {
		if !word(rs[i]) {
			i++
			continue
		}
		j := i
		for j < len(rs) && word(rs[j]) {
			j++
		}
		if n := j - i; n >= 3 || n == 2 && (i > 0 || j < len(rs)) {
			return true
		}
		i = j
	}
	return false
}

// wordTrigram reports whether pg_trgm can index t: three letters or digits.
func wordTrigram(t string) bool {
	n := 0
	for _, r := range t {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
		n++
	}
	return n == 3
}

// DefaultBudget and MaxBudget bound one retrieval call (D8): the
// statement_timeout of its queries. A caller raises the budget with a
// context deadline, up to MaxBudget.
const (
	DefaultBudget = 10 * time.Second
	MaxBudget     = 60 * time.Second
)

// budget is the query budget of ctx: the time to its deadline, else
// DefaultBudget, never above MaxBudget.
func budget(ctx context.Context) time.Duration {
	b := DefaultBudget
	if dl, ok := ctx.Deadline(); ok {
		b = time.Until(dl)
	}
	return min(max(b, 100*time.Millisecond), MaxBudget)
}

// followUp is the context for the lookups that finish a page after its
// budgeted query (session infos, addresses): a spent budget must not turn
// the hits already verified into an error, so it drops ctx's deadline for
// a short one of its own, and still ends when ctx is cancelled.
func followUp(ctx context.Context) (context.Context, context.CancelFunc) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	stop := context.AfterFunc(ctx, func() {
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			cancel()
		}
	})
	return fctx, func() { stop(); cancel() }
}

// read runs fn in a read-only transaction whose statements Postgres stops
// after b, so a slow query cannot hold a pooled connection past its budget.
func (s *Store) read(ctx context.Context, b time.Duration, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return cutShort(ctx, err)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()
	if err := setTimeout(ctx, tx, b); err != nil {
		return cutShort(ctx, err)
	}
	return cutShort(ctx, fn(tx))
}

// setTimeout sets the transaction's statement_timeout to d.
func setTimeout(ctx context.Context, tx pgx.Tx, d time.Duration) error {
	_, err := tx.Exec(ctx, `SELECT set_config('statement_timeout',$1,true)`, strconv.FormatInt(max(d.Milliseconds(), 1), 10))
	return err
}

// cutShort reports err, just observed, as context.DeadlineExceeded when
// ctx's deadline caused it: a connection i/o timeout while ctx's deadline
// has passed; and as context.Canceled when the caller's cancel did. When
// ctx ends, pgx interrupts the read or write in flight by
// setting a deadline on the net.Conn; it turns an interrupted read into a
// context error but returns an interrupted write's i/o timeout as is
// (#121). Any other error, an i/o timeout before the deadline included,
// is returned unchanged, so a genuine transport error still surfaces.
// Call it where err is observed: ctx's error only says what caused err
// while the two are close in time.
func cutShort(ctx context.Context, err error) error {
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	if cause := ctx.Err(); cause != nil {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

// timedOut reports a query stopped by its statement timeout or deadline
// (cutShort: or by a transport timeout the deadline caused).
func timedOut(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "57014" || errors.Is(err, context.DeadlineExceeded)
}

const msgCols = `m.id::text,COALESCE(m.native_id,''),m.ordinal,m.kind,COALESCE(m.role,''),COALESCE(m.tool_name,''),COALESCE(m.tool_call_id,''),
	COALESCE(m.is_error,false),m.ts,m.text,m.text_len,m.version,m.superseded,COALESCE(m.on_active_path,true),
	COALESCE(s.id::text,''),COALESCE(s.path,''),m.source_generation,COALESCE(m.line_no,0),m.byte_offset,COALESCE(m.byte_len,0),COALESCE(m.locator,''),
	c.session_id,CASE WHEN s.storage_kind='cass_export' THEN 'cass_recovery' ELSE '' END,
 CASE WHEN s.storage_kind='cass_export' THEN COALESCE(c.extra->>'cass_source_path','') ELSE '' END`

// scanMessage scans msgCols. Address holds the full session id until
// addresses shorten it.
func scanMessage(row pgx.Row) (format.Message, error) {
	var m format.Message
	var onPath bool
	err := row.Scan(&m.ID, &m.NativeID, &m.Ordinal, &m.Kind, &m.Role, &m.ToolName, &m.ToolCallID, &m.IsError, &m.TS, &m.Text, &m.TextLen, &m.Version,
		&m.Superseded, &onPath, &m.Provenance.SourceID, &m.Provenance.Path, &m.Provenance.Generation, &m.Provenance.LineNo, &m.Provenance.ByteOffset,
		&m.Provenance.ByteLen, &m.Provenance.Locator, &m.Address, &m.Provenance.EvidenceKind, &m.Provenance.OriginalPath)
	if err != nil {
		return m, err
	}
	m.OffPath = !onPath
	if strings.HasPrefix(m.Provenance.Locator, "@") {
		m.Provenance.Locator = ""
	}
	return m, nil
}

const convCols = `c.id::text,c.agent,c.session_id,COALESCE(c.title,''),COALESCE(c.cwd,''),COALESCE(c.repo_root,c.cwd,''),d.name,u.email,
	c.started_at,c.last_activity_at,COALESCE(c.parent_conversation_id::text,''),COALESCE(c.spawned_by_message_id::text,''),c.depth,
	COALESCE(pc.session_id,c.parent_native_session_id,''),
	` + messageCount + `,
	c.branches,c.digest,` + liveSQL

// messageCount is conversation c's live message count (live rows on the
// active path). The digest holds it per kind, maintained by every append
// and recounted whenever rows are replaced or superseded, so reading it
// costs the same whatever the session's length. While a parse that
// replaced rows has not recounted yet (digest_stale), or before the first
// digest, the rows are counted.
const messageCount = `CASE WHEN c.digest IS NULL OR c.digest_stale
	THEN (SELECT count(*) FROM messages mm WHERE mm.conversation_id=c.id AND NOT mm.superseded AND mm.on_active_path IS NOT FALSE)
	ELSE (SELECT COALESCE(sum(v::bigint),0) FROM jsonb_each_text(c.digest->'messages') x(k,v)) END`

// liveSQL is whether a conversation c of device d is live: active within
// format.LiveWindow, or reported open by its device (devices.live_sessions,
// within local.LiveCap of the report and of its last activity).
const liveSQL = `COALESCE(c.last_activity_at>now()-interval '10 minutes' OR (c.session_id=ANY(d.live_sessions)
	AND d.live_at>now()-interval '1 hour' AND c.last_activity_at>now()-interval '1 hour'),false)`

// scanConv scans convCols. ParentSession holds the parent's full session
// id until addresses shorten it.
func scanConv(row pgx.Row) (format.ConversationInfo, error) {
	var c format.ConversationInfo
	var dg []byte
	err := row.Scan(&c.ID, &c.Agent, &c.SessionID, &c.Title, &c.Cwd, &c.Repo, &c.Device, &c.User, &c.StartedAt, &c.LastActivityAt,
		&c.ParentConversationID, &c.SpawnedByMessageID, &c.Depth, &c.ParentSession, &c.Messages, &c.Branches, &dg, &c.Live)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if len(c.Branches) == 0 {
		c.Branches = nil
	}
	c.Digest = format.ParseDigest(dg)
	return c, err
}

const convFrom = listed + ` c` + convJoins

// convJoins joins a conversation c to what convCols needs.
const convJoins = ` JOIN devices d ON d.id=c.device_id JOIN users u ON u.id=c.user_id
	LEFT JOIN ` + visible + ` pc ON pc.id=c.parent_conversation_id`

// Raw returns bytes [offset, offset+length) of a source generation,
// fetching only the chunk objects the range touches (or the provisional
// tail), and whose evidence they are. The range is clipped to the
// generation's end.
func (s *Store) Raw(ctx context.Context, sourceID string, generation, offset, length int64) ([]byte, format.Attribution, error) {
	var a format.Attribution
	if _, err := uuid.Parse(sourceID); err != nil {
		return nil, a, ErrNotFound
	}
	if generation < 0 {
		return nil, a, fmt.Errorf("%w: need generation >= 0", ErrBadRequest)
	}
	err := s.budgeted(ctx, func(s *Store) (err error) {
		a, err = s.attribution(ctx, sourceID, generation, offset, length)
		return err
	})
	if err != nil {
		return nil, a, err
	}
	return s.fetch(ctx, a, offset, length)
}

// RawByPath is Raw for a source named the way its device knows it: the
// device's own (path, file_id), an empty file_id meaning the newest source
// at the path, and generation -1 meaning the latest. The device comes from
// the caller's credential, so a device reads back only what it uploaded.
func (s *Store) RawByPath(ctx context.Context, deviceID, path, fileID string, generation, offset, length int64) ([]byte, format.Attribution, error) {
	var a format.Attribution
	if path == "" {
		return nil, a, fmt.Errorf("%w: path is required", ErrBadRequest)
	}
	err := s.budgeted(ctx, func(s *Store) error {
		var sourceID string
		err := s.db().QueryRow(ctx, `SELECT id::text FROM sources WHERE device_id=$1 AND path=$2 AND ($3='' OR file_id=$3) AND tombstoned_at IS NULL
			ORDER BY first_seen_at DESC LIMIT 1`, deviceID, path, fileID).Scan(&sourceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if generation < 0 {
			if err := s.db().QueryRow(ctx, `SELECT COALESCE(max(generation),-1) FROM generations WHERE source_id=$1`, sourceID).Scan(&generation); err != nil {
				return err
			}
			if generation < 0 {
				return ErrNotFound
			}
		}
		a, err = s.attribution(ctx, sourceID, generation, offset, length)
		return err
	})
	if err != nil {
		return nil, a, err
	}
	return s.fetch(ctx, a, offset, length)
}

// attribution checks a raw range and says whose evidence the source is.
func (s *Store) attribution(ctx context.Context, sourceID string, generation, offset, length int64) (format.Attribution, error) {
	a := format.Attribution{SourceID: sourceID, Generation: generation}
	if offset < 0 || length <= 0 || length > MaxRaw {
		return a, fmt.Errorf("%w: need offset >= 0 and 0 < length <= %d", ErrBadRequest, MaxRaw)
	}
	// The session and repo come from the conversation the source feeds (a
	// companion's, through its parent).
	var tombstoned bool
	err := s.db().QueryRow(ctx, `SELECT s.tombstoned_at IS NOT NULL,s.agent,s.path,u.email,d.name,COALESCE(c.session_id,s.session_key,''),COALESCE(c.repo_root,c.cwd,'')
		FROM sources s JOIN devices d ON d.id=s.device_id JOIN users u ON u.id=d.user_id
		LEFT JOIN LATERAL (
			SELECT c.session_id,c.repo_root,c.cwd FROM conversations c
			WHERE c.source_id IN (s.id,s.parent_source_id)
			   OR c.id=(SELECT m.conversation_id FROM messages m WHERE m.source_id IN (s.id,s.parent_source_id) LIMIT 1)
			ORDER BY c.depth LIMIT 1) c ON true
		WHERE s.id=$1`, sourceID).Scan(&tombstoned, &a.Agent, &a.Path, &a.User, &a.Device, &a.SessionID, &a.Repo)
	if errors.Is(err, pgx.ErrNoRows) || tombstoned {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	// The evidence of a hidden conversation is hidden with it.
	hidden, err := s.sourceHidden(ctx, sourceID)
	if hidden {
		return a, ErrNotFound
	}
	return a, err
}

// sourceHidden reports whether a source's raw evidence is hidden: some
// conversation it (or its parent) feeds is hidden by an admin path rule.
func (s *Store) sourceHidden(ctx context.Context, sourceID string) (bool, error) {
	var hidden bool
	err := s.db().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversations c JOIN sources s ON s.id=$1 WHERE c.hidden_at IS NOT NULL
		AND (c.source_id IN (s.id,s.parent_source_id)
			OR EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id AND m.source_id IN (s.id,s.parent_source_id))))`, sourceID).Scan(&hidden)
	return hidden, err
}

// fetch reads the range of a's source generation from the archive, under
// ctx's deadline; a spent budget is a timed-out error, as in budgeted.
func (s *Store) fetch(ctx context.Context, a format.Attribution, offset, length int64) ([]byte, format.Attribution, error) {
	b := budget(ctx)
	g, err := ingest.LoadGeneration(ctx, s.Pool, a.SourceID, a.Generation)
	if errors.Is(err, ingest.ErrNoGeneration) {
		return nil, a, ErrNotFound
	}
	if err != nil {
		return nil, a, spent(ctx, b, err)
	}
	r := ingest.NewReader(ctx, s.Objects, g)
	if offset >= r.Size() {
		return nil, a, fmt.Errorf("%w: offset %d is past the generation's %d bytes", ErrBadRequest, offset, r.Size())
	}
	// Raw reads go through the server's redaction pass too, so bytes that
	// reached the archive unredacted are never served (notes/redaction.md).
	var kind string
	if err := s.db().QueryRow(ctx, `SELECT storage_kind FROM sources WHERE id=$1`, a.SourceID).Scan(&kind); err != nil {
		return nil, a, spent(ctx, b, err)
	}
	masks, err := ingest.LineMasks(ctx, s.db())
	if err != nil {
		return nil, a, spent(ctx, b, err)
	}
	rr := redact.NewReaderAt(r, redact.ModeFor(kind, a.Path))
	rr.SetLineMasks(masks)
	buf := make([]byte, min(length, r.Size()-offset))
	n, err := rr.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, a, spent(ctx, b, err)
	}
	return buf[:n], a, nil
}

func collect[T any](rows pgx.Rows, scan func(pgx.Row) (T, error)) ([]T, error) {
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

var termRe = regexp.MustCompile(`[\p{L}\p{N}_./:-]+`)

// queryTerms are the words of a websearch query, for snippets.
func queryTerms(q string) []string {
	var out []string
	for _, t := range termRe.FindAllString(q, -1) {
		if t != "or" && t != "OR" && len(t) > 1 {
			out = append(out, strings.ToLower(t))
		}
	}
	return out
}

// snippet is about 240 bytes of text around the first query term, and
// the line of the text it starts on.
func snippet(text string, terms []string) (string, int) {
	lower := strings.ToLower(text)
	at := -1
	for _, t := range terms {
		if i := strings.Index(lower, t); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	if at < 0 || at > len(text) {
		return clip(text, 240), 1
	}
	start := max(0, at-80)
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	s := clip(text[start:], 240)
	if start > 0 {
		s = "…" + s
	}
	return s, strings.Count(text[:start], "\n") + 1
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
