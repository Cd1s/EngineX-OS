// Package datagrow lets the deck's /data filesystem use its whole partition (#244).
//
// EngineX-Installer's mode C (Firmware Update Mode: PRIME 4 G2, SYSTEM ONE) cannot ask the
// deck how big its data partition is, so it writes a 64 MiB ext4 image there. The stock boot
// never grows it: az0x-data-mkfs runs mkfs.ext4 only when the partition has no filesystem and
// fsck.ext4 otherwise, and the stock rootfs has no resize2fs. A /data that small refuses every
// on-screen update (the update wants 256 MiB reserve plus the 256 MiB install peak free).
//
// The deck's kernel grows a mounted ext4 by itself (EXT4_IOC_RESIZE_FS, the call resize2fs
// makes online; a filesystem without reserved GDT blocks is switched to meta_bg on the way).
// Grow asks it to, once, when the filesystem is clearly smaller than its partition; on a
// /data that already fills its partition it only reads mountinfo, sysfs and statfs.
package datagrow

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ext4IocResizeFS is EXT4_IOC_RESIZE_FS, _IOW('f', 16, __u64), the same number on arm, arm64 and amd64.
const ext4IocResizeFS = 0x40086610

const (
	// minGrowBytes and the 5/4 ratio keep a filesystem that already fills its partition alone:
	// statfs leaves out the inode tables, journal and bitmaps (a few percent), never a quarter.
	minGrowBytes = 64 << 20
	growNum      = 5
	growDen      = 4
)

// Env is where Grow reads the system; tests replace the parts.
type Env struct {
	MountInfo   string // /proc/self/mountinfo
	SysDevBlock string // /sys/dev/block
	Compatible  string // the device tree's compatible list
	Statfs      func(dir string) (blockSize, blocks uint64, err error)
	// Resize asks the kernel to grow the ext4 mounted at dir to blocks filesystem blocks.
	Resize func(dir string, blocks uint64) error
}

// DeckEnv is the running system.
func DeckEnv() Env {
	return Env{
		MountInfo:   "/proc/self/mountinfo",
		SysDevBlock: "/sys/dev/block",
		Compatible:  "/sys/firmware/devicetree/base/compatible",
		Statfs:      statfs,
		Resize:      resize,
	}
}

// Result says what Grow did. Blocks are filesystem blocks of BlockSize bytes; FromBlocks and
// ToBlocks are what statfs reports (without the filesystem's own metadata).
type Result struct {
	Grown          bool
	Skipped        string // why nothing was asked of the kernel; empty when it was
	BlockSize      uint64
	FromBlocks     uint64
	ToBlocks       uint64
	PartitionBytes uint64
}

// Grow grows the ext4 mounted at dir to the size of its partition when the filesystem is at
// least 64 MiB and a quarter smaller than that partition. Only a read-write ext4 that is the
// root of a partition of a deck (an inmusic board) is touched.
func Grow(env Env, dir string) (Result, error) {
	var r Result
	if !isDeck(env.Compatible) {
		r.Skipped = "not a deck"
		return r, nil
	}
	m, ok, err := findMount(env.MountInfo, dir)
	if err != nil {
		return r, err
	}
	switch {
	case !ok:
		r.Skipped = dir + " is not a mount point"
		return r, nil
	case m.fstype != "ext4":
		r.Skipped = dir + " is " + m.fstype + ", not ext4"
		return r, nil
	case m.root != "/":
		r.Skipped = dir + " is a bind mount"
		return r, nil
	case !hasOpt(m.opts, "rw") || !hasOpt(m.superOpts, "rw"):
		r.Skipped = dir + " is read-only"
		return r, nil
	}
	sys := filepath.Join(env.SysDevBlock, m.majMin)
	if _, err := os.Stat(filepath.Join(sys, "partition")); err != nil {
		r.Skipped = dir + " is not on a partition"
		return r, nil
	}
	sectors, err := readUint(filepath.Join(sys, "size"))
	if err != nil {
		return r, fmt.Errorf("partition size of %s: %w", m.majMin, err)
	}
	r.PartitionBytes = sectors * 512
	bs, blocks, err := env.Statfs(dir)
	if err != nil {
		return r, fmt.Errorf("statfs %s: %w", dir, err)
	}
	if bs == 0 || bs&(bs-1) != 0 {
		return r, fmt.Errorf("statfs %s: block size %d", dir, bs)
	}
	r.BlockSize, r.FromBlocks, r.ToBlocks = bs, blocks, blocks
	target := r.PartitionBytes / bs
	if target <= blocks || (target-blocks)*bs < minGrowBytes || target*growDen < blocks*growNum {
		r.Skipped = dir + " already uses its partition"
		return r, nil
	}
	if err := env.Resize(dir, target); err != nil {
		return r, fmt.Errorf("grow %s to %d blocks: %w", dir, target, err)
	}
	r.Grown = true
	if _, after, err := env.Statfs(dir); err == nil {
		r.ToBlocks = after
	}
	return r, nil
}

func isDeck(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, c := range strings.Split(string(b), "\x00") {
		if strings.HasPrefix(c, "inmusic,") {
			return true
		}
	}
	return false
}

type mount struct {
	majMin, root, opts, fstype, superOpts string
}

// findMount returns the last (topmost) mountinfo entry for dir.
func findMount(path, dir string) (mount, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return mount{}, false, err
	}
	defer f.Close()
	var m mount
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || len(fields) < sep+4 { // fstype, source, super options follow the "-"
			continue
		}
		if unescape(fields[4]) != dir {
			continue
		}
		m = mount{majMin: fields[2], root: unescape(fields[3]), opts: fields[5],
			fstype: fields[sep+1], superOpts: fields[sep+3]}
		found = true
	}
	return m, found, sc.Err()
}

// unescape undoes mountinfo's octal escapes (\040 for a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hasOpt(opts, want string) bool {
	for _, o := range strings.Split(opts, ",") {
		if o == want {
			return true
		}
	}
	return false
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func statfs(dir string) (uint64, uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Bsize), uint64(st.Blocks), nil
}

func resize(dir string, blocks uint64) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	n := blocks
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), ext4IocResizeFS, uintptr(unsafe.Pointer(&n))); e != 0 {
		return e
	}
	return nil
}

// Main is the enginex-suite data-grow applet: data-grow [dir] (default /data).
func Main(args []string, stdout, stderr io.Writer) int {
	dir := "/data"
	switch len(args) {
	case 0:
	case 1:
		dir = args[0]
	default:
		fmt.Fprintln(stderr, "usage: enginex-suite data-grow [dir]")
		return 2
	}
	r, err := Grow(DeckEnv(), dir)
	if err != nil {
		fmt.Fprintln(stderr, "data-grow:", err)
		return 1
	}
	mib := func(blocks uint64) uint64 { return blocks * r.BlockSize >> 20 }
	switch {
	case r.Grown:
		fmt.Fprintf(stdout, "data-grow: %s grown from %d MiB to %d MiB (partition %d MiB)\n",
			dir, mib(r.FromBlocks), mib(r.ToBlocks), r.PartitionBytes>>20)
	default:
		fmt.Fprintf(stdout, "data-grow: nothing to do: %s\n", r.Skipped)
	}
	return 0
}
