package retrieval

// Message redaction after the fact (notes/redaction.md): an owner masks
// one of their messages, or an admin any message, with or without its
// identical copies. Rows, archived chunks and provisional tails are
// rewritten; the old chunks go to a deletion job that purges them; later
// uploads are redirected (identical chunks) or masked by the server's
// redaction pass (the same line under new chunk boundaries).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// beforeRedactTx, when set (tests), runs after a redaction picks its
// targets and before its transaction.
var beforeRedactTx func()

type redactTarget struct {
	id, text, sourceID, deviceID string
	conv                         string
	gen, off, n                  *int64
	persisted, native            string
	enrichment                   map[string]any
}

// RedactMessage masks the message an address names (lines from..to of
// its text, or all of it when from is 0) everywhere Flopwire keeps it.
//
// The caller acts as the owner (their own messages only) unless admin is
// set (any user's).
func (s *Store) RedactMessage(ctx context.Context, userID, deviceID string, admin bool, req format.RedactRequest) (format.RedactResult, error) {
	for attempt := 1; ; attempt++ {
		res, err := s.redactOnce(ctx, userID, deviceID, admin, req)
		if !errors.Is(err, errTargetsMoved) || attempt == redactAttempts {
			return res, err
		}
	}
}

// errTargetsMoved is a redaction whose targets changed between reading
// them and its transaction (a parse committed a copy): it starts again.
var errTargetsMoved = errors.New("retrieval: redaction targets changed; retry")

// redactAttempts bounds the restarts of a redaction racing parses.
const redactAttempts = 5

