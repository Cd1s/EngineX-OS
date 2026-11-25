package bridge

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestAdoptMigratesFastBaseAndRejectsStaleSyncedDigest(t *testing.T) {
	dir := t.TempDir()
	ids, libraryUUID, _, err := enginedb.BuildLibrary(context.Background(), dir, "", []enginedb.TrackRow{{
		Rel: "Contents/a.mp3", Track: rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", Title: "A"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, oldStateName)
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE BridgeState(path TEXT,trackId INTEGER,fingerprint TEXT);
		CREATE TABLE BridgeFastBase(trackId INTEGER PRIMARY KEY,record INTEGER,rating INTEGER,quickCues BLOB,loops BLOB,active INTEGER);
		CREATE TABLE BridgeWritten(trackId INTEGER PRIMARY KEY,digest BLOB);
		CREATE TABLE BridgeSynced(trackId INTEGER PRIMARY KEY,lastEditTime INTEGER,digest BLOB);
		INSERT INTO BridgeState VALUES('../Contents/a.mp3',1,'fast:old-record');
		INSERT INTO BridgeFastBase VALUES(1,?,77,X'0102',X'0304',5);
		INSERT INTO BridgeSynced VALUES(1,1,X'BA');`, enginedb.FastBaseRecord("fast:old-record"))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := adopt(context.Background(), filepath.Join(dir, "m.db"), dir, libraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	track := s.Tracks["contents/a.mp3"]
	if track == nil || !track.FastKnown || track.FastRating != 77 || track.FastActive != 5 {
		t.Fatalf("fast baseline not migrated: %+v", track)
	}
	if track.Synced != nil || track.Edit != 0 {
		t.Fatalf("stale BridgeSynced was adopted: edit=%d digest=%x", track.Edit, track.Synced)
	}
	_ = ids
}
