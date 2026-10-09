package syncproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UploadConcurrency explicitly negotiates one or two uploads with the pinned,
// authenticated server. A missing endpoint is known legacy serial support;
// transport, credential, and malformed-response errors are never capabilities.
func (c *Client) UploadConcurrency(ctx context.Context, requested int) (int, error) {
	if requested != 1 && requested != 2 {
		return 0, errors.New("syncproto: requested upload concurrency must be 1 or 2")
	}
	if c.HTTP == nil {
		return 0, ErrNoHTTPClient
	}
	if c.Token == "" {
		return 0, errors.New("syncproto: upload capability device credential is required")
	}
	path := PathCapabilities + "?max_concurrent_flushes=" + strconv.Itoa(requested)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.Server, "/")+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set(HeaderVersion, strconv.Itoa(Version))
	req.Header.Set("Accept", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return 1, nil
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (64<<10)+1))
	if err != nil {
		return 0, fmt.Errorf("syncproto: read upload capability: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		he := &HTTPError{Status: res.StatusCode, RetryAt: retryAfter(res.Header.Get("Retry-After"), time.Now())}
		if len(raw) <= 64<<10 {
			_ = json.Unmarshal(raw, &he.Body)
		}
		return 0, he
	}
	if len(raw) > 64<<10 {
		return 0, errors.New("syncproto: upload capability response exceeds limit")
	}
	var out CapabilitiesResponse
	if err = json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("syncproto: invalid upload capability: %w", err)
	}
	if out.Version != Version || out.MaxConcurrentFlushes < 1 || out.MaxConcurrentFlushes > 2 || out.MaxConcurrentFlushes > requested {
		return 0, errors.New("syncproto: unsupported upload capability response")
	}
	return out.MaxConcurrentFlushes, nil
}
