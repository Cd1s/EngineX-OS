package enginedb

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// backfillLibrary is overviewLibrary with count overviews an earlier build imported,
// rb-bridge's own database beside it, and a mount that has time for one batch of two.
func backfillLibrary(t *testing.T, count int) (mount string, state *sql.DB, left func() int, oneBatch func() bool, zero func(id int)) {
	t.Helper()
	saved := overviewBatch
	overviewBatch = 2
	t.Cleanup(func() { overviewBatch = saved })
	mount, db, overview := overviewLibrary(t)
	old := overview(0, 0, 0)
	zero = func(id int) {
		t.Helper()
		if _, err := db.Exec(`INSERT OR REPLACE INTO PerformanceData VALUES (?,?,1)`, id, old); err != nil {
			t.Fatal(err)
		}
	}
	for id := 1; id <= count; id++ {
		zero(id)
	}
	state, err := sql.Open("sqlite", filepath.Join(mount, "Engine Library", "Database2", stateFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	state.SetMaxOpenConns(1)
	if _, err := state.Exec(`CREATE TABLE BridgeState(path TEXT PRIMARY KEY, trackId INTEGER NOT NULL, fingerprint TEXT NOT NULL); INSERT INTO BridgeState VALUES('a',1,'x')`); err != nil {
		t.Fatal(err)
	}
	left = func() (n int) {
		t.Helper()
		if err := db.QueryRow(`SELECT count(*) FROM PerformanceData WHERE overviewWaveFormData=?`, old).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	oneBatch = func() bool {
		t.Helper()
		deadline, looks := time.Unix(1000, 0), 0
		through, err := fillOverviewMaxima(context.Background(), mount, deadline, func() time.Time { looks++; return deadline.Add(time.Duration(looks-2) * time.Second) })
		if err != nil {
			t.Fatal(err)
		}
		return through
	}
	return mount, state, left, oneBatch, zero
}

func anImport(t *testing.T, state *sql.DB) {
	t.Helper()
	if _, err := state.Exec(`UPDATE BridgeState SET fingerprint=fingerprint||'y'`); err != nil { // every track it encodes gets a new fingerprint
		t.Fatal(err)
	}
}

// Review of d60a683: every mount began at the first track again. Once the part already
// complete took a mount's time to read, the rest was never reached: with time for one
// batch a mount, as many overviews were left after the third mount as after the first.
// The backfill records how far it has come, and the next mount goes on from there.
func TestFillOverviewMaximaGoesOnWhereTheLastMountStopped(t *testing.T) {
	mount, state, left, oneBatch, zero := backfillLibrary(t, 5)
	for number, want := range []int{3, 1, 0} {
		if through := oneBatch(); through != (want == 0) || left() != want {
			t.Fatalf("mount %d: through %v, %d overviews left, want %d", number+1, through, left(), want)
		}
	}
	// Through it, the library is not read again,
	zero(1)
	if through := oneBatch(); !through || left() != 1 {
		t.Fatal("a library the backfill has been through was read again:", through, left())
	}
	// until another build's import, which starts it over.
	anImport(t, state)
	for number, want := range []int{0, 0, 0} {
		if through := oneBatch(); through != (number == 2) || left() != want {
			t.Fatalf("after an import, mount %d: through %v, %d left", number+1, through, left())
		}
	}
	if p := ReadOverviewProgress(mount); !p.done {
		t.Fatal("not recorded as done:", p)
	}
}

// The place is recorded once the batch is committed. Killed in between, the next mount
// reads that batch once more and changes nothing; it never skips one that was not
// committed.
func TestFillOverviewMaximaReadsABatchAgainWhenItsPlaceWasNotRecorded(t *testing.T) {
	mount, _, left, oneBatch, _ := backfillLibrary(t, 5)
	progress := filepath.Join(mount, "Engine Library", "Database2", overviewProgressFile)
	oneBatch()
	before, err := os.ReadFile(progress)
	if err != nil {
		t.Fatal(err)
	}
	if oneBatch(); left() != 1 {
		t.Fatal("fixture: the second batch left", left())
	}
	if err := os.WriteFile(progress, before, 0o644); err != nil { // as if killed after the commit
		t.Fatal(err)
	}
	if through := oneBatch(); through || left() != 1 {
		t.Fatal("the batch whose place was lost:", through, left())
	}
	if through := oneBatch(); !through || left() != 0 {
		t.Fatal("the last batch:", through, left())
	}
}

// rb-bridge reads the progress before its own import and keeps it afterwards: its import
// changes the generation and leaves no overview without band maxima. Any other import
// starts the backfill over.
func TestOverviewProgressOutlastsOnlyAnImportThatKeepsIt(t *testing.T) {
	mount, state, _, oneBatch, _ := backfillLibrary(t, 5)
	start := OverviewProgress{after: math.MinInt64}
	if p := ReadOverviewProgress(mount); p != start {
		t.Fatal("progress before any backfill:", p)
	}
	oneBatch()
	for _, want := range []OverviewProgress{{after: 2}, {done: true}} {
		if want.done {
			if through, err := FillOverviewMaxima(context.Background(), mount, time.Time{}); err != nil || !through {
				t.Fatal(through, err)
			}
		}
		before := ReadOverviewProgress(mount)
		if before != want {
			t.Fatalf("progress %+v, want %+v", before, want)
		}
		anImport(t, state)
		if p := ReadOverviewProgress(mount); p != start {
			t.Fatalf("progress %+v still stands after an import", p)
		}
		if err := before.Keep(mount); err != nil {
			t.Fatal(err)
		}
		if p := ReadOverviewProgress(mount); p != before {
			t.Fatalf("kept %+v, read %+v", before, p)
		}
	}
}

// Review of 0bf0f1e: with rb-bridge's database in WAL mode an import commits to the log
// and leaves the header as it was; and beside a journal the header is that of a write cut
// short. Neither is a generation. Without one the backfill still keeps its place from
// mount to mount, but is never done for good: it goes round the library.
func TestOverviewProgressWithoutAGenerationGoesRound(t *testing.T) {
	mount, state, left, oneBatch, zero := backfillLibrary(t, 5)
	dir := filepath.Join(mount, "Engine Library", "Database2")
	first := importGeneration(dir)
	anImport(t, state)
	second := importGeneration(dir)
	if first == "" || second == "" || first == second {
		t.Fatalf("an import left the generation as it was: %q %q", first, second)
	}
	journal := filepath.Join(dir, stateFile+"-journal")
	if err := os.WriteFile(journal, []byte("journal"), 0o644); err != nil {
		t.Fatal(err)
	}
	if g := importGeneration(dir); g != "" {
		t.Fatal("a generation beside the journal of a write cut short:", g)
	}
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := state.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatal(mode, err)
	}
	header := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, stateFile))
		if err != nil || len(b) < 100 {
			t.Fatal(len(b), err)
		}
		return string(b[:100])
	}
	before := header()
	anImport(t, state) // with the connection still open
	if header() != before {
		t.Fatal("fixture: the import changed the header, this is not the reviewed case")
	}
	if g := importGeneration(dir); g != "" {
		t.Fatal("a generation for a database whose header does not show its imports:", g)
	}
	for number, want := range []int{3, 1, 0} {
		if through := oneBatch(); through != (want == 0) || left() != want {
			t.Fatalf("mount %d: through %v, %d overviews left, want %d", number+1, through, left(), want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, overviewProgressFile)); !os.IsNotExist(err) {
		t.Fatal("a library nothing vouches for was recorded as done:", err)
	}
	zero(1)
	if through := oneBatch(); through || left() != 0 {
		t.Fatal("the next round did not begin at the first track:", through, left())
	}
}
