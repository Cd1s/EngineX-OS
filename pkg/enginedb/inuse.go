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

	"golang.org/x/sys/unix"
)

// procRoot is /proc; tests point it at a tree of their own.
var procRoot = "/proc"

// ErrLibraryInUse: another process (Engine) has the library open.
var ErrLibraryInUse = errors.New("Engine has this drive's library open")

type openFileID struct {
	dev uint64
	ino uint64
}

func readDirNames(path string) ([]string, error) {
	d, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	return names, err
}

// OpenElsewhere returns the pid of a process other than this one that holds one
// of files open, or 0. Files that do not exist are skipped.
func OpenElsewhere(files ...string) int {
	var want []openFileID
	for _, f := range files {
		var st unix.Stat_t
		if err := unix.Stat(f, &st); err == nil {
			want = append(want, openFileID{dev: uint64(st.Dev), ino: uint64(st.Ino)})
		}
	}
	if len(want) == 0 {
		return 0
	}
	entries, err := readDirNames(procRoot)
	if err != nil {
		return 0
	}
	self := os.Getpid()
	for _, name := range entries {
		if name == "" || name[0] < '1' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		fdDir, err := os.Open(filepath.Join(procRoot, name, "fd"))
		if err != nil {
			continue
		}
		fds, err := fdDir.Readdirnames(-1)
		if err != nil {
			_ = fdDir.Close()
			continue
		}
		dirFD := int(fdDir.Fd())
		for _, fd := range fds {
			var st unix.Stat_t
			if unix.Fstatat(dirFD, fd, &st, 0) != nil {
				continue
			}
			dev, ino := uint64(st.Dev), uint64(st.Ino)
			for _, w := range want {
				if dev == w.dev && ino == w.ino {
					_ = fdDir.Close()
					return pid
				}
			}
		}
		_ = fdDir.Close()
	}
	return 0
}

// LibraryInUse reports whether a process other than this one has the drive's
// m.db open. root is the folder that holds "Engine Library".
func LibraryInUse(root string) bool {
	return OpenElsewhere(filepath.Join(root, "Engine Library", "Database2", "m.db")) != 0
}
