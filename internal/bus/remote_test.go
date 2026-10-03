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
