// Package syncproto is the device-to-server raw-evidence sync protocol,
// version 1 (spec §4.1, §6.4, §7.1 of notes/local-search/README.md).
//
// A device uploads each source (a transcript file, a companion file, or a
// SQLite export) as generations of content-defined chunks. Two endpoints:
//
//	POST /v1/sync/has    JSON HasRequest -> JSON HasResponse
//	POST /v1/sync/flush  framed FlushRequest (see EncodeFlush) -> JSON FlushResponse
//
// Both authenticate with the existing device credential
// ("Authorization: Bearer <token>", internal/auth); the server derives the
// device id from the credential, never from the request. Both carry the
// header X-Flopwire-Sync-Version: 1. A server that does not speak the version
// answers 400 with an ErrorResponse whose code is "unsupported_version".
//
// # Identity
//
// A server source is (credential's device id, Source.Path, Source.FileID),
// matching the sources unique key. Generations are chosen by the device and
// are strictly increasing per device path, including across a file-identity
// change: when a path gets a new FileID, the device starts generation g+1
// under the new (path, file_id) and names the old one in Source.Previous so
// the server can mark its rows superseded.
//
// # Flush semantics (the server applies these in order, in one transaction)
//
//  1. Validate the frame: every Body is a zstd frame of ZSize bytes that
//     decodes to Size bytes hashing (BLAKE3-256) to its Hash (codec.go);
//     the payload length equals the sum of body ZSizes plus
//     Tail.Size-Tail.From (tails are not compressed); Entries have contiguous
//     ordinals and each Offset equals the previous Offset+Size. Violations
//     are 400 "bad_request" and nothing is stored.
//  2. Generation. If Generation is below the source's latest generation the
//     answer is StatusStaleGeneration with Generation set to the latest; the
//     device restarts at a higher generation. If above, create it (empty).
//  3. Entries, in order, against the generation's committed manifest of n
//     entries: an entry with Ordinal < n must equal the committed one (hash
//     and offset), else StatusNewGeneration (the device lost state or
//     rewrote history in place and must re-send the file as a new
//     generation); an entry with Ordinal == n is appended when its body is
//     in this request or the device's user already holds the chunk (one of
//     their devices uploaded it or one of their sources references it; a
//     hash alone never reaches another user's bytes), else processing stops
//     and its hash is listed in Missing; an entry with Ordinal > n stops
//     processing (a gap; the device re-sends from AckedEntries).
//  4. Tail. A stored tail always starts at the manifest end: once the
//     committed manifest passes the stored tail's offset, that tail is
//     deleted (its bytes are now finalized or will be re-sent). Then, when
//     every entry applied and Tail.Offset equals the manifest end, the
//     request's tail replaces the stored one unless the stored tail is at
//     the same offset and longer (a late retry never shrinks a tail).
//     A tail delta (From > 0) applies only on a stored tail at the same
//     offset holding at least From bytes: the new tail is the stored tail's
//     first From bytes plus the payload's tail bytes, and must hash to
//     Tail.Hash; a mismatch means the device's history differs, answered
//     StatusNewGeneration. When message redaction changed that prefix,
//     the server retains its masked tail and answers StatusPartial with
//     TailOffset=-1/TailSize=0 to request a full tail in the same generation.
//     Without such a base the tail is not stored and
//     TailOffset/TailSize tell the device what to resend from.
//     Tail == nil otherwise leaves the stored tail alone: a device that lost
//     a tail's bytes (gap) never deletes the server's last copy. Tails never
//     go to object storage.
//  5. Answer StatusOK when steps 3 and 4 applied everything, else
//     StatusPartial. AckedEntries and AckedOffset always describe the
//     committed manifest after this request.
//
// Every step is idempotent: re-sending a request, or an older request after
// a newer one, changes nothing already committed. Chunks are content
// addressed; a body for a hash the server already stores is dropped.
package syncproto

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/zeebo/blake3"
)

