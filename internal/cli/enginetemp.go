package cli

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Engine copies each track it loads from a stream (and some local files and
// .stems) into this directory and never removes the copies: 1.3 GB of repeats
// had piled up by 2026-11-04. A new Engine run copies again rather than reuse them.
const (
	engineTempDir     = "/data/inMusicTempFiles"
	engineTempBudget  = 1 << 30 // copies kept from Engine's current run
	engineTempMinFree = 1 << 30
)

// cleanEngineTemp removes copies no process has open: all of those from earlier
// Engine runs, and the oldest of the current run while the directory exceeds
// budget or its volume has less than minFree left.
func cleanEngineTemp(dir, proc string, budget, minFree int64) (removed int, freed int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	start, open := engineState(dir, proc)
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var current []file
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		total += info.Size()
		if open[path] {
			continue
		}
		// No Engine running (start is zero) means every copy is a leftover.
		if info.ModTime().Before(start) || start.IsZero() {
			if os.Remove(path) == nil {
				removed++
				freed += info.Size()
				total -= info.Size()
			}
			continue
		}
		current = append(current, file{path, info.Size(), info.ModTime()})
	}
	short := func() bool {
		var st syscall.Statfs_t
		return minFree > 0 && syscall.Statfs(dir, &st) == nil && int64(st.Bavail)*int64(st.Bsize) < minFree
	}
	sort.Slice(current, func(i, j int) bool { return current[i].mod.Before(current[j].mod) })
	for _, f := range current {
		if total <= budget && !short() {
			break
		}
		if os.Remove(f.path) == nil {
			removed++
			freed += f.size
			total -= f.size
		}
	}
	return removed, freed
}

// engineState returns when the running Engine started (zero if none runs) and
// which files in dir any process holds open.
func engineState(dir, proc string) (time.Time, map[string]bool) {
	open := map[string]bool{}
	var start time.Time
	pids, _ := os.ReadDir(proc)
	for _, p := range pids {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		base := filepath.Join(proc, p.Name())
		fds, _ := os.ReadDir(filepath.Join(base, "fd"))
		for _, fd := range fds {
			if link, err := os.Readlink(filepath.Join(base, "fd", fd.Name())); err == nil && filepath.Dir(link) == dir {
				open[link] = true
			}
		}
		// Engine's thread is named EMain; its command line is the stable mark.
		cmd, _ := os.ReadFile(filepath.Join(base, "cmdline"))
		if exe, _, _ := bytes.Cut(cmd, []byte{0}); string(exe) == "/usr/Engine/Engine" {
			if t := startTime(proc, base); t.After(start) {
				start = t
			}
		}
	}
	return start, open
}

// startTime reads field 22 of <base>/stat (clock ticks after boot, USER_HZ 100).
// An unreadable time counts as "now", so nothing of the current run is removed.
func startTime(proc, base string) time.Time {
	stat, _ := os.ReadFile(filepath.Join(base, "stat"))
	sys, _ := os.ReadFile(filepath.Join(proc, "stat"))
	var ticks, btime int64
	if i := bytes.LastIndexByte(stat, ')'); i >= 0 {
		if f := strings.Fields(string(stat[i+1:])); len(f) > 19 {
			ticks, _ = strconv.ParseInt(f[19], 10, 64)
		}
	}
	for _, line := range strings.Split(string(sys), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			btime, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	if ticks <= 0 || btime <= 0 {
		return time.Now()
	}
	return time.Unix(btime, 0).Add(time.Duration(ticks) * time.Second / 100)
}

func watchEngineTemp(ctx context.Context) {
	for {
		if n, freed := cleanEngineTemp(engineTempDir, "/proc", engineTempBudget, engineTempMinFree); n > 0 {
			log.Printf("stream-sync: removed %d unused Engine temp copies (%d MiB)", n, freed>>20)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}
