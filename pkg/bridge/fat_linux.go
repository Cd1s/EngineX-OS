//go:build linux

package bridge

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fibmap is FIBMAP: the volume block of a file's block.
const fibmap = 1

// unsafeToFree reports whether removing or replacing path, on vfat, could free clusters the FAT
// does not give it (#321): a pull can leave an entry ahead of its FAT chain, or one naming
// clusters already freed, and vfat remounts the stick read-only when it frees those. Where this
// cannot be checked (another file system, an empty file, an unreadable volume) the file is freed.
// The answer holds only until something is allocated on the volume (#321 review R3), and a
// cross-link looks owned: only fsck repairs one.
func unsafeToFree(path string) bool {
	var fs unix.Statfs_t
	if unix.Statfs(path, &fs) != nil || uint32(fs.Type) != unix.MSDOS_SUPER_MAGIC {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Size <= 0 {
		return false // empty: an entry may still name a cluster FIBMAP does not show
	}
	block := 0
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fibmap, uintptr(unsafe.Pointer(&block))); e != 0 || block <= 0 {
		return false
	}
	dev, err := blockDevice(st.Dev)
	if err != nil {
		return false
	}
	defer dev.Close()
	owned, checked := fatChainOwned(dev, int64(block), st.Size)
	return checked && !owned
}

func blockDevice(dev uint64) (*os.File, error) {
	b, err := os.ReadFile(filepath.Join("/sys/dev/block", strconv.FormatUint(uint64(unix.Major(dev)), 10)+":"+strconv.FormatUint(uint64(unix.Minor(dev)), 10), "uevent"))
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if name, ok := strings.CutPrefix(l, "DEVNAME="); ok && !strings.Contains(name, "..") {
			return os.Open(filepath.Join("/dev", name))
		}
	}
	return nil, os.ErrNotExist
}
