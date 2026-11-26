package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"golang.org/x/sys/unix"
)

func TestParseWritableMediaMounts(t *testing.T) {
	mounts := strings.Join([]string{
		"/dev/sda1 /media/USB1 vfat rw,relatime,utf8 0 0",
		"/dev/sdb1 /media/My\\040SD\\040Card exfat rw,noatime 0 0",
		"/dev/sdc1 /media/RO_USB vfat ro,relatime 0 0",
		// Read-only HFS+ with no native Engine Library: tmpfs view named enginex-hfs-view
		"/dev/sdd1 /media/HFS_VIEW hfsplus ro,relatime 0 0",
		"enginex-hfs-view /media/HFS_VIEW tmpfs rw,nosuid,nodev,noexec,relatime 0 0",
		"/dev/sda2 /media/HFS_VIEW/Engine\\040Library ext4 rw,relatime 0 0",
		// Read-only HFS+ with native Engine Library: top dir is ro bind, child Engine Library is rw
		"/dev/sde1 /media/HFS_NATIVE hfsplus ro,relatime 0 0",
		"/dev/sde1 /media/HFS_NATIVE hfsplus ro,relatime 0 0",
		"/dev/sda2 /media/HFS_NATIVE/Engine\\040Library ext4 rw,relatime 0 0",
		// Overlaid mount where top becomes rw after an earlier ro mount
		"/dev/sdf1 /media/REMOUNTED vfat ro,relatime 0 0",
		"/dev/sdf1 /media/REMOUNTED vfat rw,relatime 0 0",
		// Overlay or unrelated paths excluded
		"overlay /media/OVL overlay rw 0 0",
		"/dev/sda2 /data ext4 rw 0 0",
	}, "\n")

	got := parseWritableMediaMounts(strings.NewReader(mounts), "/media/")
	want := []string{"/media/USB1", "/media/My SD Card", "/media/REMOUNTED"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseWritableMediaMounts = %v, want %v", got, want)
	}
}

