package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// Real stick, 2026-11-19 (6013 tracks): a deck loaded one track while the update of the
// stick's library was built. The mount that publishes the update then has to carry Engine's
// writes into it first, and compared the analysis of every track of both libraries for
// that: 190 MB read off the stick in a mount that has 20 s. The hook killed it there, at
// that mount and at every later one, and the update was never published. Engine's triggers
// date a track whenever its analysis, cues or loops are written, so only the tracks whose
// date has moved since the snapshot need the comparison: 22 MB on the same stick.

// publishHotB is where hot cue B of the track with the export's id key is, and its row.
func publishHotB(t *testing.T, db *sql.DB, key int) (id int64, cues []byte, at int) {
	t.Helper()
	var blob []byte
	if err := db.QueryRow(`SELECT t.id,p.quickCues FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, key).Scan(&id, &blob); err != nil {
		t.Fatal(err)
	}
	cues, err := enginedb.QUncompress(blob, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	return id, cues, ruleHotB(cues)
}

// publishSetB moves hot cue B of the track with the export's id key, as a deck does.
func publishSetB(t *testing.T, db *sql.DB, key int, pos float64) int64 {
	t.Helper()
	id, cues, at := publishHotB(t, db, key)
	binary.BigEndian.PutUint64(cues[at:], math.Float64bits(pos))
	blob, err := enginedb.QCompress(cues)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, blob, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// publishStick is a stick of n tracks an older build converted, every track dated date,
// whose update has been built on a snapshot and waits for the mount that publishes it. The
// track with the export's id justNow (0: none) was written in the second of the snapshot.
func publishStick(t *testing.T, n int, date int64, justNow int) string {
	t.Helper()
	root := guardStick(t, n)
	ruleOlderBuild(t, root, "v3")
	ctx := context.Background()
	live := dbOpen(t, root)
	if _, err := live.Exec(`UPDATE Track SET lastEditTime=?`, date); err != nil {
		t.Fatal(err)
	}
	live.Close()
	t.Cleanup(bridge.SetLargeExportForTest())
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("mount that starts the update: %+v %v", r, err)
	}
	if justNow != 0 {
		live = dbOpen(t, root)
		if _, err := live.Exec(`UPDATE Track SET lastEditTime=CAST(strftime('%s','now') AS INTEGER) WHERE pdbImportKey=?`, justNow); err != nil {
			t.Fatal(err)
		}
		live.Close()
	}
	if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("update: %+v %v", r, err)
	}
	return root
}

// publishMount is the mount that publishes the update, and what it wrote to the journal.
func publishMount(t *testing.T, root string) string {
	t.Helper()
	var journal bytes.Buffer
	log.SetOutput(&journal)
	defer log.SetOutput(os.Stderr)
	r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil || r.Background || !r.PlaylistsChanged {
		t.Fatalf("mount publishing the update: %+v %v\n%s", r, err, journal.String())
	}
	return journal.String()
}

func TestUpdatePublishComparesOnlyTheTracksEngineWrote(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	const n = 20
	root := publishStick(t, n, 1700000000, 7) // a library of long ago

	// While the update was built: the DJ moved hot cue B of two tracks on the deck (Engine's
	// trigger dates them), and a third was played (no date: a play is not an edit).
	live := dbOpen(t, root)
	setB := func(key int, pos float64) int64 { t.Helper(); return publishSetB(t, live, key, pos) }
	setB(3, 30000.5)
	setB(4, 30000.5)
	if got := scalar[int](t, live, `SELECT count(*) FROM Track WHERE lastEditTime<>1700000000 AND pdbImportKey<>7`); got != 2 {
		t.Fatalf("%d tracks dated by Engine's trigger, want the 2 written", got)
	}
	// A track written in the very second it already had as its date: the date cannot show
	// it, so a track dated around the snapshot is compared whatever its date says.
	sameSecond := scalar[int64](t, live, `SELECT lastEditTime FROM Track WHERE pdbImportKey=7`)
	if _, err := live.Exec(`UPDATE Track SET lastEditTime=? WHERE id=?`, sameSecond, setB(7, 50000.5)); err != nil {
		t.Fatal(err)
	}
	if _, err := live.Exec(`UPDATE Track SET isPlayed=1,timeLastPlayed=1763370000 WHERE pdbImportKey=5`); err != nil {
		t.Fatal(err)
	}
	// A row changed without its date moving: nothing Engine does looks like that, and it is
	// what shows that a track whose date has not moved is not read at all.
	undated := setB(6, 40000.5)
	if _, err := live.Exec(`UPDATE Track SET lastEditTime=1700000000 WHERE id=?`, undated); err != nil {
		t.Fatal(err)
	}
	live.Close()

	if journal := publishMount(t, root); !strings.Contains(journal, "(3 tracks Engine wrote)") {
		t.Fatalf("the publish did not compare exactly the 2 tracks Engine wrote and the one dated at the snapshot:\n%s", journal)
	}

	db := dbOpen(t, root)
	defer db.Close()
	b := func(key int) float64 {
		t.Helper()
		_, cues, at := publishHotB(t, db, key)
		return math.Float64frombits(binary.BigEndian.Uint64(cues[at:]))
	}
	if b(3) != 30000.5 || b(4) != 30000.5 {
		t.Fatalf("hot cue B the deck moved: %v and %v, want 30000.5 on both", b(3), b(4))
	}
	if b(7) != 50000.5 {
		t.Fatalf("hot cue B moved in the second of the track's own date: %v, want 50000.5", b(7))
	}
	if b(6) == 40000.5 {
		t.Fatal("a track whose date had not moved was read and carried")
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM Track WHERE pdbImportKey=5 AND isPlayed=1 AND timeLastPlayed=1763370000`); got != 1 {
		t.Fatal("the play Engine recorded during the update was lost")
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey>0 AND length(p.beatData)>0`); got != n {
		t.Fatalf("published %d of %d tracks with their grid", got, n)
	}
	if got := scalar[string](t, db, `PRAGMA integrity_check`); got != "ok" {
		t.Fatal(got)
	}
}

// A library without Engine's trigger has no dates to go by: every track is compared, as
// before, and a deck's edit is carried whatever its track's date says.
func TestUpdatePublishComparesEveryTrackOfALibraryWithoutDates(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	const n = 20
	root := publishStick(t, n, 1700000000, 0)
	live := dbOpen(t, root)
	if _, err := live.Exec(`DROP TRIGGER trigger_PerformanceData_after_update_Track_timestamp`); err != nil {
		t.Fatal(err)
	}
	publishSetB(t, live, 3, 30000.5)
	if got := scalar[int](t, live, `SELECT count(*) FROM Track WHERE lastEditTime<>1700000000`); got != 0 {
		t.Fatalf("%d tracks dated without the trigger", got)
	}
	live.Close()
	if journal := publishMount(t, root); !strings.Contains(journal, fmt.Sprintf("(%d tracks Engine wrote)", n)) {
		t.Fatalf("the publish did not compare every track:\n%s", journal)
	}
	db := dbOpen(t, root)
	defer db.Close()
	_, cues, at := publishHotB(t, db, 3)
	if got := math.Float64frombits(binary.BigEndian.Uint64(cues[at:])); got != 30000.5 {
		t.Fatalf("hot cue B the deck moved: %v, want 30000.5", got)
	}
}

// A deck without network time has a clock behind the dates of a stick's library (the stick
// was written where the time was right). Dates in the clock's future are dates like any
// other: only the track the deck wrote is compared, not all of them.
func TestUpdatePublishOnADeckWhoseClockIsBehindTheLibrary(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	const n, future = 20, int64(4102444800) // 2100-01-01
	root := publishStick(t, n, future, 0)
	live := dbOpen(t, root)
	publishSetB(t, live, 3, 30000.5)
	live.Close()
	if journal := publishMount(t, root); !strings.Contains(journal, "(1 tracks Engine wrote)") {
		t.Fatalf("the publish did not compare exactly the track Engine wrote:\n%s", journal)
	}
	db := dbOpen(t, root)
	defer db.Close()
	_, cues, at := publishHotB(t, db, 3)
	if got := math.Float64frombits(binary.BigEndian.Uint64(cues[at:])); got != 30000.5 {
		t.Fatalf("hot cue B the deck moved: %v, want 30000.5", got)
	}
}
