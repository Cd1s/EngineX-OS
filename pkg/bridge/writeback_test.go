package bridge

import (
	"bytes"
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
	if entries, _ := os.ReadDir(dbDir); len(entries) != 1 {
		t.Fatalf("the refused pass left files beside the snapshot: %v", entries)
	}
}

func TestBuildChangesRequiresSnapshotPathIdentity(t *testing.T) {
	row := dbCandidate{
		key:           rbstate.FoldPath("Contents/track.mp3"),
		track:         &rbstate.Track{Key: rbstate.FoldPath("Contents/track.mp3"), Refs: []rbstate.SourceRef{{Kind: rbstate.DeviceLibrary, ID: 7}}},
		metadata:      rekordbox.Metadata{Genre: "new"},
		writtenDigest: []byte{1},
	}
	libs := map[rbstate.LibraryKind]SourceLibrary{rbstate.DeviceLibrary: {
		Kind: rbstate.DeviceLibrary,
		Lib:  &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 7, Path: "Contents/other.mp3", Genre: "old"}}},
	}}
	changes, _, err := buildChanges(context.Background(), nil, libs, []dbCandidate{row})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("matched a different path by source id: %+v", changes)
	}
	libs[rbstate.DeviceLibrary] = SourceLibrary{Kind: rbstate.DeviceLibrary, Lib: &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 7, Path: "Contents/track.mp3", Genre: "old"}}}}
	changes, _, err = buildChanges(context.Background(), nil, libs, []dbCandidate{row})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].metadata[rbstate.DeviceLibrary] {
		t.Fatalf("did not match the snapshot path: %+v", changes)
	}
}

func TestBuildChangesDoesNotConfirmMissingSourceReference(t *testing.T) {
	row := dbCandidate{
		key:           rbstate.FoldPath("Contents/track.mp3"),
		track:         &rbstate.Track{Key: rbstate.FoldPath("Contents/track.mp3"), Refs: []rbstate.SourceRef{{Kind: rbstate.OneLibrary, ID: 9}}},
		writtenDigest: []byte{1},
	}
	changes, matched, err := buildChanges(context.Background(), nil, map[rbstate.LibraryKind]SourceLibrary{}, []dbCandidate{row})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 || matched[row.key] {
		t.Fatalf("missing source was confirmed: changes=%v matched=%v", changes, matched)
	}
}

