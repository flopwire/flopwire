package api_test

import (
	"net/http"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestMessagesDeviceAPI(t *testing.T) {
	b := newBusServer(t)
	path := b.url + busproto.PathMessages + "?repo=/src/api&repo_root=/src/api&repo_remote=github.test/team/api"
	for _, token := range []string{b.gary.session, b.admin} {
		if st, code := busCall(t, "GET", path, token, nil, nil); st != 403 || code != busproto.CodeDeviceRequired {
			t.Fatalf("login/admin credentials: %d %s", st, code)
		}
	}
	_, minted := mint(t, b.url, b.gary.session, map[string]any{"label": "messages-test", "scopes": []string{"read"}})
	if st, code := busCall(t, "GET", path, minted["token"].(string), nil, nil); st != 403 || code != busproto.CodeDeviceRequired {
		t.Fatalf("minted credential: %d %s", st, code)
	}
	gp := presence("gary-1111", "codex", true)
	gp.Sessions[0].Remote = "github.test/team/api"
	busCall(t, "POST", b.url+busproto.PathPoll, b.gary.device, gp, nil)
	busCall(t, "POST", b.url+busproto.PathPoll, b.alex.device, presence("alex-1111", "codex", true), nil)
	var sent busproto.SendResponse
	if st, _ := busCall(t, "POST", b.url+busproto.PathSend, b.gary.device, busproto.SendRequest{FromSession: "gary-1111", To: "alex-1111", Body: "synthetic held secret"}, &sent); st != http.StatusCreated {
		t.Fatal(st)
	}
	var out busproto.MessagesResponse
	if st, _ := busCall(t, "GET", path, b.alex.device, nil, &out); st != 200 || len(out.Threads) != 0 {
		t.Fatalf("held incoming exposed: %d %+v", st, out)
	}
	if st, code := busCall(t, "GET", path+"&thread="+sent.ThreadID, b.alex.device, nil, nil); st != 404 || code != busproto.CodeNotFound {
		t.Fatalf("safe thread not-found: %d %s", st, code)
	}
	if st, _ := busCall(t, "GET", path, b.gary.device, nil, &out); st != 200 || len(out.Threads) != 1 {
		t.Fatalf("authored held: %d %+v", st, out)
	}
	busCall(t, "POST", b.url+busproto.PathAccepts, b.alex.session, busproto.AcceptRequest{Sender: "gary", Password: memberPassword}, nil)
	if st, _ := busCall(t, "GET", path+"&thread="+sent.ThreadID, b.alex.device, nil, &out); st != 200 || len(out.Messages) != 1 || out.Messages[0].Body != "synthetic held secret" {
		t.Fatalf("accepted full thread: %d %+v", st, out)
	}
	busCall(t, "DELETE", b.url+busproto.PathAccepts+"/gary", b.alex.session, nil, nil)
	if st, _ := busCall(t, "GET", path, b.alex.device, nil, &out); st != 200 || len(out.Threads) != 0 {
		t.Fatalf("revocation: %d %+v", st, out)
	}
	for _, bad := range []string{"&account=other", "&limit=0", "&max_bytes=3", "&since=bad"} {
		if st, _ := busCall(t, "GET", path+bad, b.gary.device, nil, nil); st != 400 {
			t.Fatalf("%s: %d", bad, st)
		}
	}
}
