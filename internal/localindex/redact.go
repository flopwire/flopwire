package localindex

// Local message tombstones (notes/redaction.md): the owner redacts a
// message after the fact, and their own index masks it too. The harness's
// transcript file on disk still holds the text, so every write of a row
// re-applies the tombstone: the sidecar file keeps them across reindexes
// and index rebuilds (it sits beside the database, not in it).
//
// Once redacted, text stays hidden in this index:
//
//   - The sidecar is replaced atomically (temp file, fsync, rename, fsync
//     of the directory), and a sidecar that does not parse refuses to open
//     the index instead of dropping tombstones.
//   - meta.redactions_reconciled is the sidecar length whose tombstones
//     the rows reflect. A redaction advances it in its own transaction; a
//     writing Open, and the writer after a failed commit, apply the
//     tombstones past it (reconcile), so a redaction whose row masks were
//     lost after its tombstone was written still takes effect.
//   - A line range keys on the hidden lines' content too, so a later
//     version of the record (grown, rewritten) is masked.
//   - Titles are masked by content: a title that holds a hidden line, or
//     is a hidden first line cut where a parser cuts titles, is masked
//     whole.
//   - Every hash in the sidecar is keyed (HMAC-SHA256) with a random key
//     in its own file beside it, so the sidecar alone allows no guessing.
//     Title cuts are recorded at the parsers' cut lengths only: hashes of
//     every prefix would give a line away a byte at a time.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

const (
	// titleMinMatch is the shortest hidden line (trimmed bytes) whose
	// presence anywhere in a title masks it, and the shortest title that
	// is masked as a truncation of a hidden first line. Shorter titles
	// are masked only when they equal a hidden line.
	titleMinMatch = 16
	// reconciledKey is the meta key holding the sidecar length the rows
	// reflect.
	reconciledKey = "redactions_reconciled"
)

// Tombstone is one sidecar entry. It holds no message text. Every hash in
// it is HMAC-SHA256 under the index's redaction key (keyer), which is in
// a separate file. In the clear it holds the session and native ids, the
// line range, the byte length of each hidden line, and Title, a masked
// title (marker bytes plus any title text that was not hidden).
//
// A row whose original content_sha keys to SHA is masked (lines From..To,
// or all of it when From is 0). A whole-message tombstone also masks
// every version of the record (Session, Native). A line-range tombstone
// masks, in every version of the record (rows of Session without a
// native id when Native is empty), each line whose trimmed text keys to
// one of Lines.
type Tombstone struct {
	SHA     string   `json:"sha,omitempty"`
	Session string   `json:"session,omitempty"`
	Native  string   `json:"native,omitempty"`
	From    int      `json:"from,omitempty"`
	To      int      `json:"to,omitempty"`
	Lines   []string `json:"lines,omitempty"` // each hidden line, trimmed, keyed
	Lens    []int    `json:"lens,omitempty"`  // and its length in bytes
	// Prefixes: base64 of the first 8 bytes of the keyed hash of each
	// titleCuts cut of the message's first line, when the redaction hid
	// it and the line is longer than the cut: a title cut there is masked.
	// At most len(titleCuts) per entry.
	Prefixes string `json:"prefixes,omitempty"`
	// TitleSHA and Title: a conversation title of Session that held the
	// redacted text (a title is the first prompt's start) keys to
	// TitleSHA and is written as Title, its masked form, wherever the
	// parser emits it.
	TitleSHA string `json:"title_sha,omitempty"`
	Title    string `json:"title,omitempty"`
	// KeyCheck, alone on the sidecar's first line, is the keyed hash of a
	// constant: a key that does not match it refuses to open the index
	// rather than miss every hash.
	KeyCheck string `json:"key_check,omitempty"`
}

type tombEntry struct {
	ts  Tombstone
	end int64 // sidecar offset after the entry
}

type tombstones struct {
	mu       sync.RWMutex
	bySHA    map[[32]byte][]Tombstone
	byNative map[string]bool              // whole-message records
	byRecord map[string]map[[32]byte]bool // line-range records: hidden line hashes
	k        *keyer                       // nil until the index has a redaction key
	byTitle  map[[32]byte]string          // exact titles seen by a redaction
	needles  map[[32]byte]bool            // every hidden line, trimmed
	lens     []int                        // needle lengths >= titleMinMatch, ascending
	prefixes map[[8]byte]bool             // truncations of hidden first lines
	entries  []tombEntry
	size     int64 // sidecar bytes loaded or written
}

