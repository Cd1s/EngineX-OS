//go:build linux

package bridge_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #321 review R3, on a real FAT32 volume (a loop device; root only). A pull during an older
// build's write-back can leave an ANLZ file or the export naming clusters the FAT has freed, the
// old content still in them. A pass that allocates anything on such a stick can take those
// clusters: R2 kept the stale entries aside and wrote the new files, a new export.pdb took the
// cluster the stale ANLZ0000.DAT named, and a computer's disk check (fsck.fat -a, which reaches
// USBANLZ before rekordbox) cut that export.pdb to 0 bytes. The deck's edit it held was recorded
// as written and never written again. Now such a stick is not written at all: the edits stay
// pending in Engine's library, and the first pass after a disk check writes them.

// fat32Image reads and patches a FAT32 image (or reads a loop device): enough to find an entry by
// path, follow its chain and point an entry at other clusters.
type fat32Image struct {
	t                                  *testing.T
	f                                  *os.File
	bps, spc, rsvd, nfats, fatSz, data int64
	rootClus, clusters, fsinfo         int64
}

func openFAT32(t *testing.T, path string, write bool) *fat32Image {
	t.Helper()
	flag := os.O_RDONLY
	if write {
		flag = os.O_RDWR
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b [512]byte
	if _, err = f.ReadAt(b[:], 0); err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	v := &fat32Image{t: t, f: f, bps: int64(le.Uint16(b[11:])), spc: int64(b[13]), rsvd: int64(le.Uint16(b[14:])),
		nfats: int64(b[16]), fatSz: int64(le.Uint32(b[36:])), rootClus: int64(le.Uint32(b[44:])), fsinfo: int64(le.Uint16(b[48:]))}
	if le.Uint16(b[22:]) != 0 || v.fatSz == 0 {
		t.Fatal("not FAT32")
	}
	v.data = v.rsvd + v.nfats*v.fatSz
	v.clusters = (int64(le.Uint32(b[32:]))-v.data)/v.spc + 2
	return v
}

func (v *fat32Image) close() { _ = v.f.Close() }

func (v *fat32Image) clusterSize() int64 { return v.spc * v.bps }

func (v *fat32Image) clusterOff(c int64) int64 { return (v.data + (c-2)*v.spc) * v.bps }

func (v *fat32Image) next(c int64) int64 {
	var e [4]byte
	if _, err := v.f.ReadAt(e[:], v.rsvd*v.bps+c*4); err != nil {
		v.t.Fatal(err)
	}
	return int64(binary.LittleEndian.Uint32(e[:]) & 0x0fffffff)
}

// chain is the clusters from c on, and whether it ends in an end-of-chain mark (rather than a
// free or reserved entry, or a loop).
func (v *fat32Image) chain(c int64) ([]int64, bool) {
	var out []int64
	for len(out) <= int(v.clusters) {
		if c < 2 || c >= v.clusters {
			return out, false
		}
		out = append(out, c)
		n := v.next(c)
		if n >= 0x0ffffff8 {
			return out, true
		}
		c = n
	}
	return out, false
}

type fat32Entry struct {
	name        string
	off         int64 // of the 32-byte entry in the image
	start, size int64
	dir         bool
}

// list reads the directory whose first cluster is c.
func (v *fat32Image) list(c int64) []fat32Entry {
	clus, _ := v.chain(c)
	var out []fat32Entry
	var lfn []uint16
	for _, cl := range clus {
		buf := make([]byte, v.clusterSize())
		if _, err := v.f.ReadAt(buf, v.clusterOff(cl)); err != nil {
			v.t.Fatal(err)
		}
		for i := 0; i < len(buf); i += 32 {
			e := buf[i : i+32]
			switch {
			case e[0] == 0:
				return out
			case e[0] == 0xe5:
				lfn = nil
				continue
			case e[11] == 0x0f:
				var part []uint16
				for _, r := range [][2]int{{1, 11}, {14, 26}, {28, 32}} {
					for j := r[0]; j < r[1]; j += 2 {
						part = append(part, binary.LittleEndian.Uint16(e[j:]))
					}
				}
				if e[0]&0x40 != 0 {
					lfn = nil
				}
				lfn = append(part, lfn...)
				continue
			}
			name := ""
			if lfn != nil {
				for k, u := range lfn {
					if u == 0 {
						lfn = lfn[:k]
						break
					}
				}
				name = string(utf16.Decode(lfn))
			} else {
				name = strings.TrimRight(string(e[0:8]), " ")
				if ext := strings.TrimRight(string(e[8:11]), " "); ext != "" {
					name += "." + ext
				}
			}
			lfn = nil
			le := binary.LittleEndian
			out = append(out, fat32Entry{name: name, off: v.clusterOff(cl) + int64(i),
				start: int64(le.Uint16(e[20:]))<<16 | int64(le.Uint16(e[26:])), size: int64(le.Uint32(e[28:])), dir: e[11]&0x10 != 0})
		}
	}
	return out
}

// lookup finds rel ("a/b/c") from the root directory.
func (v *fat32Image) lookup(rel string) fat32Entry {
	v.t.Helper()
	c := v.rootClus
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		found := false
		for _, e := range v.list(c) {
			if strings.EqualFold(e.name, p) {
				if i == len(parts)-1 {
					return e
				}
				c, found = e.start, true
				break
			}
		}
		if !found {
			v.t.Fatalf("%s: %s not found on the volume", rel, p)
		}
	}
	return fat32Entry{}
}

