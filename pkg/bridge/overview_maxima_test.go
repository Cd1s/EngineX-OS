package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// oldOverviews imports count tracks and then zeroes the band maxima that end their
// overviews, as an earlier build left them. It returns the overviews Engine expects.
func oldOverviews(t *testing.T, count int) (string, *sql.DB, map[int64][]byte) {
	t.Helper()
	root := t.TempDir()
	testfixture.LargeExport(t, root, count)
	issue233Mount(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	rows, err := db.Query(`SELECT trackId, overviewWaveFormData FROM PerformanceData WHERE thirdPartySourceId=1`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64][]byte{}
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			t.Fatal(err)
		}
		raw, err := enginedb.QUncompress(blob, 1<<20)
		if err != nil || bytes.Equal(raw[len(raw)-3:], []byte{0, 0, 0}) {
			t.Fatal("fixture overview has no band maxima", err)
		}
		want[id] = raw
	}
	if err := rows.Close(); err != nil || len(want) != count {
		t.Fatal("fixture tracks", len(want), err)
	}
	zeroMaxima(t, db, want)
	return root, db, want
}

func zeroMaxima(t *testing.T, db *sql.DB, want map[int64][]byte) {
	t.Helper()
	for id, raw := range want {
		old := bytes.Clone(raw)
		clear(old[len(old)-3:])
		blob, _ := enginedb.QCompress(old)
		if _, err := db.Exec(`UPDATE PerformanceData SET overviewWaveFormData=? WHERE trackId=?`, blob, id); err != nil {
			t.Fatal(err)
		}
	}
}

// completed reports whether every overview in table has its band maxima.
func completed(t *testing.T, db *sql.DB, table string, want map[int64][]byte) bool {
	t.Helper()
	for id, expected := range want {
		var blob []byte
		if err := db.QueryRow(`SELECT overviewWaveFormData FROM `+table+` WHERE trackId=?`, id).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		raw, err := enginedb.QUncompress(blob, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, expected) {
			return false
		}
	}
	return true
}

// overviewMarked reports whether the backfill is recorded as through the library: the
// generation of its imports alone, with no track to go on from.
func overviewMarked(root string) bool {
	b, err := os.ReadFile(filepath.Join(root, "Engine Library/Database2/.rb-bridge-overview"))
	return err == nil && len(strings.Fields(string(b))) == 1
}

