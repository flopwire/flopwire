package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

type leaseRecorder struct {
	*recorder
	authorize      func(context.Context, devicesync.SourceSpec) (*devicesync.CaptureAuthorization, error)
	evidence       devicesync.CaptureEvidence
	beforeEvidence func()
}

func (r *leaseRecorder) SetAuthorize(fn func(context.Context, devicesync.SourceSpec) (*devicesync.CaptureAuthorization, error)) {
	r.authorize = fn
}
func (r *leaseRecorder) CaptureEvidenceForOrigin(context.Context, transcript.Agent, string, string, ...string) (devicesync.CaptureEvidence, error) {
	if r.beforeEvidence != nil {
		r.beforeEvidence()
	}
	return r.evidence, nil
}

func newCoworkLeaseFixture(t *testing.T) (*fixture, *leaseRecorder, *coworkPolicyRecorder, string) {
	f := newCoworkFixture(t)
	r := &leaseRecorder{recorder: newRecorder()}
	p := &coworkPolicyRecorder{alter: func(ack *syncproto.PolicyPlacementsResponse) { ack.Allowed = true }}
	f.cfg.Sync, f.cfg.CoworkPolicy = r, p
	f.restart()
	coworkMetadata(t, f, []string{filepath.Join(f.home, "selected")}, nil, nil)
	path := coworkTranscript(t, f)
	f.once()
	return f, r, p, path
}

func TestCoworkLeaseUsesIndexedBoundAndChangedGrantHolds(t *testing.T) {
	f, r, p, path := newCoworkLeaseFixture(t)
	sp, ok := r.spec(path)
	if !ok {
		t.Fatal("known mapped evidence was not scheduled")
	}
	if r.authorize == nil {
		t.Fatal("scheduler authorization not installed")
	}
	auth, err := r.authorize(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Release()
	id, err := transcript.StatIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Proof.Identity != id || auth.Proof.Offset <= 0 || auth.Proof.Offset > id.Size || auth.Proof.PolicyRequestDigest == "" {
		t.Fatalf("proof=%+v", auth.Proof)
	}
	fh, err := auth.Open(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	fh.Close()
	last := p.requests[len(p.requests)-1]
	if last.EvidenceScope != syncproto.EvidenceMapped || !last.CurrentMappingKnown {
		t.Fatalf("request=%+v", last)
	}
	coworkMetadata(t, f, []string{filepath.Join(f.home, "different")}, nil, nil)
	if err := auth.Check(ctx); err == nil {
		t.Fatal("changed app grant retained the lease")
	}
}

func TestCoworkLeaseLegacyCaptureTaintsBeforeAcknowledgement(t *testing.T) {
	f, r, p, path := newCoworkLeaseFixture(t)
	sp, _ := r.spec(path)
	r.evidence.Unproven = true
	_, err := r.authorize(ctx, sp)
	if err == nil {
		t.Fatal("unproved historical capture was authorized")
	}
	stored, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
	if stored.how != localindex.PlacedByCoworkUnknown {
		t.Fatal("capture uncertainty was not durable")
	}
	last := p.requests[len(p.requests)-1]
	if last.EvidenceScope != syncproto.EvidenceUnmapped {
		t.Fatalf("server saw mapped history: %+v", last)
	}
	// Restart cannot recertify the old capture from current known app grants.
	f.restart()
	if f.a.coworkMaySchedule(f.a.policy(), &target{path: path, kind: kindTranscript, src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: coworkNativeID}}) {
		t.Fatal("restart forgot historical hold")
	}
}

func TestCoworkLeaseRejectsIdentityMismatchAndContainedSymlink(t *testing.T) {
	_, r, p, path := newCoworkLeaseFixture(t)
	sp, _ := r.spec(path)
	wrong := sp
	wrong.SessionKey = "another-native-session"
	before := len(p.requests)
	if _, err := r.authorize(ctx, wrong); err == nil {
		t.Fatal("changed session identity authorized")
	}
	if len(p.requests) != before {
		t.Fatal("identity mismatch reached policy RPC")
	}
	auth, err := r.authorize(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Release()
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte("synthetic unrelated bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if fh, err := auth.Open(ctx, sp); err == nil {
		fh.Close()
		t.Fatal("source symlink escaped app boundary")
	}
}

func TestCoworkLeaseUnreachablePolicyDoesNotLeaveGateHeld(t *testing.T) {
	_, r, p, path := newCoworkLeaseFixture(t)
	sp, _ := r.spec(path)
	p.fail = true
	if _, err := r.authorize(ctx, sp); err == nil {
		t.Fatal("unreachable server granted capture")
	}
	p.fail = false
	auth, err := r.authorize(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	auth.Release()
}

func TestCoworkLeaseRetargetedHostAliasInvalidatesCheck(t *testing.T) {
	f, r, _, path := newCoworkLeaseFixture(t)
	allowed, denied := filepath.Join(f.home, "allowed-real"), filepath.Join(f.home, "denied-real")
	for _, dir := range []string{allowed, denied} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(f.home, "selected-alias")
	if err := os.Symlink(allowed, alias); err != nil {
		t.Fatal(err)
	}
	coworkMetadata(t, f, []string{alias}, nil, nil)
	f.a.cfg.UserRuleList = []string{"deny " + denied}
	f.a.refreshPolicy(ctx, false)
	f.once()
	sp, _ := r.spec(path)
	auth, err := r.authorize(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Release()
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(denied, alias); err != nil {
		t.Fatal(err)
	}
	if err := auth.Check(ctx); err == nil {
		t.Fatal("changed physical host policy scope retained lease")
	}
}

func TestCoworkLeaseMappingChangesBeforeFirstSignatureHold(t *testing.T) {
	for _, change := range []string{"unknown", "new denied folder", "physical alias"} {
		t.Run(change, func(t *testing.T) {
			f, recorder, policy, path := newCoworkLeaseFixture(t)
			denied := filepath.Join(f.home, "denied-before-signature")
			if err := os.MkdirAll(denied, 0700); err != nil {
				t.Fatal(err)
			}
			f.a.cfg.UserRuleList = []string{"deny " + denied}
			f.a.refreshPolicy(ctx, false)
			alias := filepath.Join(f.home, "baseline-alias")
			if change == "physical alias" {
				allowed := filepath.Join(f.home, "allowed-before-signature")
				if err := os.MkdirAll(allowed, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(allowed, alias); err != nil {
					t.Fatal(err)
				}
				coworkMetadata(t, f, []string{alias}, nil, nil)
				f.once()
			}
			sp, _ := recorder.spec(path)
			before := len(policy.requests)
			recorder.beforeEvidence = func() {
				switch change {
				case "unknown":
					coworkMetadata(t, f, nil, nil, nil)
				case "new denied folder":
					coworkMetadata(t, f, []string{denied}, nil, nil)
				case "physical alias":
					if err := os.Remove(alias); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(denied, alias); err != nil {
						t.Fatal(err)
					}
				}
			}
			if auth, err := recorder.authorize(ctx, sp); err == nil {
				auth.Release()
				t.Fatal("changed refresh snapshot authorized")
			}
			if len(policy.requests) != before {
				t.Fatal("unregistered changed grant reached policy acknowledgement")
			}
		})
	}
}
