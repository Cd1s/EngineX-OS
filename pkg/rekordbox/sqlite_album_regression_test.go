package rekordbox

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOneLibraryAlbumLookupUsesItsOwnPrimaryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exportLibrary.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE artist(artist_id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO artist VALUES(1, 'Artist');
		CREATE TABLE album(album_id INTEGER PRIMARY KEY, name TEXT, artist_id INTEGER);
		INSERT INTO album VALUES(10, 'Album', 1);
		CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT, artist_id_artist INTEGER, album_id INTEGER);
		INSERT INTO content VALUES(7, 'Track', '/Contents/track.mp3', 1, 10);
	`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	lib, err := ReadSQLite(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Tracks) != 1 || lib.Tracks[0].Album != "Album" {
		t.Fatalf("album lookup used a foreign key: %+v", lib.Tracks)
	}
}

func legacyEachSQLRow(ctx context.Context, db *sql.DB, table string, wanted []string, fn func(sqlRecord)) error {
	want := map[string]bool{}
	for _, name := range wanted {
		want[normalized(name)] = true
	}
	quote := func(s string) string { return `"` + s + `"` }
	info, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	var cols, keys []string
	for info.Next() {
		var name string
		if err := info.Scan(&name); err != nil {
			info.Close()
			return err
		}
		if want[normalized(name)] {
			cols = append(cols, quote(name))
			keys = append(keys, normalized(name))
		}
	}
	if err := info.Close(); err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+strings.Join(cols, ",")+` FROM `+quote(table))
	if err != nil {
		return err
	}
	defer rows.Close()
	values := make([]sql.RawBytes, len(cols))
	args := make([]any, len(values))
	for i := range args {
		args[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(args...); err != nil {
			return err
		}
		r := sqlRecord{}
		for i, value := range values {
			if value != nil {
				r[keys[i]] = string(value)
			}
		}
		fn(r)
	}
	return rows.Err()
}

func legacyReadOneLibrary(ctx context.Context, db *sql.DB) (*Library, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, err
	}
	tables := map[string]string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		tables[normalized(name)] = name
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	find := func(names ...string) string {
		for _, name := range names {
			if table := tables[normalized(name)]; table != "" {
				return table
			}
		}
		return ""
	}
	lib := &Library{Artwork: map[uint32]string{}, Keys: map[uint32]string{}}
	lookups := map[string]map[uint32]string{}
	for _, kind := range []string{"artist", "album", "genre", "label", "key", "artwork", "image"} {
		values := map[uint32]string{}
		if table := find(kind, "djmd"+kind); table != "" {
			if err := legacyEachSQLRow(ctx, db, table, []string{"id", kind + "_id", "name", "scale_name", "title", "path", "file_path"}, func(row sqlRecord) {
				values[row.num("id", kind+"_id")] = row.get("name", "scale_name", "title", "path", "file_path")
			}); err != nil {
				return nil, err
			}
		}
		lookups[kind] = values
	}
	lib.Keys, lib.Artwork = lookups["key"], lookups["artwork"]
	if len(lib.Artwork) == 0 {
		lib.Artwork = lookups["image"]
	}
	content := find("content", "djmdContent", "track", "tracks")
	if content == "" {
		return nil, fmt.Errorf("missing content")
	}
	contentColumns := []string{"id", "content_id", "track_id", "file_path", "path", "location", "file_name", "file_name_l", "filename", "folder_path", "bpmx100", "bpm", "tempo", "key_name", "key_id", "key", "image_id", "artwork_id", "image_path", "file_size", "analysisDataFilePath", "analysis_data_path", "analyze_path", "analysis_path", "title", "name", "artist_id_artist", "artist_id", "artist", "artist_id_composer", "composer_id", "artist_id_remixer", "remixer_id", "album_id", "album", "genre_id", "label_id", "length", "duration", "samplingRate", "sample_rate", "bit_depth", "bit_rate", "bitrate", "release_year", "year", "rating", "djComment", "commnt", "comment"}
	if err := legacyEachSQLRow(ctx, db, content, contentColumns, func(row sqlRecord) {
		path := row.get("file_path", "path", "location")
		if path == "" {
			if filename, folder := row.get("file_name", "file_name_l", "filename"), row.get("folder_path"); filename != "" && folder != "" {
				path = strings.TrimRight(folder, "/") + "/" + filename
			}
		}
		key := row.get("key_name")
		if key == "" {
			key = lookups["key"][row.num("key_id")]
			if key == "" {
				key = row.get("key")
			}
		}
		art := lookups["image"][row.num("image_id")]
		if art == "" {
			art = lib.Artwork[row.num("image_id", "artwork_id")]
		}
		var fileSize int64
		if size, ok := row.sint("file_size"); ok && size > 0 {
			fileSize = size
		}
		t := Track{ID: row.num("id", "content_id", "track_id"), Path: path, AnalysisPath: row.get("analysisDataFilePath", "analysis_data_path", "analyze_path", "analysis_path"), Title: row.get("title", "name"), Artist: lookups["artist"][row.num("artist_id_artist", "artist_id")], Composer: lookups["artist"][row.num("artist_id_composer", "composer_id")], Remixer: lookups["artist"][row.num("artist_id_remixer", "remixer_id")], Album: lookups["album"][row.num("album_id")], Genre: lookups["genre"][row.num("genre_id")], Label: lookups["label"][row.num("label_id")], Key: key, BPM: parseSQLiteBPM(row), Duration: row.decimal("length", "duration"), SampleRate: int(row.num("samplingRate", "sample_rate")), BitDepth: int(row.num("bit_depth")), Bitrate: int(row.num("bit_rate", "bitrate")), FileSize: fileSize, Year: int(row.num("release_year", "year")), Rating: int(row.num("rating")), Comment: row.get("djComment", "commnt", "comment"), ArtworkPath: art}
		if t.Artist == "" {
			t.Artist = row.get("artist")
		}
		if t.Album == "" {
			t.Album = row.get("album")
		}
		normalizeParsedAudioMetadata(&t)
		lib.Tracks = append(lib.Tracks, t)
	}); err != nil {
		return nil, err
	}
	if table := find("cue", "djmdCue"); table != "" {
		byContent := map[uint32][]Cue{}
		if err := legacyEachSQLRow(ctx, db, table, []string{"content_id", "cue_id", "id", "kind", "inUsec", "in_usec", "outUsec", "out_usec", "inMsec", "in_msec", "outMsec", "out_msec", "isActiveLoop", "is_active_loop", "active_loop", "beatLoopNumerator", "beat_loop_numerator", "beatLoopDenominator", "beat_loop_denominator", "cueComment", "cue_comment", "comment", "colorTableIndex", "color_table_index"}, func(row sqlRecord) {
			cue, ok := parseSQLiteCue(row)
			if ok {
				byContent[row.num("content_id")] = append(byContent[row.num("content_id")], cue)
			}
		}); err != nil {
			return nil, err
		}
		for i := range lib.Tracks {
			var merged Analysis
			for _, cue := range byContent[lib.Tracks[i].ID] {
				merged.mergeCue(cue)
			}
			lib.Tracks[i].Cues = merged.Cues
		}
	}
	for i := range lib.Tracks {
		lib.Tracks[i].RecordHash = RecordHash(lib.Tracks[i])
	}
	if table := find("playlist", "djmdPlaylist"); table != "" {
		if err := legacyEachSQLRow(ctx, db, table, []string{"id", "playlist_id", "playlist_id_parent", "parent_id", "sequenceNo", "seq", "sort_order", "name", "title", "attribute", "is_folder", "is_smart", "intelligent", "smart_list", "rules"}, func(row sqlRecord) {
			attr := row.num("attribute")
			lib.Playlists = append(lib.Playlists, Playlist{ID: row.num("id", "playlist_id"), ParentID: row.num("playlist_id_parent", "parent_id"), SortOrder: row.num("sequenceNo", "seq", "sort_order"), Name: row.get("name", "title"), Folder: row.num("is_folder") != 0 || attr == 1, Intelligent: row.num("is_smart", "intelligent") != 0 || attr == 4, Rules: row.get("smart_list", "rules")})
		}); err != nil {
			return nil, err
		}
	}
	byID := map[uint32]int{}
	for i := range lib.Playlists {
		byID[lib.Playlists[i].ID] = i
	}
	if table := find("playlist_content", "djmdSongPlaylist", "playlist_entry", "playlist_entries"); table != "" {
		if err := legacyEachSQLRow(ctx, db, table, []string{"playlist_id", "sequenceNo", "track_no", "seq", "entry_index", "position", "content_id", "track_id"}, func(row sqlRecord) {
			i, ok := byID[row.num("playlist_id")]
			if !ok {
				return
			}
			id := row.num("content_id", "track_id")
			for _, track := range lib.Tracks {
				if track.ID == id {
					lib.Playlists[i].Tracks = append(lib.Playlists[i].Tracks, id)
					break
				}
			}
		}); err != nil {
			return nil, err
		}
	}
	return lib, lib.Validate()
}

func TestOneLibraryProjectedRowsMatchLegacyFieldByField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exportLibrary.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE artist(artist_id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE album(album_id INTEGER PRIMARY KEY, name TEXT, artist_id INTEGER);
		CREATE TABLE genre(genre_id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE label(label_id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE "key"(key_id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE image(image_id INTEGER PRIMARY KEY, path TEXT);
		CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, bpmx100 INTEGER, length INTEGER, artist_id_artist INTEGER, artist_id_composer INTEGER, artist_id_remixer INTEGER, album_id INTEGER, genre_id INTEGER, label_id INTEGER, key_id INTEGER, image_id INTEGER, djComment TEXT, rating INTEGER, releaseYear INTEGER, path TEXT, fileSize INTEGER, bitrate INTEGER, bitDepth INTEGER, samplingRate INTEGER, analysisDataFilePath TEXT);
		CREATE TABLE playlist(playlist_id INTEGER PRIMARY KEY, sequenceNo INTEGER, name TEXT, attribute INTEGER, playlist_id_parent INTEGER);
		CREATE TABLE playlist_content(playlist_id INTEGER, content_id INTEGER, sequenceNo INTEGER);
		CREATE TABLE cue(cue_id INTEGER PRIMARY KEY, content_id INTEGER, kind INTEGER, colorTableIndex INTEGER, cueComment TEXT, isActiveLoop INTEGER, beatLoopNumerator INTEGER, beatLoopDenominator INTEGER, inUsec INTEGER, outUsec INTEGER);
		INSERT INTO artist VALUES(1,'Artist'),(2,'Composer');
		INSERT INTO album VALUES(10,'Album',1);
		INSERT INTO genre VALUES(20,'Genre');
		INSERT INTO label VALUES(30,'Label');
		INSERT INTO "key" VALUES(40,'8A');
		INSERT INTO image VALUES(50,'/Artwork/a.jpg');
		INSERT INTO content VALUES(7,'Track',12800,240,1,2,1,10,20,30,40,50,'Comment',4,2026,'/Contents/track.mp3',1234,320,24,44100,'/PIONEER/USBANLZ/P001/ANLZ0000.DAT');
		INSERT INTO playlist VALUES(100,1,'Set',0,0);
		INSERT INTO playlist_content VALUES(100,7,1);
		INSERT INTO cue VALUES(1,7,2,9,'Loop',1,4,1,1000000,2000000);
	`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	newDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := (OneLibraryAdapter{}).Read(context.Background(), newDB)
	newDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	oldDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := legacyReadOneLibrary(context.Background(), oldDB)
	oldDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Tracks, want.Tracks) || !reflect.DeepEqual(got.Playlists, want.Playlists) || !reflect.DeepEqual(got.Artwork, want.Artwork) || !reflect.DeepEqual(got.Keys, want.Keys) {
		t.Fatalf("projected reader differs\n got tracks=%+v lists=%+v artwork=%v keys=%v\nwant tracks=%+v lists=%+v artwork=%v keys=%v", got.Tracks, got.Playlists, got.Artwork, got.Keys, want.Tracks, want.Playlists, want.Artwork, want.Keys)
	}
}
