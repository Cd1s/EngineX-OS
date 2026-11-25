//go:build linux

package datagrow

// Kernel replica of #244 and #291: the 64 MiB /data EngineX-Installer's mode C writes (the
// installer's own ext4.Create with files injected), and a small stock-made ext4, each alone on
// a partition the size of the PRIME 4 G2's /data (mmcblk0p12, 21812719 KiB), grown by the real
// kernel through Grow. Set DATAGROW_KERNEL=1 and run as root inside
// unshare -m --propagation private on a build host, with TMPDIR on a disk (not tmpfs): each
// case attaches one loop device with a partition table and detaches it again. With
// EXT4_STOCK_ROOTFS set to an unpacked AZ04 rootfs (aarch64 host), the deck's own fsck.ext4,
// which az0x-data-mkfs runs before every mount of /data, checks the grown filesystem too.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/Cd1s/EngineX-OS/internal/installer/ext4"
	"golang.org/x/sys/unix"
)

const g2PartitionBytes = 21812719 << 10

func needKernel(t *testing.T) {
	t.Helper()
	if os.Getenv("DATAGROW_KERNEL") != "1" || os.Geteuid() != 0 {
		t.Skip("set DATAGROW_KERNEL=1 and run as root inside unshare -m --propagation private")
	}
	for _, tool := range []string{"losetup", "mount", "umount", "e2fsck", "dumpe2fs", "mkfs.ext4"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

func sh(t *testing.T, name string, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("%s: %v", name, err)
	}
	return string(out), 0
}

// partition makes a sparse disk whose MBR partition 1 starts at 1 MiB and holds size bytes,
// attaches it with partition scanning and returns the partition's device node.
func partition(t *testing.T, size int64) string {
	t.Helper()
	disk := filepath.Join(t.TempDir(), "disk.img")
	mbr := make([]byte, 512)
	e := mbr[446:462]
	e[4] = 0x83
	binary.LittleEndian.PutUint32(e[8:], 2048)
	binary.LittleEndian.PutUint32(e[12:], uint32(size/512))
	mbr[510], mbr[511] = 0x55, 0xAA
	if err := os.WriteFile(disk, mbr, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(disk, 1<<20+size); err != nil {
		t.Fatal(err)
	}
	out, code := sh(t, "losetup", "--find", "--show", "--partscan", disk)
	if code != 0 {
		t.Fatalf("losetup: %s", out)
	}
	loop := strings.TrimSpace(out)
	t.Cleanup(func() { sh(t, "losetup", "-d", loop) })
	part := loop + "p1"
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(part); err == nil {
			return part
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", part)
	return ""
}

type file struct {
	path string
	sum  [32]byte
}

// pattern is n bytes that differ per seed and per 4 KiB block.
func pattern(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed ^ byte(i>>12) ^ byte(i*7)
	}
	return b
}

func writeFile(t *testing.T, dir, rel string, data []byte) file {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return file{rel, sha256.Sum256(data)}
}

func checkFiles(t *testing.T, dir string, files []file) {
	t.Helper()
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, f.path))
		if err != nil || sha256.Sum256(b) != f.sum {
			t.Errorf("%s changed: %v", f.path, err)
		}
	}
}

// modeCImage builds /data as mode C does: a fresh 64 MiB "data" image with files injected.
func modeCImage(t *testing.T) (string, []file) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "data-enginex.img")
	img, err := ext4.Create(context.Background(), p, ext4.CreateOptions{Size: 64 << 20, Label: "data"})
	if err != nil {
		t.Fatal(err)
	}
	var cs ext4.ChangeSet
	var files []file
	for i, rel := range []string{"enginex/bundle/bin/enginex-suite", "enginex/bundle/version.txt", "enginex/ui/ui-packs.bin"} {
		data := pattern(byte(i+1), []int{8 << 20, 9, 3 << 20}[i])
		files = append(files, file{rel, sha256.Sum256(data)})
		cs.Files = append(cs.Files, ext4.Entry{Path: rel, Mode: 0o755, Size: int64(len(data)), SHA256: sha256.Sum256(data),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }})
	}
	if err := img.Apply(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	return p, files
}

