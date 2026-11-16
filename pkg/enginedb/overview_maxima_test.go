package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	"modernc.org/sqlite"
)

// Engine scales each overview band by the band maxima in the last three bytes. rb-bridge
// wrote zeros there, and Engine drew an imported track at a third of its own tracks' height.
func TestOverviewEndsWithBandMaxima(t *testing.T) {
	wave := rekordbox.Waveform{Tag: "PWV7", EntryBytes: 3, Data: []byte{10, 20, 30, 102, 5, 7, 4, 123, 90}}
	p, err := EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{Waveforms: []rekordbox.Waveform{wave}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := QUncompress(p.Overview, 1<<20)
	if err != nil || !bytes.Equal(raw[len(raw)-3:], []byte{102, 123, 90}) {
		t.Fatalf("trailer %v, err %v", raw[len(raw)-3:], err)
	}
}

func TestOverviewMaximaHelper(t *testing.T) {
	if overviewBatch <= 0 || overviewBatch > 256 {
		t.Fatalf("overviewBatch = %d, want in (0, 256] so slow sticks commit within mount budget", overviewBatch)
	}
	raw := make([]byte, 24+6+3)
	copy(raw[24:], []byte{10, 200, 30, 150, 50, 90, 0, 0, 0})
	got := OverviewMaxima(raw)
	if !bytes.Equal(got[len(got)-3:], []byte{150, 200, 90}) {
		t.Fatalf("trailer = %v, want [150 200 90]", got[len(got)-3:])
	}
	raw[len(raw)-1] = 1
	if got2 := OverviewMaxima(raw); !bytes.Equal(got2, raw) {
		t.Fatalf("expected non-zero trailer to remain unchanged")
	}
	zeros := make([]byte, 24+6+3)
	if got3 := OverviewMaxima(zeros); !bytes.Equal(got3, zeros) {
		t.Fatalf("expected all-zero overview to remain unchanged")
	}
}

// overviewLibrary is a volume whose library has only the table of overviews, and an
// overview of two bins that ends in trailer.
func overviewLibrary(t *testing.T) (mount string, db *sql.DB, overview func(trailer ...byte) []byte) {
	t.Helper()
	mount = t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE PerformanceData (trackId INTEGER PRIMARY KEY, overviewWaveFormData BLOB, thirdPartySourceId INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return mount, db, func(trailer ...byte) []byte {
		raw := append(make([]byte, 24), 10, 200, 3, 60, 20, 90)
		blob, err := QCompress(append(raw, trailer...))
		if err != nil {
			t.Fatal(err)
		}
		return blob
	}
}

// Real decks, 5.3.0.5 kit with the backfill: "小波形 全幅半幅还是不对", only RGB looked right.
// On the maintainer's stick every track he had played before had thirdPartySourceId NULL
// and still a zero trailer (6 of 75 on one stick, 8 of 11 on another), the one he had
// loaded that morning among them: Engine writes a loaded track's row back without the
// mark of an import, and the backfill read only the rows that still had it.
func TestFillOverviewMaximaCompletesEveryZeroTrailer(t *testing.T) {
	mount, db, overview := overviewLibrary(t)
	old, done := overview(0, 0, 0), overview(9, 9, 9)
	silent, err := QCompress(make([]byte, 24+6+3))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO PerformanceData VALUES (1,?1,1),(2,?2,1),(3,?1,NULL),(4,x'00',1),(5,?2,NULL),(6,?3,NULL),(7,NULL,NULL)`, old, done, silent); err != nil {
		t.Fatal(err)
	}
	if through, err := FillOverviewMaxima(context.Background(), mount, time.Time{}); err != nil || !through {
		t.Fatal(through, err)
	}
	get := func(id int) (blob []byte) {
		if err := db.QueryRow(`SELECT overviewWaveFormData FROM PerformanceData WHERE trackId=?`, id).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		return blob
	}
	for id, what := range map[int]string{1: "an imported track", 3: "a track Engine has loaded since"} {
		if raw, err := QUncompress(get(id), 1<<20); err != nil || !bytes.Equal(raw[len(raw)-3:], []byte{60, 200, 90}) || !bytes.Equal(raw[24:30], []byte{10, 200, 3, 60, 20, 90}) {
			t.Fatalf("the zero trailer of %s: %v %v", what, raw, err)
		}
	}
	if !bytes.Equal(get(2), done) || !bytes.Equal(get(5), done) || !bytes.Equal(get(4), []byte{0}) || !bytes.Equal(get(6), silent) || get(7) != nil {
		t.Fatal("a completed, unreadable, silent or missing overview was rewritten")
	}
}

// The mount hook kills rb-bridge when the mount's time is up, and remembers the stick as
// too slow until the next boot. The backfill was one transaction with no end in time: on
// a library too large for the mount it was rolled back whole, at every boot. It commits a
// batch at a time and stops at the deadline, and the next mount goes on from there.
func TestFillOverviewMaximaKeepsWhatItCompletedBeforeTheDeadline(t *testing.T) {
	saved := overviewBatch
	overviewBatch = 2
	defer func() { overviewBatch = saved }()
	mount, db, overview := overviewLibrary(t)
	old := overview(0, 0, 0)
	if _, err := db.Exec(`INSERT INTO PerformanceData VALUES (1,?1,1),(2,?1,1),(3,?1,1),(4,?1,1),(5,?1,1)`, old); err != nil {
		t.Fatal(err)
	}
	left := func() (n int) {
		t.Helper()
		if err := db.QueryRow(`SELECT count(*) FROM PerformanceData WHERE overviewWaveFormData=?`, old).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	deadline := time.Unix(1000, 0)
	if through, err := fillOverviewMaxima(context.Background(), mount, deadline, func() time.Time { return deadline }); err != nil || through || left() != 5 {
		t.Fatal("past the deadline:", through, err, left())
	}
	looks := 0
	clock := func() time.Time { looks++; return deadline.Add(time.Duration(looks-2) * time.Second) } // time for one batch
	if through, err := fillOverviewMaxima(context.Background(), mount, deadline, clock); err != nil || through || left() != 3 {
		t.Fatal("one batch before the deadline:", through, err, left())
	}
	if through, err := FillOverviewMaxima(context.Background(), mount, time.Time{}); err != nil || !through || left() != 0 {
		t.Fatal("the next mount:", through, err, left())
	}
}

var (
	registerSlowUpdateOnce sync.Once
	slowUpdateDelayNs      atomic.Int64
)

func ensureSlowUpdateSQLiteFunc(t *testing.T) {
	t.Helper()
	var regErr error
	registerSlowUpdateOnce.Do(func() {
		regErr = sqlite.RegisterScalarFunction("test_overview_update_delay", 0, func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			if d := time.Duration(slowUpdateDelayNs.Load()); d > 0 {
				time.Sleep(d)
			}
			return int64(1), nil
		})
	})
	if regErr != nil {
		t.Fatal(regErr)
	}
}

// Regression test for P2: with a slow write path (2 ms/row) and a mount budget shorter than
// a 2048-row transaction, 256-row batches must commit and advance the checkpoint across
// repeated mount attempts rather than rolling back with zero progress.
func TestFillOverviewMaximaSlowWriteProgressesAcrossMountBudgets(t *testing.T) {
	ensureSlowUpdateSQLiteFunc(t)
	mount, db, overview := overviewLibrary(t)
	old := overview(0, 0, 0)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO PerformanceData VALUES (?, ?, 1)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	for id := 1; id <= 2048; id++ {
		if _, err := stmt.Exec(id, old); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`CREATE TRIGGER slow_overview_update AFTER UPDATE ON PerformanceData BEGIN SELECT test_overview_update_delay(); END;`); err != nil {
		t.Fatal(err)
	}

	slowUpdateDelayNs.Store(int64(2 * time.Millisecond))
	defer slowUpdateDelayNs.Store(0)

	budget := 2500 * time.Millisecond
	ctx1, cancel1 := context.WithTimeout(context.Background(), budget)
	_, _ = FillOverviewMaxima(ctx1, mount, time.Now().Add(budget))
	cancel1()

	p1 := ReadOverviewProgress(mount)
	if p1.after < 256 {
		t.Fatalf("attempt 1 committed checkpoint=%d, want >= 256 (batch too large for mount budget?)", p1.after)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), budget)
	_, _ = FillOverviewMaxima(ctx2, mount, time.Now().Add(budget))
	cancel2()

	p2 := ReadOverviewProgress(mount)
	if p2.after <= p1.after {
		t.Fatalf("attempt 2 did not advance checkpoint: p1=%d p2=%d", p1.after, p2.after)
	}
}
