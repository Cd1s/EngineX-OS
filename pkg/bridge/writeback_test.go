package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
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
	stagedCues := makeMainCueBlob(9920, 9920, 0)
	rowCues := makeMainCueBlob(-1105, -1105, 0)
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

func makeMainCueBlob(main, defaultMain float64, adjusted byte) []byte {
	raw := make([]byte, 8+8*(1+12)+17)
	binary.BigEndian.PutUint64(raw, 8)
	off := 8
	for i := 0; i < 8; i++ {
		raw[off] = 0
		off++
		off += 12
	}
	binary.BigEndian.PutUint64(raw[off:], math.Float64bits(main))
	raw[off+8] = adjusted
	binary.BigEndian.PutUint64(raw[off+9:], math.Float64bits(defaultMain))
	return raw
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
	// When the stage entry was replaced by a newer export, filled.jsonl must not match it.
	rec.Perf.QuickCues = []byte("newer-cues")
	rec.Digest = enginedb.WrittenDigest(rec.Perf.Beats, rec.Perf.QuickCues, rec.Perf.Loops)
	if err := stage.Put(rec); err != nil {
		t.Fatal(err)
	}
	if got = readFilledStage(&rbstate.Snapshot{Header: rbstate.Header{LibraryUUID: "filled-test"}}); len(got) != 0 {
		t.Fatalf("stale stage entry matched old filled.jsonl digest: %+v", got)
	}
}

func TestNeedsCueReadAcceptsLegacyRawWrittenDigest(t *testing.T) {
	beats := []byte("beats")
	loops := []byte("loops")
	cues, err := enginedb.QCompress(makeMainCueBlob(9920, 9920, 0))
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest := enginedb.RawWrittenDigest(beats, cues, loops)
	row := dbCandidate{
		track:            &rbstate.Track{Written: legacyDigest},
		trackData:        []byte{1},
		beatData:         beats,
		quickCues:        cues,
		loops:            loops,
		writtenDigest:    enginedb.WrittenDigest(beats, cues, loops),
		rawWrittenDigest: enginedb.RawWrittenDigest(beats, cues, loops),
	}
	if needsCueRead(row) {
		t.Fatal("untouched legacy RawWrittenDigest triggered cue write-back read")
	}
	editedCues, err := enginedb.QCompress(makeMainCueBlob(4410, 9920, 1))
	if err != nil {
		t.Fatal(err)
	}
	row.quickCues = editedCues
	row.writtenDigest = enginedb.WrittenDigest(beats, editedCues, loops)
	row.rawWrittenDigest = enginedb.RawWrittenDigest(beats, editedCues, loops)
	if !needsCueRead(row) {
		t.Fatal("deck edit on legacy row was ignored by write-back")
	}
}

