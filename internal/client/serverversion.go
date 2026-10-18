package client

import "sync"

// serverVersionHeader is the response header every flopwire server sets
// (api.VersionHeader).
const serverVersionHeader = "X-Flopwire-Version"

var (
	serverVersionMu sync.Mutex
	serverVersion   string
)

func recordServerVersion(v string) {
	if v == "" {
		return
	}
	serverVersionMu.Lock()
	serverVersion = v
	serverVersionMu.Unlock()
}

// ServerVersion is the version the server reported in its last response
// to this process, "" before any: every response through NewHTTPClient's
// transport records it. The device agent compares it with its own
// version (upgrade.Notice).
func ServerVersion() string {
	serverVersionMu.Lock()
	defer serverVersionMu.Unlock()
	return serverVersion
}
