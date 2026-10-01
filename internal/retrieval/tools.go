package retrieval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// maxCandidates bounds the rows one grep reads and verifies.
const maxCandidates = 200000

func pageLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	}
	return n
}

// Grep answers a grep query with the local index's semantics
// (internal/retrieval/grep): an RE2 pattern, multi-line, or a literal;
// messages newest first, every matching line with context, identical
// texts collapsed, -m and the offset applied, and totals for the footer.
// The planner's required trigrams select candidates through the pg_trgm
// index and Go's regexp verifies each one. A pattern with no trigram the
// index can use would scan every message; it is refused.
//
// The query budget (the context's deadline, else DefaultBudget) bounds the
// scan; when it runs out, the hits verified so far come back with a reason.
func (s *Store) Grep(ctx context.Context, gq format.GrepQuery, f format.Filters) (*format.Page, error) {
	if gq.Pattern == "" {
		return nil, fmt.Errorf("%w: pattern is empty", ErrBadRequest)
	}
	plan, err := grep.Compile(gq)
	if err != nil {
		return nil, err
	}
	if err := s.resolveFilterSession(ctx, &f); err != nil {
		return nil, err
	}
	if gq.Sort, err = format.SortFor("grep", f.Sort); err != nil {
		return nil, err
	}
	gq.Limit = pageLimit(gq.Limit)
	q := &query{}
	var cond string
	if !gq.Fixed {
		cond = trigramCond(q, plan.Query)
	} else if likeIndexable(gq.Pattern) {
		// pg_trgm narrows the whole substring itself, including short
		// words next to a space or punctuation ("go to"), which the
		// planner's 3-character runs leave out.
		cond = "m.text ILIKE " + q.arg("%"+likeEscape(gq.Pattern)+"%")
	}
	page := &format.Page{Hits: []format.Hit{}, Offset: gq.Offset, Exact: true}
	switch cond {
	case "false":
		return page, nil
	case "":
		return nil, fmt.Errorf("%w: the pattern has no run of 3 letters or digits that every match must contain, so the index cannot narrow it and it would scan every message; add a literal of at least 3 letters or digits", ErrBadRequest)
	}
	q.where(cond)
	if err := hitFilters(q, f); err != nil {
		return nil, err
	}
	col := grep.NewCollector(plan.Re, gq)
	checked := 0
	b := budget(ctx)
	deadline := time.Now().Add(b)
	// A cursor lets the scan stop without draining the rest of the
	// candidates; each fetch gets what is left of the budget.
	err = s.read(ctx, b, func(tx pgx.Tx) error {
		order := ` ORDER BY m.ts DESC NULLS LAST, m.id LIMIT `
		if gq.Sort == format.SortOldest {
			order = ` ORDER BY m.ts NULLS LAST, m.id LIMIT `
		}
		if _, err := tx.Exec(ctx, `DECLARE grep_candidates NO SCROLL CURSOR FOR SELECT `+hitCols+`,m.content_sha,m.text FROM `+from+` WHERE `+q.sql()+
			order+strconv.Itoa(maxCandidates), q.args...); err != nil {
			return err
		}
		for {
			left := time.Until(deadline)
			if left <= 0 {
				return context.DeadlineExceeded
			}
			if err := setTimeout(ctx, tx, left); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `FETCH 256 FROM grep_candidates`)
			if err != nil {
				return err
			}
			got, more := 0, true
			for rows.Next() {
				var sha []byte
				var body string
				h, err := scanHit(rows, &sha, &body)
				if err != nil {
					rows.Close()
					return err
				}
				got++
				checked++
				r := grep.Row{Session: h.SessionID, Text: body, Ref: h}
				copy(r.SHA[:], sha)
				if !col.Add(r) {
					more = false
					break
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if !more || got == 0 {
				break
			}
		}
		if checked >= maxCandidates {
			page.Truncated = true
			page.Reason = fmt.Sprintf("stopped after verifying %d candidates, newest first; narrow the pattern or the filters", checked)
		}
		return nil
	})
	if timedOut(err) {
		page.Truncated = true
		page.Reason = fmt.Sprintf("timed out after %s: checked %d candidates, newest first; narrow with --agent, --repo, --kind or --since, or a longer literal", b.Round(time.Second), checked)
	} else if err != nil {
		return nil, err
	}
	if gq.Sort == format.SortRelevance && page.Truncated {
		page.Notes = append(page.Notes, "ranked by matches per message among the messages checked")
	}
	ctx, done := followUp(ctx)
	defer done()
	res := col.Result()
	page.Total, page.TotalSessions, page.Next = res.Total, res.TotalSessions, res.Next
	page.Exact = !res.Capped && !page.Truncated
	var sessions []string
	for _, fh := range res.Hits {
		h := fh.Ref.(format.Hit)
		h.Lines, h.MoreLines, h.Copies = fh.Lines, fh.MoreLines, fh.Copies
		page.Hits = append(page.Hits, h)
		sessions = append(sessions, h.SessionID)
	}
	if page.SessionInfo, err = s.hitSessions(ctx, page.Hits); err != nil {
		return nil, err
	}
	var convIDs []string
	for _, sh := range res.Sessions {
		convIDs = append(convIDs, sh.Ref.(format.Hit).ConversationID)
	}
	infos, err := s.conversations(ctx, convIDs)
	if err != nil {
		return nil, err
	}
	for _, sh := range res.Sessions {
		info := infos[sh.Ref.(format.Hit).ConversationID]
		info.Hits = sh.Hits
		page.Sessions = append(page.Sessions, info)
	}
	short, err := s.addresses(ctx, sessions)
	if err != nil {
		return nil, err
	}
	for i := range page.Hits {
		page.Hits[i].Address = format.MessageAddress(short[page.Hits[i].SessionID], page.Hits[i].Ordinal)
	}
	return page, nil
}

// Search ranks messages matching the query (websearch syntax: words,
// "phrases", or, -not) by ts_rank_cd, newest first among equals, with
// identical texts collapsed into one hit. When no message holds every
// word, it ranks messages holding any of them, stopwords dropped, and says
// so. When the query budget runs out, the page is empty and says so.
func (s *Store) Search(ctx context.Context, sq format.SearchQuery, f format.Filters) (*format.Page, error) {
	if strings.TrimSpace(sq.Query) == "" {
		return nil, fmt.Errorf("%w: query is empty", ErrBadRequest)
	}
	if err := s.resolveFilterSession(ctx, &f); err != nil {
		return nil, err
	}
	n := pageLimit(sq.Limit)
	page := &format.Page{Hits: []format.Hit{}, Offset: sq.Offset}
	b := budget(ctx)
	sort, err := format.SortFor("search", f.Sort)
	if err != nil {
		return nil, err
	}
	f.Sort = sort
	hits, more, err := s.rank(ctx, b, sq.Query, sq.Offset, n, f)
	words, phrases := format.SplitQuery(sq.Query)
	if err == nil && len(hits) == 0 && len(words) > 1 && !strings.Contains(strings.ToLower(" "+sq.Query+" "), " or ") {
		none := sq.Offset == 0
		if !none {
			first, _, ferr := s.rank(ctx, b, sq.Query, 0, 1, f)
			none = ferr == nil && len(first) == 0
		}
		if none {
			kept := format.DropStopwords(words)
			var any []string
			for _, p := range phrases {
				any = append(any, `"`+p+`"`)
			}
			any = append(any, strings.Join(kept, " or "))
			if hits, more, err = s.rank(ctx, b, strings.Join(any, " "), sq.Offset, n, f); err == nil && len(hits) > 0 {
				page.Notes = append(page.Notes, "no message has every term; ranked messages with any of: "+strings.Join(kept, " ")+" (they may not answer the question)")
			}
		}
	}
	if timedOut(err) {
		page.Truncated = true
		page.Reason = fmt.Sprintf("timed out after %s before ranking finished; narrow with --agent, --repo, --kind or --since, or more specific terms", b.Round(time.Second))
		return page, nil
	}
	if err != nil {
		return nil, err
	}
	page.Hits = hits
	if more {
		page.Next = sq.Offset + len(hits)
	}
	ctx, done := followUp(ctx)
	defer done()
	var sessions []string
	for _, h := range hits {
		sessions = append(sessions, h.SessionID)
	}
	short, err := s.addresses(ctx, sessions)
	if err != nil {
		return nil, err
	}
	for i := range page.Hits {
		page.Hits[i].Address = format.MessageAddress(short[page.Hits[i].SessionID], page.Hits[i].Ordinal)
	}
	if page.SessionInfo, err = s.hitSessions(ctx, page.Hits); err != nil {
		return nil, err
	}
	return page, nil
}

// hitSessions describes the sessions of hits, in order of first hit.
func (s *Store) hitSessions(ctx context.Context, hits []format.Hit) ([]format.ConversationInfo, error) {
	var ids []string
	for _, h := range hits {
		if !slices.Contains(ids, h.ConversationID) {
			ids = append(ids, h.ConversationID)
		}
	}
	infos, err := s.conversations(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []format.ConversationInfo
	for _, id := range ids {
		out = append(out, infos[id])
	}
	return out, nil
}

// conversationBoost weighs the rank of prompts and replies (user,
// assistant, agent_message) against tool calls and their output, so a
// conversation turn outranks a log dump that matches as well.
const conversationBoost = 1.5

// rank returns hits offset..offset+n of a websearch query, identical texts
// collapsed (copies counted among the rows read), and whether more follow.
func (s *Store) rank(ctx context.Context, b time.Duration, text string, offset, n int, f format.Filters) ([]format.Hit, bool, error) {
	q := &query{}
	tsq := q.arg(text)
	q.where("m.tsv @@ websearch_to_tsquery('simple'," + tsq + ")")
	if err := hitFilters(q, f); err != nil {
		return nil, false, err
	}
	terms := queryTerms(text)
	// Read enough rows to fill the page after collapsing copies.
	window := min((offset+n)*3+16, 3000)
	var out []format.Hit
	more := false
	order := ` ORDER BY score DESC, m.ts DESC NULLS LAST LIMIT `
	switch f.Sort {
	case format.SortNewest:
		order = ` ORDER BY m.ts DESC NULLS LAST, m.id LIMIT `
	case format.SortOldest:
		order = ` ORDER BY m.ts NULLS LAST, m.id LIMIT `
	}
	err := s.read(ctx, b, func(tx pgx.Tx) error {
		// Prompts and replies outrank tool calls and output that match as
		// well (the local index weighs bm25 the same way).
		rows, err := tx.Query(ctx, `SELECT `+hitCols+`,ts_rank_cd(m.tsv,websearch_to_tsquery('simple',`+tsq+`))
			* CASE WHEN m.kind IN ('user','assistant','agent_message') THEN `+strconv.FormatFloat(conversationBoost, 'f', -1, 64)+` ELSE 1 END AS score,
			m.content_sha,left(m.text,65536)
			FROM `+from+` WHERE `+q.sql()+order+strconv.Itoa(window+1), q.args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		at := map[string]int{} // content hash -> index in out, or -1 before the page
		groups := 0
		read := 0
		for rows.Next() {
			read++
			var score float32
			var sha []byte
			var body string
			h, err := scanHit(rows, &score, &sha, &body)
			if err != nil {
				return err
			}
			if read > window {
				more = true
				break
			}
			if i, dup := at[string(sha)]; dup {
				if i >= 0 {
					out[i].Copies++
				}
				continue
			}
			groups++
			if groups <= offset {
				at[string(sha)] = -1
				continue
			}
			if len(out) == n {
				more = true
				at[string(sha)] = -1
				continue
			}
			h.Score = float64(score)
			h.Snippet, h.TextLine = snippet(body, terms)
			at[string(sha)] = len(out)
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, more, err
}

// Sessions lists conversations by last activity, newest first: those
// matching glob (session id, title, repo or cwd; see format.GlobLike) and
// the filters (agent, repo, device, user, subagents, the excluded session;
// since and until apply to the last activity).
func (s *Store) Sessions(ctx context.Context, glob string, offset int, f format.Filters) (out *format.Sessions, err error) {
	err = s.budgeted(ctx, func(s *Store) error {
		out, err = s.sessions(ctx, glob, offset, f)
		return err
	})
	return out, err
}

func (s *Store) sessions(ctx context.Context, glob string, offset int, f format.Filters) (*format.Sessions, error) {
	q := &query{}
	if f.Agent != "" {
		q.where("c.agent=ANY(" + q.arg(format.List(f.Agent)) + ")")
	}
	if prefix, like := format.RepoMatch(f.Repo); prefix != "" {
		a := q.arg(prefix)
		q.where(fmt.Sprintf("(c.repo_root=%s OR c.cwd=%s OR c.cwd LIKE %s OR c.repo_root LIKE %s)", a, a, q.arg(likeEscape(prefix)+"/%"), q.arg(likeEscape(prefix)+"/%")))
	} else if like != "" {
		q.where("COALESCE(c.repo_root,c.cwd) ILIKE " + q.arg(like))
	}
	if f.Device != "" {
		a := q.arg(f.Device)
		q.where(fmt.Sprintf("(d.id::text=%s OR d.name=%s)", a, a))
	}
	if f.User != "" {
		a := q.arg(f.User)
		q.where(fmt.Sprintf("(u.id::text=%s OR lower(u.email)=lower(%s))", a, a))
	}
	if f.ExcludeSubagents {
		q.where("c.depth=0 AND c.parent_native_session_id IS NULL")
	}
	if f.ExcludeSession != "" {
		q.where(excludeSessionTree(q, f.ExcludeSession))
	}
	if !f.Since.IsZero() {
		q.where("c.last_activity_at>=" + q.arg(f.Since))
	}
	if !f.Until.IsZero() {
		q.where("c.last_activity_at<" + q.arg(f.Until))
	}
	convFilters(q, f)
	if like := format.GlobLike(glob); like != "" {
		a := q.arg(like)
		q.where(fmt.Sprintf("(c.session_id ILIKE %[1]s OR COALESCE(c.title,'') ILIKE %[1]s OR COALESCE(c.repo_root,c.cwd,'') ILIKE %[1]s OR COALESCE(c.repo_root,c.cwd,'') ILIKE '%%/' || %[1]s)", a))
	}
	out := &format.Sessions{Sessions: []format.ConversationInfo{}, Offset: offset}
	if err := s.db().QueryRow(ctx, `SELECT count(*) FROM `+convFrom+` WHERE `+q.sql(), q.args...).Scan(&out.Total); err != nil {
		return nil, err
	}
	n := pageLimit(f.Limit)
	sort, err := format.SortFor("sessions", f.Sort)
	if err != nil {
		return nil, err
	}
	order := ` ORDER BY c.last_activity_at DESC NULLS LAST, c.id LIMIT `
	if sort == format.SortOldest {
		order = ` ORDER BY c.last_activity_at NULLS LAST, c.id LIMIT `
	}
	rows, err := s.db().Query(ctx, `SELECT `+convCols+` FROM `+convFrom+` WHERE `+q.sql()+
		order+strconv.Itoa(n)+` OFFSET `+strconv.Itoa(offset), q.args...)
	if err != nil {
		return nil, err
	}
	if out.Sessions, err = collect(rows, scanConv); err != nil {
		return nil, err
	}
	if err := s.shortenConvs(ctx, out.Sessions); err != nil {
		return nil, err
	}
	if offset+len(out.Sessions) < out.Total {
		out.Next = offset + len(out.Sessions)
	}
	return out, nil
}

// conversations loads conversation infos by id, addresses shortened.
func (s *Store) conversations(ctx context.Context, ids []string) (map[string]format.ConversationInfo, error) {
	out := map[string]format.ConversationInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db().Query(ctx, `SELECT `+convCols+` FROM `+convFrom+` WHERE c.id::text=ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	list, err := collect(rows, scanConv)
	if err != nil {
		return nil, err
	}
	if err := s.shortenConvs(ctx, list); err != nil {
		return nil, err
	}
	for _, c := range list {
		out[c.ID] = c
	}
	return out, nil
}

// shortenConvs sets each conversation's address and its parent's.
func (s *Store) shortenConvs(ctx context.Context, cs []format.ConversationInfo) error {
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.SessionID, c.ParentSession)
	}
	short, err := s.addresses(ctx, ids)
	if err != nil {
		return err
	}
	for i := range cs {
		cs[i].Address, cs[i].ParentSession = short[cs[i].SessionID], short[cs[i].ParentSession]
	}
	return nil
}

// addressConversations includes hidden sessions only for their owner or an
// administrator performing a mutation. An empty owner and false admin retain
// the ordinary visible-only read scope.
const addressConversations = `(SELECT * FROM conversations WHERE hidden_at IS NULL OR user_id=NULLIF($1,'')::uuid OR $2)`

// sessionsWithPrefix returns up to limit distinct visible session ids starting
// with prefix.
func (s *Store) sessionsWithPrefix(ctx context.Context, prefix string, limit int) ([]string, error) {
	return s.addressSessionsWithPrefix(ctx, prefix, limit, "", false)
}

func (s *Store) addressSessionsWithPrefix(ctx context.Context, prefix string, limit int, owner string, admin bool) ([]string, error) {
	rows, err := s.db().Query(ctx, `SELECT DISTINCT session_id COLLATE "C" FROM `+addressConversations+` c WHERE session_id COLLATE "C" LIKE $3 ORDER BY 1 LIMIT $4`, owner, admin, likeEscape(prefix)+"%", limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r pgx.Row) (string, error) {
		var id string
		return id, r.Scan(&id)
	})
}

// addresses maps session ids to their shortest unique prefixes: each id
// against the ids just before and after it in byte order.
func (s *Store) addresses(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if _, done := out[id]; done || id == "" {
			continue
		}
		var nb []string
		for _, q := range []string{
			`SELECT session_id FROM conversations WHERE hidden_at IS NULL AND session_id COLLATE "C" < $1 ORDER BY session_id COLLATE "C" DESC LIMIT 1`,
			`SELECT session_id FROM conversations WHERE hidden_at IS NULL AND session_id COLLATE "C" > $1 ORDER BY session_id COLLATE "C" LIMIT 1`,
		} {
			var x string
			err := s.db().QueryRow(ctx, q, id).Scan(&x)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			nb = append(nb, x)
		}
		out[id] = format.ShortPrefix(id, nb)
	}
	return out, nil
}

// resolveFilterSession turns f.Session, a session id prefix, into the
// one session id it names.
func (s *Store) resolveFilterSession(ctx context.Context, f *format.Filters) error {
	if f.Session == "" {
		return nil
	}
	if f.Self { // the caller's own session: its exact id, never a prefix
		if f.Owner == "" {
			return fmt.Errorf("%w: session self needs the calling user", ErrBadRequest)
		}
		return nil
	}
	sid, err := s.resolveSession(ctx, f.Session)
	f.Session = sid
	return err
}

// resolveSession returns the one session id starting with prefix (an
// exact id wins over longer ones).
func (s *Store) resolveSession(ctx context.Context, prefix string) (string, error) {
	return s.resolveAddressSession(ctx, prefix, "", false)
}

func (s *Store) resolveAddressSession(ctx context.Context, prefix, owner string, admin bool) (string, error) {
	ids, err := s.addressSessionsWithPrefix(ctx, prefix, 6, owner, admin)
	if err != nil {
		return "", err
	}
	switch {
	case len(ids) == 0:
		return "", fmt.Errorf("%w: no session starts with %q", ErrNotFound, prefix)
	case len(ids) == 1 || ids[0] == prefix:
		return ids[0], nil
	}
	return "", format.AmbiguousError(prefix, ids)
}

// sessionAfter is how many messages read shows after the first when the
// address names a whole session.
const sessionAfter = 19

// locate finds the focus message of an address, the addressed line, and
// whether the address names a whole session. A path address prefers the
// caller's own device.
func (s *Store) locate(ctx context.Context, deviceID, address string) (string, int, bool, error) {
	return s.locateAddress(ctx, deviceID, address, "", false)
}

// locateAddress expands visibility only for explicitly authorized mutations.
func (s *Store) locateAddress(ctx context.Context, deviceID, address, owner string, admin bool) (string, int, bool, error) {
	a, err := format.ParseAddress(address)
	if err != nil {
		return "", 0, false, err
	}
	var id string
	one := func(sql string, args ...any) error {
		err := s.db().QueryRow(ctx, sql, append([]any{owner, admin}, args...)...).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	const best = ` ORDER BY m.superseded, m.on_active_path IS FALSE, m.version DESC, m.id LIMIT 1`
	switch a.Kind {
	case format.AddrMessage:
		sid, err := s.resolveAddressSession(ctx, a.Session, owner, admin)
		if err != nil {
			return "", 0, false, err
		}
		err = one(`SELECT m.id::text FROM messages m JOIN `+addressConversations+` c ON c.id=m.conversation_id WHERE c.session_id=$3 AND m.ordinal=$4`+best, sid, a.Ordinal)
		if errors.Is(err, ErrNotFound) {
			err = fmt.Errorf("%w: session %s has no message at ordinal %d", ErrNotFound, sid, a.Ordinal)
		}
		return id, a.Line, false, err
	case format.AddrPath:
		dev := deviceID
		if _, err := uuid.Parse(dev); err != nil {
			dev = uuid.Nil.String()
		}
		err = one(`SELECT m.id::text FROM messages m JOIN sources s ON s.id=m.source_id JOIN `+addressConversations+` c ON c.id=m.conversation_id WHERE s.path=$3 AND m.line_no=$4
			ORDER BY s.device_id=$5::uuid DESC, m.superseded, m.ordinal, m.id LIMIT 1`, a.Path, a.PathNo, dev)
		if errors.Is(err, ErrNotFound) {
			err = fmt.Errorf("%w: no indexed message at %s:%d", ErrNotFound, a.Path, a.PathNo)
		}
		return id, 0, false, err
	}
	if _, perr := uuid.Parse(a.Token); perr == nil {
		if err := one(`SELECT m.id::text FROM messages m JOIN `+addressConversations+` c ON c.id=m.conversation_id WHERE m.id=$3`, a.Token); err == nil {
			return id, 0, false, nil
		} else if !errors.Is(err, ErrNotFound) {
			return "", 0, false, err
		}
	}
	sid, err := s.resolveAddressSession(ctx, a.Token, owner, admin)
	if err != nil {
		return "", 0, false, err
	}
	err = one(`SELECT m.id::text FROM messages m JOIN `+addressConversations+` c ON c.id=m.conversation_id WHERE c.session_id=$3
		ORDER BY m.superseded, m.on_active_path IS FALSE, c.depth, m.ordinal, m.id LIMIT 1`, sid)
	if errors.Is(err, ErrNotFound) {
		err = fmt.Errorf("%w: session %s has no messages", ErrNotFound, sid)
	}
	return id, 0, true, err
}

// locateSelf finds the first message of the caller's own session: the
// exact session id, among owner's conversations only.
func (s *Store) locateSelf(ctx context.Context, owner, session string) (string, error) {
	if owner == "" {
		return "", fmt.Errorf("%w: read self needs the calling user", ErrBadRequest)
	}
	var id string
	err := s.db().QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN `+visible+` c ON c.id=m.conversation_id WHERE c.session_id=$1 AND c.user_id=$2
		ORDER BY m.superseded, m.on_active_path IS FALSE, c.depth, m.ordinal, m.id LIMIT 1`, session, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: your session %s is not indexed yet", ErrNotFound, session)
	}
	return id, err
}

// Read returns the message an address names with its neighbours in
// conversation order (for a session address: its first messages), the
// focus text cut to the line offset and character budget, neighbours to a
// quarter of it. The filters' row view (superseded, branches) applies to
// the neighbours; the focus is always included.
func (s *Store) Read(ctx context.Context, deviceID string, rq format.ReadQuery, f format.Filters) (out *format.Context, err error) {
	err = s.budgeted(ctx, func(s *Store) error {
		out, err = s.readAt(ctx, deviceID, rq, f)
		return err
	})
	if err == nil && out != nil && s.RefreshSession != nil {
		s.RefreshSession(ctx, out.Conversation.ID)
	}
	return out, err
}

func (s *Store) readAt(ctx context.Context, deviceID string, rq format.ReadQuery, f format.Filters) (*format.Context, error) {
	var focus string
	var line int
	var session bool
	var err error
	if rq.Self {
		focus, err = s.locateSelf(ctx, f.Owner, rq.Address)
		session = true
	} else {
		focus, line, session, err = s.locate(ctx, deviceID, rq.Address)
	}
	if err != nil {
		return nil, err
	}
	before, after := rq.Before, rq.After
	if session && before == 0 && after == 0 {
		after = sessionAfter
	}
	if before < 0 || after < 0 || before > maxContext || after > maxContext {
		return nil, fmt.Errorf("%w: before and after must be between 0 and %d", ErrBadRequest, maxContext)
	}
	var conv string
	var ord int64
	if err := s.db().QueryRow(ctx, `SELECT conversation_id::text,ordinal FROM messages WHERE id=$1`, focus).Scan(&conv, &ord); err != nil {
		return nil, err
	}
	if rq.Outline {
		return s.outline(ctx, conv, rq)
	}
	out := &format.Context{Focus: focus, Line: line}
	infos, err := s.conversations(ctx, []string{conv})
	if err != nil {
		return nil, err
	}
	out.Conversation = infos[conv]
	q := &query{}
	cid, o, id, fid := q.arg(conv), q.arg(ord), q.arg(focus), q.arg(focus)
	f.ExcludeConversation, f.ExcludeSession = "", ""
	f.Kinds, f.ExcludeKinds, f.Tools = nil, nil, nil
	if err := filters(q, f); err != nil {
		return nil, err
	}
	w := q.sql()
	sql := `SELECT * FROM (
		(SELECT ` + msgCols + ` FROM ` + from + ` WHERE m.conversation_id=` + cid + ` AND (m.ordinal,m.id::text)<(` + o + `,` + id + `::text) AND ` + w + ` ORDER BY m.ordinal DESC,m.id DESC LIMIT ` + strconv.Itoa(before+1) + `)
		UNION ALL (SELECT ` + msgCols + ` FROM ` + from + ` WHERE m.id=` + fid + `)
		UNION ALL (SELECT ` + msgCols + ` FROM ` + from + ` WHERE m.conversation_id=` + cid + ` AND (m.ordinal,m.id::text)>(` + o + `,` + id + `) AND ` + w + ` ORDER BY m.ordinal,m.id LIMIT ` + strconv.Itoa(after+1) + `)
	) x ORDER BY 3, 1`
	rows, err := s.db().Query(ctx, sql, q.args...)
	if err != nil {
		return nil, err
	}
	msgs, err := collect(rows, scanMessage)
	if err != nil {
		return nil, err
	}
	at := 0
	for i, m := range msgs {
		if m.ID == focus {
			at = i
		}
	}
	if at > before {
		msgs, at, out.MoreBefore = msgs[at-before:], before, true
	}
	if len(msgs)-at-1 > after {
		msgs, out.MoreAfter = msgs[:at+after+1], true
	}
	budget := rq.MaxChars
	if budget <= 0 {
		budget = format.DefaultMaxChars
	}
	short, err := s.addresses(ctx, []string{out.Conversation.SessionID})
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		m := &msgs[i]
		m.Address = format.MessageAddress(short[m.Address], m.Ordinal)
		from, chars := 1, max(budget/4, 200)
		if m.ID == focus {
			from, chars = rq.LineOffset, budget
			if from == 0 && line > 0 {
				from = max(1, line-5)
			}
		}
		e := format.Cut(m.Text, from, chars)
		m.Text, m.Lines, m.LineFrom, m.LineTo, m.Clipped = e.Text, e.Lines, e.From, e.To, e.Clipped
	}
	out.Messages = msgs
	return out, nil
}

// outline answers read --outline for a conversation: its user prompts and
// tool calls in order, failed calls flagged, spawned subagents named.
func (s *Store) outline(ctx context.Context, conv string, rq format.ReadQuery) (*format.Context, error) {
	n, err := format.OutlinePage(rq)
	if err != nil {
		return nil, err
	}
	infos, err := s.conversations(ctx, []string{conv})
	if err != nil {
		return nil, err
	}
	out := &format.Context{Conversation: infos[conv], Messages: []format.Message{}, OutlineOffset: rq.Offset}
	const where = ` WHERE m.conversation_id=$1 AND NOT m.superseded AND m.on_active_path IS NOT FALSE AND m.kind IN ('user','tool_call')`
	if err := s.db().QueryRow(ctx, `SELECT count(*) FROM messages m`+where, conv).Scan(&out.OutlineTotal); err != nil {
		return nil, err
	}
	type row struct {
		id, kind, tool, callID, text, sid string
		ord                               int64
		ts                                *time.Time
		isErr                             bool
	}
	rows, err := s.db().Query(ctx, `SELECT m.id::text,m.kind,COALESCE(m.tool_name,''),COALESCE(m.tool_call_id,''),left(m.text,4000),c.session_id,m.ordinal,m.ts,COALESCE(m.is_error,false)
		FROM messages m JOIN conversations c ON c.id=m.conversation_id`+where+` ORDER BY m.ordinal,m.id LIMIT $2 OFFSET $3`, conv, n, rq.Offset)
	if err != nil {
		return nil, err
	}
	list, err := collect(rows, func(r pgx.Row) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.kind, &x.tool, &x.callID, &x.text, &x.sid, &x.ord, &x.ts, &x.isErr)
	})
	if err != nil {
		return nil, err
	}
	rows, err = s.db().Query(ctx, `SELECT DISTINCT tool_call_id FROM messages WHERE conversation_id=$1 AND NOT superseded AND is_error AND tool_call_id IS NOT NULL`, conv)
	if err != nil {
		return nil, err
	}
	failedList, err := collect(rows, func(r pgx.Row) (string, error) {
		var id string
		return id, r.Scan(&id)
	})
	if err != nil {
		return nil, err
	}
	rows, err = s.db().Query(ctx, `SELECT spawned_by_message_id::text,session_id FROM conversations
		WHERE parent_conversation_id=$1 AND spawned_by_message_id IS NOT NULL AND hidden_at IS NULL ORDER BY started_at,id`, conv)
	if err != nil {
		return nil, err
	}
	type kid struct{ msg, sid string }
	kids, err := collect(rows, func(r pgx.Row) (kid, error) {
		var k kid
		return k, r.Scan(&k.msg, &k.sid)
	})
	if err != nil {
		return nil, err
	}
	var sids []string
	for _, x := range list {
		sids = append(sids, x.sid)
	}
	for _, k := range kids {
		sids = append(sids, k.sid)
	}
	short, err := s.addresses(ctx, sids)
	if err != nil {
		return nil, err
	}
	root := out.Conversation.Repo
	for _, x := range list {
		e := format.NewOutlineEntry(format.MessageAddress(short[x.sid], x.ord), x.ts, x.kind, x.tool, x.text, root)
		e.Error = x.isErr || x.callID != "" && slices.Contains(failedList, x.callID)
		for _, k := range kids {
			if k.msg == x.id {
				e.Subagents = append(e.Subagents, short[k.sid])
			}
		}
		out.Outline = append(out.Outline, e)
	}
	if end := rq.Offset + len(list); end < out.OutlineTotal {
		out.OutlineNext = end
	}
	return out, nil
}

// RawAt returns the transcript bytes of the message an address names, and
// whose evidence they are.
func (s *Store) RawAt(ctx context.Context, deviceID, address string) ([]byte, format.Attribution, error) {
	var a format.Attribution
	var offset, length int64
	err := s.budgeted(ctx, func(s *Store) error {
		focus, _, _, err := s.locate(ctx, deviceID, address)
		if err != nil {
			return err
		}
		var src string
		var gen int64
		var off, n *int64
		if err := s.db().QueryRow(ctx, `SELECT COALESCE(source_id::text,''),source_generation,byte_offset,byte_len FROM messages WHERE id=$1`, focus).Scan(&src, &gen, &off, &n); err != nil {
			return err
		}
		if src == "" || off == nil || n == nil || *n <= 0 {
			return fmt.Errorf("%w: message %s has no byte range; read shows its text", ErrBadRequest, focus)
		}
		offset, length = *off, *n
		a, err = s.attribution(ctx, src, gen, offset, length)
		return err
	})
	if err != nil {
		return nil, a, err
	}
	return s.fetch(ctx, a, offset, length)
}