// RD-4 regression test (fails on f65d4fdf):
//  1. Adopting a legacy library (Synced == nil across > 64 rows) exercises the sequential scan.
//  2. Untouched legacy take-over tracks (main cue reset to -1 with memory-cue digest, pre-#178
//     0-offset cues on MP3, or PDB-vs-OneLibrary differences) must NEVER count as deck edits.
//  3. DryRun reports exact changed tracks and fields without modifying the stick.
func TestTakeOverNeverWritesBackUntouchedLegacyTracksAndDryRun(t *testing.T) {
	ctx := context.Background()
	seedFullImport := func(dir string) {
		t.Helper()
		if _, err := ConvertAtMount(ctx, dir, MountOptions{}); err != nil {
			t.Fatalf("ConvertAtMount(%s): %v", dir, err)
		}
		if _, err := FillInBackground(ctx, dir, FillOptions{}); err != nil {
			t.Fatalf("FillInBackground(%s): %v", dir, err)
		}
		if _, err := ConvertAtMount(ctx, dir, MountOptions{Publish: true}); err != nil {
			t.Fatalf("Publish(%s): %v", dir, err)
		}
	}
	root := t.TempDir()
	testfixture.LargeExport(t, root, 70)
	// Extend Track 70 to 120 frames (~3.1s) so its 500..1500ms fixture cues fit within SampleCount.
	const mp3Frame = 417
	longMP3 := make([]byte, 120*mp3Frame)
	for i := 0; i < 120; i++ {
		copy(longMP3[i*mp3Frame:], []byte{0xff, 0xfb, 0x90, 0})
	}
	copy(longMP3[36:], "Xing")
	binary.BigEndian.PutUint32(longMP3[40:], 3)
	binary.BigEndian.PutUint32(longMP3[44:], 120)
	binary.BigEndian.PutUint32(longMP3[48:], uint32(len(longMP3)))
	copy(longMP3[52:], "LAME3.100")
	longMP3[73], longMP3[74], longMP3[75] = 0x24, 0, 0
	testfixture.Write(t, root, testfixture.LargeTrackPath(70), longMP3)
	seedFullImport(root)

	dbDir := filepath.Join(root, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Track 1: simulate Engine rewriting main cue to -1 while the r2 snapshot held the ANLZ
	// memory cue's 250ms sample position (44.1kHz * 0.25s = 11025) as RawWrittenDigest.
	var beats1, cues1, loops1 []byte
	if err := db.QueryRow(`SELECT beatData, quickCues, loops FROM PerformanceData WHERE trackId=1`).Scan(&beats1, &cues1, &loops1); err != nil {
		t.Fatal(err)
	}
	rawCues1, err := enginedb.QUncompress(cues1, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	legacyCuesBuf := append([]byte(nil), rawCues1...)
	offMain := len(legacyCuesBuf) - 17
	binary.BigEndian.PutUint64(legacyCuesBuf[offMain:], math.Float64bits(11025))
	legacyCuesBuf[offMain+8] = 0
	binary.BigEndian.PutUint64(legacyCuesBuf[offMain+9:], math.Float64bits(11025))
	legacyQC1, err := enginedb.QCompress(legacyCuesBuf)
	if err != nil {
		t.Fatal(err)
	}
	engineZeroBuf := append([]byte(nil), rawCues1...)
	binary.BigEndian.PutUint64(engineZeroBuf[offMain:], math.Float64bits(-1))
	engineZeroBuf[offMain+8] = 0
	binary.BigEndian.PutUint64(engineZeroBuf[offMain+9:], math.Float64bits(-1))
	engineQC1, err := enginedb.QCompress(engineZeroBuf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=1`, engineQC1); err != nil {
		t.Fatal(err)
	}

	// Track 2: simulate pre-#178 0-offset cues on MP3 (without subtracted LAME delay).
	info, err := enginedb.EncodePerformance(
		rekordbox.Track{ID: 2, SampleRate: 44100, SampleCount: 120000, Duration: 3, BPM: 120},
		&rekordbox.Analysis{
			Beats: []rekordbox.Beat{{Number: 1, BPM: 120, TimeMS: 0}, {Number: 2, BPM: 120, TimeMS: 500}},
			Cues: []rekordbox.Cue{
				{HotCue: 2, TimeMS: 500, Name: "B 原有", Color: [4]byte{12, 34, 56, 255}, Extended: true},
				{HotCue: 8, TimeMS: 1000, EndMS: 1500, Loop: true, Name: "H Loop", Color: [4]byte{99, 88, 77, 255}, Extended: true},
				{TimeMS: 250, Name: "Mémoire", Color: [4]byte{55, 66, 77, 255}, Extended: true},
				{TimeMS: 750, EndMS: 1000, Loop: true, Name: "Memory Loop", Color: [4]byte{9, 8, 7, 255}, Extended: true},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=?, loops=? WHERE trackId=2`, info.QuickCues, info.Loops); err != nil {
		t.Fatal(err)
	}

	// Track 70: real deck edit (comment changed + hot cue B moved).
	var t70Data, t70Cues, t70Loops []byte
	if err := db.QueryRow(`SELECT trackData, quickCues, loops FROM PerformanceData WHERE trackId=70`).Scan(&t70Data, &t70Cues, &t70Loops); err != nil {
		t.Fatal(err)
	}
	rawCues70, err := enginedb.QUncompress(t70Cues, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Shift hot cue slot 1 (index 1 = B) by +44100 samples (+1000 ms). Slots have variable-length UTF-8 names.
	hcOff := 8
	hcOff += 1 + int(rawCues70[hcOff]) + 12 // skip slot 0
	hcOff += 1 + int(rawCues70[hcOff])      // advance past slot 1 nameLen + UTF-8 name
	oldSamples := math.Float64frombits(binary.BigEndian.Uint64(rawCues70[hcOff : hcOff+8]))
	if oldSamples < 0 {
		t.Fatalf("expected hot cue B set on fixture track 70, got samples=%v", oldSamples)
	}
	binary.BigEndian.PutUint64(rawCues70[hcOff:hcOff+8], math.Float64bits(oldSamples+44100))
	editedQC70, err := enginedb.QCompress(rawCues70)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=70`, editedQC70); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET comment='LiveDeckEdit' WHERE id=70`); err != nil {
		t.Fatal(err)
	}

	// Emulate legacy take-over snapshot: Synced == nil across all 70 tracks.
	for _, tr := range snap.Tracks {
		tr.Synced = nil
		if tr.EngineID == 1 {
			tr.Written = enginedb.RawWrittenDigest(beats1, legacyQC1, loops1)
		} else if tr.EngineID == 2 {
			tr.Written = []byte("old-mismatch-len")
		}
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}

	pdbPath := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
	origPDB, err := os.ReadFile(pdbPath)
	if err != nil {
		t.Fatal(err)
	}

	// DryRun must report ONLY track 70 (1 change, both comment and cues:anlz fields) without writing.
	dryRes, err := WriteBackCues(ctx, root, WriteBackOptions{DryRun: true})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dryRes.TracksChecked != 70 || dryRes.TracksRead != 70 {
		t.Fatalf("unexpected checked/read counts: %+v", dryRes)
	}
	if dryRes.TracksChanged != 1 || dryRes.FilesUpdated != 0 || len(dryRes.Changes) != 1 {
		t.Fatalf("DryRun expected 1 change and 0 writes, got %+v", dryRes)
	}
	ch := dryRes.Changes[0]
	joined := strings.Join(ch.Fields, ";")
	if ch.EngineID != 70 || !strings.Contains(joined, "comment(") || !strings.Contains(joined, "cues:anlz(") {
		t.Fatalf("DryRun diff attribution unexpected: %+v", ch)
	}
	if afterDryPDB, _ := os.ReadFile(pdbPath); !bytes.Equal(afterDryPDB, origPDB) {
		t.Fatal("DryRun modified export.pdb")
	}

	// Live write-back updates ONLY track 70.
	liveRes, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if liveRes.TracksChanged != 1 || len(liveRes.Changes) != 1 || liveRes.Changes[0].EngineID != 70 {
		t.Fatalf("expected only track 70 written back on live pass, got %+v", liveRes)
	}

	// Dual-library take-over check: if OneLibrary matches m.db metadata and ANLZ matches deck cues
	// while PDB metadata or OneLibrary cues differ on a take-over (Synced == nil), buildChanges must
	// not emit spurious changes.
	kDual := rbstate.FoldPath(testfixture.LargeTrackPath(1))
	dualDir := t.TempDir()
	testfixture.LargeExport(t, dualDir, 1)
	seedFullImport(dualDir)
	dualDB, err := sql.Open("sqlite", filepath.Join(dualDir, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	var bd, td, qc, lp []byte
	if err := dualDB.QueryRow(`SELECT beatData, trackData, quickCues, loops FROM PerformanceData WHERE trackId=1`).Scan(&bd, &td, &qc, &lp); err != nil {
		_ = dualDB.Close()
		t.Fatal(err)
	}
	_ = dualDB.Close()
	anlzRel := "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"
	dualResolver, err := compat.NewResolver(dualDir)
	if err != nil {
		t.Fatal(err)
	}
	dualRow := dbCandidate{
		engineID:         1,
		key:              kDual,
		track:            &rbstate.Track{Key: kDual, Rel: testfixture.LargeTrackPath(1), Written: enginedb.WrittenDigest(bd, qc, lp), Synced: nil, Refs: []rbstate.SourceRef{{Kind: rbstate.DeviceLibrary, ID: 1}, {Kind: rbstate.OneLibrary, ID: 1}}},
		metadata:         rekordbox.Metadata{Genre: "House"},
		beatData:         bd,
		trackData:        td,
		quickCues:        qc,
		loops:            lp,
		writtenDigest:    enginedb.WrittenDigest(bd, qc, lp),
		rawWrittenDigest: enginedb.RawWrittenDigest(bd, qc, lp),
	}
	libs := map[rbstate.LibraryKind]SourceLibrary{
		rbstate.DeviceLibrary: {Kind: rbstate.DeviceLibrary, Lib: &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 1, Path: testfixture.LargeTrackPath(1), AnalysisPath: anlzRel, Genre: "Tech House"}}}},
		rbstate.OneLibrary:    {Kind: rbstate.OneLibrary, Lib: &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 1, Path: testfixture.LargeTrackPath(1), AnalysisPath: anlzRel, Genre: "House", Cues: []rekordbox.Cue{{HotCue: 1, TimeMS: 1234, Extended: true}}}}}},
	}
	_ = fmt.Sprintf // ensure fmt used cleanly
	dualChanges, _, err := buildChanges(ctx, dualResolver, libs, []dbCandidate{dualRow})
	if err != nil || len(dualChanges) != 0 {
		t.Fatalf("dual-library take-over false positive: changes=%+v err=%v", dualChanges, err)
	}
}

