//go:build linux

package bridge_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #253 review 5: whether a state row's waveform copy can become a reference is decided only by the
// background job's paced check. The mount, and the import with its transaction and the bridge
// lock, read no ANLZ file for it: a stick whose re-imported tracks hold 5.3.0.5's copies opens
// each ANLZ file exactly as often during a mount as the same stick whose rows are references
// already. The copies stay in place, and a stick that never gets a background job neither reads
// its files again nor grows at the next mount.

// anlzOpens runs step and counts the opens of each file in dir while it runs (inotify). The closes
// are watched too: inotify merges an event into the same one still unread before it, and a close
// between two opens of a file keeps them apart.
func anlzOpens(t *testing.T, dir string, step func()) map[string]int {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Skipf("inotify: %v", err)
	}
	defer unix.Close(fd)
	if _, err = unix.InotifyAddWatch(fd, dir, unix.IN_OPEN|unix.IN_CLOSE); err != nil {
		t.Fatal(err)
	}
	step()
	opens := map[string]int{}
	buf := make([]byte, 1<<16)
	for {
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EAGAIN) || n <= 0 {
			return opens
		}
		if err != nil {
			t.Fatal(err)
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			if ev.Mask&unix.IN_Q_OVERFLOW != 0 {
				t.Fatal("inotify lost events")
			}
			name := strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+int(ev.Len)]), "\x00")
			if ev.Mask&unix.IN_OPEN != 0 {
				opens[name]++
			}
			off += unix.SizeofInotifyEvent + int(ev.Len)
		}
	}
}

// copyRows counts the tracks whose state row holds a waveform copy (5.3.0.5's RBA1).
func copyRows(t *testing.T, root string) int {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	return scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeAnalysis WHERE length(sourceJSON)>4096`)
}

func TestIssue253MountReadsNoANLZFileForTheWaveformCopies(t *testing.T) {
	anlzDir := filepath.Join("PIONEER", "USBANLZ", "P001")
	// reanalysed is what rekordbox leaves after analysing every track again: the same files,
	// written later, so the import reads each track again.
	reanalysed := func(t *testing.T, root string) {
		later := time.Now().Add(time.Hour)
		for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
			if err := os.Chtimes(filepath.Join(root, anlzDir, "ANLZ0000"+ext), later, later); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx := context.Background()
	for _, sc := range []struct {
		name string
		// prepare changes the converted stick and runs what precedes the counted mount.
		prepare func(t *testing.T, root string)
		mount   func(t *testing.T, root string)
	}{
		{"a full import at the mount", reanalysed, func(t *testing.T, root string) {
			if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true); err != nil {
				t.Fatal(err)
			}
		}},
		{"a small export's update in the mount", func(t *testing.T, root string) {
			reanalysed(t, root)
			testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.LargePDB(50))
			for i := 41; i <= 50; i++ {
				testfixture.Write(t, root, testfixture.LargeTrackPath(i), testfixture.MP3WithLAMETag())
			}
		}, func(t *testing.T, root string) {
			if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
				t.Fatalf("mount: %+v %v", r, err)
			}
		}},
		{"the remount reading a large export's update in place", func(t *testing.T, root string) {
			reanalysed(t, root)
			testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.LargePDB(50))
			for i := 41; i <= 50; i++ {
				testfixture.Write(t, root, testfixture.LargeTrackPath(i), testfixture.MP3WithLAMETag())
			}
			if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
				t.Fatalf("mount: %+v %v", r, err)
			}
			if _, err := bridge.ImportInBackground(ctx, root, nil); err != nil { // the job reads, paced
				t.Fatal(err)
			}
		}, func(t *testing.T, root string) {
			if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(sc.name, func(t *testing.T) {
			if strings.HasPrefix(sc.name, "the remount") {
				defer bridge.SetLargeExportForTest()() // else a small export: the mount reads it all
			}
			defer bridge.SetInPlaceUpdateForTest(1)()
			opens := map[bool]map[string]int{}
			for _, copies := range []bool{true, false} {
				root, _ := issue253Converted(t, 40)
				if copies {
					issue253OldRows(t, root)
				}
				sc.prepare(t, root)
				before := copyRows(t, root)
				opens[copies] = anlzOpens(t, filepath.Join(root, anlzDir), func() { sc.mount(t, root) })
				t.Logf("copies=%v: ANLZ opens during the mount %v; copy rows %d -> %d", copies, opens[copies], before, copyRows(t, root))
				if copies && !strings.HasPrefix(sc.name, "the remount") {
					// The import read every track again and left every copy in place.
					if after := copyRows(t, root); after != before {
						t.Fatalf("the import turned %d of %d waveform copies into references", before-after, before)
					}
					issue253Waveforms(t, root, 40, "after the mount")
				}
				if copies && sc.name == "a small export's update in the mount" {
					// No background job: the next mount reads no file for the copies and the state
					// does not grow.
					state := filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-state.db")
					st0, err := os.Stat(state)
					if err != nil {
						t.Fatal(err)
					}
					again := anlzOpens(t, filepath.Join(root, anlzDir), func() {
						if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Bypassed {
							t.Fatalf("next mount: %+v %v", r, err)
						}
					})
					st1, err := os.Stat(state)
					if err != nil {
						t.Fatal(err)
					}
					if len(again) != 0 || st1.Size() > st0.Size() || copyRows(t, root) != before {
						t.Fatalf("next mount: ANLZ opens %v, state %d -> %d bytes, copy rows %d -> %d", again, st0.Size(), st1.Size(), before, copyRows(t, root))
					}
				}
			}
			for _, name := range []string{"ANLZ0000.2EX", "ANLZ0000.EXT", "ANLZ0000.DAT"} {
				if opens[true][name] != opens[false][name] {
					t.Fatalf("%s opened %d times during the mount on the stick with waveform copies, %d with references: the mount read files for the copies",
						name, opens[true][name], opens[false][name])
				}
			}
		})
	}
}
