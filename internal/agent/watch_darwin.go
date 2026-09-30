//go:build darwin

package agent

import (
	"log/slog"
	"sync"

	"golang.org/x/sys/unix"
)

// watcher watches directories with kqueue. EVFILT_VNODE on a directory
// fires NOTE_WRITE when an entry is added, removed or renamed, not when a
// file in it is appended to; appends are the fast lane's job. One
// descriptor per watched directory (O_EVTONLY, so it never blocks an
// unmount), and never one per file.
type watcher struct {
	events chan watchEvent
	log    *slog.Logger
	kq     int

	mu   sync.Mutex
	fds  map[int]string
	dirs map[string]int
	quit chan struct{}
	done chan struct{}
}

func newWatcher(log *slog.Logger) *watcher {
	w := &watcher{events: make(chan watchEvent, 64), log: log, kq: -1, fds: map[int]string{}, dirs: map[string]int{},
		quit: make(chan struct{}), done: make(chan struct{})}
	kq, err := unix.Kqueue()
	if err != nil {
		log.Warn("agent: kqueue unavailable; directory events off", "err", err)
		close(w.done)
		return w
	}
	w.kq = kq
	go w.loop()
	return w
}

// set replaces the watched directories and returns those newly added.
func (w *watcher) set(dirs []string) []string {
	if w.kq < 0 {
		return nil
	}
	want := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		want[d] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for d, fd := range w.dirs {
		if !want[d] {
			unix.Close(fd) // closing the descriptor removes its kevent
			delete(w.dirs, d)
			delete(w.fds, fd)
		}
	}
	var added []string
	for _, d := range dirs {
		if _, ok := w.dirs[d]; ok {
			continue
		}
		fd, err := unix.Open(d, unix.O_EVTONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		var kev unix.Kevent_t
		unix.SetKevent(&kev, fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR)
		kev.Fflags = unix.NOTE_WRITE | unix.NOTE_DELETE | unix.NOTE_RENAME
		if _, err := unix.Kevent(w.kq, []unix.Kevent_t{kev}, nil, nil); err != nil {
			unix.Close(fd)
			continue
		}
		w.dirs[d], w.fds[fd] = fd, d
		added = append(added, d)
	}
	return added
}

func (w *watcher) loop() {
	defer close(w.done)
	evs := make([]unix.Kevent_t, 32)
	timeout := unix.NsecToTimespec(int64(250e6))
	for {
		select {
		case <-w.quit:
			return
		default:
		}
		n, err := unix.Kevent(w.kq, nil, evs, &timeout)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			w.log.Warn("agent: kqueue", "err", err)
			return
		}
		for _, ev := range evs[:n] {
			w.mu.Lock()
			d, ok := w.fds[int(ev.Ident)]
			if ok && ev.Fflags&(unix.NOTE_DELETE|unix.NOTE_RENAME) != 0 {
				// The directory is gone or no longer at its path: drop the
				// watch so the next set watches the path afresh.
				unix.Close(int(ev.Ident))
				delete(w.fds, int(ev.Ident))
				if w.dirs[d] == int(ev.Ident) {
					delete(w.dirs, d)
				}
			}
			w.mu.Unlock()
			if !ok {
				continue
			}
			select {
			case w.events <- watchEvent{path: d}:
			default: // full: the sweep catches up
			}
		}
	}
}

func (w *watcher) close() {
	if w.kq < 0 {
		return
	}
	close(w.quit)
	<-w.done
	w.mu.Lock()
	for fd := range w.fds {
		unix.Close(fd)
	}
	w.fds, w.dirs = map[int]string{}, map[string]int{}
	w.mu.Unlock()
	unix.Close(w.kq)
}
