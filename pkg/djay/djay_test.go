package djay

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func createDummyAudio(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fake-audio-content"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDjayDiscoverSQLiteStandardTables(t *testing.T) {
	mount := t.TempDir()
	audio1 := filepath.Join(mount, "Music", "Track1.mp3")
	audio2 := filepath.Join(mount, "Music", "Track2.flac")
	createDummyAudio(t, audio1)
	createDummyAudio(t, audio2)

	dbDir := filepath.Join(mount, "djay Media Library")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "djay_music_library.db")

	u := url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE tracks (
		id INTEGER PRIMARY KEY,
		title TEXT,
		artist TEXT,
		album TEXT,
		bpm REAL,
		key TEXT,
		duration REAL,
		path TEXT,
		beatgrid_offset REAL
	);
	CREATE TABLE cues (
		track_id INTEGER,
		slot INTEGER,
		position_ms REAL,
		label TEXT,
		color TEXT
	);
	CREATE TABLE loops (
		track_id INTEGER,
		slot INTEGER,
		start_ms REAL,
		length_ms REAL,
		label TEXT
	);
	CREATE TABLE playlists (
		id INTEGER PRIMARY KEY,
		name TEXT
	);
	CREATE TABLE playlist_tracks (
		playlist_id INTEGER,
		track_id INTEGER,
		position INTEGER
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO tracks (id, title, artist, album, bpm, key, duration, path, beatgrid_offset) VALUES
		(1, 'Song One', 'Artist A', 'Album X', 128.0, '8A', 240.5, 'Music/Track1.mp3', 0.12),
		(2, 'Song Two', 'Artist B', 'Album Y', 124.0, '5B', 180.0, 'Music/Track2.flac', 0.05)`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO cues (track_id, slot, position_ms, label, color) VALUES
		(1, 0, 1500.0, 'Intro', '#FF0000'),
		(1, 1, 45000.0, 'Drop', '00FF00')`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO loops (track_id, slot, start_ms, length_ms, label) VALUES
		(1, 0, 60000.0, 15000.0, 'Chorus Loop')`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO playlists (id, name) VALUES (10, 'Club Hits')`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO playlist_tracks (playlist_id, track_id, position) VALUES
		(10, 2, 1),
		(10, 1, 2)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	libs, err := Discover(mount)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(libs) != 1 {
		t.Fatalf("expected 1 library, got %d", len(libs))
	}

	lib := libs[0]
	if len(lib.Tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(lib.Tracks))
	}
	if len(lib.Playlists) != 1 {
		t.Fatalf("expected 1 playlist, got %d", len(lib.Playlists))
	}

	pl := lib.Playlists[0]
	if pl.Name != "Club Hits" {
		t.Fatalf("expected playlist name 'Club Hits', got %q", pl.Name)
	}
	if len(pl.Tracks) != 2 {
		t.Fatalf("expected 2 playlist tracks, got %d", len(pl.Tracks))
	}
	// Verify order (Song Two was position 1, Song One was position 2)
	if pl.Tracks[0].Title != "Song Two" || pl.Tracks[1].Title != "Song One" {
		t.Fatalf("unexpected playlist track order: %v, %v", pl.Tracks[0].Title, pl.Tracks[1].Title)
	}

	// Verify track 1 cues and loops
	var tr1 *Track
	for i := range lib.Tracks {
		if lib.Tracks[i].ID == "1" {
			tr1 = &lib.Tracks[i]
		}
	}
	if tr1 == nil {
		t.Fatal("track 1 not found")
	}
	if tr1.BPM != 128.0 || tr1.Key != "8A" || tr1.BeatGridOffset != 0.12 {
		t.Fatalf("track 1 metadata mismatch: BPM=%.1f Key=%s Offset=%.2f", tr1.BPM, tr1.Key, tr1.BeatGridOffset)
	}
	if len(tr1.Cues) != 2 {
		t.Fatalf("expected 2 cues, got %d", len(tr1.Cues))
	}
	if tr1.Cues[0].PositionMS != 1500.0 || tr1.Cues[0].Label != "Intro" || tr1.Cues[0].ColorHex != "#FF0000" {
		t.Fatalf("unexpected cue 0: %+v", tr1.Cues[0])
	}
	if tr1.Cues[1].PositionMS != 45000.0 || tr1.Cues[1].ColorHex != "#00FF00" {
		t.Fatalf("unexpected cue 1: %+v", tr1.Cues[1])
	}
	if len(tr1.Loops) != 1 {
		t.Fatalf("expected 1 loop, got %d", len(tr1.Loops))
	}
	if tr1.Loops[0].StartMS != 60000.0 || tr1.Loops[0].LengthMS != 15000.0 || tr1.Loops[0].Label != "Chorus Loop" {
		t.Fatalf("unexpected loop 0: %+v", tr1.Loops[0])
	}
}

