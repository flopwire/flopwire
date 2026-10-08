package devicesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

func reconcileVersionPath(spool *Spool, sid, gen int64, hash syncproto.Hash) string {
	return filepath.Join(spool.dir, "tails", fmt.Sprintf("%d-%d-%s", sid, gen, hash.String()))
}

func assertReconcileFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("retained file %s: got %q, error %v", filepath.Base(path), got, err)
	}
}

func TestRequiredTailExactCommittedReference(t *testing.T) {
	f := newTailPreparationFixture(t)
	body := []byte("committed tail identity\n")
	sid, hash := f.add(t, transcript.StorageJSONDoc, false, body, nil, false)
	got, needed, err := f.store.requiredTail(t.Context(), sid, 0)
	if err != nil || !needed || got.Hash != hash || got.Size != int64(len(body)) || got.Offset != 0 {
		t.Fatalf("reference: %+v needed=%v err=%v", got, needed, err)
	}
	if _, needed, err := f.store.requiredTail(t.Context(), sid, 1); err != nil || needed {
		t.Fatalf("absent generation: needed=%v err=%v", needed, err)
	}
	if _, err := f.store.db.Exec(`UPDATE devsync_gens SET tail_acked=1 WHERE source_id=?`, sid); err != nil {
		t.Fatal(err)
	}
	if _, needed, err := f.store.requiredTail(t.Context(), sid, 0); err != nil || needed {
		t.Fatalf("acked ordinary tail: needed=%v err=%v", needed, err)
	}
	// Only a current exporter continuation retains an acknowledged tail.
	if _, err := f.store.db.Exec(`UPDATE devsync_sources SET watermark='{"Offset":24,"Export":"cmVzdW1l"}' WHERE id=?`, sid); err != nil {
		t.Fatal(err)
	}
	if got, needed, err := f.store.requiredTail(t.Context(), sid, 0); err != nil || !needed || got.Hash != hash {
		t.Fatalf("current continuation: %+v needed=%v err=%v", got, needed, err)
	}
	if _, err := f.store.db.Exec(`UPDATE devsync_sources SET generation=1 WHERE id=?`, sid); err != nil {
		t.Fatal(err)
	}
	if _, needed, err := f.store.requiredTail(t.Context(), sid, 0); err != nil || needed {
		t.Fatalf("noncurrent continuation: needed=%v err=%v", needed, err)
	}
}

