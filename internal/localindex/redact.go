package localindex

// Local message tombstones (notes/redaction.md): the owner redacts a
// message after the fact, and their own index masks it too. The harness's
// transcript file on disk still holds the text, so every write of a row
// re-applies the tombstone: the sidecar file keeps them across reindexes
// and index rebuilds (it sits beside the database, not in it).

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Tombstone masks every row whose original text hashes to SHA (lines
// From..To, or all of it when From is 0), and, for a whole message, every
// version of the record (Session, Native).
type Tombstone struct {
	SHA     string `json:"sha"`
	Session string `json:"session,omitempty"`
	Native  string `json:"native,omitempty"`
	From    int    `json:"from,omitempty"`
	To      int    `json:"to,omitempty"`
	// TitleSHA and Title: a conversation title that held the redacted
	// text (a title is the first prompt's start) hashes to TitleSHA and
	// is written as Title, its masked form, wherever the parser emits it.
	TitleSHA string `json:"title_sha,omitempty"`
	Title    string `json:"title,omitempty"`
}

type tombstones struct {
	mu       sync.RWMutex
	bySHA    map[[32]byte]Tombstone
	byNative map[string]Tombstone
	byTitle  map[[32]byte]string
}

func (s *Store) tombstonePath() string { return s.path + ".redactions.jsonl" }

// loadTombstones reads the sidecar; a missing file means none.
func (s *Store) loadTombstones() error {
	t := &tombstones{bySHA: map[[32]byte]Tombstone{}, byNative: map[string]Tombstone{}, byTitle: map[[32]byte]string{}}
	s.tombs = t
	f, err := os.Open(s.tombstonePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ts Tombstone
		if json.Unmarshal(sc.Bytes(), &ts) == nil {
			t.add(ts)
		}
	}
	return sc.Err()
}

func (t *tombstones) add(ts Tombstone) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if sum, err := hex.DecodeString(ts.SHA); err == nil && len(sum) == 32 {
		t.bySHA[[32]byte(sum)] = ts
	}
	if ts.From == 0 && ts.Native != "" {
		t.byNative[ts.Session+"\x00"+ts.Native] = ts
	}
	if sum, err := hex.DecodeString(ts.TitleSHA); err == nil && len(sum) == 32 {
		t.byTitle[[32]byte(sum)] = ts.Title
	}
}

// maskTitles applies the title tombstones to conversations about to be
// written.
func (t *tombstones) maskTitles(convs []*transcript.Conversation) {
	if t == nil {
		return
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.byTitle) == 0 {
		return
	}
	for _, c := range convs {
		if nt, ok := t.byTitle[sha256.Sum256([]byte(c.Title))]; ok && c.Title != "" {
			c.Title = nt
		}
	}
}

// mask applies the tombstones to messages about to be written, preparing
// again (prep[i]) each one it changes.
func (t *tombstones) mask(msgs []*transcript.Message, prep []prepared) {
	if t == nil {
		return
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.bySHA) == 0 {
		return
	}
	for i, m := range msgs {
		text := m.Text
		if ts, ok := t.bySHA[m.ContentSHA]; ok {
			text, _ = redact.MaskText(m.Text, ts.From, ts.To)
		} else if _, ok := t.byNative[m.SessionID+"\x00"+m.NativeID]; ok && m.NativeID != "" {
			text, _ = redact.MaskText(m.Text, 0, 0)
		}
		if text != m.Text {
			m.Text = text
			prep[i] = prepared{z: compress(text), text: text}
		}
	}
}

// LocalRedaction names the message to redact in this index: a session
// (full id or unique prefix) and ordinal, or a transcript path and line.
type LocalRedaction struct {
	Session   string
	Ordinal   int64
	Path      string
	Line      int64
	From, To  int // text lines; 0: the whole message
	AllCopies bool
}