// owned reports whether the FAT gives e every cluster its size needs, in one chain that ends there.
func (v *fat32Image) owned(e fat32Entry) bool {
	c, end := v.chain(e.start)
	return end && int64(len(c)) == (e.size+v.clusterSize()-1)/v.clusterSize()
}

// setStart points the entry at e.off to cluster c.
func (v *fat32Image) setStart(e fat32Entry, c int64) {
	var s [4]byte
	binary.LittleEndian.PutUint16(s[0:], uint16(c>>16))
	binary.LittleEndian.PutUint16(s[2:], uint16(c))
	if _, err := v.f.WriteAt(s[0:2], e.off+20); err != nil {
		v.t.Fatal(err)
	}
	if _, err := v.f.WriteAt(s[2:4], e.off+26); err != nil {
		v.t.Fatal(err)
	}
}

// free marks cluster c free in every FAT.
func (v *fat32Image) free(c int64) {
	for i := int64(0); i < v.nfats; i++ {
		if _, err := v.f.WriteAt(make([]byte, 4), (v.rsvd+i*v.fatSz)*v.bps+c*4); err != nil {
			v.t.Fatal(err)
		}
	}
}

// countFree writes the number of free clusters into FSInfo, as vfat counts them.
func (v *fat32Image) countFree() {
	n := uint32(0)
	for c := int64(2); c < v.clusters; c++ {
		if v.next(c) == 0 {
			n++
		}
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], n)
	if _, err := v.f.WriteAt(b[:], v.fsinfo*v.bps+488); err != nil {
		v.t.Fatal(err)
	}
}

func vfatRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// vfatMount attaches img to a loop device and mounts it as the deck does; the returned function
// unmounts and detaches.
func vfatMount(t *testing.T, img, mnt string) (dev string, unmount func()) {
	t.Helper()
	return fsMount(t, "vfat", img, mnt)
}

// fsMount is vfatMount for a vfat or an exfat image.
func fsMount(t *testing.T, fsType, img, mnt string) (dev string, unmount func()) {
	t.Helper()
	opts := "utf8,flush"
	if fsType == "exfat" {
		opts = "rw"
	}
	dev = vfatRun(t, "losetup", "-f", "--show", img)
	if out, err := exec.Command("mount", "-t", fsType, "-o", opts, dev, mnt).CombinedOutput(); err != nil {
		_ = exec.Command("losetup", "-d", dev).Run()
		t.Fatalf("mount: %v %s", err, out)
	}
	done := false
	unmount = func() {
		if done {
			return
		}
		done = true
		vfatRun(t, "umount", mnt)
		vfatRun(t, "losetup", "-d", dev)
	}
	t.Cleanup(unmount)
	return dev, unmount
}

// vfatReadOnly reports whether mnt is now mounted read-only (vfat's answer to a FAT error).
func vfatReadOnly(t *testing.T, mnt string) bool {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 3 && f[1] == mnt {
			return strings.HasPrefix(f[3], "ro")
		}
	}
	t.Fatalf("%s not mounted", mnt)
	return false
}

// vfatImage makes, in work, a FAT32 image with 32 KiB clusters, as a large stick has, and a
// directory to mount it on; it skips unless this runs as root with loop devices and the tools.
func vfatImage(t *testing.T, work string) (img, mnt string) {
	t.Helper()
	return vfatImageOf(t, work, 2112, 64)
}

