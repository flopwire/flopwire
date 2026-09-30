package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/google/uuid"
)

func TestPostgresConcurrentBootstrapCreatesOneAdmin(t *testing.T) {
	p := NewPostgres(migratedPool(t), nil, "")
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	start := make(chan struct{})
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := time.Now().UTC()
			u := domain.User{ID: uuid.NewString(), Email: uuid.NewString() + "@example.test", Name: "Admin", Role: domain.RoleAdmin, IdentityType: domain.IdentityHuman, CreatedAt: now}
			c := domain.Credential{ID: uuid.NewString(), UserID: u.ID, Kind: domain.CredentialSession, TokenHash: uuid.NewString(), CreatedAt: now, Active: true}
			<-start
			errs <- p.BootstrapIdentity(context.Background(), u, c, domain.AuditEvent{ID: uuid.NewString(), ActorID: u.ID, Action: "bootstrap", Metadata: map[string]any{"i": i}, CreatedAt: now})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrConflict):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	users, err := p.ListUsers(context.Background())
	if err != nil || ok != 1 || len(users) != 1 {
		t.Fatalf("winners=%d users=%d err=%v", ok, len(users), err)
	}
	if n, err := p.CountAudit(context.Background()); err != nil || n != 1 {
		t.Fatalf("audit events=%d err=%v", n, err)
	}
}
