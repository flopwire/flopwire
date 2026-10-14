package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
)

type collectionPolicyFixtureTransport func(*http.Request) (*http.Response, error)

func (f collectionPolicyFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// Shared repository expansion requires known current-authority collection
// policy. Establish it through the agent's production fetch decoder and cache
// writer, without adding policy requests to each retrieval fixture's counters.
func establishEmptyCollectionPolicy(t *testing.T, cc client.Config) {
	t.Helper()
	dir, err := configDir()
	if err != nil {
		t.Fatal(err)
	}
	_, cache := pathRuleFiles(dir)
	hc := &http.Client{Transport: collectionPolicyFixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != strings.TrimRight(cc.Server, "/")+"/v1/policy" || r.Header.Get("Authorization") != "Bearer "+cc.Token {
			return nil, fmt.Errorf("unexpected fixture policy request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"path_rules":[],"unplaceable":"local"}`)), Header: make(http.Header)}, nil
	})}
	store, err := localindex.Open(filepath.Join(t.TempDir(), "policy-bootstrap.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := agent.New(store, agent.Config{ClaudeProjects: "-", CodexHome: "-", DevinDB: "-", OpencodeDB: "-", CoworkRoot: "-", DesktopCodeRoot: "-", AdminRulesCache: cache, PolicyAuthority: collectionPolicyAuthority(cc), AdminRules: agent.FetchAdminRules(cc.Server, cc.Token, hc), Logger: slog.New(slog.DiscardHandler)})
	if err := a.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
}