func makeLAMEFixtureMP3(xingFrames uint32) []byte {
	const mp3Frame = 417
	raw := make([]byte, 120*mp3Frame)
	for i := 0; i < 120; i++ {
		copy(raw[i*mp3Frame:], []byte{0xff, 0xfb, 0x90, 0})
	}
	copy(raw[36:], "Xing")
	binary.BigEndian.PutUint32(raw[40:], 3)
	binary.BigEndian.PutUint32(raw[44:], xingFrames)
	binary.BigEndian.PutUint32(raw[48:], uint32(len(raw)))
	copy(raw[52:], "LAME3.100")
	raw[73], raw[74], raw[75] = 0x24, 0, 0
	return raw
}

func seedRealRekordboxOneLibraryStick(t *testing.T, root string) (dat1Rel, ext1Rel, dat2Rel, ext2Rel string) {
	t.Helper()
	dat1, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_compact.dat"))
	if err != nil {
		t.Fatal(err)
	}
	ext1, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_compact.ext"))
	if err != nil {
		t.Fatal(err)
	}
	dat2, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_mem_full.dat"))
	if err != nil {
		t.Fatal(err)
	}
	ext2, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_mem_full.ext"))
	if err != nil {
		t.Fatal(err)
	}

	dat1Rel = "PIONEER/USBANLZ/P001/ANLZ0000.DAT"
	ext1Rel = "PIONEER/USBANLZ/P001/ANLZ0000.EXT"
	dat2Rel = "PIONEER/USBANLZ/P002/ANLZ0000.DAT"
	ext2Rel = "PIONEER/USBANLZ/P002/ANLZ0000.EXT"
	testfixture.Write(t, root, dat1Rel, dat1)
	testfixture.Write(t, root, ext1Rel, ext1)
	testfixture.Write(t, root, dat2Rel, dat2)
	testfixture.Write(t, root, ext2Rel, ext2)

	mp3 := makeLAMEFixtureMP3(25000)
	testfixture.Write(t, root, "Contents/FixtureA/Track01.mp3", mp3)
	testfixture.Write(t, root, "Contents/FixtureB/Track02.mp3", mp3)

	onePath := filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db")
	if err := os.MkdirAll(filepath.Dir(onePath), 0o755); err != nil {
		t.Fatal(err)
	}
	oneDB, err := sql.Open("sqlite", onePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = oneDB.Exec(`
		PRAGMA journal_mode=DELETE;
		CREATE TABLE content(
			content_id integer primary key,
			title varchar,
			bpmx100 integer,
			length integer,
			path varchar,
			samplingRate integer,
			djComment varchar,
			analysisDataFilePath varchar,
			hasModified integer,
			cueUpdateCount integer,
			analysisDataUpdateCount integer
		);
		CREATE TABLE cue(
			cue_id integer primary key,
			content_id integer,
			kind integer,
			colorTableIndex integer,
			cueComment varchar,
			isActiveLoop integer,
			beatLoopNumerator integer,
			beatLoopDenominator integer,
			inUsec integer,
			outUsec integer,
			in150FramePerSec integer,
			out150FramePerSec integer
		);
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, analysisDataFilePath, hasModified, cueUpdateCount, analysisDataUpdateCount)
		VALUES
			(1, 'Fixture Track 1', 12800, 650, '/Contents/FixtureA/Track01.mp3', 44100, '', '/PIONEER/USBANLZ/P001/ANLZ0000.DAT', 0, NULL, NULL),
			(2, 'Fixture Track 2', 12400, 650, '/Contents/FixtureB/Track02.mp3', 44100, '', '/PIONEER/USBANLZ/P002/ANLZ0000.DAT', 0, NULL, NULL);
	`)
	_ = oneDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := ConvertAtMount(ctx, root, MountOptions{}); err != nil {
		t.Fatalf("ConvertAtMount: %v", err)
	}
	if _, err := FillInBackground(ctx, root, FillOptions{}); err != nil {
		t.Fatalf("FillInBackground: %v", err)
	}
	if _, err := ConvertAtMount(ctx, root, MountOptions{Publish: true}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return dat1Rel, ext1Rel, dat2Rel, ext2Rel
}

type anlzSecDump struct {
	tag  string
	kind uint32
	raw  []byte
}

func dumpANLZSections(t *testing.T, raw []byte) []anlzSecDump {
	t.Helper()
	if len(raw) < 12 || string(raw[:4]) != "PMAI" {
		t.Fatalf("invalid PMAI header")
	}
	h, n := int(binary.BigEndian.Uint32(raw[4:])), int(binary.BigEndian.Uint32(raw[8:]))
	var out []anlzSecDump
	for off := h; off < n; {
		tag := string(raw[off : off+4])
		sn := int(binary.BigEndian.Uint32(raw[off+8:]))
		sec := append([]byte(nil), raw[off:off+sn]...)
		var kind uint32
		if (tag == "PCOB" || tag == "PCO2") && sn >= 16 {
			kind = binary.BigEndian.Uint32(sec[12:])
		}
		out = append(out, anlzSecDump{tag: tag, kind: kind, raw: sec})
		off += sn
	}
	return out
}

func assertANLZNonTargetUnchanged(t *testing.T, label string, before, after []byte, targetTags map[string]bool) {
	t.Helper()
	bs := dumpANLZSections(t, before)
	as := dumpANLZSections(t, after)
	if len(bs) != len(as) {
		t.Fatalf("%s: section count changed %d -> %d", label, len(bs), len(as))
	}
	for i := range bs {
		if bs[i].tag != as[i].tag {
			t.Fatalf("%s: section[%d] tag %s -> %s", label, i, bs[i].tag, as[i].tag)
		}
		if targetTags[bs[i].tag] {
			continue
		}
		if !bytes.Equal(bs[i].raw, as[i].raw) {
			t.Fatalf("%s: non-target section[%d] %s modified", label, i, bs[i].tag)
		}
	}
}

func buildEngineQuickCuesBlob(slots [8]struct {
	name    string
	samples float64
	color   [4]byte
}) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint64(8))
	for _, s := range slots {
		buf.WriteByte(byte(len(s.name)))
		buf.WriteString(s.name)
		_ = binary.Write(&buf, binary.BigEndian, s.samples)
		buf.Write(s.color[:])
	}
	_ = binary.Write(&buf, binary.BigEndian, float64(-1))
	buf.WriteByte(0)
	_ = binary.Write(&buf, binary.BigEndian, float64(-1))
	blob, err := enginedb.QCompress(buf.Bytes())
	if err != nil {
		panic(err)
	}
	return blob
}

