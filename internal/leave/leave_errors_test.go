package leave

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func auditReset(t *testing.T) {
	t.Helper()
	s, r, l, o, f, u, a := lstat, readFile, readlink, openFile, flock, remove, removeAll
	t.Cleanup(func() { lstat, readFile, readlink, openFile, flock, remove, removeAll = s, r, l, o, f, u, a })
}

func auditExists(p string) bool { _, err := os.Lstat(p); return err == nil }
func auditDir(p string) bool    { st, err := os.Lstat(p); return err == nil && st.IsDir() }
func auditText(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return strings.TrimSpace(string(b))
}
func auditState(root string) string {
	base := filepath.Join(root, "data/sc5000-custom")
	return fmt.Sprintf("journal=%v version=%q pack=%q icon=%q hook=%q unlocked=%v enginex-real=%v legacy-real=%v", auditExists(filepath.Join(root, "data", journalName)), auditText(filepath.Join(base, "version.txt")), auditText(filepath.Join(base, "ui/packs/en/Deck.qml")), auditText(filepath.Join(base, "ui/icons/a.png")), auditText(filepath.Join(base, "bundle/lib/libengine-ui-hook.so")), auditExists(filepath.Join(base, "gen2-unlocked")), auditDir(filepath.Join(root, "data/enginex")), auditDir(base))
}
func auditWholeCurrent(root string) bool {
	base := filepath.Join(root, "data/sc5000-custom")
	return auditText(filepath.Join(base, "version.txt")) == "5.3.0.5" && auditText(filepath.Join(base, "ui/packs/en/Deck.qml")) == "5.3.0.5 pack" && auditText(filepath.Join(base, "ui/icons/a.png")) == "5.3.0.5 icon" && auditText(filepath.Join(base, "bundle/lib/libengine-ui-hook.so")) == "hook 5.3.0.5"
}

func auditFault(kind, path string, when int) *int {
	hits := new(int)
	fail := func(p string) bool {
		if p != path {
			return false
		}
		*hits++
		return *hits == when
	}
	switch kind {
	case "read":
		real := readFile
		readFile = func(p string) ([]byte, error) {
			if fail(p) {
				return nil, syscall.EIO
			}
			return real(p)
		}
	case "stat":
		real := lstat
		lstat = func(p string) (os.FileInfo, error) {
			if fail(p) {
				return nil, syscall.EIO
			}
			return real(p)
		}
	case "readlink":
		real := readlink
		readlink = func(p string) (string, error) {
			if fail(p) {
				return "", syscall.EIO
			}
			return real(p)
		}
	case "remove":
		real := remove
		remove = func(p string) error {
			if fail(p) {
				return syscall.EIO
			}
			return real(p)
		}
	}
	return hits
}

func TestLeaveIORecoveryDecisionMatrix(t *testing.T) {
	for _, c := range []struct {
		name, kind, relative      string
		legacy, noUnlock, noPacks bool
		when                      int
	}{
		{name: "control"},
		{name: "journal-id-read", kind: "read", relative: "journal/id", when: 1},
		{name: "staged-bundle-marker-read", kind: "read", relative: "journal/stage/root/bundle/" + markerName, when: 1},
		{name: "live-packs-marker-read", kind: "read", relative: "live/ui/packs/" + markerName, when: 1},
		{name: "live-icons-marker-read", kind: "read", relative: "live/ui/icons/" + markerName, when: 1},
		{name: "saved-version-read", kind: "read", relative: "journal/old/version.txt", when: 1},
		{name: "saved-absence-stat", kind: "stat", relative: "journal/old/gen2-unlocked.absent", noUnlock: true, when: 1},
		{name: "layout-record-stat", kind: "stat", relative: "journal/layout", legacy: true, when: 1},
		{name: "layout-directory-stat", kind: "stat", relative: "sc5000-custom", legacy: true, when: 2},
		{name: "layout-readlink", kind: "readlink", relative: "enginex", legacy: true, when: 1},
		{name: "rollback-remove-error", kind: "remove", relative: "live/gen2-unlocked", noUnlock: true, when: 1},
		{name: "absent-packs-stat-still-errors", kind: "stat", relative: "journal/absent-packs", noPacks: true, when: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			auditReset(t)
			d, root := newDeck(t)
			if c.noUnlock {
				if err := os.Remove(d.data("enginex/gen2-unlocked")); err != nil {
					t.Fatal(err)
				}
			}
			if c.noPacks {
				if err := os.RemoveAll(d.data("enginex/ui/packs")); err != nil {
					t.Fatal(err)
				}
			}
			version, liveName := "5.3.0.3", "enginex"
			if c.legacy {
				version, liveName = "5.3.0.2", "sc5000-custom"
			}
			cutAt(t, d, legacyBundle(t, version), "bundle")
			before := snapshot(t, root)
			journal := filepath.Join(root, "data", journalName)
			p := strings.Replace(c.relative, "journal/", journal+"/", 1)
			if strings.HasPrefix(c.relative, "live/") {
				p = filepath.Join(root, "data", liveName, strings.TrimPrefix(c.relative, "live/"))
			}
			if !strings.Contains(c.relative, "/") {
				p = filepath.Join(root, "data", c.relative)
			}
			hits := auditFault(c.kind, p, c.when)
			err := d.Recover()
			t.Logf("fault-hits=%d error=%v %s", *hits, err, auditState(root))
			if c.kind == "" {
				if err != nil || auditExists(journal) || !auditWholeCurrent(root) {
					t.Fatal("control did not recover")
				}
				return
			}
			if *hits < c.when {
				t.Fatal("fault was not exercised")
			}
			if err == nil || !auditExists(journal) {
				t.Fatal("filesystem error was followed by recovery cleanup")
			}
			if c.kind != "remove" {
				if changes := diff(before, snapshot(t, root)); changes != "" {
					t.Fatalf("metadata failure changed live state: %s", changes)
				}
			}
		})
	}
}

