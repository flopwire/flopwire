package devicesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/syncproto"
)

// PolicyClient exchanges authenticated placement metadata independently of
// evidence capture and upload. Its HTTP client must carry the server TLS pin.
// Calling it does not change the scheduler's upload filter or concurrency.
type PolicyClient struct {
	Server string
	Token  string
	HTTP   *http.Client
}

// ErrPolicyCredential means no device credential was provided. Anonymous
// responses cannot establish shared-history readiness.
var ErrPolicyCredential = errors.New("devicesync: policy client device credential is required")

func (c *PolicyClient) policyJSON(ctx context.Context, method, path string, body []byte, maxResponse int64, out any) error {
	if c.HTTP == nil {
		return syncproto.ErrNoHTTPClient
	}
	if c.Token == "" {
		return ErrPolicyCredential
	}
	var input io.Reader
	if body != nil {
		input = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Server, "/")+path, input)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set(syncproto.HeaderVersion, strconv.Itoa(syncproto.Version))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("devicesync: read policy response: %w", err)
	}
	if int64(len(raw)) > maxResponse {
		return errors.New("devicesync: policy response exceeds limit")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		he := &syncproto.HTTPError{Status: res.StatusCode}
		_ = json.Unmarshal(raw, &he.Body)
		return he
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("devicesync: invalid policy response: %w", err)
	}
	return nil
}

// SupportedPolicyPlacementsVersion is the durable policy contract this client
// understands. Version zero advertises a disabled feature and is never ready.
const SupportedPolicyPlacementsVersion = 1

// ErrPolicyUnsupported means this server cannot establish safe placement
// readiness. Callers must retain their existing evidence hold on every error.
var ErrPolicyUnsupported = errors.New("devicesync: durable policy placements unsupported")

// Capabilities fetches and validates feature support. The initial client
// contract requires serial flushes; it never enables scheduler parallelism.
func (c *PolicyClient) Capabilities(ctx context.Context) (*syncproto.CapabilitiesResponse, error) {
	var out syncproto.CapabilitiesResponse
	if err := c.policyJSON(ctx, http.MethodGet, syncproto.PathCapabilities, nil, 64<<10, &out); err != nil {
		return nil, err
	}
	if out.Version != syncproto.Version || out.PolicyPlacementsVersion != SupportedPolicyPlacementsVersion || out.MaxConcurrentFlushes != 1 {
		return nil, ErrPolicyUnsupported
	}
	return &out, nil
}

// PolicyPlacements sends only identity and host policy metadata, including
// local and deny modes. It never opens evidence files or invokes capture.
// A validated capability is checked afresh so an old or unreachable server
// cannot receive metadata under an assumed durable reconciliation contract.
func (c *PolicyClient) PolicyPlacements(ctx context.Context, in *syncproto.PolicyPlacementsRequest) (*syncproto.PolicyPlacementsResponse, error) {
	if in == nil {
		return nil, errors.New("devicesync: policy placements request is required")
	}
	snapshot := *in
	if in.Placements != nil {
		snapshot.Placements = append(make([]syncproto.PolicyPlacement, 0, len(in.Placements)), in.Placements...)
	}
	if in.Sources != nil {
		snapshot.Sources = append(make([]syncproto.PolicySource, 0, len(in.Sources)), in.Sources...)
	}
	in = &snapshot
	if in.Version != SupportedPolicyPlacementsVersion {
		return nil, errors.New("devicesync: invalid policy placements request version")
	}
	switch in.EvidenceScope {
	case syncproto.EvidenceNone, syncproto.EvidenceMapped, syncproto.EvidenceUnmapped:
	default:
		return nil, errors.New("devicesync: invalid policy evidence scope")
	}
	switch in.ClientMode {
	case syncproto.ClientModeAllow, syncproto.ClientModeLocal, syncproto.ClientModeDeny:
	default:
		return nil, errors.New("devicesync: invalid policy client mode")
	}
	if in.Agent == "" || in.SessionID == "" {
		return nil, errors.New("devicesync: policy session identity is required")
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if len(raw) > syncproto.MaxPolicyPlacementsBytes {
		return nil, errors.New("devicesync: policy placements request exceeds limit")
	}
	expectedDigest, err := syncproto.PolicyPlacementsDigest(in)
	if err != nil {
		return nil, err
	}
	if _, err := c.Capabilities(ctx); err != nil {
		return nil, err
	}
	var out syncproto.PolicyPlacementsResponse
	if err := c.policyJSON(ctx, http.MethodPost, syncproto.PathPolicyPlacements, raw, syncproto.MaxPolicyPlacementsBytes, &out); err != nil {
		return nil, err
	}
	if out.RequestDigest != expectedDigest {
		return nil, errors.New("devicesync: policy acknowledgement does not match submitted batch")
	}
	if out.Version != SupportedPolicyPlacementsVersion || out.Revision <= 0 {
		return nil, errors.New("devicesync: invalid policy placements acknowledgement")
	}
	switch out.EvidenceScope {
	case syncproto.EvidenceNone, syncproto.EvidenceMapped, syncproto.EvidenceUnmapped:
	default:
		return nil, errors.New("devicesync: invalid acknowledged evidence scope")
	}
	if in.EvidenceScope == syncproto.EvidenceUnmapped && out.EvidenceScope != syncproto.EvidenceUnmapped ||
		in.EvidenceScope == syncproto.EvidenceMapped && out.EvidenceScope == syncproto.EvidenceNone {
		return nil, errors.New("devicesync: policy acknowledgement weakens evidence scope")
	}
	if out.Allowed && (!in.CurrentMappingKnown || out.EvidenceScope == syncproto.EvidenceUnmapped || in.ClientMode != syncproto.ClientModeAllow) {
		return nil, errors.New("devicesync: inconsistent policy readiness acknowledgement")
	}
	return &out, nil
}
