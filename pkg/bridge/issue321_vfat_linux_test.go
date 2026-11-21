//go:build linux

package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #321 review R2, on a real FAT32 volume (a loop device; root only). A pull during an older
// build's write-back can leave an ANLZ file or the export naming clusters the FAT has freed, the
// old content still in them. The next file written may take exactly those clusters. When a pass
// then replaces such a file it must still see that the file does not own them: deciding after
// its own temporary file took them, the pass found the file owning a whole chain, held it, and
// its close freed the chain the new file had just been given. The new ANLZ file and the new
// export then named free clusters, and the next file written overwrote them.
//
// The volume is built so that the temporary file of each replaced file takes the clusters of its
// stale entry: a first run on a copy of the volume finds where the pass writes each new file, and
// the stale entry is made to name those clusters, which hold its content and are free in the FAT.

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
	dev = vfatRun(t, "losetup", "-f", "--show", img)
	if out, err := exec.Command("mount", "-t", "vfat", "-o", "utf8,flush", dev, mnt).CombinedOutput(); err != nil {
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
	if err = f.Truncate(2112 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	vfatRun(t, "mkfs.fat", "-F", "32", "-s", "64", "-S", "512", img)
	return img, mnt
}

func TestIssue321StaleEntriesOnARealFATVolumeKeepTheirClusters(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	// 32 KiB clusters: each file below fits one cluster before and after the pass, so a stale
	// entry names a chain as long as the new file's.
	img, mnt := vfatImage(t, work)
	dry := filepath.Join(work, "dry.img")
	var err error

	// The stick: converted, a deck edit written back once (its backups made), then a second edit.
	src := setupWriteBackStick(t, true)
	_, unmount := vfatMount(t, img, mnt)
	issue321Copy(t, src, mnt)
	if _, err = bridge.Convert(ctx, mnt, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}
	injectDeckCueEdit(t, mnt)
	opts := bridge.WriteBackOptions{DateDir: "2026-11-22", SkipPreparingCheck: true}
	if res, err := bridge.WriteBackCues(ctx, mnt, opts); err != nil || res.FilesUpdated == 0 {
		t.Fatalf("first pass: %+v %v", res, err)
	}
	issue321Settle()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(mnt, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	issue321Edit(t, db, 1, "Stale 321", 1500)
	db.Close()
	issue321Settle()
	unmount()

	targets := []string{"PIONEER/USBANLZ/P001/ANLZ0000.DAT", "PIONEER/rekordbox/exportLibrary.db"}
	// Where the pass writes each new file: a run on a copy.
	vfatRun(t, "cp", "--sparse=always", img, dry)
	_, unmountDry := vfatMount(t, dry, mnt)
	if res, err := bridge.WriteBackCues(ctx, mnt, opts); err != nil || res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("pass on the copy: %+v %v", res, err)
	}
	syscall.Sync()
	unmountDry()
	written := map[string]fat32Entry{}
	writtenTo := map[string][]int64{}
	v := openFAT32(t, dry, false)
	for _, rel := range targets {
		written[rel] = v.lookup(rel)
		writtenTo[rel], _ = v.chain(written[rel].start)
	}
	v.close()

	// Each target's entry names, instead of its chain, the clusters the pass will write its new
	// file to: free in the FAT, holding the target's content (as a pull leaves a replaced file).
	v = openFAT32(t, img, true)
	cs := v.clusterSize()
	for _, rel := range targets {
		e, w := v.lookup(rel), written[rel]
		if (e.size+cs-1)/cs != (w.size+cs-1)/cs {
			t.Fatalf("%s: %d bytes before the pass, %d after: not the same number of clusters", rel, e.size, w.size)
		}
		old, _ := v.chain(e.start)
		if len(old) != len(writtenTo[rel]) {
			t.Fatalf("%s: a chain of %d clusters, the pass writes %d", rel, len(old), len(writtenTo[rel]))
		}
		for i, c := range old {
			to := writtenTo[rel][i]
			if v.next(to) != 0 {
				t.Fatalf("%s: cluster %d, where the pass writes, is not free", rel, to)
			}
			buf := make([]byte, cs)
			if _, err := v.f.ReadAt(buf, v.clusterOff(c)); err != nil {
				t.Fatal(err)
			}
			if _, err := v.f.WriteAt(buf, v.clusterOff(to)); err != nil {
				t.Fatal(err)
			}
		}
		var s [4]byte
		binary.LittleEndian.PutUint16(s[0:], uint16(w.start>>16))
		binary.LittleEndian.PutUint16(s[2:], uint16(w.start))
		if _, err := v.f.WriteAt(s[0:2], e.off+20); err != nil {
			t.Fatal(err)
		}
		if _, err := v.f.WriteAt(s[2:4], e.off+26); err != nil {
			t.Fatal(err)
		}
		if v.owned(v.lookup(rel)) {
			t.Fatalf("%s still owns its clusters", rel)
		}
	}
	v.close()

	dev, unmount := vfatMount(t, img, mnt)
	issue321ExportSound(t, mnt) // the stale entries still read
	res, err := bridge.WriteBackCues(ctx, mnt, opts)
	if err != nil || res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("pass: %+v %v", res, err)
	}
	syscall.Sync()
	if vfatReadOnly(t, mnt) {
		t.Fatal("the pass made vfat remount the stick read-only (a FAT error)")
	}
	content := map[string][]byte{}
	v = openFAT32(t, dev, false)
	for _, rel := range targets {
		if e := v.lookup(rel); !v.owned(e) {
			t.Errorf("%s: after the pass it names clusters the FAT does not give it (from %d, %d bytes): the next file written takes them", rel, e.start, e.size)
		}
		if content[rel], err = os.ReadFile(filepath.Join(mnt, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	v.close()
	if !issue321ANLZHas(t, mnt, "Stale 321") {
		t.Fatal("the edit is not on the stick")
	}
	// The next mount's cleanup frees nothing of it either.
	bridge.CleanWriteBackLeftovers(mnt)
	syscall.Sync()
	if vfatReadOnly(t, mnt) {
		t.Fatal("the next mount's cleanup made vfat remount the stick read-only")
	}
	unmount()

	// What the deck reads next: a file written after the pass, its allocation starting where the
	// stale entries pointed, must not land in the new files.
	v = openFAT32(t, img, true)
	low := written[targets[0]].start
	for _, rel := range targets {
		low = min(low, written[rel].start)
	}
	var hint [4]byte
	binary.LittleEndian.PutUint32(hint[:], uint32(low-1))
	if _, err := v.f.WriteAt(hint[:], v.fsinfo*v.bps+492); err != nil {
		t.Fatal(err)
	}
	v.close()
	_, unmount = vfatMount(t, img, mnt)
	if err = os.WriteFile(filepath.Join(mnt, "after.bin"), bytes.Repeat([]byte{0xa5}, int(8*cs)), 0o644); err != nil {
		t.Fatal(err)
	}
	unmount()
	_, _ = vfatMount(t, img, mnt)
	for _, rel := range targets {
		got, err := os.ReadFile(filepath.Join(mnt, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, content[rel]) {
			t.Fatalf("%s: overwritten by the next file written (read error %v): the pass freed its clusters", rel, err)
		}
	}
	issue321ExportSound(t, mnt)
}

// #321 review R2 (P2-3), the Eject's overrun on a real FAT volume: the hook's Eject marks the
// stick and asks stream-sync for the final pass, rb-bridge gives up waiting (its 10.5 s, shortened
// here) and the hook unmounts while the pass is still writing. The pass stops at its next check
// and leaves every file whole (old or new) and no temporary file; the unmount meanwhile fails
// (the pass holds the stick's lock file open), visibly (EBUSY); the hook then clears its
// rb-ejected mark, and the watcher writes the stick again; a second Eject unmounts it.
func TestIssue321EjectDuringTheFinalPassFailsVisiblyAndTheNextOneWorks(t *testing.T) {
	ctx := context.Background()
	img, mnt := vfatImage(t, t.TempDir())
	src := setupWriteBackStick(t, true)
	_, unmount := vfatMount(t, img, mnt)
	issue321Copy(t, src, mnt)
	if _, err := bridge.Convert(ctx, mnt, enginedb.Options{}); err != nil {
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
