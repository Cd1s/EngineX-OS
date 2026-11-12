package enginedb

// While Engine has a library open no other process may write its m.db
// (CONTRIBUTING.md): Engine 5.3's connection pool and an outside writer have
// produced SQLITE_CORRUPT, after which Engine deletes m.db. Writes asked for
// from the Web Console while the drive is in use are queued in the library
// (pending.go) and applied by engine-guardian before Engine next opens it
// (BH-GC2-1, BH-GC1-3).

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// procRoot is /proc; tests point it at a tree of their own.
var procRoot = "/proc"

// ErrLibraryInUse: another process (Engine) has the library open.
var ErrLibraryInUse = errors.New("Engine has this drive's library open")

// OpenElsewhere returns the pid of a process other than this one that holds one
// of files open, or 0. Files that do not exist are skipped.
func OpenElsewhere(files ...string) int {
	var want []os.FileInfo
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			want = append(want, st)
		}
	}
	if len(want) == 0 {
		return 0
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0
	}
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		dir := filepath.Join(procRoot, e.Name(), "fd")
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			st, err := os.Stat(filepath.Join(dir, fd.Name()))
			if err != nil {
				continue
			}
			for _, w := range want {
				if os.SameFile(st, w) {
					return pid
				}
			}
		}
	}
	return 0
}

// LibraryInUse reports whether a process other than this one has the drive's
// m.db open. root is the folder that holds "Engine Library".
func LibraryInUse(root string) bool {
	return OpenElsewhere(filepath.Join(root, "Engine Library", "Database2", "m.db")) != 0
}