func TestDjayDiscoverSQLiteMediaItemsTables(t *testing.T) {
	mount := t.TempDir()
	audio := filepath.Join(mount, "Audio", "Test.mp3")
	createDummyAudio(t, audio)

	dbDir := filepath.Join(mount, "Algoriddim")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "djay_music_library.db")

	u := url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE mediaItems (
		rowid INTEGER PRIMARY KEY,
		title TEXT,
		artist TEXT,
		album TEXT,
		tempo REAL,
		tonality TEXT,
		duration_sec REAL,
		file_path TEXT,
		first_beat_sec REAL
	);
	CREATE TABLE mediaItemCuePoints (
		media_item_id INTEGER,
		index_num INTEGER,
		time_sec REAL,
		name TEXT,
		color_hex TEXT
	);
	CREATE TABLE mediaItemLoops (
		media_item_id INTEGER,
		index_num INTEGER,
		start_sec REAL,
		length_sec REAL,
		name TEXT
	);
	CREATE TABLE mediaItemLists (
		rowid INTEGER PRIMARY KEY,
		name TEXT
	);
	CREATE TABLE mediaItemListItems (
		media_item_list_id INTEGER,
		media_item_id INTEGER,
		sort_order INTEGER
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO mediaItems (rowid, title, artist, album, tempo, tonality, duration_sec, file_path, first_beat_sec) VALUES
		(101, 'Media Track', 'Media Artist', 'Media Album', 130.0, '11B', 210.0, 'Audio/Test.mp3', 0.25)`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO mediaItemCuePoints (media_item_id, index_num, time_sec, name, color_hex) VALUES
		(101, 0, 2.5, 'Start', '#0000FF')`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO mediaItemLoops (media_item_id, index_num, start_sec, length_sec, name) VALUES
		(101, 0, 10.0, 4.0, 'Build Loop')`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO mediaItemLists (rowid, name) VALUES (5, 'Warmup')`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO mediaItemListItems (media_item_list_id, media_item_id, sort_order) VALUES
		(5, 101, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	libs, err := Discover(mount)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(libs) != 1 {
		t.Fatalf("expected 1 library, got %d", len(libs))
	}
	lib := libs[0]
	if len(lib.Tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(lib.Tracks))
	}
	tr := lib.Tracks[0]
	if tr.BPM != 130.0 || tr.Key != "11B" || tr.DurationSec != 210.0 || tr.BeatGridOffset != 0.25 {
		t.Fatalf("unexpected track data: %+v", tr)
	}
	if len(tr.Cues) != 1 || tr.Cues[0].PositionMS != 2500.0 || tr.Cues[0].ColorHex != "#0000FF" {
		t.Fatalf("unexpected cues: %+v", tr.Cues)
	}
	if len(tr.Loops) != 1 || tr.Loops[0].StartMS != 10000.0 || tr.Loops[0].LengthMS != 4000.0 {
		t.Fatalf("unexpected loops: %+v", tr.Loops)
	}
	if len(lib.Playlists) != 1 || lib.Playlists[0].Name != "Warmup" || len(lib.Playlists[0].Tracks) != 1 {
		t.Fatalf("unexpected playlist: %+v", lib.Playlists)
	}
}

