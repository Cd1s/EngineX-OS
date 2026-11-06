package djay

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
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
