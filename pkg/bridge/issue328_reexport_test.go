package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
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
// P1-B: a stick re-exported while its rows are still fast. The DJ's edits stay (the rating and
// the grid's BPM included, which the insert of the re-export used to overwrite at once); the
// re-export's title comes in.
//
// #328 review R2, P1-1 (the maintainer's rule H, 2026-11-22): the deck's value is kept only where
// the deck changed it; a value the deck did not change takes rekordbox's new export, field by
// field. R1 kept every rating and every cue of such a row (what the fast import wrote for the
// earlier record was not known): a rating rekordbox's re-export gave a track the deck never rated
// stayed 0, and a cue rekordbox moved that the DJ did not touch stayed where the deck had it. The
// state now keeps what the import left in each fast row (BridgeFastBase), and the insert of the
// re-export carries it to the new record.

// issue328ReExport is the export at root re-exported: tracks 2 to 11 renamed, tracks 1 (loaded on
// the deck, not edited) and 12 (never loaded) rated 77 in rekordbox, and on a OneLibrary export
// hot cue A of track 7 (which the DJ also moved) and hot cue B of track 2 (which the DJ did not
// touch) moved by rekordbox.
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
		for _, key := range issue328Rated {
			issue328PDBRating(t, b, key, 77)
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
		`UPDATE content SET rating=77 WHERE content_id IN (1,12)`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// issue328Rated are the tracks issue328ReExport rates 77 in rekordbox: one the deck loaded and did
// not edit, one it never loaded.
var issue328Rated = []int{1, 12}

// issue328PDBRating sets the rating of track key's row in b, an export.pdb of LargePDB's layout
// (the sample rate at 8, the bitrate at 48, the tempo at 56, the id at 72, the length at 84; the
// rating at 89, as rekordbox writes it).
func issue328PDBRating(t *testing.T, b []byte, key int, rating byte) {
	t.Helper()
	le := binary.LittleEndian
	at := -1
	for o := 0; o+136 <= len(b); o++ {
		if le.Uint32(b[o+8:]) == 44100 && le.Uint32(b[o+48:]) == 128 && le.Uint32(b[o+56:]) == 12000 && le.Uint32(b[o+72:]) == uint32(key) && le.Uint16(b[o+84:]) == 8 {
			if at >= 0 {
				t.Fatalf("export.pdb: the row of track %d found twice", key)
			}
			at = o
		}
	}
	if at < 0 {
		t.Fatalf("export.pdb: no row of track %d", key)
	}
	b[at+89] = rating
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
// re-export small enough for the insert to import it in place; "edited after the copy", as
// "pulled", and the DJ rates track 20 after the re-export's update copied the library.
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
	if mode == "edited after the copy" {
		// The re-export's mount, its update to the end, then the DJ rates track 20 (fast still)
		// before the mount that publishes: the publish carries that edit over the copy.
		if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
			t.Fatalf("mount of the re-export: %+v %v", r, err)
		}
		if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
			t.Fatalf("its background update: %+v %v", r, err)
		}
		if deck {
			db := dbOpen(t, root)
			if _, err := db.Exec(`UPDATE Track SET rating=60 WHERE pdbImportKey=20`); err != nil {
				t.Fatal(err)
			}
			db.Close()
		}
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
	if bridge.InChildForTest(t) {
		return
	}
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
	if bridge.InChildForTest(t) {
		return
	}
	for _, src := range []struct {
		name   string
		oneLib bool
	}{{"PDB", false}, {"OneLibrary", true}} {
		for _, mode := range []string{"finished", "pulled", "in place", "edited after the copy"} {
			t.Run(src.name+"/"+mode, func(t *testing.T) {
				db, before := issue328RunReExport(t, src.oneLib, mode, true)
				for key := 2; key <= 11; key++ {
					if got := scalar[string](t, db, `SELECT title FROM Track WHERE pdbImportKey=?`, key); got != fmt.Sprintf("Remix %05d", key) {
						t.Errorf("track %d: title %q, the re-export's is %q", key, got, fmt.Sprintf("Remix %05d", key))
					}
				}
				issue328TheDJsEdits(t, db, before, src.oneLib)
				// Rule H (review R2): what the deck did not change is the re-export's, as on the
				// same stick without the deck (R1 kept hot cue B of track 2 where the deck had it,
				// though only rekordbox moved it).
				control, _ := issue328RunReExport(t, src.oneLib, mode, false)
				issue328RuleH(t, db, control, src.oneLib)
				if got := issue328Read(t, db, 20).Rating; mode == "edited after the copy" && got != 60 {
					t.Errorf("track 20: rating %d, the DJ's after the copy was 60", got)
				}
			})
		}
	}
}