func (s *Store) redactOnce(ctx context.Context, userID, deviceID string, admin bool, req format.RedactRequest) (format.RedactResult, error) {
	who := struct {
		UserID, DeviceID string
		Admin            bool
	}{userID, deviceID, admin}
	var res format.RedactResult
	address, from, to, err := format.SplitLineRange(req.Address)
	if err != nil {
		return res, err
	}
	focus, _, _, err := s.locateAddress(ctx, who.DeviceID, address, who.UserID, who.Admin)
	if err != nil {
		return res, err
	}
	var owner, conv, text string
	var native *string
	var sha []byte
	if err := s.Pool.QueryRow(ctx, `SELECT c.user_id::text,m.conversation_id::text,m.native_id,m.content_sha,m.text
		FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=$1`, focus).Scan(&owner, &conv, &native, &sha, &text); err != nil {
		return res, err
	}
	if !who.Admin && owner != who.UserID {
		return res, format.ErrForbidden
	}
	if from > 0 && from > strings.Count(text, "\n")+1 {
		return res, fmt.Errorf("%w: the message has %d lines", ErrBadRequest, strings.Count(text, "\n")+1)
	}
	_, needles := redact.MaskText(text, from, to)
	if from > 0 && len(needles) == 0 {
		return res, fmt.Errorf("%w: lines %d-%d are empty", ErrBadRequest, from, to)
	}

	// Targets: the row, its other versions, and with all_copies every row
	// with the same text (the +N copies group), within the caller's reach.
	targets, err := redactTargets(ctx, s.Pool, focus, conv, native, req.AllCopies, sha, who.Admin, who.UserID)
	if err != nil {
		return res, err
	}

	priorMasks, err := ingest.LineMasks(ctx, s.Pool)
	if err != nil {
		return res, err
	}
	// New row texts, and the raw spans to mask per (source, generation).
	type srcGen struct {
		source string
		gen    int64
	}
	spans := map[srcGen][]redact.Span{}
	type lineFix struct {
		raw   []byte
		spans []redact.Span
	}
	lines := map[[32]byte]lineFix{}
	newText := map[string]string{}
	for _, t := range targets {
		masked := t.text
		if from == 0 {
			masked, _ = redact.MaskText(t.text, 0, 0)
		} else {
			for _, n := range needles {
				masked = strings.ReplaceAll(masked, n, maskString(n))
			}
		}
		if masked == t.text {
			continue // another version without the hidden lines
		}
		newText[t.id] = masked
		if t.sourceID == "" {
			continue
		}
		k := srcGen{t.sourceID, *t.gen}
		add := func(off int64, rec []byte, sp []redact.Span) {
			if priorMasks != nil {
				sp = append(sp, priorMasks.MatchBytes(rec)...)
			}
			sum := redact.LineSum(rec)
			line := lines[sum]
			if line.raw == nil {
				line.raw = bytes.Clone(rec)
			}
			line.spans = append(line.spans, sp...)
			lines[sum] = line
			for _, x := range sp {
				spans[k] = append(spans[k], redact.Span{Start: x.Start + int(off), End: x.End + int(off)})
			}
		}
		if t.off == nil || t.n == nil || *t.n <= 0 {
			// No byte range (a Devin row, parsed from a rebuilt store): find
			// the record among the generation's lines, by the hidden lines or,
			// for a whole message, by its native id.
			if err := s.eachLine(ctx, t.sourceID, *t.gen, func(off int64, line []byte) {
				if from > 0 {
					if sp, _ := redact.MaskRecord(line, needles, false); len(sp) > 0 {
						add(off, line, sp)
					}
				} else if q := jsonQuoted(t.native); t.native != "" &&
					(bytes.Contains(line, []byte(`"`+q+`"`)) || bytes.Contains(line, []byte(`\"`+jsonQuoted(q)+`\"`))) {
					sp, _ := redact.MaskRecord(line, nil, true)
					add(off, line, sp)
				}
			}); err != nil {
				return res, err
			}
			continue
		}
		rec, err := s.archived(ctx, t.sourceID, *t.gen, *t.off, *t.n)
		if err != nil {
			return res, err
		}
		sp, found := redact.MaskRecord(rec, needles, from == 0)
		if !found || len(sp) == 0 {
			// The text was assembled by the parser, or is not in the record
			// as written: hide the whole record's text instead.
			sp, _ = redact.MaskRecord(rec, nil, true)
			res.Fallbacks++
		}
		add(*t.off, rec, sp)
		if t.persisted != "" {
			if err := s.persistedSpans(ctx, t, needles, from == 0, func(src string, gen int64, sp []redact.Span) {
				spans[srcGen{src, gen}] = append(spans[srcGen{src, gen}], sp...)
			}); err != nil {
				return res, err
			}
		}
	}
	if len(newText) == 0 {
		return res, fmt.Errorf("%w: nothing to redact", ErrBadRequest)
	}

	var masks []ingest.ArchiveMask
	for k, sp := range spans {
		g, err := ingest.LoadGeneration(ctx, s.Pool, k.source, k.gen)
		if err != nil {
			return res, err
		}
		masks = append(masks, ingest.ArchiveMask{Generation: g, Spans: sp})
	}
	plan, err := ingest.PrepareArchiveRewrite(ctx, s.Pool, s.Objects, masks)
	if err != nil {
		return res, err
	}
	defer plan.Close()
	chunks, tails := plan.Counts()

	if beforeRedactTx != nil {
		beforeRedactTx()
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	err = plan.WithTx(ctx, func(tx pgx.Tx) error {
		// Parse writes share this lock, so none commits while the redaction
		// runs. A copy one committed after the targets were read is found
		// here; a parse that loaded the catalog before this commits sees the
		// revision move when it next writes.
		if err := ingest.LockRedactedLines(ctx, tx); err != nil {
			return err
		}
		current, err := redactTargets(ctx, tx, focus, conv, native, req.AllCopies, sha, who.Admin, who.UserID)
		if err != nil {
			return err
		}
		if !slices.EqualFunc(current, targets, func(a, b redactTarget) bool { return a.id == b.id && a.text == b.text }) {
			return errTargetsMoved
		}
		lr := ""
		if from > 0 {
			lr = fmt.Sprintf("%d-%d", from, to)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO message_redactions(id,requested_by,device_id,message_id,lines,all_copies,by_admin,messages,chunks,tails,created_at)
			VALUES($1,$2,NULLIF($3,'')::uuid,$4,$5,$6,$7,$8,$9,$10,$11)`,
			id, who.UserID, who.DeviceID, focus, lr, req.AllCopies, who.Admin, len(newText), chunks, tails, now); err != nil {
			return err
		}
		applied, err := plan.Apply(ctx, tx, id, who.UserID)
		if err != nil {
			return err
		}
		res.JobID = applied.JobID
		for _, t := range targets {
			txt, ok := newText[t.id]
			if !ok {
				continue
			}
			sum := sha256.Sum256([]byte(txt))
			enr := maskEnrichment(t.enrichment, needles, from == 0)
			enr["redacted"] = id
			if _, err := tx.Exec(ctx, `UPDATE messages SET text=$2,content_sha=$3,enrichment=$4 WHERE id=$1`, t.id, txt, sum[:], enr); err != nil {
				return err
			}
			// The conversation's title is the first line of its first
			// prompt: it holds the redacted text too.
			var title string
			var dg []byte
			if err := tx.QueryRow(ctx, `SELECT COALESCE(title,''),digest FROM conversations WHERE id=$1 FOR UPDATE`, t.conv).Scan(&title, &dg); err != nil {
				return err
			}
			if nt := maskTitle(title, t.text, txt, needles); nt != title {
				if _, err := tx.Exec(ctx, `UPDATE conversations SET title=$2 WHERE id=$1`, t.conv, nt); err != nil {
					return err
				}
			}
			// The digest keeps the intent and last reply (and the paths
			// and ids) drawn from the text.
			hidden := needles
			if from == 0 {
				hidden = strings.Split(t.text, "\n")
			}
			if nd := digest.Mask(dg, hidden, maskString); !bytes.Equal(nd, dg) {
				if _, err := tx.Exec(ctx, `UPDATE conversations SET digest=$2 WHERE id=$1`, t.conv, nd); err != nil {
					return err
				}
			}
		}
		// A missing row cannot be locked FOR UPDATE. Serialize first insertions
		// too, in stable hash order, before merging evidence and its proof.
		keys := make([]string, 0, len(lines))
		byKey := make(map[string][32]byte, len(lines))
		for sum := range lines {
			key := fmt.Sprintf("redacted-line:%x", sum)
			keys = append(keys, key)
			byKey[key] = sum
		}
		slices.Sort(keys)
		for _, key := range keys {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
				return err
			}
		}
		for _, key := range keys {
			sum := byKey[key]
			line := lines[sum]
			var previous []redact.Span
			err := tx.QueryRow(ctx, `SELECT spans FROM redacted_lines WHERE line_sha=$1 FOR UPDATE`, sum[:]).Scan(&previous)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			combined := append(previous, line.spans...)
			raw, _ := json.Marshal(combined)
			proof, _ := json.Marshal(redact.NewLineProof(line.raw, combined))
			if _, err := tx.Exec(ctx, `INSERT INTO redacted_lines(line_sha,spans,redaction_id,proof) VALUES($1,$2,$3,$4) ON CONFLICT(line_sha) DO UPDATE SET spans=EXCLUDED.spans,redaction_id=EXCLUDED.redaction_id,proof=EXCLUDED.proof`, sum[:], raw, id, proof); err != nil {
				return err
			}
		}
		return store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), ActorID: who.UserID, DeviceID: who.DeviceID,
			Action: "message.redaction.requested", TargetType: "message", TargetID: focus,
			Metadata: map[string]any{"redaction_id": id, "lines": lr, "all_copies": req.AllCopies, "by_admin": who.Admin, "owner": owner,
				"messages": len(newText), "chunks": chunks, "tails": tails, "fallbacks": res.Fallbacks, "job_id": res.JobID}, CreatedAt: now})
	})
	if err != nil {
		return res, err
	}
	res.ID, res.Messages, res.Chunks, res.Tails = id, len(newText), chunks, tails
	return res, nil
}

// redactTargets reads a redaction's target rows, ordered by id.
func redactTargets(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, focus, conv string, native *string, allCopies bool, sha []byte, admin bool, userID string) ([]redactTarget, error) {
	rows, err := q.Query(ctx, `SELECT m.id::text,m.text,COALESCE(m.source_id::text,''),COALESCE(s.device_id::text,''),
			m.source_generation,m.byte_offset,m.byte_len,COALESCE(m.enrichment->>'persisted_output',''),m.enrichment,COALESCE(m.native_id,''),m.conversation_id::text
		FROM messages m JOIN conversations c ON c.id=m.conversation_id LEFT JOIN sources s ON s.id=m.source_id
		WHERE m.id=$1 OR (m.conversation_id=$2 AND m.native_id IS NOT DISTINCT FROM $3 AND $3 IS NOT NULL)
		   OR ($4 AND m.content_sha=$5 AND ($6 OR c.user_id=$7))
		ORDER BY m.id`, focus, conv, native, allCopies, sha, admin, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []redactTarget
	for rows.Next() {
		var t redactTarget
		var gen int64
		if err := rows.Scan(&t.id, &t.text, &t.sourceID, &t.deviceID, &gen, &t.off, &t.n, &t.persisted, &t.enrichment, &t.native, &t.conv); err != nil {
			return nil, err
		}
		t.gen = &gen
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

// archived reads bytes of a source generation as stored (not through the
// redaction pass: these are the bytes being rewritten).
func (s *Store) archived(ctx context.Context, source string, gen, off, n int64) ([]byte, error) {
	g, err := ingest.LoadGeneration(ctx, s.Pool, source, gen)
	if err != nil {
		return nil, err
	}
	r := ingest.NewReader(ctx, s.Objects, g)
	buf := make([]byte, n)
	m, err := r.ReadAt(buf, off)
	if m == len(buf) {
		err = nil
	}
	return buf[:m], err
}

// persistedSpans masks a Claude tool output kept in a tool-results
// companion file: its text is the row's text.
func (s *Store) persistedSpans(ctx context.Context, t redactTarget, needles []string, whole bool, add func(string, int64, []redact.Span)) error {
	var src string
	err := s.Pool.QueryRow(ctx, `SELECT id::text FROM sources WHERE device_id=$1 AND storage_kind='companion' AND tombstoned_at IS NULL
		AND path LIKE '%/' || $2 ORDER BY first_seen_at DESC LIMIT 1`, t.deviceID, likeEscape(t.persisted)).Scan(&src)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	g, err := ingest.LoadGeneration(ctx, s.Pool, src, -1)
	if err != nil {
		return err
	}
	size := g.Size()
	data, err := s.archived(ctx, src, g.Generation, 0, size)
	if err != nil {
		return err
	}
	sp, found := redact.MaskRecord(data, needles, whole)
	if !found && !whole {
		sp, _ = redact.MaskRecord(data, nil, true)
	}
	add(src, g.Generation, sp)
	return nil
}

// maskTitle masks a conversation title derived from a redacted message:
// when the title is the start of the message's first line (the parsers'
// title rule), it takes the masked text at the same bytes (masks keep
// lengths); any needle elsewhere in it is masked too.
func maskTitle(title, orig, masked string, needles []string) string {
	if title == "" {
		return title
	}
	start := len(orig) - len(strings.TrimLeft(orig, " \t\r\n"))
	if len(masked) == len(orig) && strings.HasPrefix(orig[start:], title) {
		title = masked[start : start+len(title)]
	}
	for _, n := range needles {
		title = strings.ReplaceAll(title, n, maskString(n))
	}
	return title
}

// maskString is a needle's marker of the same length.
func maskString(n string) string {
	b := []byte(n)
	redact.FillSpans(b, []redact.Span{{Start: 0, End: len(b)}}, redact.MessageRule)
	return string(b)
}

// maskEnrichment hides the redacted text in a row's enrichment: the needles
// in every string, or for a whole message the fields that carry command
// lines and paths.
func maskEnrichment(e map[string]any, needles []string, whole bool) map[string]any {
	if e == nil {
		e = map[string]any{}
	}
	if whole {
		delete(e, "commands")
		delete(e, "changed_paths")
		return e
	}
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			for _, n := range needles {
				x = strings.ReplaceAll(x, n, maskString(n))
			}
			return x
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		case map[string]any:
			for k := range x {
				x[k] = walk(x[k])
			}
		}
		return v
	}
	walk(e)
	return e
}

// eachLine calls fn with every line (and its offset) of a source
// generation as stored.
func (s *Store) eachLine(ctx context.Context, source string, gen int64, fn func(off int64, line []byte)) error {
	g, err := ingest.LoadGeneration(ctx, s.Pool, source, gen)
	if err != nil {
		return err
	}
	r := ingest.NewReader(ctx, s.Objects, g)
	sc := bufio.NewScanner(io.NewSectionReader(r, 0, r.Size()))
	sc.Buffer(make([]byte, 64<<10), 256<<20)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i+1], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	var off int64
	for sc.Scan() {
		line := sc.Bytes()
		fn(off, bytes.TrimRight(line, "\n"))
		off += int64(len(line))
	}
	return sc.Err()
}

// jsonQuoted is s as it appears inside a JSON string (one level of
// escaping), for finding a native id in a record that nests JSON.
func jsonQuoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}
