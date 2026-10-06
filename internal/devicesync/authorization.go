package devicesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/transcript"
)

// CaptureProof records which indexed bytes were captured under an acknowledged
// host policy. It belongs to a generation, never the latest source spec.
type CaptureProof struct {
	Origin, Root        string
	PolicyRequestDigest string
	Identity            transcript.Identity
	Offset              int64
	ContentSHA          []byte // full raw SHA256, required for companions
}

// CaptureAuthorization holds the caller's policy lease through capture and
// network handovers. Open must enforce the source's containment boundary.
type CaptureAuthorization struct {
	Origin, Root string
	Proof        CaptureProof
	Open         func(context.Context, SourceSpec) (*os.File, error)
	Check        func(context.Context) error
	Release      func()
	OnError      func(context.Context, error) // scheduler calls outside Syncer.mu, before Release
}

var ErrProtectionMismatch = errors.New("devicesync: source protection boundary changed; explicit migration required")

var ErrUnprovenCapture = errors.New("devicesync: capture lacks indexed policy proof")

type UnprovenCaptureError struct {
	Source               SourceSpec
	SourceID, Generation int64
	Reason               string
	Captured             bool // existing nonempty generation, requiring historical scope reconciliation
}

func (e *UnprovenCaptureError) Error() string {
	return fmt.Sprintf("%v: %s generation %d: %s", ErrUnprovenCapture, e.Source.Path, e.Generation, e.Reason)
}
func (e *UnprovenCaptureError) Unwrap() error { return ErrUnprovenCapture }

func (s *Syncer) unproven(src *sourceRow, g *genRow, reason string) error {
	gen := src.Gen
	if g != nil {
		gen = g.Gen
	}
	return &UnprovenCaptureError{Source: src.Spec, SourceID: src.ID, Generation: gen, Reason: reason, Captured: g != nil && (g.Size > 0 || g.Entries > 0 || g.Tail.Size > 0)}
}

func (s *Syncer) checkAuthorization(ctx context.Context) error {
	if s.authorization != nil {
		return s.authorization.Check(ctx)
	}
	return nil
}

func validCaptureProof(p *CaptureProof) bool {
	return p != nil && validProtection(p.Origin, p.Root) && p.PolicyRequestDigest != "" && p.Identity.ID != (transcript.FileID{}) && p.Identity.CTime != 0 && p.Identity.Size >= 0 && p.Offset >= 0 && p.Offset <= p.Identity.Size
}

func (s *Syncer) validateGeneration(src *sourceRow, g *genRow) error {
	if s.authorization == nil && g.Proof == nil && src.ProtectedOrigin == "" && src.ProtectedRoot == "" {
		return nil
	}
	if s.authorization == nil {
		return s.unproven(src, g, "authorization lease missing")
	}
	if !generationProofValid(src.Spec, g) || g.Proof.Origin != src.ProtectedOrigin || g.Proof.Root != src.ProtectedRoot {
		return s.unproven(src, g, "generation exceeds its stored identity, digest, or indexed bound")
	}
	return nil
}

func generationProofValid(sp SourceSpec, g *genRow) bool {
	if !validCaptureProof(g.Proof) || g.FileID != fileID(g.Proof.Identity) || g.ChangeTime != g.Proof.Identity.CTime || g.Size < 0 || g.Size > g.Proof.Offset || g.Tail.Offset < 0 || g.Tail.Size < 0 || g.Tail.Offset > g.Size-g.Tail.Size {
		return false
	}
	return sp.StorageKind != transcript.StorageCompanion || (len(g.Proof.ContentSHA) == sha256.Size && g.Proof.Offset == g.Proof.Identity.Size)
}

// nativeClaudeParser accepts a positive major version and optional numeric
// version components, including the current claude@4.1 parser.
func nativeClaudeParser(parser string) bool {
	if !strings.HasPrefix(parser, "claude@") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(parser, "claude@"), ".")
	for i, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil || (i == 0 && n == 0) {
			return false
		}
	}
	return true
}

func validProtection(origin, root string) bool {
	return (origin == "cowork" || origin == "desktop-code") && filepath.IsAbs(root) && filepath.Clean(root) == root
}

