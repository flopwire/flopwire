package ingest

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/minio/minio-go/v7"
)

// Objects is the chunk store: S3 in production. Objects are immutable and
// content addressed (by the uncompressed chunk), so a put of an existing
// key rewrites bytes that decode to the same chunk. Read chunks with
// GetChunk.
type Objects interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

// ChunkKey is the object key of a finalized chunk.
func ChunkKey(h syncproto.Hash) string {
	s := hex.EncodeToString(h[:])
	return "chunks/" + s[:2] + "/" + s
}

// GetChunk fetches a chunk's object and decodes it (objects are zstd
// frames of the chunk, syncproto codec.go), checking it against the
// chunk's size and content address.
func GetChunk(ctx context.Context, objects Objects, c Chunk) ([]byte, error) {
	z, err := objects.Get(ctx, c.Key)
	if err != nil {
		return nil, fmt.Errorf("ingest: fetch chunk %s: %w", c.Hash, err)
	}
	data, err := syncproto.Decompress(nil, z, c.Size, c.Hash)
	if err != nil {
		return nil, fmt.Errorf("ingest: chunk %s: stored object does not match its hash: %w", c.Hash, err)
	}
	return data, nil
}

// MinIO adapts a minio client and bucket to Objects.
type MinIO struct {
	Client *minio.Client
	Bucket string
}

func (m MinIO) Put(ctx context.Context, key string, data []byte) error {
	_, err := m.Client.PutObject(ctx, m.Bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream", DisableMultipart: true})
	return err
}

func (m MinIO) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := m.Client.GetObject(ctx, m.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}
