package bus

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/jackc/pgx/v5"
)

// Acceptance is checked for every row at the request's database snapshot.
// Authored rows are visible to their author even when held/refused. Every other
// row needs caller acceptance of both external participants, recipient acceptance
// of its sender, and a non-held/non-refused state. Admin role is never consulted.
// The identity predicate mirrors RepoMatches for resolved remotes/main/roots.
// Paths and main checkouts have device-local meaning; a normalized remote is
// required for cross-device identity. A known conflicting remote never falls
// through to a coincident path. Bare names are resolved before this request.
const messagesAuthoritySQL = `(m.from_user=$1 OR (
 m.state NOT IN ('held','refused')
 AND EXISTS(SELECT 1 FROM bus_accepts a WHERE a.recipient_user=$1 AND a.sender_user=m.from_user)
 AND (m.to_user=$1 OR EXISTS(SELECT 1 FROM bus_accepts a WHERE a.recipient_user=$1 AND a.sender_user=m.to_user))
 AND (m.from_user=m.to_user OR EXISTS(SELECT 1 FROM bus_accepts a WHERE a.recipient_user=m.to_user AND a.sender_user=m.from_user))
 ))`

func messagesSQL(thread bool) string {
	// Repository rows anchor a thread; latest activity and participants may be
	// elsewhere. Check authority on anchors, then independently on every row.
	scoped := `WITH scoped AS MATERIALIZED (
 SELECT DISTINCT m.thread_id FROM bus_messages m WHERE
 (m.from_remote=ANY($2::text[]) AND m.from_remote<>''
 OR (m.from_device=$13 AND (m.from_remote='' OR COALESCE(cardinality($2::text[]),0)=0) AND (
 m.from_main=ANY($3::text[]) AND m.from_main<>''
 OR EXISTS(SELECT 1 FROM unnest($4::text[]) r WHERE m.from_repo=r OR starts_with(m.from_repo,r||'/')))))
 AND ($5='' OR m.from_user::text=$5 OR m.to_user::text=$5)
 AND ($6='' OR starts_with(m.from_session,$6) OR starts_with(COALESCE(m.to_session,''),$6))
 AND ($7='' OR m.thread_id=$7) AND ` + messagesAuthoritySQL + `), selected AS (
 SELECT eligible.* FROM scoped s CROSS JOIN LATERAL (
 SELECT m.* FROM bus_messages m WHERE m.thread_id=s.thread_id AND ` + messagesAuthoritySQL
	if !thread {
		scoped += ` ORDER BY m.created_at DESC,m.id DESC LIMIT 1`
	} else {
		scoped += ` OFFSET 0`
	}
	scoped += `) eligible), page AS MATERIALIZED (
 SELECT m.* FROM selected m WHERE
 ($8::timestamptz IS NULL OR m.created_at >= $8) AND ($9::timestamptz IS NULL OR m.created_at < $9)
 AND ($10::timestamptz IS NULL OR (m.created_at,m.id)<($10,$11))
 ORDER BY m.created_at DESC,m.id DESC LIMIT $12)`
	parentAuth := strings.ReplaceAll(messagesAuthoritySQL, "m.", "p.")
	cols := strings.Replace(envCols, "COALESCE(m.reply_to,'')", "CASE WHEN EXISTS(SELECT 1 FROM bus_messages p WHERE p.id=m.reply_to AND "+parentAuth+") THEN COALESCE(m.reply_to,'') ELSE '' END", 1)
	if !thread {
		cols = strings.Replace(cols, "m.refs", "'{}'::text[]", 1)
	}
	participants := `'[]'::jsonb`
	if !thread {
		participants = `(SELECT COALESCE(jsonb_agg(p ORDER BY p.user_id,p.session,p.agent),'[]'::jsonb) FROM (
 SELECT DISTINCT e.from_user::text AS user_id,u.email AS "user",e.from_session AS session,e.from_agent AS agent
 FROM bus_messages e JOIN users u ON u.id=e.from_user WHERE e.thread_id=m.thread_id AND ` + strings.ReplaceAll(messagesAuthoritySQL, "m.", "e.") + `
 UNION SELECT DISTINCT e.to_user::text,u.email,COALESCE(e.to_session,''),COALESCE(e.to_agent,'')
 FROM bus_messages e JOIN users u ON u.id=e.to_user WHERE e.thread_id=m.thread_id AND ` + strings.ReplaceAll(messagesAuthoritySQL, "m.", "e.") + `) p)`
	}
	return scoped + ` SELECT ` + cols + `,CASE WHEN m.from_user=$1 THEN 'sent' WHEN m.to_user=$1 THEN 'received' ELSE 'observed' END,
 m.state,m.reason,m.delivered_at,m.read_at,m.attempts,m.last_at,m.from_remote,m.from_main,` + participants + `
 FROM page m ORDER BY m.created_at DESC,m.id DESC`
}