// titleCuts are the rune counts the parsers cut a first-line title to
// (claude, codex). A hidden first line longer than a cut records the hash
// of that cut only: never a run of prefixes, which would give the line
// away a byte at a time.
var titleCuts = uniqueInts(claude.TitleRunes, codex.TitleRunes)

func uniqueInts(v ...int) []int {
	slices.Sort(v)
	return slices.Compact(v)
}

// keyer computes the sidecar's hashes: HMAC-SHA256 under the index's
// redaction key, with a domain byte per kind, so the sidecar (or its
// .prev) without the key file allows no offline guessing.
type keyer struct{ pool sync.Pool }

const (
	domSHA    = 's' // a row's content_sha
	domLine   = 'l' // a hidden line, trimmed (also title windows)
	domTitle  = 't' // an exact title
	domPrefix = 'p' // a cut of a hidden first line, trimmed
	domCheck  = 'c' // the sidecar header's key check
)

// check is the sidecar header's KeyCheck under this key.
func (k *keyer) check() string {
	sum := k.sum(domCheck, []byte("flopwire-redactions-v1"))
	return hex.EncodeToString(sum[:])
}

// errKeyMismatch: the key file does not belong to the sidecar.
var errKeyMismatch = errors.New("the key does not match the redactions")

func newKeyer(key []byte) *keyer {
	k := &keyer{}
	k.pool.New = func() any { return hmac.New(sha256.New, key) }
	return k
}

func (k *keyer) sum(dom byte, b []byte) [32]byte {
	h := k.pool.Get().(hash.Hash)
	h.Reset()
	h.Write([]byte{dom})
	h.Write(b)
	var out [32]byte
	h.Sum(out[:0])
	k.pool.Put(h)
	return out
}

func (s *Store) keyPath() string { return s.path + ".redactions.key" }

// ensureKey creates the index's redaction key on its first redaction: 32
// random bytes, hex, in a 0600 file beside the sidecar. It lives as long
// as the sidecar: a rebuild of the index keeps it.
func (s *Store) ensureKey() error {
	t := s.tombs
	t.mu.RLock()
	have := t.k != nil
	t.mu.RUnlock()
	if have {
		return nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	if _, err := writeFileAtomic(s.keyPath(), []byte(hex.EncodeToString(key)+"\n")); err != nil {
		return err
	}
	t.mu.Lock()
	t.k = newKeyer(key)
	t.mu.Unlock()
	return nil
}

// loadKey reads the redaction key; a missing file means none yet.
func loadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s is corrupt: want 64 hex digits; see %s", path, RecoveryDoc)
	}
	return key, nil
}

func newTombstones() *tombstones {
	return &tombstones{bySHA: map[[32]byte][]Tombstone{}, byNative: map[string]bool{}, byRecord: map[string]map[[32]byte]bool{},
		byTitle: map[[32]byte]string{}, needles: map[[32]byte]bool{}, prefixes: map[[8]byte]bool{}}
}

func recordKey(session, native string) string { return session + "\x00" + native }

func (s *Store) tombstonePath() string { return s.path + ".redactions.jsonl" }

// loadTombstones reads the sidecar; a missing file means none. A sidecar
// that does not parse fails: dropping an entry would show what it hid.
func (s *Store) loadTombstones() error {
	t := newTombstones()
	s.tombs = t
	key, err := loadKey(s.keyPath())
	if err != nil {
		return err
	}
	if key != nil {
		t.k = newKeyer(key)
	}
	data, err := os.ReadFile(s.tombstonePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) > 0 && key == nil {
		return fmt.Errorf("%s has redactions but its key %s is missing: the index will not open without them; see %s", s.tombstonePath(), s.keyPath(), RecoveryDoc)
	}
	if err := t.load(data); errors.Is(err, errKeyMismatch) {
		return fmt.Errorf("%s: %w in %s (a key from another index or backup?); the index will not open without every redaction it records: see %s", s.keyPath(), err, s.tombstonePath(), RecoveryDoc)
	} else if err != nil {
		return fmt.Errorf("%s is corrupt (%w); the index will not open without every redaction it records: see %s", s.tombstonePath(), err, RecoveryDoc)
	}
	return nil
}

