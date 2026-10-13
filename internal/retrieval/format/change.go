package format

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// ChangeQuery searches observed commit evidence within Filters.Repo.
type ChangeQuery struct {
	Commits []string `json:"commits"`
	Limit   int      `json:"limit,omitempty"`
	Cursor  string   `json:"cursor,omitempty"`
}
type Changes struct {
	Kind            string         `json:"kind"`
	Matches         []ChangeMatch  `json:"matches"`
	HasMore         bool           `json:"has_more"`
	NextCursor      string         `json:"next_cursor,omitempty"`
	RestartRequired bool           `json:"restart_required,omitempty"`
	Coverage        ChangeCoverage `json:"coverage"`
}
type ChangeMatch struct {
	CommandRaw           bool             `json:"command_raw,omitempty"`
	RevealCommandRaw     bool             `json:"reveal_command_raw,omitempty"`
	Session              ConversationInfo `json:"session"`
	InputCommits         []string         `json:"input_commits"`
	ObservedHash         string           `json:"observed_hash"`
	Match                string           `json:"match"`
	Operation            string           `json:"operation"`
	Outcome              string           `json:"outcome"`
	CommandAddress       string           `json:"command_address"`
	ResultAddress        string           `json:"result_address"`
	RevealCommandAddress string           `json:"reveal_command_address,omitempty"`
	RevealResultAddress  string           `json:"reveal_result_address,omitempty"`
	CommandDirectory     string           `json:"command_directory,omitempty"`
	DirectoryKnown       bool             `json:"directory_known"`
}
type ChangeCoverage struct {
	SourceTextTruncated bool `json:"source_text_truncated"`
	VisibleIndex        bool `json:"visible_index"`
	HistoryComplete     bool `json:"history_complete"`
	IndexingLag         bool `json:"indexing_lag"`
	UnknownDirectory    bool `json:"unknown_directory"`
	Truncated           bool `json:"truncated"`
	UnavailableSources  int  `json:"unavailable_sources,omitempty"`
}

func NormalizeChange(q ChangeQuery, f Filters) (ChangeQuery, error) {
	if strings.TrimSpace(f.Repo) == "" {
		return q, fmt.Errorf("%w: repo is required", ErrBadRequest)
	}
	if len(q.Commits) == 0 || len(q.Commits) > 100 {
		return q, fmt.Errorf("%w: supply 1 to 100 full commit IDs", ErrBadRequest)
	}
	q.Commits = slices.Clone(q.Commits)
	for i, h := range q.Commits {
		h = strings.ToLower(h)
		if len(h) != 40 && len(h) != 64 {
			return q, fmt.Errorf("%w: commit IDs must contain 40 or 64 hexadecimal characters", ErrBadRequest)
		}
		if _, err := hex.DecodeString(h); err != nil {
			return q, fmt.Errorf("%w: invalid commit ID", ErrBadRequest)
		}
		q.Commits[i] = h
	}
	slices.Sort(q.Commits)
	q.Commits = slices.Compact(q.Commits)
	if q.Limit < 0 || q.Limit > 100 {
		return q, fmt.Errorf("%w: limit must be between 1 and 100", ErrBadRequest)
	}
	if q.Limit == 0 {
		q.Limit = 20
	}
	return q, nil
}
func (q ChangeQuery) Values(v url.Values) {
	v["commit"] = slices.Clone(q.Commits)
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	if q.Limit != 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
}
func ParseChangeQuery(v url.Values) (ChangeQuery, error) {
	q := ChangeQuery{Commits: v["commit"], Cursor: v.Get("cursor")}
	if len(q.Commits) == 0 {
		q.Commits = v["commits"]
	}
	if n := v.Get("limit"); n != "" {
		var err error
		q.Limit, err = strconv.Atoi(n)
		if err != nil {
			return q, fmt.Errorf("%w: bad limit", ErrBadRequest)
		}
	}
	return q, nil
}

// ChangeCursor contains only index and query identities, never evidence text.
type ChangeCursor struct {
	Scope    string `json:"s"`
	IndexID  string `json:"i"`
	Revision int64  `json:"r"`
	Cutoff   int64  `json:"c"`
	After    int64  `json:"a"`
}

func ChangeScope(q ChangeQuery, f Filters) string {
	q.Cursor = ""
	q.Limit = 0
	b, _ := json.Marshal(struct {
		Q                   ChangeQuery
		F                   Filters
		Owner, CallerDevice string
	}{q, f, f.Owner, f.CallerDevice})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func EncodeChangeCursor(c ChangeCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func ParseChangeCursor(s string) (ChangeCursor, error) {
	var c ChangeCursor
	if len(s) > 2048 {
		return c, fmt.Errorf("%w: invalid change cursor", ErrBadRequest)
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || c.Scope == "" || c.IndexID == "" || c.After < 0 || c.Cutoff < c.After || c.Revision < 0 {
		return c, fmt.Errorf("%w: invalid change cursor", ErrBadRequest)
	}
	return c, nil
}

// ChangeResolvedScope binds pagination to a backend's effective repository and
// visibility filters in addition to the public query.
func ChangeResolvedScope(scope string, resolved any) string {
	b, _ := json.Marshal(struct {
		Scope    string
		Resolved any
	}{scope, resolved})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

const ChangePageBudget = 60 << 10

// BoundedChangeMatch omits summary bodies and bounds descriptive metadata.
// Evidence IDs and addresses remain exact.
func BoundedChangeMatch(m ChangeMatch) (ChangeMatch, int) {
	c := &m.Session
	c.Digest = nil
	cut := func(s string) string { return ClipAround(s, 0, 512) }
	c.Title = cut(c.Title)
	c.Cwd = cut(c.Cwd)
	c.Repo = cut(c.Repo)
	c.RepoIdentity = cut(c.RepoIdentity)
	c.Worktree = cut(c.Worktree)
	c.Device = cut(c.Device)
	c.User = cut(c.User)
	c.Branches = slices.Clone(c.Branches[:min(len(c.Branches), 16)])
	for i := range c.Branches {
		c.Branches[i] = cut(c.Branches[i])
	}
	m.CommandDirectory = cut(m.CommandDirectory)
	b, _ := json.Marshal(m)
	return m, len(b) + 1
}

// SignChangeCursor prevents caller-selected ingestion cutoffs and revisions.
func SignChangeCursor(c ChangeCursor, secret []byte) string {
	payload := EncodeChangeCursor(c)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func VerifyChangeCursor(token string, secret []byte) (ChangeCursor, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 2048 {
		return ChangeCursor{}, fmt.Errorf("%w: invalid change cursor", ErrBadRequest)
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return ChangeCursor{}, fmt.Errorf("%w: invalid change cursor signature", ErrBadRequest)
	}
	return ParseChangeCursor(payload)
}

// ChangeCursorIndex reads an untrusted instance ID only to detect a rebuilt
// index. A mismatch may return an empty restart response. It must never grant
// access to results; matching instances still require VerifyChangeCursor.
func ChangeCursorIndex(token string) (string, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 2048 {
		return "", fmt.Errorf("%w: invalid change cursor", ErrBadRequest)
	}
	signature, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(signature) != sha256.Size {
		return "", fmt.Errorf("%w: invalid change cursor", ErrBadRequest)
	}
	c, err := ParseChangeCursor(payload)
	if err != nil {
		return "", err
	}
	return c.IndexID, nil
}
