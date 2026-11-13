//go:build linux

package ext4

// Independent oracles: e2fsprogs on the build host (never at runtime) and,
// when EXT4_STOCK_ROOTFS points at an unpacked Gen-2 rootfs, the deck's own
// armhf mke2fs/e2fsck run natively through the rootfs loader.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed (oracle tests run on the ARM build hosts)", tool)
		}
	}
}

func run(t *testing.T, env []string, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s: %v", name, err)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

// e2fsckClean runs e2fsck -fn and requires exit 0 (nothing to fix).
func e2fsckClean(t *testing.T, img string) {
	t.Helper()
	out, code := run(t, nil, "e2fsck", "-fn", img)
	if code != 0 {
		t.Fatalf("e2fsck -fn %s exited %d:\n%s", filepath.Base(img), code, out)
	}
}

func debugfs(t *testing.T, img string, req string) string {
	t.Helper()
	out, _ := run(t, nil, "debugfs", "-R", req, img)
	return out
}

// debugfsReadback dumps every file of cs with debugfs and compares bytes and
// permission bits, and checks every link target and every removed path.
func debugfsReadback(t *testing.T, img string, cs ChangeSet) {
	t.Helper()
	dir := t.TempDir()
	var script strings.Builder
	for i, e := range cs.Files {
		fmt.Fprintf(&script, "dump -p \"/%s\" \"%s/%d\"\n", strings.TrimPrefix(e.Path, "/"), dir, i)
	}
	sf := filepath.Join(dir, "script")
	os.WriteFile(sf, []byte(script.String()), 0o600)
	run(t, nil, "debugfs", "-f", sf, img)
	for i, e := range cs.Files {
		p := filepath.Join(dir, fmt.Sprint(i))
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("debugfs could not dump %s: %v", e.Path, err)
			continue
		}
		if sha256.Sum256(data) != e.SHA256 {
			t.Errorf("debugfs: %s differs (%d bytes)", e.Path, len(data))
		}
		st, _ := os.Stat(p)
		if uint32(st.Mode().Perm())|uint32(st.Mode()&os.ModeSetuid>>12) != e.Mode&0o777 {
			if uint32(st.Mode().Perm()) != e.Mode&0o777 {
				t.Errorf("debugfs: %s mode %o, want %o", e.Path, st.Mode().Perm(), e.Mode&0o777)
			}
		}
	}
	for p, target := range cs.Links {
		out := debugfs(t, img, "stat /"+strings.TrimPrefix(p, "/"))
		if !strings.Contains(out, "Type: symlink") || !strings.Contains(out, `"`+target+`"`) {
			t.Errorf("debugfs: /%s is not a link to %q:\n%s", p, target, out)
		}
	}
	for _, p := range cs.Remove {
		out := debugfs(t, img, "stat /"+strings.TrimPrefix(p, "/"))
		if strings.Contains(out, "Type:") {
			t.Errorf("debugfs: removed /%s is still there", p)
		}
	}
}

