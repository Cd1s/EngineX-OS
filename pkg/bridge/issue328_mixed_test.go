package bridge_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #328 review R2, P1-2: a deck still on 5.3.0.5-r1 never goes on with, nor publishes, a stage of
// this build that holds a copy of fast rows. r1 reads neither the note of what the fast import
// left in them (enginedb.FastBaseFile) nor keeps the DJ's edits on them when it publishes (its
// carryLiveEdits leaves fast rows out): published by r1, such a stage put rekordbox's rows over
// the DJ's edits. r1 takes a stage for an update only when its KIND mark reads "update"; it drops
// any other at its mount and starts its own update from the live library, a stage this build in
// turn never publishes (fastBaseMissing). This build still goes on with its own stage and
// publishes it with the DJ's edits; a stage without fast rows keeps the kind r1 reads.
func TestIssue328BStageOfFastRowsIsNotOneTheReleasedBuildTakes(t *testing.T) {
	for _, src := range []struct {
		name   string
		oneLib bool
	}{{"PDB", false}, {"OneLibrary", true}} {
		for _, path := range []struct {
			name   string
			pulled bool
			mounts int // the mount that publishes
		}{{"finished", false, 1}, {"pulled during the update", true, 2}} {
			t.Run(src.name+"/"+path.name, func(t *testing.T) {
				root := t.TempDir()
				if src.oneLib {
					issue328OneLibrary(t, root, 2)
				} else {
					testfixture.LargeExport(t, root, issue328Tracks)
				}
				defer bridge.SetUpdateExportForTest()()
				ctx := context.Background()
				if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
					t.Fatalf("first mount: %+v %v", r, err)
				}
				if path.pulled {
					cut, cancel := context.WithCancel(ctx)
					restore := bridge.SetCutForTest(func(name string) {
						if name == "stage pass committed" {
							cancel()
						}
					})
					window := bridge.SetImportWindowForTest(time.Nanosecond)
					_, err := bridge.ImportInBackground(cut, root, nil)
					window()
					restore()
					cancel()
					if err == nil {
						t.Fatal("the update was not cut")
					}
				} else if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
					t.Fatalf("background update: %+v %v", r, err)
				}
				stage := filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-import")
				if _, err := os.Stat(filepath.Join(stage, enginedb.FastBaseFile)); err != nil {
					t.Fatalf("the stage holds no copy of the fast rows: %v", err)
				}
				if kind := issue328StageKind(t, stage); kind == "update" || kind == "" {
					t.Fatalf("stage kind %q: a 5.3.0.5-r1 deck takes it for its own update", kind)
				}
				issue328Deck(t, root, src.oneLib)
				before := issue328Before(t, root)
				db, mounts := issue328PublishCounted(t, root)
				issue328TheDJsEdits(t, db, before, src.oneLib)
				if mounts != path.mounts {
					t.Errorf("published at mount %d, want %d: this build did not go on with its own stage", mounts, path.mounts)
				}
				db.Close()

				// No fast row left: the next update's stage is one r1 goes on with, as before.
				issue328ReExport(t, root, src.oneLib)
				if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
					t.Fatalf("mount of the re-export: %+v %v", r, err)
				}
				if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
					t.Fatalf("its background update: %+v %v", r, err)
				}
				if kind := issue328StageKind(t, stage); kind != "update" {
					t.Errorf("stage kind %q of an update without fast rows, want \"update\"", kind)
				}
			})
		}
	}
}

// Fast rows an earlier build left: 5.3.0.5-r1 keeps no note of what its fast import left in them
// (BridgeFastBase), and a note of another export record is not the row's (an earlier build moved
// the row to a new record without it). For a row of the same export record it is what the fast
// import computes from that record, as before review R2: the DJ's edits stay and the rest is
// rekordbox's.
func TestIssue328BFastRowsOfAnEarlierBuildWithoutTheirNote(t *testing.T) {
	for _, src := range []struct {
		name   string
		oneLib bool
	}{{"PDB", false}, {"OneLibrary", true}} {
		for _, note := range []struct{ name, sql string }{
			{"no note", `DROP TABLE BridgeFastBase`},
			{"a note of another record", `UPDATE BridgeFastBase SET record=record+1,rating=55,quickCues=NULL,loops=NULL,active=0`},
		} {
			t.Run(src.name+"/"+note.name, func(t *testing.T) {
				root := t.TempDir()
				if src.oneLib {
					issue328OneLibrary(t, root, 2)
				} else {
					testfixture.LargeExport(t, root, issue328Tracks)
				}
				defer bridge.SetUpdateExportForTest()()
				if r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !r.Background {
					t.Fatalf("first mount: %+v %v", r, err)
				}
				state, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-state.db"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = state.Exec(note.sql); err != nil {
					t.Fatal(err)
				}
				state.Close()
				issue328Deck(t, root, src.oneLib)
				before := issue328Before(t, root)
				db := issue328Publish(t, root)
				issue328TheDJsEdits(t, db, before, src.oneLib)
				control, _ := issue328Run(t, src.oneLib, true, false)
				issue328RuleH(t, db, control, src.oneLib)
			})
		}
	}
}