func TestDjayDiscoverM3U8(t *testing.T) {
	mount := t.TempDir()
	audio := filepath.Join(mount, "Music", "M3UTrack.mp3")
	createDummyAudio(t, audio)

	djayDir := filepath.Join(mount, "djay")
	if err := os.MkdirAll(djayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m3uContent := `#EXTM3U
#EXTINF:185,Djay Artist - Super Track
#EXTDJAY:BPM=126.0,KEY=4A,BEATGRID_OFFSET=0.10,CUES=0:1200:Cue1:#FF0000;1:32000:Drop:#00FF00,LOOPS=0:64000:8000:Break
../Music/M3UTrack.mp3
`
	if err := os.WriteFile(filepath.Join(djayDir, "Favorites.m3u8"), []byte(m3uContent), 0o644); err != nil {
		t.Fatal(err)
	}

	libs, err := Discover(mount)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(libs) != 1 {
		t.Fatalf("expected 1 library, got %d", len(libs))
	}
	lib := libs[0]
	if len(lib.Playlists) != 1 {
		t.Fatalf("expected 1 playlist, got %d", len(lib.Playlists))
	}
	pl := lib.Playlists[0]
	if pl.Name != "Favorites" {
		t.Fatalf("expected playlist name 'Favorites', got %q", pl.Name)
	}
	if len(pl.Tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(pl.Tracks))
	}
	tr := pl.Tracks[0]
	if tr.Title != "Super Track" || tr.Artist != "Djay Artist" || tr.BPM != 126.0 || tr.Key != "4A" || tr.DurationSec != 185.0 || tr.BeatGridOffset != 0.10 {
		t.Fatalf("unexpected track data: %+v", tr)
	}
	if len(tr.Cues) != 2 || tr.Cues[0].Label != "Cue1" || tr.Cues[0].PositionMS != 1200.0 || tr.Cues[1].ColorHex != "#00FF00" {
		t.Fatalf("unexpected cues: %+v", tr.Cues)
	}
	if len(tr.Loops) != 1 || tr.Loops[0].Label != "Break" || tr.Loops[0].StartMS != 64000.0 || tr.Loops[0].LengthMS != 8000.0 {
		t.Fatalf("unexpected loops: %+v", tr.Loops)
	}
}

func TestDjayDiscoverPathTraversalRejection(t *testing.T) {
	mount := t.TempDir()
	outsideFile := filepath.Join(t.TempDir(), "secret.mp3")
	createDummyAudio(t, outsideFile)

	// Symlink pointing outside mount
	symlinkPath := filepath.Join(mount, "Escape.mp3")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Fatal(err)
	}

	validAudio := filepath.Join(mount, "Valid.mp3")
	createDummyAudio(t, validAudio)

	dbPath := filepath.Join(mount, "djay_music_library.db")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE tracks (id INTEGER PRIMARY KEY, title TEXT, path TEXT)`); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO tracks (id, title, path) VALUES
		(1, 'Relative Escape', '../../../../etc/passwd'),
		(2, 'Absolute Escape', '/etc/passwd'),
		(3, 'Symlink Escape', 'Escape.mp3'),
		(4, 'Valid Track', 'Valid.mp3'),
		(5, 'Non Existent', 'Missing.mp3')`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	libs, err := Discover(mount)
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if len(libs) != 1 {
		t.Fatalf("expected 1 library, got %d", len(libs))
	}
	// Only track 4 (Valid Track) should survive!
	if len(libs[0].Tracks) != 1 || libs[0].Tracks[0].Title != "Valid Track" {
		t.Fatalf("expected only Valid Track, got: %+v", libs[0].Tracks)
	}
}

// MediaLibrary.db is found, and columns named after SQL keywords ("index", "order") do not break the queries.
func TestDjayMediaLibraryKeywordColumns(t *testing.T) {
	mount := t.TempDir()
	createDummyAudio(t, filepath.Join(mount, "Music", "T.mp3"))
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(mount, "MediaLibrary.db"))}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE tracks (id INTEGER PRIMARY KEY, title TEXT, path TEXT)`,
		`CREATE TABLE cues (track_id INTEGER, "index" INTEGER, position_ms REAL, label TEXT)`,
		`INSERT INTO tracks VALUES (1, 'T', 'Music/T.mp3')`,
		`INSERT INTO cues VALUES (1, 0, 500.0, 'A')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	libs, err := Discover(mount)
	if err != nil || len(libs) != 1 || len(libs[0].Tracks) != 1 {
		t.Fatalf("%v %+v", err, libs)
	}
	if c := libs[0].Tracks[0].Cues; len(c) != 1 || c[0].Label != "A" || c[0].PositionMS != 500 {
		t.Fatalf("cues: %+v", c)
	}
}

func TestParseExtDjayLabelsWithCommasAndColons(t *testing.T) {
	_, _, _, cues, loops := parseExtDjay(`BPM=120,CUES=0:100:Big: Drop, part 2:#FF0000;1:200:Plain,LOOPS=0:10:20:A:B`)
	if len(cues) != 2 || cues[0].Label != "Big: Drop, part 2" || cues[0].ColorHex != "#FF0000" || cues[1].Label != "Plain" || cues[1].ColorHex != "" {
		t.Fatalf("cues: %+v", cues)
	}
	if len(loops) != 1 || loops[0].Label != "A:B" {
		t.Fatalf("loops: %+v", loops)
	}
}

