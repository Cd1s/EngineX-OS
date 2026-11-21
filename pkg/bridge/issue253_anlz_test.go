package bridge_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #253 review 2: an update replaces a state row that still holds a waveform (5.3.0.5's RBA1) by a
// reference to the track's ANLZ files only when those files give the display the waveform. When
// the export names files that are missing, truncated or renamed on the stick, the row stays as it
// is, through the compaction and through the imports after it.

// issue253Waveforms fails unless tracks 1 to n each have a state row that gives the display a
// waveform.
func issue253Waveforms(t *testing.T, root string, n int, when string) {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	for key := 1; key <= n; key++ {
		var b []byte
		if err := db.QueryRow(`SELECT a.sourceJSON FROM bridge.BridgeAnalysis a JOIN Track t ON t.id=a.trackId WHERE t.pdbImportKey=?`, key).Scan(&b); err != nil {
			t.Fatalf("%s: track %d: %v", when, key, err)
		}
		if enginedb.BridgeDetail(b, root, 16<<20) == nil {
			t.Fatalf("%s: track %d's state row (%d bytes) gives the display no waveform", when, key, len(b))
		}
	}
}

func TestIssue253BrokenANLZKeepsItsWaveformRow(t *testing.T) {
	anlz := "PIONEER/USBANLZ/P001/ANLZ0000"
	breaks := []struct {
		name  string
		apply func(t *testing.T, root string)
	}{
		{"missing", func(t *testing.T, root string) {
			for _, ext := range []string{".2EX", ".EXT"} {
				if err := os.Remove(filepath.Join(root, anlz+ext)); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"truncated", func(t *testing.T, root string) {
			for _, ext := range []string{".2EX", ".EXT"} {
				p := filepath.Join(root, anlz+ext)
				st, err := os.Stat(p)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Truncate(p, st.Size()/2); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"renamed in the export", func(t *testing.T, root string) {
			p := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			// The export's strings are UTF-16LE; every track names the same files.
			utf16le := func(s string) []byte {
				var u []byte
				for _, r := range s {
					u = append(u, byte(r), 0)
				}
				return u
			}
			if n := bytes.Count(b, utf16le("ANLZ0000.DAT")); n != 60 {
				t.Fatalf("the export names its analysis file %d times, want 60", n)
			}
			b = bytes.ReplaceAll(b, utf16le("ANLZ0000.DAT"), utf16le("ANLZ0009.DAT"))
			if err = os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	// #253 review 3: a section the parser refuses, of each kind it reads, in front of the valid
	// PWV7 (.2EX) and of the cues (.EXT): BridgeDetail finds no waveform in either file.
	for _, kind := range testfixture.MalformedANLZKinds {
		breaks = append(breaks, struct {
			name  string
			apply func(t *testing.T, root string)
		}{"malformed " + kind, func(t *testing.T, root string) {
			for _, ext := range []string{".2EX", ".EXT"} {
				p := filepath.Join(root, anlz+ext)
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(p, testfixture.WithANLZSectionFirst(b, testfixture.MalformedANLZSection(kind)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}})
	}
	// #253 review 4: the files read, but not through the display's reader: a bad page in the
	// waveform's body, or in the last page (padding past the declared length), of the .2EX and
	// the .EXT. The import (it reads them itself) accepts them; the display cannot serve them, so
	// neither the update's compaction nor the next import may turn the copy into a reference.
	for _, fault := range []string{"waveform body", "last page"} {
		breaks = append(breaks, struct {
			name  string
			apply func(t *testing.T, root string)
		}{"read error in the " + fault, func(t *testing.T, root string) {
			bad := map[string][2]int64{}
			for _, ext := range []string{".2EX", ".EXT"} {
				p := filepath.Join(root, anlz+ext)
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				at := issue253WaveBody(t, b)
				if fault == "last page" { // FAT padding past the declared length, a page of its own
					b = append(b, make([]byte, 4096+(4096-len(b)%4096)%4096)...)
					if err = os.WriteFile(p, b, 0o644); err != nil {
						t.Fatal(err)
					}
					at = int64(len(b) - 4096)
				}
				bad[p] = [2]int64{at, at + 1}
			}
			t.Cleanup(enginedb.SetDetailReadFailForTest(func(path string, off int64, n int) bool {
				s, ok := bad[path]
				return ok && off < s[1] && off+int64(n) > s[0]
			}))
		}})
	}
	flows := []struct {
		name     string
		perTrack int64
	}{
		{"in place", 1},     // the job hands the reading to its remount, which compacts the state
		{"staged", 1 << 40}, // the job's stage, with its compact copy of the state
	}
	for _, f := range flows {
		for _, br := range breaks {
			t.Run(f.name+"/"+br.name, func(t *testing.T) {
				root, cues := issue253Converted(t, 40)
				issue253OldRows(t, root)
				issue253Waveforms(t, root, 40, "before")
				// The re-export adds 20 tracks; the stick's ANLZ files are broken.
				testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.LargePDB(60))
				for i := 41; i <= 60; i++ {
					testfixture.Write(t, root, testfixture.LargeTrackPath(i), testfixture.MP3WithLAMETag())
				}
				br.apply(t, root)
				defer bridge.SetLargeExportForTest()()
				defer bridge.SetInPlaceUpdateForTest(f.perTrack)()
				cutConverge(t, root)
				issue253Waveforms(t, root, 40, "after the update")
				// The next import, one that reads every track.
				if _, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, true); err != nil {
					t.Fatalf("next import: %v", err)
				}
				issue253Waveforms(t, root, 40, "after the next import")
				db := dbOpen(t, root)
				issue253DeckEdit(t, db, cues)
				if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE pdbImportKey>0`); n != 60 {
					t.Fatalf("%d of 60 tracks", n)
				}
				db.Close()
			})
		}
	}
}

// issue253WaveBody is where the first PWV7 or PWV5 section's entries start in the ANLZ file b.
func issue253WaveBody(t *testing.T, b []byte) int64 {
	t.Helper()
	be := binary.BigEndian
	for off := int64(be.Uint32(b[4:])); off+12 <= int64(len(b)); {
		tag, h, n := string(b[off:off+4]), int64(be.Uint32(b[off+4:])), int64(be.Uint32(b[off+8:]))
		if (tag == "PWV7" || tag == "PWV5") && n > h {
			return off + h
		}
		if n < 12 {
			break
		}
		off += n
	}
	t.Fatal("no PWV7 or PWV5 with entries")
	return 0
}
