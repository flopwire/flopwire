package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/busproto"
)

// Bus is the message bus client (internal/busproto) for one server, with
// the device credential. The device agent holds the poll and proxies the
// CLI's send, peers and inbox through it.
//
// A failure the server explains with a bus code (a refusal, an unknown
// recipient, a session not on this device) is a *busproto.Error. A 401, or
// any answer without a bus code (a proxy page, 501 from a server without
// the bus), is an *APIError, so callers tell a refused credential from a
// refused message.
type Bus struct {
	Server, Token string
	HTTP          *http.Client // the configured client (TLS pin); nil is DefaultHTTPClient
}

func (b Bus) Send(ctx context.Context, req busproto.SendRequest) (busproto.SendResponse, error) {
	var out busproto.SendResponse
	return out, b.do(ctx, "POST", busproto.PathSend, req, &out)
}

// Poll is the long poll: it holds up to req.WaitSeconds.
func (b Bus) Poll(ctx context.Context, req busproto.PollRequest) (busproto.PollResponse, error) {
	var out busproto.PollResponse
	return out, b.do(ctx, "POST", busproto.PathPoll, req, &out)
}

func (b Bus) Claim(ctx context.Context, req busproto.ClaimRequest) (busproto.ClaimResponse, error) {
	var out busproto.ClaimResponse
	return out, b.do(ctx, "POST", busproto.PathClaim, req, &out)
}

func (b Bus) Ack(ctx context.Context, req busproto.AckRequest) (busproto.AckResponse, error) {
	var out busproto.AckResponse
	return out, b.do(ctx, "POST", busproto.PathAck, req, &out)
}

func (b Bus) Peers(ctx context.Context, q busproto.PeersQuery) (busproto.PeersResponse, error) {
	v := url.Values{}
	set(v, "session", q.Session)
	set(v, "repo", q.Repo)
	set(v, "user", q.User)
	set(v, "agent", q.Agent)
	var out busproto.PeersResponse
	return out, b.do(ctx, "GET", withQuery(busproto.PathPeers, v), nil, &out)
}

func (b Bus) Inbox(ctx context.Context, q busproto.InboxQuery) (busproto.InboxResponse, error) {
	v := url.Values{}
	set(v, "session", q.Session)
	set(v, "agent", q.Agent)
	set(v, "thread", q.Thread)
	set(v, "before", q.Before)
	if q.SentOnly {
		v.Set("sent", "true")
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	var out busproto.InboxResponse
	return out, b.do(ctx, "GET", withQuery(busproto.PathInbox, v), nil, &out)
}

func set(v url.Values, k, s string) {
	if s != "" {
		v.Set(k, s)
	}
}

func withQuery(path string, v url.Values) string {
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

func (b Bus) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.Server, "/")+path, body)
	if err != nil {
		return err
	}
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := b.HTTP
	if hc == nil {
		hc = DefaultHTTPClient()
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(out)
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var be busproto.Error
	var problem struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &problem)
	// Only the server's own problem document with a bus code is a bus
	// answer; the detail is shown to the agent, so one holding the
	// credential is not passed on.
	if res.StatusCode != http.StatusUnauthorized && problem.Type == "about:blank" && json.Unmarshal(raw, &be) == nil &&
		be.Code != "" && be.Status == res.StatusCode && (b.Token == "" || !strings.Contains(string(raw), b.Token)) {
		return &be
	}
	var code struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &code)
	return &APIError{StatusCode: res.StatusCode, Code: code.Code}
}
