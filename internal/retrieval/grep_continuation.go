package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const grepContinuationTTL = 2 * time.Minute
const grepContinuationRows = 256
const grepContinuationBytes = 8 << 20
const grepContinuationBody = 2 << 20
const grepContinuationPages = 4
const grepContinuationAddresses = 32768

// GrepContinuations owns short-lived PostgreSQL snapshots. It admits no queue.
// Cached pages retain identities and fingerprints, never historical bodies.
// All retained searches, including completed replay caches, consume admission.
type GrepContinuations struct {
	mu       sync.Mutex
	pool     *pgxpool.Pool
	slots    int
	searches map[string]*grepWalk
	closed   bool
}
type grepWalk struct {
	mu                 sync.Mutex
	id, owner, binding string
	expires            time.Time
	timer              *time.Timer
	tx                 pgx.Tx
	re                 *regexp.Regexp
	query              format.GrepQuery
	filters            format.Filters
	pending            []uuid.UUID
	pages              []grepFrozenPage
	baseSeq            int
	seen               map[string]bool
	exhausted          bool
}
type grepFrozenHit struct {
	id, session string
	ordinal     int64
	hash        [32]byte
}
type grepFrozenPage struct {
	hits     []grepFrozenHit
	checked  int
	complete bool
}

func NewGrepContinuations(pool *pgxpool.Pool) *GrepContinuations {
	slots := min(4, int(pool.Config().MaxConns)/4)
	return &GrepContinuations{pool: pool, slots: slots, searches: make(map[string]*grepWalk)}
}
func (m *GrepContinuations) Close() {
	m.mu.Lock()
	m.closed = true
	walks := make([]*grepWalk, 0, len(m.searches))
	for _, w := range m.searches {
		walks = append(walks, w)
	}
	m.searches = make(map[string]*grepWalk)
	m.mu.Unlock()
	for _, w := range walks {
		w.mu.Lock()
		if w.timer != nil {
			w.timer.Stop()
		}
		w.release()
		w.mu.Unlock()
	}
}
func (w *grepWalk) release() {
	if w.tx != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = w.tx.Rollback(ctx)
		w.tx = nil
	}
}
func (m *GrepContinuations) expire(w *grepWalk) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.release()
	m.mu.Lock()
	if m.searches[w.id] == w {
		delete(m.searches, w.id)
	}
	m.mu.Unlock()
}