// A row Engine wrote without changing its cues, loops or tags (an analysis, a grid, a rating) is
// read, and that is all: the export is not opened (here it is not even a library), nothing is
// written. A row whose lastEditTime is the one the snapshot noted is not read at all.
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
	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE Track(id INTEGER PRIMARY KEY, path TEXT, lastEditTime INTEGER, genre TEXT, comment TEXT, label TEXT); CREATE TABLE PerformanceData(trackId INTEGER PRIMARY KEY, beatData BLOB, trackData BLOB, quickCues BLOB, loops BLOB, activeOnLoadLoops INTEGER); INSERT INTO Track VALUES(1,'../Contents/track.mp3',2,'','',''); INSERT INTO PerformanceData VALUES(1,NULL,NULL,NULL,NULL,0);`)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(export)
	if err != nil {
		t.Fatal(err)
	}
	key := rbstate.FoldPath("Contents/track.mp3")
	snap := rbstate.New("uuid")
	snap.Header.Layout = rbstate.LayoutFolders
	snap.Header.Sources = []rbstate.FileSig{{Rel: "PIONEER/rekordbox/export.pdb", Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}}
	snap.Header.Stamp = rbstate.Stamp(snap.Header.Layout, snap.Header.Sources, "")
	snap.Tracks[key] = &rbstate.Track{Key: key, Rel: "Contents/track.mp3", EngineID: 1, Written: enginedb.WrittenDigest(nil, nil, nil),
		Edit: 1, Synced: enginedb.SyncedDigest(nil, nil, nil, 0, "", "", ""), Refs: []rbstate.SourceRef{{Kind: rbstate.DeviceLibrary, ID: 1}}}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(filepath.Join(dbDir, rbstate.SnapshotName))
	session := &CueSession{}
	pass := func() WriteBackResult {
		t.Helper()
		res, err := WriteBackCues(context.Background(), root, WriteBackOptions{Session: session})
		if err != nil {
			t.Fatal(err)
		}
		if res.TracksChecked != 1 || res.FilesUpdated != 0 || res.PhasesMS["decrypt"] != 0 {
			t.Fatalf("the pass opened the export or wrote: %+v", res)
		}
		if now, _ := os.ReadFile(filepath.Join(dbDir, rbstate.SnapshotName)); !bytes.Equal(now, saved) {
			t.Fatal("a pass that read no export wrote the snapshot")
		}
		return res
	}
	// Engine wrote the row (its time moved, 1 to 2) and left cues, loops and tags alone.
	if res := pass(); res.TracksRead != 1 {
		t.Fatalf("the written row was not read: %+v", res)
	}
	// The watcher remembers that: its next pass does not read the row again.
	if res := pass(); res.TracksRead != 0 {
		t.Fatalf("a row found in sync was read again: %+v", res)
	}
	// A pass of another process has the snapshot alone: it reads the row once more, no more.
	session = nil
	if res := pass(); res.TracksRead != 1 {
		t.Fatalf("pass without a session: %+v", res)
	}
	if _, err = db.Exec(`UPDATE Track SET lastEditTime=1`); err != nil {
		t.Fatal(err)
	}
	if res := pass(); res.TracksRead != 0 {
		t.Fatalf("an untouched row was read: %+v", res)
	}
}

func TestNeedsCueReadFilledStageIgnoresEngineMainCueTail(t *testing.T) {
	stagedCues := []byte("stage-cues-012345")
	rowCues := append([]byte(nil), stagedCues...)
	copy(rowCues[len(rowCues)-8:], []byte("engine!!"))
	row := dbCandidate{
		track:       &rbstate.Track{Written: nil, Synced: nil},
		trackData:   []byte{1},
		beatData:    []byte("beats"),
		quickCues:   rowCues,
		loops:       []byte("loops"),
		filledBeats: []byte("beats"),
		filledCues:  stagedCues,
		filledLoops: []byte("loops"),
	}
	if needsCueRead(row) {
		t.Fatal("filled row was treated as a deck cue edit")
	}
	row.quickCues[0] ^= 1
	if !needsCueRead(row) {
		t.Fatal("filled row with a real cue edit was ignored")
	}
}

func TestReadFilledStageUsesLastValidRowAndStageDigest(t *testing.T) {
	root := t.TempDir()
	t.Setenv(rbstate.StageEnv, root)
	stage, err := rbstate.OpenStage("filled-test")
	if err != nil {
		t.Fatal(err)
	}
	rec := &rbstate.Staged{Key: "contents/track.mp3", Perf: rbstate.Perf{Beats: []byte("beats"), QuickCues: []byte("cues"), Loops: []byte("loops")}}
	rec.Digest = enginedb.WrittenDigest(rec.Perf.Beats, rec.Perf.QuickCues, rec.Perf.Loops)
	if err := stage.Put(rec); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(struct {
		ID int64  `json:"id"`
		K  string `json:"k"`
		WR []byte `json:"wr"`
	}{ID: 9, K: rec.Key, WR: rec.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage.Dir(), "filled.jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readFilledStage(&rbstate.Snapshot{Header: rbstate.Header{LibraryUUID: "filled-test"}})
	row, ok := got[rec.Key]
	if !ok || row.id != 9 || !bytes.Equal(row.written, rec.Digest) || !bytes.Equal(row.cues, rec.Perf.QuickCues) {
		t.Fatalf("filled row was not read: %+v", got)
	}
}
