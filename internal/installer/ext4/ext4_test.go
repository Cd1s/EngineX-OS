package ext4

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var fixedNow = func() time.Time { return time.Unix(1794000000, 123456789) }

func memEntry(path string, mode uint32, data []byte) Entry {
	return Entry{Path: path, Mode: mode, Size: int64(len(data)), SHA256: sha256.Sum256(data),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed ^ byte(i>>9)
	}
	return b
}

func newImage(t *testing.T, size int64, label string) (*Image, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "data.img")
	img, err := Create(context.Background(), p, CreateOptions{Size: size, Label: label, Now: fixedNow})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return img, p
}

func mustCheck(t *testing.T, img *Image) {
	t.Helper()
	if err := img.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func reopen(t *testing.T, img *Image, p string) *Image {
	t.Helper()
	if err := img.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ro, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { ro.Close() })
	return ro
}

// bundleChangeSet mirrors what the installer injects (host-install-userdata.py).
func bundleChangeSet(root, version string, alsaName string) ChangeSet {
	cs := ChangeSet{Links: map[string]string{}}
	add := func(p string, mode uint32, data []byte) { cs.Files = append(cs.Files, memEntry(p, mode, data)) }
	cs.Directories = []string{root, root + "/bundle", root + "/bundle/bin", root + "/bundle/lib", root + "/ui",
		"system/var/overlay/lib/alsa/conf.d", "system/var-lib/alsa/conf.d"}
	add(root+"/bundle/bin/sc5000-suite", 0o755, pattern(3<<20+123, 1))
	add(root+"/bundle/lib/libengine-ui-hook.so", 0o755, pattern(400<<10, 2))
	add(root+"/bundle/boot-activate.sh", 0o755, []byte("#!/bin/sh\necho activate\n"))
	add(root+"/bundle/version.txt", 0o644, []byte(version+"\n"))
	add(root+"/bundle/"+alsaName, 0o644, []byte("# alsa hook\n"))
	for i := 0; i < 40; i++ {
		add(fmt.Sprintf("%s/ui/qml/Pack%02d/File%03d.qml", root, i%5, i), 0o644, pattern(1000+i*37, byte(i)))
	}
	add(root+"/ui/.pack-version", 0o644, []byte(version+"\n"))
	add(root+"/version.txt", 0o644, []byte(version+"\n"))
	add(root+"/gen2-unlocked", 0o644, []byte("1\n"))
	add(root+"/authorized_keys", 0o600, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest test\n"))
	add("system/var/overlay/lib/alsa/conf.d/"+alsaName, 0o644, []byte("# alsa hook\n"))
	add("system/var-lib/alsa/conf.d/"+alsaName, 0o644, []byte("# alsa hook\n"))
	cs.Remove = []string{root + "/gen2-locked"}
	return cs
}

func TestCreateFreshProfile(t *testing.T) {
	img, p := newImage(t, 64<<20, "data")
	mustCheck(t, img)
	pr := img.Profile()
	if pr.Label != "data" || pr.BlockSize != 4096 || pr.Blocks != 16384 || !pr.HasJournal {
		t.Fatalf("profile %+v", pr)
	}
	ro := reopen(t, img, p)
	mustCheck(t, ro)
	if k, _ := ro.Lstat("lost+found"); k != Directory {
		t.Fatalf("lost+found is %v", k)
	}
}

func TestApplyVerifyRoundTrip(t *testing.T) {
	for _, alsa := range []string{"99-sc5000-custom.conf", "99-enginex.conf"} {
		t.Run(alsa, func(t *testing.T) {
			img, p := newImage(t, 64<<20, "data")
			root, links, err := img.ChooseRuntimeDir()
			if err != nil || root != "enginex" || links["sc5000-custom"] != "enginex" {
				t.Fatalf("ChooseRuntimeDir = %q %v %v", root, links, err)
			}
			cs := bundleChangeSet(root, "5.3.0.5", alsa)
			cs.Links = links
			if err := img.Apply(context.Background(), cs); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			mustCheck(t, img)
			ro := reopen(t, img, p)
			mustCheck(t, ro)
			if err := ro.Verify(context.Background(), cs); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got, _ := ro.ReadFile("/enginex/gen2-unlocked", 16); string(got) != "1\n" {
				t.Fatalf("gen2-unlocked = %q", got)
			}
			if m, _, _, _, _ := ro.Stat("/enginex/authorized_keys"); m&0o7777 != 0o600 {
				t.Fatalf("authorized_keys mode %o", m)
			}
			if err := VerifyFile(context.Background(), p, cs); err != nil {
				t.Fatalf("VerifyFile: %v", err)
			}
		})
	}
}

func TestRepeatedInjectionReplacesAndFrees(t *testing.T) {
	img, p := newImage(t, 64<<20, "az01-data")
	cs1 := bundleChangeSet("enginex", "5.3.0.4", "99-sc5000-custom.conf")
	if err := img.Apply(context.Background(), cs1); err != nil {
		t.Fatal(err)
	}
	free1 := img.Profile().FreeBlocks
	cs2 := bundleChangeSet("enginex", "5.3.0.5", "99-sc5000-custom.conf")
	if err := img.Apply(context.Background(), cs2); err != nil {
		t.Fatal(err)
	}
	mustCheck(t, img)
	if free2 := img.Profile().FreeBlocks; free2 != free1 {
		t.Fatalf("free blocks %d after reinjection, %d after first", free2, free1)
	}
	ro := reopen(t, img, p)
	if err := ro.Verify(context.Background(), cs2); err != nil {
		t.Fatal(err)
	}
	if got, _ := ro.ReadFile("enginex/version.txt", 64); string(got) != "5.3.0.5\n" {
		t.Fatalf("version %q", got)
	}
}

func TestCapacityPreflightLeavesImageUnchanged(t *testing.T) {
	img, p := newImage(t, 64<<20, "data")
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	before := fileHash(t, p)
	img2 := openRW(t, p)
	big := memEntry("enginex/huge.bin", 0o644, make([]byte, 70<<20))
	err := img2.Apply(context.Background(), ChangeSet{Files: []Entry{big}})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("Apply = %v, want ErrNoSpace", err)
	}
	img2.Close()
	if after := fileHash(t, p); after != before {
		t.Fatalf("image changed after a refused change")
	}
}