func buildEngineBeatDataBlob(sr, totalSamples float64, def, adj []struct {
	pos      float64
	idx      int64
	numBeats uint32
}, trailingNaN bool) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, sr)
	_ = binary.Write(&buf, binary.BigEndian, totalSamples)
	buf.WriteByte(1)
	writeGrid := func(ms []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}, withNaN bool) {
		cnt := uint64(len(ms))
		if withNaN {
			cnt++
		}
		_ = binary.Write(&buf, binary.BigEndian, cnt)
		for _, m := range ms {
			_ = binary.Write(&buf, binary.LittleEndian, m.pos)
			_ = binary.Write(&buf, binary.LittleEndian, m.idx)
			_ = binary.Write(&buf, binary.LittleEndian, m.numBeats)
			_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
		}
		if withNaN {
			_ = binary.Write(&buf, binary.LittleEndian, math.NaN())
			_ = binary.Write(&buf, binary.LittleEndian, ms[len(ms)-1].idx)
			_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
			_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
		}
	}
	writeGrid(def, false)
	writeGrid(adj, trailingNaN)
	// Engine 5.3.0 writes a 9-byte zero tail.
	buf.Write(make([]byte, 9))
	blob, err := enginedb.QCompress(buf.Bytes())
	if err != nil {
		panic(err)
	}
	return blob
}