func copyTo(t *testing.T, src, dev string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Sync(); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

func dumpField(t *testing.T, dev, field string) string {
	t.Helper()
	out, code := sh(t, "dumpe2fs", "-h", dev)
	if code != 0 {
		t.Fatalf("dumpe2fs: %s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, field+":") {
			return strings.TrimSpace(strings.TrimPrefix(l, field+":"))
		}
	}
	t.Fatalf("dumpe2fs has no %q", field)
	return ""
}

// deckFsck runs the deck's own fsck.ext4 through the rootfs loader, as the stock boot does
// ("-y"), after a read-only pass; exit 0 means nothing needed fixing.
func deckFsck(t *testing.T, dev string) {
	t.Helper()
	r := os.Getenv("EXT4_STOCK_ROOTFS")
	if r == "" {
		t.Log("EXT4_STOCK_ROOTFS not set: the deck's own fsck.ext4 is not run")
		return
	}
	bin := filepath.Join(r, "sbin/fsck.ext4")
	for i := 0; i < 8; i++ {
		l, err := os.Readlink(bin)
		if err != nil {
			break
		}
		if filepath.IsAbs(l) {
			bin = filepath.Join(r, l)
		} else {
			bin = filepath.Join(filepath.Dir(bin), l)
		}
	}
	loaders, _ := filepath.Glob(filepath.Join(r, "lib/ld-linux*.so*"))
	if len(loaders) == 0 {
		t.Fatalf("no loader in %s", r)
	}
	for _, mode := range []string{"-fn", "-fy"} {
		out, code := sh(t, loaders[0], "--library-path", filepath.Join(r, "lib")+":"+filepath.Join(r, "usr/lib"), bin, mode, dev)
		if code != 0 {
			t.Fatalf("deck fsck.ext4 %s exited %d:\n%s", mode, code, out)
		}
	}
	t.Log("the deck's own fsck.ext4 -fn and -fy: clean")
}

// encryptedDir does what the deck's encrypt-fs.sh does for one directory (fscryptctl add_key
// /data, then set_policy on an empty directory, v2), then writes and reads a file inside it.
func encryptedDir(t *testing.T, mnt, name string) {
	t.Helper()
	key := pattern(0x3c, 64)
	arg := make([]byte, 80+len(key)) // struct fscrypt_add_key_arg + raw key
	binary.LittleEndian.PutUint32(arg[0:], unix.FSCRYPT_KEY_SPEC_TYPE_IDENTIFIER)
	binary.LittleEndian.PutUint32(arg[40:], uint32(len(key)))
	copy(arg[80:], key)
	ioctl := func(path string, req uintptr, p unsafe.Pointer) unix.Errno {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		_, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, uintptr(p))
		return e
	}
	if e := ioctl(mnt, unix.FS_IOC_ADD_ENCRYPTION_KEY, unsafe.Pointer(&arg[0])); e != 0 {
		t.Fatalf("FS_IOC_ADD_ENCRYPTION_KEY on the grown filesystem: %v", e)
	}
	dir := filepath.Join(mnt, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pol := make([]byte, 24) // struct fscrypt_policy_v2
	pol[0] = 2
	pol[1] = unix.FSCRYPT_MODE_AES_256_XTS
	pol[2] = unix.FSCRYPT_MODE_AES_256_CTS
	pol[3] = unix.FSCRYPT_POLICY_FLAGS_PAD_32
	copy(pol[8:], arg[8:24]) // the key identifier the kernel returned
	if e := ioctl(dir, unix.FS_IOC_SET_ENCRYPTION_POLICY, unsafe.Pointer(&pol[0])); e != 0 {
		t.Fatalf("FS_IOC_SET_ENCRYPTION_POLICY on the grown filesystem: %v", e)
	}
	want := pattern(0x77, 1<<20)
	if err := os.WriteFile(filepath.Join(dir, "a.stems"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "a.stems")); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("encrypted file reads back wrong: %v", err)
	}
	t.Logf("fscrypt v2 key and policy on %s: ok", name)
}