func TestRequiredTailUncertainMetadata(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"missing source", `DELETE FROM devsync_sources`},
		{"missing current generation", `DELETE FROM devsync_gens`},
		{"malformed watermark", `UPDATE devsync_sources SET watermark='{'`},
		{"short hash", `UPDATE devsync_gens SET tail_hash=x'01'`},
		{"negative offset", `UPDATE devsync_gens SET tail_offset=-1`},
		{"inconsistent range", `UPDATE devsync_gens SET tail_offset=1`},
		{"tail exceeds capture", `UPDATE devsync_gens SET size=1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTailPreparationFixture(t)
			sid, _ := f.add(t, transcript.StorageJSONDoc, false, []byte("durable body\n"), nil, false)
			if _, err := f.store.db.Exec(tc.change); err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.store.requiredTail(t.Context(), sid, 0); err == nil {
				t.Fatal("uncertain committed metadata permitted a cleanup decision")
			}
		})
	}
}

func TestTailReconcileAndSweepHoldAllOnDecisionFailure(t *testing.T) {
	for _, sweep := range []bool{false, true} {
		for _, failure := range []string{"lookup error", "canceled lookup", "missing source"} {
			t.Run(fmt.Sprintf("sweep=%v/%s", sweep, failure), func(t *testing.T) {
				f := newTailPreparationFixture(t)
				old, stale := []byte("old unreferenced\n"), []byte("other unreferenced\n")
				sid, _ := f.add(t, transcript.StorageJSONDoc, false, []byte("required not obsolete\n"), nil, false)
				oldHash, staleHash := syncproto.Sum(old), syncproto.Sum(stale)
				if err := f.spool.PutTailVersion(sid, 0, oldHash, old); err != nil {
					t.Fatal(err)
				}
				if err := f.spool.PutTailVersion(sid, 1, staleHash, stale); err != nil {
					t.Fatal(err)
				}
				before := f.spool.Used()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				required := func(source, gen int64) (syncproto.Tail, bool, error) {
					calls++
					if failure == "missing source" {
						if calls == 2 {
							if _, err := f.store.db.Exec(`DELETE FROM devsync_sources`); err != nil {
								t.Fatal(err)
							}
							return f.store.requiredTail(ctx, sid, 0)
						}
					} else if calls == 2 {
						if failure == "canceled lookup" {
							cancel()
							return syncproto.Tail{}, false, ctx.Err()
						}
						return syncproto.Tail{}, false, errors.New("injected committed-reference lookup failure")
					}
					return syncproto.Tail{}, false, nil
				}
				var err error
				if sweep {
					err = f.spool.sweepTails(ctx, required)
				} else {
					err = f.spool.reconcileTailVersions(ctx, sid, required)
				}
				if err == nil || calls != 2 {
					t.Fatalf("expected second decision failure: calls=%d err=%v", calls, err)
				}
				assertReconcileFile(t, reconcileVersionPath(f.spool, sid, 0, oldHash), old)
				assertReconcileFile(t, reconcileVersionPath(f.spool, sid, 1, staleHash), stale)
				if f.spool.Used() != before {
					t.Fatalf("failed decisions changed accounting: %d -> %d", before, f.spool.Used())
				}
			})
		}
	}
}

func TestTailReconcileSourceIsolationAndExactVersion(t *testing.T) {
	spool := publicationSpool(t, 4096)
	needed, obsolete, unrelated, chunk := []byte("needed immutable\n"), []byte("obsolete canonical\n"), []byte("another source\n"), []byte("shared chunk\n")
	hash, oldHash := syncproto.Sum(needed), syncproto.Sum(obsolete)
	if err := spool.PutTail(7, 0, obsolete); err != nil {
		t.Fatal(err)
	}
	if err := spool.PutTailVersion(7, 0, hash, needed); err != nil {
		t.Fatal(err)
	}
	if err := spool.PutTailVersion(7, 0, oldHash, obsolete); err != nil {
		t.Fatal(err)
	}
	if err := spool.PutTailVersion(8, 0, syncproto.Sum(unrelated), unrelated); err != nil {
		t.Fatal(err)
	}
	if err := spool.PutChunk(syncproto.Sum(chunk), chunk); err != nil {
		t.Fatal(err)
	}
	// A malformed name is outside source-local reclamation, even with its prefix.
	malformed := filepath.Join(spool.dir, "tails", "7-0-unrecognized")
	if err := os.WriteFile(malformed, []byte("unclassified"), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	spool, err = OpenSpool(spool.dir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	required := func(sid, gen int64) (syncproto.Tail, bool, error) {
		if sid != 7 {
			t.Fatalf("source-local cleanup consulted source %d", sid)
		}
		return syncproto.Tail{Hash: hash, Size: int64(len(needed))}, gen == 0, nil
	}
	if err := spool.reconcileTailVersions(t.Context(), 7, required); err != nil {
		t.Fatal(err)
	}
	assertReconcileFile(t, reconcileVersionPath(spool, 7, 0, hash), needed)
	assertReconcileFile(t, reconcileVersionPath(spool, 8, 0, syncproto.Sum(unrelated)), unrelated)
	assertReconcileFile(t, malformed, []byte("unclassified"))
	assertReconcileFile(t, spool.chunkPath(syncproto.Sum(chunk)), chunk)
	for _, path := range []string{spool.tailPath(7, 0), reconcileVersionPath(spool, 7, 0, oldHash)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("obsolete file retained: %s: %v", path, err)
		}
	}
	if want := int64(len(needed) + len(unrelated) + len(chunk) + len("unclassified")); spool.Used() != want {
		t.Fatalf("accounting: got=%d want=%d", spool.Used(), want)
	}
}

func TestTailSweepLegacyBodyCannotBorrowSiblingIdentity(t *testing.T) {
	spool := publicationSpool(t, 4096)
	body, stale := []byte("required immutable body\n"), []byte("stale canonical bytes\n")
	hash := syncproto.Sum(body)
	if err := spool.PutTail(7, 0, stale); err != nil {
		t.Fatal(err)
	}
	if err := spool.PutTailVersion(7, 0, hash, body); err != nil {
		t.Fatal(err)
	}
	if err := spool.PutTail(8, 0, body); err != nil {
		t.Fatal(err)
	}
	if err := spool.sweepTails(t.Context(), func(sid, gen int64) (syncproto.Tail, bool, error) {
		return syncproto.Tail{Hash: hash, Size: int64(len(body))}, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spool.tailPath(7, 0)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale canonical survived matching sibling: %v", err)
	}
	assertReconcileFile(t, reconcileVersionPath(spool, 7, 0, hash), body)
	assertReconcileFile(t, spool.tailPath(8, 0), body)
	if spool.Used() != int64(2*len(body)) {
		t.Fatalf("sweep accounting: %d", spool.Used())
	}
}

func TestTailReconcileAndSweepInvalidCandidateHoldsCleanup(t *testing.T) {
	for _, sweep := range []bool{false, true} {
		for _, invalid := range []string{"corrupt version", "nonregular canonical", "required size mismatch"} {
			t.Run(fmt.Sprintf("sweep=%v/%s", sweep, invalid), func(t *testing.T) {
				spool := publicationSpool(t, 4096)
				obsolete, body := []byte("obsolete planned removal\n"), []byte("expected committed body\n")
				oldHash, hash := syncproto.Sum(obsolete), syncproto.Sum(body)
				if err := spool.PutTailVersion(7, 0, oldHash, obsolete); err != nil {
					t.Fatal(err)
				}
				if invalid == "nonregular canonical" {
					if err := os.Mkdir(spool.tailPath(7, 1), 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := spool.PutTailVersion(7, 1, hash, body); err != nil {
						t.Fatal(err)
					}
					if invalid == "corrupt version" {
						corrupted := bytes.Repeat([]byte("x"), len(body))
						if err := os.WriteFile(reconcileVersionPath(spool, 7, 1, hash), corrupted, 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				before := spool.Used()
				required := func(sid, gen int64) (syncproto.Tail, bool, error) {
					size := int64(len(body))
					if invalid == "required size mismatch" {
						size++
					}
					return syncproto.Tail{Hash: hash, Size: size}, gen == 1, nil
				}
				var err error
				if sweep {
					err = spool.sweepTails(t.Context(), required)
				} else {
					err = spool.reconcileTailVersions(t.Context(), 7, required)
				}
				if err == nil {
					t.Fatal("invalid candidate permitted reclamation")
				}
				assertReconcileFile(t, reconcileVersionPath(spool, 7, 0, oldHash), obsolete)
				if spool.Used() != before {
					t.Fatalf("invalid candidate changed accounting: %d -> %d", before, spool.Used())
				}
			})
		}
	}
}