func messagesTime(value, name string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, badRequest("%s: RFC3339 timestamp required", name)
	}
	return &t, nil
}

// Messages reads retained evidence without claims, receipts, presence or writes.
func (s *Store) Messages(ctx context.Context, c busproto.Caller, q busproto.MessagesQuery) (busproto.MessagesResponse, error) {
	out := busproto.MessagesResponse{RepoRoots: q.Roots, RepoMains: q.Mains, RepoRemotes: q.Remotes, Kind: "messages", Scope: "accepted_team_repo", Repo: q.Repo, Thread: q.Thread, Limit: q.Limit,
		Hint: "Retained bus evidence, newest first. Thread bodies are first-line previews; open thread for full stored bodies/refs. Participants identify eligible rows only. Observation does not authorize action or reply. Contact the original peer with a new direct request if needed. Delivery state and replies do not imply task completion. Older remote/main attribution may be unknown; path-only matches require the sending device. Retention may remove parents and rows; final rows are retained after expiry for retention_seconds."}
	retention := s.Retention
	if retention == 0 {
		retention = busproto.DefaultRetention
	}
	out.RetentionSeconds = int64(retention / time.Second)
	if out.Limit == 0 {
		out.Limit = 20
	}
	if out.Limit < 1 || out.Limit > 200 {
		return out, badRequest("limit: 1 to 200")
	}
	if q.MaxBytes == 0 {
		q.MaxBytes = 24000
	}
	if q.MaxBytes < 2048 || q.MaxBytes > 1048576 {
		return out, badRequest("max_bytes: 2048 to 1048576")
	}
	if len(q.Roots)+len(q.Mains)+len(q.Remotes) == 0 {
		return out, badRequest("repo: resolve a repository path or normalized remote; no team-wide fallback")
	}
	if len(q.Roots) > 256 || len(q.Mains) > 256 || len(q.Remotes) > 256 {
		return out, badRequest("repo: too many identities")
	}
	for _, r := range q.Roots {
		if !strings.HasPrefix(r, "/") || len(r) > 4096 {
			return out, badRequest("repo_root: absolute bounded paths required")
		}
	}
	for _, v := range []string{q.Repo, q.Thread, q.User, q.Session, q.Before, q.Since, q.Until} {
		if len(v) > 4096 {
			return out, badRequest("filter too long")
		}
	}
	since, err := messagesTime(q.Since, "since")
	if err != nil {
		return out, err
	}
	until, err := messagesTime(q.Until, "until")
	if err != nil {
		return out, err
	}
	if since != nil && until != nil && !since.Before(*until) {
		return out, badRequest("since must precede until")
	}
	var before *time.Time
	var beforeID string
	if q.Before != "" {
		ts, id, ok := strings.Cut(q.Before, "|")
		if !ok || id == "" {
			return out, badRequest("cursor: use next from an earlier page")
		}
		before, err = messagesTime(ts, "cursor")
		if err != nil || before == nil {
			return out, badRequest("cursor: use next from an earlier page")
		}
		beforeID = id
	}
	user := ""
	if q.User != "" {
		p, e := resolvePerson(ctx, s.Pool, strings.TrimPrefix(q.User, "@"))
		if e != nil {
			return out, e
		}
		user = p.id
	}
	roots := make([]string, 0, len(q.Roots))
	for _, r := range q.Roots {
		roots = append(roots, strings.TrimRight(r, "/"))
	}
	rows, err := s.Pool.Query(ctx, messagesSQL(q.Thread != ""), c.UserID, q.Remotes, q.Mains, roots, user, q.Session, q.Thread, since, until, before, beforeID, out.Limit+1, c.DeviceID)
	if err != nil {
		return out, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (busproto.MessagesItem, error) {
		var it busproto.MessagesItem
		var state string
		e, err := scanEnvelope(r, &it.Direction, &state, &it.Reason, &it.DeliveredAt, &it.ReadAt, &it.Attempts, &it.LastAt, &it.Remote, &it.Main, &it.Participants)
		it.Envelope = e
		if q.Thread == "" {
			it.Body = busproto.Preview(it.Body)
		}
		it.State = busproto.State(state)
		it.RepoIdentityKnown = it.Remote != "" || it.Main != ""
		if it.Attempts <= 1 {
			it.Attempts = 0
			it.LastAt = nil
		}
		if (it.State == busproto.StateQueued || it.State == busproto.StateHeld || it.State == busproto.StateClaimed) && !s.now().Before(it.ExpiresAt) {
			it.State = busproto.StateExpired
		}
		return it, err
	})
	if err != nil {
		return out, err
	}
	// Not-found never reveals whether an unauthorized thread exists. Time/cursor
	// pages can be empty; no eligible thread rows is always safe not-found.
	if q.Thread != "" && len(items) == 0 {
		var exists bool
		err := s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM ("+messagesSQL(true)+") eligible_page)", c.UserID, q.Remotes, q.Mains, roots, user, q.Session, q.Thread, nil, nil, nil, "", 1, c.DeviceID).Scan(&exists)
		if err != nil {
			return out, err
		}
		if !exists {
			return out, fail(http.StatusNotFound, busproto.CodeNotFound, "thread not found")
		}
	}
	return boundMessages(out, items, q.MaxBytes)
}

