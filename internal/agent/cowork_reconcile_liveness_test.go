package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

type deadlineCoworkPolicy struct {
	coworkPolicyRecorder
	remaining   time.Duration
	hasDeadline bool
}

func (p *deadlineCoworkPolicy) PolicyPlacements(ctx context.Context, req *syncproto.PolicyPlacementsRequest) (*syncproto.PolicyPlacementsResponse, error) {
	deadline, ok := ctx.Deadline()
	p.hasDeadline = ok
	p.remaining = time.Until(deadline)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.coworkPolicyRecorder.PolicyPlacements(ctx, req)
}

func TestCoworkReconcilePreparationDoesNotConsumeRPCDeadline(t *testing.T) {
	f := newCoworkFixture(t)
	p := &deadlineCoworkPolicy{}
	f.a.cfg.CoworkPolicy = p
	key := placeKey{transcript.AgentClaude, coworkNativeID}
	if err := f.a.saveCoworkPlace(ctx, key, placed{how: localindex.PlacedByCowork}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan bool, 1), make(chan struct{})
	f.a.cfg.RecoveredPolicySources = func(prep context.Context, _ string) ([]syncproto.PolicyRecoverySource, error) {
		_, hasDeadline := prep.Deadline()
		entered <- hasDeadline
		<-release // Represents queued local proof work, before request creation.
		return nil, nil
	}
	done := make(chan struct{})
	go func() {
		f.a.reconcileCoworkBounded(context.Background())
		close(done)
	}()
	select {
	case hasDeadline := <-entered:
		close(release)
		if hasDeadline {
			<-done
			t.Fatal("network deadline began before local proof preparation completed")
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("preparation did not start")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reconciliation did not finish")
	}
	if len(p.requests) != 1 || p.requests[0].SessionID != coworkNativeID {
		t.Fatalf("metadata requests=%+v", p.requests)
	}
	if !p.hasDeadline || p.remaining < 9*time.Second || p.remaining > 10*time.Second {
		t.Fatalf("POST did not receive its fresh 10-second deadline: present=%v remaining=%s", p.hasDeadline, p.remaining)
	}
}

func TestCoworkReconcileCancellationAfterLocalWaitDoesNotDispatch(t *testing.T) {
	for _, wait := range []string{"capture gate", "local proof"} {
		t.Run(wait, func(t *testing.T) {
			f := newCoworkFixture(t)
			p := &coworkPolicyRecorder{}
			f.a.cfg.CoworkPolicy = p
			key := placeKey{transcript.AgentClaude, coworkNativeID}
			if err := f.a.saveCoworkPlace(ctx, key, placed{how: localindex.PlacedByCowork}); err != nil {
				t.Fatal(err)
			}
			pass, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			if wait == "capture gate" {
				f.a.captureScopeMu.Lock()
			} else {
				f.a.cfg.RecoveredPolicySources = func(context.Context, string) ([]syncproto.PolicyRecoverySource, error) {
					close(entered)
					<-release
					return nil, nil // A queued SQLite read can complete despite cancellation.
				}
			}
			done := make(chan error, 1)
			go func() { done <- f.a.reconcileCoworkKey(pass, key) }()
			if wait == "capture gate" {
				cancel()
				f.a.captureScopeMu.Unlock()
			} else {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					close(release)
					t.Fatal("local proof did not start")
				}
				cancel()
				close(release)
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled reconciliation returned %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("canceled reconciliation did not finish after local wait released")
			}
			if len(p.requests) != 0 {
				t.Fatalf("canceled local work dispatched %d metadata requests", len(p.requests))
			}
		})
	}
}
