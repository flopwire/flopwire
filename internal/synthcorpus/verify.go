package synthcorpus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// Report is what the real parsers made of a corpus.
type Report struct {
	Sources  int
	Messages int64
	// Issues counts diagnostics by code, text_truncated excepted (capping
	// long tool output is expected).
	Issues    map[transcript.DiagnosticCode]uint64
	Truncated uint64
	// Parser counters that mean a record was not read as intended.
	ClaudeMalformed, ClaudePersistMiss, ClaudeTooLarge int64
	CodexMalformed, CodexTypeErrors, CodexEventErrors  int64
	// Needles counts the messages whose text holds each needle.
	Needles map[string]int
}

// Verify parses every source under root with the production parsers and
// counts diagnostics and needle-bearing messages.
func Verify(ctx context.Context, root string, needles []string) (*Report, error) {
	rep := &Report{Issues: map[transcript.DiagnosticCode]uint64{}, Needles: map[string]int{}}
	sessions, err := claude.Discover(ClaudeProjects(root))
	if err != nil {
		return nil, err
	}
	codexSrcs, err := codex.Discover(CodexHome(root))
	if err != nil {
		return nil, err
	}
	var cstats claude.Stats
	var xstats codex.Stats
	cp := &claude.Parser{Stats: &cstats}
	xp := &codex.Parser{Stats: &xstats}
	type job struct {
		p   transcript.Parser
		src transcript.Source
	}
	var jobs []job
	for _, s := range sessions {
		for _, src := range s.Sources() {
			jobs = append(jobs, job{cp, src})
		}
	}
	for _, src := range codexSrcs {
		jobs = append(jobs, job{xp, src})
	}
	jobs = append(jobs, job{&devin.Parser{}, transcript.Source{Agent: transcript.AgentDevin, Path: DevinDB(root), StorageKind: transcript.StorageSQLite, Parser: devin.Name}})

	var mu sync.Mutex
	var errs []error
	ch := make(chan job)
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				sink := &countSink{needles: needles, counts: map[string]int{}}
				res, err := parseOne(ctx, j.p, j.src, sink)
				mu.Lock()
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", j.src.Path, err))
				}
				rep.Sources++
				rep.Messages += sink.n
				for k, v := range sink.counts {
					rep.Needles[k] += v
				}
				if res.Report != nil {
					for _, is := range res.Report.Issues {
						if is.Code == transcript.TextTruncated {
							rep.Truncated += is.Count
						} else {
							rep.Issues[is.Code] += is.Count
						}
					}
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	rep.ClaudeMalformed, rep.ClaudePersistMiss, rep.ClaudeTooLarge = cstats.Malformed.Load(), cstats.PersistMiss.Load(), cstats.TooLarge.Load()
	rep.CodexMalformed, rep.CodexTypeErrors, rep.CodexEventErrors = xstats.Malformed.Load(), xstats.TypeErrors.Load(), xstats.EventErrors.Load()
	return rep, errors.Join(errs...)
}

func parseOne(ctx context.Context, p transcript.Parser, src transcript.Source, sink transcript.Sink) (transcript.ParseResult, error) {
	if src.StorageKind == transcript.StorageSQLite {
		return transcript.Extract(ctx, p, transcript.Input{Source: &src}, transcript.Cursor{}, sink)
	}
	f, err := os.Open(src.Path)
	if err != nil {
		return transcript.ParseResult{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return transcript.ParseResult{}, err
	}
	return transcript.Extract(ctx, p, transcript.Input{Source: &src, R: f, Size: fi.Size()}, transcript.Cursor{}, sink)
}

type countSink struct {
	needles []string
	counts  map[string]int
	n       int64
}

func (s *countSink) Conversation(*transcript.Conversation) error { return nil }
func (s *countSink) SupersedeSession(transcript.Agent, string) error {
	return nil
}
func (s *countSink) Message(m *transcript.Message) error {
	s.n++
	for _, n := range s.needles {
		if strings.Contains(m.Text, n) {
			s.counts[n]++
		}
	}
	return nil
}

// Check returns an error unless the corpus parsed cleanly and every needle
// is in exactly as many messages as were planted.
func (r *Report) Check(m *Manifest) error {
	var problems []string
	for code, n := range r.Issues {
		problems = append(problems, fmt.Sprintf("%d %s diagnostics", n, code))
	}
	for name, n := range map[string]int64{
		"claude malformed": r.ClaudeMalformed, "claude persisted-output misses": r.ClaudePersistMiss, "claude too large": r.ClaudeTooLarge,
		"codex malformed": r.CodexMalformed, "codex type errors": r.CodexTypeErrors, "codex event errors": r.CodexEventErrors,
	} {
		if n != 0 {
			problems = append(problems, fmt.Sprintf("%d %s", n, name))
		}
	}
	for needle, want := range m.Needles {
		if got := r.Needles[needle]; got != want {
			problems = append(problems, fmt.Sprintf("needle %q in %d messages, planted %d", needle, got, want))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("synthcorpus: %s", strings.Join(problems, "; "))
	}
	return nil
}

// NeedleList returns the manifest's needles in a stable order.
func (m *Manifest) NeedleList() []string {
	var out []string
	for n := range m.Needles {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
