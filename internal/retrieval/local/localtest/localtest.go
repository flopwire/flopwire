// Package localtest indexes a fake $HOME's Claude Code and Codex
// transcripts into a local index with the real parsers, for retrieval
// tests. It is a test fixture loader, not the device agent: one full parse
// per source, sequentially, in discovery order, so row ids are stable.
package localtest

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// IndexHome indexes every Claude Code and Codex source under home.
func IndexHome(ctx context.Context, s *localindex.Store, home string) error {
	sessions, err := claude.Discover(claude.ProjectsRoot(func(string) string { return "" }, home))
	if err != nil {
		return err
	}
	cp := &claude.Parser{}
	for _, sess := range sessions {
		for _, src := range sess.Sources() {
			if err := IndexSource(ctx, s, cp, src); err != nil {
				return err
			}
		}
	}
	srcs, err := codex.Discover(filepath.Join(home, ".codex"))
	if err != nil {
		return err
	}
	for _, src := range srcs {
		if err := IndexSource(ctx, s, &codex.Parser{}, src); err != nil {
			return err
		}
	}
	return nil
}

// IndexSource parses one file source from the start as generation 1.
func IndexSource(ctx context.Context, s *localindex.Store, p transcript.Parser, src transcript.Source) error {
	f, err := os.Open(src.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	id := transcript.IdentityOf(fi)
	src.FileID = id.ID
	st, err := s.EnsureSource(ctx, src)
	if err != nil {
		return err
	}
	gen := transcript.Generation{Generation: 1, Size: id.Size, ChangeTime: time.Unix(0, id.CTime), CapturedAt: time.Now(), Complete: true}
	if err := s.StartGeneration(ctx, st.ID, gen, "initial"); err != nil {
		return err
	}
	sink := s.NewSink(ctx, st.ID, 1)
	sampled := time.Now()
	cur, err := p.Parse(ctx, transcript.Input{Source: &src, R: f, Size: fi.Size()}, transcript.Cursor{}, sink)
	if err != nil {
		return err
	}
	wm, err := transcript.NewWatermark(f, id, sampled, cur)
	if err != nil {
		return err
	}
	return sink.Flush(&wm, cur.State)
}
