//go:build linux

package enginedb

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openNoAtime: reading a file does not write its access time (on FAT, its directory entry).
const openNoAtime = syscall.O_NOATIME

// noReadahead: the page cache reads only the pages each read asks for, so a paced reader leaves
// the stick idle between its reads (no readahead into the pause, #253 review 4).
func noReadahead(f *os.File) { _ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_RANDOM) }
