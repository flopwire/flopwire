package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Generation is one generation's layout: finalized chunks in order, then
// the provisional tail, if any.
//
// A generation loaded from an offset (LoadGenerationFrom) holds only the
// entries from the one that covers that offset on; a Reader loads earlier
// entries when a read reaches them (Generation.extend), so Entries is
// always a contiguous run that ends with the manifest's last entry.
type Generation struct {
	SourceID   string
	Generation int64
	Entries    []Chunk
	TailOffset int64
	Tail       []byte

	// pool loads the entries before Entries[0] (first > 0); nil when the
	// whole manifest is loaded.
	pool  *pgxpool.Pool
	first int64 // ordinal of Entries[0]
}

// Chunk is one manifest entry with its object key.
type Chunk struct {
	Hash   syncproto.Hash
	Offset int64
	Size   int64
	Key    string
}

// Size is the reconstructible length: the manifest plus the tail.
func (g *Generation) Size() int64 {
	if g.Tail != nil {
		return g.TailOffset + int64(len(g.Tail))
	}
	if n := len(g.Entries); n > 0 {
		return g.Entries[n-1].Offset + g.Entries[n-1].Size
	}
	return 0
}

// ErrNoGeneration: the source has no such generation.
var ErrNoGeneration = errors.New("ingest: no such generation")

// entriesSQL lists a generation's manifest entries with ordinals in
// [$3's entry, $4): the first is the last entry that starts at or before
// byte $3, so the run covers $3 on (all of them for $3 = 0). The lookup
// walks the primary key back from $4, past only the entries it returns;
// each entry's chunk is looked up by its key, never by a join that could
// read all of chunks.
const entriesSQL = `SELECT m.ordinal,m.chunk_hash,m.byte_offset,c.size,c.object_key FROM manifest_entries m
	CROSS JOIN LATERAL (SELECT size,object_key FROM chunks WHERE hash=m.chunk_hash) c
	WHERE m.source_id=$1 AND m.generation=$2 AND m.ordinal<$4 AND m.ordinal>=CASE WHEN $3::bigint<=0 THEN 0 ELSE COALESCE((SELECT ordinal FROM manifest_entries
		WHERE source_id=$1 AND generation=$2 AND ordinal<$4 AND byte_offset<=$3::bigint ORDER BY ordinal DESC LIMIT 1),0) END
	ORDER BY m.ordinal`

// LoadGeneration reads a generation's manifest and tail in one snapshot.
// gen < 0 selects the latest generation. When ctx has a deadline, its
// statements stop there in Postgres too (statement_timeout).
func LoadGeneration(ctx context.Context, pool *pgxpool.Pool, sourceID string, gen int64) (*Generation, error) {
	return LoadGenerationFrom(ctx, pool, sourceID, gen, 0)
}

// LoadGenerationFrom is LoadGeneration that reads only the manifest
// entries from the one covering byte from on: an append is parsed from
// its cursor, and the manifest before it can be as long as the session.
// A Reader of the generation loads earlier entries if a read reaches
// them.
func LoadGenerationFrom(ctx context.Context, pool *pgxpool.Pool, sourceID string, gen, from int64) (*Generation, error) {
	g := &Generation{SourceID: sourceID, TailOffset: -1}
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if dl, ok := ctx.Deadline(); ok {
			ms := max(time.Until(dl).Milliseconds(), 1)
			if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout',$1,true)`, strconv.FormatInt(ms, 10)); err != nil {
				return err
			}
		}
		err := tx.QueryRow(ctx, `SELECT generation FROM generations WHERE source_id=$1 AND ($2<0 OR generation=$2) ORDER BY generation DESC LIMIT 1`, sourceID, gen).Scan(&g.Generation)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoGeneration
		}
		if err != nil {
			return err
		}
		var first int64
		if from > 0 {
			g.pool = pool
		}
		g.Entries, first, err = loadEntries(ctx, tx, sourceID, g.Generation, max(from, 0), math.MaxInt32)
		if err != nil {
			return err
		}
		g.first = first
		err = tx.QueryRow(ctx, `SELECT byte_offset,bytes FROM provisional_tails WHERE source_id=$1 AND generation=$2`, sourceID, g.Generation).Scan(&g.TailOffset, &g.Tail)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if g.first == 0 {
		g.pool = nil
	}
	return g, err
}

