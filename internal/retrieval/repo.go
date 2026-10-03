package retrieval

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// A repository on the server (issue #102). Each device uploads, with
// every source, the repository it placed the session in: its main
// checkout and its normalized remote (sources.checkout, sources.remote).
// One remote is one repository, on any device and at any path. A
// repository without a remote is one device's main checkout: two
// devices' checkouts of it cannot be told from two repositories of the
// same name, so they never merge.

// maxRepoNameRows bounds the identities a repo name is resolved over.
const maxRepoNameRows = 1000

// resolveFilterRepo turns f.Repo, when it is a repository name (not a
// path, not a glob), into the repository it names among the uploaded
// placements: RepoRemotes for one with a remote, RepoCheckouts for one
// without. A name that fits no repository, or more than one, is a bad
// request that says so; it never falls back to matching the last
// element of a session's directory, which would merge two repositories
// of one name.
//
// A name is the last element of a remote or of a main checkout (a bare
// repository's without ".git" and a leading dot); owner/name or
// host/owner/name is a remote, or a main checkout path, ending with it.
// Matching ignores case. Two uploads are one repository when they share
// the remote, or the device and main checkout (as the device groups its
// own placements).
func (s *Store) resolveFilterRepo(ctx context.Context, f *format.Filters) error {
	name := strings.TrimSuffix(strings.TrimSpace(f.Repo), "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "*?[") {
		return nil
	}
	lower := likeEscape(strings.ToLower(name))
	pats := []string{"%/" + lower}
	if !strings.Contains(name, "/") {
		pats = append(pats, "%/"+lower+".git", "%/."+lower+".git")
	}
	rows, err := s.db().Query(ctx, `SELECT DISTINCT COALESCE(s.remote,''),s.device_id::text,COALESCE(d.name,''),COALESCE(s.checkout,'')
		FROM sources s JOIN devices d ON d.id=s.device_id
		WHERE (lower(s.remote)=$1 OR lower(s.remote) LIKE ANY($2) OR lower(s.checkout) LIKE ANY($2))
			AND EXISTS (SELECT 1 FROM conversations c WHERE c.source_id=s.id AND c.hidden_at IS NULL)
		LIMIT `+fmt.Sprint(maxRepoNameRows), strings.ToLower(name), pats)
	if err != nil {
		return err
	}
	type up struct{ remote, device, deviceName, checkout string }
	var ups []up
	for rows.Next() {
		var u up
		if err := rows.Scan(&u.remote, &u.device, &u.deviceName, &u.checkout); err != nil {
			rows.Close()
			return err
		}
		if u.remote != "" && !strings.Contains(name, "/") && !strings.EqualFold(repoName(u.remote), name) &&
			!strings.EqualFold(repoName(u.checkout), name) {
			continue // a remote whose path, not name, ends in name
		}
		ups = append(ups, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Group the uploads: a remote, or a device's main checkout, is a key;
	// one upload joins its keys.
	parent := map[string]string{}
	var find func(string) string
	find = func(k string) string {
		if p, ok := parent[k]; ok && p != k {
			r := find(p)
			parent[k] = r
			return r
		}
		parent[k] = k
		return k
	}
	keys := func(u up) []string {
		var ks []string
		if u.remote != "" {
			ks = append(ks, "r\x00"+u.remote)
		}
		if u.checkout != "" {
			ks = append(ks, "c\x00"+u.device+"\x00"+u.checkout)
		}
		return ks
	}
	for _, u := range ups {
		ks := keys(u)
		for _, k := range ks[1:] {
			parent[find(k)] = find(ks[0])
		}
	}
	type repo struct {
		remotes   []string
		checkouts []format.DeviceCheckout
		labels    []string
	}
	groups := map[string]*repo{}
	var order []string
	for _, u := range ups {
		ks := keys(u)
		if len(ks) == 0 {
			continue
		}
		root := find(ks[0])
		g := groups[root]
		if g == nil {
			g = &repo{}
			groups[root] = g
			order = append(order, root)
		}
		if u.remote != "" && !slices.Contains(g.remotes, u.remote) {
			g.remotes = append(g.remotes, u.remote)
			g.labels = append(g.labels, u.remote)
		}
		if u.checkout != "" {
			dc := format.DeviceCheckout{Device: u.device, Checkout: u.checkout}
			if !slices.Contains(g.checkouts, dc) {
				g.checkouts = append(g.checkouts, dc)
				if u.remote == "" {
					g.labels = append(g.labels, u.deviceName+":"+u.checkout)
				}
			}
		}
	}
	switch len(order) {
	case 0:
		return fmt.Errorf("%w: repo %q names no repository any device uploaded a session from; pass a path, or owner/name", ErrBadRequest, f.Repo)
	case 1:
		g := groups[order[0]]
		f.Repo = ""
		f.RepoRemotes = append(f.RepoRemotes, g.remotes...)
		f.RepoCheckouts = append(f.RepoCheckouts, g.checkouts...)
		return nil
	}
	labels := make([]string, 0, len(order))
	for _, k := range order {
		g := groups[k]
		slices.Sort(g.labels)
		labels = append(labels, strings.Join(g.labels, ", "))
	}
	slices.Sort(labels)
	return fmt.Errorf("%w: repo %q names %d repositories: %s; pass owner/name or a path", ErrBadRequest, f.Repo, len(order), strings.Join(labels, "; "))
}

// repoName is the last element of a remote or a main checkout, a bare
// repository's without ".git" and a leading dot.
func repoName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	if n, ok := strings.CutSuffix(p, ".git"); ok {
		p = strings.TrimPrefix(n, ".")
	}
	return p
}
