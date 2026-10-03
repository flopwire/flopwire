package bus_test

import (
	"context"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
)

// #102: presence carries each session's remote, and the server routes an
// @user message and filters peers by it. gary sends from his checkout
// of acme/web; alex has a live session in his own checkout of acme/web
// at another path and one in other/web, a different repository with the
// same name. The message is for the first only, and peers by the remote
// lists it and not the other.
func TestRoutesAndPeersByRemote(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	if _, err := tm.s.Accept(ctx, busproto.Caller{UserID: tm.alex}, "gary"); err != nil {
		t.Fatal(err)
	}
	from := busproto.PresenceSession{SessionID: "g-acme-5555", Agent: "claude", Repo: "/Users/gary/code/web", Remote: "github.com/acme/web"}
	tm.present(tm.garyMac, from)
	same := busproto.PresenceSession{SessionID: "a-acme-6666", Agent: "claude", Repo: "/home/alex/src/web-local", Remote: "github.com/acme/web"}
	fork := busproto.PresenceSession{SessionID: "a-fork-7777", Agent: "codex", Repo: "/home/alex/web", Remote: "github.com/other/web", Busy: true}
	alexLinux := tm.device(tm.alex)
	tm.present(alexLinux, same, fork)
	out := tm.mustSend(tm.garyMac, "g-acme-5555", "@alex", "about the web repo")
	if out.To.Repo != "github.com/acme/web" || !out.To.Live || out.To.Busy {
		t.Fatalf("routed by %+v", out.To)
	}
	got := tm.present(alexLinux, same, fork)
	if len(got.Claimable) != 1 || len(got.Claimable[0].Sessions) != 1 || got.Claimable[0].Sessions[0] != "a-acme-6666" {
		t.Fatalf("claimable %+v", got.Claimable)
	}
	if _, err := tm.s.Claim(ctx, alexLinux, busproto.ClaimRequest{MessageID: out.ID, SessionID: "a-fork-7777"}); code(err) != busproto.CodeNotEligible {
		t.Fatalf("claim for the same-name repository: %v", err)
	}
	peers, err := tm.s.Peers(ctx, tm.garyMac, busproto.PeersQuery{Remotes: []string{"github.com/acme/web"}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range peers.Peers {
		ids = append(ids, p.Session)
	}
	if strings.Join(ids, ",") != "g-acme-5555,a-acme-6666" && strings.Join(ids, ",") != "a-acme-6666,g-acme-5555" {
		t.Fatalf("peers by remote: %v", ids)
	}
}

// Review of #125: a recipient session that reported no remote (none yet,
// or a repository without one) is still on a route by remote when its
// root has the remote's name, as name routing had it. alex's live session
// in his web checkout has no remote; his other session is on api. A
// message from gary's checkout of acme/web is for the web session only,
// not for any session as if none were on the repository.
func TestRouteByRemoteReachesSessionWithoutRemote(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	if _, err := tm.s.Accept(ctx, busproto.Caller{UserID: tm.alex}, "gary"); err != nil {
		t.Fatal(err)
	}
	tm.present(tm.garyMac, busproto.PresenceSession{SessionID: "g-acme-5555", Agent: "claude", Repo: "/Users/gary/code/web", Remote: "github.com/acme/web"})
	web := busproto.PresenceSession{SessionID: "a-web-6666", Agent: "claude", Repo: "/home/alex/web"}
	api := live("a-api-4444", "claude", "/Users/alex/code/api", true)
	tm.present(tm.alexMac, web, api)
	out := tm.mustSend(tm.garyMac, "g-acme-5555", "@alex", "about the web repo")
	got := tm.present(tm.alexMac, web, api)
	if len(got.Claimable) != 1 || strings.Join(got.Claimable[0].Sessions, ",") != "a-web-6666" {
		t.Fatalf("claimable %+v", got.Claimable)
	}
	if _, err := tm.s.Claim(ctx, tm.alexMac, busproto.ClaimRequest{MessageID: out.ID, SessionID: "a-api-4444"}); code(err) != busproto.CodeNotEligible {
		t.Fatalf("claim by the api session: %v", err)
	}
	// A session that reports the remote comes first: the one without
	// waits while it is live.
	same := busproto.PresenceSession{SessionID: "a-acme-7777", Agent: "codex", Repo: "/home/alex/web-local", Remote: "github.com/acme/web"}
	got = tm.present(tm.alexMac, web, api, same)
	if len(got.Claimable) != 1 || strings.Join(got.Claimable[0].Sessions, ",") != "a-acme-7777" {
		t.Fatalf("claimable with a session on the remote %+v", got.Claimable)
	}
}

// Review of #125: an @user send whose repo is a name the sending device
// does not know is resolved among the recipient's live sessions. When
// they hold two repositories of that name (acme/web and other/web), the
// send is refused and names them, rather than reaching either one; when
// they hold one, the message is routed by its remote.
func TestUserSendAmbiguousRepoName(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	if _, err := tm.s.Accept(ctx, busproto.Caller{UserID: tm.alex}, "gary"); err != nil {
		t.Fatal(err)
	}
	acme := busproto.PresenceSession{SessionID: "a-acme-6666", Agent: "claude", Repo: "/home/alex/web-local", Remote: "github.com/acme/web"}
	fork := busproto.PresenceSession{SessionID: "a-fork-7777", Agent: "codex", Repo: "/home/alex/web", Remote: "github.com/other/web"}
	tm.present(tm.alexMac, acme, fork)
	webRepo := func(r *busproto.SendRequest) { r.Repo = "web" }
	_, err := tm.send(tm.garyMac, "g-api-1111", "@alex", "which web?", webRepo)
	if code(err) != busproto.CodeBadRequest || !strings.Contains(err.Error(), "github.com/acme/web") || !strings.Contains(err.Error(), "github.com/other/web") {
		t.Fatalf("ambiguous name: %v", err)
	}
	tm.present(tm.alexMac, acme)
	out := tm.mustSend(tm.garyMac, "g-api-1111", "@alex", "the acme web", webRepo)
	if out.To.Repo != "github.com/acme/web" {
		t.Fatalf("routed by %q", out.To.Repo)
	}
	// The fork's session coming back does not make it eligible.
	got := tm.present(tm.alexMac, acme, fork)
	if len(got.Claimable) != 1 || strings.Join(got.Claimable[0].Sessions, ",") != "a-acme-6666" {
		t.Fatalf("claimable %+v", got.Claimable)
	}
}
