//go:build linux

package broadcast

import (
	"os"

	"golang.org/x/sys/unix"
)

// feedPipeSize is the capacity asked for the feed FIFO: 1.36 s of audio instead of
// the default 64 KiB (0.34 s). The pipe is empty while the loop keeps up; after a
// stall of this process the hook's audio is still there, so the stream has no hole
// to recover from (a listener's own backlog is trimmed to its ?buffer=).
const feedPipeSize = 256 << 10

func growPipe(f *os.File) {
	if rc, err := f.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) { _, _ = unix.FcntlInt(fd, unix.F_SETPIPE_SZ, feedPipeSize) })
	}
}
