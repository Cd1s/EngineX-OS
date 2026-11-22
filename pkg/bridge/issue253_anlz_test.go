package bridge_test

import (
	"bytes"
	"context"
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
	// #253 review 5: files the import reads but the display does not: the .2EX and the .EXT padded
	// past the 16 MiB the display reads (the import reads a .2EX of up to 128 MiB, and the .EXT's
	// headers). The import reads the track again and leaves its copy in place, and the background
	// check refuses the files, so the copy stays. A read error at a page is the same case: see
	// pkg/enginedb's TestIssue253GateReadsTheFileAsTheDisplayDoes and the dm-error run of review 4.
	breaks = append(breaks, struct {
		name  string
		apply func(t *testing.T, root string)
	}{"larger than the display reads", func(t *testing.T, root string) {
		for _, ext := range []string{".2EX", ".EXT"} {
			p := filepath.Join(root, anlz+ext)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(p, append(b, make([]byte, 17<<20)...), 0o644); err != nil { // padding past the declared length
				t.Fatal(err)
			}
		}
	}})
	flows := []struct {
		name     string
		perTrack int64
	}{
		{"in place", 1},     // the job hands the reading to its remount, which compacts the state
		{"staged", 1 << 40}, // the job's stage, with its compact copy of the state
	}
	for _, f := range flows {
		// A group for each flow: the two hooks it sets are process-wide, so they are set once for
		// the group and put back when its breaks, which run side by side, are over (Cleanup waits
		// for the parallel subtests; a defer would not). The subtests are the same, with the
		// same names, bodies and assertions: flow/break.
		t.Run(f.name, func(t *testing.T) {
			t.Cleanup(bridge.SetLargeExportForTest())
			t.Cleanup(bridge.SetInPlaceUpdateForTest(f.perTrack))
			for _, br := range breaks {
				t.Run(br.name, func(t *testing.T) {
					t.Parallel()
					root, cues := issue253Converted(t, 40)
					issue253OldRows(t, root)
					issue253Waveforms(t, root, 40, "before")
					// The re-export adds 20 tracks; the stick's ANLZ files are broken.
					testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.LargePDB(60))
					for i := 41; i <= 60; i++ {
						testfixture.Write(t, root, testfixture.LargeTrackPath(i), testfixture.MP3WithLAMETag())
					}
					printBefore := issue253Print(t, root, 1)
					br.apply(t, root)
					cutConverge(t, root)
					issue253Waveforms(t, root, 40, "after the update")
					if issue253Print(t, root, 1) == printBefore {
						t.Fatal("the update did not read track 1 again: its copy was not put through the import")
					}
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
		})
	}
}

// issue253Print is the fingerprint of the track with the import key in the state: the import writes a
// new one when it reads the track again.
func issue253Print(t *testing.T, root string, key int) string {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	return scalar[string](t, db, `SELECT s.fingerprint FROM bridge.BridgeState s JOIN Track t ON t.id=s.trackId WHERE t.pdbImportKey=?`, key)
}
