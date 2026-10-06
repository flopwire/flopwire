package localindex

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

// How a placement was found. Cowork origins preserve app policy provenance.
// The other methods describe transcript placement and checkout recovery.
const (
	PlacedByCoworkUnknown = "cowork-unknown" // Cowork evidence with unresolved historical host scope
	PlacedByCowork        = "cowork"         // verified Claude Desktop host-folder policy evidence
	PlacedByCwd           = "cwd"            // the named directory (a main checkout, or no repository)
	PlacedByWorktree      = "worktree"       // the named directory is in a linked worktree; its gitdir led to the main checkout
	PlacedByFolder        = "folder"         // decoded from the Claude project folder name
	PlacedByRemote        = "remote"         // the git remote the transcript recorded (and the one local checkout with that origin)
	PlacedByBranch        = "branch"         // a deleted worktree, found by the branch it recorded
	PlacedByWorktreeAdd   = "worktree-add"   // a deleted worktree, found by the `git worktree add` that made it
	PlacedByCommit        = "commit"         // a deleted worktree, found by a commit it printed
	PlacedByNone          = "unplaceable"    // nothing: the session is unplaceable
)

// PlacedFromCwd reports whether how starts from a directory the transcript
// named; such a placement is final.
func PlacedFromCwd(how string) bool {
	switch how {
	case PlacedByCwd, PlacedByWorktree, PlacedByBranch, PlacedByWorktreeAdd, PlacedByCommit:
		return true
	}
	return false
}

// Placement is where one session ran (the placements table).
type Placement struct {
	Agent     transcript.Agent
	SessionID string
	pathpolicy.Placement
	How string
	// CheckedAt is when the agent last looked for the deleted worktree's
	// main checkout (unix ms; 0 never).
	CheckedAt int64
	// Candidates are the repositories an ambiguous search found (no main
	// checkout was recorded): one per line, its main checkout and remote
	// separated by a tab. Rules are applied with each of them.
	Candidates string
	// OtherCwds are the other working directories the session named, each
	// resolved when first seen: one per line, its directory, worktree
	// root, main checkout and remote separated by tabs. Rules are applied
	// to each, and the most restrictive verdict wins.
	OtherCwds string
}

// SavePlacement records (or replaces) a session's placement.
func (s *Store) SavePlacement(ctx context.Context, p Placement) error {
	return s.write(ctx, func(w *writeTx) error {
		_, err := w.exec(`INSERT INTO placements (agent, session_id, cwd, worktree_root, main_root, remote, how, resolved_at, checked_at, candidates, other_cwds)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (agent, session_id) DO UPDATE SET cwd = excluded.cwd, worktree_root = excluded.worktree_root,
			  main_root = excluded.main_root, remote = excluded.remote, how = excluded.how, resolved_at = excluded.resolved_at,
			  checked_at = excluded.checked_at, candidates = excluded.candidates, other_cwds = excluded.other_cwds`,
			string(p.Agent), p.SessionID, nullStr(p.Cwd), nullStr(p.Worktree), nullStr(p.Main), nullStr(p.Remote), p.How, time.Now().UnixMilli(),
			sql.NullInt64{Int64: p.CheckedAt, Valid: p.CheckedAt != 0}, nullStr(p.Candidates), nullStr(p.OtherCwds))
		return err
	})
}

// Placements returns every recorded placement.
func (s *Store) Placements(ctx context.Context) ([]Placement, error) {
	rows, err := s.DB().QueryContext(ctx, `SELECT agent, session_id, ifnull(cwd, ''), ifnull(worktree_root, ''), ifnull(main_root, ''),
		ifnull(remote, ''), how, ifnull(checked_at, 0), ifnull(candidates, ''), ifnull(other_cwds, '') FROM placements`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Placement
	for rows.Next() {
		var p Placement
		var agent string
		if err := rows.Scan(&agent, &p.SessionID, &p.Cwd, &p.Worktree, &p.Main, &p.Remote, &p.How, &p.CheckedAt, &p.Candidates, &p.OtherCwds); err != nil {
			return nil, err
		}
		p.Agent = transcript.Agent(agent)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Withhold is a server deletion the device owes for a session: a directory
// it named later tightened its path-rule verdict to Mode (local or deny)
// under Rule after some of it was uploaded (placements.withhold).
type Withhold struct {
	Agent      transcript.Agent
	SessionID  string
	Mode, Rule string
}

// SetWithhold records (or, with mode "", clears) the server deletion owed
// for a session whose placement is stored. SavePlacement leaves it alone.
func (s *Store) SetWithhold(ctx context.Context, agent transcript.Agent, session, mode, rule string) error {
	v := ""
	if mode != "" {
		v = mode + "\t" + rule
	}
	return s.write(ctx, func(w *writeTx) error {
		_, err := w.exec(`UPDATE placements SET withhold = ? WHERE agent = ? AND session_id = ?`, nullStr(v), string(agent), session)
		return err
	})
}

// Withholds returns every server deletion owed.
func (s *Store) Withholds(ctx context.Context) ([]Withhold, error) {
	rows, err := s.DB().QueryContext(ctx, `SELECT agent, session_id, withhold FROM placements WHERE withhold IS NOT NULL ORDER BY agent, session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Withhold
	for rows.Next() {
		var w Withhold
		var agent, v string
		if err := rows.Scan(&agent, &w.SessionID, &v); err != nil {
			return nil, err
		}
		w.Agent = transcript.Agent(agent)
		w.Mode, w.Rule, _ = strings.Cut(v, "\t")
		out = append(out, w)
	}
	return out, rows.Err()
}

// RepoDir is a directory a session ran in and the repository its
// placement put it in.
type RepoDir struct {
	Dir    string // the worktree root, else the working directory
	Main   string // the main checkout, or the bare repository
	Remote string // normalized host/owner/name
}

// RepoDirs lists, without duplicates, the directories of every placement
// that names a repository (a main checkout or a remote): the identity
// facts --repo resolves against. An ambiguous recovery's candidates are
// not counted: the session keeps matching by its directory only.
func (s *Store) RepoDirs(ctx context.Context) ([]RepoDir, error) {
	rows, err := s.DB().QueryContext(ctx, `SELECT DISTINCT ifnull(nullif(worktree_root, ''), ifnull(cwd, '')), ifnull(main_root, ''), ifnull(remote, '')
		FROM placements WHERE ifnull(main_root, '') <> '' OR ifnull(remote, '') <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoDir
	for rows.Next() {
		var d RepoDir
		if err := rows.Scan(&d.Dir, &d.Main, &d.Remote); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// IsCoworkPlacement reports durable app provenance, including unknown scope.
func IsCoworkPlacement(how string) bool { return how == PlacedByCowork || how == PlacedByCoworkUnknown }
