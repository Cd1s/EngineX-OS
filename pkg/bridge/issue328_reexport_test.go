package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #328 review R1.
//
// P1-A: a stage whose snapshot has no note of what the fast import left in its fast rows
// (fast-base.db: a stage of #253's or 5.3.0.5-r1's build, or the note lost) is not published over
// the DJ's edits made on those rows after the snapshot: it starts again from a new snapshot.
//
// P1-B: a stick re-exported while its rows are still fast. What the fast import wrote for the
// earlier record is not known, so nothing the deck can change is overwritten on such a row: the
// DJ's edits stay (the rating and the grid's BPM included, which the insert of the re-export used
// to overwrite at once); the re-export's title comes in. The limit: a cue rekordbox moved that the
// DJ did not touch stays where the deck has it, until the DJ's next export after the full import.

// issue328ReExport is the export at root re-exported: tracks 2 to 11 renamed, and on a OneLibrary
// export hot cue A of track 7 (which the DJ also moved) and hot cue B of track 2 (which the DJ did
// not touch) moved by rekordbox.
func issue328ReExport(t *testing.T, root string, oneLib bool) {
	t.Helper()
	if !oneLib {
		p := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for key := 2; key <= 11; key++ {
			old, new := issue328UTF16(fmt.Sprintf("Track %05d", key)), issue328UTF16(fmt.Sprintf("Remix %05d", key)) // the same length: the pages stay
			if bytes.Count(b, old) != 1 {
				t.Fatalf("export.pdb: the title of track %d found %d times", key, bytes.Count(b, old))
			}
			b = bytes.Replace(b, old, new, 1)
		}
		testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", b)
		return
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`UPDATE content SET title='Remix '||printf('%05d',content_id) WHERE content_id BETWEEN 2 AND 11`,
		`UPDATE cue SET inUsec=1000000 WHERE content_id=7 AND kind=1`,
		`UPDATE cue SET inUsec=1500000 WHERE content_id=2 AND kind=2`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// issue328UTF16 is s as export.pdb stores it (UTF-16LE).
func issue328UTF16(s string) []byte {
	var b []byte
	for _, r := range s {
		b = append(b, byte(r), byte(r>>8))
	}
	return b
}

// issue328RunReExport takes a stick through its fast publish, Engine's analysis of tracks 1 to 11
// and the DJ's edits on 2 to 11 (deck=false: none of this), a pull and a re-export
// (issue328ReExport), and the inserts that import the re-export in full and publish it. mode:
// "finished", the first insert's background update had finished before the edits (its stage is
// complete); "pulled", the stick was pulled before it started; "in place", the same, and the
// re-export small enough for the insert to import it in place.
func issue328RunReExport(t *testing.T, oneLib bool, mode string, deck bool) (*sql.DB, map[int]issue328Row) {
	t.Helper()
	root := t.TempDir()
	if oneLib {
		issue328OneLibrary(t, root, 2)
	} else {
		testfixture.LargeExport(t, root, issue328Tracks)
	}
	restore := bridge.SetUpdateExportForTest()
	defer func() { restore() }()
	ctx := context.Background()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || r.Added != issue328Tracks || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	if mode == "finished" {
		if r, err = bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
			t.Fatalf("first background update: %+v %v", r, err)
		}
	}
	var before map[int]issue328Row
	if deck {
		issue328Deck(t, root, oneLib)
		before = issue328Before(t, root)
	}
	issue328ReExport(t, root, oneLib)
	if mode == "in place" {
		restore()
		restore = func() {}
	}
	return issue328Publish(t, root), before
}

// issue328Deck is Engine's analysis of tracks 1 to 11 at their loads and the DJ's edits on 2 to 11.
func issue328Deck(t *testing.T, root string, oneLib bool) {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	for key := 1; key <= 11; key++ {
		issue328Analysed(t, db, key)
	}
	for _, e := range issue328Edits {
		if !e.oneLib || oneLib {
			e.edit(t, db, e.key)
		}
	}
}

// issue328Publish runs the inserts and background updates until a mount leaves nothing to the
// background, and returns the library, which must have no fast row left.
func issue328Publish(t *testing.T, root string) *sql.DB {
	db, _ := issue328PublishCounted(t, root)
	return db
}

// issue328PublishCounted is issue328Publish, with the number of mounts it took.
func issue328PublishCounted(t *testing.T, root string) (*sql.DB, int) {
	t.Helper()
	ctx := context.Background()
	mounts := 0
	for i := 0; ; i++ {
		mounts++
		r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
		if err != nil {
			t.Fatalf("mount %d: %+v %v", i+1, r, err)
		}
		if !r.Background {
			break
		}
		if i == 3 {
			t.Fatalf("still updating in the background after %d mounts", i+1)
		}
		if r, err = bridge.ImportInBackground(ctx, root, nil); err != nil {
			t.Fatalf("background update %d: %+v %v", i+1, r, err)
		}
	}
	db := dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeState WHERE fingerprint LIKE 'fast:%'`); n != 0 {
		t.Fatalf("%d tracks still fast after the publish", n)
	}
	return db, mounts
}

// issue328TheDJsEdits checks that the DJ's edits of issue328Edits are in the published library db.
// before: the rows as the deck had them before the publish (the grids and their BPM).
func issue328TheDJsEdits(t *testing.T, db *sql.DB, before map[int]issue328Row, oneLib bool) {
	t.Helper()
	if gs, _ := issue328Cues(t, issue328Read(t, db, 2).Cues); gs[2].Label != "Deck 328" || gs[2].Start != 2*issue328Rate {
		t.Errorf("hot cue: the DJ's slot C is %+v", gs[2])
	}
	gl := issue328Loops(t, issue328Read(t, db, 3).Loops)
	if got := issue328Read(t, db, 3); gl[1].Label != "Deck Loop" || gl[1].Start != 2.5*issue328Rate || got.Active.Int64&2 == 0 {
		t.Errorf("saved loop: the DJ's slot 2 is %+v, active %v", gl[1], got.Active)
	}
	for _, key := range []int{4, 5} {
		if got := issue328Read(t, db, key); !bytes.Equal(got.Beats, before[key].Beats) || got.BPM != before[key].BPM || got.BPMRounded != before[key].BPMRounded {
			t.Errorf("grid of track %d: not the DJ's (BPM %v/%v, the DJ's %v/%v)", key, got.BPM, got.BPMRounded, before[key].BPM, before[key].BPMRounded)
		}
	}
	if got := issue328Read(t, db, 5); got.Locked != 1 {
		t.Errorf("grid lock: unlocked")
	}
	if got := issue328Read(t, db, 6); got.Rating != 80 {
		t.Errorf("rating: %d, the DJ's was 80", got.Rating)
	}
	if !oneLib {
		return
	}
	for key, want := range map[int]func(s issue328Slot) bool{
		7:  func(s issue328Slot) bool { return s.Start == 1.5*issue328Rate },
		8:  func(s issue328Slot) bool { return s.Start < 0 },
		9:  func(s issue328Slot) bool { return s.Label == "Deck 328" },
		10: func(s issue328Slot) bool { return s.Color == [4]byte{255, 0x30, 0xff, 0x5a} },
	} {
		if gs, _ := issue328Cues(t, issue328Read(t, db, key).Cues); !want(gs[0]) {
			t.Errorf("track %d: hot cue A is %+v, not the DJ's", key, gs[0])
		}
	}
}

// issue328Before reads tracks 1 to 12 of the library at root as the deck has them.
func issue328Before(t *testing.T, root string) map[int]issue328Row {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	before := map[int]issue328Row{}
	for key := 1; key <= 12; key++ {
		before[key] = issue328Read(t, db, key)
	}
	return before
}

// The reviewer's reproduction: the stage's fast-base.db gone after the snapshot. stayed mounted:
// the update finished, then the DJ's edits; pulled: the update was cut after its first pass (the
// stick pulled), the edits made, and the stick inserted again.
func TestIssue328AStageWithoutItsFastBaseIsNotPublishedOverTheDJsEdits(t *testing.T) {
	for _, src := range []struct {
		name   string
		oneLib bool
	}{{"PDB", false}, {"OneLibrary", true}} {
		for _, path := range []struct {
			name   string
			pulled bool
		}{{"stayed mounted", false}, {"pulled during the update", true}} {
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
				base := filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-import", enginedb.FastBaseFile)
				if err := os.Remove(base); err != nil {
					t.Fatalf("the snapshot's note of its fast rows: %v", err)
				}
				issue328Deck(t, root, src.oneLib)
				before := issue328Before(t, root)
				db, mounts := issue328PublishCounted(t, root)
				issue328TheDJsEdits(t, db, before, src.oneLib)
				// The stage starts again once: stayed mounted, the publishing mount refuses it; pulled,
				// the background job takes the snapshot again before its passes. Either way the
				// second mount publishes.
				if mounts != 2 {
					t.Errorf("published at mount %d, want 2", mounts)
				}
			})
		}
	}
}

func TestIssue328AReExportWhileFastKeepsTheDJsEdits(t *testing.T) {
	for _, src := range []struct {
		name   string
		oneLib bool
	}{{"PDB", false}, {"OneLibrary", true}} {
		for _, mode := range []string{"finished", "pulled", "in place"} {
			t.Run(src.name+"/"+mode, func(t *testing.T) {
				db, before := issue328RunReExport(t, src.oneLib, mode, true)
				for key := 2; key <= 11; key++ {
					if got := scalar[string](t, db, `SELECT title FROM Track WHERE pdbImportKey=?`, key); got != fmt.Sprintf("Remix %05d", key) {
						t.Errorf("track %d: title %q, the re-export's is %q", key, got, fmt.Sprintf("Remix %05d", key))
					}
				}
				issue328TheDJsEdits(t, db, before, src.oneLib)
				if !src.oneLib {
					return
				}
				// The fallback's limit: hot cue B of track 2, which rekordbox moved and the DJ did not
				// touch, stays where the deck has it.
				gs, _ := issue328Cues(t, issue328Read(t, db, 2).Cues)
				if bs, _ := issue328Cues(t, before[2].Cues); gs[1] != bs[1] {
					t.Errorf("hot cue B of track 2 is %+v, the deck had %+v", gs[1], bs[1])
				}
			})
		}
	}
}