func (t *tombstones) load(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var off int64
	for i := 1; len(data) > 0; i++ {
		j := bytes.IndexByte(data, '\n')
		if j < 0 {
			return fmt.Errorf("line %d is incomplete", i)
		}
		ts, err := parseTombstone(data[:j])
		if err != nil {
			return fmt.Errorf("line %d: %w", i, err)
		}
		off += int64(j + 1)
		switch {
		case i == 1 && ts.KeyCheck == "":
			return errors.New("line 1 is not the key check header")
		case i == 1:
			if !hmac.Equal([]byte(ts.KeyCheck), []byte(t.k.check())) {
				return errKeyMismatch
			}
			t.size = off
		case ts.KeyCheck != "":
			return fmt.Errorf("line %d: a second key check header", i)
		default:
			t.addLocked(ts, off)
		}
		data = data[j+1:]
	}
	return nil
}

func parseTombstone(b []byte) (Tombstone, error) {
	var ts Tombstone
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return ts, err
	}
	if dec.More() {
		return ts, errors.New("trailing data")
	}
	isSum := func(h string) bool {
		sum, err := hex.DecodeString(h)
		return err == nil && len(sum) == 32
	}
	if ts.KeyCheck != "" {
		if !isSum(ts.KeyCheck) || ts.SHA != "" || ts.Session != "" || ts.Native != "" || ts.From != 0 || ts.To != 0 ||
			len(ts.Lines) != 0 || len(ts.Lens) != 0 || ts.Prefixes != "" || ts.TitleSHA != "" || ts.Title != "" {
			return ts, errors.New("bad key check header")
		}
		return ts, nil
	}
	switch {
	case ts.SHA == "" && len(ts.Lines) == 0 && ts.TitleSHA == "":
		return ts, errors.New("empty tombstone")
	case ts.SHA != "" && !isSum(ts.SHA), ts.TitleSHA != "" && !isSum(ts.TitleSHA):
		return ts, errors.New("bad hash")
	case ts.From < 0 || ts.To < ts.From:
		return ts, errors.New("bad line range")
	}
	if len(ts.Lens) != len(ts.Lines) {
		return ts, errors.New("line hashes and lengths differ")
	}
	for i, l := range ts.Lines {
		if !isSum(l) || ts.Lens[i] <= 0 {
			return ts, errors.New("bad line hash")
		}
	}
	if p, err := base64.StdEncoding.DecodeString(ts.Prefixes); err != nil || len(p)%8 != 0 || len(p) > 8*len(titleCuts) {
		return ts, errors.New("bad title prefixes")
	}
	return ts, nil
}

// addLocked adds one entry that ends at sidecar offset end. t.mu is held.
func (t *tombstones) addLocked(ts Tombstone, end int64) {
	t.entries = append(t.entries, tombEntry{ts: ts, end: end})
	t.size = max(t.size, end)
	if sum, err := hex.DecodeString(ts.SHA); err == nil && len(sum) == 32 {
		t.bySHA[[32]byte(sum)] = append(t.bySHA[[32]byte(sum)], ts)
	}
	key := recordKey(ts.Session, ts.Native)
	if ts.From == 0 && ts.Native != "" && ts.SHA != "" {
		t.byNative[key] = true
	}
	for i, l := range ts.Lines {
		sum, _ := hex.DecodeString(l)
		h := [32]byte(sum)
		if ts.From > 0 {
			if t.byRecord[key] == nil {
				t.byRecord[key] = map[[32]byte]bool{}
			}
			t.byRecord[key][h] = true
		}
		t.needles[h] = true
		if n := ts.Lens[i]; n >= titleMinMatch {
			if j, found := slices.BinarySearch(t.lens, n); !found {
				t.lens = slices.Insert(t.lens, j, n)
			}
		}
	}
	if sum, err := hex.DecodeString(ts.TitleSHA); err == nil && len(sum) == 32 {
		t.byTitle[[32]byte(sum)] = ts.Title
	}
	p, _ := base64.StdEncoding.DecodeString(ts.Prefixes)
	for ; len(p) >= 8; p = p[8:] {
		t.prefixes[[8]byte(p[:8])] = true
	}
}

// lineHash is the hash a tombstone records for a hidden line: of its
// trimmed text; blank lines have none.
func (k *keyer) lineHash(line string) ([32]byte, bool) {
	l := strings.TrimSpace(line)
	if l == "" {
		return [32]byte{}, false
	}
	return k.sum(domLine, []byte(l)), true
}

func maskWhole(s string) string {
	b := []byte(s)
	redact.FillSpans(b, []redact.Span{{Start: 0, End: len(b)}}, redact.MessageRule)
	return string(b)
}

// maskLines masks every line of text whose hash is in set.
func (k *keyer) maskLines(text string, set map[[32]byte]bool) string {
	lines := strings.Split(text, "\n")
	changed := false
	for i, l := range lines {
		if h, ok := k.lineHash(l); ok && set[h] {
			lines[i], changed = maskWhole(l), true
		}
	}
	if !changed {
		return text
	}
	return strings.Join(lines, "\n")
}