const (
	Version = 1

	PathHas   = "/v1/sync/has"
	PathFlush = "/v1/sync/flush"

	HeaderVersion    = "X-Flopwire-Sync-Version"
	FlushContentType = "application/vnd.flopwire.flush.v1"

	// MaxHeaderBytes bounds the JSON header of a flush frame.
	MaxHeaderBytes = 8 << 20
	// MaxHasHashes bounds one HasRequest.
	MaxHasHashes = 4096
	// MaxPartBytes bounds one announced body or tail. Chunks are cut at
	// most 4MB (devicesync.DefaultChunkParams) and a tail is shorter than
	// a chunk; the bound leaves room for other chunker settings while
	// keeping a hostile announcement from allocating unbounded memory.
	MaxPartBytes = 16 << 20
	// MaxPayloadBytes bounds the payload one frame may announce.
	MaxPayloadBytes = 72 << 20
)

// frameMagic starts every flush body; the last byte is the version.
var frameMagic = [4]byte{'T', 'M', 'F', Version}

// Hash is a BLAKE3-256 content hash. JSON form: lowercase hex.
type Hash [32]byte

// Sum hashes b.
func Sum(b []byte) Hash { return Hash(blake3.Sum256(b)) }

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

func (h *Hash) UnmarshalText(b []byte) error {
	if hex.DecodedLen(len(b)) != len(h) {
		return fmt.Errorf("syncproto: hash must be %d hex chars", 2*len(h))
	}
	_, err := hex.Decode(h[:], b)
	return err
}

// SourceRef names a source by its device-local identity.
type SourceRef struct {
	Path   string `json:"path"`
	FileID string `json:"file_id"`
}

// Source describes the source a flush belongs to (sources row, §4.1).
type Source struct {
	Path        string `json:"path"`
	FileID      string `json:"file_id"` // transcript.FileID.String(); "" for exports
	Agent       string `json:"agent"`
	StorageKind string `json:"storage_kind"`
	SessionKey  string `json:"session_key,omitempty"`
	Parser      string `json:"parser,omitempty"`
	// Parent links a companion file (Claude tool-results/*, agent-*.meta.json)
	// to the source it belongs to. Companions are archived, not parsed as
	// transcripts.
	Parent *SourceRef `json:"parent,omitempty"`
	// Previous names the source this one replaces at the same path (new
	// file identity). The server marks the previous source's rows superseded.
	Previous *SourceRef `json:"previous,omitempty"`
	// Checkout and Remote are the repository the session ran in, as the
	// device placed it (D18): its main checkout (the bare repository for
	// a bare-backed layout) and its normalized remote (host/owner/name,
	// origin else the first). Either is "" when unknown. Every flush
	// reports the device's current placement, so a later one (the
	// deleted-worktree recovery pass) replaces an earlier one; a source
	// whose bytes are all acknowledged is sent again header only.
	// Retrieval and the bus match a repository across devices by Remote
	// (issue #102).
	Checkout string `json:"checkout,omitempty"`
	Remote   string `json:"remote,omitempty"`
}

// MaxRepoField bounds Source.Checkout and Source.Remote.
const MaxRepoField = 4096

// ChunkerParams records how the device cut chunks. The server never
// re-chunks; the parameters are recorded for diagnosis.
type ChunkerParams struct {
	Algorithm string `json:"algorithm"` // "fastcdc-v1.0.0"
	Min       int    `json:"min"`
	Avg       int    `json:"avg"`
	Max       int    `json:"max"`
}

// Entry is one finalized manifest entry (manifest_entries row).
type Entry struct {
	Ordinal int64 `json:"ordinal"`
	Hash    Hash  `json:"hash"`
	Offset  int64 `json:"offset"`
	Size    int64 `json:"size"`
}

// End is the byte offset just past the entry.
func (e Entry) End() int64 { return e.Offset + e.Size }

// Body announces a chunk carried in the frame payload, in order, as one
// zstd frame of ZSize bytes (EncodeBody). Hash and Size describe the
// uncompressed chunk: the content address does not depend on the encoder.
type Body struct {
	Hash  Hash  `json:"hash"`
	Size  int64 `json:"size"`
	ZSize int64 `json:"zsize"`
}