func TestOracleFreshImages(t *testing.T) {
	needTools(t, "e2fsck", "debugfs", "dumpe2fs")
	sizes := []struct {
		name  string
		size  int64
		label string
	}{
		{"az04-64MiB", 64 << 20, "data"},
		{"odd-300MiB", 300<<20 + 12345, "az01-data"},
		{"2.25GiB", 2304 << 20, "az01-data"},
		{"4.5GiB", 4608 << 20, "az01-data"},
	}
	for _, s := range sizes {
		t.Run(s.name, func(t *testing.T) {
			img, p := newImage(t, s.size, s.label)
			img.Close()
			e2fsckClean(t, p)
			out, _ := run(t, nil, "dumpe2fs", "-h", p)
			feat := regexp.MustCompile(`(?m)^Filesystem features:\s+(.*)$`).FindStringSubmatch(out)
			if feat == nil {
				t.Fatalf("dumpe2fs:\n%s", out)
			}
			got := strings.Fields(feat[1])
			sort.Strings(got)
			want := []string{"64bit", "dir_index", "dir_nlink", "encrypt", "extent", "extra_isize", "ext_attr",
				"filetype", "flex_bg", "has_journal", "huge_file", "large_file", "metadata_csum", "sparse_super"}
			sort.Strings(want)
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("features %v, want %v", got, want)
			}
			if !strings.Contains(out, "Filesystem volume name:   "+s.label) || !strings.Contains(out, "Filesystem state:         clean") {
				t.Errorf("dumpe2fs header:\n%s", out)
			}
			img = openRW(t, p)
			root, links, _ := img.ChooseRuntimeDir()
			cs := bundleChangeSet(root, "5.3.0.5", "99-sc5000-custom.conf")
			cs.Links = links
			if err := img.Apply(context.Background(), cs); err != nil {
				t.Fatal(err)
			}
			img.Close()
			e2fsckClean(t, p)
			debugfsReadback(t, p, cs)
			if err := VerifyFile(context.Background(), p, cs); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// backupProfiles are mke2fs feature sets a deck's /data may carry.
var backupProfiles = []struct {
	name string
	args []string
}{
	{"stock-1.47", []string{"-b", "4096", "-I", "256", "-O", "none,has_journal,ext_attr,resize_inode,dir_index,filetype,extent,64bit,flex_bg,sparse_super,large_file,huge_file,dir_nlink,extra_isize,metadata_csum,metadata_csum_seed,orphan_file,encrypt"}},
	{"legacy-uninit_bg", []string{"-b", "4096", "-I", "256", "-O", "none,has_journal,ext_attr,resize_inode,dir_index,filetype,extent,flex_bg,sparse_super,large_file,huge_file,uninit_bg,dir_nlink,extra_isize"}},
	{"csum-32bit", []string{"-b", "4096", "-I", "256", "-O", "none,has_journal,ext_attr,resize_inode,dir_index,filetype,extent,flex_bg,sparse_super,large_file,huge_file,dir_nlink,extra_isize,metadata_csum"}},
	{"1k-noflex-csum", []string{"-b", "1024", "-I", "128", "-O", "none,has_journal,ext_attr,dir_index,filetype,extent,sparse_super,large_file,metadata_csum,64bit"}},
	{"no-csum", []string{"-b", "4096", "-I", "256", "-O", "none,has_journal,ext_attr,resize_inode,dir_index,filetype,extent,flex_bg,sparse_super,large_file,huge_file,dir_nlink,extra_isize"}},
}

// populate fills a backup like a used deck: user files, an xattr, a
// hard link, a symlink, an old EngineX install and an indexed (htree)
// directory that the injection must modify.
func populate(t *testing.T, img string, legacyDir bool) map[string][32]byte {
	t.Helper()
	dir := t.TempDir()
	sentinels := map[string][32]byte{}
	var s strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&s, format+"\n", a...) }
	file := func(path string, data []byte) {
		host := filepath.Join(dir, fmt.Sprintf("f%d", len(sentinels)))
		os.WriteFile(host, data, 0o644)
		w("write %s %s", host, path)
		sentinels[path] = sha256.Sum256(data)
	}
	for _, d := range []string{"/Music", "/Engine Library", "/system", "/system/var", "/system/var/overlay",
		"/system/var/overlay/lib", "/system/var/overlay/lib/alsa", "/system/var/overlay/lib/alsa/conf.d",
		"/system/var-lib", "/system/var-lib/alsa", "/system/var-lib/alsa/conf.d", "/downloads"} {
		w("mkdir %q", d)
	}
	file("/Music/track one.flac", pattern(700<<10, 1))
	file("/Engine Library/m.db", pattern(90<<10, 2))
	file("/downloads/cached.bin", pattern(5000, 3))
	w("ea_set /Music/track\\ one.flac user.enginex sentinel")
	w("ln /Music/track\\ one.flac /Music/hardlink.flac")
	w("sif /Music/track\\ one.flac links_count 2")
	w("symlink /Music/latest /Music/track\\ one.flac")
	for i := 0; i < 400; i++ { // > 1 block: e2fsck -D indexes it
		file(fmt.Sprintf("/system/var-lib/alsa/conf.d/%03d-user-%s.conf", i, strings.Repeat("x", 20)), []byte(fmt.Sprintf("# %d\n", i)))
	}
	if legacyDir {
		w("mkdir /sc5000-custom")
		w("mkdir /sc5000-custom/bundle")
		file("/sc5000-custom/settings.json", []byte(`{"user":"setting"}`))
	} else {
		w("mkdir /enginex")
		w("mkdir /enginex/bundle")
		w("mkdir /enginex/bundle/bin")
		file("/enginex/bundle/bin/sc5000-suite", pattern(2<<20, 7)) // old version, replaced
		file("/enginex/gen2-locked", []byte("1\n"))
		file("/enginex/settings.json", []byte(`{"user":"setting"}`))
		w("symlink /sc5000-custom enginex")
	}
	sf := filepath.Join(dir, "populate")
	os.WriteFile(sf, []byte(s.String()), 0o600)
	if out, code := run(t, nil, "debugfs", "-w", "-f", sf, img); code != 0 {
		t.Fatalf("debugfs populate: %d\n%s", code, out)
	}
	if out, code := run(t, nil, "e2fsck", "-fyD", img); code > 1 {
		t.Fatalf("e2fsck -fyD: %d\n%s", code, out)
	}
	if !strings.Contains(debugfs(t, img, "stat /system/var-lib/alsa/conf.d"), "Flags: 0x81000") &&
		!strings.Contains(debugfs(t, img, "htree /system/var-lib/alsa/conf.d"), "Root node dump") {
		t.Fatalf("fixture directory is not indexed:\n%s", debugfs(t, img, "stat /system/var-lib/alsa/conf.d"))
	}
	delete(sentinels, "/enginex/bundle/bin/sc5000-suite")
	delete(sentinels, "/enginex/gen2-locked")
	return sentinels
}

func mkfsBackup(t *testing.T, args []string, size int64, label string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "backup.img")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, size); err != nil {
		t.Fatal(err)
	}
	a := append([]string{"-F", "-q", "-t", "ext4", "-L", label}, args...)
	a = append(a, p)
	if out, code := run(t, []string{"MKE2FS_CONFIG=/dev/null"}, "mke2fs", a...); code != 0 {
		t.Fatalf("mke2fs %v: %s", a, out)
	}
	return p
}

