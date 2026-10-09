package devicesync

import (
	"context"
	"github.com/flopwire/flopwire/internal/syncproto"
)

// UploadConcurrency negotiates upload admission separately from placement
// readiness. Capabilities retains its legacy, exactly-serial policy contract.
func (c *PolicyClient) UploadConcurrency(ctx context.Context, requested int) (int, error) {
	client := syncproto.Client{Server: c.Server, Token: c.Token, HTTP: c.HTTP}
	return client.UploadConcurrency(ctx, requested)
}
