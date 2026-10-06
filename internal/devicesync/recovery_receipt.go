package devicesync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
)

// AcknowledgedSourceRefs returns the current CASS generation only when every
// captured byte was acknowledged. The caller must derive recovery provenance
// from the same verified snapshot just passed to SyncSnapshot. Earlier
// generations may describe different bytes and cannot inherit that provenance.
// Empty FileID is valid for an export identity and is preserved exactly.
func (s *Store) AcknowledgedSourceRefs(ctx context.Context, spec SourceSpec, identity transcript.Identity) ([]syncproto.PolicySource, error) {
	id, err := uuid.Parse(spec.SessionKey)
	if spec.Agent != transcript.AgentClaude || spec.StorageKind != "cass_export" || spec.Parser != "cass@1" || err != nil || id == uuid.Nil || id.String() != spec.SessionKey {
		return nil, fmt.Errorf("devicesync: unqualified recovery source identity")
	}
	src, err := s.source(ctx, spec.Path, nil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if src.Spec != spec {
		return nil, fmt.Errorf("devicesync: recovery source ownership changed")
	}
	if src.Gen < 0 {
		return nil, nil
	}
	g, err := s.gen(ctx, src.ID, src.Gen)
	if err != nil {
		return nil, err
	}
	if g.FileID != fileID(identity) || g.Size != identity.Size || g.ChangeTime != identity.CTime {
		return nil, fmt.Errorf("devicesync: acknowledged recovery snapshot changed")
	}
	if g.Size <= 0 || g.Lost || g.Closed || g.Acked != g.Entries || !g.TailAcked {
		return nil, nil
	}
	return []syncproto.PolicySource{{Path: spec.Path, FileID: g.FileID, Generation: g.Gen}}, nil
}
