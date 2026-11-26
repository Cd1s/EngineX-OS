//go:build linux

package bridge

import (
	"context"
	"errors"
	"io"
	"sync"

	"golang.org/x/sys/unix"
)

func drainMountinfo(fd int) {
	if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
		return
	}
	var buf [4096]byte
	for {
		n, err := unix.Read(fd, buf[:])
		if n <= 0 || err != nil {
			return
		}
	}
}

// watchMountChanges monitors /proc/self/mountinfo via Linux epoll (EPOLLPRI|EPOLLERR)
// and emits a coalesced notification whenever the kernel mount table changes. Returns (nil, noop)
// if epoll or /proc/self/mountinfo setup fails so callers fall back to polling safely.
func watchMountChanges(ctx context.Context) (<-chan struct{}, func()) {
	fd, err := unix.Open("/proc/self/mountinfo", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, func() {}
	}
	drainMountinfo(fd)

	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		_ = unix.Close(fd)
		return nil, func() {}
	}
	var pipeFds [2]int
	if err := unix.Pipe2(pipeFds[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		_ = unix.Close(epfd)
		_ = unix.Close(fd)
		return nil, func() {}
	}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Events: unix.EPOLLPRI | unix.EPOLLERR,
		Fd:     int32(fd),
	}); err != nil {
		_ = unix.Close(pipeFds[0])
		_ = unix.Close(pipeFds[1])
		_ = unix.Close(epfd)
		_ = unix.Close(fd)
		return nil, func() {}
	}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, pipeFds[0], &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(pipeFds[0]),
	}); err != nil {
		_ = unix.Close(pipeFds[0])
		_ = unix.Close(pipeFds[1])
		_ = unix.Close(epfd)
		_ = unix.Close(fd)
		return nil, func() {}
	}

	ch := make(chan struct{}, 1)
	done := make(chan struct{})
	var wakeOnce sync.Once
	wakeStop := func() {
		wakeOnce.Do(func() {
			_, _ = unix.Write(pipeFds[1], []byte{1})
			_ = unix.Close(pipeFds[1])
		})
	}
	go func() {
		<-ctx.Done()
		wakeStop()
	}()
	go func() {
		defer close(done)
		defer close(ch)
		defer func() {
			_ = unix.Close(pipeFds[0])
			_ = unix.Close(epfd)
			_ = unix.Close(fd)
		}()
		var events [4]unix.EpollEvent
		for {
			n, err := unix.EpollWait(epfd, events[:], -1)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil || n <= 0 {
				return
			}
			stopNow := false
			changed := false
			for i := 0; i < n; i++ {
				if int(events[i].Fd) == pipeFds[0] {
					stopNow = true
				} else if int(events[i].Fd) == fd {
					drainMountinfo(fd)
					changed = true
				}
			}
			if stopNow {
				return
			}
			if changed {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	stop := func() {
		wakeStop()
		<-done
	}
	return ch, stop
}
