package devicesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/google/uuid"
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
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		he := &syncproto.HTTPError{Status: res.StatusCode}
		if int64(len(raw)) <= maxResponse {
			_ = json.Unmarshal(raw, &he.Body)
		}
		return he
	}
	if int64(len(raw)) > maxResponse {
		return errors.New("devicesync: policy response exceeds limit")
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
	if in.Device != nil {
		device := *in.Device
		snapshot.Device = &device
	}
	if in.Placements != nil {
		snapshot.Placements = append(make([]syncproto.PolicyPlacement, 0, len(in.Placements)), in.Placements...)
	}
	if in.Sources != nil {
		snapshot.Sources = append(make([]syncproto.PolicySource, 0, len(in.Sources)), in.Sources...)
	}
	if in.RecoverySources != nil {
		snapshot.RecoverySources = append(make([]syncproto.PolicyRecoverySource, 0, len(in.RecoverySources)), in.RecoverySources...)
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
	if in.ScopeStatus != "" && in.ScopeStatus != syncproto.ScopeLimitHeld {
		return nil, errors.New("devicesync: invalid policy scope status")
	}
	if in.Agent == "" || in.SessionID == "" {
		return nil, errors.New("devicesync: policy session identity is required")
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if len(raw) > syncproto.MaxPolicyPlacementsBytes || len(in.Placements) > 256 || len(in.Sources) > 256 || len(in.RecoverySources) > 256 {
		return nil, c.limitHeld(ctx, in)
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
		var he *syncproto.HTTPError
		if errors.As(err, &he) && he.Status == http.StatusRequestEntityTooLarge {
			return nil, c.limitHeld(ctx, in)
		}
		return nil, err
	}
	if err := validatePolicyAck(in, &out, expectedDigest); err != nil {
		return nil, err
	}
	return &out, nil
}

func validatePolicyAck(in *syncproto.PolicyPlacementsRequest, out *syncproto.PolicyPlacementsResponse, expectedDigest string) error {
	if out.RequestDigest != expectedDigest {
		return errors.New("devicesync: policy acknowledgement does not match submitted batch")
	}
	if out.Version != SupportedPolicyPlacementsVersion || out.Revision <= 0 {
		return errors.New("devicesync: invalid policy placements acknowledgement")
	}
	switch out.EvidenceScope {
	case syncproto.EvidenceNone, syncproto.EvidenceMapped, syncproto.EvidenceUnmapped:
	default:
		return errors.New("devicesync: invalid acknowledged evidence scope")
	}
	if in.EvidenceScope == syncproto.EvidenceUnmapped && out.EvidenceScope != syncproto.EvidenceUnmapped ||
		in.EvidenceScope == syncproto.EvidenceMapped && out.EvidenceScope == syncproto.EvidenceNone {
		return errors.New("devicesync: policy acknowledgement weakens evidence scope")
	}
	if out.Allowed && (in.ScopeStatus == syncproto.ScopeLimitHeld || !in.CurrentMappingKnown || out.EvidenceScope == syncproto.EvidenceUnmapped || in.ClientMode != syncproto.ClientModeAllow) {
		return errors.New("devicesync: inconsistent policy readiness acknowledgement")
	}
	return nil
}

// ErrPolicyLimitHeld reports that the complete authorization request was too
// large. A compact revocation never authorizes the original request.
var ErrPolicyLimitHeld = errors.New("devicesync: policy scope exceeds transport limits")

// PolicyLimitHeldError records whether every compact restriction was acknowledged.
// Failure leaves evidence held and preserves the underlying retryable error.
type PolicyLimitHeldError struct {
	Reconciled bool
	Cause      error
}

func (e *PolicyLimitHeldError) Error() string {
	if e.Cause != nil {
		return ErrPolicyLimitHeld.Error() + ": " + e.Cause.Error()
	}
	return ErrPolicyLimitHeld.Error()
}
func (e *PolicyLimitHeldError) Is(target error) bool { return target == ErrPolicyLimitHeld }
func (e *PolicyLimitHeldError) Unwrap() error        { return e.Cause }

var nativePolicyChildID = regexp.MustCompile(`^agent-[a-zA-Z0-9_-]{1,128}$`)

func canonicalPolicyID(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if (err == nil && parsed.String() == id) || nativePolicyChildID.MatchString(id) {
		return id, nil
	}
	return "", errors.New("devicesync: compact restriction requires a canonical native session identity")
}

func (c *PolicyClient) limitHeld(ctx context.Context, original *syncproto.PolicyPlacementsRequest) error {
	result := &PolicyLimitHeldError{}
	fail := func(err error) error { result.Cause = err; return result }
	id, err := canonicalPolicyID(original.SessionID)
	if err != nil {
		return fail(err)
	}
	parent := ""
	if original.ParentSessionID != "" {
		parent, err = canonicalPolicyID(original.ParentSessionID)
		if err != nil {
			return fail(err)
		}
	}
	if parent == id {
		return fail(errors.New("devicesync: compact restriction parent must differ from session"))
	}
	if original.Agent != "claude" {
		return fail(errors.New("devicesync: compact restriction requires native Claude identity"))
	}
	// Identity-scoped revocation retains the server's recorded home. A moved
	// or unavailable local home must not block restrictions on old copies.
	compact := &syncproto.PolicyPlacementsRequest{
		Version: original.Version, Agent: original.Agent, SessionID: id, ParentSessionID: parent,
		CurrentMappingKnown: false, EvidenceScope: original.EvidenceScope, ClientMode: original.ClientMode,
		ScopeStatus: syncproto.ScopeLimitHeld, Placements: []syncproto.PolicyPlacement{},
	}
	historyScope := original.EvidenceScope
	var revision int64
	send := func() error {
		raw, err := json.Marshal(compact)
		if err != nil {
			return err
		}
		if len(raw) >= syncproto.MaxPolicyPlacementsBytes {
			return errors.New("devicesync: compact restriction exceeds limit")
		}
		if _, err := c.Capabilities(ctx); err != nil {
			return err
		}
		digest, err := syncproto.PolicyPlacementsDigest(compact)
		if err != nil {
			return err
		}
		var out syncproto.PolicyPlacementsResponse
		if err := c.policyJSON(ctx, http.MethodPost, syncproto.PathPolicyPlacements, raw, syncproto.MaxPolicyPlacementsBytes, &out); err != nil {
			return err
		}
		if err := validatePolicyAck(compact, &out, digest); err != nil {
			return err
		}
		if out.Allowed {
			return errors.New("devicesync: compact restriction acknowledged as allowed")
		}
		if (historyScope == syncproto.EvidenceUnmapped && out.EvidenceScope != syncproto.EvidenceUnmapped) || (historyScope == syncproto.EvidenceMapped && out.EvidenceScope == syncproto.EvidenceNone) || out.Revision < revision {
			return errors.New("devicesync: compact restriction acknowledgement weakens prior history")
		}
		historyScope, revision = out.EvidenceScope, out.Revision
		return nil
	}
	// Commit the native component hold before attempting recovered-copy batches.
	if err := send(); err != nil {
		return fail(err)
	}
	// Bind every observed source, including legacy sources whose parsed native
	// identity differs. These references only extend the compact restriction.
	appendReference := func(add, undo func()) error {
		add()
		raw, err := json.Marshal(compact)
		if err != nil {
			return err
		}
		if len(compact.Sources) <= 256 && len(compact.RecoverySources) <= 256 && len(raw) < syncproto.MaxPolicyPlacementsBytes {
			return nil
		}
		undo()
		if len(compact.Sources) == 0 && len(compact.RecoverySources) == 0 {
			return errors.New("devicesync: source restriction exceeds limit")
		}
		if err := send(); err != nil {
			return err
		}
		compact.Sources, compact.RecoverySources = nil, nil
		add()
		raw, err = json.Marshal(compact)
		if err != nil {
			return err
		}
		if len(raw) >= syncproto.MaxPolicyPlacementsBytes {
			return errors.New("devicesync: source restriction exceeds limit")
		}
		return nil
	}
	for _, source := range original.Sources {
		err := appendReference(func() { compact.Sources = append(compact.Sources, source) }, func() { compact.Sources = compact.Sources[:len(compact.Sources)-1] })
		if err != nil {
			return fail(err)
		}
	}
	for _, recovery := range original.RecoverySources {
		err := appendReference(func() { compact.RecoverySources = append(compact.RecoverySources, recovery) }, func() { compact.RecoverySources = compact.RecoverySources[:len(compact.RecoverySources)-1] })
		if err != nil {
			return fail(err)
		}
	}
	if len(compact.Sources) > 0 || len(compact.RecoverySources) > 0 {
		if err := send(); err != nil {
			return fail(err)
		}
	}
	result.Reconciled = true
	return result
}