func TestLeaveIOInstallDecisionMatrix(t *testing.T) {
	for _, c := range []struct {
		name, kind, relative string
		when                 int
		legacy, cut          bool
	}{
		{name: "bundle-marker-creation-stat", relative: "stage/root/bundle", when: 1, cut: true},
		{name: "packs-marker-creation-stat", relative: "stage/root/ui/packs", when: 1, cut: true},
		{name: "icons-marker-creation-stat", relative: "stage/root/ui/icons", when: 1, cut: true},
		{name: "capture-current-version-read", kind: "read", relative: "live/version.txt", when: 1, cut: true},
		{name: "staged-packs-before-exchange-stat", relative: "stage/root/ui/packs", when: 2},
		{name: "staged-version-read", kind: "read", relative: "stage/root/version.txt", when: 2},
		{name: "commit-layout-record-stat", relative: "layout", when: 1, legacy: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			auditReset(t)
			d, root := newDeck(t)
			version := "5.3.0.3"
			if c.legacy {
				version = "5.3.0.2"
			}
			target := legacyBundle(t, version)
			before := snapshot(t, root)
			journal := filepath.Join(root, "data", journalName)
			p := filepath.Join(journal, c.relative)
			if strings.HasPrefix(c.relative, "live/") {
				p = filepath.Join(root, "data/enginex", strings.TrimPrefix(c.relative, "live/"))
			}
			kind := c.kind
			if kind == "" {
				kind = "stat"
			}
			hits := auditFault(kind, p, c.when)
			oldCrash := crash
			cut := false
			if c.cut {
				crash = func(step string) {
					if step == "bundle" {
						panic(errCut)
					}
				}
			}
			var installErr error
			func() {
				defer func() {
					if r := recover(); r != nil {
						if r != errCut {
							panic(r)
						}
						cut = true
					}
				}()
				installErr = d.Install(target)
			}()
			crash = oldCrash
			var recoverErr error
			if cut {
				recoverErr = d.Recover()
			}
			t.Logf("fault-hits=%d install=%v cut=%v recovery=%v %s", *hits, installErr, cut, recoverErr, auditState(root))
			if *hits < c.when {
				t.Fatal("fault was not exercised")
			}
			if installErr == nil || (!auditWholeCurrent(root) && !auditExists(journal)) {
				t.Fatal("lookup error passed publication and discarded rollback state")
			}
			if cut {
				t.Fatal("metadata failure reached the live bundle swap")
			}
			if changes := diff(before, snapshot(t, root)); changes != "" {
				t.Fatalf("metadata failure changed live state: %s", changes)
			}
		})
	}
}

