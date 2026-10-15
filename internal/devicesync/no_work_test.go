package devicesync

import (
	"context"
	"errors"
	"fmt"

	"github.com/flopwire/flopwire/internal/fsprobe"
	"os"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestSchedulerNoWorkProof(t *testing.T) {
	for _, scenario := range []string{"unchanged", "append", "replace", "same size rewrite", "parent metadata", "racy", "missing", "no watermark", "unknown ctime", "metadata", "repository", "unreported repository", "tail", "zero tail unacked", "old pending", "lost current", "protected", "bounded", "resume", "export", "store error"} {
		t.Run(scenario, func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := e.spec("quiet.jsonl", transcript.StorageJSONLAppend)
			if err := os.WriteFile(sp.Path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := e.sy.Sync(ctx, sp); err != nil {
				t.Fatal(err)
			}
			src := mustSource(t, e, sp)
			// Make a synthetic old sample without waiting for filesystem timestamp
			// granularity. The unchanged tuple remains the real file's metadata.
			w := *src.Watermark
			w.SampledAt = w.Identity.CTime + int64(3*time.Second)
			if err := e.store.setWatermark(ctx, src, &w); err != nil {
				t.Fatal(err)
			}
			sql := func(q string, args ...any) {
				t.Helper()
				if _, err := e.store.db.Exec(q, args...); err != nil {
					t.Fatal(err)
				}
			}
			j := &job{spec: sp}
			bounded := false
			switch scenario {
			case "append":
				appendFile(t, sp.Path, []byte("{}\n"))
			case "replace":
				if err := os.Rename(sp.Path, sp.Path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(sp.Path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "same size rewrite":
				if err := os.WriteFile(sp.Path, []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(sp.Path, 0); err != nil {
					t.Fatal(err)
				}
			case "parent metadata":
				j.spec.Parent = "/synthetic/parent"
			case "racy":
				w.SampledAt = w.Identity.CTime
				if err := e.store.setWatermark(ctx, src, &w); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(sp.Path); err != nil {
					t.Fatal(err)
				}
			case "no watermark":
				sql("UPDATE devsync_sources SET watermark=NULL")
			case "unknown ctime":
				w.Identity.CTime = 0
				if err := e.store.setWatermark(ctx, src, &w); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				j.spec.Parser += "changed"
			case "repository":
				j.spec.Checkout = "/synthetic/new"
			case "unreported repository":
				sp.Checkout = "/synthetic/repo"
				j.spec = sp
				if _, err := e.store.source(ctx, sp.Path, &sp); err != nil {
					t.Fatal(err)
				}
			case "tail":
				sql("UPDATE devsync_gens SET tail_size=1")
			case "zero tail unacked":
				sql("UPDATE devsync_gens SET tail_acked=0")
			case "old pending":
				sql("UPDATE devsync_gens SET generation=-1,tail_acked=0")
				sql("INSERT INTO devsync_gens (source_id,generation,file_id,size,change_time,captured_at,entries,tail_offset,tail_size,tail_hash,acked,tail_acked,closed,lost,srv_tail_off,srv_tail_size) SELECT source_id,0,file_id,size,change_time,captured_at,entries,tail_offset,tail_size,tail_hash,acked,1,closed,lost,srv_tail_off,srv_tail_size FROM devsync_gens")
			case "lost current":
				sql("UPDATE devsync_gens SET lost=1")
			case "protected":
				sql("UPDATE devsync_sources SET protected_origin='synthetic'")
			case "bounded":
				bounded = true
			case "resume":
				j.action = resumeUpload
			case "export":
				j.spec.Export = true
			case "store error":
				sql("DROP TABLE devsync_gens")
			}
			tr := &authTransport{Transport: e.client}
			e.sy.tr = tr
			denied := errors.New("fresh authorization required")
			calls := 0
			sc := NewScheduler(e.sy, SchedulerConfig{})
			outcome, err := sc.syncJobTurn(ctx, j, -1, bounded, func(context.Context, SourceSpec) (*CaptureAuthorization, error) { calls++; return nil, denied })
			if scenario == "unchanged" {
				if err != nil || outcome != syncDone || calls != 0 {
					t.Fatalf("outcome=%v err=%v authorization calls=%d", outcome, err, calls)
				}
			} else if !errors.Is(err, denied) || calls != 1 {
				t.Fatalf("err=%v authorization calls=%d", err, calls)
			}
			if tr.has != 0 || tr.flush != 0 {
				t.Fatalf("transport calls: has=%d flush=%d", tr.has, tr.flush)
			}
			// In particular, a metadata mismatch must not update the stored spec
			// before the fresh authorization decision.
			if scenario == "metadata" || scenario == "repository" {
				got := mustSource(t, e, sp)
				if got.Spec != sp {
					t.Fatalf("preauthorization spec mutation: %+v", got.Spec)
				}
			}
		})
	}
}

func TestSchedulerSettledFilesDoNoCaptureWork(t *testing.T) {
	const count = 128
	now := time.Now().Add(time.Hour)
	e := newEnv(t, Config{Now: func() time.Time { return now }, SealAfter: time.Minute}, 1<<20)
	ctx := context.Background()
	specs := make([]SourceSpec, count)
	data := jsonlLines(987, 3, 100)
	for i := range specs {
		sp := e.spec(fmt.Sprintf("settled-%03d.jsonl", i), transcript.StorageJSONLAppend)
		appendFile(t, sp.Path, data)
		if err := e.sy.Sync(ctx, sp); err != nil {
			t.Fatal(err)
		}
		src := mustSource(t, e, sp)
		g, err := e.store.gen(ctx, src.ID, src.Gen)
		if err != nil || g == nil || g.Size == 0 || g.Tail.Size != 0 || !g.done() || src.Watermark.Racy() {
			t.Fatalf("fixture not settled: gen=%+v err=%v", g, err)
		}
		specs[i] = sp
	}
	tr := &authTransport{Transport: e.client}
	e.sy.tr = tr
	before := e.sy.CaptureStats()
	opens, stats, authorizations := 0, 0, 0
	fsprobe.SetHook(func(op fsprobe.Op, _ string) {
		if op == fsprobe.OpOpen {
			opens++
		}
		if op == fsprobe.OpStat {
			stats++
		}
	})
	defer fsprobe.SetHook(nil)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	authorize := func(context.Context, SourceSpec) (*CaptureAuthorization, error) {
		authorizations++
		return nil, errors.New("settled source requested authorization")
	}
	for _, sp := range specs {
		if e.sy.noWork(ctx, sp, int64(len(data)-1)) {
			t.Fatal("partial bound must not qualify")
		}
		outcome, err := sc.syncJobTurn(ctx, &job{spec: sp}, int64(len(data)), true, authorize)
		if err != nil || outcome != syncDone {
			t.Fatalf("settled turn: outcome=%v err=%v", outcome, err)
		}
	}
	if opens != 0 || stats != count || authorizations != 0 || tr.has != 0 || tr.flush != 0 || e.sy.CaptureStats() != before {
		t.Fatalf("settled sweep: opens=%d stats=%d authorizations=%d has=%d flush=%d captures before=%+v after=%+v", opens, stats, authorizations, tr.has, tr.flush, before, e.sy.CaptureStats())
	}
	// An equal-length overwrite after the settled sweep requires a new lease.
	changed := append([]byte(nil), data...)
	changed[0] ^= 1
	if err := os.WriteFile(specs[0].Path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.syncJobTurn(ctx, &job{spec: specs[0]}, -1, false, authorize); err == nil || authorizations != 1 {
		t.Fatalf("rewrite: err=%v authorizations=%d", err, authorizations)
	}
}
