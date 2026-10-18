package main

import (
	"context"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
)

// probeLocalInbox bounds one read before the prompt. It proves local row
// visibility, not import time or lease availability: inbox displays leases as
// queued too. The native transcript remains the context-entry oracle.
type probeLocalInbox struct {
	Started  time.Time `json:"started_utc"`
	Finished time.Time `json:"finished_utc"`
	State    string    `json:"state"`
}

func (r *harnessRun) observeLocalInbox(ctx context.Context, id string) *probeLocalInbox {
	o := &probeLocalInbox{Started: time.Now().UTC(), State: "unavailable"}
	defer func() { o.Finished = time.Now().UTC() }()
	if !r.p.o.local {
		return o // A connected inbox would read the server, not the device.
	}
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	resp, err := agent.Call(cctx, r.p.sock, agent.Request{Op: "inbox", Inbox: &busproto.InboxQuery{
		Session: r.recv.ID(), Agent: string(r.name), Thread: id, Limit: 1,
	}})
	if err != nil || resp.Inbox == nil {
		return o
	}
	o.State = "missing"
	for _, m := range resp.Inbox.Messages {
		if m.ID == id && m.Direction == "received" && m.ToSession == r.recv.ID() && m.ToAgent == string(r.name) {
			o.State = string(m.State)
			break
		}
	}
	return o
}

func (v *verdict) localInbox(o probeLocalInbox) {
	v.fact("one local recipient inbox observation at %s..%s: %s (no readiness wait)",
		o.Started.Format(time.RFC3339Nano), o.Finished.Format(time.RFC3339Nano), o.State)
	if o.State != string(busproto.StateQueued) {
		v.fail("local queued row was not established before the prompt")
	}
}
