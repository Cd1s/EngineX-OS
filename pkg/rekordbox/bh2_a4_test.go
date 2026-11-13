package rekordbox

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOneLibrarySkipsSoftDeletedRekordboxRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "exportLibrary.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE djmdContent (
			ID INTEGER PRIMARY KEY,
			Title TEXT,
			file_path TEXT,
			BPM INTEGER,
			rb_local_deleted INTEGER DEFAULT 0
		);
		CREATE TABLE djmdPlaylist (
			ID INTEGER PRIMARY KEY,
			ParentID TEXT,
			Name TEXT,
			Attribute INTEGER,
			Seq INTEGER,
			rb_local_deleted INTEGER DEFAULT 0
		);
		CREATE TABLE djmdSongPlaylist (
			ID INTEGER PRIMARY KEY,
			PlaylistID INTEGER,
			ContentID INTEGER,
			TrackNo INTEGER,
			rb_local_deleted INTEGER DEFAULT 0
		);
		CREATE TABLE djmdCue (
			ID INTEGER PRIMARY KEY,
			ContentID INTEGER,
			Kind INTEGER,
			InUsec INTEGER,
			CueComment TEXT,
			rb_local_deleted INTEGER DEFAULT 0
		);

		INSERT INTO djmdContent VALUES (1, 'Active Track', '/Contents/a.mp3', 12800, 0);
		INSERT INTO djmdContent VALUES (2, 'Deleted Track', '/Contents/deleted.mp3', 12800, 1);

		INSERT INTO djmdPlaylist VALUES (10, 'root', 'Live Set', 0, 1, 0);
		INSERT INTO djmdPlaylist VALUES (11, 'root', 'Deleted Set', 0, 2, 1);

		INSERT INTO djmdSongPlaylist VALUES (100, 10, 1, 1, 0);
		INSERT INTO djmdSongPlaylist VALUES (101, 10, 2, 2, 1);
		INSERT INTO djmdSongPlaylist VALUES (102, 10, 1, 3, 1);

		INSERT INTO djmdCue VALUES (200, 1, 1, 1000000, 'Keep Cue', 0);
		INSERT INTO djmdCue VALUES (201, 1, 2, 2000000, 'Deleted Cue', 1);
	`)
	if err != nil {
		t.Fatal(err)
	}

	lib, err := ReadSQLite(context.Background(), dbPath, nil)
	if err != nil {
		t.Fatalf("ReadSQLite: %v", err)
	}
	if len(lib.Tracks) != 1 || lib.Tracks[0].ID != 1 {
		t.Fatalf("soft-deleted track resurrected: %+v", lib.Tracks)
	}
	if len(lib.Playlists) != 1 || lib.Playlists[0].ID != 10 {
		t.Fatalf("soft-deleted playlist resurrected: %+v", lib.Playlists)
	}
	if len(lib.Playlists[0].Tracks) != 1 || lib.Playlists[0].Tracks[0] != 1 {
		t.Fatalf("soft-deleted playlist entry resurrected: %+v", lib.Playlists[0].Tracks)
	}
	if len(lib.Tracks[0].Cues) != 1 || lib.Tracks[0].Cues[0].Name != "Keep Cue" {
		t.Fatalf("soft-deleted cue resurrected: %+v", lib.Tracks[0].Cues)
	}
}