// The reviewer's case (#328 delta review 1, P1-1): the deck rated track 6 (80) and never rated
// tracks 1 and 12; rekordbox's re-export rates 1 and 12 77. The deck's 80 stays, and 1 and 12 are
// 77: R1 left them 0 on both export shapes. "twice": the stick pulled after the mount of the
// re-export and exported once more (1 and 12 rated 55) before the update read it.
func TestIssue328BReExportTakesTheRatingsTheDeckDidNotChange(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	for _, src := range []struct {
		name   string
		oneLib bool
	}{{"PDB", false}, {"OneLibrary", true}} {
		for _, mode := range []string{"finished", "pulled", "in place", "twice"} {
			t.Run(src.name+"/"+mode, func(t *testing.T) {
				var db *sql.DB
				want := int64(77)
				if mode == "twice" {
					db, want = issue328RunReExportTwice(t, src.oneLib), 55
				} else {
					db, _ = issue328RunReExport(t, src.oneLib, mode, true)
				}
				if got := issue328Read(t, db, 6).Rating; got != 80 {
					t.Errorf("track 6: rating %d, the deck's is 80", got)
				}
				for _, key := range issue328Rated {
					if got := issue328Read(t, db, key).Rating; got != want {
						t.Errorf("track %d: rating %d, the re-export's is %d and the deck did not change it", key, got, want)
					}
				}
				// What the imports left in the fast rows goes with them.
				if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeFastBase`); n != 0 {
					t.Errorf("%d notes of fast rows left after the last one was read in full", n)
				}
			})
		}
	}
}

// issue328RunReExportTwice is issue328RunReExport's "pulled" with the re-export's mount (the
// insert of its new tracks and records, the update not started) and one more export before the
// update: tracks 1 and 12 rated 55 in rekordbox.
func issue328RunReExportTwice(t *testing.T, oneLib bool) *sql.DB {
	t.Helper()
	root := t.TempDir()
	if oneLib {
		issue328OneLibrary(t, root, 2)
	} else {
		testfixture.LargeExport(t, root, issue328Tracks)
	}
	defer bridge.SetUpdateExportForTest()()
	ctx := context.Background()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	issue328Deck(t, root, oneLib)
	issue328ReExport(t, root, oneLib)
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("mount of the re-export: %+v %v", r, err)
	}
	live := dbOpen(t, root)
	if n := scalar[int](t, live, `SELECT count(*) FROM bridge.BridgeState WHERE fingerprint LIKE 'fast:%'`); n != issue328Tracks {
		t.Fatalf("%d fast rows after the mount of the re-export, want %d", n, issue328Tracks)
	}
	live.Close()
	issue328ReRate(t, root, oneLib, 55)
	return issue328Publish(t, root)
}

// issue328ReRate rates tracks issue328Rated in the export at root.
func issue328ReRate(t *testing.T, root string, oneLib bool, rating byte) {
	t.Helper()
	if !oneLib {
		b, err := os.ReadFile(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range issue328Rated {
			issue328PDBRating(t, b, key, rating)
		}
		testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", b)
		return
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE content SET rating=? WHERE content_id IN (1,12)`, int(rating)); err != nil {
		t.Fatal(err)
	}
}

// issue328DeckChanged is what issue328Edits changed on track key: its rating, its grid (with the
// Track's BPM and the lock), its saved loops (with activeOnLoadLoops) or hot cue slots.
func issue328DeckChanged(key int, oneLib bool) (rating, grid, loops bool, slots map[int]bool) {
	switch key {
	case 2:
		return false, false, false, map[int]bool{2: true}
	case 3:
		return false, false, true, nil
	case 4, 5:
		return false, true, false, nil
	case 6:
		return true, false, false, nil
	case 7, 8, 9, 10:
		if oneLib {
			return false, false, false, map[int]bool{0: true}
		}
	}
	// Track 11 (OneLibrary): every hot cue the fast import listed deleted; rekordbox's come back
	// (the rule's limit, fastedits.go), as without the deck.
	return false, false, false, nil
}

// issue328RuleH checks, field by field, that every value of tracks 1 to 12 the deck did not change
// in db is the one control (the same stick and re-export, no deck) has: rekordbox's.
func issue328RuleH(t *testing.T, db, control *sql.DB, oneLib bool) {
	t.Helper()
	for key := 1; key <= 12; key++ {
		rating, grid, loops, slots := issue328DeckChanged(key, oneLib)
		got, want := issue328Read(t, db, key), issue328Read(t, control, key)
		if !rating && got.Rating != want.Rating {
			t.Errorf("track %d: rating %d, rekordbox's %d", key, got.Rating, want.Rating)
		}
		if !grid && (!bytes.Equal(got.Beats, want.Beats) || got.BPM != want.BPM || got.BPMRounded != want.BPMRounded || got.Locked != want.Locked) {
			t.Errorf("track %d: grid (BPM %v/%v, locked %d) not rekordbox's (BPM %v/%v, locked %d)", key, got.BPM, got.BPMRounded, got.Locked, want.BPM, want.BPMRounded, want.Locked)
		}
		if !loops && (!bytes.Equal(got.Loops, want.Loops) || got.Active != want.Active) {
			t.Errorf("track %d: saved loops (active %v) not rekordbox's (active %v)", key, got.Active, want.Active)
		}
		if got.Cues == nil || want.Cues == nil {
			if !bytes.Equal(got.Cues, want.Cues) {
				t.Errorf("track %d: hot cues %x, rekordbox's %x", key, got.Cues, want.Cues)
			}
			continue
		}
		gs, gt := issue328Cues(t, got.Cues)
		ws, wt := issue328Cues(t, want.Cues)
		for i := range gs {
			if !slots[i] && gs[i] != ws[i] {
				t.Errorf("track %d: hot cue %c is %+v, rekordbox's %+v", key, 'A'+i, gs[i], ws[i])
			}
		}
		if !bytes.Equal(gt, wt) {
			t.Errorf("track %d: main cue %x, rekordbox's %x", key, gt, wt)
		}
	}
}