func TestLeaveIOLayoutFlagCannotSkipPendingDataSync(t *testing.T) {
	auditReset(t)
	d, root := newDeck(t)
	cutAt(t, d, legacyBundle(t, "5.3.0.2"), "bundle")
	journal := filepath.Join(root, "data", journalName)
	fault := injectSyncFault(t, "/data/sc5000-custom", filepath.Join(root, "data"))
	if err := d.Recover(); err == nil || !auditExists(journal) {
		t.Fatal("fixture did not retain failed layout sync")
	}
	realStat := lstat
	hits := 0
	lstat = func(p string) (os.FileInfo, error) {
		if p == filepath.Join(journal, "layout") {
			hits++
			return nil, syscall.EIO
		}
		return realStat(p)
	}
	err := d.Recover()
	t.Logf("layout-lookups=%d recovery=%v fsync-errors=%d %s", hits, err, fault.fails, auditState(root))
	if hits == 0 {
		t.Fatal("metadata fault was not exercised")
	}
	if err == nil || !auditExists(journal) {
		t.Fatal("layout lookup error bypassed a persistent directory fsync failure")
	}
	lstat = realStat
	if err := d.Recover(); err == nil || !auditExists(journal) || fault.fails < 2 {
		t.Fatal("pending directory barrier was not retried after metadata became readable")
	}
	fault.armed = false
	if err := d.Recover(); err != nil || auditExists(journal) || !auditWholeCurrent(root) {
		t.Fatal("not restored after both faults cleared")
	}
}

func TestLeaveIOVersionReadErrorCannotSelectInPlaceInstall(t *testing.T) {
	auditReset(t)
	d, _ := newDeck(t)
	target := legacyBundle(t, "5.3.0.3")
	hits := auditFault("read", filepath.Join(target, "version.txt"), 1)
	older, err := d.Older(target)
	t.Logf("read-errors=%d older=%v error=%v", *hits, older, err)
	if *hits != 1 {
		t.Fatal("fault was not exercised")
	}
	if err == nil {
		t.Fatal("version read error selects Main's in-place installer branch")
	}
}

func TestLeaveIOLockErrorsCannotAuthorizeCleanup(t *testing.T) {
	for _, c := range []string{"busy-control", "open-recover", "flock-recover", "open-install", "flock-install"} {
		t.Run(c, func(t *testing.T) {
			auditReset(t)
			d, root := newDeck(t)
			cutAt(t, d, legacyBundle(t, "5.3.0.3"), "bundle")
			journal := filepath.Join(root, "data", journalName)
			bad := legacyBundle(t, "5.3.0.2")
			write(t, filepath.Join(bad, "bin/stream-sync"), "incorrect checksum")
			lockPath := d.data(journalName + ".lock")
			owner, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			faults := 0
			if strings.HasPrefix(c, "open-") {
				real := openFile
				openFile = func(p string, flag int, mode os.FileMode) (*os.File, error) {
					if p == lockPath {
						faults++
						return nil, syscall.EIO
					}
					return real(p, flag, mode)
				}
			}
			if strings.HasPrefix(c, "flock-") {
				flock = func(fd, how int) error { faults++; return syscall.EIO }
			}
			if strings.HasSuffix(c, "install") {
				err = d.Install(bad)
			} else {
				err = d.Recover()
			}
			t.Logf("faults=%d operation=%v owner-fd-still-open=true %s", faults, err, auditState(root))
			if c == "busy-control" {
				if err != nil || !auditExists(journal) {
					t.Fatal("busy control changed transaction")
				}
				return
			}
			if faults == 0 {
				t.Fatal("fault was not exercised")
			}
			if err == nil || !auditExists(journal) {
				t.Fatal("lock I/O error did not stop activation/cleanup")
			}
		})
	}
}

func TestLeaveIOJournalOrder(t *testing.T) {
	for _, installFirst := range []bool{false, true} {
		name := "recover-first"
		if installFirst {
			name = "install-first"
		}
		t.Run(name, func(t *testing.T) {
			auditReset(t)
			d, root := newDeck(t)
			cutAt(t, d, legacyBundle(t, "5.3.0.3"), "bundle")
			journal := filepath.Join(root, "data", journalName)
			bad := legacyBundle(t, "5.3.0.2")
			write(t, filepath.Join(bad, "bin/stream-sync"), "incorrect checksum")
			runs := []func() error{d.Recover, func() error { return d.Install(bad) }}
			if installFirst {
				runs[0], runs[1] = runs[1], runs[0]
			}
			for _, run := range runs {
				failed := false
				lstat = func(p string) (os.FileInfo, error) {
					if p == journal && !failed {
						failed = true
						return nil, syscall.EIO
					}
					return os.Lstat(p)
				}
				err := run()
				if !failed || err == nil || !auditExists(journal) || auditText(filepath.Join(journal, "stage/root/ui/packs/en/Deck.qml")) != "5.3.0.5 pack" {
					t.Fatalf("journal error was ignored in %s: %v %s", name, err, auditState(root))
				}
			}
			lstat = os.Lstat
			if err := d.Recover(); err != nil || !auditWholeCurrent(root) {
				t.Fatalf("not recovered after faults cleared: %v", err)
			}
		})
	}
}