func TestCleanProlinkCacheRemovesExactDirAndKeepsNeighborsIntact(t *testing.T) {
	drive := t.TempDir()
	cache := filepath.Join(drive, ".prolink-cache")

	// Seed 260 files across nested directories (> 2 Readdirnames batches) to test full drain.
	var wantBytes int64
	for i := 0; i < 260; i++ {
		sub := filepath.Join(cache, fmt.Sprintf("CDJ-3000-%d", i%4), fmt.Sprintf("dir-%d", i%10))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		payload := []byte(fmt.Sprintf("track-payload-%04d", i))
		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f-%03d.dat", i)), payload, 0o644); err != nil {
			t.Fatal(err)
		}
		wantBytes += int64(len(payload))
	}

	// Neighbor files and nested identically-named subdirs MUST remain completely untouched.
	keepFiles := map[string]string{
		filepath.Join(drive, "PIONEER", "rekordbox", "export.pdb"):    "pdb-bytes",
		filepath.Join(drive, "Engine Library", "Database2", "m.db"):   "mdb-bytes",
		filepath.Join(drive, "Contents", "song.flac"):                 "audio-bytes",
		filepath.Join(drive, ".rekordbox-cache", "on-demand.dat"):     "ondemand-bytes",
		filepath.Join(drive, "nested", ".prolink-cache", "untouched"): "nested-keep",
	}
	for p, content := range keepFiles {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var logs []string
	res, err := cleanProlinkCache(context.Background(), drive, prolinkCleanupOptions{
		isBusy:     func() bool { return false },
		isEjecting: func() bool { return false },
		logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatalf("cleanProlinkCache: %v", err)
	}
	if !res.Removed || res.Stopped || res.BytesFreed != wantBytes {
		t.Fatalf("res = %+v, want Removed=true Stopped=false BytesFreed=%d", res, wantBytes)
	}
	if _, err := os.Lstat(cache); !os.IsNotExist(err) {
		t.Fatalf(".prolink-cache still exists: err=%v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], ".prolink-cache") || !strings.Contains(logs[0], fmt.Sprintf("%d bytes freed", wantBytes)) {
		t.Fatalf("unexpected journal logs: %v", logs)
	}
	for p, want := range keepFiles {
		got, err := os.ReadFile(p)
		if err != nil || string(got) != want {
			t.Fatalf("neighbor file %s modified or deleted: got=%q want=%q err=%v", p, got, want, err)
		}
	}
}

func TestCleanProlinkCacheNeverFollowsSymlinksAndSkipsReadOnly(t *testing.T) {
	// Case 1: top-level .prolink-cache is a symlink -> skipped, target intact.
	drive1 := t.TempDir()
	outside1 := filepath.Join(t.TempDir(), "outside-target")
	if err := os.MkdirAll(outside1, 0o755); err != nil {
		t.Fatal(err)
	}
	secret1 := filepath.Join(outside1, "keep.txt")
	if err := os.WriteFile(secret1, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkTop := filepath.Join(drive1, ".prolink-cache")
	if err := os.Symlink(outside1, linkTop); err != nil {
		t.Fatal(err)
	}
	res1, err := cleanProlinkCache(context.Background(), drive1, prolinkCleanupOptions{
		isBusy:     func() bool { return false },
		isEjecting: func() bool { return false },
		logf:       func(string, ...any) {},
	})
	if err != nil || res1.Removed || res1.BytesFreed != 0 {
		t.Fatalf("symlink top-level res = %+v, err = %v", res1, err)
	}
	if _, err := os.Lstat(linkTop); err != nil {
		t.Fatalf("top-level symlink unexpectedly removed: %v", err)
	}
	if b, err := os.ReadFile(secret1); err != nil || string(b) != "precious" {
		t.Fatalf("symlink target modified: b=%q err=%v", b, err)
	}

	// Case 2: symlink INSIDE .prolink-cache directory -> symlink itself unlinked, target untouched.
	drive2 := t.TempDir()
	outside2 := filepath.Join(t.TempDir(), "outside-dir")
	if err := os.MkdirAll(outside2, 0o755); err != nil {
		t.Fatal(err)
	}
	secret2 := filepath.Join(outside2, "song.mp3")
	if err := os.WriteFile(secret2, []byte("keep-audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache2 := filepath.Join(drive2, ".prolink-cache", "CDJ-1")
	if err := os.MkdirAll(cache2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside2, filepath.Join(cache2, "dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache2, "real.bin"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := cleanProlinkCache(context.Background(), drive2, prolinkCleanupOptions{
		isBusy:     func() bool { return false },
		isEjecting: func() bool { return false },
		logf:       func(string, ...any) {},
	})
	if err != nil || !res2.Removed || res2.BytesFreed != 5 {
		t.Fatalf("inner symlink res = %+v, err = %v", res2, err)
	}
	if b, err := os.ReadFile(secret2); err != nil || string(b) != "keep-audio" {
		t.Fatalf("inner symlink target modified: b=%q err=%v", b, err)
	}

	// Case 3: ST_RDONLY statfs skips without deleting anything.
	drive3 := t.TempDir()
	kept3 := filepath.Join(drive3, ".prolink-cache", "CDJ-2", "a.dat")
	if err := os.MkdirAll(filepath.Dir(kept3), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept3, []byte("ro-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	res3, err := cleanProlinkCache(context.Background(), drive3, prolinkCleanupOptions{
		isBusy:     func() bool { return false },
		isEjecting: func() bool { return false },
		statfs: func(_ int, st *unix.Statfs_t) error {
			st.Flags = stRdonlyFlag
			return nil
		},
		logf: func(string, ...any) {},
	})
	if err != nil || res3.Removed || res3.BytesFreed != 0 {
		t.Fatalf("read-only statfs res = %+v, err = %v", res3, err)
	}
	if _, err := os.Stat(kept3); err != nil {
		t.Fatalf("read-only .prolink-cache deleted: %v", err)
	}
}

func TestCleanProlinkCachePausesWhenBusyAndResumesOnNextInsert(t *testing.T) {
	drive := t.TempDir()
	cache := filepath.Join(drive, ".prolink-cache", "CDJ-3000")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if err := os.WriteFile(filepath.Join(cache, fmt.Sprintf("f%d.bin", i)), []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	busy := false
	pausedOnce := false
	checks := 0
	var logs []string
	// Become busy after checking/unlinking a couple files -> stops partway, does NOT finish!
	now := time.Unix(1_700_000_000, 0)
	present := true
	cw := &prolinkCleanupWatcher{
		handled:   map[mountDevKey]bool{},
		firstSeen: map[mountDevKey]time.Time{},
		list: func() []string {
			if !present {
				return nil
			}
			return []string{drive}
		},
		clean: func(ctx context.Context, mount string) (prolinkCleanupResult, error) {
			return cleanProlinkCache(ctx, mount, prolinkCleanupOptions{
				isBusy: func() bool {
					checks++
					if busy {
						return true
					}
					if !pausedOnce && checks >= 8 {
						busy = true
						pausedOnce = true
					}
					return busy
				},
				isEjecting: func() bool { return false },
				logf: func(format string, args ...any) {
					logs = append(logs, fmt.Sprintf(format, args...))
				},
			})
		},
		grace: 3 * time.Second,
		now:   func() time.Time { return now },
	}

	// First insert: grace wait elapses, deck becomes busy partway -> pauses without retrying while inserted.
	cw.step(context.Background())
	now = now.Add(4 * time.Second)
	cw.step(context.Background())
	if _, err := os.Stat(filepath.Join(drive, ".prolink-cache")); err != nil {
		t.Fatalf("expected partial .prolink-cache to remain after busy pause: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "paused removing leftover .prolink-cache") {
		t.Fatalf("expected 1 paused log line, got %v", logs)
	}
	prevChecks := checks
	now = now.Add(10 * time.Second)
	cw.step(context.Background())
	if checks != prevChecks {
		t.Fatalf("watcher retried on same insert after pause (checks %d -> %d)", prevChecks, checks)
	}

	// Eject clears state, next insert (now idle) finishes deleting the remainder.
	present = false
	cw.step(context.Background())
	present = true
	busy = false
	cw.step(context.Background())
	now = now.Add(4 * time.Second)
	cw.step(context.Background())

	if _, err := os.Lstat(filepath.Join(drive, ".prolink-cache")); !os.IsNotExist(err) {
		t.Fatalf(".prolink-cache not finished on next insert: err=%v", err)
	}
	if len(logs) != 2 || !strings.Contains(logs[1], "removed leftover .prolink-cache") {
		t.Fatalf("expected completion log on next insert, got %v", logs)
	}
}

func TestCleanProlinkCacheMountIDPreparingFailClosedAndByteAccounting(t *testing.T) {
	// 1. Default grace must outlast the 17 s mount window.
	def := newProlinkCleanupWatcher()
	if def.grace < 18*time.Second {
		t.Fatalf("default grace = %s, want >= 18s", def.grace)
	}

	// 2. Hardlinked + sparse files must not overcount bytesFreed.
	drive := t.TempDir()
	cache := filepath.Join(drive, ".prolink-cache", "CDJ-3000")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(cache, "orig.dat")
	if err := os.WriteFile(src, []byte("hello-world-123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, filepath.Join(cache, "hardlink.dat")); err != nil {
		t.Fatal(err)
	}
	sparse, err := os.OpenFile(filepath.Join(cache, "sparse.bin"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(10 << 20); err != nil {
		sparse.Close()
		t.Fatal(err)
	}
	sparse.Close()

	res, err := cleanProlinkCache(context.Background(), drive, prolinkCleanupOptions{
		isBusy:     func() bool { return false },
		isEjecting: func() bool { return false },
		logf:       func(string, ...any) {},
	})
	if err != nil || !res.Removed || res.BytesFreed != 15 {
		t.Fatalf("hardlink+sparse accounting = %+v (err=%v), want Removed=true BytesFreed=15", res, err)
	}

	// 3. Stale/missing deck activity fails closed when using default busy check; preparing defers cleanly.
	priDir := t.TempDir()
	defer bridge.SetPriorityDirForTest(priDir)()
	prepFile := filepath.Join(t.TempDir(), "usb-prep")
	t.Setenv("ENGINEX_USB_PREPARING_PATH", prepFile)

	drive2 := t.TempDir()
	cache2 := filepath.Join(drive2, ".prolink-cache", "CDJ-2")
	if err := os.MkdirAll(cache2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache2, "f.dat"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Missing deck activity -> pauses (fails closed).
	resStale, err := cleanProlinkCache(context.Background(), drive2, prolinkCleanupOptions{
		isEjecting: func() bool { return false },
		logf:       func(string, ...any) {},
	})
	if err != nil || !resStale.Stopped || resStale.Removed {
		t.Fatalf("stale/missing deck activity did not fail closed: %+v err=%v", resStale, err)
	}

	// Preparing file present -> watcher waits without marking handled; mountID bump re-triggers!
	if err := os.WriteFile(prepFile, []byte(`{"state":"loading"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	var mntID uint64 = 100
	cleanCalls := 0
	cw := &prolinkCleanupWatcher{
		handled:   map[mountDevKey]bool{},
		firstSeen: map[mountDevKey]time.Time{},
		list:      func() []string { return []string{drive2} },
		statMount: func(m string) (mountDevKey, bool) {
			return mountDevKey{mount: m, dev: 42, mntID: mntID}, true
		},
		isPreparing: usbPreparingActive,
		clean: func(context.Context, string) (prolinkCleanupResult, error) {
			cleanCalls++
			return prolinkCleanupResult{Removed: true}, nil
		},
		grace: 18 * time.Second,
		now:   func() time.Time { return now },
	}
	cw.step(context.Background())
	now = now.Add(19 * time.Second)
	cw.step(context.Background())
	if cleanCalls != 0 {
		t.Fatalf("cleaned while usb preparing was active: cleanCalls=%d", cleanCalls)
	}
	_ = os.Remove(prepFile)
	cw.step(context.Background())
	if cleanCalls != 1 {
		t.Fatalf("did not clean after usb preparing ended: cleanCalls=%d", cleanCalls)
	}
	// Quick remount at same path & dev with new Linux mntID must clean again!
	mntID = 101
	cw.step(context.Background())
	now = now.Add(19 * time.Second)
	cw.step(context.Background())
	if cleanCalls != 2 {
		t.Fatalf("new mount ID on same path/dev did not re-trigger cleanup: cleanCalls=%d", cleanCalls)
	}
}

func TestCleanProlinkCacheMountReplacementOutsideHardlinkAndErrorLogging(t *testing.T) {
	// 1. Mount replacement between watcher selection and cleaner open: expectedMount mismatch stops cleanly.
	drive := t.TempDir()
	cacheFile := filepath.Join(drive, ".prolink-cache", "CDJ-1", "keep.bin")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("keep-new-mount"), 0o644); err != nil {
		t.Fatal(err)
	}
	key, ok := defaultStatMountKey(drive)
	if !ok || !key.hasMntID {
		t.Fatalf("defaultStatMountKey failed: ok=%v key=%+v", ok, key)
	}
	staleExpected := mountIdentity{dev: key.dev, mntID: key.mntID + 9999, hasMntID: true}
	resMismatch, err := cleanProlinkCache(context.Background(), drive, prolinkCleanupOptions{
		isBusy:        func() bool { return false },
		isEjecting:    func() bool { return false },
		expectedMount: &staleExpected,
		logf:          func(string, ...any) {},
	})
	if err != nil || !resMismatch.Stopped || resMismatch.Removed || resMismatch.BytesFreed != 0 {
		t.Fatalf("expectedMount mismatch res=%+v err=%v; want Stopped=true Removed=false BytesFreed=0", resMismatch, err)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("replacement mount .prolink-cache was touched on mount ID mismatch: %v", err)
	}

	// 2. Outside hard link leaves data allocated -> BytesFreed must be 0 for the outside-linked inode,
	//    while an internal-only pair counts once when the last link is removed.
	base := t.TempDir()
	driveHL := filepath.Join(base, "stick")
	outsideFile := filepath.Join(base, "outside-asset.dat")
	if err := os.WriteFile(outsideFile, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheHL := filepath.Join(driveHL, ".prolink-cache", "CDJ-2")
	if err := os.MkdirAll(cacheHL, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outsideFile, filepath.Join(cacheHL, "outside-link.dat")); err != nil {
		t.Fatal(err)
	}
	inA := filepath.Join(cacheHL, "internal-a.dat")
	if err := os.WriteFile(inA, make([]byte, 250), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(inA, filepath.Join(cacheHL, "internal-b.dat")); err != nil {
		t.Fatal(err)
	}
	resHL, err := cleanProlinkCache(context.Background(), driveHL, prolinkCleanupOptions{
		isBusy:     func() bool { return false },
		isEjecting: func() bool { return false },
		logf:       func(string, ...any) {},
	})
	if err != nil || !resHL.Removed || resHL.BytesFreed != 250 {
		t.Fatalf("outside hardlink accounting = %+v (err=%v), want Removed=true BytesFreed=250", resHL, err)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("outside hardlink target missing: %v", err)
	}

	// 3. Cleanup errors must be logged explicitly while preserving once-per-insert policy.
	driveErr := t.TempDir()
	if err := os.MkdirAll(filepath.Join(driveErr, ".prolink-cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	cleanCalls := 0
	var errLogs []string
	cwErr := &prolinkCleanupWatcher{
		handled:   map[mountDevKey]bool{},
		firstSeen: map[mountDevKey]time.Time{},
		list:      func() []string { return []string{driveErr} },
		statMount: func(m string) (mountDevKey, bool) {
			return mountDevKey{mount: m, dev: 11, mntID: 222, hasMntID: true}, true
		},
		clean: func(context.Context, string) (prolinkCleanupResult, error) {
			cleanCalls++
			return prolinkCleanupResult{BytesFreed: 64}, fmt.Errorf("simulated I/O error")
		},
		logf: func(format string, args ...any) {
			errLogs = append(errLogs, fmt.Sprintf(format, args...))
		},
		grace: time.Second,
		now:   func() time.Time { return now },
	}
	cwErr.step(context.Background())
	now = now.Add(2 * time.Second)
	cwErr.step(context.Background())
	cwErr.step(context.Background())
	if cleanCalls != 1 {
		t.Fatalf("cleanCalls = %d, want 1 (once-per-insert)", cleanCalls)
	}
	if len(errLogs) != 1 || !strings.Contains(errLogs[0], "simulated I/O error") {
		t.Fatalf("cleanup error was swallowed instead of logged: %v", errLogs)
	}
}

func TestProlinkCleanupOnStockEngineStickRemovesOnlyProlinkCache(t *testing.T) {
	drive := t.TempDir()
	keep := map[string]string{
		filepath.Join(drive, "Engine Library", "Database2", "m.db"): "engine-master-db-bytes",
		filepath.Join(drive, "Engine Library", "Database2", "p.db"): "engine-perf-db-bytes",
		filepath.Join(drive, "Music", "Track01.flac"):               "flac-audio-bytes",
	}
	for p, data := range keep {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(drive, "PIONEER", "rekordbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	cacheFile := filepath.Join(drive, ".prolink-cache", "CDJ-1", "cache.bin")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("cache-payload-1234"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := cleanProlinkCache(context.Background(), drive, prolinkCleanupOptions{
		isBusy:     func() bool { return false },
		isEjecting: func() bool { return false },
		logf:       func(string, ...any) {},
	})
	if err != nil || !res.Removed || res.BytesFreed != 18 {
		t.Fatalf("cleanProlinkCache on stock stick = (%+v, %v), want Removed=true BytesFreed=18", res, err)
	}
	if _, err := os.Lstat(filepath.Join(drive, ".prolink-cache")); !os.IsNotExist(err) {
		t.Fatalf(".prolink-cache still present: err=%v", err)
	}
	for p, want := range keep {
		got, err := os.ReadFile(p)
		if err != nil || string(got) != want {
			t.Fatalf("stock file %s changed: got=%q want=%q err=%v", p, got, want, err)
		}
	}
	if st, err := os.Stat(filepath.Join(drive, "PIONEER", "rekordbox")); err != nil || !st.IsDir() {
		t.Fatalf("neighbor PIONEER/rekordbox dir unexpectedly affected: %v", err)
	}
}