func TestOracleBackupProfiles(t *testing.T) {
	needTools(t, "e2fsck", "debugfs", "mke2fs")
	for _, prof := range backupProfiles {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%v", prof.name, legacy), func(t *testing.T) {
				backup := mkfsBackup(t, prof.args, 256<<20, "az01-data")
				sentinels := populate(t, backup, legacy)
				e2fsckClean(t, backup)
				testInjectBackup(t, backup, sentinels, legacy)
			})
		}
	}
}

func testInjectBackup(t *testing.T, backup string, sentinels map[string][32]byte, legacy bool) {
	t.Helper()
	before := fileHash(t, backup)
	src, closer, err := OpenFileSource(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	out := filepath.Join(t.TempDir(), "prepared.img")
	img, err := OpenCopy(context.Background(), src, out)
	if err != nil {
		t.Fatalf("OpenCopy: %v", err)
	}
	root, links, err := img.ChooseRuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	if legacy && (root != "sc5000-custom" || len(links) != 0) || !legacy && (root != "enginex" || len(links) != 0) {
		t.Fatalf("ChooseRuntimeDir = %q %v", root, links)
	}
	cs := bundleChangeSet(root, "5.3.0.5", "99-enginex.conf")
	cs.Links = links
	if err := img.Apply(context.Background(), cs); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(context.Background(), out, cs); err != nil {
		t.Fatal(err)
	}
	e2fsckClean(t, out)
	debugfsReadback(t, out, cs)
	if fileHash(t, backup) != before {
		t.Fatal("the original backup changed")
	}
	// User data, xattr, hard link and symlink survive byte for byte.
	ro, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	for p, want := range sentinels {
		got, err := ro.ReadFile(p, 8<<20)
		if err != nil || sha256.Sum256(got) != want {
			t.Errorf("sentinel %s changed: %v", p, err)
		}
	}
	if !strings.Contains(debugfs(t, out, "ea_get /Music/track\\ one.flac user.enginex"), "sentinel") {
		t.Error("xattr lost")
	}
	if !strings.Contains(debugfs(t, out, "stat /Music/hardlink.flac"), "Links: 2") {
		t.Error("hard link count lost")
	}
	if tgt, _ := ro.Readlink("/Music/latest"); tgt != "/Music/track one.flac" {
		t.Errorf("symlink target %q", tgt)
	}
	if got, _ := ro.ReadFile("/system/var-lib/alsa/conf.d/123-user-xxxxxxxxxxxxxxxxxxxx.conf", 64); !bytes.Equal(got, []byte("# 123\n")) {
		t.Errorf("entry in the linearized directory lost: %q", got)
	}
	// Injecting again over the prepared image (a deck updated twice).
	img2, err := OpenCopy(context.Background(), mustSource(t, out), out+".2")
	if err != nil {
		t.Fatal(err)
	}
	cs2 := bundleChangeSet(root, "5.3.0.6", "99-enginex.conf")
	if err := img2.Apply(context.Background(), cs2); err != nil {
		t.Fatal(err)
	}
	img2.Close()
	if err := VerifyFile(context.Background(), out+".2", cs2); err != nil {
		t.Fatal(err)
	}
	e2fsckClean(t, out+".2")
}

func mustSource(t *testing.T, p string) Source {
	t.Helper()
	s, c, err := OpenFileSource(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return s
}

// stockTool returns a command line running a deck binary natively through
// the rootfs dynamic loader.
func stockTool(t *testing.T, tool string) (string, []string, []string) {
	t.Helper()
	r := os.Getenv("EXT4_STOCK_ROOTFS")
	if r == "" {
		t.Skip("EXT4_STOCK_ROOTFS not set (unpacked Gen-2 rootfs with e2fsprogs)")
	}
	bin := filepath.Join(r, tool)
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
	args := []string{"--library-path", filepath.Join(r, "lib") + ":" + filepath.Join(r, "usr/lib"), bin}
	return loaders[0], args, []string{"MKE2FS_CONFIG=" + filepath.Join(r, "etc/mke2fs.conf")}
}

// TestOracleStockDeckTools runs the deck's own e2fsprogs: its boot-time
// "fsck.ext4 -y" must leave a fresh or injected image byte-identical, and a
// /data made by the deck's own az0x-data-mkfs command must be injectable.
func TestOracleStockDeckTools(t *testing.T) {
	needTools(t, "debugfs", "e2fsck")
	loader, fsckArgs, env := stockTool(t, "sbin/fsck.ext4")
	_, mkfsArgs, _ := stockTool(t, "sbin/mkfs.ext4")
	deckFsck := func(p string) {
		t.Helper()
		before := fileHash(t, p)
		out, code := run(t, env, loader, append(append([]string{}, fsckArgs...), "-fy", p)...)
		if code != 0 {
			t.Fatalf("deck fsck.ext4 -fy exited %d:\n%s", code, out)
		}
		if fileHash(t, p) != before {
			t.Fatalf("deck fsck.ext4 changed the image:\n%s", out)
		}
	}
	img, p := newImage(t, 512<<20, "az01-data")
	cs := bundleChangeSet("enginex", "5.3.0.5", "99-sc5000-custom.conf")
	cs.Links = map[string]string{"sc5000-custom": "enginex"}
	if err := img.Apply(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	img.Close()
	deckFsck(p)

	backup := filepath.Join(t.TempDir(), "deck-data.img")
	os.WriteFile(backup, nil, 0o600)
	os.Truncate(backup, 384<<20)
	if out, code := run(t, env, loader, append(append([]string{}, mkfsArgs...), "-O", "encrypt", "-L", "az01-data", backup)...); code != 0 {
		t.Fatalf("deck mkfs.ext4: %d\n%s", code, out)
	}
	feat, _ := run(t, nil, "dumpe2fs", "-h", backup)
	t.Logf("deck mkfs profile: %s", regexp.MustCompile(`(?m)^Filesystem features:.*$`).FindString(feat))
	sentinels := populate(t, backup, false)
	deckFsck(backup)
	testInjectBackup(t, backup, sentinels, false)
}