// RedactMessage masks a message (and its other versions, and with
// AllCopies every row with the same text) in this index, and records the
// tombstone so later writes of those rows are masked too. It returns the
// rows masked.
func (s *Store) RedactMessage(ctx context.Context, r LocalRedaction) (int, error) {
	var n int
	var added []Tombstone
	err := s.write(ctx, func(w *writeTx) error {
		n, added = 0, nil
		var row *sql.Row
		var err error
		if r.Path != "" {
			row, err = w.queryRow(`SELECT m.id, m.conversation_id, coalesce(m.native_id,''), m.content_sha, c.session_id FROM messages m
				JOIN conversations c ON c.id = m.conversation_id JOIN sources s ON s.id = m.source_id
				WHERE s.path = ? AND m.line_no = ? ORDER BY m.superseded, m.version DESC LIMIT 1`, r.Path, r.Line)
		} else {
			session, err := w.uniqueSession(r.Session)
			if err != nil {
				return err
			}
			row, err = w.queryRow(`SELECT m.id, m.conversation_id, coalesce(m.native_id,''), m.content_sha, c.session_id FROM messages m
				JOIN conversations c ON c.id = m.conversation_id
				WHERE c.session_id = ? AND m.ordinal = ? ORDER BY m.superseded, m.version DESC LIMIT 1`, session, r.Ordinal)
		}
		if err != nil {
			return err
		}
		var id, conv int64
		var native, session string
		var sha []byte
		if err := row.Scan(&id, &conv, &native, &sha, &session); errors.Is(err, sql.ErrNoRows) {
			return &NotFoundError{What: "message", ID: id}
		} else if err != nil {
			return err
		}
		rows, err := w.tx.QueryContext(w.ctx, `SELECT id, text, content_sha, conversation_id FROM messages
			WHERE id = ? OR (conversation_id = ? AND native_id = ? AND ? <> '') OR (? AND content_sha = ?)`,
			id, conv, native, native, r.AllCopies, sha)
		if err != nil {
			return err
		}
		type target struct {
			id   int64
			text string
			sha  []byte
			conv int64
		}
		var targets []target
		for rows.Next() {
			var t target
			var z []byte
			if err := rows.Scan(&t.id, &z, &t.sha, &t.conv); err != nil {
				rows.Close()
				return err
			}
			if t.text, err = decompress(z); err != nil {
				rows.Close()
				return err
			}
			targets = append(targets, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, t := range targets {
			from, to := r.From, r.To
			if !bytesEqual(t.sha, sha) && from > 0 {
				continue // another version: its lines may differ
			}
			masked, hidden := redact.MaskText(t.text, from, to)
			if masked == t.text {
				continue
			}
			if from == 0 {
				hidden = strings.Split(t.text, "\n")
			}
			if err := w.maskDigest(t.conv, hidden); err != nil {
				return err
			}
			if ts, err := w.maskTitle(t.conv, t.text, masked, hidden); err != nil {
				return err
			} else if ts.TitleSHA != "" {
				added = append(added, ts)
			}
			p := &prepared{z: compress(masked), text: masked}
			if err := w.ftsDelete(t.id); err != nil {
				return err
			}
			if _, err := w.exec(`UPDATE messages SET text = ?, text_len = ? WHERE id = ?`, p.z, len(masked), t.id); err != nil {
				return err
			}
			w.ftsInsert(t.id, p)
			n++
			if k := hex.EncodeToString(t.sha); !seen[k] {
				seen[k] = true
				added = append(added, Tombstone{SHA: k, Session: session, Native: native, From: from, To: to})
			}
		}
		// Record the tombstones in this request, on the writer: a batch
		// the writer runs next masks its rows with them (ApplyBatch), and
		// a batch it ran before wrote rows this request just masked. A
		// tombstone whose transaction is lost after this (a failed deferred
		// commit) still masks later writes, which errs toward hiding.
		return s.recordTombstones(added)
	})
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("localindex: nothing to redact")
	}
	return n, s.Sync(ctx)
}

// recordTombstones appends tombstones to the sidecar, then applies them to
// later writes. It runs on the writer.
func (s *Store) recordTombstones(added []Tombstone) error {
	if len(added) == 0 {
		return nil
	}
	f, err := os.OpenFile(s.tombstonePath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var buf []byte
	for _, ts := range added {
		b, _ := json.Marshal(ts)
		buf = append(append(buf, b...), '\n')
	}
	_, err = f.Write(buf)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	for _, ts := range added {
		s.tombs.add(ts)
	}
	return nil
}

// uniqueSession resolves a session id or unique prefix.
func (w *writeTx) uniqueSession(prefix string) (string, error) {
	rows, err := w.tx.QueryContext(w.ctx, `SELECT DISTINCT session_id FROM conversations
		WHERE substr(session_id, 1, length(?)) = ? ORDER BY session_id <> ? LIMIT 2`, prefix, prefix, prefix)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	switch {
	case len(ids) == 0:
		return "", fmt.Errorf("localindex: no session %s", prefix)
	case len(ids) > 1 && ids[0] != prefix:
		return "", fmt.Errorf("localindex: session prefix %s is ambiguous", prefix)
	}
	return ids[0], rows.Err()
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

// maskTitle masks the conversation's title when it holds redacted text:
// a title is the start of the first prompt's first line (the parsers'
// title rule), so it takes the masked bytes there (masks keep lengths);
// a title holding a hidden line elsewhere is masked whole. It returns the
// title tombstone to record, empty when the title is unchanged.
func (w *writeTx) maskTitle(conv int64, orig, masked string, hidden []string) (Tombstone, error) {
	row, err := w.queryRow(`SELECT ifnull(title, '') FROM conversations WHERE id = ?`, conv)
	if err != nil {
		return Tombstone{}, err
	}
	var title string
	if err := row.Scan(&title); err != nil || title == "" {
		return Tombstone{}, err
	}
	nt := title
	start := len(orig) - len(strings.TrimLeft(orig, " \t\r\n"))
	if len(masked) == len(orig) && strings.HasPrefix(orig[start:], title) {
		nt = masked[start : start+len(title)]
	}
	for _, h := range hidden {
		if h = strings.TrimSpace(h); nt == title && h != "" && strings.Contains(title, h) {
			b := []byte(title)
			redact.FillSpans(b, []redact.Span{{Start: 0, End: len(b)}}, redact.MessageRule)
			nt = string(b)
		}
	}
	if nt == title {
		return Tombstone{}, nil
	}
	if _, err := w.exec(`UPDATE conversations SET title = ? WHERE id = ?`, nt, conv); err != nil {
		return Tombstone{}, err
	}
	sum := sha256.Sum256([]byte(title))
	return Tombstone{TitleSHA: hex.EncodeToString(sum[:]), Title: nt}, nil
}

// maskDigest hides a redaction's lines in the conversation's digest (its
// intent, last reply, paths and ids come from message text).
func (w *writeTx) maskDigest(conv int64, lines []string) error {
	row, err := w.queryRow(`SELECT digest FROM conversations WHERE id = ?`, conv)
	if err != nil {
		return err
	}
	var d sql.NullString
	if err := row.Scan(&d); err != nil || !d.Valid {
		return err
	}
	mask := func(s string) string {
		b := []byte(s)
		redact.FillSpans(b, []redact.Span{{Start: 0, End: len(b)}}, redact.MessageRule)
		return string(b)
	}
	if nd := string(digest.Mask([]byte(d.String), lines, mask)); nd != d.String {
		_, err = w.exec(`UPDATE conversations SET digest = ? WHERE id = ?`, nd, conv)
	}
	return err
}
