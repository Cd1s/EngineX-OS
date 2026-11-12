package rekordbox

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	_ "modernc.org/sqlite"
)

func TestAnalysisSkipsPadsIPAndDisabledCueSlots(t *testing.T) {
	// Build a PCOB hot-cue list containing:
	// - slot 1 (valid Hot Cue A at 500 ms)
	// - slot 0 disabled placeholder (p[28]=0, TimeMS=0xffffffff)
	// - slot 9 (Rekordbox Desktop Pad I at 1500 ms)
	// - slot 16 (Rekordbox Desktop Pad P loop 2000..3000 ms)
	disabledOld := make([]byte, 56)
	copy(disabledOld, "PCPT")
	binary.BigEndian.PutUint32(disabledOld[4:], 28)
	binary.BigEndian.PutUint32(disabledOld[8:], 56)
	disabledOld[28] = 0
	binary.BigEndian.PutUint32(disabledOld[32:], 0xffffffff)
	binary.BigEndian.PutUint32(disabledOld[36:], 0xffffffff)

	mkOld := func(hot, start, end uint32, typ byte) []byte {
		b := make([]byte, 56)
		copy(b, "PCPT")
		binary.BigEndian.PutUint32(b[4:], 28)
		binary.BigEndian.PutUint32(b[8:], 56)
		binary.BigEndian.PutUint32(b[12:], hot)
		b[28] = typ
		binary.BigEndian.PutUint32(b[32:], start)
		binary.BigEndian.PutUint32(b[36:], end)
		return b
	}

	pcobHot := make([]byte, 12)
	binary.BigEndian.PutUint32(pcobHot, 1) // hot cue list
	binary.BigEndian.PutUint16(pcobHot[6:], 4)
	pcobHot = append(pcobHot, disabledOld...)
	pcobHot = append(pcobHot, mkOld(1, 500, 0xffffffff, 1)...)
	pcobHot = append(pcobHot, mkOld(9, 1500, 0xffffffff, 1)...)
	pcobHot = append(pcobHot, mkOld(16, 2000, 3000, 2)...)

	dat := testfixture.PMAI(testfixture.Section("PCOB", 24, pcobHot))
	a, err := ParseAnalysis(dat)
	if err != nil {
		t.Fatalf("ParseAnalysis(DAT): %v", err)
	}

	// Extended PCO2 with disabled slot, valid slot 8, and Desktop Pad J (slot 10) + Pad P (slot 16).
	disabledExt := testfixture.CueExtended(0, 0xffffffff, 0, "", [3]byte{})
	disabledExt[16] = 0
	pco2Hot := make([]byte, 8)
	binary.BigEndian.PutUint32(pco2Hot, 1) // hot cue list
	binary.BigEndian.PutUint16(pco2Hot[4:], 4)
	pco2Hot = append(pco2Hot, disabledExt...)
	pco2Hot = append(pco2Hot, testfixture.CueExtended(8, 800, 1200, "Slot H", [3]byte{10, 20, 30})...)
	pco2Hot = append(pco2Hot, testfixture.CueExtended(10, 2500, 0, "Pad J", [3]byte{40, 50, 60})...)
	pco2Hot = append(pco2Hot, testfixture.CueExtended(16, 3000, 4000, "Pad P", [3]byte{70, 80, 90})...)

	ext, err := ParseAnalysis(testfixture.PMAI(testfixture.Section("PCO2", 20, pco2Hot)))
	if err != nil {
		t.Fatalf("ParseAnalysis(EXT): %v", err)
	}
	a.Merge(ext)

	if len(a.Cues) != 2 {
		t.Fatalf("got %d cues (%+v), want only valid slots 1 and 8", len(a.Cues), a.Cues)
	}
	if a.Cues[0].HotCue != 1 || a.Cues[0].TimeMS != 500 || a.Cues[1].HotCue != 8 || !a.Cues[1].Loop || a.Cues[1].EndMS != 1200 {
		t.Fatalf("unexpected merged cues: %+v", a.Cues)
	}

	// Library.Validate must accept a track populated with these parsed cues.
	lib := &Library{Tracks: []Track{{ID: 1, Path: "/Contents/t.mp3", Cues: a.Cues}}}
	if err := lib.Validate(); err != nil {
		t.Fatalf("Library.Validate failed after parsing ANLZ cues: %v", err)
	}
}

func TestOneLibraryAdapterStreamsProjectedColumnsWithoutMaterializingUnusedBlobs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "exportLibrary.db")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range []string{
		`CREATE TABLE artist (id INTEGER PRIMARY KEY, name TEXT, unused_bio BLOB)`,
		`CREATE TABLE content (id INTEGER PRIMARY KEY, title TEXT, artist_id INTEGER, file_path TEXT, bpmx100 INTEGER, length INTEGER, unused_waveform BLOB)`,
		`CREATE TABLE playlist (id INTEGER PRIMARY KEY, playlist_id_parent INTEGER, sequenceNo INTEGER, name TEXT, is_folder INTEGER, unused_meta BLOB)`,
		`CREATE TABLE playlist_content (playlist_id INTEGER, content_id INTEGER, sequenceNo INTEGER, unused_sync BLOB)`,
		`INSERT INTO artist VALUES (1, 'Artist One', zeroblob(1048576))`,
		`INSERT INTO playlist VALUES (10, 0, 1, 'Main Set', 0, zeroblob(1048576))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 32; i++ {
		if _, err := db.Exec(`INSERT INTO content VALUES (?, ?, 1, ?, 12800, 240, zeroblob(1048576))`,
			i, fmt.Sprintf("Track %d", i), fmt.Sprintf("/Contents/track_%d.flac", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO playlist_content VALUES (10, ?, ?, zeroblob(262144))`, i, i); err != nil {
			t.Fatal(err)
		}
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	lib, err := (OneLibraryAdapter{}).Read(context.Background(), db)
	if err != nil {
		t.Fatalf("OneLibraryAdapter.Read: %v", err)
	}
	runtime.ReadMemStats(&after)
	allocDelta := after.TotalAlloc - before.TotalAlloc

	if len(lib.Tracks) != 32 || len(lib.Playlists) != 1 || len(lib.Playlists[0].Tracks) != 32 {
		t.Fatalf("unexpected library counts: tracks=%d playlists=%+v", len(lib.Tracks), lib.Playlists)
	}
	if lib.Tracks[0].Artist != "Artist One" || lib.Tracks[0].BPM != 128 || lib.Tracks[0].Duration != 240 {
		t.Fatalf("unexpected track metadata: %+v", lib.Tracks[0])
	}
	// With SELECT * (readSQLRows), unused zeroblobs allocate >40 MiB in Go heap strings;
	// projected streaming via eachSQLRow stays well under 8 MiB even under -race.
	if allocDelta > 8<<20 {
		t.Fatalf("OneLibraryAdapter.Read allocated %d bytes (>8 MiB); unused columns were materialized via SELECT *", allocDelta)
	}
}

