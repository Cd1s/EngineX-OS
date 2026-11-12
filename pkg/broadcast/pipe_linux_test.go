//go:build linux

package broadcast

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The feed FIFO holds more than a second of audio, so a stall of the capture loop
// shorter than that loses nothing (the default 64 KiB is 0.34 s).
func TestFeedPipeHoldsMoreThanASecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.pcm")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	size := func() (n int) {
		rc, err := r.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Control(func(fd uintptr) { n, _ = unix.FcntlInt(fd, unix.F_GETPIPE_SZ, 0) })
		return n
	}
	before := size()
	growPipe(r)
	if got := size(); got < SampleRate*4 {
		t.Fatalf("the feed pipe holds %d bytes (%.2f s; %d before), want more than a second", got, float64(got)/(SampleRate*4), before)
	}
}