// vfatImageOf is vfatImage with mb MiB and spc sectors of 512 bytes per cluster.
func vfatImageOf(t *testing.T, work string, mb int64, spc int) (img, mnt string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root for a loop device")
	}
	for _, tool := range []string{"mkfs.fat", "losetup", "mount", "umount", "cp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s: %v", tool, err)
		}
	}
	if _, err := os.Stat("/dev/loop-control"); err != nil {
		t.Skipf("no loop devices: %v", err)
	}
	img, mnt = filepath.Join(work, "stick.img"), filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(mb << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	vfatRun(t, "mkfs.fat", "-F", "32", "-s", strconv.Itoa(spc), "-S", "512", img)
	return img, mnt
}

func issue321Hash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// issue321Changed says where image b differs from image a: the boot sector, FSInfo, the FATs, or a
// cluster (the first 16 places).
func issue321Changed(t *testing.T, a, b string) string {
	t.Helper()
	out, _ := exec.Command("cmp", "-l", a, b).Output()
	v := openFAT32(t, b, false)
	defer v.close()
	var where []string
	last := ""
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		off, _ := strconv.ParseInt(f[0], 10, 64)
		off--
		sec := off / v.bps
		w := ""
		switch {
		case sec == 0 || sec == 6:
			w = "boot sector"
		case sec == v.fsinfo || sec == v.fsinfo+6:
			w = "FSInfo"
		case sec < v.rsvd:
			w = "reserved sector " + strconv.FormatInt(sec, 10)
		case sec < v.data:
			w = "FAT entry " + strconv.FormatInt((off-v.rsvd*v.bps)%(v.fatSz*v.bps)/4, 10)
		default:
			w = "cluster " + strconv.FormatInt((sec-v.data)/v.spc+2, 10) + " byte " + strconv.FormatInt(off%v.clusterSize(), 10)
		}
		if w != last && len(where) < 16 {
			where = append(where, w+" ("+f[1]+"->"+f[2]+")")
		}
		last = w
	}
	return strings.Join(where, ", ")
}

// issue321Whole reports whether the stick's file rel reads whole as its kind of file.
func issue321Whole(stick, rel string) bool {
	p := filepath.Join(stick, filepath.FromSlash(rel))
	if strings.HasSuffix(rel, ".pdb") {
		_, err := rekordbox.ReadPDB(p)
		return err == nil
	}
	b, err := os.ReadFile(p)
	if err == nil {
		_, err = rekordbox.ParseAnalysis(b)
	}
	return err == nil
}

// The reviewer's two scenarios: 4 KiB and 32 KiB clusters, the stick nearly full, a stale
// ANLZ0000.DAT and a stale export.pdb.
func TestIssue321AStickWithStaleEntriesIsNotWrittenUntilADiskCheck(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	for _, tc := range []struct {
		name   string
		spc    int
		cutPDB bool
	}{
		{"4KiB clusters", 8, true},
		{"32KiB clusters", 64, true},
		{"stale ANLZ only", 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) { issue321StaleStick(t, tc.spc, tc.cutPDB) })
	}
}

