package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

type overlapTrackSnapshot struct {
	id     int64
	rating int
	cues   []byte
}

func writeSyntheticOneLibrary(t *testing.T, root string, count, idOffset int) string {
	t.Helper()
	p := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := `
CREATE TABLE content(
	content_id INTEGER PRIMARY KEY,
	title TEXT,
	path TEXT,
	analysisDataFilePath TEXT,
	bpmx100 INTEGER,
	length INTEGER,
	samplingRate INTEGER,
	bit_rate INTEGER,
	file_size INTEGER,
	rating INTEGER DEFAULT 0
);
CREATE TABLE playlist(
	playlist_id INTEGER PRIMARY KEY,
	playlist_id_parent INTEGER DEFAULT 0,
	sequenceNo INTEGER DEFAULT 1,
	name TEXT,
	is_folder INTEGER DEFAULT 0
);
CREATE TABLE playlist_content(
	playlist_id INTEGER,
	sequenceNo INTEGER,
	content_id INTEGER
);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO playlist(playlist_id,playlist_id_parent,sequenceNo,name,is_folder) VALUES(1,0,1,'Large Set',0)`); err != nil {
		t.Fatal(err)
	}
	fileSize := len(testfixture.MP3WithLAMETag())
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insertContent, err := tx.Prepare(`INSERT INTO content(content_id,title,path,analysisDataFilePath,bpmx100,length,samplingRate,bit_rate,file_size,rating) VALUES(?,?,?,?,12000,8,44100,128,?,0)`)
	if err != nil {
		t.Fatal(err)
	}
	insertEntry, err := tx.Prepare(`INSERT INTO playlist_content(playlist_id,sequenceNo,content_id) VALUES(1,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= count; i++ {
		id := i + idOffset
		title := fmt.Sprintf("Track %05d", i)
		relPath := "/" + testfixture.LargeTrackPath(i)
		if _, err := insertContent.Exec(id, title, relPath, "/PIONEER/USBANLZ/P001/ANLZ0000.DAT", fileSize); err != nil {
			t.Fatal(err)
		}
		if _, err := insertEntry.Exec(i, id); err != nil {
			t.Fatal(err)
		}
	}
	insertContent.Close()
	insertEntry.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return p
}

func buildOverlapFlipStick(t *testing.T, count int) (root, pdbPath, onePath string, base time.Time, want map[string]overlapTrackSnapshot) {
	t.Helper()
	root = t.TempDir()
	testfixture.LargeExport(t, root, count)
	pdbPath = filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	onePath = writeSyntheticOneLibrary(t, root, count, 5) // IDs 6..65 on the same files as PDB IDs 1..60
	base = time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(pdbPath, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(onePath, base, base); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || res.Added != count || res.Background || res.Bypassed {
		t.Fatalf("initial overlap union import: %+v %v", res, err)
	}

	db := dbOpen(t, root)
	defer db.Close()
	want = make(map[string]overlapTrackSnapshot, count)
	for i := 1; i <= count; i++ {
		enginePath := "../" + testfixture.LargeTrackPath(i)
		var id int64
		var origCues []byte
		if err := db.QueryRow(`SELECT t.id, p.quickCues FROM Track t JOIN PerformanceData p ON p.trackId=t.id WHERE t.path=?`, enginePath).Scan(&id, &origCues); err != nil {
			t.Fatalf("query track %d (%s): %v", i, enginePath, err)
		}
		cues := append([]byte(nil), origCues...)
		if len(cues) < 2 {
			cues = append(cues, 0xaa, 0x55)
		}
		// Give every track a distinct deck-edited cue blob and rating so any cross-file row move is detected.
		cues[len(cues)-2] ^= byte(i)
		cues[len(cues)-1] ^= byte(i ^ 0x5a)
		rating := 20 + i
		if _, err := db.Exec(`UPDATE Track SET rating=? WHERE id=?`, rating, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, cues, id); err != nil {
			t.Fatal(err)
		}
		want[enginePath] = overlapTrackSnapshot{id: id, rating: rating, cues: cues}
	}
	return root, pdbPath, onePath, base, want
}

func assertOverlapRows(t *testing.T, root string, want map[string]overlapTrackSnapshot, stageLabel string) {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	if total := scalar[int](t, db, `SELECT count(*) FROM Track`); total != len(want) {
		t.Fatalf("%s: Track table count=%d, want %d", stageLabel, total, len(want))
	}
	rows, err := db.Query(`SELECT t.path, t.id, t.rating, p.quickCues FROM Track t LEFT JOIN PerformanceData p ON p.trackId=t.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	seenIDs := map[int64]string{}
	for rows.Next() {
		var p string
		var id int64
		var rating int
		var cues []byte
		if err := rows.Scan(&p, &id, &rating, &cues); err != nil {
			t.Fatal(err)
		}
		seen++
		exp, ok := want[p]
		if !ok {
			t.Fatalf("%s: unexpected track path %q (id=%d)", stageLabel, p, id)
		}
		if prev, dup := seenIDs[id]; dup {
			t.Fatalf("%s: duplicate row id %d on %q and %q", stageLabel, id, prev, p)
		}
		seenIDs[id] = p
		if id != exp.id {
			t.Fatalf("%s: path %q moved to row id %d, want %d", stageLabel, p, id, exp.id)
		}
		if rating != exp.rating {
			t.Fatalf("%s: path %q rating=%d, want %d", stageLabel, p, rating, exp.rating)
		}
		if !bytes.Equal(cues, exp.cues) {
			t.Fatalf("%s: path %q deck quickCues changed", stageLabel, p)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(want) {
		t.Fatalf("%s: track row count=%d, want %d", stageLabel, seen, len(want))
	}
}

func twoExportStamp(t *testing.T, firstRel, firstPath, secondRel, secondPath string) string {
	t.Helper()
	st1, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("v4 %s %d %d | %s %d %d",
		firstRel, st1.Size(), st1.ModTime().UnixNano(),
		secondRel, st2.Size(), st2.ModTime().UnixNano())
}

func finishBackgroundUpdate(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	bgRes, err := bridge.ImportInBackground(ctx, root, nil)
	if err != nil {
		t.Fatalf("ImportInBackground: %v", err)
	}
	if !bgRes.Bypassed {
		pubRes, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
		if err != nil || pubRes.Background {
			t.Fatalf("publish after background: %+v %v", pubRes, err)
		}
	}
}

// Pins M1b: handToMounts zero-work branch must write both the stamp and .rb-bridge-read.
func TestIssue327HandToMountsZeroWorkRefreshesReadMark(t *testing.T) {
	root := t.TempDir()
	// Use matched-case paths for both exports so PlanUpdate sees zero work and enters
	// handToMounts's work==0 branch (testfixture.Export intentionally mismatches /Music vs MUSIC).
	testfixture.LargeExport(t, root, 5)
	for _, name := range []string{"one.wav", "two.wav", "three.wav"} {
		testfixture.Write(t, root, "MUSIC/"+name, testfixture.WAV(true))
	}
	pdb := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	one := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, one)
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(pdb, when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(one, when, when); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Added != 8 || res.Background || res.Bypassed {
		t.Fatalf("initial mount import: %+v %v", res, err)
	}

	later := when.Add(time.Minute)
	if err := os.Chtimes(pdb, later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(one, later, later); err != nil {
		t.Fatal(err)
	}
	defer bridge.SetLargeExportForTest()()
	defer bridge.SetInPlaceUpdateForTest(1)()
	handoffStamped := false
	defer bridge.SetCutForTest(func(name string) {
		if name == "handoff stamped" {
			handoffStamped = true
		}
	})()

	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !res.Background {
		t.Fatalf("mount before zero-work handoff: %+v %v", res, err)
	}
	bgRes, err := bridge.ImportInBackground(ctx, root, nil)
	if err != nil || !bgRes.Bypassed {
		t.Fatalf("zero-work handToMounts background: %+v %v", bgRes, err)
	}
	if !handoffStamped {
		t.Fatal("handToMounts zero-work branch (handoff stamped) was not reached")
	}

	dir := filepath.Join(root, "Engine Library/Database2")
	stampRaw, err := os.ReadFile(filepath.Join(dir, ".rb-bridge-stamp"))
	if err != nil {
		t.Fatal(err)
	}
	readRaw, err := os.ReadFile(filepath.Join(dir, ".rb-bridge-read"))
	if err != nil {
		t.Fatal(err)
	}
	stamp := strings.TrimSpace(string(stampRaw))
	firstLine, _, _ := strings.Cut(string(readRaw), "\n")
	if strings.TrimSpace(firstLine) != stamp {
		t.Fatalf(".rb-bridge-read first line %q != stamp %q", firstLine, stamp)
	}

	next, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !next.Bypassed {
		t.Fatalf("next insert after handToMounts zero-work was not bypassed: %+v %v", next, err)
	}
}

// Pins M12a: PathFirst in quickUpdate during overlapping-ID library flips.
func TestIssue327OverlapFlipQuickUpdate(t *testing.T) {
	root, pdbPath, onePath, base, want := buildOverlapFlipStick(t, 60)
	defer bridge.SetUpdateExportForTest()()
	ctx := context.Background()

	flip1 := base.Add(100 * time.Second)
	if err := os.Chtimes(pdbPath, flip1, flip1); err != nil {
		t.Fatal(err)
	}
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !res.Background {
		t.Fatalf("flip1 quickUpdate mount: %+v %v", res, err)
	}
	assertOverlapRows(t, root, want, "quickUpdate flip1 mount")
	finishBackgroundUpdate(t, ctx, root)
	assertOverlapRows(t, root, want, "quickUpdate flip1 published")

	flip2 := base.Add(200 * time.Second)
	if err := os.Chtimes(onePath, flip2, flip2); err != nil {
		t.Fatal(err)
	}
	res, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !res.Background {
		t.Fatalf("flip2 quickUpdate mount: %+v %v", res, err)
	}
	assertOverlapRows(t, root, want, "quickUpdate flip2 mount")
	finishBackgroundUpdate(t, ctx, root)
	assertOverlapRows(t, root, want, "quickUpdate flip2 published")
}

// Pins M12b: PathFirst in quickInStage during overlapping-ID library flips.
func TestIssue327OverlapFlipQuickInStage(t *testing.T) {
	root, pdbPath, onePath, base, want := buildOverlapFlipStick(t, 60)
	defer bridge.SetLargeExportForTest()()
	defer bridge.SetMountWindowForTest(time.Second)()
	ctx := context.Background()

	runStageFlip := func(label string, touchPath string, ts time.Time) {
		if err := os.Chtimes(touchPath, ts, ts); err != nil {
			t.Fatal(err)
		}
		res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
		if err != nil || !res.Background {
			t.Fatalf("%s mount: %+v %v", label, res, err)
		}
		if b, err := os.ReadFile(stagePath(root, "QUICK")); err != nil || len(b) == 0 {
			t.Fatalf("%s expected QUICK stage mark: %v", label, err)
		}
		if _, err := bridge.ImportInBackground(ctx, root, nil); err != nil {
			t.Fatalf("%s quickInStage background: %v", label, err)
		}
		res, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
		if err != nil || !res.Background {
			t.Fatalf("%s publish quickInStage: %+v %v", label, res, err)
		}
		assertOverlapRows(t, root, want, label+" after quickInStage publish")
		finishBackgroundUpdate(t, ctx, root)
		assertOverlapRows(t, root, want, label+" after full publish")
	}

	runStageFlip("quickInStage flip1", pdbPath, base.Add(100*time.Second))
	runStageFlip("quickInStage flip2", onePath, base.Add(200*time.Second))
}

// Pins M12c (PathFirst) and M12f (MatchByPath) in the background update pass (update.go:439).
func TestIssue327OverlapFlipBackgroundPass(t *testing.T) {
	root, pdbPath, onePath, base, want := buildOverlapFlipStick(t, 60)
	defer bridge.SetLargeExportForTest()()
	defer bridge.SetInPlaceUpdateForTest(1 << 40)()
	ctx := context.Background()
	dir := filepath.Join(root, "Engine Library/Database2")

	stagePassCount := 0
	defer bridge.SetCutForTest(func(name string) {
		if name == "stage pass committed" {
			stagePassCount++
		}
	})()

	runBgFlip := func(label, touchPath string, ts time.Time, firstRel, firstPath, secondRel, secondPath string) {
		if err := os.Chtimes(touchPath, ts, ts); err != nil {
			t.Fatal(err)
		}
		// Mark pending with the current export stamp so ConvertAtMount skips quickUpdate
		// and lets updateInBackground run its stage pass directly against the un-flipped live library.
		curStamp := twoExportStamp(t, firstRel, firstPath, secondRel, secondPath)
		if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-pending"), []byte(curStamp+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
		if err != nil || !res.Background {
			t.Fatalf("%s mount: %+v %v", label, res, err)
		}
		if _, err := os.Stat(stagePath(root, "QUICK")); !os.IsNotExist(err) {
			t.Fatalf("%s unexpected QUICK stage mark: %v", label, err)
		}
		beforePasses := stagePassCount
		finishBackgroundUpdate(t, ctx, root)
		if stagePassCount == beforePasses {
			t.Fatalf("%s did not execute background stage pass", label)
		}
		assertOverlapRows(t, root, want, label)
	}

	runBgFlip("background flip1", pdbPath, base.Add(100*time.Second),
		"PIONEER/rekordbox/export.pdb", pdbPath,
		"PIONEER/rekordbox/exportLibrary.db", onePath)
	runBgFlip("background flip2", onePath, base.Add(200*time.Second),
		"PIONEER/rekordbox/exportLibrary.db", onePath,
		"PIONEER/rekordbox/export.pdb", pdbPath)
}

// Pins M12d: opts.PathFirst in the in-place update pass (bridge.go:971).
func TestIssue327OverlapFlipInPlace(t *testing.T) {
	root, pdbPath, onePath, base, want := buildOverlapFlipStick(t, 60)
	ctx := context.Background()

	flip1 := base.Add(100 * time.Second)
	if err := os.Chtimes(pdbPath, flip1, flip1); err != nil {
		t.Fatal(err)
	}
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || res.Background || res.Bypassed {
		t.Fatalf("in-place flip1: %+v %v", res, err)
	}
	assertOverlapRows(t, root, want, "in-place flip1")

	flip2 := base.Add(200 * time.Second)
	if err := os.Chtimes(onePath, flip2, flip2); err != nil {
		t.Fatal(err)
	}
	res, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || res.Background || res.Bypassed {
		t.Fatalf("in-place flip2: %+v %v", res, err)
	}
	assertOverlapRows(t, root, want, "in-place flip2")
}

// Pins M12e: quickPaths must not rewrite a track's path onto an old path when the export path
// is already occupied by another existing row (same primary source, flip == false).
func TestIssue327QuickPathsGuardKeepsOccupiedExportPaths(t *testing.T) {
	const count = 60
	root := t.TempDir()
	testfixture.LargeExport(t, root, count)
	if err := os.Remove(filepath.Join(root, "PIONEER/rekordbox/export.pdb")); err != nil {
		t.Fatal(err)
	}
	onePath := writeSyntheticOneLibrary(t, root, count, 0) // IDs 1..60
	base := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(onePath, base, base); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Added != count {
		t.Fatalf("initial OneLibrary import: %+v %v", res, err)
	}

	// Same primary export re-exported with shifted IDs 6..65 on the same 60 files (flip == false).
	onePath = writeSyntheticOneLibrary(t, root, count, 5)
	later := base.Add(100 * time.Second)
	if err := os.Chtimes(onePath, later, later); err != nil {
		t.Fatal(err)
	}
	defer bridge.SetUpdateExportForTest()()
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !res.Background {
		t.Fatalf("quickUpdate with shifted IDs: %+v %v", res, err)
	}

	db := dbOpen(t, root)
	defer db.Close()
	if got := scalar[int](t, db, `SELECT count(*) FROM Track`); got != count {
		t.Fatalf("quickUpdate dropped occupied paths: track count=%d, want %d", got, count)
	}
	for i := 1; i <= count; i++ {
		enginePath := "../" + testfixture.LargeTrackPath(i)
		wantTitle := fmt.Sprintf("Track %05d", i)
		var gotTitle string
		if err := db.QueryRow(`SELECT title FROM Track WHERE path=?`, enginePath).Scan(&gotTitle); err != nil {
			t.Fatalf("missing track at %q after quickUpdate: %v", enginePath, err)
		}
		if gotTitle != wantTitle {
			t.Fatalf("track %q got title %q, want %q", enginePath, gotTitle, wantTitle)
		}
	}
}

// Pins N1: readLibraryDetailed must compare the stale read mark against the current export stamp,
// not the older stored stamp.
func TestIssue327StaleReadMarkDiffersFromCurrentExportStamp(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	for _, name := range []string{"one.wav", "two.wav", "three.wav"} {
		testfixture.Write(t, root, "MUSIC/"+name, testfixture.WAV(true))
	}
	one := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, one)
	pdb := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(one, when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdb, when.Add(30*time.Second), when.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Added != 7 {
		t.Fatalf("initial union import: %+v %v", res, err)
	}
	db := dbOpen(t, root)
	if _, err := db.Exec(`UPDATE Track SET rating=77`); err != nil {
		t.Fatal(err)
	}
	before := scalar[int](t, db, `SELECT count(*) FROM Track`)
	db.Close()

	// Leave the stored .rb-bridge-stamp with export.pdb primary, make .rb-bridge-read stale
	// relative to both the stored stamp and the new export stamp, then advance exportLibrary.db
	// so the current export stamp differs from the stored stamp and makes exportLibrary.db primary.
	dir := filepath.Join(root, "Engine Library/Database2")
	staleMark := "v4 PIONEER/rekordbox/export.pdb 1 1 | PIONEER/rekordbox/exportLibrary.db 1 1\nPIONEER/rekordbox/export.pdb\nPIONEER/rekordbox/exportLibrary.db\n"
	if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-read"), []byte(staleMark), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(one, when.Add(2*time.Minute), when.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(one+"-wal", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	log.SetOutput(oldWriter)
	if err != nil || !res.Bypassed {
		t.Fatalf("stale mark with newer unreadable OneLibrary did not freeze: %+v %v logs=%q", res, err, logs.String())
	}
	if !strings.Contains(logs.String(), "existing library kept") {
		t.Fatalf("missing existing library kept journal line: %q", logs.String())
	}
	db = dbOpen(t, root)
	defer db.Close()
	if got := scalar[int](t, db, `SELECT count(*) FROM Track`); got != before {
		t.Fatalf("track count changed: %d -> %d", before, got)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM Track WHERE rating=77`); got != before {
		t.Fatalf("ratings changed: %d -> %d", before, got)
	}
}

// Pins M4: when two owned Engine rows share a source ID that still exists in the export,
// Sync must pick the next free folded-path candidate in either twin order rather than failing
// with ambiguous bridge source ID.
func TestIssue327DuplicateSharedSourceReassignsFreeFoldedCandidate(t *testing.T) {
	orders := []struct {
		name         string
		first, other string
	}{
		{"lower_then_upper", "three.wav", "THREE.wav"},
		{"upper_then_lower", "THREE.wav", "three.wav"},
	}
	for _, tc := range orders {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			testfixture.Export(t, root)
			testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb",
				testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", tc.first, tc.other, "five.wav"}, "r6-m4"))
			testfixture.Write(t, root, "MUSIC/THREE.wav", testfixture.WAV(false))
			ctx := context.Background()
			library, err := bridge.ReadLibrary(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := enginedb.Sync(ctx, root, library, enginedb.Options{MatchByPath: true}); err != nil {
				t.Fatal(err)
			}
			firstPath := "../MUSIC/" + tc.first
			otherPath := "../MUSIC/" + tc.other
			db := dbOpen(t, root)
			var firstID, secondID, firstImportKey int64
			var sourceJSON []byte
			if err := db.QueryRow(`SELECT t.id, t.pdbImportKey, ba.sourceJSON FROM bridge.BridgeAnalysis ba JOIN Track t ON t.id=ba.trackId WHERE t.path=?`, firstPath).Scan(&firstID, &firstImportKey, &sourceJSON); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT id FROM Track WHERE path=?`, otherPath).Scan(&secondID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE Track SET pdbImportKey=? WHERE id=?`, firstImportKey, secondID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE BridgeAnalysis SET sourceJSON=? WHERE trackId=?`, sourceJSON, secondID); err != nil {
				t.Fatal(err)
			}
			db.Close()

			// Re-sync with the shared source ID still present in targets (MatchByPath=true, PathFirst=false):
			// targets[source] clears pathMatch, so the second row hits the duplicate-owner fallback loop and
			// must claim the unused folded-path twin rather than failing with ambiguous bridge source ID.
			library, err = bridge.ReadLibrary(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := enginedb.Sync(ctx, root, library, enginedb.Options{MatchByPath: true}); err != nil {
				t.Fatalf("duplicate shared source sync failed: %v", err)
			}
			db = dbOpen(t, root)
			defer db.Close()
			if got := scalar[int64](t, db, `SELECT id FROM Track WHERE path=?`, firstPath); got != firstID {
				t.Fatalf("%s id=%d, want %d", firstPath, got, firstID)
			}
			if got := scalar[int64](t, db, `SELECT id FROM Track WHERE path=?`, otherPath); got != secondID {
				t.Fatalf("%s id=%d, want %d", otherPath, got, secondID)
			}
		})
	}
}

// Pins M8a and M8b: a staged-only pass must retain both path-derived local tracks (source >= 1<<31)
// and path-derived local playlists (BridgePlaylist.sourceId >= 1<<31).
func TestIssue327StagedOnlyRetainsPathDerivedLocalTracksAndPlaylists(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	for _, name := range []string{"one.wav", "two.wav", "three.wav"} {
		testfixture.Write(t, root, "MUSIC/"+name, testfixture.WAV(true))
	}
	copyEncryptedOneLibraryFixture(t, filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"))
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(filepath.Join(root, "PIONEER/rekordbox/export.pdb"), when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"), when, when); err != nil {
		t.Fatal(err)
	}

	staged := filepath.Join(root, ".prolink-cache", "CDJ-3000")
	for _, name := range []string{"staged-one.wav", "staged-two.wav", "staged-three.wav"} {
		testfixture.Write(t, staged, "MUSIC/"+name, testfixture.WAV(true))
	}
	testfixture.Write(t, staged, "PIONEER/rekordbox/export.pdb",
		testfixture.PDBNamed([5]string{"staged-one.wav", "staged-two.wav", "staged-three.wav", "staged-four.wav", "staged-five.wav"}, "staged"))

	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true); err != nil || res.Bypassed {
		t.Fatalf("initial union+staged import: %+v %v", res, err)
	}

	readPathDerived := func() (tracks map[int64]string, playlists map[int64]string, stagedLists int) {
		db := dbOpen(t, root)
		defer db.Close()
		tracks = map[int64]string{}
		tr, err := db.Query(`SELECT id, path FROM Track WHERE pdbImportKey >= 2147483648`)
		if err != nil {
			t.Fatal(err)
		}
		for tr.Next() {
			var id int64
			var p string
			if err := tr.Scan(&id, &p); err != nil {
				t.Fatal(err)
			}
			tracks[id] = p
		}
		tr.Close()

		playlists = map[int64]string{}
		pr, err := db.Query(`SELECT p.id, p.title FROM Playlist p JOIN bridge.BridgePlaylist b ON b.listId=p.id WHERE b.sourceId >= 2147483648`)
		if err != nil {
			t.Fatal(err)
		}
		for pr.Next() {
			var id int64
			var title string
			if err := pr.Scan(&id, &title); err != nil {
				t.Fatal(err)
			}
			playlists[id] = title
		}
		pr.Close()
		stagedLists = scalar[int](t, db, `SELECT count(*) FROM Playlist WHERE title LIKE '[Pro DJ Link: CDJ-3000]%'`)
		return tracks, playlists, stagedLists
	}

	beforeTracks, beforePlaylists, beforeStagedLists := readPathDerived()
	if len(beforeTracks) == 0 {
		t.Fatal("expected path-derived local tracks (pdbImportKey >= 1<<31) before staged-only pass")
	}
	if len(beforePlaylists) == 0 {
		t.Fatal("expected path-derived local playlists (BridgePlaylist.sourceId >= 1<<31) before staged-only pass")
	}
	if beforeStagedLists == 0 {
		t.Fatal("expected staged playlists before staged-only pass")
	}

	if err := os.Rename(filepath.Join(root, "PIONEER"), filepath.Join(root, "PIONEER.saved")); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Convert(ctx, root, enginedb.Options{}); err != nil {
		t.Fatalf("staged-only convert: %v", err)
	}

	afterTracks, afterPlaylists, afterStagedLists := readPathDerived()
	if len(afterTracks) != len(beforeTracks) {
		t.Fatalf("staged-only pass changed path-derived local tracks: %d -> %d", len(beforeTracks), len(afterTracks))
	}
	for id, wantPath := range beforeTracks {
		if gotPath, ok := afterTracks[id]; !ok || gotPath != wantPath {
			t.Fatalf("path-derived track %d (%q) missing or moved after staged-only pass: got %q (%v)", id, wantPath, gotPath, ok)
		}
	}
	if len(afterPlaylists) != len(beforePlaylists) {
		t.Fatalf("staged-only pass changed path-derived local playlists: %d -> %d", len(beforePlaylists), len(afterPlaylists))
	}
	for id, wantTitle := range beforePlaylists {
		if gotTitle, ok := afterPlaylists[id]; !ok || gotTitle != wantTitle {
			t.Fatalf("path-derived playlist %d (%q) missing or changed after staged-only pass: got %q (%v)", id, wantTitle, gotTitle, ok)
		}
	}
	if afterStagedLists != beforeStagedLists {
		t.Fatalf("staged-only pass changed staged playlist count: %d -> %d", beforeStagedLists, afterStagedLists)
	}
}

// Pins M9b: quickInStage must reconcile missing secondary case spellings so TrustPaths does not
// insert phantom rows when quickUpdate defers list building to the stage.
func TestIssue327QuickInStageReconcilesMissingSecondaryCaseSpelling(t *testing.T) {
	root := t.TempDir()
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.LargePDB(4))
	for i := 1; i <= 4; i++ {
		testfixture.Write(t, root, testfixture.LargeTrackPath(i), testfixture.MP3WithLAMETag())
	}
	pdb := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(pdb, when, when); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true); err != nil || res.Added != 4 {
		t.Fatalf("initial import: %+v %v", res, err)
	}

	// Re-export adds track 5 with a secondary case spelling whose file does not exist on disk.
	data := testfixture.LargePDB(5)
	oldPath := "/" + testfixture.LargeTrackPath(5)
	newPath := strings.Replace(oldPath, "/Album", "/album", 1)
	if bytes.Count(data, qfU16(oldPath)) != 1 {
		t.Fatalf("expected one occurrence of %q in LargePDB(5)", oldPath)
	}
	data = bytes.Replace(data, qfU16(oldPath), qfU16(newPath), 1)
	if err := os.WriteFile(pdb, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdb, when.Add(time.Minute), when.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	defer bridge.SetLargeExportForTest()()
	defer bridge.SetMountWindowForTest(time.Second)()
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !res.Background {
		t.Fatalf("mount deferring quick list to stage: %+v %v", res, err)
	}
	if b, err := os.ReadFile(stagePath(root, "QUICK")); err != nil || len(b) == 0 {
		t.Fatalf("expected QUICK stage mark: %v", err)
	}
	stageRes, err := bridge.ImportInBackground(ctx, root, nil)
	if err != nil {
		t.Fatalf("quickInStage background: %v", err)
	}
	if stageRes.Added != 0 {
		t.Fatalf("quickInStage added %d phantom row(s) without reconciling missing secondary case spelling", stageRes.Added)
	}
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatalf("publish quickInStage: %v", err)
	}
	if got := scalar[int](t, dbOpen(t, root), `SELECT count(*) FROM Track`); got != 4 {
		t.Fatalf("track count after quickInStage publish=%d, want 4", got)
	}
}
