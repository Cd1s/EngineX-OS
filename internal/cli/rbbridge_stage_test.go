package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestStageName(t *testing.T) {
	for in, want := range map[string]string{
		"CDJ-3000":                  "CDJ-3000 2 USB",
		"../../etc\x00":             "etc 2 USB",
		"":                          "Player 2 USB",
		"a very long player name x": "a very long player name 2 USB",
	} {
		if got := stageName(in, 2, 3); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := stageName("XDJ", 1, 2); got != "XDJ 1 SD" {
		t.Error(got)
	}
}

// stageDrive is a drive of size bytes for the stager: other bytes of it are the DJ's own
// files, and what lies in its .prolink-cache is counted as it is on disk (sparse files).
type stageDrive struct {
	root        string
	size, other int64
}

func (d stageDrive) cache() string { return filepath.Join(d.root, prolink.StagedDir) }

func (d stageDrive) statfs(_ string, fs *syscall.Statfs_t) error {
	fs.Bsize = 1
	fs.Blocks = uint64(d.size)
	fs.Bavail = uint64(max(0, d.size-d.other-dirSize(d.cache())))
	return nil
}

// staged puts a player's library of size bytes into the drive's cache: finished (its
// export.pdb written age ago) or cut short before that.
func (d stageDrive) staged(t *testing.T, name string, size int64, finished bool, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(d.cache(), name)
	stageFile(t, filepath.Join(dir, "Contents/a/track.aiff"), size, time.Time{})
	if finished {
		stageFile(t, prolink.StagedExport(dir), 4096, time.Now().Add(-age))
	}
	return dir
}

func stageFile(t *testing.T, path string, size int64, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err == nil {
		err = f.Truncate(size)
		f.Close()
	}
	if err == nil && !at.IsZero() {
		err = os.Chtimes(path, at, at)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// stageSee lets a stager for the drive see a player once and returns what each of its two
// slots' copies was given, USB first. A copy leaves copies bytes in its folder.
func stageSee(t *testing.T, d stageDrive, player prolink.Device, copies int64, inUse func(string) bool) []staging252Copy {
	t.Helper()
	var mu sync.Mutex
	var calls []staging252Copy
	s := &stager{last: map[string]time.Time{}, mounts: func() []string { return []string{d.root} }, statfs: d.statfs, inUse: inUse,
		copy: func(_ context.Context, c *prolink.NFSClient, export, dst string, budget int64) (int, error) {
			mu.Lock()
			calls = append(calls, staging252Copy{c.IP.String(), export, dst, budget})
			mu.Unlock()
			if copies > 0 {
				stageFile(t, filepath.Join(dst, "Contents/new/track.aiff"), copies, time.Time{})
				stageFile(t, prolink.StagedExport(dst), 4096, time.Now())
			}
			return 0, nil
		}}
	if s.inUse == nil {
		s.inUse = func(string) bool { return false }
	}
	s.observe(t.Context(), player)
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		s.mu.Lock()
		n := len(s.running)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("staging did not finish")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return calls
}

const stageGiB = int64(1) << 30

var stageCDJ = prolink.Device{Address: "192.0.2.1", Number: 1, Name: "CDJ-3000"}

// Left deck, 2026-11-14: an SD card of 58.6 GiB held 50 GiB of staged libraries and had
// 1.97 GiB free, the 2 GiB the stager left; a press on Stems said "not enough space
// available on SD". Each slot of each player had its own 32 GiB.
func TestStagedLibrariesShareOneBudgetOfTheDrive(t *testing.T) {
	for name, c := range map[string]struct {
		size, other, copies int64
		want                []int64
	}{
		"a tenth of the drive for both slots together": {64 * stageGiB, 20 * stageGiB, 5 * stageGiB, []int64{64 * stageGiB / 10, 64*stageGiB/10 - 5*stageGiB - 4096}},
		"never more than 8 GiB on a large drive":       {512 * stageGiB, 100 * stageGiB, 0, []int64{stageMax, stageMax}},
		"a tenth of the drive stays free":              {64 * stageGiB, 64*stageGiB - 64*stageGiB/10 - 1<<20, 0, []int64{1 << 20, 1 << 20}},
		"at least 4 GiB stay free on a small drive":    {16 * stageGiB, 12*stageGiB - 1<<20, 0, []int64{1 << 20, 1 << 20}},
		"no room left": {64 * stageGiB, 64*stageGiB - 64*stageGiB/10, 0, nil},
	} {
		d := stageDrive{root: t.TempDir(), size: c.size, other: c.other}
		calls := stageSee(t, d, stageCDJ, c.copies, nil)
		var got []int64
		for _, call := range calls {
			got = append(got, call.budget)
		}
		if len(got) != len(c.want) || (len(got) == 2 && (got[0] != c.want[0] || got[1] != c.want[1])) {
			t.Errorf("%s: the two slots may copy %v bytes, want %v", name, got, c.want)
		}
	}
	// A folder's own bytes are not counted against it twice: Stage counts them itself.
	d := stageDrive{root: t.TempDir(), size: 64 * stageGiB, other: 20 * stageGiB}
	own := d.staged(t, "CDJ-3000 1 USB", 2*stageGiB, true, time.Hour)
	if calls := stageSee(t, d, stageCDJ, 0, nil); len(calls) != 2 || calls[0].dst != own || calls[0].budget != 64*stageGiB/10 {
		t.Errorf("a library copied before goes on within the drive's budget: %+v", calls)
	}
}

// The same card: one of the two libraries on it had no export.pdb, its copy was never
// finished and no import could ever use its 23 GiB; three .fetch- files lay in the other.
func TestStagingRemovesWhatNoImportCanUse(t *testing.T) {
	d := stageDrive{root: t.TempDir(), size: 64 * stageGiB, other: 20 * stageGiB}
	gone := d.staged(t, "XDJ 2 USB", stageGiB, false, 0)
	kept := d.staged(t, "XDJ 3 USB", stageGiB, true, time.Hour)
	leftover := filepath.Join(kept, "Contents/a/.fetch-123")
	stageFile(t, leftover, 1<<20, time.Time{})
	mine := d.staged(t, "CDJ-3000 1 USB", stageGiB, false, 0) // this player's own, cut short: the copy goes on
	calls := stageSee(t, d, stageCDJ, 0, nil)
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("a library without its export.pdb and with no copy running is still there: %v", err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("a download cut short is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kept, "Contents/a/track.aiff")); err != nil {
		t.Errorf("a finished library lost a track: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mine, "Contents/a/track.aiff")); err != nil || len(calls) != 2 || calls[0].dst != mine {
		t.Errorf("the library being copied was not kept to go on from: %v %+v", err, calls)
	}
}

// A player that is gone kept its library for good. One not copied for 14 days gives it up,
// the longest gone first, but only while the drive's budget has no room, never one a deck
// has a file open in, and never a player seen since.
func TestStagingDropsLongUnseenPlayersForRoom(t *testing.T) {
	day := 24 * time.Hour
	drive := func() (stageDrive, string, string, string) {
		d := stageDrive{root: t.TempDir(), size: 64 * stageGiB, other: 20 * stageGiB}
		return d, d.staged(t, "Old 1 USB", 3*stageGiB, true, 30*day), d.staged(t, "Older 2 USB", 3*stageGiB, true, 40*day), d.staged(t, "Recent 3 USB", stageGiB, true, 2*day)
	}
	there := func(dir string) bool { _, err := os.Stat(dir); return err == nil }

	d, old, older, recent := drive()
	calls := stageSee(t, d, stageCDJ, 0, nil)
	if there(older) || !there(old) || !there(recent) {
		t.Errorf("out of room: only the longest gone goes: older %v old %v recent %v", there(older), there(old), there(recent))
	}
	if want := 64*stageGiB/10 - 4*stageGiB - 2*4096; len(calls) != 2 || calls[0].budget != want {
		t.Errorf("the room it made is what may be copied: %+v, want %d", calls, want)
	}

	d, old, older, recent = drive()
	stageSee(t, d, stageCDJ, 0, func(dir string) bool { return dir == older })
	if !there(older) || there(old) || !there(recent) {
		t.Errorf("a library with a file open stays, the next goes: older %v old %v recent %v", there(older), there(old), there(recent))
	}

	d = stageDrive{root: t.TempDir(), size: 64 * stageGiB, other: 20 * stageGiB}
	old = d.staged(t, "Old 1 USB", 3*stageGiB, true, 40*day)
	if calls := stageSee(t, d, stageCDJ, 0, nil); !there(old) || len(calls) != 2 {
		t.Errorf("with room left a long unseen player keeps its library: %v %+v", there(old), calls)
	}

	d = stageDrive{root: t.TempDir(), size: 64 * stageGiB, other: 20 * stageGiB}
	recent = d.staged(t, "Recent 3 USB", 7*stageGiB, true, 13*day)
	if calls := stageSee(t, d, stageCDJ, 0, nil); !there(recent) || len(calls) != 0 {
		t.Errorf("a player seen within 14 days keeps its library, and nothing is copied over the budget: %v %+v", there(recent), calls)
	}
}

// The 50 GiB on that card were "EngineX Export Beta 3 USB" and "... 4 USB": the two decks'
// own shared libraries, each copying the other's whole stick.
func TestStagingNeverCopiesADeckOfOurs(t *testing.T) {
	d := stageDrive{root: t.TempDir(), size: 64 * stageGiB, other: 20 * stageGiB}
	ours := prolink.Device{Address: "192.0.2.9", Number: 4, Name: "EngineX Export Beta"}
	if calls := stageSee(t, d, ours, 0, nil); len(calls) != 0 {
		t.Fatalf("a deck of ours was copied: %+v", calls)
	}
	if calls := stageSee(t, d, stageCDJ, 0, nil); len(calls) != 2 || !strings.HasSuffix(calls[0].dst, "CDJ-3000 1 USB") {
		t.Fatalf("a player was not copied: %+v", calls)
	}
}