func grepScanPage(status string) *format.Page {
	return &format.Page{Hits: []format.Hit{}, Scan: &format.GrepScan{Status: status}, Exact: false}
}
func (s *Store) continueGrep(ctx context.Context, gq format.GrepQuery, f format.Filters) (*format.Page, error) {
	ctx, callCancel := context.WithTimeout(ctx, budget(ctx))
	defer callCancel()
	m := s.GrepContinuations
	if m == nil {
		return nil, fmt.Errorf("%w: resumable hosted grep is disabled", ErrBadRequest)
	}
	if f.Owner == "" || gq.Authority == "" {
		return nil, fmt.Errorf("%w: resumable grep needs authenticated authority", ErrBadRequest)
	}
	if gq.Pattern == "" {
		return nil, fmt.Errorf("%w: pattern is empty", ErrBadRequest)
	}
	sort, err := format.SortFor("grep", f.Sort)
	if err != nil {
		return nil, err
	}
	if gq.Offset != 0 || gq.MaxPerSession != 0 || (gq.Mode != "" && gq.Mode != format.ModeContent) || sort == format.SortRelevance {
		return nil, fmt.Errorf("%w: resumable grep supports content in newest or oldest order without offset or max_per_session", ErrBadRequest)
	}
	gq.Sort = sort
	gq.Limit = min(20, pageLimit(gq.Limit))
	bindingQuery := gq
	bindingQuery.Cursor = ""
	bindingQuery.Continue = false
	encoded, err := json.Marshal(struct {
		Query     format.GrepQuery
		Filters   format.Filters
		Authority string
	}{bindingQuery, f, gq.Authority})
	if err != nil {
		return nil, err
	}
	binding := string(encoded)
	var w *grepWalk
	seq := 0
	if gq.Cursor != "" {
		parts := strings.Split(gq.Cursor, ".")
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: invalid grep cursor", ErrBadRequest)
		}
		seq, err = strconv.Atoi(parts[1])
		if err != nil || seq < 0 {
			return nil, fmt.Errorf("%w: invalid grep cursor", ErrBadRequest)
		}
		m.mu.Lock()
		w = m.searches[parts[0]]
		m.mu.Unlock()
		if w == nil {
			return grepScanPage("restart_required"), nil
		}
		if w.owner != f.Owner || w.binding != binding {
			return nil, fmt.Errorf("%w: grep cursor does not match query or authority", ErrBadRequest)
		}
	} else {
		plan, err := grep.Compile(gq)
		if err != nil {
			return nil, err
		}
		if err = s.resolveFilterSession(ctx, &f); err != nil {
			return nil, err
		}
		if err = s.resolveFilterRepo(ctx, &f); err != nil {
			return nil, err
		}
		w = &grepWalk{id: uuid.NewString(), owner: f.Owner, binding: binding, expires: time.Now().Add(grepContinuationTTL), re: plan.Re, query: gq, filters: f, seen: make(map[string]bool)}
		// Reserve the slot before acquiring a connection, so no waiting search holds
		// a transaction outside admission. At most a quarter of the pool is pinned.
		m.mu.Lock()
		var previous *grepWalk
		for _, other := range m.searches {
			if other.owner == f.Owner {
				previous = other
				break
			}
		}
		if previous != nil && !previous.mu.TryLock() {
			m.mu.Unlock()
			return grepScanPage("overloaded"), nil
		}
		if m.closed || m.slots < 1 || (previous == nil && len(m.searches) >= m.slots) {
			if previous != nil {
				previous.mu.Unlock()
			}
			m.mu.Unlock()
			return grepScanPage("overloaded"), nil
		}
		if previous != nil {
			delete(m.searches, previous.id)
			previous.expires = time.Now()
		}

		w.mu.Lock()
		m.searches[w.id] = w
		m.mu.Unlock()
		if previous != nil {
			if previous.timer != nil {
				previous.timer.Stop()
			}
			previous.release()
			previous.mu.Unlock()
		}
		w.timer = time.AfterFunc(time.Until(w.expires), func() { m.expire(w) })
		err = s.startGrepWalk(ctx, w)
		if err != nil {
			w.release()
			w.mu.Unlock()
			m.expire(w)
			if timedOut(err) || ctx.Err() != nil {
				return grepScanPage("restart_required"), nil
			}
			return nil, err
		}
		w.mu.Unlock()
	}
	if !w.mu.TryLock() {
		return grepScanPage("overloaded"), nil
	}
	defer w.mu.Unlock()
	ctx, cancel := context.WithDeadline(ctx, w.expires)
	defer cancel()
	if time.Now().After(w.expires) {
		w.release()
		return grepScanPage("restart_required"), nil
	}
	if seq < w.baseSeq || seq > w.baseSeq+len(w.pages) || (seq == w.baseSeq+len(w.pages) && w.exhausted) {
		return grepScanPage("restart_required"), nil
	}
	if seq == w.baseSeq+len(w.pages) {
		frozen, err := s.advanceGrepWalk(ctx, w)
		if err != nil {
			w.release()
			w.exhausted = true
			p := grepScanPage("restart_required")
			p.Reason = "snapshot ended before the next page completed; restart the search"
			if strings.Contains(err.Error(), "body limit") {
				p.Reason = fmt.Sprintf("candidate exceeds the %d-byte body limit; narrow the search", grepContinuationBody)
			}
			if strings.Contains(err.Error(), "address limit") {
				p.Reason = "search reached its retained-address limit; narrow the search"
			}
			return p, nil
		}
		w.pages = append(w.pages, frozen)
		if len(w.pages) > grepContinuationPages {
			w.pages[0] = grepFrozenPage{}
			w.pages = w.pages[1:]
			w.baseSeq++
		}
		if frozen.complete {
			w.exhausted = true
			w.release()
		}
	}
	return s.authorizeGrepPage(ctx, w, seq)
}
func (s *Store) startGrepWalk(ctx context.Context, w *grepWalk) error {
	q := &query{}
	cond := ""
	if w.query.Fixed {
		if likeIndexable(w.query.Pattern) {
			cond = "m.text ILIKE " + q.arg("%"+likeEscape(w.query.Pattern)+"%")
		}
	} else {
		cond = literalRegexpCond(q, w.re)
		if cond == "" {
			cond = requiredRegexpCond(q, w.re)
		}
		if cond == "" {
			plan, err := grep.Compile(w.query)
			if err != nil {
				return err
			}
			cond = trigramCond(q, plan.Query)
		}
	}
	if cond == "" && w.filters.Session == "" {
		return fmt.Errorf("%w: unindexed resumable grep requires a session", ErrBadRequest)
	}
	if cond != "" {
		q.where(cond)
	}
	if err := hitFilters(q, w.filters); err != nil {
		return err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	w.tx = tx
	if err = setTimeout(ctx, tx, min(budget(ctx), time.Until(w.expires))); err != nil {
		return err
	}
	// As in ordinary grep, resolve a bounded scoped conversation set first.
	// Without this indexed entry point a broad text bitmap can read the whole
	// corpus before applying a session or repository filter.
	if grepNarrows(w.filters) {
		ids, complete, err := grepConversations(ctx, tx, w.filters)
		if err != nil {
			return err
		}
		if complete {
			q.where("m.conversation_id=ANY(" + q.arg(ids) + "::uuid[])")
		} else if cond == "" {
			return fmt.Errorf("%w: the selected session tree exceeds %d conversations; narrow the session or use an indexable pattern", ErrBadRequest, grepConvCap)
		}
	}
	if err = setTimeout(ctx, tx, min(budget(ctx), time.Until(w.expires))); err != nil {
		return err
	}
	// No SQL LIMIT: exhaustion means the frozen candidate set was traversed.
	order := " ORDER BY m.ts DESC NULLS LAST,m.id"
	if w.query.Sort == format.SortOldest {
		order = " ORDER BY m.ts NULLS LAST,m.id"
	}
	_, err = tx.Exec(ctx, `DECLARE resumable_grep NO SCROLL CURSOR FOR SELECT m.id FROM `+from+` WHERE `+q.sql()+order, q.args...)
	return err
}
func (s *Store) advanceGrepWalk(ctx context.Context, w *grepWalk) (grepFrozenPage, error) {
	p := grepFrozenPage{}
	if w.tx == nil {
		return p, fmt.Errorf("snapshot is unavailable")
	}
	deadline := time.Now().Add(min(budget(ctx), time.Until(w.expires)))
	var bytes int64
	for p.checked < grepContinuationRows && len(p.hits) < w.query.Limit {
		if time.Until(deadline) <= 0 {
			return p, context.DeadlineExceeded
		}
		if err := setTimeout(ctx, w.tx, time.Until(deadline)); err != nil {
			return p, err
		}
		if len(w.pending) == 0 {
			rows, err := w.tx.Query(ctx, `FETCH 32 FROM resumable_grep`)
			if err != nil {
				return p, err
			}
			w.pending, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return p, err
			}
			if len(w.pending) == 0 {
				p.complete = true
				break
			}
		}
		id := w.pending[0]
		var size int64
		if err := w.tx.QueryRow(ctx, `SELECT octet_length(text)::bigint FROM messages WHERE id=$1`, id).Scan(&size); err != nil {
			return p, err
		}
		if size > grepContinuationBody {
			return p, fmt.Errorf("candidate exceeds the %d-byte body limit; narrow the search", grepContinuationBody)
		}
		if bytes+size > grepContinuationBytes {
			break
		}
		var body, session string
		var ordinal int64
		if err := w.tx.QueryRow(ctx, `SELECT m.text,c.session_id,m.ordinal FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=$1`, id).Scan(&body, &session, &ordinal); err != nil {
			return p, err
		}
		w.pending = w.pending[1:]
		p.checked++
		bytes += size
		address := format.MessageAddress(session, ordinal)
		if !w.seen[address] && w.re.MatchString(body) {
			if len(w.seen) >= grepContinuationAddresses {
				return p, fmt.Errorf("retained address limit reached")
			}
			w.seen[address] = true
			p.hits = append(p.hits, grepFrozenHit{id: id.String(), session: session, ordinal: ordinal, hash: sha256.Sum256([]byte(body))})
		}
	}
	return p, nil
}
func (s *Store) authorizeGrepPage(ctx context.Context, w *grepWalk, seq int) (*format.Page, error) {
	frozen := w.pages[seq-w.baseSeq]
	p := grepScanPage("more")
	p.Scan.Checked = frozen.checked
	p.Scan.ExpiresAt = w.expires
	if frozen.complete {
		p.Scan.Status = "complete"
	} else {
		p.Scan.NextCursor = w.id + "." + strconv.Itoa(seq+1)
	}
	if len(frozen.hits) == 0 {
		return p, nil
	}
	ids := make([]string, 0, len(frozen.hits))
	hashes := make([][]byte, 0, len(frozen.hits))
	for _, h := range frozen.hits {
		ids = append(ids, h.id)
		hashes = append(hashes, h.hash[:])
	}
	q := &query{}
	q.where("m.id=ANY(" + q.arg(ids) + "::uuid[])")
	if err := hitFilters(q, w.filters); err != nil {
		return nil, err
	}
	// This transaction is new on every response and replay. It never shares the
	// retained snapshot and returns only bodies that still have the frozen hash.
	err := s.readTransaction(ctx, budget(ctx), pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		args := append(q.args, ids, hashes)
		idArg := "$" + strconv.Itoa(len(q.args)+1)
		hashArg := "$" + strconv.Itoa(len(q.args)+2)
		rows, err := tx.Query(ctx, `SELECT `+hitCols+`,m.text FROM unnest(`+idArg+`::uuid[],`+hashArg+`::bytea[]) WITH ORDINALITY page(id,hash,n) JOIN `+from+` ON m.id=page.id WHERE `+q.sql()+` AND octet_length(m.text)<=`+strconv.Itoa(grepContinuationBody)+` AND sha256(convert_to(m.text,'UTF8'))=page.hash ORDER BY page.n`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			h, err := scanHit(rows, &body)
			if err != nil {
				return err
			}
			// Ordinal/session can change independently of the body during reparsing.
			valid := false
			for _, old := range frozen.hits {
				if old.id == h.MessageID && old.session == h.SessionID && old.ordinal == h.Ordinal {
					valid = true
					break
				}
			}
			if !valid {
				continue
			}
			locs := w.re.FindAllStringIndex(body, 1000)
			if len(locs) == 0 {
				continue
			}
			h.Lines, h.MoreLines = grep.MatchLines(body, locs, grep.LineOpts{Before: w.query.Before, After: w.query.After, Only: w.query.OnlyMatching, Multi: w.query.Multiline})
			h.Address = format.MessageAddress(h.SessionID, h.Ordinal)
			p.Hits = append(p.Hits, h)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// Header metadata shares the response authority snapshot, so old titles or
		// digests cannot escape from the retained historical search transaction.
		copy := *s
		copy.tx = tx
		p.SessionInfo, err = copy.hitSessions(ctx, p.Hits)
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}
