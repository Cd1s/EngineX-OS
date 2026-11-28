package rbstate

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// A publisher renames its new library over the old one while it holds an exclusive SQLite lock
// on the old file. A process that opened the old file between the publisher's last open-file
// scan and that rename could not read or write it under the lock, but it still has it open, and
// goes on once the lock is gone. Measured (#336 Go review R13): on a rollback-journal library
// its write fails as "database moved"; on a WAL library its write makes a -wal beside the new
// library, and SQLite replays that into it.
//
// So the old file is retired before the lock is released: emptied, for whoever still has it
// open. An empty file is no database: a read finds no table, a write fails as "database moved",
// and no sidecar is made. After the rename nobody can open the old file any more, so one look
// at who has it open is complete.

// ReplacedHeldElsewhere reports whether a process other than this one has old's file open.
func ReplacedHeldElsewhere(old *os.File) bool {
	var st syscall.Stat_t
	if old == nil || syscall.Fstat(int(old.Fd()), &st) != nil {
		return false
	}
	return heldElsewhere(uint64(st.Dev), uint64(st.Ino))
}

// RetireReplaced empties old's file when it has no name any more and another process has it
// open; it reports whether it did. A file that still has a name was not replaced, or lives on
// as a hard-linked backup, and is left as it is. old is any descriptor of the file.
func RetireReplaced(old *os.File) (emptied bool, err error) {
	var st syscall.Stat_t
	if old == nil {
		return false, nil
	}
	if err = syscall.Fstat(int(old.Fd()), &st); err != nil {
		return false, err
	}
	if st.Nlink != 0 || !heldElsewhere(uint64(st.Dev), uint64(st.Ino)) {
		return false, nil
	}
	// Through /proc, so that old may be a read-only or an O_PATH descriptor.
	w, err := os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", old.Fd()), os.O_WRONLY, 0)
	if err != nil {
		return false, err
	}
	defer w.Close()
	if err = w.Truncate(0); err != nil {
		return false, err
	}
	return true, w.Sync()
}

func heldElsewhere(dev, ino uint64) bool {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	self := os.Getpid()
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		dir := "/proc/" + p.Name() + "/fd"
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			var st syscall.Stat_t
			if syscall.Stat(dir+"/"+fd.Name(), &st) == nil && uint64(st.Dev) == dev && uint64(st.Ino) == ino {
				return true
			}
		}
	}
	return false
}
