//go:build linux

package bridge

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCueWatcherLinuxMountinfoEpollIdleZeroWakeups(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch, stop := watchMountChanges(ctx)
	if ch == nil {
		cancel()
		t.Fatal("watchMountChanges returned nil on Linux")
	}
	defer func() {
		cancel()
		stop()
	}()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("mountinfo epoll woke spuriously with no mount table changes")
		}
	case <-time.After(250 * time.Millisecond):
	}
	cancel()
	stop()
}

func TestCueWatcherLinuxIdlePollBenchmark(t *testing.T) {
	if os.Getenv("ENGINEX_BENCH_IDLE_POLL") != "1" {
		t.Skip("set ENGINEX_BENCH_IDLE_POLL=1 to run the 10s Linux mountinfo epoll vs poll benchmark")
	}
	const dur = 10 * time.Second
	runMode := func(name string, useEvent bool) (polls int64, cpuUS int64) {
		var cnt atomic.Int64
		t.Cleanup(SetCueRunDirForTest(t.TempDir()))
		w := &CueWatcher{
			quiet:         5 * time.Second,
			retry:         15 * time.Second,
			poll:          time.Second,
			ejectDir:      cueEjectingDir,
			useMountWatch: useEvent,
			mounts: func() []string {
				cnt.Add(1)
				return mediaMounts("/nonexistent-empty-media-root")
			},
			pass: WriteBackCues,
		}
		var u0, u1 unix.Rusage
		_ = unix.Getrusage(unix.RUSAGE_SELF, &u0)
		ctx, cancel := context.WithTimeout(context.Background(), dur)
		defer cancel()
		_ = w.Run(ctx)
		_ = unix.Getrusage(unix.RUSAGE_SELF, &u1)
		us := int64(u1.Utime.Sec-u0.Utime.Sec)*1e6 + int64(u1.Utime.Usec-u0.Utime.Usec) +
			int64(u1.Stime.Sec-u0.Stime.Sec)*1e6 + int64(u1.Stime.Usec-u0.Stime.Usec)
		t.Logf("BENCH %s duration=%v polls=%d rusage_cpu_us=%d", name, dur, cnt.Load(), us)
		return cnt.Load(), us
	}
	legacyPolls, legacyUS := runMode("legacy_poll_1s", false)
	epollPolls, epollUS := runMode("linux_epoll_mountinfo", true)
	if epollPolls != 1 || legacyPolls < 9 {
		t.Fatalf("unexpected idle poll counts: epoll=%d legacy=%d (cpu_us epoll=%d legacy=%d)",
			epollPolls, legacyPolls, epollUS, legacyUS)
	}
}