// Tail is the provisional tail: the bytes past the last finalized boundary
// (provisional_tails row). Offset, Size and Hash describe the whole tail.
// The payload carries only its bytes [From, Size) after the bodies: a
// device that knows the server holds the first From bytes of this tail
// (TailOffset/TailSize of an earlier answer) sends just what was appended
// since. Replay of a busy Codex rollout: whole tails cost a median 364KB
// per flush and 22.7x the file size; deltas cut that to the appended bytes.
type Tail struct {
	Offset int64 `json:"offset"`
	Size   int64 `json:"size"`
	Hash   Hash  `json:"hash"`
	From   int64 `json:"from,omitempty"`
}

// Apply builds the whole tail from the stored tail's bytes and the payload
// delta, and verifies its hash. stored may be nil when From is 0.
func (t *Tail) Apply(stored, delta []byte) ([]byte, error) {
	if int64(len(stored)) < t.From || int64(len(delta)) != t.Size-t.From {
		return nil, errors.New("syncproto: tail base missing")
	}
	whole := append(append(make([]byte, 0, t.Size), stored[:t.From]...), delta...)
	if Sum(whole) != t.Hash {
		return nil, ErrTailMismatch
	}
	return whole, nil
}

// ErrTailMismatch: the stored tail plus the delta does not hash to the
// announced tail; the device's bytes differ from what the server holds.
var ErrTailMismatch = errors.New("syncproto: tail hash mismatch")

// FlushHeader is the JSON header of a flush frame.
type FlushHeader struct {
	Version    int           `json:"version"`
	Source     Source        `json:"source"`
	Generation int64         `json:"generation"`
	ChangeTime time.Time     `json:"change_time,omitzero"` // inode ctime at capture
	CapturedAt time.Time     `json:"captured_at"`
	Chunker    ChunkerParams `json:"chunker"`
	Entries    []Entry       `json:"entries,omitempty"` // manifest delta
	Bodies     []Body        `json:"bodies,omitempty"`
	Tail       *Tail         `json:"tail,omitempty"`
	// Redaction reports how the device redacted the generation's bytes
	// before chunking (notes/redaction.md): the rule set, and the secrets
	// masked so far in the whole generation, per rule. Nil: the device did
	// not redact.
	Redaction *Redaction `json:"redaction,omitempty"`
	// Device is what the device reports of its own file system; the
	// server expands "~" in admin path rules with its Home (D18).
	Device *DeviceDirs `json:"device,omitempty"`
	// Live lists the session ids the device's harnesses hold open (at
	// most MaxLive), so retrieval marks them live exactly; null reports
	// nothing, [] reports none.
	Live []string `json:"live"`
}

// MaxLive bounds the live sessions one flush reports.
const MaxLive = 32

// Redaction is a generation's redaction record. Counts only: never a
// matched value.
type Redaction struct {
	Rules  string           `json:"rules"`
	Counts map[string]int64 `json:"counts,omitempty"`
}

// DeviceDirs are a device's home directory and harness directories, as
// the device agent resolved them.
type DeviceDirs struct {
	Home           string `json:"home,omitempty"`
	ClaudeProjects string `json:"claude_projects,omitempty"` // CLAUDE_CONFIG_DIR/projects or ~/.claude/projects
	CodexHome      string `json:"codex_home,omitempty"`      // CODEX_HOME or ~/.codex
}

// FlushRequest is a header plus the payload bytes it announces.
type FlushRequest struct {
	Header FlushHeader
	// Payload yields exactly sum(Bodies.Size)+Tail.Size bytes: every body in
	// order, then the tail. Streaming keeps device memory bounded.
	Payload io.Reader
}

// FlushStatus is the outcome of a flush.
type FlushStatus string

const (
	StatusOK              FlushStatus = "ok"
	StatusPartial         FlushStatus = "partial"
	StatusNewGeneration   FlushStatus = "new_generation_required"
	StatusStaleGeneration FlushStatus = "stale_generation"
)

// FlushResponse is the JSON answer to a flush.
type FlushResponse struct {
	Version      int         `json:"version"`
	Status       FlushStatus `json:"status"`
	Generation   int64       `json:"generation"`    // the server's latest generation of the source
	AckedEntries int64       `json:"acked_entries"` // committed manifest length of the request's generation
	AckedOffset  int64       `json:"acked_offset"`  // end of the committed manifest: the finalized watermark
	TailAcked    bool        `json:"tail_acked"`    // every entry applied and the request's tail (if any) is stored
	TailOffset   int64       `json:"tail_offset"`   // the stored provisional tail, or -1 to request a full resend
	TailSize     int64       `json:"tail_size"`     // 0: no reusable prefix (a masked tail may still be stored)
	Missing      []Hash      `json:"missing,omitempty"`
	// Refused names the admin path rule under which the server refused
	// the source (D18): its bytes are acknowledged and discarded.
	Refused string `json:"refused,omitempty"`
}