func issue321StaleStick(t *testing.T, spc int, cutPDB bool) {
	ctx := context.Background()
	work := t.TempDir()
	img, mnt := vfatImageOf(t, work, 64, spc)
	opts := bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true}
	const cue, genre = "Stale 321", "Stale Genre 321"
	targets := []string{"PIONEER/USBANLZ/P001/ANLZ0000.DAT"}
	if cutPDB {
		targets = append(targets, "PIONEER/rekordbox/export.pdb")
	}

	// A stick without OneLibrary (its metadata goes to export.pdb, #314), converted, a deck edit
	// written back once (the stick an older build wrote); then the DJ sets a hot cue and a genre,
	// and the stick is nearly full (2 MiB left).
	src := setupWriteBackStick(t, false)
	mp3s, err := filepath.Glob(filepath.Join(src, "Contents", "*", "*", "*.mp3"))
	if err != nil || len(mp3s) == 0 {
		t.Fatalf("fixture mp3s: %v %v", mp3s, err)
	}
	for _, p := range mp3s {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		// Give the header fixture's Xing tag ~10.4 s of frames (matching LargePDB's 8 s track)
		// so a 1.5 s/3 s deck cue remains within SampleCount after ConvertAtMount re-imports.
		binary.BigEndian.PutUint32(b[44:48], 400)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, unmount := vfatMount(t, img, mnt)
	issue321Copy(t, src, mnt)
	if _, err := convertWriteBackAtTest(ctx, mnt); err != nil {
		t.Fatal(err)
	}
	injectDeckCueEdit(t, mnt)
	if res, err := bridge.WriteBackCues(ctx, mnt, opts); err != nil || res.FilesUpdated == 0 {
		t.Fatalf("first pass: %+v %v", res, err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(mnt, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	issue321Settle()
	issue321Edit(t, db, 1, cue, 1500)
	if _, err = db.Exec(`UPDATE Track SET genre=? WHERE pdbImportKey=1`, genre); err != nil {
		t.Fatal(err)
	}
	db.Close()
	issue321Settle()
	var vfs syscall.Statfs_t
	if err = syscall.Statfs(mnt, &vfs); err != nil {
		t.Fatal(err)
	}
	fill, err := os.Create(filepath.Join(mnt, "FILL.BIN"))
	if err != nil {
		t.Fatal(err)
	}
	if err = fill.Truncate(int64(vfs.Bavail)*int64(vfs.Bsize) - 2<<20); err != nil {
		t.Fatal(err)
	}
	fill.Close()
	unmount()

	// Where the pass writes each new file: a run on a copy.
	dry := filepath.Join(work, "dry.img")
	vfatRun(t, "cp", "--sparse=always", img, dry)
	_, unmountDry := vfatMount(t, dry, mnt)
	if res, err := bridge.WriteBackCues(ctx, mnt, opts); err != nil || res.FilesUpdated == 0 {
		t.Fatalf("pass on the copy: %+v %v", res, err)
	}
	unmountDry()
	v := openFAT32(t, dry, false)
	pdbAt := v.lookup("PIONEER/rekordbox/export.pdb").start
	v.close()
	if err = os.Remove(dry); err != nil {
		t.Fatal(err)
	}

	// The stale entries a pull during an older build's pass leaves: ANLZ0000.DAT names the cluster
	// where the pass writes the new export.pdb, which holds its content and is free in the FAT (its
	// own cluster stays allocated, named by nothing); when cutPDB is true, export.pdb's last cluster
	// is also free in the FAT. All still read.
	v = openFAT32(t, img, true)
	dat := v.lookup(targets[0])
	if v.next(pdbAt) != 0 {
		t.Fatalf("cluster %d, where the pass writes export.pdb, is not free", pdbAt)
	}
	buf := make([]byte, v.clusterSize())
	if _, err = v.f.ReadAt(buf, v.clusterOff(dat.start)); err != nil {
		t.Fatal(err)
	}
	if _, err = v.f.WriteAt(buf, v.clusterOff(pdbAt)); err != nil {
		t.Fatal(err)
	}
	v.setStart(dat, pdbAt)
	if cutPDB {
		pdbChain, _ := v.chain(v.lookup(targets[1]).start)
		v.free(pdbChain[len(pdbChain)-1])
	}
	v.countFree()
	for _, rel := range targets {
		if v.owned(v.lookup(rel)) {
			t.Fatalf("%s still owns its clusters", rel)
		}
	}
	v.close()
	before := issue321Hash(t, img)
	beforeImg := filepath.Join(work, "before.img")
	vfatRun(t, "cp", "--sparse=always", img, beforeImg)

	// The pass is refused before it allocates anything: the stick is what it was, and read-write.
	_, unmount = vfatMount(t, img, mnt)
	for _, rel := range targets {
		if !issue321Whole(mnt, rel) {
			t.Fatalf("%s: the stale entry does not read", rel)
		}
	}
	res, err := bridge.WriteBackCues(ctx, mnt, opts)
	if !errors.Is(err, bridge.ErrUnownedClustersForTest) || res.FilesUpdated != 0 ||
		!strings.Contains(err.Error(), targets[0]) || !strings.Contains(err.Error(), "the drive needs a disk check on a computer") {
		t.Errorf("the pass on a stick with stale entries: %+v %v, want it refused, naming %s and the disk check", res, err, targets[0])
	}
	if vfatReadOnly(t, mnt) {
		t.Error("the stick turned read-only")
	}
	unmount()
	if after := issue321Hash(t, img); after != before {
		t.Errorf("the stick changed: sha256 %s before the pass, %s after; at %s", before, after, issue321Changed(t, beforeImg, img))
	}
	if err = os.Remove(beforeImg); err != nil {
		t.Fatal(err)
	}

	// A computer's disk check, as Windows or macOS repair a stick (fsck.fat -a). It cuts each file
	// whose entry names a free cluster: rekordbox exports again what it cut. Then the first pass
	// writes the edits, which stayed pending, and the volume is clean.
	out, _ := exec.Command("fsck.fat", "-a", img).CombinedOutput()
	t.Logf("fsck.fat -a:\n%s", out)
	_, unmount = vfatMount(t, img, mnt)
	for _, rel := range targets {
		st, err := os.Stat(filepath.Join(mnt, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		whole := issue321Whole(mnt, rel)
		t.Logf("%s after the disk check: %d bytes, reads whole: %v", rel, st.Size(), whole)
		if !whole {
			b, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(mnt, filepath.FromSlash(rel)), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Flush and unmount the repaired filesystem before taking the baseline image hash so dirty
	// page-cache writes from recreating cut files are not attributed to the pass below.
	unmount()
	if !cutPDB {
		// When fsck only cut ANLZ0000.DAT, export.pdb keeps its v4 stamp (bridge.go:778-794)
		// and the first write-back pass after the disk check writes both the pending cue and
		// genre edits cleanly.
		_, unmount = vfatMount(t, img, mnt)
		res, err = bridge.WriteBackCues(ctx, mnt, opts)
		if err != nil || !issue321ANLZHas(t, mnt, cue) || issue321PDBGenre(t, mnt, 1) != genre {
			t.Fatalf("the first pass after the disk check: %+v %v: the edits are not on the stick", res, err)
		}
		issue321ANLZSound(t, mnt)
		unmount()
		if out, err := exec.Command("fsck.fat", "-n", img).CombinedOutput(); err != nil {
			t.Fatalf("fsck.fat -n after that pass: %v\n%s", err, out)
		}
		return
	}
	// When fsck also cut export.pdb and rekordbox exported it again, exportStamp (bridge.go:778-794)
	// no longer matches .rb-bridge-imported, so writeBackStampReason (cue_writeback.go:267-270)
	// refuses a pre-conversion pass without touching the repaired stick. Once ConvertAtMount imports
	// the re-export, importKeeps and InvalidateUnsyncedEdits (realign.go:51-65, 368-401) keep the
	// pending deck cue edit (while sync.go:988,1042 refreshes Track.genre from the re-exported
	// export.pdb), and the next write-back pass writes the pending cue edit cleanly.
	afterRepair := issue321Hash(t, img)
	_, unmount = vfatMount(t, img, mnt)
	res, err = bridge.WriteBackCues(ctx, mnt, opts)
	if err != nil || res.FilesUpdated != 0 || res.TracksChanged != 0 || res.OneLibraryUpdated {
		t.Fatalf("the first pass after the disk check: %+v %v, want stamp-mismatch refusal", res, err)
	}
	unmount()
	if after := issue321Hash(t, img); after != afterRepair {
		t.Fatalf("the refused pass changed the repaired stick: sha256 %s before, %s after", afterRepair, after)
	}
	_, unmount = vfatMount(t, img, mnt)
	if _, err := bridge.ConvertAtMount(ctx, mnt, enginedb.Options{}, false); err != nil {
		t.Fatalf("ConvertAtMount after re-export: %v", err)
	}
	res, err = bridge.WriteBackCues(ctx, mnt, opts)
	if err != nil || !issue321ANLZHas(t, mnt, cue) {
		t.Fatalf("write-back after ConvertAtMount on repaired stick: %+v %v: cue edit not written", res, err)
	}
	issue321ANLZSound(t, mnt)
	unmount()
	if out, err := exec.Command("fsck.fat", "-n", img).CombinedOutput(); err != nil {
		t.Fatalf("fsck.fat -n after that pass: %v\n%s", err, out)
	}
}

// #321 review R4 (delta review 3, P1-2): the pass decides whether the stick's FAT may be written
// before it creates anything on it. Its lock was a file on the stick, created first: on a stick's
// first pass that changed the image the refusal must leave as it was, and on a stick whose root
// directory cluster was full it took the very cluster a stale entry named, so that entry looked
// owned, the pass was not refused, and its rename freed a cluster of the root directory. The lock
// is in /run now (beside the stick in a test). Layouts: a normal root, a full root directory
// cluster, and the pass's own mark left stale by a cut.
func TestIssue321ARefusedFirstPassLeavesTheStickAsItWas(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	for _, layout := range []string{"normal root", "full root directory cluster", "a stale mark"} {
		t.Run(layout, func(t *testing.T) { issue321FirstPassRefused(t, layout) })
	}
}

func issue321FirstPassRefused(t *testing.T, layout string) {
	ctx := context.Background()
	work := t.TempDir()
	img, mnt := vfatImageOf(t, work, 64, 8)
	opts := bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true}
	stale := "PIONEER/USBANLZ/P001/ANLZ0000.DAT"
	if layout == "a stale mark" {
		stale = "Engine Library/Database2/.rb-bridge-writeback"
	}
	// A PDB stick converted at its insert, and a deck edit; no write-back pass has run on it.
	_, unmount := vfatMount(t, img, mnt)
	issue321Copy(t, setupWriteBackStick(t, false), mnt)
	if _, err := convertWriteBackAtTest(ctx, mnt); err != nil {
		t.Fatal(err)
	}
	issue321Settle() // the edit dates the row a second after the import
	injectDeckCueEdit(t, mnt)
	issue321Settle()
	if layout == "a stale mark" {
		if err := os.WriteFile(filepath.Join(mnt, filepath.FromSlash(stale)), []byte("PIONEER/USBANLZ/P001\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unmount()
	slots := func() (free int, clusters int) { // the root directory's free 32-byte entries
		v := openFAT32(t, img, false)
		defer v.close()
		chain, _ := v.chain(v.rootClus)
		buf := make([]byte, v.clusterSize())
		for _, c := range chain {
			if _, err := v.f.ReadAt(buf, v.clusterOff(c)); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < len(buf); i += 32 {
				if buf[i] == 0 || buf[i] == 0xe5 {
					free++
				}
			}
		}
		return free, len(chain)
	}
	if layout == "full root directory cluster" {
		// Short upper-case names take one entry each: two are left, and the lock's three
		// (".rb-bridge.lock" and its long name) need a new cluster.
		free, _ := slots()
		_, unmount = vfatMount(t, img, mnt)
		for i := 0; i < free-2; i++ {
			name := strconv.Itoa(1000 + i)[1:]
			if err := os.WriteFile(filepath.Join(mnt, "F"+name+".BIN"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		unmount()
		if free, n := slots(); free != 2 || n != 1 {
			t.Fatalf("the root directory has %d free entries in %d clusters, want 2 in 1", free, n)
		}
	}

	// The cluster a new entry at the root takes next: a run on a copy creates the lock where the
	// earlier build did (with a byte in it, so that a root with room shows the next free cluster).
	dry := filepath.Join(work, "dry.img")
	vfatRun(t, "cp", "--sparse=always", img, dry)
	_, unmountDry := vfatMount(t, dry, mnt)
	if err := os.WriteFile(filepath.Join(mnt, ".rb-bridge.lock"), []byte{0}, 0o600); err != nil {
		t.Fatal(err)
	}
	unmountDry()
	v := openFAT32(t, dry, false)
	next := v.lookup(".rb-bridge.lock").start
	if chain, _ := v.chain(v.rootClus); len(chain) > 1 {
		next = chain[1] // the root directory's new cluster
	}
	v.close()
	if err := os.Remove(dry); err != nil {
		t.Fatal(err)
	}

	// The stale entry a pull during an older build's pass leaves: it names that cluster, which holds
	// its content and is free in the FAT; it still reads.
	v = openFAT32(t, img, true)
	e := v.lookup(stale)
	if v.next(next) != 0 {
		t.Fatalf("cluster %d is not free", next)
	}
	buf := make([]byte, v.clusterSize())
	if _, err := v.f.ReadAt(buf, v.clusterOff(e.start)); err != nil {
		t.Fatal(err)
	}
	if _, err := v.f.WriteAt(buf, v.clusterOff(next)); err != nil {
		t.Fatal(err)
	}
	v.setStart(e, next)
	if v.owned(v.lookup(stale)) {
		t.Fatalf("%s still owns its clusters", stale)
	}
	v.close()
	before := issue321Hash(t, img)
	beforeImg := filepath.Join(work, "before.img")
	vfatRun(t, "cp", "--sparse=always", img, beforeImg)

	_, unmount = vfatMount(t, img, mnt)
	if _, err := os.ReadFile(filepath.Join(mnt, filepath.FromSlash(stale))); err != nil {
		t.Fatalf("the stale entry does not read: %v", err)
	}
	res, err := bridge.WriteBackCues(ctx, mnt, opts)
	if !errors.Is(err, bridge.ErrUnownedClustersForTest) || res.FilesUpdated != 0 || !strings.Contains(err.Error(), stale) {
		t.Errorf("the first pass on a stick with a stale entry: %+v %v, want it refused, naming %s", res, err, stale)
	}
	if vfatReadOnly(t, mnt) {
		t.Error("the stick turned read-only")
	}
	unmount()
	if after := issue321Hash(t, img); after != before {
		t.Errorf("the stick changed: sha256 %s before the pass, %s after; at %s", before, after, issue321Changed(t, beforeImg, img))
	}
	if err = os.Remove(beforeImg); err != nil {
		t.Fatal(err)
	}
}

// #321 review R4b (c′): the next insert removes the staged copies a cut pass left, on exFAT as on
// FAT32. R4 kept them on exFAT, because removing one whose allocation had not reached the stick
// turned exFAT read-only; but the next pass was then given the clusters such a copy names, and a
// disk check cut live files (the replay: 9 + 2 and 1 + 2 states). Each staged file's directory is
// now synced before the file, so its entry never names clusters the volume does not give it. A
// pass is caught with its staged copy (the one its mark names) and its mark on the stick (both are
// put back after it ends, as a pull at that step leaves them), then the next insert.
func TestIssue321TheNextInsertRemovesStagedCopies(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	for _, fsType := range []string{"exfat", "vfat"} {
		t.Run(fsType, func(t *testing.T) {
			ctx := context.Background()
			work := t.TempDir()
			img, mnt := vfatImageOf(t, work, 256, 8)
			if fsType == "exfat" {
				if _, err := exec.LookPath("mkfs.exfat"); err != nil {
					t.Skipf("mkfs.exfat: %v", err)
				}
				vfatRun(t, "mkfs.exfat", img)
			}
			_, unmount := fsMount(t, fsType, img, mnt)
			defer unmount()
			issue321Copy(t, setupWriteBackStick(t, true), mnt)
			if _, err := convertWriteBackAtTest(ctx, mnt); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(mnt, "Engine Library", "Database2", "m.db")}).String())
			if err != nil {
				t.Fatal(err)
			}
			issue321Settle()
			issue321Edit(t, db, 1, "Left 321", 1500)
			db.Close()
			issue321Settle()
			anlz := filepath.Join(mnt, "PIONEER", "USBANLZ", "P001")
			mark := filepath.Join(mnt, "Engine Library", "Database2", ".rb-bridge-writeback")
			var markBytes, copyBytes []byte
			var copyName string
			restore := bridge.SetWriteBackCutForTest(func(step string) {
				if step != "temp "+filepath.Join(anlz, "ANLZ0000.EXT") {
					return
				}
				markBytes, _ = os.ReadFile(mark) // it names the staged copy (#321 review R4)
				for _, line := range strings.Split(string(markBytes), "\n") {
					if name, ok := strings.CutSuffix(line, " PIONEER/USBANLZ/P001/ANLZ0000.EXT"); ok && strings.HasPrefix(name, "= ") {
						copyName = filepath.Join(anlz, strings.TrimPrefix(name, "= "))
						copyBytes, _ = os.ReadFile(copyName)
					}
				}
			})
			res, err := bridge.WriteBackCues(ctx, mnt, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true})
			restore()
			if err != nil || res.FilesUpdated == 0 || copyName == "" || len(markBytes) == 0 {
				t.Fatalf("pass: %+v %v; staged copy %q, mark %d bytes", res, err, copyName, len(markBytes))
			}
			if err = os.WriteFile(mark, markBytes, 0o644); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(copyName, copyBytes, 0o644); err != nil {
				t.Fatal(err)
			}
			syscall.Sync()
			bridge.CleanWriteBackLeftovers(mnt) // the next insert
			if _, err = os.Stat(copyName); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("on %s the next insert left the staged copy: %v", fsType, err)
			}
			if _, err = os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the mark is still there: %v", err)
			}
			if vfatReadOnly(t, mnt) {
				t.Error("the stick turned read-only")
			}
			issue321ANLZSound(t, mnt)
			issue321ExportSound(t, mnt)
		})
	}
}

// #321 review R2 (P2-3), the Eject's overrun on a real FAT volume: the hook's Eject marks the
// stick and asks stream-sync for the final pass, rb-bridge gives up waiting (its 10.5 s, shortened
// here) and the hook unmounts while the pass is still writing. The pass stops at its next check
// and leaves every file whole (old or new) and no temporary file; the unmount meanwhile fails
// (the pass holds the stick's lock file open), visibly (EBUSY); the hook then clears its
// rb-ejected mark, and the watcher writes the stick again; a second Eject unmounts it.
func TestIssue321EjectDuringTheFinalPassFailsVisiblyAndTheNextOneWorks(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	img, mnt := vfatImage(t, t.TempDir())
	src := setupWriteBackStick(t, true)
	_, unmount := vfatMount(t, img, mnt)
	issue321Copy(t, src, mnt)
	if _, err := convertWriteBackAtTest(ctx, mnt); err != nil {
		t.Fatal(err)
	}
	run := t.TempDir()
	t.Cleanup(bridge.SetCueRunDirForTest(run))
	w := bridge.CueWatcherForTest([]string{mnt}, 300*time.Millisecond, 500*time.Millisecond)
	wctx, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = w.Run(wctx) }()
	defer func() { stop(); <-stopped }()
	time.Sleep(100 * time.Millisecond)
	edit := func(name string, ms uint32) {
		db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(mnt, "Engine Library", "Database2", "m.db")}).String())
		if err != nil {
			t.Fatal(err)
		}
		issue321Settle()
		issue321Edit(t, db, 1, name, ms)
		db.Close()
	}
	marks := func() { // the hook's, at the start of each Eject
		for _, m := range []string{bridge.EjectingNameForTest(mnt), bridge.EjectedNameForTest(mnt)} {
			if err := os.WriteFile(filepath.Join(run, m), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The DJ edits and taps Eject at once: the hook's marks, then the final pass, slow here (it
	// sleeps at its first write, the mark's temporary file, as a slow stick keeps it there).
	marks()
	edit("At the Eject", 1000)
	old := map[string][]byte{}
	for _, rel := range []string{"PIONEER/USBANLZ/P001/ANLZ0000.DAT", "PIONEER/USBANLZ/P001/ANLZ0000.EXT", "PIONEER/rekordbox/exportLibrary.db"} {
		b, err := os.ReadFile(filepath.Join(mnt, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		old[rel] = b
	}
	var slept atomic.Bool
	restore := bridge.SetWriteBackCutForTest(func(step string) {
		if !slept.Swap(true) {
			time.Sleep(1500 * time.Millisecond)
		}
	})
	fctx, giveUp := context.WithTimeout(ctx, 500*time.Millisecond) // rb-bridge's wait, then its request ends
	defer giveUp()
	passDone := make(chan error, 1)
	t0 := time.Now()
	go func() { _, err := w.Final(fctx, mnt); passDone <- err }()
	time.Sleep(700 * time.Millisecond) // rb-bridge gave up; the hook's unmount
	if err := syscall.Unmount(mnt, 0); err != syscall.EBUSY {
		t.Fatalf("the unmount during the pass: %v, want EBUSY (a visible failed Eject)", err)
	}
	var err error
	select {
	case err = <-passDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the final pass did not stop")
	}
	restore()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(t0) > 3*time.Second {
		t.Fatalf("the final pass ended after %s with %v, want it stopped by the end of its request", time.Since(t0), err)
	}
	for rel, b := range old {
		if got, err := os.ReadFile(filepath.Join(mnt, filepath.FromSlash(rel))); err != nil || !bytes.Equal(got, b) {
			t.Fatalf("%s changed by the stopped pass (%v)", rel, err)
		}
	}
	if left := issue321Temps(t, mnt); len(left) != 0 {
		t.Fatalf("the stopped pass left %q", left)
	}
	issue321ExportSound(t, mnt)
	issue321ANLZSound(t, mnt)

	// The failed Eject: the hook clears rb-ejected (and keeps ejecting for Pro DJ Link). The
	// watcher writes the stick again, the pending edit and the next one.
	if err := os.Remove(filepath.Join(run, bridge.EjectedNameForTest(mnt))); err != nil {
		t.Fatal(err)
	}
	edit("After the failed Eject", 2000)
	issue321WaitANLZ(t, mnt, "After the failed Eject", 5*time.Second)
	issue321ExportSound(t, mnt)

	// The second Eject: its final pass finds nothing left, and the stick unmounts.
	marks()
	if res, err := w.Final(ctx, mnt); err != nil || res.TracksChanged != 0 {
		t.Fatalf("second Eject's pass: %+v %v", res, err)
	}
	unmount()
	_, _ = vfatMount(t, img, mnt)
	bridge.CleanWriteBackLeftovers(mnt) // the next mount
	issue321ExportSound(t, mnt)
	issue321ANLZSound(t, mnt)
	if !issue321ANLZHas(t, mnt, "After the failed Eject") || len(issue321Temps(t, mnt)) != 0 {
		t.Fatal("after the second Eject the stick lacks the edit or holds a temporary file")
	}
}