func TestDjayLongMixDurationLargeGridOffsetAndSubSecondMsCueLoop(t *testing.T) {
	mount := t.TempDir()
	createDummyAudio(t, filepath.Join(mount, "Music", "Mix.mp3"))
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(mount, "djay_music_library.db"))}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE tracks (id INTEGER PRIMARY KEY, title TEXT, path TEXT, duration REAL, beatgrid_offset REAL)`,
		`CREATE TABLE cues (track_id INTEGER, slot INTEGER, position REAL, label TEXT)`,
		`CREATE TABLE loops (track_id INTEGER, slot INTEGER, start REAL, length REAL, label TEXT)`,
		// 20-minute DJ mix (1200.0 s) with ambient intro first beat at 105.0 s:
		`INSERT INTO tracks VALUES (1, 'Mix', 'Music/Mix.mp3', 1200.0, 105.0)`,
		// Cues and loops stored in ms in unlabeled position/start/length columns, including <1000 ms:
		`INSERT INTO cues VALUES (1, 0, 250.0, 'Early'), (1, 1, 45000.0, 'Drop')`,
		`INSERT INTO loops VALUES (1, 0, 250.0, 500.0, 'HalfSecLoop')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	libs, err := Discover(mount)
	if err != nil || len(libs) != 1 || len(libs[0].Tracks) != 1 {
		t.Fatalf("Discover: %v %+v", err, libs)
	}
	tr := libs[0].Tracks[0]
	if tr.DurationSec != 1200.0 {
		t.Fatalf("DurationSec = %.3f, want 1200.0", tr.DurationSec)
	}
	if tr.BeatGridOffset != 105.0 {
		t.Fatalf("BeatGridOffset = %.3f, want 105.0", tr.BeatGridOffset)
	}
	if len(tr.Cues) != 2 || tr.Cues[0].PositionMS != 250.0 || tr.Cues[1].PositionMS != 45000.0 {
		t.Fatalf("Cues corrupted: %+v", tr.Cues)
	}
	if len(tr.Loops) != 1 || tr.Loops[0].StartMS != 250.0 || tr.Loops[0].LengthMS != 500.0 {
		t.Fatalf("Loops corrupted: %+v", tr.Loops)
	}
}

func TestDjayIntegerSecondsWithThreeHourMixAndWholeSecondCues(t *testing.T) {
	mount := t.TempDir()
	createDummyAudio(t, filepath.Join(mount, "Music", "Short.mp3"))
	createDummyAudio(t, filepath.Join(mount, "Music", "LongSet.mp3"))
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(mount, "djay_music_library.db"))}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE tracks (id INTEGER PRIMARY KEY, title TEXT, path TEXT, duration INTEGER, beatgrid_offset INTEGER)`,
		`CREATE TABLE cues (track_id INTEGER, slot INTEGER, position REAL, label TEXT)`,
		`CREATE TABLE loops (track_id INTEGER, slot INTEGER, start REAL, length REAL, label TEXT)`,
		// Integer-seconds library containing normal tracks plus a 3-hour (10800 s) recorded set:
		`INSERT INTO tracks VALUES (1, 'Club Cut', 'Music/Short.mp3', 215, 1)`,
		`INSERT INTO tracks VALUES (2, '3h Set', 'Music/LongSet.mp3', 10800, 2)`,
		// Whole-second cue/loop positions without _sec/_ms suffix: 12 s must become 12000 ms, not 12 ms.
		`INSERT INTO cues VALUES (1, 0, 12, 'Intro'), (1, 1, 64, 'Drop')`,
		`INSERT INTO loops VALUES (1, 0, 12, 8, '8s Loop')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	libs, err := Discover(mount)
	if err != nil || len(libs) != 1 || len(libs[0].Tracks) != 2 {
		t.Fatalf("Discover: %v %+v", err, libs)
	}
	var short, longSet *Track
	for i := range libs[0].Tracks {
		switch libs[0].Tracks[i].ID {
		case "1":
			short = &libs[0].Tracks[i]
		case "2":
			longSet = &libs[0].Tracks[i]
		}
	}
	if short == nil || short.DurationSec != 215 || short.BeatGridOffset != 1 {
		t.Fatalf("short track corrupted by integer 10800s mix: %+v", short)
	}
	if longSet == nil || longSet.DurationSec != 10800 || longSet.BeatGridOffset != 2 {
		t.Fatalf("long set corrupted: %+v", longSet)
	}
	if len(short.Cues) != 2 || short.Cues[0].PositionMS != 12000 || short.Cues[1].PositionMS != 64000 {
		t.Fatalf("whole-second cues became ms instead of s*1000: %+v", short.Cues)
	}
	if len(short.Loops) != 1 || short.Loops[0].StartMS != 12000 || short.Loops[0].LengthMS != 8000 {
		t.Fatalf("whole-second loops became ms instead of s*1000: %+v", short.Loops)
	}
}

