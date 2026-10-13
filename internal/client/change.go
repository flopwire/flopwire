package client

import (
	"context"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func (c HTTP) Change(ctx context.Context, q format.ChangeQuery, f format.Filters) (*format.Changes, error) {
	var out format.Changes
	v := f.Values()
	q.Values(v)
	err := c.do(ctx, "GET", "/v1/change?"+v.Encode(), nil, nil, &out)
	return &out, err
}