// loadEntries reads the entries of entriesSQL and the first one's ordinal.
func loadEntries(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, sourceID string, gen, from, before int64) ([]Chunk, int64, error) {
	rows, err := q.Query(ctx, entriesSQL, sourceID, gen, from, before)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Chunk
	first := int64(-1)
	for rows.Next() {
		var c Chunk
		var h []byte
		var ord int64
		if err := rows.Scan(&ord, &h, &c.Offset, &c.Size, &c.Key); err != nil {
			return nil, 0, err
		}
		if first < 0 {
			first = ord
		}
		c.Hash = syncproto.Hash(h)
		out = append(out, c)
	}
	return out, max(first, 0), rows.Err()
}

// extend loads the entries before Entries[0] down to the one covering
// byte pos. It reports false when there are none to load. The entries
// before a loaded run do not change within a generation (a redaction
// rewrites a chunk in place, at the same size), so they are read outside
// the load's snapshot.
func (g *Generation) extend(ctx context.Context, pos int64) (bool, error) {
	if g.pool == nil || g.first == 0 {
		return false, nil
	}
	more, first, err := loadEntries(ctx, g.pool, g.SourceID, g.Generation, pos, g.first)
	if err != nil || len(more) == 0 {
		return false, err
	}
	if n := len(more); first+int64(n) != g.first || len(g.Entries) > 0 && more[n-1].Offset+more[n-1].Size != g.Entries[0].Offset {
		return false, fmt.Errorf("ingest: manifest of %s generation %d changed while it was read", g.SourceID, g.Generation)
	}
	g.Entries, g.first = append(more, g.Entries...), first
	return true, nil
}

// Reader reads a generation by manifest arithmetic, fetching the chunk
// objects it touches and keeping the last few in memory. Reconstruction is
// transient: nothing is written anywhere.
type Reader struct {
	ctx     context.Context
	objects Objects
	g       *Generation
	size    int64

	mu    sync.Mutex
	cache []cached // most recent last
	// Fetched counts object fetches (for tests and metrics).
	Fetched int
}

type cached struct {
	key  string
	data []byte
}

const readerCache = 4

// NewReader returns an io.ReaderAt over g.
func NewReader(ctx context.Context, objects Objects, g *Generation) *Reader {
	return &Reader{ctx: ctx, objects: objects, g: g, size: g.Size()}
}

func (r *Reader) Size() int64  { return r.size }
func (r *Reader) Close() error { return nil }

func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("ingest: negative offset")
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}
		if r.g.Tail != nil && pos >= r.g.TailOffset {
			n += copy(p[n:], r.g.Tail[pos-r.g.TailOffset:])
			continue
		}
		c, err := r.entry(pos)
		if err != nil {
			return n, err
		}
		data, err := r.chunk(c)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], data[pos-c.Offset:])
	}
	return n, nil
}

// entry is the manifest entry that covers pos, loading earlier entries
// when pos is before the loaded run.
func (r *Reader) entry(pos int64) (Chunk, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		es := r.g.Entries
		if len(es) == 0 || pos < es[0].Offset {
			ok, err := r.g.extend(r.ctx, pos)
			if err != nil {
				return Chunk{}, err
			}
			if ok {
				continue
			}
		}
		i := sort.Search(len(es), func(i int) bool { return es[i].Offset+es[i].Size > pos })
		if i == len(es) || es[i].Offset > pos {
			return Chunk{}, fmt.Errorf("ingest: offset %d not covered by the manifest", pos)
		}
		return es[i], nil
	}
}

func (r *Reader) chunk(c Chunk) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.cache {
		if e.key == c.Key {
			r.cache = append(append(r.cache[:i:i], r.cache[i+1:]...), e)
			return e.data, nil
		}
	}
	data, err := GetChunk(r.ctx, r.objects, c)
	if err != nil {
		return nil, err
	}
	r.Fetched++
	if len(r.cache) == readerCache {
		r.cache = r.cache[1:]
	}
	r.cache = append(r.cache, cached{c.Key, data})
	return data, nil
}