func TestLeaveIOCleanupStopsOnError(t *testing.T) {
	for _, point := range []string{"cache", "marker", "journal"} {
		t.Run(point, func(t *testing.T) {
			auditReset(t)
			d, root := newDeck(t)
			journal := filepath.Join(root, "data", journalName)
			target := legacyBundle(t, "5.3.0.3")
			if point != "cache" {
				cutAt(t, d, target, "committed")
			}
			realRemove, realRemoveAll := remove, removeAll
			failed, prepared, later := false, false, 0
			remove = func(p string) error {
				if failed {
					later++
				}
				if point == "marker" && p == filepath.Join(root, "data/enginex/bundle", markerName) {
					failed = true
					prepared = auditExists(filepath.Join(journal, "id"))
					return syscall.EIO
				}
				return realRemove(p)
			}
			removeAll = func(p string) error {
				if failed {
					later++
				}
				if (point == "cache" && p == d.data("AppDataUser/AIR Music Technology/Engine/.cache/qml")) || (point == "journal" && p == journal) {
					failed = true
					prepared = auditExists(filepath.Join(journal, "id"))
					return syscall.EIO
				}
				return realRemoveAll(p)
			}
			var err error
			if point == "cache" {
				err = d.Install(target)
			} else {
				err = d.Recover()
			}
			if err == nil || !failed || !prepared || later != 0 || !auditExists(journal) {
				t.Fatalf("cleanup did not stop: error=%v failed=%v prepared=%v later=%d journal=%v", err, failed, prepared, later, auditExists(journal))
			}
			remove, removeAll = realRemove, realRemoveAll
			if err := d.Recover(); err != nil || auditExists(journal) {
				t.Fatalf("cleanup retry: %v", err)
			}
			version := "5.3.0.3"
			if point == "cache" {
				version = "5.3.0.5"
			}
			if got := auditText(d.data("sc5000-custom/bundle/lib/libengine-ui-hook.so")); got != "hook "+version {
				t.Fatalf("retry changed the generation: %s", got)
			}
		})
	}
}

func TestLeaveActivationWaitsAndHoldsTheMigrationLock(t *testing.T) {
	auditReset(t)
	d, root := newDeck(t)
	t.Setenv("ENGINEX_LEAVE_ROOT", root)
	script := filepath.Join(root, "activate.sh")
	write(t, script, `#!/bin/sh
set -eu
test "$ENGINEX_LEAVE_LOCKED" = 1
test "$ENGINEX_LEAVE_PID" = "$PPID"
: > "$ENGINEX_LEAVE_ROOT/activation-ready"
while [ ! -f "$ENGINEX_LEAVE_ROOT/activation-release" ]; do sleep 0.01; done
: > "$ENGINEX_LEAVE_ROOT/migrated"
`)
	lockPath := d.data(journalName + ".lock")
	owner, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	entered := make(chan int, 1)
	realFlock := flock
	flock = func(fd, how int) error { entered <- how; return realFlock(fd, how) }
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { defer close(finished); done <- d.activate([]string{script}) }()
	t.Cleanup(func() {
		owner.Close()
		os.WriteFile(filepath.Join(root, "activation-release"), nil, 0600)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("activation worker did not exit")
		}
	})
	select {
	case how := <-entered:
		if how&unix.LOCK_NB != 0 {
			t.Fatal("activation used a nonblocking lock")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("activation did not attempt its lock")
	}
	if auditExists(filepath.Join(root, "activation-ready")) {
		t.Fatal("migration started while another holder owned the lock")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !auditExists(filepath.Join(root, "activation-ready")) {
		select {
		case err := <-done:
			t.Fatalf("activation exited before migration: %v", err)
		case <-deadline.C:
			t.Fatal("migration did not start after the holder released its lock")
		case <-ticker.C:
		}
	}
	other, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("activation did not retain its lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "activation-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-deadline.C:
		t.Fatal("activation did not finish")
	}
	if !auditExists(filepath.Join(root, "migrated")) {
		t.Fatal("script did not finish")
	}
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("activation did not release its lock: %v", err)
	}
}
