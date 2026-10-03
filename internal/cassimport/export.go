package cassimport

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
	_ "modernc.org/sqlite"
)

type Entry struct {
	File           string           `json:"file"`
	Agent          transcript.Agent `json:"agent"`
	SessionID      string           `json:"session_id"`
	ConversationID int64            `json:"cass_conversation_id"`
	Messages       int64            `json:"messages"`
	SHA256         string           `json:"sha256"`
}
type Manifest struct {
	Version int     `json:"version"`
	Origin  string  `json:"origin"`
	Entries []Entry `json:"entries"`
}

var addressID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func millis(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.UnixMilli(n.Int64).UTC()
}

// Export reads selected conversation IDs from a consistent read-only SQLite
// transaction. Selection is explicit so current native history wins. Output
// is private and fresh; the manifest is published only after every file closes.
func Export(ctx context.Context, database, out, origin string, selection []int64) (*Manifest, error) {
	if origin == "" || len(selection) == 0 {
		return nil, fmt.Errorf("cass: origin and nonempty selection are required")
	}
	abs, err := filepath.Abs(database)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = os.Mkdir(out, 0700); err != nil {
		return nil, fmt.Errorf("cass: output must be a new directory: %w", err)
	}
	manifest := &Manifest{Version: 1, Origin: origin}
	seen := map[int64]bool{}
	sessions := map[string]bool{}
	for _, id := range selection {
		if id <= 0 || seen[id] {
			return nil, fmt.Errorf("cass: invalid or duplicate conversation ID")
		}
		seen[id] = true
		e, err := exportConversation(ctx, tx, out, origin, id)
		if err != nil {
			return nil, err
		}
		key := string(e.Agent) + "|" + e.SessionID
		if sessions[key] {
			return nil, fmt.Errorf("cass: duplicate native session in selection")
		}
		sessions[key] = true
		manifest.Entries = append(manifest.Entries, *e)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(out, "manifest.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	err = json.NewEncoder(f).Encode(manifest)
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return manifest, nil
}

func exportConversation(ctx context.Context, tx *sql.Tx, out, origin string, id int64) (*Entry, error) {
	var slug string
	var external, title, path, cwd, sourceID, originHost, metadata sql.NullString
	var started, ended sql.NullInt64
	var metadataBin []byte
	err := tx.QueryRowContext(ctx, `SELECT a.slug,c.external_id,c.title,c.source_path,w.path,c.started_at,c.ended_at,c.source_id,c.origin_host,c.metadata_json,c.metadata_bin FROM conversations c JOIN agents a ON a.id=c.agent_id LEFT JOIN workspaces w ON w.id=c.workspace_id WHERE c.id=?`, id).
		Scan(&slug, &external, &title, &path, &cwd, &started, &ended, &sourceID, &originHost, &metadata, &metadataBin)
	if err != nil {
		return nil, fmt.Errorf("cass: conversation %d: %w", id, err)
	}
	agents := map[string]transcript.Agent{"claude_code": transcript.AgentClaude, "codex": transcript.AgentCodex, "devin": transcript.AgentDevin}
	agent, ok := agents[slug]
	if !ok {
		return nil, fmt.Errorf("cass: unsupported agent %q", slug)
	}
	session := external.String
	if !addressID.MatchString(session) {
		key := sha256.Sum256([]byte(origin + "\x00" + strconv.FormatInt(id, 10)))
		session = "cass-" + hex.EncodeToString(key[:16])
	}
	c := &transcript.Conversation{Agent: agent, SessionID: session, Cwd: cwd.String, Title: title.String, StartedAt: millis(started), LastActivityAt: millis(ended), Extra: map[string]any{
		"recovered_history": true, "recovery_format": Name, "cass_origin": origin, "cass_conversation_id": id, "cass_agent": slug, "cass_external_id": external.String, "cass_source_path": path.String, "cass_source_id": sourceID.String, "cass_origin_host": originHost.String, "cass_metadata_json": metadata.String, "cass_metadata_bin": metadataBin,
	}}
	hash := sha256.Sum256([]byte(origin + "\x00" + strconv.FormatInt(id, 10)))
	name := hex.EncodeToString(hash[:]) + ".jsonl"
	f, err := os.OpenFile(filepath.Join(out, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	enc := json.NewEncoder(io.MultiWriter(f, h))
	if err = enc.Encode(Record{Version: 1, Conversation: c}); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,idx,role,author,created_at,content,extra_json,extra_bin FROM messages WHERE conversation_id=? ORDER BY idx,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	e := &Entry{File: name, Agent: agent, SessionID: session, ConversationID: id}
	for rows.Next() {
		var mid, idx int64
		var role, author, text, extra sql.NullString
		var created sql.NullInt64
		var extraBin []byte
		if err = rows.Scan(&mid, &idx, &role, &author, &created, &text, &extra, &extraBin); err != nil {
			return nil, err
		}
		kind := transcript.KindUnknown
		switch role.String {
		case "user":
			kind = transcript.KindUser
		case "agent", "assistant":
			kind = transcript.KindAssistant
		case "system", "developer":
			kind = transcript.KindSystem
		}
		// CASS's tool role does not distinguish calls from results. Preserve it
		// without inferring either, or inventing native tool/message identifiers.
		m := &transcript.Message{SessionID: session, NativeID: "cass:" + origin + ":" + strconv.FormatInt(mid, 10), Ordinal: e.Messages, Kind: kind, Role: role.String, TS: millis(created), Text: text.String, Parser: Name, Enrichment: map[string]any{
			"recovered_history": true, "cass_origin": origin, "cass_message_id": mid, "cass_idx": idx, "cass_role": role.String, "cass_author": author.String, "cass_extra_json": extra.String, "cass_extra_bin": extraBin,
		}}
		if err = enc.Encode(Record{Version: 1, Message: m}); err != nil {
			return nil, err
		}
		e.Messages++
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	e.SHA256 = hex.EncodeToString(h.Sum(nil))
	return e, nil
}
