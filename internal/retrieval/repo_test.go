package retrieval

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// twoDevices is one team's server with sessions uploaded from two
// devices, each source carrying the repository its device placed the
// session in (issue #102):
//
//	session   device  cwd                          checkout                 remote
//	gary-web  laptop  /Users/gary/code/web/api     /Users/gary/code/web     github.com/acme/web
//	bob-web   desk    /home/bob/src/web-local-wt   /home/bob/src/web-local  github.com/acme/web
//	bob-fork  desk    /home/bob/web                /home/bob/web            github.com/other/web
//	gary-app  laptop  /Users/gary/app              /Users/gary/app          (none)
//	bob-app   desk    /home/bob/app                /home/bob/app            (none)
//	bob-tmp   desk    /tmp/scratch                 (none)                   (none)
func twoDevices(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	gary, bob, laptop, desk := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'gary@example.test','Gary','member','human',$3),($2,'bob@example.test','Bob','member','human',$3)`, gary, bob, now)
	exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$3,'laptop','darwin',$5),($2,$4,'desk','linux',$5)`, laptop, desk, gary, bob, now)
	for i, s := range []struct{ session, user, device, cwd, checkout, remote string }{
		{"gary-web", gary, laptop, "/Users/gary/code/web/api", "/Users/gary/code/web", "github.com/acme/web"},
		{"bob-web", bob, desk, "/home/bob/src/web-local-wt", "/home/bob/src/web-local", "github.com/acme/web"},
		{"bob-fork", bob, desk, "/home/bob/web", "/home/bob/web", "github.com/other/web"},
		{"gary-app", gary, laptop, "/Users/gary/app", "/Users/gary/app", ""},
		{"bob-app", bob, desk, "/home/bob/app", "/home/bob/app", ""},
		{"bob-tmp", bob, desk, "/tmp/scratch", "", ""},
	} {
		src, conv := uuid.NewString(), uuid.NewString()
		exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at,checkout,remote)
			VALUES($1,$2,'claude',$3,'f','jsonl_append','test',now(),NULLIF($4,''),NULLIF($5,''))`, src, s.device, "/h/"+s.session+".jsonl", s.checkout, s.remote)
		exec(`INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id,cwd) VALUES($1,$2,'claude',$3,$4,$5,$6)`, conv, src, s.session, s.device, s.user, s.cwd)
		exec(`UPDATE conversation_activity SET last_activity_at=$2 WHERE conversation_id=$1`, conv, now.Add(time.Duration(i)*time.Second))
		text := "retry the upload in " + s.session
		sum := sha256.Sum256([]byte(text))
		exec(`INSERT INTO messages(id,conversation_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser)
			VALUES($1,$2,1,'user',$3,$4,$5,$6,0,'test')`, uuid.NewString(), conv, now, text, len(text), sum[:])
	}
	return &Store{Pool: pool}
}

func sessionNames(t *testing.T, s *Store, f format.Filters) (string, error) {
	t.Helper()
	ss, err := s.Sessions(context.Background(), "", "", f)
	if err != nil {
		return "", err
	}
	var out []string
	for _, c := range ss.Sessions {
		out = append(out, c.SessionID)
	}
	slices.Sort(out)
	return strings.Join(out, ","), nil
}

// #102: --server --repo from one device matches another device's
// checkout of the same remote at another path, and never a repository
// of the same name with another remote. Before, the device sent its own
// checkout paths and the bare name: bob-web (another path) was missed,
// and the name matched bob-fork's directory as well.
func TestServerRepoAcrossDevices(t *testing.T) {
	s := twoDevices(t)
	ctx := context.Background()
	// Gary's laptop knows its own checkout of acme/web; --server --repo
	// web (or its path) expands there, as the CLI does.
	dirs := []localindex.RepoDir{{Dir: "/Users/gary/code/web", Main: "/Users/gary/code/web", Remote: "github.com/acme/web"},
		{Dir: "/Users/gary/app", Main: "/Users/gary/app"}}
	var laptop, desk string
	if err := s.Pool.QueryRow(ctx, `SELECT (SELECT id::text FROM devices WHERE name='laptop'),(SELECT id::text FROM devices WHERE name='desk')`).Scan(&laptop, &desk); err != nil {
		t.Fatal(err)
	}
	r, err := local.ExpandRepo("web", dirs, true)
	if err != nil {
		t.Fatal(err)
	}
	f := format.Filters{Repo: r.Repo, RepoRoots: r.Roots, RepoMains: r.Mains, RepoRemotes: r.Remotes}
	// Through the wire form, as the request carries it; the API sets the
	// calling device from the credential.
	if f, err = format.ParseFilters(f.Values()); err != nil {
		t.Fatal(err)
	}
	f.CallerDevice = laptop
	if got, err := sessionNames(t, s, f); err != nil || got != "bob-web,gary-web" {
		t.Fatalf("sessions --server --repo web: %q %v", got, err)
	}
	p, err := s.Grep(ctx, format.GrepQuery{Pattern: "retry", Fixed: true}, f)
	if err != nil || len(p.Hits) != 2 {
		t.Fatalf("grep --server --repo web: %+v %v", p, err)
	}
	sp, err := s.Search(ctx, format.SearchQuery{Query: "retry upload"}, f)
	if err != nil || len(sp.Hits) != 2 {
		t.Fatalf("search --server --repo web: %+v %v", sp, err)
	}
	// A repository without a remote is matched by its checkout's path
	// only: bob-app, another device's directory of the same name, is not
	// gary-app's repository.
	if r, err = local.ExpandRepo("app", dirs, true); err != nil || len(r.Remotes) != 0 {
		t.Fatal(r, err)
	}
	if got, err := sessionNames(t, s, format.Filters{RepoRoots: r.Roots, RepoMains: r.Mains, CallerDevice: laptop}); err != nil || got != "gary-app" {
		t.Fatalf("sessions --server --repo app: %q %v", got, err)
	}
	// A main checkout names the calling device's checkout only, not
	// another device's at that path.
	if got, err := sessionNames(t, s, format.Filters{RepoMains: []string{"/Users/gary/app"}, CallerDevice: desk}); err != nil || got != "" {
		t.Fatalf("another device's main checkout: %q %v", got, err)
	}
}

// #102: a name the querying device does not know is resolved on the
// server, among the repositories the devices uploaded: one repository
// by that name answers; two (different remotes, or two devices'
// checkouts without one) are an error that lists them; none is an error,
// never a match on the last element of a session's directory.
func TestServerRepoName(t *testing.T) {
	s := twoDevices(t)
	for _, c := range []struct{ repo, want, err string }{
		{"acme/web", "bob-web,gary-web", ""},
		{"github.com/acme/web", "bob-web,gary-web", ""},
		{"other/web", "bob-fork", ""},
		// A main checkout's own name names its repository: every
		// checkout of the remote.
		{"web-local", "bob-web,gary-web", ""},
		{"WEB-LOCAL", "bob-web,gary-web", ""},
		{"web", "", "names 2 repositories: github.com/acme/web; github.com/other/web"},
		{"app", "", "names 2 repositories: desk:/home/bob/app; laptop:/Users/gary/app"},
		{"scratch", "", "names no repository"},
		{"nosuch", "", "names no repository"},
	} {
		got, err := sessionNames(t, s, format.Filters{Repo: c.repo})
		if c.err != "" {
			if !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), c.err) {
				t.Errorf("--repo %s: %q %v, want an error with %q", c.repo, got, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("--repo %s: %q %v, want %q", c.repo, got, err, c.want)
		}
	}
	// Paths and globs match directories, as before.
	if got, err := sessionNames(t, s, format.Filters{Repo: "/tmp/scratch"}); err != nil || got != "bob-tmp" {
		t.Fatalf("--repo /tmp/scratch: %q %v", got, err)
	}
	if got, err := sessionNames(t, s, format.Filters{Repo: "scr*"}); err != nil || got != "bob-tmp" {
		t.Fatalf("--repo scr*: %q %v", got, err)
	}
}
