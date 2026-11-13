package bridge_test

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
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

func overviewMarked(root string) bool {
	_, err := os.Stat(filepath.Join(root, "Engine Library/Database2/.rb-bridge-overview"))
	return err == nil
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