func (s *Syncer) protect(ctx context.Context, src *sourceRow) error {
	a := s.authorization
	protected := src.ProtectedOrigin != "" || src.ProtectedRoot != ""
	if a == nil {
		if protected {
			return s.unproven(src, nil, "protected source requires authorization lease")
		}
		return nil
	}
	if err := s.checkAuthorization(ctx); err != nil {
		return err
	}
	if protected && (a.Origin != src.ProtectedOrigin || a.Root != src.ProtectedRoot) {
		return ErrProtectionMismatch
	}
	return s.store.protectSource(ctx, src, a.Origin, a.Root)
}

func (s *Syncer) preflight(ctx context.Context, src *sourceRow) error {
	if err := s.checkAuthorization(ctx); err != nil {
		return err
	}
	gens, err := s.store.pendingGens(ctx, src.ID)
	if err != nil {
		return err
	}
	for _, g := range gens {
		if err := s.validateGeneration(src, g); err != nil {
			return err
		}
	}
	current, err := s.store.gen(ctx, src.ID, src.Gen)
	if err != nil {
		return err
	}
	if current != nil && (s.authorization != nil || current.Proof != nil) {
		return s.validateGeneration(src, current)
	}
	return nil
}

func validateProofIdentity(f *os.File, p *CaptureProof) error {
	if f == nil || p == nil {
		return ErrSourceChanged
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || transcript.IdentityOf(fi) != p.Identity {
		return ErrSourceChanged
	}
	return nil
}

func validateProofFile(f *os.File, p *CaptureProof) error {
	if err := validateProofIdentity(f, p); err != nil {
		return err
	}
	if len(p.ContentSHA) > 0 {
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, 0, p.Identity.Size)); err != nil {
			return err
		}
		if !bytes.Equal(h.Sum(nil), p.ContentSHA) {
			return ErrSourceChanged
		}
		return validateProofIdentity(f, p)
	}
	return nil
}

func (s *Syncer) validateAuthorization(src *sourceRow) error {
	a := s.authorization
	if a == nil {
		return nil
	}
	if src.Spec.Agent != transcript.AgentClaude || !nativeClaudeParser(src.Spec.Parser) || src.Spec.Export || (src.Spec.StorageKind != transcript.StorageJSONLAppend && src.Spec.StorageKind != transcript.StorageCompanion) {
		return s.unproven(src, nil, "authorization requires native Claude source")
	}
	if a.Open == nil || a.Check == nil || a.Release == nil || !validProtection(a.Origin, a.Root) || !validCaptureProof(&a.Proof) {
		return s.unproven(src, nil, "invalid authorization")
	}
	if src.Spec.StorageKind == transcript.StorageCompanion && (len(a.Proof.ContentSHA) != sha256.Size || a.Proof.Offset != a.Proof.Identity.Size) {
		return s.unproven(src, nil, "companion requires complete raw digest")
	}
	return nil
}

// SyncAuthorized captures only the proven indexed prefix and retains the
// supplied lease until return. The caller is responsible for Release.
func (s *Syncer) SyncAuthorized(ctx context.Context, spec SourceSpec, a *CaptureAuthorization) error {
	if a == nil {
		return errors.New("devicesync: authorization is required")
	}
	a, err := freezeAuthorization(a)
	if err != nil {
		return err
	}
	return s.run(ctx, spec, nil, a.Proof.Offset, nil, a)
}

func (s *Syncer) ResumeAuthorized(ctx context.Context, spec SourceSpec, a *CaptureAuthorization) error {
	if a == nil {
		return errors.New("devicesync: authorization is required")
	}
	a, err := freezeAuthorization(a)
	if err != nil {
		return err
	}
	return s.resume(ctx, spec, a)
}

func freezeAuthorization(a *CaptureAuthorization) (*CaptureAuthorization, error) {
	frozen := *a
	if frozen.Proof.Origin != "" && frozen.Proof.Origin != a.Origin || frozen.Proof.Root != "" && frozen.Proof.Root != a.Root {
		return nil, ErrProtectionMismatch
	}
	frozen.Proof.Origin, frozen.Proof.Root = a.Origin, a.Root
	frozen.Proof.ContentSHA = bytes.Clone(a.Proof.ContentSHA)
	return &frozen, nil
}