func TestWriteBackDefaultCueNameStrippedAndCompactPCP2(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dat1Rel, ext1Rel, _, _ := seedRealRekordboxOneLibraryStick(t, root)
	dat1Path := filepath.Join(root, filepath.FromSlash(dat1Rel))
	ext1Path := filepath.Join(root, filepath.FromSlash(ext1Rel))
	origDAT, err := os.ReadFile(dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	origEXT, err := os.ReadFile(ext1Path)
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Set Pad A with Engine's auto-generated "Cue 1" + pad color (should strip name & emit 48-byte b29=0x03),
	// and Pad B with explicit user name "Vocal Drop" (should preserve name & emit 88+clen b29=0x01).
	var slots [8]struct {
		name    string
		samples float64
		color   [4]byte
	}
	for i := range slots {
		slots[i].samples = -1
	}
	slots[0] = struct {
		name    string
		samples float64
		color   [4]byte
	}{name: "Cue 1", samples: 44100 * 10, color: [4]byte{255, 0xf4, 0xd3, 0x38}}
	slots[1] = struct {
		name    string
		samples float64
		color   [4]byte
	}{name: "Vocal Drop", samples: 44100 * 20, color: [4]byte{255, 0x30, 0x5a, 0xff}}

	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=1`, buildEngineQuickCuesBlob(slots)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+10 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	res, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("unexpected write-back result: %+v", res)
	}

	gotDAT, err := os.ReadFile(dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	gotEXT, err := os.ReadFile(ext1Path)
	if err != nil {
		t.Fatal(err)
	}
	assertANLZNonTargetUnchanged(t, "DAT", origDAT, gotDAT, map[string]bool{"PCOB": true})
	assertANLZNonTargetUnchanged(t, "EXT", origEXT, gotEXT, map[string]bool{"PCOB": true, "PCO2": true})
	if rekordbox.ANLZNeedsCueRepair(gotDAT, false) || rekordbox.ANLZNeedsCueRepair(gotEXT, true) {
		t.Fatal("written DAT/EXT triggered ANLZNeedsCueRepair")
	}

	extSecs := dumpANLZSections(t, gotEXT)
	var pcobKinds, pco2Kinds []uint32
	for _, s := range extSecs {
		switch s.tag {
		case "PCOB":
			pcobKinds = append(pcobKinds, s.kind)
			if cnt := binary.BigEndian.Uint16(s.raw[18:20]); cnt != 0 {
				t.Fatalf("EXT PCOB(%d) should be empty for A/B point cues, got %d entries", s.kind, cnt)
			}
		case "PCO2":
			pco2Kinds = append(pco2Kinds, s.kind)
			if s.kind == 1 {
				cnt := int(binary.BigEndian.Uint16(s.raw[16:18]))
				if cnt != 2 {
					t.Fatalf("EXT PCO2(1) count=%d, want 2", cnt)
				}
				// Entry 0: Pad A compact 48-byte b29=0x03 with RGB tail.
				off := 20
				n0 := int(binary.BigEndian.Uint32(s.raw[off+8:]))
				if n0 != 48 || s.raw[off+29] != 0x03 || binary.BigEndian.Uint32(s.raw[off+40:]) != 0 ||
					!bytes.Equal(s.raw[off+44:off+48], []byte{0x00, 0xf4, 0xd3, 0x38}) {
					t.Fatalf("Pad A not compact 48-byte b29=0x03: len=%d b29=0x%02x tail=%x", n0, s.raw[off+29], s.raw[off+44:off+n0])
				}
				// Entry 1: Pad B full 88+clen b29=0x01 preserving "Vocal Drop".
				off += n0
				n1 := int(binary.BigEndian.Uint32(s.raw[off+8:]))
				clen1 := int(binary.BigEndian.Uint32(s.raw[off+40:]))
				if s.raw[off+29] != 0x01 || clen1 == 0 || n1 != 88+clen1 {
					t.Fatalf("Pad B not full 88+clen b29=0x01: len=%d b29=0x%02x clen=%d", n1, s.raw[off+29], clen1)
				}
			}
		}
	}
	if len(pcobKinds) != 2 || pcobKinds[0] != 1 || pcobKinds[1] != 0 || len(pco2Kinds) != 2 || pco2Kinds[0] != 1 || pco2Kinds[1] != 0 {
		t.Fatalf("EXT cue order wrong: PCOB=%v PCO2=%v", pcobKinds, pco2Kinds)
	}

	// Second pass must be a complete 0-read / 0-write no-op (no OneLibrary color loop).
	res2, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues #2: %v", err)
	}
	if res2.TracksRead != 0 || res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
		t.Fatalf("second pass was not a no-op: %+v", res2)
	}
}

func TestWriteBackSelfRepairsSwappedEngineXFilesOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dat1Rel, ext1Rel, dat2Rel, ext2Rel := seedRealRekordboxOneLibraryStick(t, root)
	dat1Path := filepath.Join(root, filepath.FromSlash(dat1Rel))
	ext1Path := filepath.Join(root, filepath.FromSlash(ext1Rel))
	dat2Path := filepath.Join(root, filepath.FromSlash(dat2Rel))
	ext2Path := filepath.Join(root, filepath.FromSlash(ext2Rel))

	origDAT1, err := os.ReadFile(dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	origEXT1, err := os.ReadFile(ext1Path)
	if err != nil {
		t.Fatal(err)
	}
	origDAT2, err := os.ReadFile(dat2Path)
	if err != nil {
		t.Fatal(err)
	}
	origEXT2, err := os.ReadFile(ext2Path)
	if err != nil {
		t.Fatal(err)
	}

	// Construct a swapped legacy EngineX pair (PCOB(0) before PCOB(1), PCO2(0) before PCO2(1),
	// non-empty .EXT PCOB(1) with Hot A, and b29=0 PCP2 with Engine default "Cue 1").
	makeLegacyBrokenPair := func(datRaw, extRaw []byte) ([]byte, []byte) {
		buildBroken := func(raw []byte, isExt bool) []byte {
			h := int(binary.BigEndian.Uint32(raw[4:]))
			secs := dumpANLZSections(t, raw)
			// Build PCOB(0) empty + PCOB(1) with Hot A at 12000ms.
			pcob0 := make([]byte, 24)
			copy(pcob0, "PCOB")
			binary.BigEndian.PutUint32(pcob0[4:], 24)
			binary.BigEndian.PutUint32(pcob0[8:], 24)
			binary.BigEndian.PutUint32(pcob0[12:], 0)
			binary.BigEndian.PutUint32(pcob0[20:], 0xffffffff)

			pcpt := make([]byte, 56)
			copy(pcpt, "PCPT")
			binary.BigEndian.PutUint32(pcpt[4:], 28)
			binary.BigEndian.PutUint32(pcpt[8:], 56)
			binary.BigEndian.PutUint32(pcpt[12:], 1)
			binary.BigEndian.PutUint32(pcpt[20:], 0x10000)
			binary.BigEndian.PutUint16(pcpt[24:], 0xffff)
			binary.BigEndian.PutUint16(pcpt[26:], 0xffff)
			pcpt[28], pcpt[30], pcpt[31] = 1, 3, 0xe8
			binary.BigEndian.PutUint32(pcpt[32:], 12000)
			binary.BigEndian.PutUint32(pcpt[36:], 0xffffffff)
			pcob1 := append(make([]byte, 24), pcpt...)
			copy(pcob1, "PCOB")
			binary.BigEndian.PutUint32(pcob1[4:], 24)
			binary.BigEndian.PutUint32(pcob1[8:], uint32(len(pcob1)))
			binary.BigEndian.PutUint32(pcob1[12:], 1)
			binary.BigEndian.PutUint16(pcob1[18:], 1)
			binary.BigEndian.PutUint32(pcob1[20:], 0xffffffff)

			var pco20, pco21 []byte
			if isExt {
				pco20 = make([]byte, 20)
				copy(pco20, "PCO2")
				binary.BigEndian.PutUint32(pco20[4:], 20)
				binary.BigEndian.PutUint32(pco20[8:], 20)
				binary.BigEndian.PutUint32(pco20[12:], 0)

				// Legacy EngineX b29=0 PCP2 with "Cue 1\x00" (12 UTF-16BE bytes) + 16-byte tail (ColorCode=33 + RGB).
				comment := []byte{0, 'C', 0, 'u', 0, 'e', 0, ' ', 0, '1', 0, 0}
				pcp2 := make([]byte, 44+len(comment)+16)
				copy(pcp2, "PCP2")
				binary.BigEndian.PutUint32(pcp2[4:], 16)
				binary.BigEndian.PutUint32(pcp2[8:], uint32(len(pcp2)))
				binary.BigEndian.PutUint32(pcp2[12:], 1)
				pcp2[16], pcp2[18], pcp2[19] = 1, 3, 0xe8
				binary.BigEndian.PutUint32(pcp2[20:], 12000)
				binary.BigEndian.PutUint32(pcp2[24:], 0xffffffff)
				pcp2[29] = 0x00 // buggy legacy byte 29
				binary.BigEndian.PutUint32(pcp2[40:], uint32(len(comment)))
				copy(pcp2[44:], comment)
				q := 44 + len(comment)
				pcp2[q], pcp2[q+1], pcp2[q+2], pcp2[q+3] = 33, 0xf4, 0xd3, 0x38

				pco21 = append(make([]byte, 20), pcp2...)
				copy(pco21, "PCO2")
				binary.BigEndian.PutUint32(pco21[4:], 20)
				binary.BigEndian.PutUint32(pco21[8:], uint32(len(pco21)))
				binary.BigEndian.PutUint32(pco21[12:], 1)
				binary.BigEndian.PutUint16(pco21[16:], 1)
			}

			var body [][]byte
			inserted := false
			for _, s := range secs {
				if s.tag == "PCOB" || s.tag == "PCO2" {
					if !inserted {
						body = append(body, pcob0, pcob1)
						if isExt {
							body = append(body, pco20, pco21)
						}
						inserted = true
					}
					continue
				}
				body = append(body, s.raw)
			}
			total := h
			for _, b := range body {
				total += len(b)
			}
			out := make([]byte, total)
			copy(out, raw[:h])
			binary.BigEndian.PutUint32(out[8:], uint32(total))
			pos := h
			for _, b := range body {
				copy(out[pos:], b)
				pos += len(b)
			}
			return out
		}
		return buildBroken(datRaw, false), buildBroken(extRaw, true)
	}

	brokenDAT1, brokenEXT1 := makeLegacyBrokenPair(origDAT1, origEXT1)
	brokenDAT2, brokenEXT2 := makeLegacyBrokenPair(origDAT2, origEXT2)
	if !rekordbox.ANLZNeedsCueRepair(brokenDAT1, false) || !rekordbox.ANLZNeedsCueRepair(brokenEXT1, true) {
		t.Fatal("test fixture broken pair did not trigger ANLZNeedsCueRepair")
	}

	// Track 1: simulate previously EngineX-modified file (original exists in .enginex-backup/).
	bak1DAT := filepath.Join(root, ".enginex-backup", "2026-11-25", filepath.FromSlash(dat1Rel))
	bak1EXT := filepath.Join(root, ".enginex-backup", "2026-11-25", filepath.FromSlash(ext1Rel))
	testfixture.Write(t, root, filepath.ToSlash(filepath.Join(".enginex-backup", "2026-11-25", dat1Rel)), origDAT1)
	testfixture.Write(t, root, filepath.ToSlash(filepath.Join(".enginex-backup", "2026-11-25", ext1Rel)), origEXT1)
	if err := os.WriteFile(dat1Path, brokenDAT1, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ext1Path, brokenEXT1, 0o644); err != nil {
		t.Fatal(err)
	}

	// Track 2: swapped file NOT in .enginex-backup/ (third-party file EngineX never wrote).
	if err := os.WriteFile(dat2Path, brokenDAT2, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ext2Path, brokenEXT2, 0o644); err != nil {
		t.Fatal(err)
	}

	// DryRun reports ONLY Track 1 with cues:anlz-repair and writes nothing.
	dryRes, err := WriteBackCues(ctx, root, WriteBackOptions{DryRun: true})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dryRes.TracksChanged != 1 || dryRes.FilesUpdated != 0 || len(dryRes.Changes) != 1 ||
		dryRes.Changes[0].EngineID != 1 || strings.Join(dryRes.Changes[0].Fields, ",") != "cues:anlz-repair" {
		t.Fatalf("unexpected DryRun repair result: %+v", dryRes)
	}
	if cur, _ := os.ReadFile(dat1Path); !bytes.Equal(cur, brokenDAT1) {
		t.Fatal("DryRun modified DAT1")
	}

	// Live pass repairs ONLY Track 1, leaves Track 2 and first-original backups untouched.
	res, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues repair: %v", err)
	}
	if res.TracksChanged != 1 || res.FilesUpdated != 2 || res.OneLibraryUpdated {
		t.Fatalf("unexpected live repair result: %+v", res)
	}

	repairedDAT1, err := os.ReadFile(dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	repairedEXT1, err := os.ReadFile(ext1Path)
	if err != nil {
		t.Fatal(err)
	}
	if rekordbox.ANLZNeedsCueRepair(repairedDAT1, false) || rekordbox.ANLZNeedsCueRepair(repairedEXT1, true) {
		t.Fatal("repaired Track 1 still needs cue repair")
	}
	assertANLZNonTargetUnchanged(t, "repaired DAT1", origDAT1, repairedDAT1, map[string]bool{"PCOB": true})
	assertANLZNonTargetUnchanged(t, "repaired EXT1", origEXT1, repairedEXT1, map[string]bool{"PCOB": true, "PCO2": true})
	aDat, err := rekordbox.ParseAnalysis(repairedDAT1)
	if err != nil {
		t.Fatal(err)
	}
	aExt, err := rekordbox.ParseAnalysis(repairedEXT1)
	if err != nil {
		t.Fatal(err)
	}
	aDat.Merge(aExt)
	if len(aDat.Cues) != 1 || aDat.Cues[0].HotCue != 1 || aDat.Cues[0].TimeMS != 12000 || aDat.Cues[0].Name != "" || aDat.Cues[0].Color != ([4]byte{255, 0xf4, 0xd3, 0x38}) {
		t.Fatalf("repaired cues mismatch: %+v", aDat.Cues)
	}

	// First-original backups must remain byte-for-byte untouched.
	if b, _ := os.ReadFile(bak1DAT); !bytes.Equal(b, origDAT1) {
		t.Fatal("first-original backup DAT1 was overwritten")
	}
	if b, _ := os.ReadFile(bak1EXT); !bytes.Equal(b, origEXT1) {
		t.Fatal("first-original backup EXT1 was overwritten")
	}
	// Third-party Track 2 (not in .enginex-backup/) must remain byte-for-byte untouched.
	if cur, _ := os.ReadFile(dat2Path); !bytes.Equal(cur, brokenDAT2) {
		t.Fatal("third-party Track 2 DAT was modified")
	}
	if cur, _ := os.ReadFile(ext2Path); !bytes.Equal(cur, brokenEXT2) {
		t.Fatal("third-party Track 2 EXT was modified")
	}

	// Subsequent pass must be a 0-change no-op.
	res2, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues post-repair: %v", err)
	}
	if res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
		t.Fatalf("post-repair pass not a no-op: %+v", res2)
	}
}

func TestWriteBackBeatGridEditRoundTripAndByteIdentity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dat1Rel, ext1Rel, _, _ := seedRealRekordboxOneLibraryStick(t, root)
	dat1Path := filepath.Join(root, filepath.FromSlash(dat1Rel))
	ext1Path := filepath.Join(root, filepath.FromSlash(ext1Rel))
	origDAT, err := os.ReadFile(dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	origEXT, err := os.ReadFile(ext1Path)
	if err != nil {
		t.Fatal(err)
	}

	// Expected LAME MP3 RekordboxOffset for our fixture MP3 (576 + 529 = 1105 samples).
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	offset, ok := trackRekordboxOffset(r, "Contents/FixtureA/Track01.mp3")
	if !ok || offset <= 0 {
		t.Fatalf("expected positive LAME MP3 offset, got offset=%v ok=%v", offset, ok)
	}

	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Edit Track 1 beatData: 125.00 BPM (480ms = 21168 samples/beat at 44.1kHz),
	// starting at beat 0 = 5000.25 samples (engine) -> with offset added = (5000.25 + offset)/44100 s,
	// plus trailing Engine 5.3.0 NaN sentinel marker.
	const sr = 44100.0
	const step = 60.0 * sr / 125.0 // 21168.0 samples/beat
	defGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{
		{pos: 0, idx: 0, numBeats: 16},
		{pos: 16 * 20671.875, idx: 16, numBeats: 0},
	}
	adjGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{
		{pos: 5000.25 - 4*step, idx: -4, numBeats: 4}, // pre-zero bar anchor (must be clipped cleanly)
		{pos: 5000.25, idx: 0, numBeats: 20},
		{pos: 5000.25 + 20*step, idx: 20, numBeats: 0},
	}
	editedBeats := buildEngineBeatDataBlob(sr, 20*sr, defGrid, adjGrid, true)
	if _, err := db.Exec(`UPDATE PerformanceData SET beatData=? WHERE trackId=1`, editedBeats); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+10 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	res, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues grid: %v", err)
	}
	if res.TracksChanged != 1 || !res.OneLibraryUpdated || len(res.Changes) != 1 || !strings.Contains(strings.Join(res.Changes[0].Fields, ","), "grid:anlz(") {
		t.Fatalf("unexpected grid write-back result: %+v", res)
	}

	gotDAT, err := os.ReadFile(dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	gotEXT, err := os.ReadFile(ext1Path)
	if err != nil {
		t.Fatal(err)
	}
	// Every section other than PQTZ (.DAT) and PQT2 (.EXT) must be byte-for-byte identical.
	assertANLZNonTargetUnchanged(t, "grid DAT", origDAT, gotDAT, map[string]bool{"PQTZ": true})
	assertANLZNonTargetUnchanged(t, "grid EXT", origEXT, gotEXT, map[string]bool{"PQT2": true})

	aDat, err := rekordbox.ParseAnalysis(gotDAT)
	if err != nil {
		t.Fatal(err)
	}
	aExt, err := rekordbox.ParseAnalysis(gotEXT)
	if err != nil {
		t.Fatal(err)
	}
	aDat.Merge(aExt)
	if len(aDat.Beats) != 21 {
		t.Fatalf("got %d beats, want 21 (indices 0..20)", len(aDat.Beats))
	}
	wantFirstUS := uint64(math.Round((5000.25 + offset) * 1e6 / sr))
	gotFirstUS := uint64(aDat.Beats[0].TimeMS)*1000 + uint64(aDat.Beats[0].SubUS)
	if aDat.Beats[0].Number != 1 || math.Abs(aDat.Beats[0].BPM-125.00) > 0.005 || gotFirstUS != wantFirstUS {
		t.Fatalf("beat[0] got=%+v (%d us), want num=1 bpm=125.00 time=%d us", aDat.Beats[0], gotFirstUS, wantFirstUS)
	}
	wantLastUS := uint64(math.Round((5000.25 + 20*step + offset) * 1e6 / sr))
	gotLastUS := uint64(aDat.Beats[20].TimeMS)*1000 + uint64(aDat.Beats[20].SubUS)
	if aDat.Beats[20].Number != 1 || gotLastUS != wantLastUS {
		t.Fatalf("beat[20] got=%+v (%d us), want num=1 time=%d us", aDat.Beats[20], gotLastUS, wantLastUS)
	}

	// Verify OneLibrary change-tracking marks.
	oneDB, err := sql.Open("sqlite", filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer oneDB.Close()
	var hasMod, bpm100 int
	var cueUpd, anlzUpd sql.NullInt64
	if err := oneDB.QueryRow(`SELECT hasModified, bpmx100, cueUpdateCount, analysisDataUpdateCount FROM content WHERE content_id=1`).Scan(&hasMod, &bpm100, &cueUpd, &anlzUpd); err != nil {
		t.Fatal(err)
	}
	if hasMod != 1 || bpm100 != 12500 || cueUpd.Valid || !anlzUpd.Valid || anlzUpd.Int64 != 1 {
		t.Fatalf("OneLibrary marks wrong: hasMod=%d bpm100=%d cueUpd=%v anlzUpd=%v", hasMod, bpm100, cueUpd, anlzUpd)
	}

	// Pass 2: untouched -> 0 reads, 0 changes, 0 writes.
	res2, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues #2: %v", err)
	}
	if res2.TracksRead != 0 || res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
		t.Fatalf("second pass after grid write-back not a no-op: %+v", res2)
	}

	// Pass 3: lastEditTime bumps again with same beatData -> 0 writes, export not opened!
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+10 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	res3, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues #3: %v", err)
	}
	if res3.TracksChanged != 0 || res3.FilesUpdated != 0 || res3.PhasesMS["decrypt"] != 0 {
		t.Fatalf("bumped lastEditTime without grid change opened export or wrote: %+v", res3)
	}
}

func TestWriteBackCombinedGridAndCueEditOnePass(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dat1Rel, ext1Rel, _, _ := seedRealRekordboxOneLibraryStick(t, root)
	dat1Path := filepath.Join(root, filepath.FromSlash(dat1Rel))
	ext1Path := filepath.Join(root, filepath.FromSlash(ext1Rel))
	origDAT, err := os.ReadFile(dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	origEXT, err := os.ReadFile(ext1Path)
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Edit BOTH grid (126.00 BPM) AND hot cue A in one pass.
	const sr = 44100.0
	step := 60.0 * sr / 126.0
	defGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{{0, 0, 16}, {16 * 20671.875, 16, 0}}
	adjGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{{2205.0, 0, 16}, {2205.0 + 16*step, 16, 0}}

	var slots [8]struct {
		name    string
		samples float64
		color   [4]byte
	}
	for i := range slots {
		slots[i].samples = -1
	}
	slots[0] = struct {
		name    string
		samples float64
		color   [4]byte
	}{name: "Drop A", samples: 44100 * 5, color: [4]byte{255, 0x30, 0x5a, 0xff}}

	if _, err := db.Exec(`UPDATE PerformanceData SET beatData=?, quickCues=? WHERE trackId=1`,
		buildEngineBeatDataBlob(sr, 20*sr, defGrid, adjGrid, false),
		buildEngineQuickCuesBlob(slots)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+10 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	res, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues combined: %v", err)
	}
	if res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("unexpected combined result: %+v", res)
	}
	joined := strings.Join(res.Changes[0].Fields, ",")
	if !strings.Contains(joined, "cues:anlz(") || !strings.Contains(joined, "grid:anlz(") {
		t.Fatalf("combined fields missing cues or grid: %v", res.Changes[0].Fields)
	}

	gotDAT, err := os.ReadFile(dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	gotEXT, err := os.ReadFile(ext1Path)
	if err != nil {
		t.Fatal(err)
	}
	assertANLZNonTargetUnchanged(t, "combined DAT", origDAT, gotDAT, map[string]bool{"PCOB": true, "PQTZ": true})
	assertANLZNonTargetUnchanged(t, "combined EXT", origEXT, gotEXT, map[string]bool{"PCOB": true, "PCO2": true, "PQT2": true})

	aDat, err := rekordbox.ParseAnalysis(gotDAT)
	if err != nil {
		t.Fatal(err)
	}
	aExt, err := rekordbox.ParseAnalysis(gotEXT)
	if err != nil {
		t.Fatal(err)
	}
	aDat.Merge(aExt)
	if len(aDat.Beats) != 17 || math.Abs(aDat.Beats[0].BPM-126.00) > 0.005 {
		t.Fatalf("combined beats mismatch: len=%d bpm=%v", len(aDat.Beats), aDat.Beats[0].BPM)
	}
	if len(aDat.Cues) != 1 || aDat.Cues[0].HotCue != 1 || aDat.Cues[0].Name != "Drop A" {
		t.Fatalf("combined cues mismatch: %+v", aDat.Cues)
	}

	oneDB, err := sql.Open("sqlite", filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer oneDB.Close()
	var hasMod, bpm100 int
	var cueUpd, anlzUpd sql.NullInt64
	if err := oneDB.QueryRow(`SELECT hasModified, bpmx100, cueUpdateCount, analysisDataUpdateCount FROM content WHERE content_id=1`).Scan(&hasMod, &bpm100, &cueUpd, &anlzUpd); err != nil {
		t.Fatal(err)
	}
	if hasMod != 1 || bpm100 != 12600 || !cueUpd.Valid || cueUpd.Int64 != 1 || !anlzUpd.Valid || anlzUpd.Int64 != 1 {
		t.Fatalf("OneLibrary combined marks wrong: hasMod=%d bpm100=%d cueUpd=%v anlzUpd=%v", hasMod, bpm100, cueUpd, anlzUpd)
	}

	// Second pass is a no-op.
	res2, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues combined #2: %v", err)
	}
	if res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
		t.Fatalf("combined second pass not a no-op: %+v", res2)
	}
}

func TestWriteBackUntouchedTrackZeroGridWrites(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dat1Rel, ext1Rel, dat2Rel, ext2Rel := seedRealRekordboxOneLibraryStick(t, root)
	dat1Before, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(dat1Rel)))
	ext1Before, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(ext1Rel)))
	dat2Before, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(dat2Rel)))
	ext2Before, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(ext2Rel)))

	// Pass 1: completely untouched stick -> 0 reads, 0 changes, 0 writes.
	res1, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues untouched: %v", err)
	}
	if res1.TracksChanged != 0 || res1.FilesUpdated != 0 {
		t.Fatalf("untouched stick wrote files: %+v", res1)
	}

	// Pass 2: simulate Engine auto-analysis on a track with no bridge Written baseline
	// (default_grid == adjusted_grid) and Synced == nil (legacy take-over).
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range snap.Tracks {
		tr.Synced = nil
		if tr.EngineID == 1 {
			tr.Written = nil
		}
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Replace track 1 beatData with an auto-analyzed grid where adjusted == default (different BPM from ANLZ).
	autoGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{{1000, 0, 16}, {1000 + 16*21168, 16, 0}}
	if _, err := db.Exec(`UPDATE PerformanceData SET beatData=? WHERE trackId=1`, buildEngineBeatDataBlob(44100, 20*44100, autoGrid, autoGrid, false)); err != nil {
		t.Fatal(err)
	}

	res2, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues take-over auto-analysis: %v", err)
	}
	if res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
		t.Fatalf("untouched / auto-analyzed tracks triggered writes: %+v", res2)
	}
	for label, pair := range map[string][2]string{
		"DAT1": {string(dat1Before), filepath.Join(root, filepath.FromSlash(dat1Rel))},
		"EXT1": {string(ext1Before), filepath.Join(root, filepath.FromSlash(ext1Rel))},
		"DAT2": {string(dat2Before), filepath.Join(root, filepath.FromSlash(dat2Rel))},
		"EXT2": {string(ext2Before), filepath.Join(root, filepath.FromSlash(ext2Rel))},
	} {
		after, _ := os.ReadFile(pair[1])
		if !bytes.Equal([]byte(pair[0]), after) {
			t.Fatalf("%s was modified on untouched pass", label)
		}
	}
}
