package bridge_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #321: the cue write-back reads only the rows whose Track.lastEditTime moved since the stick
// last held them. Engine's trigger dates every write of a row's analysis, cues and loops.

func issue321Stick(t *testing.T, n int) (string, *sql.DB) {
	t.Helper()
	stick := t.TempDir()
	testfixture.LargeExport(t, stick, n)
	if _, err := bridge.Convert(context.Background(), stick, enginedb.Options{}); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(stick, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	// Engine opens the library well after the import committed (a pass trusts a date from
	// the second second after it on).
	issue321Settle()
	return stick, db
}

// issue321Settle waits until a date written now is two seconds old, as Engine's last write is
// when the watcher reads m.db (CueQuiet after it).
func issue321Settle() {
	for s := time.Now().Unix(); time.Now().Unix() < s+2; {
		time.Sleep(20 * time.Millisecond)
	}
}

// issue321Bloat gives every row a 256 KiB overview, as Engine's analysis is, without dating it
// (the trigger is dropped and put back): the rows stay untouched, only reading them costs.
func issue321Bloat(t *testing.T, db *sql.DB) {
	t.Helper()
	var trig string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trigger_PerformanceData_after_update_Track_timestamp'`).Scan(&trig); err != nil {
		t.Fatalf("Engine's PerformanceData trigger missing from the bridge-built library: %v", err)
	}
	for _, q := range []string{`DROP TRIGGER trigger_PerformanceData_after_update_Track_timestamp`,
		`UPDATE PerformanceData SET overviewWaveFormData=randomblob(262144)`, trig} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func issue321TrackID(t *testing.T, db *sql.DB, key int) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM Track WHERE pdbImportKey=?`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// issue321Edit is a deck's edit of track key: hot cue A named name at ms (as injectDeckCueEdit
// writes it, with a 4 s trackData so the position is inside the track). Engine's trigger dates it.
func issue321Edit(t *testing.T, db *sql.DB, key int, name string, ms uint32) {
	t.Helper()
	perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{
		Beats: []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}},
		Cues:  []rekordbox.Cue{{HotCue: 1, TimeMS: ms, Name: name, Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=?, loops=? WHERE trackId=?`, perf.TrackData, perf.QuickCues, perf.Loops, issue321TrackID(t, db, key)); err != nil {
		t.Fatal(err)
	}
}

// issue321ReadBytes is what this process has read through read/pread so far (rchar): SQLite's
// page reads, whatever the file system caches.
func issue321ReadBytes(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Skip("no /proc/self/io")
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "rchar: "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	t.Skip("no rchar")
	return 0
}

func issue321Pass(t *testing.T, stick string, opts bridge.WriteBackOptions) (bridge.WriteBackResult, int64) {
	t.Helper()
	opts.SkipPreparingCheck = true
	opts.DateDir = "2026-11-22"
	before := issue321ReadBytes(t)
	res, err := bridge.WriteBackCues(context.Background(), stick, opts)
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	return res, issue321ReadBytes(t) - before
}

func issue321ANLZHas(t *testing.T, stick, name string) bool {
	t.Helper()
	a, err := rekordbox.ReadAnalysis(filepath.Join(stick, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range a.Cues {
		if c.Name == name {
			return true
		}
	}
	return false
}

// issue321Read is the result's tracksRead (-1 when a build has no such count).
func issue321Read(res bridge.WriteBackResult) int {
	b, _ := json.Marshal(res)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if v, ok := m["tracksRead"].(float64); ok {
		return int(v)
	}
	return -1
}

func TestIssue321UntouchedTracksAreNotRead(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	const n = 40
	stick, db := issue321Stick(t, n)
	issue321Bloat(t, db)
	res, read := issue321Pass(t, stick, bridge.WriteBackOptions{Keys: testKeys})
	if res.TracksChecked != n || res.TracksChanged != 0 {
		t.Fatalf("untouched stick: %+v", res)
	}
	// 40 rows of 256 KiB are 10 MiB of overflow pages; Track and the state are well under 2 MiB.
	if read > 2<<20 {
		t.Fatalf("a pass over %d untouched tracks read %d bytes: it read their PerformanceData rows", n, read)
	}
	if issue321Read(res) != 0 {
		t.Fatalf("tracksRead=%d, want 0", issue321Read(res))
	}
}

func TestIssue321EditIsFoundAndOnlyItIsRead(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	const n = 40
	stick, db := issue321Stick(t, n)
	issue321Bloat(t, db)
	issue321Edit(t, db, 7, "Deck 321", 1000)
	res, read := issue321Pass(t, stick, bridge.WriteBackOptions{Keys: testKeys})
	if res.TracksChanged != 1 || issue321Read(res) != 1 || !issue321ANLZHas(t, stick, "Deck 321") {
		t.Fatalf("edit of track 7 not written back: %+v", res)
	}
	if read > 3<<20 {
		t.Fatalf("one edited track: the pass read %d bytes", read)
	}
	// Written: the next pass writes nothing (it reads at most the edited row, which the pass
	// that wrote it read in the second of the edit), and a later edit of the track is found.
	if res, _ = issue321Pass(t, stick, bridge.WriteBackOptions{Keys: testKeys}); issue321Read(res) > 1 || res.TracksChanged != 0 {
		t.Fatalf("second pass after the write: %+v", res)
	}
	issue321Settle()
	issue321Edit(t, db, 7, "Deck 321 again", 2000)
	if res, _ = issue321Pass(t, stick, bridge.WriteBackOptions{Keys: testKeys}); res.TracksChanged != 1 || !issue321ANLZHas(t, stick, "Deck 321 again") {
		t.Fatalf("second edit of track 7 not written back: %+v", res)
	}
}

// A track edited before a pull is found at the next pass: nothing is recorded until the stick
// holds the edit, neither when the stick went before any pass nor when it went in the middle.
func TestIssue321EditBeforeAPullIsFoundAtTheNextPass(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	stick, db := issue321Stick(t, 12)
	issue321Edit(t, db, 3, "Before pull", 1500)
	pulled := errors.New("stick pulled")
	res, err := bridge.WriteBackCues(context.Background(), stick, bridge.WriteBackOptions{Keys: testKeys, SkipPreparingCheck: true, DateDir: "2026-11-22",
		BeforeRename: func(string) error { return pulled }})
	if !errors.Is(err, pulled) || issue321ANLZHas(t, stick, "Before pull") {
		t.Fatalf("cut pass: %+v %v", res, err)
	}
	// The next mount: a new caller, no session.
	if res, _ = issue321Pass(t, stick, bridge.WriteBackOptions{Keys: testKeys}); res.TracksChanged != 1 || !issue321ANLZHas(t, stick, "Before pull") {
		t.Fatalf("edit made before the pull was not found: %+v", res)
	}
}
