//go:build linux

package ext4

// Kernel oracle: loop-mounts images built and changed here. Set
// EXT4_KERNEL_MOUNT=1 and run as root inside a private mount namespace
// (unshare -m with a private tmpfs /data), as on the ARM build hosts.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func needKernelMount(t *testing.T) {
	t.Helper()
	if os.Getenv("EXT4_KERNEL_MOUNT") != "1" || os.Geteuid() != 0 {
		t.Skip("set EXT4_KERNEL_MOUNT=1 and run as root inside unshare -m")
	}
	needTools(t, "mount", "umount", "e2fsck")
}

func mountImage(t *testing.T, img string, ro bool) string {
	t.Helper()
	mnt := t.TempDir()
	opts := "loop"
	if ro {
		opts += ",ro"
	}
	if out, code := run(t, nil, "mount", "-t", "ext4", "-o", opts, img, mnt); code != 0 {
		t.Fatalf("mount %s: %s", img, out)
	}
	return mnt
}

func umount(t *testing.T, mnt string) {
	t.Helper()
	if out, code := run(t, nil, "umount", mnt); code != 0 {
		t.Fatalf("umount: %s", out)
	}
}

// kernelMatches compares what the kernel reads with the change set.
func kernelMatches(t *testing.T, mnt string, cs ChangeSet) {
	t.Helper()
	for _, e := range cs.Files {
		p := filepath.Join(mnt, e.Path)
		data, err := os.ReadFile(p)
		if err != nil || sha256.Sum256(data) != e.SHA256 {
			t.Errorf("kernel read of %s differs: %v", e.Path, err)
			continue
		}
		st, _ := os.Lstat(p)
		if uint32(st.Mode().Perm()) != e.Mode&0o777 || !st.Mode().IsRegular() {
			t.Errorf("kernel: %s mode %v", e.Path, st.Mode())
		}
	}
	for p, target := range cs.Links {
		if got, err := os.Readlink(filepath.Join(mnt, p)); err != nil || got != target {
			t.Errorf("kernel: /%s -> %q (%v), want %q", p, got, err, target)
		}
	}
}

func TestKernelReadsFreshAndInjected(t *testing.T) {
	needKernelMount(t)
	img, p := newImage(t, 256<<20, "az01-data")
	cs := bundleChangeSet("enginex", "5.3.0.5", "99-sc5000-custom.conf")
	cs.Links = map[string]string{"sc5000-custom": "enginex"}
	if err := img.Apply(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	img.Close()
	mnt := mountImage(t, p, true)
	kernelMatches(t, mnt, cs)
	// The compatibility link resolves for the deck's legacy paths.
	if b, err := os.ReadFile(filepath.Join(mnt, "sc5000-custom/bundle/boot-activate.sh")); err != nil || !bytes.HasPrefix(b, []byte("#!/bin/sh")) {
		t.Errorf("legacy path through the link: %v", err)
	}
	umount(t, mnt)
}

// fscrypt v2 through ioctls, as the deck's encrypt-fs.sh does with its tools.
func addKey(t *testing.T, mnt string, key []byte) [16]byte {
	t.Helper()
	buf := make([]byte, 80+len(key))
	le.PutUint32(buf[0:], unix.FSCRYPT_KEY_SPEC_TYPE_IDENTIFIER)
	le.PutUint32(buf[40:], uint32(len(key)))
	copy(buf[80:], key)
	d, err := os.Open(mnt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, d.Fd(), unix.FS_IOC_ADD_ENCRYPTION_KEY, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		t.Skipf("FS_IOC_ADD_ENCRYPTION_KEY: %v (no fscrypt in this kernel)", errno)
	}
	var id [16]byte
	copy(id[:], buf[8:24])
	return id
}

func setPolicy(t *testing.T, dir string, id [16]byte) {
	t.Helper()
	pol := make([]byte, 24)
	pol[0] = 2 // v2
	pol[1] = unix.FSCRYPT_MODE_AES_256_XTS
	pol[2] = unix.FSCRYPT_MODE_AES_256_CTS
	pol[3] = unix.FSCRYPT_POLICY_FLAGS_PAD_16
	copy(pol[8:], id[:])
	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, d.Fd(), unix.FS_IOC_SET_ENCRYPTION_POLICY, uintptr(unsafe.Pointer(&pol[0]))); errno != 0 {
		t.Fatalf("FS_IOC_SET_ENCRYPTION_POLICY: %v", errno)
	}
}

