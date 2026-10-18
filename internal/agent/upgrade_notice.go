package agent

import (
	"context"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/upgrade"
)

var upgradeLogMu sync.Mutex

// releaseCheckDelay is how long after start the first release check
// waits: a device that just started has indexing to do first.
const releaseCheckDelay = time.Minute

// latestKnown is the newest release this agent has heard of: the server's
// version or the published release, whichever is newer.
func (a *Agent) latestKnown() string {
	latest := ""
	if a.cfg.ServerVersion != nil {
		latest = a.cfg.ServerVersion()
	}
	upgradeLogMu.Lock()
	published := a.latestRelease
	upgradeLogMu.Unlock()
	if published != "" && (latest == "" || upgrade.Newer(latest, published)) {
		latest = published
	}
	return latest
}

// upgradeNotice is the upgrade line for this device, "" when no newer
// release than cfg.Version is known. The first time a given version earns
// a notice, the agent logs it.
func (a *Agent) upgradeNotice() string {
	latest := a.latestKnown()
	n := upgrade.Notice(a.cfg.Version, latest)
	if n == "" {
		return ""
	}
	upgradeLogMu.Lock()
	logged := a.upgradeLogged == latest
	a.upgradeLogged = latest
	upgradeLogMu.Unlock()
	if !logged && a.log != nil {
		a.log.Info("agent: a newer flopwire release is available", "device", a.cfg.Version, "latest", latest, "upgrade", upgrade.InstallCommand)
	}
	return n
}

// runReleaseCheck asks ReleaseCheck for the newest release after
// releaseCheckDelay and then every ReleaseEvery, until ctx ends. A failed
// check keeps the last answer.
func (a *Agent) runReleaseCheck(ctx context.Context) {
	every := a.cfg.ReleaseEvery
	if every <= 0 {
		every = 24 * time.Hour
	}
	wait := releaseCheckDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		latest, err := a.cfg.ReleaseCheck(cctx)
		cancel()
		if err != nil {
			if a.log != nil {
				a.log.Debug("agent: release check failed", "err", err)
			}
		} else {
			upgradeLogMu.Lock()
			a.latestRelease = latest
			upgradeLogMu.Unlock()
			a.upgradeNotice() // logs once when it is newer
		}
		wait = every
	}
}
