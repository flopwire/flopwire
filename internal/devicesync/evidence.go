package devicesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// CaptureEvidence describes actual persisted native generations, including
// fully acknowledged history. Sources are ledger identities, never upload
// grants. Unproven must force an unmapped historical scope before metadata RPC.
type CaptureEvidence struct {
	Sources []syncproto.PolicySource
	// RestrictionSources match caller-verified canonical native paths whose
	// stored session key is empty. They may tighten restrictions only.
	RestrictionSources []syncproto.PolicySource
	Unproven           bool
}

// CaptureEvidence reads capture facts without changing queue, watermark, or
// acknowledgements. paths are verified native paths for empty-key aliases;
// arbitrary matching filenames are not considered. Recovery exports are
// excluded because their original-source association needs separate proof.
func (s *Scheduler) CaptureEvidence(ctx context.Context, agent transcript.Agent, session string, paths ...string) (CaptureEvidence, error) {
	return s.sy.store.captureEvidence(ctx, agent, session, "", paths)
}

// CaptureEvidenceForOrigin requires every capture proof to attest the requested
// collector origin. A proof for another origin retains its ledger reference,
// but cannot qualify historical bytes for a newly discovered origin.
func (s *Scheduler) CaptureEvidenceForOrigin(ctx context.Context, agent transcript.Agent, session, origin string, paths ...string) (CaptureEvidence, error) {
	if origin != "cowork" && origin != "desktop-code" {
		return CaptureEvidence{}, errors.New("devicesync: valid capture evidence origin required")
	}
	return s.sy.store.captureEvidence(ctx, agent, session, origin, paths)
}

func (s *Store) captureEvidence(ctx context.Context, agent transcript.Agent, session, expectedOrigin string, paths []string) (CaptureEvidence, error) {
	var out CaptureEvidence
	if agent == "" || session == "" {
		return out, errors.New("devicesync: capture evidence session identity required")
	}
	verified := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path == "" {
			return out, errors.New("devicesync: empty verified native source path")
		}
		verified[path] = true
	}
	// Use one read snapshot, including all acknowledged and lost generations.
	// Malformed relevant specs fail the query rather than silently omit history.
	cols := strings.Split(genCols, ",")
	for i, c := range cols {
		cols[i] = "g." + strings.TrimSpace(c)
	}
	where := `(json_extract(s.spec,'$.Agent')=? AND json_extract(s.spec,'$.SessionKey')=?)`
	args := []any{string(agent), session}
	if len(paths) > 0 {
		where += ` OR s.path IN (` + strings.TrimSuffix(strings.Repeat("?,", len(paths)), ",") + `)`
		for _, path := range paths {
			args = append(args, path)
		}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.path,s.spec,s.protected_origin,s.protected_root,`+strings.Join(cols, ",")+` FROM devsync_sources s JOIN devsync_gens g ON g.source_id=s.id WHERE `+where+` ORDER BY s.path,g.generation`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var sid int64
		var path, raw, origin, root string
		g, err := scanGen(&evidenceScanner{row: rows, prefix: []any{&sid, &path, &raw, &origin, &root}})
		if err != nil {
			return CaptureEvidence{}, err
		}
		var sp SourceSpec
		if err := json.Unmarshal([]byte(raw), &sp); err != nil {
			return CaptureEvidence{}, err
		}
		if sp.Path != path || sp.Agent != agent {
			return CaptureEvidence{}, errors.New("devicesync: captured source identity mismatch")
		}
		// CASS recovery may carry the original native session ID, but its export
		// path is not native evidence and must not become a native source grant.
		if sp.Export || sp.StorageKind == "cass_export" || strings.HasPrefix(sp.Parser, "cass@") {
			continue
		}
		if sp.Agent != transcript.AgentClaude || !nativeClaudeParser(sp.Parser) ||
			(sp.StorageKind != transcript.StorageJSONLAppend && sp.StorageKind != transcript.StorageCompanion) {
			return CaptureEvidence{}, errors.New("devicesync: captured source is not native Claude evidence")
		}
		if sp.StorageKind == transcript.StorageCompanion && !canonicalRestrictionSource(sp, session) {
			return CaptureEvidence{}, errors.New("devicesync: companion lacks canonical native parent association")
		}
		if g.Size <= 0 && g.Entries <= 0 && g.Tail.Size <= 0 {
			continue
		}
		ref := syncproto.PolicySource{Path: path, FileID: g.FileID, Generation: g.Gen}
		if ref.Path == "" || ref.FileID == "" || ref.Generation < 0 {
			return CaptureEvidence{}, errors.New("devicesync: invalid captured source reference")
		}
		switch {
		case sp.SessionKey == session:
			out.Sources = append(out.Sources, ref)
			if !generationProofValid(sp, g) || g.Proof.Origin != origin || g.Proof.Root != root || (expectedOrigin != "" && g.Proof.Origin != expectedOrigin) {
				out.Unproven = true
			}
		case sp.SessionKey == "" && verified[path] && canonicalRestrictionSource(sp, session):
			out.RestrictionSources = append(out.RestrictionSources, ref)
			out.Unproven = true
		default:
			return CaptureEvidence{}, fmt.Errorf("devicesync: captured path is not associated with native session %s", session)
		}
	}
	if err := rows.Err(); err != nil {
		return CaptureEvidence{}, err
	}
	return out, nil
}

// evidenceScanner lets scanGen share its strict decoding with this query.
type evidenceScanner struct {
	row    interface{ Scan(...any) error }
	prefix []any
}

func (s *evidenceScanner) Scan(values ...any) error {
	return s.row.Scan(append(s.prefix, values...)...)
}

func canonicalRestrictionSource(sp SourceSpec, session string) bool {
	if sp.StorageKind == transcript.StorageJSONLAppend {
		return filepath.Base(sp.Path) == session+".jsonl"
	}
	if sp.StorageKind != transcript.StorageCompanion || sp.Parent == "" || filepath.Base(sp.Parent) != session+".jsonl" {
		return false
	}
	parentStem := strings.TrimSuffix(sp.Parent, ".jsonl")
	if sp.Path == parentStem+".meta.json" {
		return true
	}
	rel, err := filepath.Rel(parentStem, sp.Path)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