// writtenSectors is the device's count of 512-byte sectors written (field 7 of its block stat).
func writtenSectors(t *testing.T, dev string) uint64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("/sys/class/block", filepath.Base(dev), "stat"))
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b))
	if len(f) < 7 {
		t.Fatalf("block stat %q", b)
	}
	var n uint64
	if _, err := fmt.Sscan(f[6], &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestKernelGrowsDataToThePartition(t *testing.T) {
	needKernel(t)
	compat := filepath.Join(t.TempDir(), "compatible")
	if err := os.WriteFile(compat, []byte("inmusic,jp24\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := DeckEnv()
	env.Compatible = compat
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, dev string) []file
		feature string // a feature the grown filesystem must carry
	}{
		{"mode C 64 MiB image (#244)", func(t *testing.T, dev string) []file {
			img, files := modeCImage(t)
			copyTo(t, img, dev)
			return files
		}, "meta_bg"}, // no reserved GDT blocks: the kernel switches it to meta_bg
		{"stock mkfs.ext4 256 MiB (#291)", func(t *testing.T, dev string) []file {
			if out, code := sh(t, "mkfs.ext4", "-F", "-q", "-O", "encrypt", "-L", "data", "-b", "4096", dev, "65536"); code != 0 {
				t.Fatalf("mkfs.ext4: %s", out)
			}
			return nil
		}, "encrypt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := partition(t, g2PartitionBytes)
			files := tc.prepare(t, dev)
			mnt := t.TempDir()
			if out, code := sh(t, "mount", "-t", "ext4", dev, mnt); code != 0 {
				t.Fatalf("mount: %s", out)
			}
			mounted := true
			t.Cleanup(func() {
				if mounted {
					sh(t, "umount", mnt)
				}
			})
			files = append(files, writeFile(t, mnt, "before.bin", pattern(0x5a, 4<<20)))
			sectors := writtenSectors(t, dev)
			start := time.Now()
			r, err := Grow(env, mnt)
			if err != nil || !r.Grown {
				t.Fatalf("grow: %+v, %v", r, err)
			}
			// What the deck's eMMC would see once: the new groups' bitmaps, descriptors and zeroed
			// inode tables (a loop device zeroes ranges cheaply, so the time is not the deck's).
			t.Logf("the grow wrote %d MiB in %v", (writtenSectors(t, dev)-sectors)*512>>20, time.Since(start).Round(time.Millisecond))
			if r.ToBlocks*r.BlockSize < g2PartitionBytes/10*9 {
				t.Fatalf("after the grow statfs says %d MiB of %d MiB", r.ToBlocks*r.BlockSize>>20, g2PartitionBytes>>20)
			}
			t.Logf("grown from %d MiB to %d MiB", r.FromBlocks*r.BlockSize>>20, r.ToBlocks*r.BlockSize>>20)
			// The next boot asks nothing of the kernel.
			if r2, err := Grow(env, mnt); err != nil || r2.Grown || !strings.Contains(r2.Skipped, "already uses its partition") {
				t.Fatalf("second run: %+v, %v", r2, err)
			}
			// 200 MiB more than the old filesystem could hold goes into the new block groups.
			files = append(files, writeFile(t, mnt, "after.bin", pattern(0xa5, 200<<20)))
			// The deck's encrypt-fs.sh adds an fscrypt key to /data and sets a policy on fresh
			// downloads/ and stems/ directories; if that fails, Engine's Reporter may answer with a
			// factory reset. It must still work on the grown filesystem.
			encryptedDir(t, mnt, "stems")
			checkFiles(t, mnt, files)
			if out, code := sh(t, "umount", mnt); code != 0 {
				t.Fatalf("umount: %s", out)
			}
			mounted = false
			// The deck's boot runs fsck.ext4 -y before every mount: it must find nothing to fix.
			if out, code := sh(t, "e2fsck", "-fn", dev); code != 0 {
				t.Fatalf("e2fsck -fn exit %d:\n%s", code, out)
			}
			deckFsck(t, dev)
			if got, want := dumpField(t, dev, "Block count"), fmt.Sprint(g2PartitionBytes/4096); got != want {
				t.Errorf("block count %s, want %s (the whole partition)", got, want)
			}
			if f := dumpField(t, dev, "Filesystem features"); !strings.Contains(" "+f+" ", " "+tc.feature+" ") || !strings.Contains(f, "encrypt") {
				t.Errorf("features %q", f)
			}
			// The kernel leaves the new groups' inode tables for its lazy-init thread, which zeroes
			// them after the next mount, paced as after a stock mkfs.ext4: log how much that is.
			var inodes, ipg, itb uint64
			for f, v := range map[string]*uint64{"Inode count": &inodes, "Inodes per group": &ipg, "Inode blocks per group": &itb} {
				if _, err := fmt.Sscan(dumpField(t, dev, f), v); err != nil {
					t.Fatalf("%s: %v", f, err)
				}
			}
			t.Logf("%d inodes, %d per group; inode tables %d MiB in all; journal %s", inodes, ipg,
				inodes/ipg*itb*4096>>20, dumpField(t, dev, "Total journal size"))
			if out, code := sh(t, "mount", "-t", "ext4", "-o", "ro", dev, mnt); code != 0 {
				t.Fatalf("remount: %s", out)
			}
			mounted = true
			checkFiles(t, mnt, files)
		})
	}
}
