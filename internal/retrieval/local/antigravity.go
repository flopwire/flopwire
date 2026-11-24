package local

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

func antigravityLockID(dir, path string) string {
	if filepath.Dir(path) != dir {
		return ""
	}
	id, ok := strings.CutSuffix(filepath.Base(path), ".lock")
	if !ok {
		return ""
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return ""
	}
	return id
}

func antigravityPresenceDir(brain string) string {
	dir := filepath.Join(filepath.Dir(brain), "presence")
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

// AntigravityPresence reads the conversation locks held open by this
// user's agy processes. Lock files persist after conversations end, so
// their existence does not say which conversations are currently open.
type AntigravityPresence struct {
	mu       sync.Mutex
	dir      string
	identity string
	at       time.Time
	holders  map[string]int
}

// Holders returns an immutable snapshot. A changed process set forces an
// immediate refresh. The short cache avoids spawning lsof on every agent
// presence poll and bounds lag when a running agy changes conversations.
func (p *AntigravityPresence) Holders(ctx context.Context, brain string) (map[string]int, bool) {
	dir := antigravityPresenceDir(brain)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pids, identity, ok := antigravityPIDs()
	if !ok {
		p.at = time.Time{}
		return nil, false
	}
	if len(pids) == 0 {
		p.at = time.Time{}
		return nil, true
	}
	if identity != "" && p.dir == dir && p.identity == identity && time.Since(p.at) < 5*time.Second {
		return p.holders, true
	}
	holders, ok := antigravityOpenHolders(ctx, dir, pids)
	if !ok {
		p.at = time.Time{}
		return nil, false
	}
	if identity != "" {
		_, current, ok := antigravityPIDs()
		if !ok || current != identity {
			p.at = time.Time{}
			return nil, false
		}
	}
	p.dir, p.identity, p.at, p.holders = dir, identity, time.Now(), holders
	return holders, true
}

func antigravityHoldersFromFiles(dir string, out []byte) (map[string]int, bool) {
	holders := make(map[string]int)
	var pid int
	var agy bool
	lines := bufio.NewScanner(bytes.NewReader(out))
	for lines.Scan() {
		line := lines.Text()
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			agy = false
		case 'c':
			agy = line[1:] == "agy"
		case 'n':
			if id := antigravityLockID(dir, line[1:]); agy && pid > 1 && id != "" {
				noteAntigravityHolder(holders, id, pid)
			}
		}
	}
	return holders, lines.Err() == nil
}

func noteAntigravityHolder(holders map[string]int, id string, pid int) {
	if prior, exists := holders[id]; exists && prior != pid {
		holders[id] = 0 // more than one process has this file open
		return
	}
	holders[id] = pid
}