func openRW(t *testing.T, p string) *Image {
	t.Helper()
	fh, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := fh.Stat()
	f, err := openFS(fh, st.Size(), true)
	if err != nil {
		t.Fatal(err)
	}
	f.now = fixedNow
	return &Image{path: p, file: fh, fs: f}
}

func fileHash(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestSymlinkInPathIsRefused(t *testing.T) {
	img, _ := newImage(t, 64<<20, "data")
	if err := img.Apply(context.Background(), ChangeSet{Links: map[string]string{"enginex": "/etc"}}); err != nil {
		t.Fatal(err)
	}
	err := img.Apply(context.Background(), ChangeSet{Files: []Entry{memEntry("enginex/passwd", 0o644, []byte("x"))}})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Apply through a symlink = %v, want ErrUnsafePath", err)
	}
	for _, bad := range []string{"../x", "a/../b", "a//b", `a\b`, "a/./b"} {
		if err := img.Apply(context.Background(), ChangeSet{Directories: []string{bad}}); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("path %q: %v", bad, err)
		}
	}
	mustCheck(t, img)
}

func TestFragmentedFileUsesDeepExtentTree(t *testing.T) {
	img, p := newImage(t, 64<<20, "data")
	var cs ChangeSet
	for i := 0; i < 3000; i++ {
		cs.Files = append(cs.Files, memEntry(fmt.Sprintf("frag/d%02d/f%04d", i%30, i), 0o644, pattern(4096, byte(i))))
	}
	if err := img.Apply(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	var rm ChangeSet
	for i := 0; i < 3000; i += 2 {
		rm.Remove = append(rm.Remove, fmt.Sprintf("frag/d%02d/f%04d", i%30, i))
	}
	if err := img.Apply(context.Background(), rm); err != nil {
		t.Fatal(err)
	}
	mustCheck(t, img)
	// Free space is now mostly single blocks between files.
	data := pattern(img.Profile().BlockSize*1700, 9)
	big := ChangeSet{Files: []Entry{memEntry("big/fragmented.bin", 0o644, data)}}
	if err := img.Apply(context.Background(), big); err != nil {
		t.Fatal(err)
	}
	mustCheck(t, img)
	in, err := img.fs.lstat("big/fragmented.bin")
	if err != nil {
		t.Fatal(err)
	}
	exts, meta, err := img.fs.fileMap(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) <= 4 || len(meta) == 0 {
		t.Fatalf("expected a multi-level tree, got %d extents and %d tree blocks", len(exts), len(meta))
	}
	ro := reopen(t, img, p)
	mustCheck(t, ro)
	if err := ro.Verify(context.Background(), big); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCopyNeverTouchesOriginal(t *testing.T) {
	img, p := newImage(t, 64<<20, "az01-data")
	if err := img.Apply(context.Background(), ChangeSet{Files: []Entry{memEntry("Music/user.flac", 0o644, pattern(1<<20, 3))}}); err != nil {
		t.Fatal(err)
	}
	img.Close()
	before := fileHash(t, p)
	src, closer, err := OpenFileSource(p)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if _, err := OpenCopy(context.Background(), src, p); err == nil {
		t.Fatal("OpenCopy onto the backup itself succeeded")
	}
	dst := filepath.Join(t.TempDir(), "copy.img")
	cp, err := OpenCopy(context.Background(), src, dst)
	if err != nil {
		t.Fatal(err)
	}
	cs := bundleChangeSet("enginex", "5.3.0.5", "99-enginex.conf")
	if err := cp.Apply(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(context.Background(), dst, cs); err != nil {
		t.Fatal(err)
	}
	if after := fileHash(t, p); after != before {
		t.Fatal("the original backup changed")
	}
	ro, _ := Open(dst)
	defer ro.Close()
	if got, _ := ro.ReadFile("Music/user.flac", 2<<20); !bytes.Equal(got, pattern(1<<20, 3)) {
		t.Fatal("user file changed")
	}
}

func TestRefusedBackupsLeaveNoOutput(t *testing.T) {
	img, p := newImage(t, 64<<20, "data")
	img.Close()
	good, _ := os.ReadFile(p)
	mutate := func(fn func(sb []byte)) []byte {
		b := append([]byte(nil), good...)
		sb := b[superblockOffset : superblockOffset+superblockSize]
		fn(sb)
		le.PutUint32(sb[sbChecksum:], crc32c(^uint32(0), sb[:sbChecksum]))
		return b
	}
	cases := map[string]struct {
		data []byte
		want error
	}{
		"needs_recovery": {mutate(func(sb []byte) { setFlag(sb, sbFeatureIncompat, incompatRecover) }), ErrDirty},
		"not_clean":      {mutate(func(sb []byte) { le.PutUint16(sb[sbState:], 0) }), ErrDirty},
		"errors":         {mutate(func(sb []byte) { le.PutUint16(sb[sbState:], 3) }), ErrDirty},
		"inline_data":    {mutate(func(sb []byte) { setFlag(sb, sbFeatureIncompat, 0x8000) }), ErrUnsupported},
		"quota":          {mutate(func(sb []byte) { setFlag(sb, sbFeatureROCompat, 0x100) }), ErrUnsupported},
		"bad_csum":       {flipByte(good, superblockOffset+sbVolumeName), ErrCorrupt},
		"not_ext4":       {make([]byte, 1<<20), ErrNotExt4},
		"truncated":      {good[:32<<20], ErrCorrupt},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "out.img")
			_, err := OpenCopy(context.Background(), bytesSource(c.data), dst)
			if !errors.Is(err, c.want) {
				t.Fatalf("OpenCopy = %v, want %v", err, c.want)
			}
			if _, err := os.Stat(dst); !os.IsNotExist(err) {
				t.Fatalf("output exists after refusal: %v", err)
			}
		})
	}
}

