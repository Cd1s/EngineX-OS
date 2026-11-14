package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
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
