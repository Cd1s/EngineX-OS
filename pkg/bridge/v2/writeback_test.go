package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

func TestWriteBackGateRejectsPendingImport(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	if err := os.MkdirAll(filepath.Join(root, "PIONEER", "rekordbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
	if err := os.WriteFile(export, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(export)
	if err != nil {
		t.Fatal(err)
	}
	snap := rbstate.New("uuid")
	snap.Header.Layout = rbstate.LayoutFolders
	snap.Header.Sources = []rbstate.FileSig{{Rel: "PIONEER/rekordbox/export.pdb", Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}}
	snap.Header.Stamp = rbstate.Stamp(snap.Header.Layout, snap.Header.Sources, "")
	snap.Header.Pending.Rows = 1
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	res, err := WriteBackCues(context.Background(), root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("pending gate returned error: %v", err)
	}
	if res.FilesUpdated != 0 || res.TracksChanged != 0 {
		t.Fatalf("pending gate wrote files: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dbDir, rbstate.SyncedName)); !os.IsNotExist(err) {
		t.Fatalf("pending gate created synced state: %v", err)
	}
}

func TestBuildChangesRequiresSnapshotPathIdentity(t *testing.T) {
	row := dbCandidate{
		key:           rbstate.FoldPath("Contents/track.mp3"),
		track:         &rbstate.Track{Key: rbstate.FoldPath("Contents/track.mp3"), Refs: []rbstate.SourceRef{{Kind: rbstate.DeviceLibrary, ID: 7}}},
		metadata:      rekordbox.Metadata{Genre: "new"},
		writtenDigest: []byte{1},
	}
	libs := map[rbstate.LibraryKind]sourceLibrary{rbstate.DeviceLibrary: {
		Kind: rbstate.DeviceLibrary,
		Lib:  &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 7, Path: "Contents/other.mp3", Genre: "old"}}},
	}}
	changes, err := buildChanges(context.Background(), nil, libs, []dbCandidate{row})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("matched a different path by source id: %+v", changes)
	}
	libs[rbstate.DeviceLibrary] = sourceLibrary{Kind: rbstate.DeviceLibrary, Lib: &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 7, Path: "Contents/track.mp3", Genre: "old"}}}}
	changes, err = buildChanges(context.Background(), nil, libs, []dbCandidate{row})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].metadata[rbstate.DeviceLibrary] {
		t.Fatalf("did not match the snapshot path: %+v", changes)
	}
}

func TestWriteBackAnalysisOnlyPassSkipsExportRead(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	exportDir := filepath.Join(root, "PIONEER", "rekordbox")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(exportDir, "export.pdb")
	if err := os.WriteFile(export, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mDB := filepath.Join(dbDir, "m.db")
	db, err := sql.Open("sqlite", mDB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE Track(id INTEGER PRIMARY KEY, path TEXT, lastEditTime INTEGER, genre TEXT, comment TEXT, label TEXT); CREATE TABLE PerformanceData(trackId INTEGER PRIMARY KEY, beatData BLOB, trackData BLOB, quickCues BLOB, loops BLOB, activeOnLoadLoops INTEGER); INSERT INTO Track VALUES(1,'../Contents/track.mp3',1,'','',''); INSERT INTO PerformanceData VALUES(1,NULL,NULL,NULL,NULL,0);`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	info, err := os.Stat(export)
	if err != nil {
		t.Fatal(err)
	}
	written := enginedb.WrittenDigest(nil, nil, nil)
	syncDigest := enginedb.SyncedDigest(nil, nil, nil, 0, "", "", "")
	snap := rbstate.New("uuid")
	snap.Header.Layout = rbstate.LayoutFolders
	snap.Header.Sources = []rbstate.FileSig{{Rel: "PIONEER/rekordbox/export.pdb", Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}}
	snap.Header.Stamp = rbstate.Stamp(snap.Header.Layout, snap.Header.Sources, "")
	snap.Tracks[rbstate.FoldPath("Contents/track.mp3")] = &rbstate.Track{Key: rbstate.FoldPath("Contents/track.mp3"), Rel: "Contents/track.mp3", EngineID: 1, Written: written, Refs: []rbstate.SourceRef{{Kind: rbstate.DeviceLibrary, ID: 1}}}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	state := syncedState{Version: 1, Rows: map[string]syncedStateRecord{rbstate.FoldPath("Contents/track.mp3"): {EngineID: 1, Edit: "1", Digest: syncDigest}}}
	b, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dbDir, rbstate.SyncedName), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := WriteBackCues(context.Background(), root, WriteBackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TracksRead != 0 || res.FilesUpdated != 0 || res.PhasesMS["decrypt"] != 0 {
		t.Fatalf("analysis-only pass read or decrypted export: %+v", res)
	}
}
