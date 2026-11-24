//go:build linux

package bridge

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Run watches the mount table (poll on /proc/self/mountinfo) and, on every rekordbox stick, the
// directory of Engine's library and its m.db (inotify), until ctx ends. It blocks in one
// epoll_wait: no polling, nothing at all while Engine writes nothing.
func (w *CueWatcher) Run(ctx context.Context) error {
	ep, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(ep)
	in, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(in)
	wake, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(wake)
	mi, err := unix.Open("/proc/self/mountinfo", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(mi)
	for _, r := range []struct {
		fd int
		ev uint32
	}{{mi, unix.EPOLLPRI | unix.EPOLLERR}, {in, unix.EPOLLIN}, {wake, unix.EPOLLIN}} {
		if err = unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, r.fd, &unix.EpollEvent{Events: r.ev, Fd: int32(r.fd)}); err != nil {
			return err
		}
	}
	w.mu.Lock()
	w.wakeFn = func() {
		var one [8]byte
		binary.NativeEndian.PutUint64(one[:], 1)
		_, _ = unix.Write(wake, one[:])
	}
	w.mu.Unlock()
	drained := make([]byte, 8)
	stop := context.AfterFunc(ctx, w.wake)
	defer stop()

	type watch struct {
		mount string
		file  bool // m.db itself, else its directory
	}
	wds := map[int]watch{}
	byMount := map[string][]int{}
	unwatch := func(m string) {
		for _, wd := range byMount[m] {
			_, _ = unix.InotifyRmWatch(in, uint32(wd))
			delete(wds, wd)
		}
		delete(byMount, m)
	}
	watchFile := func(m string) {
		wd, err := unix.InotifyAddWatch(in, filepath.Join(m, "Engine Library", "Database2", "m.db"), unix.IN_MODIFY)
		if err == nil {
			if _, ok := wds[wd]; !ok {
				byMount[m] = append(byMount[m], wd)
			}
			wds[wd] = watch{m, true}
		}
	}
	rescan := func() {
		readAll(mi)
		added, removed := w.rescan()
		for _, m := range removed {
			unwatch(m)
		}
		for _, m := range added {
			dir := filepath.Join(m, "Engine Library", "Database2")
			if wd, err := unix.InotifyAddWatch(in, dir, unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_ONLYDIR); err == nil {
				wds[wd] = watch{m, false}
				byMount[m] = append(byMount[m], wd)
			}
			watchFile(m)
		}
	}
	rescan()
	defer func() {
		w.mu.Lock()
		var all []string
		for m := range w.sticks {
			all = append(all, m)
		}
		w.wakeFn = nil
		w.mu.Unlock()
		for _, m := range all {
			w.detach(m)
		}
	}()

	evs := make([]unix.EpollEvent, 8)
	buf := make([]byte, 64*1024)
	for {
		timeout := -1
		if next := w.startDue(ctx, time.Now()); !next.IsZero() {
			timeout = int(time.Until(next)/time.Millisecond) + 1
		}
		if ctx.Err() != nil {
			return nil
		}
		n, err := unix.EpollWait(ep, evs, timeout)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		now := time.Now()
		for _, e := range evs[:max(n, 0)] {
			switch int(e.Fd) {
			case mi:
				rescan()
			case wake:
				_, _ = unix.Read(wake, drained)
			case in:
				gone := false
				for {
					k, err := unix.Read(in, buf)
					if k <= 0 || err != nil {
						break
					}
					for off := 0; off+unix.SizeofInotifyEvent <= k; {
						wd := int(int32(binary.NativeEndian.Uint32(buf[off:])))
						mask := binary.NativeEndian.Uint32(buf[off+4:])
						nl := int(binary.NativeEndian.Uint32(buf[off+12:]))
						name := ""
						if nl > 0 && off+unix.SizeofInotifyEvent+nl <= k {
							name = string(trimNUL(buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+nl]))
						}
						off += unix.SizeofInotifyEvent + nl
						ref, ok := wds[wd]
						switch {
						case !ok:
						case mask&unix.IN_IGNORED != 0:
							delete(wds, wd) // unmounted, or m.db replaced: the mount table says which
							gone = true
						case ref.file:
							w.changed(ref.mount, now)
						case name == "m.db":
							watchFile(ref.mount) // a new m.db (a publication)
							w.changed(ref.mount, now)
						}
					}
				}
				if gone {
					rescan()
				}
			}
		}
	}
}

func readAll(fd int) {
	_, _ = unix.Seek(fd, 0, 0)
	b := make([]byte, 16*1024)
	for {
		if n, err := unix.Read(fd, b); n <= 0 || err != nil {
			return
		}
	}
}

func trimNUL(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}
