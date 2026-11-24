package enginedb_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

func TestSyncPlaylistsOnlyLeavesTracksAndBridgeStateUntouched(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	if _, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, true); err != nil {
		t.Fatal(err)
	}
	library, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(library.Tracks) == 0 {
		t.Fatal("fixture has no tracks")
	}
	library.Playlists = append(library.Playlists, rekordbox.Playlist{ID: 999999, Name: "Added", Tracks: []uint32{library.Tracks[0].ID}})
	dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-state.db")
	if _, err = db.Exec(`ATTACH DATABASE ? AS bridge`, statePath); err != nil {
		t.Fatal(err)
	}
	var beforeTracks, beforeState int
	if err = db.QueryRow(`SELECT count(*) FROM Track`).Scan(&beforeTracks); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM bridge.BridgeState`).Scan(&beforeState); err != nil {
		t.Fatal(err)
	}
	db.Close()

	res, err := enginedb.SyncPlaylistsOnly(context.Background(), root, library)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.Updated != 0 || res.Removed != 0 {
		t.Fatalf("playlist result %+v", res)
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`ATTACH DATABASE ? AS bridge`, statePath); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var afterTracks, afterState, added int
	if err = db.QueryRow(`SELECT count(*) FROM Track`).Scan(&afterTracks); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM bridge.BridgeState`).Scan(&afterState); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM Playlist WHERE title='Added'`).Scan(&added); err != nil {
		t.Fatal(err)
	}
	if afterTracks != beforeTracks || afterState != beforeState || added != 1 {
		t.Fatalf("tracks/state changed: tracks %d/%d state %d/%d added %d", beforeTracks, afterTracks, beforeState, afterState, added)
	}
}
