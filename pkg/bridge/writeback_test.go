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
