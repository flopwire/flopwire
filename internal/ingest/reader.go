package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
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
type Generation struct {
	SourceID   string
	Generation int64
	Entries    []Chunk
	TailOffset int64
	Tail       []byte
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

// LoadGeneration reads a generation's manifest and tail in one snapshot.
// gen < 0 selects the latest generation. When ctx has a deadline, its
// statements stop there in Postgres too (statement_timeout).
func LoadGeneration(ctx context.Context, pool *pgxpool.Pool, sourceID string, gen int64) (*Generation, error) {
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
		rows, err := tx.Query(ctx, `SELECT m.chunk_hash,m.byte_offset,c.size,c.object_key FROM manifest_entries m JOIN chunks c ON c.hash=m.chunk_hash
			WHERE m.source_id=$1 AND m.generation=$2 ORDER BY m.ordinal`, sourceID, g.Generation)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c Chunk
			var h []byte
			if err := rows.Scan(&h, &c.Offset, &c.Size, &c.Key); err != nil {
				rows.Close()
				return err
			}
			c.Hash = syncproto.Hash(h)
			g.Entries = append(g.Entries, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT byte_offset,bytes FROM provisional_tails WHERE source_id=$1 AND generation=$2`, sourceID, g.Generation).Scan(&g.TailOffset, &g.Tail)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return g, err
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
		es := r.g.Entries
		i := sort.Search(len(es), func(i int) bool { return es[i].Offset+es[i].Size > pos })
		if i == len(es) || es[i].Offset > pos {
			return n, fmt.Errorf("ingest: offset %d not covered by the manifest", pos)
		}
		data, err := r.chunk(es[i])
		if err != nil {
			return n, err
		}
		n += copy(p[n:], data[pos-es[i].Offset:])
	}
	return n, nil
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