func TestDjayPathHashPercentAndIndexNum(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"plain.mp3", "Track #1.mp3", "100%20Pure.mp3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(dir, "djay_music_library.db"))}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE tracks (id TEXT PRIMARY KEY, title TEXT, path TEXT, duration REAL)`,
		`CREATE TABLE cues (track_id TEXT, index_num INTEGER, position_ms REAL, label TEXT, color TEXT)`,
		`CREATE TABLE loops (track_id TEXT, index_num INTEGER, start_ms REAL, length_ms REAL, label TEXT)`,
		`INSERT INTO tracks VALUES ('1', 'Plain', 'file://` + filepath.ToSlash(filepath.Join(dir, "plain.mp3")) + `', 180)`,
		`INSERT INTO tracks VALUES ('2', 'Hash', 'file://` + strings.ReplaceAll(filepath.ToSlash(filepath.Join(dir, "Track #1.mp3")), " ", "%20") + `', 180)`,
		`INSERT INTO tracks VALUES ('3', 'LiteralPct', 'file://` + filepath.ToSlash(filepath.Join(dir, "100%20Pure.mp3")) + `', 180)`,
		`INSERT INTO cues VALUES ('1', 2, 1000, 'Drop', '#ff0000'), ('1', 5, 4000, 'Outro', '#00ff00')`,
		`INSERT INTO loops VALUES ('1', 3, 2000, 1000, 'Loop3')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	libs, err := Discover(dir)
	if err != nil || len(libs) != 1 {
		t.Fatalf("Discover: libs=%v err=%v", libs, err)
	}
	lib := libs[0]
	if len(lib.Tracks) != 3 {
		t.Fatalf("Discovered %d of 3 tracks: %+v", len(lib.Tracks), lib.Tracks)
	}
	var plain *Track
	for i := range lib.Tracks {
		if lib.Tracks[i].Title == "Plain" {
			plain = &lib.Tracks[i]
		}
	}
	if plain == nil || len(plain.Cues) != 2 || plain.Cues[0].Index != 2 || plain.Cues[1].Index != 5 {
		t.Fatalf("index_num cue slots lost: %+v", plain)
	}
	if len(plain.Loops) != 1 || plain.Loops[0].Index != 3 {
		t.Fatalf("index_num loop slot lost: %+v", plain.Loops)
	}
}

func TestDjayMixedExplicitAndUnlabeledLoopUnitColumns(t *testing.T) {
	dir := t.TempDir()
	createDummyAudio(t, filepath.Join(dir, "Music", "Track.mp3"))
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(dir, "djay_music_library.db"))}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE tracks (id INTEGER PRIMARY KEY, title TEXT, path TEXT, duration INTEGER)`,
		`CREATE TABLE loops (track_id INTEGER, slot INTEGER, start_ms REAL, length REAL, label TEXT)`,
		// Track is 180 s; start_ms is explicitly in ms (12000 ms) while unlabeled length is in seconds (8 s and 30.5 s).
		`INSERT INTO tracks VALUES (1, 'MixedLoopUnits', 'Music/Track.mp3', 180)`,
		`INSERT INTO loops VALUES (1, 0, 12000, 8, '8s Loop'), (1, 1, 12000, 30.5, '30.5s Loop')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	libs, err := Discover(dir)
	if err != nil || len(libs) != 1 || len(libs[0].Tracks) != 1 {
		t.Fatalf("Discover: libs=%v err=%v", libs, err)
	}
	loops := libs[0].Tracks[0].Loops
	if len(loops) != 2 || loops[0].StartMS != 12000 || loops[0].LengthMS != 8000 || loops[1].StartMS != 12000 || loops[1].LengthMS != 30500 {
		t.Fatalf("mixed loop columns misread: got %+v, want [12000/8000, 12000/30500]", loops)
	}
}
