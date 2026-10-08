package main

import (
	"fmt"
	"math"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/store"
)

const (
	maxActiveFlushes       = 8
	flushNonUploadHeadroom = 16
)

func serverParseWorkers(workers int) (int, error) {
	// Match ingest.Queue's effective default before sizing the pool. A raw
	// zero or negative setting must not undercount the workers Queue starts.
	if workers <= 0 {
		workers = 4
	}
	if int64(workers) > math.MaxInt32-api.RetrievalConcurrency-1-store.PoolHeadroom {
		return 0, fmt.Errorf("parse worker configuration exceeds supported database pool capacity")
	}
	return workers, nil
}

func flushAdmissionBudget(maxConns int32, workers int) (int, error) {
	workers, err := serverParseWorkers(workers)
	if err != nil {
		return 0, err
	}
	minimum := int64(api.RetrievalConcurrency) + int64(workers) + 1 + flushNonUploadHeadroom + 1
	if int64(maxConns) < minimum {
		return 0, fmt.Errorf("database pool configuration requires at least %d connections for serial flush admission; configured maximum is %d", minimum, maxConns)
	}
	return int(min(int64(maxActiveFlushes), int64(maxConns)-minimum+1)), nil
}