// HasRequest asks which chunks the server lacks.
type HasRequest struct {
	Version int    `json:"version"`
	Hashes  []Hash `json:"hashes"`
}

// HasResponse lists the requested hashes the device's user does not hold
// (the server may store them for another user; send the body anyway).
type HasResponse struct {
	Version int    `json:"version"`
	Missing []Hash `json:"missing"`
}

// ErrorResponse is the JSON body of a 4xx/5xx answer.
type ErrorResponse struct {
	Code    string `json:"code"` // bad_request, unsupported_version, unauthorized, ...
	Message string `json:"message"`
}

// PayloadSize is the number of payload bytes the header announces.
func (h *FlushHeader) PayloadSize() int64 {
	var n int64
	for _, b := range h.Bodies {
		n += b.ZSize
	}
	if h.Tail != nil {
		n += h.Tail.Size - h.Tail.From
	}
	return n
}

// Validate checks the header's internal consistency (step 1 minus hashing).
func (h *FlushHeader) Validate() error {
	if r := h.Redaction; r != nil {
		if len(r.Rules) > 64 || len(r.Counts) > 256 {
			return errors.New("redaction record too large")
		}
		for k, n := range r.Counts {
			if len(k) > 64 || n < 0 {
				return errors.New("bad redaction count")
			}
		}
	}
	if h.Version != Version {
		return fmt.Errorf("unsupported version %d", h.Version)
	}
	if h.Source.Path == "" || h.Source.Agent == "" || h.Source.StorageKind == "" {
		return errors.New("source path, agent and storage_kind are required")
	}
	if len(h.Source.Checkout) > MaxRepoField || len(h.Source.Remote) > MaxRepoField {
		return errors.New("source checkout or remote too long")
	}
	if c := h.Source.Checkout; c != "" && c[0] != '/' {
		return errors.New("source checkout is not an absolute path")
	}
	if h.Generation < 0 {
		return errors.New("negative generation")
	}
	for i, e := range h.Entries {
		if e.Size <= 0 || e.Offset < 0 || e.Ordinal < 0 || e.Size > MaxPartBytes || e.Offset > math.MaxInt64-e.Size {
			return fmt.Errorf("entry %d: bad ordinal, offset or size", i)
		}
		if i > 0 {
			p := h.Entries[i-1]
			if e.Ordinal != p.Ordinal+1 || e.Offset != p.End() {
				return fmt.Errorf("entry %d: not contiguous with entry %d", i, i-1)
			}
		}
	}
	var payload int64
	for i, b := range h.Bodies {
		if b.Size <= 0 || b.Size > MaxPartBytes || b.ZSize <= 0 || b.ZSize > MaxPartBytes+maxFrameOverhead {
			return fmt.Errorf("body %d: bad size", i)
		}
		if payload += b.ZSize; payload > MaxPayloadBytes {
			return errors.New("payload too large")
		}
	}
	if t := h.Tail; t != nil {
		if t.Size <= 0 || t.Offset < 0 || t.From < 0 || t.From >= t.Size || t.Size > MaxPartBytes || t.Offset > math.MaxInt64-t.Size {
			return errors.New("tail: bad offset, size or from")
		}
		if payload += t.Size - t.From; payload > MaxPayloadBytes {
			return errors.New("payload too large")
		}
		if n := len(h.Entries); n > 0 && t.Offset != h.Entries[n-1].End() {
			return errors.New("tail does not start at the end of the entries")
		}
	}
	return nil
}

