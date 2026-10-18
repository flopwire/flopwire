package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/busproto"
)

func (b Bus) Messages(ctx context.Context, q busproto.MessagesQuery) (busproto.MessagesResponse, error) {
	v := url.Values{}
	for k, s := range map[string]string{"repo": q.Repo, "user": q.User, "session": q.Session, "thread": q.Thread, "since": q.Since, "until": q.Until, "cursor": q.Before} {
		set(v, k, s)
	}
	if len(q.Roots) > 0 {
		v["repo_root"] = q.Roots
	}
	if len(q.Mains) > 0 {
		v["repo_checkout"] = q.Mains
	}
	if len(q.Remotes) > 0 {
		v["repo_remote"] = q.Remotes
	}
	if q.Limit != 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.MaxBytes != 0 {
		v.Set("max_bytes", strconv.Itoa(q.MaxBytes))
	}
	var out busproto.MessagesResponse
	return out, b.messagesGET(ctx, withQuery(busproto.PathMessages, v), &out)
}

// Keep bus problem decoding while checking the credential actually sent by
// the rotating read transport, as well as the initially loaded credential.
func (b Bus) messagesGET(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(b.Server, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
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
	usedToken := b.Token
	if res.Request != nil {
		usedToken = strings.TrimPrefix(res.Request.Header.Get("Authorization"), "Bearer ")
	}
	var be busproto.Error
	var problem struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &problem)
	if res.StatusCode != http.StatusUnauthorized && problem.Type == "about:blank" && json.Unmarshal(raw, &be) == nil && be.Code != "" && be.Status == res.StatusCode &&
		(b.Token == "" || !strings.Contains(string(raw), b.Token)) && (usedToken == "" || !strings.Contains(string(raw), usedToken)) {
		return &be
	}
	if (b.Token != "" && strings.Contains(problem.Code, b.Token)) || (usedToken != "" && strings.Contains(problem.Code, usedToken)) {
		problem.Code = ""
	}
	return &APIError{StatusCode: res.StatusCode, Code: problem.Code}
}
