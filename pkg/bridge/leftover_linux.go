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

// unsafeToFree reports whether removing or replacing path could free clusters the FAT does not
// give it (#321). A pull can leave, on vfat, an entry ahead of its FAT chain (a temporary file
// cut while written) or one naming clusters already freed (a file replaced or removed by older
// builds): reading it may still work, but vfat treats freeing those clusters as corruption, and
// by default remounts the stick read-only. So on vfat a file the FAT does not give all its
// clusters (fatOwned) is not freed. Where that cannot be checked (another file system, an empty
// file, a volume that cannot be read) the file is freed as before.
//
// "Owned" means an intact chain of the file's length, not an unshared one: no other entry is
// looked at. So the answer holds only if it is taken before anything is allocated on the volume
// (a new file can take the clusters a stale entry names, and the entry then looks like their
// owner: the pass decides first, and then does not write such a stick, #321 review R3), and two
// entries that name one intact chain (a cross-link left by a pull or another system) both look
// owned: freeing either frees the other's clusters. Only fsck can repair a cross-link.
func unsafeToFree(path string) bool {
	owned, checked := fatOwned(path)
	return checked && !owned
}

// onExFAT reports whether path is on exFAT (the statfs fatOwned reads).
func onExFAT(path string) bool {
	var fs unix.Statfs_t
	return unix.Statfs(path, &fs) == nil && uint32(fs.Type) == unix.EXFAT_SUPER_MAGIC
}

// fatOwned reports whether path, on vfat, owns all its clusters in the FAT (fatChainOwned);
// checked is false off vfat, for an empty file, or where the volume cannot be read.
func fatOwned(path string) (owned, checked bool) {
	var fs unix.Statfs_t
	if unix.Statfs(path, &fs) != nil || uint32(fs.Type) != unix.MSDOS_SUPER_MAGIC {
		return false, false
	}
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Size <= 0 {
		return false, false // empty: an entry may still name a cluster FIBMAP does not show
	}
	block := 0
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fibmap, uintptr(unsafe.Pointer(&block))); e != 0 || block <= 0 {
		return false, false
	}
	dev, err := blockDevice(st.Dev)
	if err != nil {
		return false, false
	}
	defer dev.Close()
	return fatChainOwned(dev, int64(block), st.Size)
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