type bytesSource []byte

func (b bytesSource) ReadAt(p []byte, off int64) (int, error) {
	return bytes.NewReader(b).ReadAt(p, off)
}
func (b bytesSource) Size() int64 { return int64(len(b)) }

func TestEntryMismatchFails(t *testing.T) {
	img, _ := newImage(t, 64<<20, "data")
	e := memEntry("enginex/x", 0o644, []byte("hello"))
	e.SHA256[0] ^= 1
	if err := img.Apply(context.Background(), ChangeSet{Files: []Entry{e}}); err == nil {
		t.Fatal("Apply accepted content with the wrong SHA-256")
	}
	e = memEntry("enginex/y", 0o644, []byte("hello"))
	e.Size = 4
	if err := img.Apply(context.Background(), ChangeSet{Files: []Entry{e}}); err == nil {
		t.Fatal("Apply accepted a longer stream")
	}
}

func setFlag(sb []byte, off int, bit uint32) { le.PutUint32(sb[off:], le.Uint32(sb[off:])|bit) }

func flipByte(b []byte, off int) []byte {
	c := append([]byte(nil), b...)
	c[off] ^= 1
	return c
}

func TestJournalWithTransactionsIsRefused(t *testing.T) {
	img, p := newImage(t, 64<<20, "data")
	j, err := img.fs.readInode(journalIno)
	if err != nil {
		t.Fatal(err)
	}
	exts, _, _ := img.fs.fileMap(j)
	img.Close()
	data, _ := os.ReadFile(p)
	be.PutUint32(data[int64(exts[0].phys)*4096+0x1C:], 1) // s_start: a log to replay
	dst := filepath.Join(t.TempDir(), "out.img")
	if _, err := OpenCopy(context.Background(), bytesSource(data), dst); !errors.Is(err, ErrDirty) {
		t.Fatalf("OpenCopy = %v, want ErrDirty", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("output exists after refusal")
	}
}

func TestEncryptedDirectoryIsNeverModified(t *testing.T) {
	img, _ := newImage(t, 64<<20, "data")
	if err := img.Apply(context.Background(), ChangeSet{Directories: []string{"downloads"}}); err != nil {
		t.Fatal(err)
	}
	in, _ := img.fs.lstat("downloads")
	in.setFlags(in.flags() | flagEncrypt)
	if err := img.fs.writeInode(in); err != nil {
		t.Fatal(err)
	}
	for _, cs := range []ChangeSet{
		{Files: []Entry{memEntry("downloads/x", 0o644, []byte("x"))}},
		{Directories: []string{"downloads/sub"}},
		{Remove: []string{"downloads/old"}},
		{Links: map[string]string{"downloads/l": "x"}},
	} {
		if err := img.Apply(context.Background(), cs); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Apply %+v = %v, want ErrUnsupported", cs, err)
		}
	}
	mustCheck(t, img)
}