// maskText applies the message tombstones to a row's text. t.mu is held
// (read).
func (t *tombstones) maskText(text string, sha [32]byte, session, native string) string {
	if t.k == nil {
		return text
	}
	if len(t.bySHA) > 0 {
		for _, ts := range t.bySHA[t.k.sum(domSHA, sha[:])] {
			text, _ = redact.MaskText(text, ts.From, ts.To)
		}
	}
	if len(t.byNative) == 0 && len(t.byRecord) == 0 {
		return text
	}
	key := recordKey(session, native)
	if native != "" && t.byNative[key] {
		text, _ = redact.MaskText(text, 0, 0)
		return text
	}
	if set := t.byRecord[key]; set != nil {
		text = t.k.maskLines(text, set)
	}
	return text
}

// maskTitle returns the title to store: the masked form a redaction
// recorded for this exact title, else the whole title masked when its
// trimmed text equals a hidden line, contains one of titleMinMatch bytes
// or more, or is a truncation (titleMinMatch bytes or more) of a hidden
// first line. t.mu is held (read).
func (t *tombstones) maskTitle(title string) string {
	if title == "" || t.k == nil {
		return title
	}
	if nt, ok := t.byTitle[t.k.sum(domTitle, []byte(title))]; ok {
		return nt
	}
	if len(t.needles) == 0 && len(t.prefixes) == 0 {
		return title
	}
	tt := []byte(strings.TrimSpace(title))
	if len(tt) == 0 {
		return title
	}
	p := t.k.sum(domPrefix, tt)
	hit := t.needles[t.k.sum(domLine, tt)] || len(tt) >= titleMinMatch && t.prefixes[[8]byte(p[:8])]
	for _, n := range t.lens {
		if hit || n > len(tt) {
			break
		}
		for i := 0; i+n <= len(tt) && !hit; i++ {
			hit = t.needles[t.k.sum(domLine, tt[i:i+n])]
		}
	}
	if !hit {
		return title
	}
	return maskWhole(title)
}