// rawSubtree hashes the on-disk bytes of every inode under path: the
// inode body (minus timestamps nothing should touch anyway) and all of its
// blocks. Encrypted names are read as stored, never decrypted.
func rawSubtree(t *testing.T, imgPath, path string) map[uint32][32]byte {
	t.Helper()
	ro, err := Open(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	f := ro.fs
	top, err := f.lstat(path)
	if err != nil || top == nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := map[uint32][32]byte{}
	var walk func(in *inode)
	walk = func(in *inode) {
		if _, ok := out[in.num]; ok {
			return
		}
		h := sha256.New()
		h.Write(in.raw)
		exts, meta, err := f.fileMap(in)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range exts {
			for i := uint64(0); i < uint64(e.length); i++ {
				b, _ := f.readBlock(e.phys + i)
				h.Write(b)
			}
		}
		for _, b := range meta {
			d, _ := f.readBlock(b)
			h.Write(d)
		}
		var s [32]byte
		copy(s[:], h.Sum(nil))
		out[in.num] = s
		if !in.isDir() {
			return
		}
		blocks, err := f.readDir(in)
		if err != nil {
			t.Fatal(err)
		}
		ents, _ := f.listEntries(blocks)
		for _, e := range ents[2:] {
			c, err := f.readInode(e.ino)
			if err != nil {
				t.Fatal(err)
			}
			walk(c)
		}
	}
	walk(top)
	return out
}

// TestKernelWrittenBackup: the kernel fills a /data like a used deck
// (indexed directories, xattrs, hard links, an fscrypt-encrypted
// downloads/stems tree); the injection keeps every encrypted byte, the
// result is e2fsck-clean and the kernel reads both the new files and the
// decrypted user data afterwards.
func TestKernelWrittenBackup(t *testing.T) {
	needKernelMount(t)
	img, backup := newImage(t, 512<<20, "az01-data")
	img.Close()
	mnt := mountImage(t, backup, false)
	key := bytes.Repeat([]byte{0x5a}, 64)
	id := addKey(t, mnt, key)
	plain := map[string][]byte{}
	for _, d := range []string{"downloads", "stems"} {
		os.Mkdir(filepath.Join(mnt, d), 0o755)
		setPolicy(t, filepath.Join(mnt, d), id)
		for i := 0; i < 150; i++ { // several blocks: an indexed encrypted directory
			rel := fmt.Sprintf("%s/track-%03d-%s.bin", d, i, strings.Repeat("n", 30))
			data := pattern(1000+i*97, byte(i))
			if err := os.WriteFile(filepath.Join(mnt, rel), data, 0o644); err != nil {
				t.Fatal(err)
			}
			plain[rel] = data
		}
	}
	userDirs := []string{"Music", "system/var-lib/alsa/conf.d", "system/var/overlay/lib/alsa/conf.d", "enginex/bundle/bin"}
	for _, d := range userDirs {
		os.MkdirAll(filepath.Join(mnt, d), 0o755)
	}
	for i := 0; i < 300; i++ {
		rel := fmt.Sprintf("system/var-lib/alsa/conf.d/%03d-%s.conf", i, strings.Repeat("u", 40))
		data := []byte(fmt.Sprintf("# %d\n", i))
		os.WriteFile(filepath.Join(mnt, rel), data, 0o644)
		plain[rel] = data
	}
	music := pattern(3<<20, 42)
	os.WriteFile(filepath.Join(mnt, "Music/set.flac"), music, 0o644)
	plain["Music/set.flac"] = music
	os.Link(filepath.Join(mnt, "Music/set.flac"), filepath.Join(mnt, "Music/set-link.flac"))
	plain["Music/set-link.flac"] = music
	if err := unix.Setxattr(filepath.Join(mnt, "Music/set.flac"), "user.deck", []byte("cue-points"), 0); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(mnt, "enginex/bundle/bin/sc5000-suite"), pattern(1<<20, 1), 0o755)
	os.WriteFile(filepath.Join(mnt, "enginex/gen2-locked"), []byte("1\n"), 0o644)
	os.Symlink("enginex", filepath.Join(mnt, "sc5000-custom"))
	umount(t, mnt)
	e2fsckClean(t, backup)
	if out := debugfs(t, backup, "stat /system/var-lib/alsa/conf.d"); !strings.Contains(out, "Flags: 0x81000") {
		t.Logf("note: kernel did not index the directory:\n%s", out)
	}

	encBefore := map[string]map[uint32][32]byte{"downloads": rawSubtree(t, backup, "downloads"), "stems": rawSubtree(t, backup, "stems")}
	before := fileHash(t, backup)
	out := filepath.Join(t.TempDir(), "prepared.img")
	cp, err := OpenCopy(context.Background(), mustSource(t, backup), out)
	if err != nil {
		t.Fatal(err)
	}
	mustCheck(t, cp)
	// Writing into an encrypted directory is refused before any change.
	refused := ChangeSet{Files: []Entry{memEntry("downloads/new.bin", 0o644, []byte("x"))}}
	if err := cp.Apply(context.Background(), refused); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("Apply into an encrypted directory = %v", err)
	}
	root, links, _ := cp.ChooseRuntimeDir()
	cs := bundleChangeSet(root, "5.3.0.5", "99-enginex.conf")
	cs.Links = links
	if err := cp.Apply(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	cp.Close()
	if err := VerifyFile(context.Background(), out, cs); err != nil {
		t.Fatal(err)
	}
	e2fsckClean(t, out)
	if fileHash(t, backup) != before {
		t.Fatal("the original backup changed")
	}
	for d, want := range encBefore {
		got := rawSubtree(t, out, d)
		if len(got) != len(want) {
			t.Fatalf("%s: %d inodes, had %d", d, len(got), len(want))
		}
		for ino, h := range want {
			if got[ino] != h {
				t.Fatalf("%s: encrypted inode %d changed on disk", d, ino)
			}
		}
	}
	mnt = mountImage(t, out, false)
	defer func() {
		if mnt != "" {
			umount(t, mnt)
		}
	}()
	kernelMatches(t, mnt, cs)
	addKey(t, mnt, key)
	err = filepath.WalkDir(mnt, func(p string, d fs.DirEntry, err error) error { return err })
	if err != nil {
		t.Fatal(err)
	}
	for rel, data := range plain {
		got, err := os.ReadFile(filepath.Join(mnt, rel))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("user file %s after injection: %v", rel, err)
		}
	}
	if v, err := unix.Getxattr(filepath.Join(mnt, "Music/set.flac"), "user.deck", make([]byte, 64)); err != nil || v != len("cue-points") {
		t.Errorf("xattr: %d %v", v, err)
	}
	var st unix.Stat_t
	if unix.Stat(filepath.Join(mnt, "Music/set.flac"), &st) != nil || st.Nlink != 2 {
		t.Errorf("hard link count %d", st.Nlink)
	}
	// The deck keeps writing to the injected filesystem.
	if err := os.WriteFile(filepath.Join(mnt, "downloads/after.bin"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "enginex/state.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	umount(t, mnt)
	mnt = ""
	e2fsckClean(t, out)
	ro, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	mustCheck(t, ro)
}
