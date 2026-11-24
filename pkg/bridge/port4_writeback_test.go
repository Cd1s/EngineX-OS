package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

func p4ImportedStick(t *testing.T) string {
	t.Helper()
	mount := t.TempDir()
	testfixture.Export(t, mount)
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{}); err != nil {
		t.Fatalf("mount window: %v", err)
	}
	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatalf("background job: %v", err)
	}
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{Publish: true})
	if err != nil {
		t.Fatalf("publishing window: %v", err)
	}
	if !res.Pending.None() && !res.Bypassed {
		t.Fatalf("import is incomplete: %+v", res.Pending)
	}
	return mount
}

func p4DB(t *testing.T, mount string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(mount, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func p4TrackID(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM Track ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func p4DeckEdit(t *testing.T, mount, cueName, genre, comment, label string) {
	t.Helper()
	db := p4DB(t, mount)
	id := p4TrackID(t, db)
	perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{
		Beats: []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 1000, Name: cueName, Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
			{HotCue: 2, TimeMS: 2000, EndMS: 3000, Loop: true, Name: "Deck Loop", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,quickCues=?,loops=?,activeOnLoadLoops=? WHERE trackId=?`, perf.TrackData, perf.QuickCues, perf.Loops, perf.ActiveLoops, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET genre=?,comment=?,label=? WHERE id=?`, genre, comment, label, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+7 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func p4AnalysisHas(t *testing.T, mount, name string) bool {
	t.Helper()
	for _, ext := range []string{".DAT", ".EXT"} {
		raw, err := os.ReadFile(filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000"+ext))
		if err != nil {
			t.Fatal(err)
		}
		a, err := rekordbox.ParseAnalysis(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", ext, err)
		}
		for _, cue := range a.Cues {
			if cue.Name == name {
				return true
			}
		}
	}
	return false
}

func TestPort4WriteBackPDBCuesMetadataBackupAndConvergence(t *testing.T) {
	mount := p4ImportedStick(t)
	oldDAT, err := os.ReadFile(filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT"))
	if err != nil {
		t.Fatal(err)
	}
	p4DeckEdit(t, mount, "P4 hot cue", "P4 genre", "P4 comment", "P4 label")
	res, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-24", SkipPreparingCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.TracksChecked != 5 || res.TracksRead != 1 || res.TracksChanged != 1 || res.FilesUpdated < 3 || res.OneLibraryUpdated {
		t.Fatalf("write-back result: %+v", res)
	}
	backup, err := os.ReadFile(filepath.Join(mount, ".enginex-backup", "2026-11-24", "PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT"))
	if err != nil || !bytes.Equal(backup, oldDAT) {
		t.Fatalf("original DAT backup mismatch: %v", err)
	}
	if !p4AnalysisHas(t, mount, "P4 hot cue") {
		t.Fatal("deck cue was not written to ANLZ")
	}
	lib, err := rekordbox.ReadPDB(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Tracks) == 0 || lib.Tracks[0].Genre != "P4 genre" || lib.Tracks[0].Comment != "P4 comment" || lib.Tracks[0].Label != "P4 label" {
		t.Fatalf("PDB metadata: %+v", lib.Tracks)
	}
	second, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-25", SkipPreparingCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.TracksRead != 0 || second.TracksChanged != 0 || second.FilesUpdated != 0 || second.PhasesMS["decrypt"] != 0 {
		t.Fatalf("second pass was not a no-op: %+v", second)
	}
	if _, err := os.Stat(filepath.Join(mount, ".enginex-backup", "2026-11-25")); !os.IsNotExist(err) {
		t.Fatalf("second pass made a backup: %v", err)
	}
}

func TestPort4WriteBackAnalysisOnlyChangeSkipsExportRead(t *testing.T) {
	mount := p4ImportedStick(t)
	db := p4DB(t, mount)
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+7 WHERE id=?`, p4TrackID(t, db)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	session := &CueSession{}
	res, err := WriteBackCues(context.Background(), mount, WriteBackOptions{SkipPreparingCheck: true, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if res.TracksChecked != 5 || res.TracksRead != 1 || res.TracksChanged != 0 || res.FilesUpdated != 0 || res.PhasesMS["decrypt"] != 0 {
		t.Fatalf("analysis-only pass: %+v", res)
	}
	second, err := WriteBackCues(context.Background(), mount, WriteBackOptions{SkipPreparingCheck: true, Session: session})
	if err != nil || second.TracksRead != 0 {
		t.Fatalf("noted analysis-only row was read again: %+v %v", second, err)
	}
	third, err := WriteBackCues(context.Background(), mount, WriteBackOptions{SkipPreparingCheck: true})
	if err != nil || third.TracksRead != 1 || third.PhasesMS["decrypt"] != 0 {
		t.Fatalf("a pass without the watcher session: %+v %v", third, err)
	}
}

func TestPort4WriteBackRejectsSnapshotPathMismatch(t *testing.T) {
	mount := p4ImportedStick(t)
	db := p4DB(t, mount)
	id := p4TrackID(t, db)
	if _, err := db.Exec(`UPDATE Track SET path='../MUSIC/other.wav' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+7 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := WriteBackCues(context.Background(), mount, WriteBackOptions{SkipPreparingCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.TracksRead != 0 || res.TracksChanged != 0 || res.FilesUpdated != 0 {
		t.Fatalf("path mismatch was treated as a write-back candidate: %+v", res)
	}
}
