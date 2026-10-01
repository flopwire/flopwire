package retrieval_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/retrieval"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/syncproto"
)

// payloadGate stops the server after it has read redirects, either before
// the body or on the final EOF read immediately before manifest commit.
type payloadGate struct {
	io.Reader
	reached, release chan struct{}
	once             bool
}

func (r *payloadGate) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		close(r.reached)
		<-r.release
	}
	return r.Reader.Read(p)
}

func TestRedactionRacingFlush(t *testing.T) {
	for _, name := range []string{"before-body", "before-commit", "after-body", "existing-manifest", "redirect-chain"} {
		body := name == "before-body" || name == "after-body" || name == "redirect-chain"
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s := newServer(t)
			specs, dv, export := s.writeRedactFixtures()
			s.syncRedact(s.sy, specs, dv, export)
			var id, device, key, originalPath, originalFile string
			var generation int64
			var hash []byte
			var size int64
			err := s.pool.QueryRow(ctx, `SELECT m.id::text,c.device_id::text,ch.hash,ch.size,ch.object_key,src.path,src.file_id,m.source_generation FROM messages m JOIN conversations c ON c.id=m.conversation_id JOIN sources src ON src.id=m.source_id JOIN manifest_entries e ON e.source_id=m.source_id AND e.generation=m.source_generation AND e.ordinal=0 JOIN chunks ch ON ch.hash=e.chunk_hash WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&id, &device, &hash, &size, &key, &originalPath, &originalFile, &generation)
			if err != nil {
				t.Fatal(err)
			}
			data, err := ingest.GetChunk(ctx, s.objects, ingest.Chunk{Hash: syncproto.Hash(hash), Size: size, Key: key})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, []byte("BLUEFALCON")) {
				t.Fatal("fixture secret not in first chunk")
			}
			h := syncproto.FlushHeader{Version: syncproto.Version, CapturedAt: time.Now().UTC(), Source: syncproto.Source{Path: "/race.jsonl", FileID: "1:1", Agent: "claude", StorageKind: "jsonl_append", Parser: specs[0].Parser}, Chunker: syncproto.ChunkerParams{Algorithm: devicesync.Algorithm, Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}, Entries: []syncproto.Entry{{Ordinal: 0, Hash: syncproto.Hash(hash), Size: size}}}
			if name == "existing-manifest" {
				h.Source.Path = originalPath
				h.Source.FileID = originalFile
				h.Generation = generation
			}
			var z []byte
			if body {
				b, encoded := syncproto.EncodeBody(data)
				h.Bodies = []syncproto.Body{b}
				z = encoded
			}
			var wire bytes.Buffer
			if err := syncproto.EncodeFlush(&wire, &syncproto.FlushRequest{Header: h, Payload: bytes.NewReader(z)}); err != nil {
				t.Fatal(err)
			}
			frame := wire.Bytes()
			if name == "redirect-chain" {
				if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: id + ":1-1", AllCopies: true}); err != nil {
					t.Fatal(err)
				}
			}
			gateOffset, gateData := len(frame)-len(z), z
			if name == "after-body" {
				gateOffset, gateData = len(frame), nil
			}
			gate := &payloadGate{Reader: bytes.NewReader(gateData), reached: make(chan struct{}), release: make(chan struct{})}
			released := false
			defer func() {
				if !released {
					close(gate.release)
				}
			}()
			header, pr, err := syncproto.DecodeFlush(io.MultiReader(bytes.NewReader(frame[:gateOffset]), gate))
			if err != nil {
				t.Fatal(err)
			}
			uploader := &ingest.Server{Pool: s.pool, Objects: s.objects}
			var first *syncproto.FlushResponse
			done := make(chan error, 1)
			go func() {
				var err error
				first, err = uploader.Flush(ctx, device, header, pr)
				done <- err
			}()
			select {
			case <-gate.reached:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: id, AllCopies: true}); err != nil {
				t.Fatal(err)
			}
			close(gate.release)
			released = true
			select {
			case err = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err == nil && first.Status != syncproto.StatusOK {
				t.Fatalf("racing request forced generation change: %+v", first)
			}
			if err != nil || len(first.Missing) > 0 || first.AckedEntries == 0 {
				if err != nil {
					var retry *ingest.Error
					if !errors.As(err, &retry) || retry.Status != http.StatusServiceUnavailable {
						t.Fatal(err)
					}
				}
				// Retrying reads the newly committed redirect and still requires the
				// verified body (or this user's existing possession) for its target.
				header, pr, err := syncproto.DecodeFlush(bytes.NewReader(frame))
				if err != nil {
					t.Fatal(err)
				}
				resp, err := uploader.Flush(ctx, device, header, pr)
				if err != nil || resp.Status != syncproto.StatusOK || resp.AckedEntries < 1 {
					t.Fatalf("retry: %+v %v", resp, err)
				}
			}
			if s.archiveHas("BLUEFALCON") {
				t.Fatal("racing flush restored redacted bytes to archive")
			}
			if s.count(`SELECT count(*) FROM manifest_entries WHERE chunk_hash=$1`, hash) != 0 {
				t.Fatal("old chunk referenced again")
			}
			if s.count(`SELECT count(*) FROM chunks WHERE hash=$1 AND state='deletion_pending' AND deletion_job_id IS NOT NULL`, hash) != 1 {
				t.Fatal("upload took chunk away from redaction purge job")
			}
		})
	}
}

// A copy parsed after a redaction picked its targets, and committed before
// the redaction commits, is masked too: the redaction re-reads its targets
// under the lock that parse writes share.
func TestRedactionMasksCopyParsedMeanwhile(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	var addr string
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	// Another Codex session holding the same message.
	b, err := os.ReadFile(specs[1].Path)
	if err != nil {
		t.Fatal(err)
	}
	copySpec := specs[1]
	copySpec.Path = strings.ReplaceAll(specs[1].Path, "0000000000c1", "0000000000c2")
	fired := 0
	retrieval.SetBeforeRedactTx(func() {
		fired++
		if fired > 1 {
			return
		}
		if err := os.WriteFile(copySpec.Path, bytes.ReplaceAll(b, []byte("0000000000c1"), []byte("0000000000c2")), 0o600); err != nil {
			t.Error(err)
			return
		}
		s.syncRedact(s.sy, []devicesync.SourceSpec{copySpec}, dv, export)
	})
	defer retrieval.SetBeforeRedactTx(nil)
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: addr + ":2-2", AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	if fired == 0 {
		t.Fatal("hook did not run")
	}
	if n := s.count(`SELECT count(*) FROM messages m JOIN sources src ON src.id=m.source_id WHERE src.path=$1`, copySpec.Path); n == 0 {
		t.Fatal("the copy was not parsed")
	}
	if n := s.rowsWith("BLUEFALCON"); n != 0 {
		t.Fatalf("%d rows hold the codename: a copy committed during the redaction kept it", n)
	}
}
