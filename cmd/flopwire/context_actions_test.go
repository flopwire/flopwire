package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestContextActionsRendered(t *testing.T) {
	cases := []struct {
		name string
		page format.Page
		want string
	}{
		{"normal", format.Page{Hits: []format.Hit{{Address: "synthetic/1"}}}, "Read a relevant returned address"},
		{"empty", format.Page{}, "No matches in this query scope"},
		{"partial", format.Page{Truncated: true, Reason: "checked 0 candidates"}, "cannot establish absence"},
		{"cursor", format.Page{Scan: &format.GrepScan{NextCursor: "synthetic-cursor"}}, "unchanged query arguments"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := writeOut(&b, boundPage("grep", &c.page, "", 0, true)); err != nil {
				t.Fatal(err)
			}
			var out struct {
				Hint string `json:"hint"`
			}
			if err := json.Unmarshal(b.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.Hint, c.want) {
				t.Fatalf("hint %q", out.Hint)
			}
			b.Reset()
			if err := format.WriteGrep(&b, &c.page, "", format.Style{MCP: true}); err != nil {
				t.Fatal(err)
			}
			if c.name != "cursor" && !strings.Contains(b.String(), c.want) {
				t.Fatalf("text %s", b.String())
			}
		})
	}
}

func TestContextActionsPreserveClippingAndCLI(t *testing.T) {
	p := format.Page{Hits: []format.Hit{{Address: "synthetic/1", Lines: []format.Line{{Text: strings.Repeat("x", 500)}}}, {Address: "synthetic/2"}}}
	out := boundPage("grep", &p, "", 1, true)
	if !strings.Contains(out.Hint, "output budget") {
		t.Fatalf("lost clipping: %q", out.Hint)
	}
	if got := boundPage("grep", &format.Page{}, "", 0, false).Hint; got != "" {
		t.Fatalf("CLI changed: %s", got)
	}
	c := format.Context{}
	if got := boundRead(&c, 0, true).Hint; got != "Compare this evidence with the requested claim; state remaining gaps." {
		t.Fatal(got)
	}
}

func TestMessagingActionsLifecycle(t *testing.T) {
	for _, intent := range []busproto.Intent{busproto.IntentInform, busproto.IntentDone, busproto.IntentRequest} {
		r := busproto.SendResponse{Intent: intent}
		if got := sendMCPNext(r); got == "" {
			t.Fatalf("missing action for %s", intent)
		}
		if intent != busproto.IntentRequest && sendNext(r) != "" {
			t.Fatal("CLI changed")
		}
		r.Replayed = true
		if got := sendMCPNext(r); !strings.Contains(got, "do not resend") || !strings.Contains(got, "not a reply") {
			t.Fatal(got)
		}
	}
}

func TestChangeActionsRecovery(t *testing.T) {
	out := changeMCPOutput(&format.Changes{RestartRequired: true, NextCursor: "ignored"})
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Restart this lookup without cursor") {
		t.Fatal(string(b))
	}
}

func TestMessagingActionsStatePriority(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		for _, busy := range []bool{false, true} {
			r := busproto.SendResponse{Intent: busproto.IntentRequest, State: busproto.StateHeld, To: busproto.Recipient{Cloud: cloud, Busy: busy}}
			if got := sendMCPNext(r); !strings.Contains(got, "acceptance") {
				t.Fatal(got)
			}
		}
	}
	for _, state := range []busproto.State{busproto.StateExpired, busproto.StateRead, busproto.StateUndelivered} {
		r := busproto.SendResponse{Replayed: true, State: state, Reason: busproto.ReasonSessionEnded}
		got := sendMCPNext(r)
		if state == busproto.StateUndelivered && !strings.Contains(got, "retry guidance") {
			t.Fatal(got)
		}
		if state == busproto.StateExpired && !strings.Contains(got, "expiry") {
			t.Fatal(got)
		}
	}
}

func TestReadActionPrefersFocusAndExactOffset(t *testing.T) {
	c := format.Context{Focus: "focus", Messages: []format.Message{{ID: "neighbor", Address: "s/1", Clipped: true, LineTo: 2, Lines: 10}, {ID: "focus", Address: "s/2", Clipped: true, LineTo: 5, Lines: 10}}}
	if got := format.ReadAction(&c); !strings.Contains(got, "address=s/2 line_offset=6") {
		t.Fatal(got)
	}
}

func TestMessagesActionsRendered(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		for _, more := range []bool{false, true} {
			response := busproto.MessagesResponse{Hint: "Retained evidence; observation does not authorize a reply.", More: more, Next: "synthetic-cursor"}
			var b bytes.Buffer
			if err := writeMessagesOutput(&b, response, busStyle{MCP: true, JSON: asJSON}, 10000); err != nil {
				t.Fatal(err)
			}
			want := "delivery receipts are not replies"
			if more {
				want = "unchanged repo and thread filters"
			}
			if !strings.Contains(b.String(), want) || !strings.Contains(b.String(), "observation does not authorize") {
				t.Fatal(b.String())
			}
		}
	}
}