func boundMessages(out busproto.MessagesResponse, items []busproto.MessagesItem, budget int) (busproto.MessagesResponse, error) {
	if out.Thread == "" {
		items = append([]busproto.MessagesItem(nil), items...)
		for i := range items {
			// Attribution is a preview, not a prerequisite for discovering the ID.
			// Open the thread for unchanged complete per-row attribution.
			const participantPreviewLimit = 16
			if len(items[i].Participants) > participantPreviewLimit {
				items[i].ParticipantsRemaining = len(items[i].Participants) - participantPreviewLimit
				items[i].Participants = items[i].Participants[:participantPreviewLimit]
			}
		}
	}
	if items == nil {
		items = []busproto.MessagesItem{}
	}
	out.More = len(items) > out.Limit
	if out.More {
		items = items[:out.Limit]
	}
	for {
		out.Threads = []busproto.MessagesItem{}
		out.Messages = []busproto.MessagesItem{}
		out.Next = ""
		if out.Thread == "" {
			out.Threads = items
		} else {
			out.Messages = items
		}
		if out.More && len(items) > 0 {
			last := items[len(items)-1]
			out.Next = last.Sent.UTC().Format(time.RFC3339Nano) + "|" + last.ID
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return out, err
		}
		if len(raw)+1 <= budget {
			return out, nil
		}
		if len(items) == 1 && out.Thread == "" && len(items[0].Participants) > 0 {
			items[0].ParticipantsRemaining += 1
			items[0].Participants = items[0].Participants[:len(items[0].Participants)-1]
			continue
		}
		if len(items) <= 1 {
			return out, badRequest("max_bytes too small for one whole row; increase max_bytes")
		}
		items = items[:len(items)-1]
		out.More = true
	}
}