// maskTitles applies the title tombstones to conversations about to be
// written.
func (t *tombstones) maskTitles(convs []*transcript.Conversation) {
	if t == nil {
		return
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.byTitle) == 0 && len(t.needles) == 0 && len(t.prefixes) == 0 {
		return
	}
	for _, c := range convs {
		c.Title = t.maskTitle(c.Title)
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
	if len(t.bySHA) == 0 && len(t.byNative) == 0 && len(t.byRecord) == 0 {
		return
	}
	for i, m := range msgs {
		if text := t.maskText(m.Text, m.ContentSHA, m.SessionID, m.NativeID); text != m.Text {
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
// rows masked once the transaction holding the masks commits, or that
// commit's error.
func (s *Store) RedactMessage(ctx context.Context, r LocalRedaction) (int, error) {
	var n int
	err := s.writeWait(ctx, func(w *writeTx) error {
		var err error
		n, err = w.redactMessage(r)
		return err
	})
	if err != nil {
		if s.reconcileDue.Load() {
			// Apply what reached the sidecar now, not at the next write.
			err = errors.Join(err, s.Sync(ctx))
		}
		return 0, err
	}
	return n, nil
}

// redactTarget is a stored row a redaction or reconcile may mask.
type redactTarget struct {
	id, conv        int64
	text            string
	sha             [32]byte
	session, native string
}

const targetSQL = `SELECT m.id, m.conversation_id, m.text, m.content_sha, c.session_id, ifnull(m.native_id, '')
	FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE `

// Lookups of a redaction's and a reconcile's rows, all through indexes:
// by row id, by text hash (messages_sha, built on first use), and by
// record within one conversation.
const (
	targetByIDSQL       = targetSQL + `m.id = ?`
	targetBySHASQL      = targetSQL + `m.content_sha = ?`
	targetByNativeSQL   = targetSQL + `m.conversation_id = ? AND m.native_id = ?`
	targetNoNativeSQL   = targetSQL + `m.conversation_id = ? AND m.native_id IS NULL`
	sessionConvsSQL     = `SELECT id FROM conversations WHERE session_id = ?`
	reconciledSQL       = `SELECT CAST(value AS INTEGER) FROM meta WHERE key = '` + reconciledKey + `'`
	setReconciledSQL    = `INSERT INTO meta (key, value) VALUES ('` + reconciledKey + `', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`
	addressByPathSQL    = `SELECT m.id FROM messages m JOIN sources s ON s.id = m.source_id WHERE s.path = ? AND m.line_no = ? ORDER BY m.superseded, m.version DESC LIMIT 1`
	addressBySessionSQL = `SELECT m.id FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE c.session_id = ? AND m.ordinal = ? ORDER BY m.superseded, m.version DESC LIMIT 1`
)

// targets appends the rows q returns that are not in seen.
func (w *writeTx) targets(out []redactTarget, seen map[int64]bool, q string, args ...any) ([]redactTarget, error) {
	st, err := w.stmt(q)
	if err != nil {
		return nil, err
	}
	rows, err := st.QueryContext(w.ctx, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t redactTarget
		var z, sha []byte
		if err := rows.Scan(&t.id, &t.conv, &z, &sha, &t.session, &t.native); err != nil {
			return nil, err
		}
		if seen[t.id] || len(sha) != 32 {
			continue
		}
		seen[t.id] = true
		if t.text, err = decompress(z); err != nil {
			return nil, err
		}
		t.sha = [32]byte(sha)
		out = append(out, t)
	}
	return out, rows.Err()
}

// recordTargets appends every version of a record: the rows of session
// with native id native, or without one when native is empty.
func (w *writeTx) recordTargets(out []redactTarget, seen map[int64]bool, session, native string) ([]redactTarget, error) {
	convs, err := w.sessionConvs(session)
	if err != nil {
		return nil, err
	}
	for _, c := range convs {
		if native != "" {
			out, err = w.targets(out, seen, targetByNativeSQL, c, native)
		} else {
			out, err = w.targets(out, seen, targetNoNativeSQL, c)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (w *writeTx) sessionConvs(session string) ([]int64, error) {
	st, err := w.stmt(sessionConvsSQL)
	if err != nil {
		return nil, err
	}
	rows, err := st.QueryContext(w.ctx, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (w *writeTx) redactMessage(r LocalRedaction) (int, error) {
	var row *sql.Row
	var err error
	if r.Path != "" {
		row, err = w.queryRow(addressByPathSQL, r.Path, r.Line)
	} else {
		var session string
		if session, err = w.uniqueSession(r.Session); err != nil {
			return 0, err
		}
		row, err = w.queryRow(addressBySessionSQL, session, r.Ordinal)
	}
	if err != nil {
		return 0, err
	}
	var id int64
	if err := row.Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return 0, &NotFoundError{What: "message", ID: id}
	} else if err != nil {
		return 0, err
	}
	seen := map[int64]bool{}
	targets, err := w.targets(nil, seen, targetByIDSQL, id)
	if err != nil || len(targets) != 1 {
		return 0, errors.Join(err, &NotFoundError{What: "message", ID: id})
	}
	addr := targets[0]
	if targets, err = w.recordTargets(targets, seen, addr.session, addr.native); err != nil {
		return 0, err
	}
	if r.AllCopies {
		if err := w.ensureSHAIndex(); err != nil {
			return 0, err
		}
		if targets, err = w.targets(targets, seen, targetBySHASQL, addr.sha[:]); err != nil {
			return 0, err
		}
	}

	if err := w.s.ensureKey(); err != nil {
		return 0, err
	}
	w.s.tombs.mu.RLock()
	k := w.s.tombs.k
	w.s.tombs.mu.RUnlock()
	from, to := r.From, r.To
	_, hidden := redact.MaskText(addr.text, from, to)
	if from == 0 {
		hidden = strings.Split(addr.text, "\n")
	}
	lines, lens := k.lineHashes(hidden)
	// The tombstones: one per row text and record. A line range applies
	// by line number only to the addressed text (another version's lines
	// may differ), and by line content to every version.
	var added []Tombstone
	keys := map[string]bool{}
	for _, t := range targets {
		ts := Tombstone{Session: t.session, Native: t.native, From: from, To: to}
		if from == 0 || t.sha == addr.sha {
			sum := k.sum(domSHA, t.sha[:])
			ts.SHA = hex.EncodeToString(sum[:])
		}
		if from > 0 {
			ts.Lines, ts.Lens = lines, lens
		}
		if ts.SHA == "" && len(ts.Lines) == 0 {
			continue // another version, and only blank lines hidden: nothing to key on
		}
		k := ts.SHA + "\x00" + recordKey(ts.Session, ts.Native)
		if !keys[k] {
			keys[k] = true
			added = append(added, ts)
		}
	}
	if from == 0 {
		// Title needles only: a whole message masks its versions by record.
		added[0].Lines, added[0].Lens = lines, lens
	}
	added[0].Prefixes = k.titlePrefixes(addr.text, from, to)
	tmp := newTombstones()
	tmp.k = k
	for _, ts := range added {
		tmp.addLocked(ts, 0)
	}
	n := 0
	for _, t := range targets {
		masked := tmp.maskText(t.text, t.sha, t.session, t.native)
		if masked == t.text {
			continue
		}
		ts, err := w.maskRow(t, masked)
		if err != nil {
			return 0, err
		}
		if ts.TitleSHA != "" {
			ts.Session = t.session
			added = append(added, ts)
		}
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("localindex: nothing to redact")
	}
	// Record the tombstones in this request, on the writer: a batch the
	// writer runs next masks its rows with them (ApplyBatch), and a batch
	// it ran before wrote rows this request just masked. The marker moves
	// in this transaction, so if it is lost the next reconcile applies
	// the sidecar again.
	return n, w.recordTombstones(added)
}

// shaIndexSQL builds messages_sha, the index on messages.content_sha
// that --all-copies finds copies through. It is not in schemaSQL: its key
// is random, so every row written would dirty a different leaf page, and
// each one is copied into the savepoint's sub-journal of every write
// request (write-path CPU +48%, pages fetched per row +11%, for 60k rows).
// Only an --all-copies redaction looks rows up by content_sha (reconcile
// finds a tombstone's rows by session and native id), so the first one
// builds it, inside its own request, and the index is kept from then on.
// Building it reads the whole messages table once and holds the writer
// meanwhile (reads go on): measured 1.3s at 200k rows (a 416MB database)
// and 13.3s at 1.5M rows (3.3GB), against 0.3s for the same redaction
// with the index in place. That is a one-time cost of the first
// --all-copies redaction, which the user asked for and waits on.
const shaIndexSQL = `CREATE INDEX IF NOT EXISTS messages_sha ON messages (content_sha)`

// ensureSHAIndex builds messages_sha when the index has none yet. A
// request that fails afterwards rolls the build back with it; the next
// --all-copies redaction builds it again.
func (w *writeTx) ensureSHAIndex() error {
	var n int
	if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'messages_sha'`).Scan(&n); err != nil || n > 0 {
		return err
	}
	start := time.Now()
	if _, err := w.tx.ExecContext(w.ctx, shaIndexSQL); err != nil {
		return fmt.Errorf("localindex: build messages_sha: %w", err)
	}
	slog.Info("localindex: built the copies index for --all-copies", "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// lineHashes returns the tombstone hashes and lengths of the non-blank
// hidden lines, without repeats.
func (k *keyer) lineHashes(hidden []string) ([]string, []int) {
	var hashes []string
	var lens []int
	seen := map[[32]byte]bool{}
	for _, l := range hidden {
		h, ok := k.lineHash(l)
		if !ok || seen[h] {
			continue
		}
		seen[h] = true
		hashes = append(hashes, hex.EncodeToString(h[:]))
		lens = append(lens, len(strings.TrimSpace(l)))
	}
	return hashes, lens
}

// titlePrefixes returns the Prefixes of a redaction of lines from..to of
// text: when its first non-blank line (the parsers' title source) is
// hidden, the hash of each titleCuts cut of that line that is shorter
// than the line and at least titleMinMatch bytes (trimmed, as the title
// is matched); else "". A line within every cut needs none: its title
// is the line itself, a needle.
func (k *keyer) titlePrefixes(text string, from, to int) string {
	for i, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if from > 0 && (i+1 < from || i+1 > to) {
			return ""
		}
		// As the parsers' strings.TrimSpace: Unicode spaces too.
		l = strings.TrimLeftFunc(l, unicode.IsSpace)
		full := strings.TrimSpace(l)
		var buf []byte
		for _, n := range titleCuts {
			r := []rune(l)
			if len(r) <= n {
				continue
			}
			cut := strings.TrimSpace(string(r[:n]))
			if len(cut) < titleMinMatch || cut == full {
				continue
			}
			h := k.sum(domPrefix, []byte(cut))
			buf = append(buf, h[:8]...)
		}
		return base64.StdEncoding.EncodeToString(buf)
	}
	return ""
}

// maskRow stores masked as the row's text: digest, title and FTS
// included. It returns the title tombstone to record, if the title
// changed.
func (w *writeTx) maskRow(t redactTarget, masked string) (Tombstone, error) {
	orig := strings.Split(t.text, "\n")
	now := strings.Split(masked, "\n")
	hidden := orig
	if len(now) == len(orig) {
		hidden = nil
		for i := range orig {
			if orig[i] != now[i] {
				hidden = append(hidden, orig[i])
			}
		}
	}
	if !w.scrub {
		// Zero the old text, title and digest as their cells are freed;
		// commit turns it off and truncates the WAL (scrubMain).
		if _, err := w.exec(`PRAGMA secure_delete = ON`); err != nil {
			return Tombstone{}, err
		}
		w.scrub = true
	}
	if err := w.maskDigest(t.conv, hidden); err != nil {
		return Tombstone{}, err
	}
	ts, err := w.maskTitle(t.conv, t.text, masked, hidden)
	if err != nil {
		return Tombstone{}, err
	}
	p := &prepared{z: compress(masked), text: masked}
	if err := w.ftsDelete(t.id); err != nil {
		return Tombstone{}, err
	}
	if _, err := w.exec(`UPDATE messages SET text = ?, text_len = ? WHERE id = ?`, p.z, len(masked), t.id); err != nil {
		return Tombstone{}, err
	}
	w.ftsInsert(t.id, p)
	return ts, nil
}

// testHookSidecarWrite, when set, writes the sidecar's new content to the
// temporary file in place of f.Write (tests inject a short write).
var testHookSidecarWrite func(f *os.File, b []byte) (int, error)

// testHookDirSync, when set, replaces the directory sync after a file
// replaced by writeFileAtomic is renamed into place (tests inject a
// failure).
var testHookDirSync func(path string) error

// testHookReconcile, when set, runs before each reconcile on the writer;
// an error it returns fails the reconcile (tests inject a failure).
var testHookReconcile func() error

// recordTombstones adds tombstones to the sidecar, then applies them to
// later writes, and advances the reconciled marker when nothing earlier
// is pending. It runs on the writer. Tombstones stay in effect for later
// writes even if this transaction is lost, which errs toward hiding; the
// marker makes the next reconcile apply them to the rows.
func (w *writeTx) recordTombstones(added []Tombstone) error {
	s, t := w.s, w.s.tombs
	old, err := os.ReadFile(s.tombstonePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	t.mu.RLock()
	known := t.size
	t.mu.RUnlock()
	if int64(len(old)) != known {
		return fmt.Errorf("localindex: %s changed since the index opened (%d bytes, %d expected); reopen the index", s.tombstonePath(), len(old), known)
	}
	buf := old
	if len(buf) == 0 {
		t.mu.RLock()
		hdr, err := json.Marshal(Tombstone{KeyCheck: t.k.check()})
		t.mu.RUnlock()
		if err != nil {
			return err
		}
		buf = append(hdr, '\n')
	}
	ends := make([]int64, len(added))
	for i, ts := range added {
		b, err := json.Marshal(ts)
		if err != nil {
			return err
		}
		// Write only what loadTombstones accepts: an entry it rejects
		// would keep the index from opening.
		if _, err := parseTombstone(b); err != nil {
			return fmt.Errorf("localindex: tombstone %s: %w", b, err)
		}
		buf = append(append(buf, b...), '\n')
		ends[i] = int64(len(buf))
	}
	// The sidecar being replaced is kept as .prev, the last good copy for
	// recovery (RecoveryDoc).
	if len(old) > 0 {
		if _, err := writeFileAtomic(s.tombstonePath()+".prev", old); err != nil {
			return err
		}
	}
	renamed, err := writeFileAtomic(s.tombstonePath(), buf)
	if renamed {
		// The new file is in place (even if syncing its directory
		// failed): it is what the index holds now.
		t.mu.Lock()
		for i, ts := range added {
			t.addLocked(ts, ends[i])
		}
		t.mu.Unlock()
	}
	if err != nil {
		// This request rolls back its row masks; with the entries
		// loaded, the next transaction reconciles them. A file not
		// renamed leaves a reconcile due from before as it is.
		if renamed {
			s.reconcileDue.Store(true)
		}
		return err
	}
	marker, err := w.reconciled()
	if err == nil && marker == known {
		_, err = w.exec(setReconciledSQL, strconv.FormatInt(int64(len(buf)), 10))
	} else if err == nil {
		s.reconcileDue.Store(true) // earlier entries are pending: the next transaction applies them
	}
	if err != nil {
		s.reconcileDue.Store(true)
	}
	return err
}

// writeFileAtomic replaces path with data: a temporary file beside it,
// synced, renamed over it, and the directory synced, so a crash or a full
// disk leaves the old file or the new one, never a torn one. renamed
// reports that the new file is in place, which holds when only the
// directory sync failed.
func writeFileAtomic(path string, data []byte) (renamed bool, err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	var n int
	if testHookSidecarWrite != nil {
		n, err = testHookSidecarWrite(f, data)
	} else {
		n, err = f.Write(data)
	}
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return false, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return true, err
	}
	if testHookDirSync != nil {
		err = testHookDirSync(path)
	} else {
		err = d.Sync()
	}
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return true, err
}

// reconciled returns the sidecar length the rows reflect.
func (w *writeTx) reconciled() (int64, error) {
	row, err := w.queryRow(reconciledSQL)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := row.Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return n, nil
}

// reconcile applies the sidecar entries past the reconciled marker to the
// stored rows and titles (a redaction whose transaction was lost after
// it wrote the sidecar, or a sidecar placed beside a database), then
// advances the marker. With nothing pending it is one read of meta. Each
// entry is found through indexes: rows by text hash, records and titles
// by session.
func (w *writeTx) reconcile() error {
	if testHookReconcile != nil {
		if err := testHookReconcile(); err != nil {
			return err
		}
	}
	t := w.s.tombs
	t.mu.RLock()
	size, entries := t.size, t.entries
	t.mu.RUnlock()
	marker, err := w.reconciled()
	if err != nil || marker == size {
		return err
	}
	if marker > size {
		marker = 0 // not this sidecar's length: apply all of it
	}
	for _, e := range entries {
		if e.end > marker {
			if err := w.applyTombstone(e.ts); err != nil {
				return err
			}
		}
	}
	_, err = w.exec(setReconciledSQL, strconv.FormatInt(size, 10))
	return err
}

// applyTombstone masks the stored rows and titles one sidecar entry
// covers, with every tombstone loaded.
func (w *writeTx) applyTombstone(ts Tombstone) error {
	t := w.s.tombs
	seen := map[int64]bool{}
	var targets []redactTarget
	var err error
	// Every message tombstone names its record (a redaction records one
	// per target, copies included); its SHA is keyed, so rows are found by
	// record, not by content_sha.
	if ts.Session != "" && (ts.SHA != "" || len(ts.Lines) > 0) {
		if targets, err = w.recordTargets(targets, seen, ts.Session, ts.Native); err != nil {
			return err
		}
	}
	for _, tg := range targets {
		t.mu.RLock()
		masked := t.maskText(tg.text, tg.sha, tg.session, tg.native)
		t.mu.RUnlock()
		if masked != tg.text {
			if _, err := w.maskRow(tg, masked); err != nil {
				return err
			}
		}
	}
	if ts.Session == "" {
		return nil
	}
	convs, err := w.sessionConvs(ts.Session)
	if err != nil {
		return err
	}
	for _, c := range convs {
		row, err := w.queryRow(`SELECT ifnull(title, '') FROM conversations WHERE id = ?`, c)
		if err != nil {
			return err
		}
		var title string
		if err := row.Scan(&title); err != nil {
			return err
		}
		t.mu.RLock()
		nt := t.maskTitle(title)
		t.mu.RUnlock()
		if nt != title {
			if _, err := w.exec(`UPDATE conversations SET title = ? WHERE id = ?`, nt, c); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileRequest runs reconcile as a request of the writer's current
// transaction, logging a failure and leaving the reconcile due.
func (s *Store) reconcileRequest() writeReq {
	return writeReq{ctx: context.Background(), done: make(chan error, 1), fn: func(w *writeTx) error {
		err := w.reconcile()
		if err != nil {
			s.reconcileDue.Store(true)
			// Log once per run of failures: it retries before every write.
			if !s.reconcileFailing.Swap(true) {
				slog.Warn("localindex: applying redactions failed; retrying before each write", "err", err, "help", RecoveryDoc)
			}
		} else if s.reconcileFailing.Swap(false) {
			slog.Info("localindex: applying redactions succeeded")
		}
		return err
	}}
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
	start := len(orig) - len(strings.TrimLeftFunc(orig, unicode.IsSpace)) // as the parsers' TrimSpace
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
	w.s.tombs.mu.RLock()
	k := w.s.tombs.k
	w.s.tombs.mu.RUnlock()
	sum := k.sum(domTitle, []byte(title))
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

// RecoveryDoc is the user documentation of what to do when the index will
// not open because of its redactions.
const RecoveryDoc = "docs/agent.md#recover-the-local-index"
