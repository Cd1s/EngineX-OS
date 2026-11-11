package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimesyncClockPath  = "/var/lib/systemd/timesync/clock"
	clockPollInterval         = 2 * time.Second
	clockPeriodicInterval     = 10 * time.Minute
	clockForwardStepThreshold = 5 * time.Second
	minSaneClockUnix          = 1735689600 // 2025-01-01T00:00:00Z
)

var (
	timesyncOwnerOnce sync.Once
	timesyncUID       = -1
	timesyncGID       = -1
)

func lookupTimesyncOwner() (int, int) {
	timesyncOwnerOnce.Do(func() {
		b, err := os.ReadFile("/etc/passwd")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "systemd-timesync:") {
				continue
			}
			fields := strings.Split(line, ":")
			if len(fields) < 4 {
				return
			}
			uid, errU := strconv.Atoi(fields[2])
			gid, errG := strconv.Atoi(fields[3])
			if errU == nil && errG == nil {
				timesyncUID, timesyncGID = uid, gid
			}
			return
		}
	})
	return timesyncUID, timesyncGID
}

func chownTimesync(path string) {
	if uid, gid := lookupTimesyncOwner(); uid >= 0 && gid >= 0 {
		_ = os.Chown(path, uid, gid)
	}
}

func defaultTimesyncClockFile() string {
	if p := strings.TrimSpace(os.Getenv("ENGINEX_TIMESYNC_CLOCK")); p != "" {
		return p
	}
	for _, marker := range []string{"/data/enginex", "/data/sc5000-custom"} {
		if _, err := os.Stat(marker); err == nil {
			return defaultTimesyncClockPath
		}
	}
	return ""
}

func ensureNotSymlink(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink path: %s", path)
	}
	return nil
}

// advanceClockFile updates path's mtime to wall if wall is strictly newer than
// the existing mtime, creating the file atomically when absent. It never moves
// mtime backwards and refuses symlinks.
func advanceClockFile(path string, wall time.Time) (bool, error) {
	wall = wall.Round(0)
	if path == "" || wall.Unix() < minSaneClockUnix {
		return false, nil
	}
	dir := filepath.Dir(path)
	for _, p := range []string{filepath.Dir(dir), dir} {
		if err := ensureNotSymlink(p); err != nil {
			return false, err
		}
	}
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, fmt.Errorf("refusing non-regular clock file: %s", path)
		}
		if !wall.After(info.ModTime()) {
			return false, nil
		}
		if err := os.Chtimes(path, wall, wall); err != nil {
			return false, err
		}
		return true, nil
	}
	if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	if err := ensureNotSymlink(dir); err != nil {
		return false, err
	}
	chownTimesync(dir)
	tmp, err := os.CreateTemp(dir, ".clock-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return false, err
	}
	chownTimesync(tmpName)
	if err := os.Chtimes(tmpName, wall, wall); err != nil {
		return false, err
	}
	if err := ensureNotSymlink(path); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, err
	}
	return true, nil
}

type clockTracker struct {
	path            string
	periodic        time.Duration
	stepThreshold   time.Duration
	initialized     bool
	lastWall        time.Time
	lastMono        time.Duration
	lastPersistMono time.Duration
}

func newClockTracker(path string) *clockTracker {
	return &clockTracker{
		path:          path,
		periodic:      clockPeriodicInterval,
		stepThreshold: clockForwardStepThreshold,
	}
}

func (t *clockTracker) step(wall time.Time, mono time.Duration) (bool, error) {
	wall = wall.Round(0)
	if !t.initialized {
		t.initialized = true
		t.lastWall = wall
		t.lastMono = mono
		t.lastPersistMono = mono
		return advanceClockFile(t.path, wall)
	}
	monoDelta := mono - t.lastMono
	if monoDelta < 0 {
		monoDelta = 0
	}
	wallDelta := wall.Sub(t.lastWall)
	t.lastWall = wall
	t.lastMono = mono

	forwardStep := wallDelta-monoDelta >= t.stepThreshold
	duePeriodic := mono-t.lastPersistMono >= t.periodic
	if !forwardStep && !duePeriodic {
		return false, nil
	}
	updated, err := advanceClockFile(t.path, wall)
	if err == nil {
		t.lastPersistMono = mono
	}
	return updated, err
}

// watchPersistentClock advances /var/lib/systemd/timesync/clock every 10 minutes
// and immediately on a forward wall-clock step (manual SetTime or NTP jump), so
// offline sessions retain elapsed time without touching any Engine thread.
func watchPersistentClock(ctx context.Context) {
	path := defaultTimesyncClockFile()
	if path == "" {
		return
	}
	tracker := newClockTracker(path)
	start := time.Now()
	_, _ = tracker.step(start, 0)

	ticker := time.NewTicker(clockPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_, _ = tracker.step(now, now.Sub(start))
		}
	}
}