// recordPlace writes the backfill's progress as a mount cut short after track leaves it.
func recordPlace(t *testing.T, root string, track int64) {
	t.Helper()
	dir := filepath.Join(root, "Engine Library/Database2")
	state, err := os.ReadFile(filepath.Join(dir, ".rb-bridge-state.db"))
	if err != nil || len(state) < 100 {
		t.Fatal(len(state), err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-overview"), []byte("2:"+hex.EncodeToString(state[:100])+" "+strconv.FormatInt(track, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A stick an earlier build imported has overviews without the band maxima Engine scales
// by. The stamp bypass completes them, once.
func TestStampBypassCompletesOldOverviewsOnce(t *testing.T) {
	root, db, want := oldOverviews(t, 1)
	if r := issue233Mount(t, root, enginedb.Options{}); !r.Bypassed || !completed(t, db, "PerformanceData", want) || !overviewMarked(root) {
		t.Fatalf("old overview not completed on the bypass: %+v", r)
	}
	zeroMaxima(t, db, want)
	if issue233Mount(t, root, enginedb.Options{}); completed(t, db, "PerformanceData", want) {
		t.Fatal("the library was scanned again")
	}
}

// Review of a2cfaa3: after this build completed a library, an earlier build on another
// deck imports into it again and leaves overviews without band maxima. The mark of a
// completed library was final, so they stayed a third of the height. That import wrote
// rb-bridge's own database: the mark no longer stands, and the next insert completes them.
func TestImportByAnEarlierBuildIsCompletedAtTheNextInsert(t *testing.T) {
	root, db, want := oldOverviews(t, 2)
	if issue233Mount(t, root, enginedb.Options{}); !completed(t, db, "PerformanceData", want) || !overviewMarked(root) {
		t.Fatal("the library was not completed and marked")
	}
	zeroMaxima(t, db, want)
	if _, err := db.Exec(`UPDATE bridge.BridgeState SET fingerprint=fingerprint||'0'`); err != nil { // as it encodes them again
		t.Fatal(err)
	}
	if r := issue233Mount(t, root, enginedb.Options{}); !r.Bypassed || !completed(t, db, "PerformanceData", want) {
		t.Fatalf("the earlier build's overviews were not completed: %+v", r)
	}
	zeroMaxima(t, db, want)
	if issue233Mount(t, root, enginedb.Options{}); completed(t, db, "PerformanceData", want) {
		t.Fatal("the library was scanned once more with no import in between")
	}
}

// This build's own import of a changed export writes rb-bridge's database too. A library
// completed before it is still complete after it, and is not read through again: on a
// large library that holds the mount up.
func TestOwnImportDoesNotScanACompletedLibraryAgain(t *testing.T) {
	root, db, want := oldOverviews(t, 1)
	if issue233Mount(t, root, enginedb.Options{}); !completed(t, db, "PerformanceData", want) || !overviewMarked(root) {
		t.Fatal("the library was not completed and marked")
	}
	testfixture.LargeExport(t, root, 2)
	if r := issue233Mount(t, root, enginedb.Options{}); r.Added != 1 || r.Deferred != 0 {
		t.Fatalf("second import: %+v", r)
	}
	zeroMaxima(t, db, want)
	if r := issue233Mount(t, root, enginedb.Options{}); !r.Bypassed || completed(t, db, "PerformanceData", want) {
		t.Fatalf("the library was scanned again after this build's own import: %+v", r)
	}
}

// A scan the mount's time cut short leaves the library unmarked, and the next insert goes
// on with it.
func TestOverviewBackfillCutShortGoesOnAtTheNextInsert(t *testing.T) {
	root, db, want := oldOverviews(t, 2)
	restore := bridge.SlowExportReadForTest(0, time.Nanosecond)
	r := issue233Mount(t, root, enginedb.Options{})
	restore()
	if !r.Bypassed || completed(t, db, "PerformanceData", want) || overviewMarked(root) {
		t.Fatalf("fixture: the mount had time for the overviews: %+v", r)
	}
	if issue233Mount(t, root, enginedb.Options{}); !completed(t, db, "PerformanceData", want) || !overviewMarked(root) {
		t.Fatal("the next insert did not complete the library")
	}
}

// Review of 9527001: a library with tracks still waiting for their files takes the resume
// path, not the bypass. The tracks an earlier build had finished kept their zero maxima
// until the stick was inserted once more.
func TestResumedImportCompletesOldOverviews(t *testing.T) {
	root, db, want := oldOverviews(t, 2)
	if _, err := db.Exec(`UPDATE bridge.BridgeState SET fingerprint='fast:'||fingerprint WHERE trackId=(SELECT max(trackId) FROM bridge.BridgeState)`); err != nil {
		t.Fatal(err)
	}
	if r := issue233Mount(t, root, enginedb.Options{}); r.Updated != 1 || r.Unchanged != 1 || r.Deferred != 0 {
		t.Fatalf("resume fixture: %+v", r)
	}
	if !completed(t, db, "PerformanceData", want) || !overviewMarked(root) {
		t.Fatal("the resumed import left an old overview without its band maxima")
	}
}

// Review of 3a88aeb: a read error that ended the scan early was ignored, so part of the
// library was completed, marked as done and never finished.
func TestOverviewBackfillStopsOnReadError(t *testing.T) {
	root, db, want := oldOverviews(t, 2)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// Reading the second row's overview fails; the first row reads fine.
	exec(`ALTER TABLE PerformanceData RENAME TO RealPerformanceData;
CREATE VIEW PerformanceData AS SELECT trackId, thirdPartySourceId, CASE WHEN trackId=(SELECT max(trackId) FROM RealPerformanceData) THEN json_extract('invalid json', '$') ELSE overviewWaveFormData END AS overviewWaveFormData FROM RealPerformanceData;
CREATE TRIGGER ViewUpdate INSTEAD OF UPDATE ON PerformanceData BEGIN UPDATE RealPerformanceData SET overviewWaveFormData=NEW.overviewWaveFormData WHERE trackId=NEW.trackId; END;`)
	issue233Mount(t, root, enginedb.Options{})
	for id := range want {
		if completed(t, db, "RealPerformanceData", map[int64][]byte{id: want[id]}) {
			t.Errorf("track %d was completed although the scan failed", id)
		}
	}
	if overviewMarked(root) {
		t.Error("a failed scan was marked as done")
	}
	exec(`DROP VIEW PerformanceData; ALTER TABLE RealPerformanceData RENAME TO PerformanceData`)
	if issue233Mount(t, root, enginedb.Options{}); !completed(t, db, "PerformanceData", want) || !overviewMarked(root) {
		t.Fatal("the next insert did not complete the library")
	}
}

// Review of d60a683: every mount read the library from its first track, so one whose
// finished part took the mount's time never got further. A mount goes on after the track
// the last one recorded,
func TestOverviewBackfillGoesOnAfterTheRecordedTrack(t *testing.T) {
	root, db, want := oldOverviews(t, 2)
	first, second := int64(math.MaxInt64), int64(0)
	for id := range want {
		first, second = min(first, id), max(second, id)
	}
	recordPlace(t, root, first)
	if r := issue233Mount(t, root, enginedb.Options{}); !r.Bypassed || !overviewMarked(root) {
		t.Fatalf("the backfill did not finish: %+v", r)
	}
	if !completed(t, db, "PerformanceData", map[int64][]byte{second: want[second]}) || completed(t, db, "PerformanceData", map[int64][]byte{first: want[first]}) {
		t.Fatal("the mount did not go on after the recorded track")
	}
}

// and this build's own import in between keeps the place, also on a mount that has no
// time left for the backfill itself.
func TestOwnImportKeepsThePlaceOfTheBackfill(t *testing.T) {
	root, db, want := oldOverviews(t, 2)
	first, second := int64(math.MaxInt64), int64(0)
	for id := range want {
		first, second = min(first, id), max(second, id)
	}
	recordPlace(t, root, first)
	testfixture.LargeExport(t, root, 3)
	restore := bridge.SlowExportReadForTest(0, time.Nanosecond)
	t.Cleanup(restore)
	r := issue233Mount(t, root, enginedb.Options{})
	restore()
	// #264 starts the budget after reconciliation; its computed deadline must
	// also reach the backfill after the import has consumed that budget.
	if r.Bypassed || r.Deferred == 0 || completed(t, db, "PerformanceData", map[int64][]byte{second: want[second]}) {
		t.Fatalf("fixture: the mount had no import or had time for the backfill: %+v", r)
	}
	for r.Deferred > 0 { // the mounts that read the new track's files
		r = issue233Mount(t, root, enginedb.Options{})
	}
	issue233Mount(t, root, enginedb.Options{})
	if !overviewMarked(root) || !completed(t, db, "PerformanceData", map[int64][]byte{second: want[second]}) {
		t.Fatal("the backfill did not finish after the import")
	}
	if completed(t, db, "PerformanceData", map[int64][]byte{first: want[first]}) {
		t.Fatal("the backfill began again at the first track after this build's own import")
	}
}

// Review of 6a23453: beside a journal there is no generation, and a place recorded then
// says nothing about an import behind it. This build's own import removed the journal and
// kept that place under the new generation: the backfill went on from it and marked the
// library as done, with an earlier build's overview before the place still without its
// band maxima. (The reviewer's test, with SQLite's own persistent journal.)
func TestOwnImportDoesNotVouchForAPlaceRecordedWithoutAGeneration(t *testing.T) {
	root, db, want := oldOverviews(t, 2)
	first := int64(math.MaxInt64)
	for id := range want {
		first = min(first, id)
	}
	var old []byte
	if err := db.QueryRow(`SELECT overviewWaveFormData FROM PerformanceData WHERE trackId=?`, first).Scan(&old); err != nil {
		t.Fatal(err)
	}
	fixed, err := enginedb.QCompress(want[first])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET overviewWaveFormData=? WHERE trackId=?`, fixed, first); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA bridge.journal_mode=PERSIST`).Scan(&mode); err != nil || mode != "persist" {
		t.Fatal(mode, err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO bridge.BridgeLibrary(key,value) VALUES('source','{}')`); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "Engine Library", "Database2")
	journal := filepath.Join(dir, ".rb-bridge-state.db-journal")
	if _, err := os.Stat(journal); err != nil {
		t.Fatal("fixture has no persistent journal:", err)
	}
	progress := filepath.Join(dir, ".rb-bridge-overview")
	if err := os.WriteFile(progress, []byte("- "+strconv.FormatInt(first, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The place was right when it was recorded. An earlier build now replaces that
	// overview and writes its import's metadata in the same transaction.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE PerformanceData SET overviewWaveFormData=? WHERE trackId=?`, old, first); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO bridge.BridgeLibrary(key,value) VALUES('source','{"foreign":true}')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatal("fixture: the earlier build's import left no persistent journal:", err)
	}
	testfixture.LargeExport(t, root, 3)
	restore := bridge.SlowExportReadForTest(0, time.Nanosecond)
	t.Cleanup(restore)
	r := issue233Mount(t, root, enginedb.Options{})
	restore()
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal("fixture: this build's import did not remove the journal:", err)
	}
	for attempt := 0; r.Deferred > 0 && attempt < 5; attempt++ {
		r = issue233Mount(t, root, enginedb.Options{})
	}
	issue233Mount(t, root, enginedb.Options{})
	mark, err := os.ReadFile(progress)
	if err != nil {
		t.Fatal(err)
	}
	if !completed(t, db, "PerformanceData", map[int64][]byte{first: want[first]}) {
		t.Fatalf("the place recorded without a generation was kept (%q): the earlier build's overview before it is still without its band maxima", mark)
	}
}

// Real decks: a stick the backfill had recorded as done still had the tracks the
// maintainer plays without their band maxima. Engine had loaded them before, and it
// writes a loaded track's row back without the mark of an import; the backfill read only
// the rows with that mark. It reads every overview, and what the narrower scan recorded
// (the generation alone, with no "2:" before it) does not count as done.
func TestBackfillCompletesTheTracksEngineHasLoaded(t *testing.T) {
	root, db, want := oldOverviews(t, 3)
	loaded := int64(math.MaxInt64)
	for id := range want {
		loaded = min(loaded, id)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET thirdPartySourceId=NULL WHERE trackId=?`, loaded); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "Engine Library/Database2")
	state, err := os.ReadFile(filepath.Join(dir, ".rb-bridge-state.db"))
	if err != nil || len(state) < 100 {
		t.Fatal(len(state), err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-overview"), []byte(hex.EncodeToString(state[:100])+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := issue233Mount(t, root, enginedb.Options{}); !r.Bypassed || !overviewMarked(root) {
		t.Fatalf("the mount: %+v", r)
	}
	if !completed(t, db, "PerformanceData", want) {
		t.Fatal("a track Engine has loaded kept its small overview")
	}
	var mark sql.NullInt64
	if err := db.QueryRow(`SELECT thirdPartySourceId FROM PerformanceData WHERE trackId=?`, loaded).Scan(&mark); err != nil || mark.Valid {
		t.Fatal("the row's mark was changed:", mark, err)
	}
}

// Real deck: an SD card that also carried two staged exports (15 000 files). Its import was
// killed at every boot when the mount's time was up, the backfill came after the import,
// and every track on the card kept its small overview. The overviews no longer wait for
// the import: here the export has changed and cannot be read, so the import ends early.
func TestOverviewsDoNotWaitForAnImportThatDoesNotFinish(t *testing.T) {
	root, db, want := oldOverviews(t, 2)
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", []byte("an export that is not one"))
	if r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err == nil {
		t.Fatalf("fixture: the import went through: %+v", r)
	}
	if !completed(t, db, "PerformanceData", want) {
		t.Fatal("the overviews waited for the import")
	}
}

// Real deck, an SC5000 with an SD card that also carries the staged exports of two
// Pioneer players: reading the export with those outlasted the 17 s the mount hook gives
// rb-bridge, at every mount, and the overviews had been moved behind that read. 10 of the
// card's 11 tracks kept their small Full and Half overview; the build with the overviews
// first had them complete 0.6 s after the same mount. The test above did not show it: its
// export fails at once, and a fast-failing read is not a slow read.
func TestOverviewsDoNotWaitForAnExportThatIsStillBeingRead(t *testing.T) {
	root, db, want := oldOverviews(t, 2)
	later := time.Now().Add(time.Minute) // written again since the import: this mount reads it
	if err := os.Chtimes(filepath.Join(root, "PIONEER/rekordbox/export.pdb"), later, later); err != nil {
		t.Fatal(err)
	}
	reading, release, restore := bridge.HoldExportReadForTest()
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err = bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	}()
	t.Cleanup(func() { release(); <-done; restore() })
	select {
	case <-reading:
	case <-done:
		t.Fatalf("fixture: the export was not read: %v", err)
	}
	// rb-bridge is in the read, where the hook's kill found it on the card.
	if !completed(t, db, "PerformanceData", want) || !overviewMarked(root) {
		t.Fatal("the overviews waited for the export to be read")
	}
	release()
	<-done
	if err != nil {
		t.Fatal(err)
	}
}
