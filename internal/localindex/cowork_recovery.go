package localindex

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
)

func recoveryParser(parser string) bool {
	version, err := strconv.Atoi(strings.TrimPrefix(parser, "cass@"))
	return strings.HasPrefix(parser, "cass@") && err == nil && version > 0
}

// CoworkRecoverySources identifies already-shared CASS copies solely to add
// restrictions. Conversation provenance, retained local source association,
// and an acknowledged device-sync generation must all agree. It never infers
// folder grants from an export or qualifies recovered history for sharing.
func (s *Store) CoworkRecoverySources(ctx context.Context, nativeUUID string) ([]syncproto.PolicyRecoverySource, error) {
	id, err := uuid.Parse(nativeUUID)
	if err != nil || id == uuid.Nil || id.String() != nativeUUID {
		return nil, fmt.Errorf("localindex: invalid canonical Cowork native UUID")
	}
	var result []syncproto.PolicyRecoverySource
	err = s.readSources(ctx, func(ctx context.Context, q dbtx) error {
		var tables int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('devsync_sources','devsync_gens')`).Scan(&tables); err != nil {
			return err
		}
		if tables == 0 {
			return nil
		}
		if tables != 2 {
			return fmt.Errorf("localindex: incomplete Cowork recovery sync schema")
		}
		// Validate required columns even when no indexed provenance is present.
		check, err := q.QueryContext(ctx, `SELECT s.path,s.spec,g.file_id,g.generation,g.acked,g.tail_acked,g.tail_size FROM devsync_sources s JOIN devsync_gens g ON g.source_id=s.id LIMIT 0`)
		if err != nil {
			return err
		}
		if err = check.Close(); err != nil {
			return err
		}
		rows, err := q.QueryContext(ctx, `SELECT id,session_id,coalesce(extra,'{}') FROM conversations WHERE device_id=? AND agent='claude'`, s.opts.DeviceID)
		if err != nil {
			return err
		}
		type provenance struct {
			id       int64
			original string
		}
		var proven []provenance
		for rows.Next() {
			var cid int64
			var session, raw string
			if err = rows.Scan(&cid, &session, &raw); err != nil {
				rows.Close()
				return err
			}
			var extra map[string]json.RawMessage
			if err = json.Unmarshal([]byte(raw), &extra); err != nil {
				if session == nativeUUID || strings.Contains(raw, nativeUUID) {
					rows.Close()
					return fmt.Errorf("localindex: malformed relevant CASS provenance: %w", err)
				}
				continue
			}
			var external string
			if err = json.Unmarshal(extra["cass_external_id"], &external); err != nil {
				if session == nativeUUID && len(extra["recovered_history"]) > 0 {
					rows.Close()
					return fmt.Errorf("localindex: invalid CASS native provenance: %w", err)
				}
				continue
			}
			if external != nativeUUID {
				continue
			}
			var recovered bool
			if err = json.Unmarshal(extra["recovered_history"], &recovered); err != nil {
				rows.Close()
				return fmt.Errorf("localindex: invalid CASS recovery provenance: %w", err)
			}
			if !recovered {
				continue
			}
			var original string
			if err = json.Unmarshal(extra["cass_source_path"], &original); err != nil {
				rows.Close()
				return fmt.Errorf("localindex: invalid CASS original path: %w", err)
			}
			if !filepath.IsAbs(original) || filepath.Clean(original) != original || strings.ContainsAny(original, "\x00\t\n\r") {
				rows.Close()
				return fmt.Errorf("localindex: invalid CASS original path")
			}
			proven = append(proven, provenance{cid, original})
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		seen := map[syncproto.PolicyRecoverySource]bool{}
		for _, p := range proven {
			sources, err := q.QueryContext(ctx, `SELECT DISTINCT src.path,src.parser FROM sources src WHERE src.device_id=? AND src.agent='claude' AND src.storage_kind='cass_export' AND (src.id=(SELECT source_id FROM conversations WHERE id=?) OR EXISTS(SELECT 1 FROM messages m WHERE m.source_id=src.id AND m.conversation_id=?))`, s.opts.DeviceID, p.id, p.id)
			if err != nil {
				return err
			}
			var paths []string
			for sources.Next() {
				var path, parser string
				if err = sources.Scan(&path, &parser); err != nil {
					sources.Close()
					return err
				}
				if recoveryParser(parser) && filepath.IsAbs(path) && filepath.Clean(path) == path {
					paths = append(paths, path)
				}
			}
			if err = sources.Err(); err != nil {
				sources.Close()
				return err
			}
			sources.Close()
			for _, path := range paths {
				gens, err := q.QueryContext(ctx, `SELECT s.spec,g.file_id,g.generation,g.acked,g.tail_acked,g.tail_size FROM devsync_sources s JOIN devsync_gens g ON g.source_id=s.id WHERE s.path=?`, path)
				if err != nil {
					return err
				}
				for gens.Next() {
					var raw, fileID string
					var generation, acked, tailAcked, tailSize int64
					if err = gens.Scan(&raw, &fileID, &generation, &acked, &tailAcked, &tailSize); err != nil {
						gens.Close()
						return err
					}
					var spec struct{ Path, Agent, StorageKind, SessionKey, Parser string }
					if err = json.Unmarshal([]byte(raw), &spec); err != nil {
						gens.Close()
						return fmt.Errorf("localindex: malformed CASS captured source: %w", err)
					}
					if spec.Path != path || spec.Agent != "claude" || spec.StorageKind != "cass_export" || spec.SessionKey != nativeUUID || !recoveryParser(spec.Parser) {
						continue
					}
					if generation < 0 {
						gens.Close()
						return fmt.Errorf("localindex: invalid CASS captured identity")
					}
					if acked <= 0 && !(tailAcked > 0 && tailSize > 0) {
						continue
					}
					ref := syncproto.PolicyRecoverySource{Source: syncproto.PolicySource{Path: path, FileID: fileID, Generation: generation}, OriginalPath: p.original}
					if !seen[ref] {
						seen[ref] = true
						result = append(result, ref)
					}
				}
				if err = gens.Err(); err != nil {
					gens.Close()
					return err
				}
				gens.Close()
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Source.Path != b.Source.Path {
			return a.Source.Path < b.Source.Path
		}
		if a.Source.Generation != b.Source.Generation {
			return a.Source.Generation < b.Source.Generation
		}
		return a.OriginalPath < b.OriginalPath
	})
	return result, nil
}
