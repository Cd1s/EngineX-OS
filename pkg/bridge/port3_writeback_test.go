package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	bridge "github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/bridgetest"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

func p3OpenDB(t *testing.T, mount string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(mount, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	return db
}

func p3DeckCue(t *testing.T, db *sql.DB, id int64, name string, hot uint32, ms uint32) {
	t.Helper()
	perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 2, SampleRate: 44100}, &rekordbox.Analysis{
		Beats: []rekordbox.Beat{{TimeMS: 250, BPM: 120, Number: 1}},
		Cues:  []rekordbox.Cue{{HotCue: hot, TimeMS: ms, Name: name, Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=?, loops=? WHERE trackId=?`, perf.TrackData, perf.QuickCues, perf.Loops, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+7 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func p3FirstTrackID(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM Track ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func p3AnalysisHas(t *testing.T, mount, name string) bool {
	t.Helper()
	a, err := rekordbox.ReadAnalysis(filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}
	for _, cue := range a.Cues {
		if cue.Name == name {
			return true
		}
	}
	return false
}

func TestP3CueWriteBackUpdatesANLZBackupAndReinsert(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	id := p3FirstTrackID(t, db)
	p3DeckCue(t, db, id, "P3 hot cue", 1, 750)
	_ = db.Close()

	datPath := filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT")
	originalDAT, err := os.ReadFile(datPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{DateDir: "2026-11-24", SkipPreparingCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.TracksChecked != 5 || res.TracksRead != 1 || res.TracksChanged != 1 || res.FilesUpdated == 0 {
		t.Fatalf("write-back result: %+v", res)
	}
	backup, err := os.ReadFile(filepath.Join(mount, ".enginex-backup", "2026-11-24", "PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT"))
	if err != nil || !bytes.Equal(backup, originalDAT) {
		t.Fatalf("original DAT backup mismatch: %v", err)
	}
	for _, ext := range []string{".DAT", ".EXT"} {
		a, readErr := rekordbox.ReadAnalysis(filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000"+ext))
		if readErr != nil {
			t.Fatal(readErr)
		}
		found := false
		for _, cue := range a.Cues {
			if cue.Name == "P3 hot cue" {
				found = true
			}
		}
		if ext == ".EXT" && !found {
			t.Fatalf("%s does not contain the deck cue", ext)
		}
		if ext == ".DAT" && len(a.Cues) < 2 {
			t.Fatalf("%s lost the compact cue: %+v", ext, a.Cues)
		}
	}
	second, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{DateDir: "2026-11-25", SkipPreparingCheck: true})
	if err != nil || second.TracksChanged != 0 || second.TracksRead != 0 || second.FilesUpdated != 0 {
		t.Fatalf("second pass was not idempotent: %+v %v", second, err)
	}
	bridgetest.Import(t, mount)
	check := p3OpenDB(t, mount)
	var tracks int
	if err := check.QueryRow(`SELECT count(*) FROM Track`).Scan(&tracks); err != nil {
		t.Fatal(err)
	}
	if tracks != 5 || !p3AnalysisHas(t, mount, "P3 hot cue") {
		t.Fatalf("reinsert lost the written cue: tracks=%d", tracks)
	}
}

func TestP3MetadataWriteBackUpdatesPDB(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	id := p3FirstTrackID(t, db)
	if _, err := db.Exec(`UPDATE Track SET genre=?, comment=?, label=? WHERE id=?`, strings.Repeat("G", 140), "New Comment", strings.Repeat("L", 100), id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+7 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	res, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{DateDir: "2026-11-24", SkipPreparingCheck: true})
	if err != nil || res.TracksChanged != 1 {
		t.Fatalf("metadata write-back: %+v %v", res, err)
	}
	lib, err := rekordbox.ReadPDB(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil || len(lib.Tracks) == 0 {
		t.Fatalf("read PDB: %v", err)
	}
	track := lib.Tracks[0]
	if track.Genre != strings.Repeat("G", 140) || track.Comment != "New Comment" || track.Label != strings.Repeat("L", 100) {
		t.Fatalf("metadata was not written: %+v", track)
	}
}

func TestP3PullBeforeRenameLeavesOriginalFilesIntact(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	p3DeckCue(t, db, p3FirstTrackID(t, db), "pulled cue", 1, 900)
	_ = db.Close()
	paths := []string{
		filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT"),
		filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"),
		filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.2EX"),
		filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"),
	}
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		before[path], _ = os.ReadFile(path)
	}
	pulled := errors.New("simulated pull-out")
	_, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{DateDir: "2026-11-24", SkipPreparingCheck: true, BeforeRename: func(string) error { return pulled }})
	if !errors.Is(err, pulled) {
		t.Fatalf("write-back error=%v, want pull error", err)
	}
	for path, want := range before {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s changed after pull: %v", path, readErr)
		}
	}
}

func TestP3SecondRenameFailureLeavesAllExportTargetsIntact(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	p3DeckCue(t, db, p3FirstTrackID(t, db), "second rename", 1, 950)
	_ = db.Close()
	paths := []string{
		filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT"),
		filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"),
		filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.2EX"),
		filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"),
	}
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		before[path], _ = os.ReadFile(path)
	}
	calls := 0
	pulled := errors.New("simulated pull after staging")
	_, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{DateDir: "2026-11-24", SkipPreparingCheck: true, BeforeRename: func(string) error {
		calls++
		if calls == 2 {
			return pulled
		}
		return nil
	}})
	if !errors.Is(err, pulled) || calls < 2 {
		t.Fatalf("second rename failure: calls=%d err=%v", calls, err)
	}
	for path, want := range before {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s changed after aborted staged write: %v", path, readErr)
		}
	}
}

func TestP3WriteBackReadsUncheckpointedWAL(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;`); err != nil {
		t.Fatal(err)
	}
	p3DeckCue(t, db, p3FirstTrackID(t, db), "WAL cue", 1, 1000)
	res, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{DateDir: "2026-11-24", SkipPreparingCheck: true})
	if err != nil || res.TracksChanged != 1 || !p3AnalysisHas(t, mount, "WAL cue") {
		t.Fatalf("uncheckpointed WAL edit was missed: %+v %v", res, err)
	}
}

func TestP3WriteBackDetectsActiveLoopDeactivation(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	loop := []rekordbox.Cue{{HotCue: 1, TimeMS: 500, EndMS: 1500, Loop: true, Active: true, Name: "active loop", Color: [4]byte{255, 0, 224, 255}, ColorCode: 9}}
	for _, item := range []struct {
		ext      string
		extended bool
	}{{".DAT", false}, {".EXT", true}} {
		path := filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000"+item.ext)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		updated, _, err := rekordbox.RewriteANLZCues(raw, loop, item.extended, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, updated, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	id := p3FirstTrackID(t, db)
	if _, err := db.Exec(`UPDATE PerformanceData SET activeOnLoadLoops=0 WHERE trackId=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+7 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	res, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{SkipPreparingCheck: true})
	if err != nil || res.TracksChanged != 1 {
		t.Fatalf("active-loop deactivation: %+v %v", res, err)
	}
	after, err := rekordbox.ReadAnalysis(filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}
	for _, cue := range after.Cues {
		if cue.HotCue == 1 && cue.Loop {
			if cue.Active {
				t.Fatalf("active loop remained active after deck deactivation: %+v", cue)
			}
			return
		}
	}
	t.Fatalf("active loop missing after write-back: %+v", after.Cues)
}

func TestP3WriteBackRoundTripsCueDeletion(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	id := p3FirstTrackID(t, db)
	var original, originalTrack, originalLoops []byte
	if err := db.QueryRow(`SELECT quickCues, trackData, loops FROM PerformanceData WHERE trackId=?`, id).Scan(&original, &originalTrack, &originalLoops); err != nil {
		t.Fatal(err)
	}
	p3DeckCue(t, db, id, "round trip cue", 1, 750)
	_ = db.Close()
	first, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{SkipPreparingCheck: true})
	if err != nil || first.TracksChanged != 1 || !p3AnalysisHas(t, mount, "round trip cue") {
		t.Fatalf("cue add: %+v %v", first, err)
	}
	db = p3OpenDB(t, mount)
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=?, trackData=?, loops=? WHERE trackId=?`, original, originalTrack, originalLoops, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+17 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	second, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{SkipPreparingCheck: true})
	if err != nil || second.TracksChanged != 1 {
		t.Fatalf("cue deletion: %+v %v", second, err)
	}
	if p3AnalysisHas(t, mount, "round trip cue") {
		t.Fatal("deleted cue remained on the stick")
	}
}

func TestP3UnreadableOneLibraryIsExcludedFromWriteBack(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	if err := os.WriteFile(filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db"), []byte("not a SQLCipher database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.ConvertAtMount(context.Background(), mount, bridge.MountOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.FillInBackground(context.Background(), mount, bridge.FillOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := bridge.ConvertAtMount(context.Background(), mount, bridge.MountOptions{Publish: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pending.None() {
		t.Fatalf("v2 gap: unreadable OneLibrary should be excluded so PDB write-back can proceed, got pending=%+v", res.Pending)
	}
}