// EncodeFlush writes the frame: 4-byte magic "TMF\x01", a big-endian uint32
// header length, the JSON header, then the payload.
func EncodeFlush(w io.Writer, req *FlushRequest) error {
	hdr, err := marshalJSON(&req.Header)
	if err != nil {
		return err
	}
	var pre [8]byte
	copy(pre[:4], frameMagic[:])
	binary.BigEndian.PutUint32(pre[4:], uint32(len(hdr)))
	if _, err := w.Write(pre[:]); err != nil {
		return err
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	want := req.Header.PayloadSize()
	if want == 0 {
		return nil
	}
	if req.Payload == nil {
		return errors.New("syncproto: payload missing")
	}
	n, err := io.Copy(w, io.LimitReader(req.Payload, want))
	if err == nil && n != want {
		err = fmt.Errorf("syncproto: payload short: %d of %d bytes", n, want)
	}
	return err
}

// DecodeFlush reads a frame header and validates it. The returned reader
// yields the payload; use NextBody and ReadTail to consume it with hash
// checks.
func DecodeFlush(r io.Reader) (*FlushHeader, *PayloadReader, error) {
	var pre [8]byte
	if _, err := io.ReadFull(r, pre[:]); err != nil {
		return nil, nil, fmt.Errorf("syncproto: frame prefix: %w", err)
	}
	if !bytes.Equal(pre[:3], frameMagic[:3]) {
		return nil, nil, errors.New("syncproto: bad frame magic")
	}
	if pre[3] != Version {
		return nil, nil, fmt.Errorf("unsupported version %d", pre[3])
	}
	n := binary.BigEndian.Uint32(pre[4:])
	if n > MaxHeaderBytes {
		return nil, nil, errors.New("syncproto: header too large")
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, nil, fmt.Errorf("syncproto: header: %w", err)
	}
	var h FlushHeader
	if err := unmarshalJSON(raw, &h); err != nil {
		return nil, nil, fmt.Errorf("syncproto: header: %w", err)
	}
	if err := h.Validate(); err != nil {
		return nil, nil, err
	}
	return &h, &PayloadReader{r: r, h: &h}, nil
}

// PayloadReader walks a flush payload, verifying each part's size and hash.
type PayloadReader struct {
	r    io.Reader
	h    *FlushHeader
	next int
}

// NextBody reads the next announced body: its compressed frame into zbuf
// and the decoded chunk into buf (both grown as needed), verified against
// the body's Size and Hash. It returns io.EOF after the last body.
func (p *PayloadReader) NextBody(zbuf, buf []byte) (b Body, data, z []byte, err error) {
	if p.next >= len(p.h.Bodies) {
		return Body{}, nil, nil, io.EOF
	}
	b = p.h.Bodies[p.next]
	p.next++
	if z, err = p.read(zbuf, b.ZSize, Hash{}); err != nil {
		return b, nil, nil, err
	}
	if data, err = Decompress(buf, z, b.Size, b.Hash); err != nil {
		return b, nil, nil, fmt.Errorf("syncproto: payload part %s: %w", b.Hash, err)
	}
	return b, data, z, nil
}

// ReadTail reads the tail bytes after all bodies: the whole tail, verified,
// when Tail.From is 0; otherwise the delta [From, Size), which the server
// verifies with Tail.Apply against its stored tail. It returns nil when the
// header has no tail, and fails if trailing bytes remain.
func (p *PayloadReader) ReadTail(buf []byte) ([]byte, error) {
	if p.next != len(p.h.Bodies) {
		return nil, errors.New("syncproto: bodies not fully read")
	}
	var data []byte
	if t := p.h.Tail; t != nil {
		var err error
		if t.From == 0 {
			data, err = p.read(buf, t.Size, t.Hash)
		} else {
			data, err = p.read(buf, t.Size-t.From, Hash{})
		}
		if err != nil {
			return nil, err
		}
	}
	var one [1]byte
	if n, _ := io.ReadFull(p.r, one[:]); n != 0 {
		return nil, errors.New("syncproto: trailing payload bytes")
	}
	return data, nil
}

func (p *PayloadReader) read(buf []byte, size int64, want Hash) ([]byte, error) {
	if int64(cap(buf)) < size {
		buf = make([]byte, size)
	}
	buf = buf[:size]
	if _, err := io.ReadFull(p.r, buf); err != nil {
		return nil, fmt.Errorf("syncproto: payload: %w", err)
	}
	if want != (Hash{}) && Sum(buf) != want {
		return nil, fmt.Errorf("syncproto: payload part %s: hash mismatch", want)
	}
	return buf, nil
}
