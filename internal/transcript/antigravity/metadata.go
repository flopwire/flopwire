package antigravity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/flopwire/flopwire/internal/fsprobe"
	_ "modernc.org/sqlite"
)

// Metadata is the session information stored beside the JSONL trajectories.
type Metadata struct {
	Cwd             string
	OtherCwds       []string
	ParentSessionID string
	Depth           int
	Title           string
	Invalid         bool // The recorded workspace or lineage could not be validated.
}

// ReadMetadata reads the SQLite summary without writing it or checkpointing its
// WAL. If the catalog or row is missing, consult Antigravity's metadata cache.
func ReadMetadata(ctx context.Context, catalogPath, sessionID string) (Metadata, error) {
	if catalogPath == "" {
		return Metadata{}, nil
	}
	db, err := openCatalog(catalogPath)
	if errors.Is(err, os.ErrNotExist) {
		return cachedMetadata(catalogPath, sessionID)
	}
	if err != nil {
		return Metadata{}, err
	}
	defer db.Close()

	_, meta, err := scanCatalogMetadata(db.QueryRowContext(ctx, catalogSelect+` WHERE conversation_id = ?`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return cachedMetadata(catalogPath, sessionID)
	}
	return meta, err
}

// ReadAllMetadata returns a catalog snapshot. Invalid workspace metadata
// leaves the corresponding session unplaced, even if the cache has a copy.
func ReadAllMetadata(ctx context.Context, catalogPath string) (map[string]Metadata, error) {
	if catalogPath == "" {
		return nil, nil
	}
	db, err := openCatalog(catalogPath)
	if errors.Is(err, os.ErrNotExist) {
		return readMetadataCache(catalogPath)
	}
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// SQLite is authoritative. Cache errors affect fallback sessions only.
	metadata, _ := readMetadataCache(catalogPath)
	if metadata == nil {
		metadata = make(map[string]Metadata)
	}
	// Bound memory retained from the full catalog independently of the cache.
	const maxRows = 100_000
	const maxMetadataBytes = 64 << 20
	rows, err := db.QueryContext(ctx, catalogSelect+` LIMIT ?`, maxRows+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	count, retained := 0, 0
	for rows.Next() {
		id, meta, err := scanCatalogMetadata(rows)
		if err != nil && !meta.Invalid {
			return nil, err
		}
		count++
		retained += len(id) + len(meta.Title) + len(meta.ParentSessionID) + len(meta.Cwd)
		for _, cwd := range meta.OtherCwds {
			retained += len(cwd)
		}
		if count > maxRows || retained > maxMetadataBytes {
			return nil, errors.New("Antigravity summary catalog exceeds the read limit")
		}
		if id != "" {
			metadata[id] = meta
		}
	}
	return metadata, rows.Err()
}

// Bound the columns returned to Go. A title is descriptive, while a missing
// workspace or lineage field makes the catalog row unsafe for placement.
const catalogSelect = `SELECT
	CASE WHEN length(CAST(conversation_id AS BLOB)) <= 128 THEN conversation_id END,
	CASE WHEN length(CAST(workspace_uris AS BLOB)) <= 1048576 THEN workspace_uris END,
	CASE WHEN length(CAST(title AS BLOB)) <= 65536 THEN title END,
	CASE WHEN length(CAST(parent_conversation_id AS BLOB)) <= 128 THEN parent_conversation_id END,
	CASE WHEN typeof(nesting_depth) = 'integer' THEN nesting_depth END
	FROM conversation_summaries`

func scanCatalogMetadata(row interface{ Scan(...any) error }) (string, Metadata, error) {
	var id, workspaceJSON, title, parent sql.NullString
	var depth sql.NullInt64
	if err := row.Scan(&id, &workspaceJSON, &title, &parent, &depth); err != nil {
		return "", Metadata{}, err
	}
	meta := Metadata{Title: title.String, ParentSessionID: parent.String}
	if !id.Valid || !parent.Valid || !depth.Valid || depth.Int64 < 0 || depth.Int64 > 1<<31-1 ||
		(parent.String == "") != (depth.Int64 == 0) || !workspaceJSON.Valid {
		meta.Invalid = true
		return id.String, meta, errors.New("invalid Antigravity workspace or lineage metadata")
	}
	meta.Depth = int(depth.Int64)
	meta, err := decodeWorkspaces(meta, workspaceJSON.String)
	return id.String, meta, err
}

func decodeWorkspaces(meta Metadata, workspaceJSON string) (Metadata, error) {
	var workspaces []string
	if err := json.Unmarshal([]byte(workspaceJSON), &workspaces); err != nil || workspaces == nil {
		meta.Invalid = true
		return meta, errors.New("invalid Antigravity workspace metadata")
	}
	return withWorkspaces(meta, workspaces)
}

func openCatalog(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := fsprobe.Stat(abs); err != nil {
		return nil, err
	}
	fsprobe.Note(fsprobe.OpOpen, abs)
	u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)"}
	return sql.Open("sqlite", u.String())
}

func cachedMetadata(catalogPath, sessionID string) (Metadata, error) {
	metadata, err := readMetadataCache(catalogPath)
	meta := metadata[sessionID]
	if err == nil && meta.Invalid {
		err = errors.New("invalid Antigravity workspace or lineage metadata")
	}
	return meta, err
}

func readMetadataCache(catalogPath string) (map[string]Metadata, error) {
	path := filepath.Join(filepath.Dir(catalogPath), "cache", "conversation_metadata.json")
	file, err := fsprobe.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	const maxCacheBytes = 16 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxCacheBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCacheBytes {
		return nil, errors.New("Antigravity metadata cache exceeds the read limit")
	}
	var cache struct {
		Conversations map[string]struct {
			Internal *bool `json:"is_internal"`
			Summary  *struct {
				Title         string
				WorkspaceURIs []string
				Internal      *bool
			}
		} `json:"conversations"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, errors.New("invalid Antigravity metadata cache")
	}
	metadata := make(map[string]Metadata, len(cache.Conversations))
	for id, entry := range cache.Conversations {
		if entry.Summary == nil {
			metadata[id] = Metadata{Invalid: true}
			continue
		}
		meta := Metadata{Title: entry.Summary.Title}
		// The JSON cache does not record parents. It can attest to placement
		// only when it explicitly describes a non-internal conversation.
		outer, inner := entry.Internal, entry.Summary.Internal
		if outer == nil && inner == nil || outer != nil && *outer || inner != nil && *inner ||
			entry.Summary.WorkspaceURIs == nil {
			meta.Invalid = true
			metadata[id] = meta
			continue
		}
		metadata[id], _ = withWorkspaces(meta, entry.Summary.WorkspaceURIs)
	}
	return metadata, nil
}

func withWorkspaces(meta Metadata, uris []string) (Metadata, error) {
	seen := make(map[string]bool, len(uris))
	for i, uri := range uris {
		u, err := url.Parse(uri)
		if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
			!filepath.IsAbs(u.Path) || strings.ContainsAny(u.Path, "\x00\t\r\n") {
			meta.Cwd = ""
			meta.OtherCwds = nil
			meta.Invalid = true
			return meta, fmt.Errorf("invalid Antigravity workspace URI at index %d", i)
		}
		path := filepath.Clean(u.Path)
		if seen[path] {
			continue
		}
		seen[path] = true
		if meta.Cwd == "" {
			meta.Cwd = path
		} else {
			meta.OtherCwds = append(meta.OtherCwds, path)
		}
	}
	return meta, nil
}