// The other half of the turn r1 takes (P1-2): at its mount 5.3.0.5-r1 drops this build's stage
// and starts its own update, whose passes on its copy put rekordbox's rows over the DJ's edits of
// fast rows (r1's importKeeps leaves fast rows out), and the copy is complete before this build
// mounts the stick again. Its stage has no note of the fast rows (enginedb.FastBaseFile), and
// Engine wrote nothing since its copy: this build does not publish it but starts again (it
// published it as it was before review R2: the edits were lost). Here the stage of this build's
// own update stands in for r1's: the note removed, the kind r1 writes, and in the copy the rating
// and the grid lock r1's passes put back.
func TestIssue328BStageOfAnEarlierBuildIsNotPublishedOverEditsInItsCopy(t *testing.T) {
	for _, src := range []struct {
		name   string
		oneLib bool
	}{{"PDB", false}, {"OneLibrary", true}} {
		t.Run(src.name, func(t *testing.T) {
			root := t.TempDir()
			if src.oneLib {
				issue328OneLibrary(t, root, 2)
			} else {
				testfixture.LargeExport(t, root, issue328Tracks)
			}
			defer bridge.SetUpdateExportForTest()()
			ctx := context.Background()
			if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
				t.Fatalf("first mount: %+v %v", r, err)
			}
			issue328Deck(t, root, src.oneLib)
			before := issue328Before(t, root)
			if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
				t.Fatalf("background update: %+v %v", r, err)
			}
			stage := filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-import")
			if err := os.Remove(filepath.Join(stage, enginedb.FastBaseFile)); err != nil {
				t.Fatal(err)
			}
			testfixture.Write(t, stage, "KIND", []byte("update\n"))
			copied, err := sql.Open("sqlite", filepath.Join(stage, "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = copied.Exec(`UPDATE Track SET rating=0 WHERE pdbImportKey=6; UPDATE Track SET isBeatGridLocked=0 WHERE pdbImportKey=5`); err != nil {
				t.Fatal(err)
			}
			copied.Close()
			db, mounts := issue328PublishCounted(t, root)
			issue328TheDJsEdits(t, db, before, src.oneLib)
			if mounts != 2 {
				t.Errorf("published at mount %d, want 2 (the stage refused, a new copy updated)", mounts)
			}
		})
	}
}

// The note of what the import left in a fast row (BridgeFastBase) goes with the row: a track the
// deck deleted while it was fast takes its note with it, as the state's other tables do.
func TestIssue328BNoteOfAFastRowGoesWithItsTrack(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, issue328Tracks)
	defer bridge.SetUpdateExportForTest()()
	if r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	live := dbOpen(t, root)
	if n := scalar[int](t, live, `SELECT count(*) FROM bridge.BridgeFastBase`); n != issue328Tracks {
		t.Fatalf("%d notes of fast rows after the fast import, want %d", n, issue328Tracks)
	}
	if _, err := live.Exec(`DELETE FROM Track WHERE pdbImportKey=40`); err != nil {
		t.Fatal(err)
	}
	live.Close()
	db := issue328Publish(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeFastBase`); n != 0 {
		t.Errorf("%d notes left after the publish, want 0", n)
	}
}

// A fast row whose first full import cannot read its analysis (an unreadable ANLZ file: the row
// is imported without cues, "degraded") follows the same rule for what that import writes: the
// deck's rating and edited grid's BPM stay, a rating the deck did not change is the re-export's,
// and the row's note goes (it is not fast any more). Before, such an import wrote the export's
// rating and BPM over the deck's (found on the real stick copies, #328 review R2).
func TestIssue328BDegradedFullImportKeepsTheDeckRating(t *testing.T) {
	for _, src := range []struct {
		name   string
		oneLib bool
	}{{"PDB", false}, {"OneLibrary", true}} {
		t.Run(src.name, func(t *testing.T) {
			root := t.TempDir()
			if src.oneLib {
				issue328OneLibrary(t, root, 2)
			} else {
				testfixture.LargeExport(t, root, issue328Tracks)
			}
			defer bridge.SetUpdateExportForTest()()
			ctx := context.Background()
			if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
				t.Fatalf("first mount: %+v %v", r, err)
			}
			issue328Deck(t, root, src.oneLib)
			before := issue328Before(t, root)
			issue328ReExport(t, root, src.oneLib)
			// Every track's analysis file unreadable from now on.
			testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", []byte("not an analysis file"))
			if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
				t.Fatalf("mount of the re-export: %+v %v", r, err)
			}
			if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Degraded == 0 {
				t.Fatalf("background update: %+v %v (want rows imported without their analysis)", r, err)
			}
			db := issue328Publish(t, root)
			if got := issue328Read(t, db, 6).Rating; got != 80 {
				t.Errorf("track 6: rating %d, the deck's is 80", got)
			}
			if got := issue328Read(t, db, 4); got.BPM != before[4].BPM || got.BPMRounded != before[4].BPMRounded {
				t.Errorf("track 4: BPM %v/%v, the deck's grid's is %v/%v", got.BPM, got.BPMRounded, before[4].BPM, before[4].BPMRounded)
			}
			for _, key := range issue328Rated {
				if got := issue328Read(t, db, key).Rating; got != 77 {
					t.Errorf("track %d: rating %d, the re-export's is 77", key, got)
				}
			}
			if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeFastBase`); n != 0 {
				t.Errorf("%d notes of fast rows left", n)
			}
		})
	}
}

func issue328StageKind(t *testing.T, stage string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stage, "KIND"))
	if err != nil {
		t.Fatalf("stage kind: %v", err)
	}
	return strings.TrimSpace(string(b))
}
