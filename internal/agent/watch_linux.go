//go:build linux

package agent

import (
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// watcher watches directories with inotify. A directory watch reports
// writes to the files in it by name, so appends arrive as events too; the
// path sent is the file's, flagged modify so the agent only re-stats files
// it already tracks (a write to any other file must not rescan a project).
type watcher struct {
	events chan watchEvent
	log    *slog.Logger
	fd     int

	mu   sync.Mutex
	wds  map[int]string
	dirs map[string]int
	quit chan struct{}
	done chan struct{}
}

const watchMask = unix.IN_CREATE | unix.IN_MOVED_TO | unix.IN_MODIFY | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF

func newWatcher(log *slog.Logger) *watcher {
	w := &watcher{events: make(chan watchEvent, 64), log: log, fd: -1, wds: map[int]string{}, dirs: map[string]int{},
		quit: make(chan struct{}), done: make(chan struct{})}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		log.Warn("agent: inotify unavailable; directory events off", "err", err)
		close(w.done)
		return w
	}
	w.fd = fd
	go w.loop()
	return w
}

func (w *watcher) set(dirs []string) []string {
	if w.fd < 0 {
		return nil
	}
	want := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		want[d] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for d, wd := range w.dirs {
		if !want[d] {
			unix.InotifyRmWatch(w.fd, uint32(wd))
			delete(w.dirs, d)
			delete(w.wds, wd)
		}
	}
	var added []string
	for _, d := range dirs {
		if _, ok := w.dirs[d]; ok {
			continue
		}
		wd, err := unix.InotifyAddWatch(w.fd, d, watchMask)
		if err != nil {
			continue
		}
		if old, ok := w.wds[wd]; ok && old != d {
			delete(w.dirs, old) // a reused descriptor: the old watch is gone
		}
		w.dirs[d], w.wds[wd] = wd, d
		added = append(added, d)
	}
	return added
}

// forget drops a watch the kernel removed (IN_IGNORED: the directory was
// deleted or the watch removed) or that no longer names its path
// (IN_MOVE_SELF), so the next set watches the path afresh.
func (w *watcher) forget(wd int, remove bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, ok := w.wds[wd]
	if !ok {
		return
	}
	if remove {
		unix.InotifyRmWatch(w.fd, uint32(wd))
	}
	delete(w.wds, wd)
	if w.dirs[d] == wd {
		delete(w.dirs, d)
	}
}

func (w *watcher) loop() {
	defer close(w.done)
	buf := make([]byte, 64<<10)
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	for {
		select {
		case <-w.quit:
			return
		default:
		}
		n, err := unix.Poll(fds, 250)
		if errors.Is(err, unix.EINTR) {
			continue
		} else if err != nil {
			w.log.Warn("agent: inotify poll failed; directory events off", "err", err)
			return
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL|unix.POLLHUP) != 0 {
			w.log.Warn("agent: inotify descriptor failed; directory events off", "revents", fds[0].Revents)
			return
		}
		n, err = unix.Read(w.fd, buf)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		} else if err != nil || n <= 0 {
			w.log.Warn("agent: inotify read failed; directory events off", "err", err)
			return
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			name := ""
			if ev.Len > 0 {
				raw := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+int(ev.Len)]
				for i, c := range raw {
					if c == 0 {
						raw = raw[:i]
						break
					}
				}
				name = string(raw)
			}
			off += unix.SizeofInotifyEvent + int(ev.Len)
			switch {
			case ev.Mask&unix.IN_Q_OVERFLOW != 0:
				continue // events were lost; the sweep catches up
			case ev.Mask&unix.IN_IGNORED != 0:
				w.forget(int(ev.Wd), false)
				continue
			case ev.Mask&unix.IN_MOVE_SELF != 0:
				w.forget(int(ev.Wd), true)
				continue
			case ev.Mask&unix.IN_DELETE_SELF != 0:
				continue // IN_IGNORED follows
			}
			w.mu.Lock()
			d, ok := w.wds[int(ev.Wd)]
			w.mu.Unlock()
			if !ok {
				continue
			}
			e := watchEvent{path: d}
			if name != "" && ev.Mask&unix.IN_ISDIR == 0 {
				e.path = filepath.Join(d, name)
				e.modify = ev.Mask&unix.IN_MODIFY != 0
			}
			select {
			case w.events <- e:
			default: // full: the sweep catches up
			}
		}
	}
}

func (w *watcher) close() {
	if w.fd < 0 {
		return
	}
	close(w.quit)
	<-w.done
	unix.Close(w.fd)
}
