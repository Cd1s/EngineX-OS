package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	// Set Pad A with Engine's auto-generated "Cue 1" + default Pad 1 color (must strip name & default
	// color -> 44-byte uncolored compact b29=0x03 matching compact rekordbox oracle), Pad B with explicit user name
	// "Vocal Drop" (-> 88+clen b29=0x01), and Pad C with "Cue 3" + custom non-default RGB (-> 48-byte
	// colored compact b29=0x03).
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
	slots[2] = struct {
		name    string
		samples float64
		color   [4]byte
	}{name: "Cue 3", samples: 44100 * 30, color: [4]byte{255, 0xff, 0x00, 0x17}}

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
				t.Fatalf("EXT PCOB(%d) should be empty for A/B/C point cues, got %d entries", s.kind, cnt)
			}
		case "PCO2":
			pco2Kinds = append(pco2Kinds, s.kind)
			if s.kind == 1 {
				cnt := int(binary.BigEndian.Uint16(s.raw[16:18]))
				if cnt != 3 {
					t.Fatalf("EXT PCO2(1) count=%d, want 3", cnt)
				}
				// Entry 0: Pad A compact 44-byte b29=0x03 (default name & default Pad 1 color stripped).
				off := 20
				n0 := int(binary.BigEndian.Uint32(s.raw[off+8:]))
				if n0 != 44 || s.raw[off+29] != 0x03 || binary.BigEndian.Uint32(s.raw[off+40:]) != 0 {
					t.Fatalf("Pad A not compact 44-byte b29=0x03: len=%d b29=0x%02x bytes=%x", n0, s.raw[off+29], s.raw[off:off+n0])
				}
				// Entry 1: Pad B full 88+clen b29=0x01 preserving "Vocal Drop".
				off += n0
				n1 := int(binary.BigEndian.Uint32(s.raw[off+8:]))
				clen1 := int(binary.BigEndian.Uint32(s.raw[off+40:]))
				if s.raw[off+29] != 0x01 || clen1 == 0 || n1 != 88+clen1 {
					t.Fatalf("Pad B not full 88+clen b29=0x01: len=%d b29=0x%02x clen=%d", n1, s.raw[off+29], clen1)
				}
				// Entry 2: Pad C compact 48-byte b29=0x03 preserving custom RGB tail.
				off += n1
				n2 := int(binary.BigEndian.Uint32(s.raw[off+8:]))
				if n2 != 48 || s.raw[off+29] != 0x03 || binary.BigEndian.Uint32(s.raw[off+40:]) != 0 ||
					!bytes.Equal(s.raw[off+44:off+48], []byte{0x00, 0xff, 0x00, 0x17}) {
					t.Fatalf("Pad C not compact 48-byte b29=0x03: len=%d b29=0x%02x tail=%x", n2, s.raw[off+29], s.raw[off+44:off+n2])
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

	// Track 3: backed-up swapped file holding the 8x23ms content-76 phantom-cue signature
	// (PM 19:58 item 1: must be cleaned across ANLZ and exportLibrary.db, resetting content marks).
	dat3Rel := "PIONEER/USBANLZ/P003/ANLZ0000.DAT"
	ext3Rel := "PIONEER/USBANLZ/P003/ANLZ0000.EXT"
	dat3Path := filepath.Join(root, filepath.FromSlash(dat3Rel))
	ext3Path := filepath.Join(root, filepath.FromSlash(ext3Rel))
	testfixture.Write(t, root, "Contents/FixtureC/Track03.mp3", makeLAMEFixtureMP3(25000))
	emptyDAT3, _, err := rekordbox.RewriteANLZCues(rewritePPTHSection(t, origDAT1, "/Contents/FixtureC/Track03.mp3"), nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	emptyEXT3, _, err := rekordbox.RewriteANLZCues(rewritePPTHSection(t, origEXT1, "/Contents/FixtureC/Track03.mp3"), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, filepath.ToSlash(filepath.Join(".enginex-backup", "2026-11-25", dat3Rel)), emptyDAT3)
	testfixture.Write(t, root, filepath.ToSlash(filepath.Join(".enginex-backup", "2026-11-25", ext3Rel)), emptyEXT3)
	makeSwappedEight23ms := func(raw []byte, isExt bool) []byte {
		var eight []rekordbox.Cue
		for pad := uint32(1); pad <= 8; pad++ {
			eight = append(eight, rekordbox.Cue{
				HotCue: pad, TimeMS: 23, Name: fmt.Sprintf("Cue %d", pad), ColorCode: byte(pad),
			})
		}
		rewritten, _, rerr := rekordbox.RewriteANLZCues(raw, eight, isExt, false)
		if rerr != nil {
			t.Fatal(rerr)
		}
		h := int(binary.BigEndian.Uint32(rewritten[4:]))
		secs := dumpANLZSections(t, rewritten)
		var pcob0, pcob1, pco20, pco21 []byte
		var body [][]byte
		inserted := false
		for _, s := range secs {
			switch {
			case s.tag == "PCOB" && s.kind == 0:
				pcob0 = s.raw
			case s.tag == "PCOB" && s.kind == 1:
				pcob1 = s.raw
			case s.tag == "PCO2" && s.kind == 0:
				pco20 = s.raw
			case s.tag == "PCO2" && s.kind == 1:
				pco21 = s.raw
			default:
				if !inserted && s.tag != "PPTH" && s.tag != "PVBR" && s.tag != "PQTZ" && s.tag != "PWAV" && s.tag != "PWV2" && s.tag != "PWV3" {
					inserted = true
				}
				body = append(body, s.raw)
			}
		}
		// Place swapped type 0 before type 1 at end so ANLZNeedsCueRepair triggers.
		body = append(body, pcob0, pcob1)
		if isExt {
			body = append(body, pco20, pco21)
		}
		total := h
		for _, b := range body {
			total += len(b)
		}
		out := make([]byte, total)
		copy(out, rewritten[:h])
		binary.BigEndian.PutUint32(out[8:], uint32(total))
		pos := h
		for _, b := range body {
			copy(out[pos:], b)
			pos += len(b)
		}
		return out
	}
	brokenDAT3 := makeSwappedEight23ms(emptyDAT3, false)
	brokenEXT3 := makeSwappedEight23ms(emptyEXT3, true)
	testfixture.Write(t, root, dat3Rel, brokenDAT3)
	testfixture.Write(t, root, ext3Rel, brokenEXT3)
	onePath3 := filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db")
	oneDB3, err := sql.Open("sqlite", onePath3)
	if err != nil {
		t.Fatal(err)
	}
	_, err = oneDB3.Exec(`
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, analysisDataFilePath, hasModified, cueUpdateCount, analysisDataUpdateCount)
		VALUES (3, 'Fixture Track 3', 12800, 650, '/Contents/FixtureC/Track03.mp3', 44100, '', '/PIONEER/USBANLZ/P003/ANLZ0000.DAT', 1, 1, NULL);
		INSERT INTO cue(cue_id, content_id, kind, colorTableIndex, cueComment, isActiveLoop, beatLoopNumerator, beatLoopDenominator, inUsec, outUsec, in150FramePerSec, out150FramePerSec)
		VALUES
			(301, 3, 1, 1, 'Cue 1', 0, 0, 0, 23000, -1, 3, -1),
			(302, 3, 2, 2, 'Cue 2', 0, 0, 0, 23000, -1, 3, -1),
			(303, 3, 3, 3, 'Cue 3', 0, 0, 0, 23000, -1, 3, -1),
			(304, 3, 4, 4, 'Cue 4', 0, 0, 0, 23000, -1, 3, -1),
			(305, 3, 5, 5, 'Cue 5', 0, 0, 0, 23000, -1, 3, -1),
			(306, 3, 6, 6, 'Cue 6', 0, 0, 0, 23000, -1, 3, -1),
			(307, 3, 7, 7, 'Cue 7', 0, 0, 0, 23000, -1, 3, -1),
			(308, 3, 8, 8, 'Cue 8', 0, 0, 0, 23000, -1, 3, -1);
	`)
	_ = oneDB3.Close()
	if err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	snap3, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	snap3.Tracks["contents/fixturec/track03.mp3"] = &rbstate.Track{
		Key:          "contents/fixturec/track03.mp3",
		EngineID:     3,
		Rel:          "Contents/FixtureC/Track03.mp3",
		AnalysisPath: dat3Rel,
		Refs:         []rbstate.SourceRef{{Kind: rbstate.OneLibrary, ID: 3}},
	}
	if _, snap3.Header.Stamp, err = exportNow(root, snap3.Header.Layout); err != nil {
		t.Fatal(err)
	}
	r3, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	snap3.Header.Sources, _ = FindSources(r3, root)
	if err := snap3.Save(dbDir); err != nil {
		t.Fatal(err)
	}

	// DryRun reports Track 1 (cues:anlz-repair) and Track 3 (cues:phantom-cleanup), excludes third-party Track 2, and writes nothing.
	dryRes, err := WriteBackCues(ctx, root, WriteBackOptions{DryRun: true})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dryRes.TracksChanged != 2 || dryRes.FilesUpdated != 0 || len(dryRes.Changes) != 2 ||
		dryRes.Changes[0].EngineID != 1 || strings.Join(dryRes.Changes[0].Fields, ",") != "cues:anlz-repair" ||
		dryRes.Changes[1].EngineID != 3 || strings.Join(dryRes.Changes[1].Fields, ",") != "cues:phantom-cleanup" {
		t.Fatalf("unexpected DryRun repair result: %+v", dryRes)
	}
	if cur, _ := os.ReadFile(dat1Path); !bytes.Equal(cur, brokenDAT1) {
		t.Fatal("DryRun modified DAT1")
	}

	// Live pass repairs Track 1 AND strips Track 3's phantom set across ANLZ + exportLibrary.db,
	// leaving Track 2 and first-original backups untouched.
	res, err := WriteBackCues(ctx, root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues repair: %v", err)
	}
	if res.TracksChanged != 2 || res.FilesUpdated != 5 || !res.OneLibraryUpdated {
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
	if len(aDat.Cues) != 1 || aDat.Cues[0].HotCue != 1 || aDat.Cues[0].TimeMS != 12000 || aDat.Cues[0].Name != "" || aDat.Cues[0].Color != ([4]byte{}) || aDat.Cues[0].ColorCode != 0 {
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
	// Phantom-set Track 3 (8x23ms) must now be restored to clean empty cues + DB marks reset.
	if cur, _ := os.ReadFile(dat3Path); !bytes.Equal(cur, emptyDAT3) {
		t.Fatal("phantom Track 3 DAT was not restored to clean original")
	}
	if cur, _ := os.ReadFile(ext3Path); !bytes.Equal(cur, emptyEXT3) {
		t.Fatal("phantom Track 3 EXT was not restored to clean original")
	}
	oneCheck3, err := sql.Open("sqlite", onePath3)
	if err != nil {
		t.Fatal(err)
	}
	var cnt3, mod3 int
	var cueUpd3 sql.NullInt64
	if err := oneCheck3.QueryRow(`SELECT (SELECT count(*) FROM cue WHERE content_id=3), COALESCE(hasModified,0), cueUpdateCount FROM content WHERE content_id=3`).Scan(&cnt3, &mod3, &cueUpd3); err != nil {
		_ = oneCheck3.Close()
		t.Fatal(err)
	}
	_ = oneCheck3.Close()
	if cnt3 != 0 || mod3 != 0 || cueUpd3.Valid {
		t.Fatalf("Track 3 OneLibrary rows/marks not cleaned: cues=%d hasModified=%d cueUpdateCount=%v", cnt3, mod3, cueUpd3)
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

// TestWriteBackZeroWritesOnImportedRepairedTrack reproduces the PM 17:20 P0 bug where a repaired
// track (minimal 44-byte PCP2 with no name/color) imported into Engine was re-broken within a
// minute after mount update + CueWatcher, and proves 0 writes across mount update, forced
// candidate reads, and CueWatcher passes, while genuine deck edits still write the 44-byte layout.
func TestWriteBackZeroWritesOnImportedRepairedTrack(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	t.Cleanup(SetCueRunDirForTest(t.TempDir()))
	ctx := context.Background()
	parentDir := t.TempDir()
	root := filepath.Join(parentDir, "STICK")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	rawDAT, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_compact.dat"))
	if err != nil {
		t.Fatal(err)
	}
	rawEXT, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_compact.ext"))
	if err != nil {
		t.Fatal(err)
	}
	// Repaired minimal 44-byte layout with Hot Cue A at 28264 ms (matching compact rekordbox oracle).
	const cueMS = uint32(28264)
	repDAT, _, err := rekordbox.RewriteANLZCues(rawDAT, []rekordbox.Cue{{HotCue: 1, TimeMS: cueMS}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	repEXT, _, err := rekordbox.RewriteANLZCues(rawEXT, []rekordbox.Cue{{HotCue: 1, TimeMS: cueMS}}, true, false)
	if err != nil {
		t.Fatal(err)
	}

	datRel := "PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT"
	extRel := "PIONEER/USBANLZ/P001/00000001/ANLZ0000.EXT"
	testfixture.Write(t, root, datRel, repDAT)
	testfixture.Write(t, root, extRel, repEXT)
	testfixture.Write(t, root, "PIONEER/Artwork/00001/a1.jpg", []byte("fakejpg1"))
	testfixture.Write(t, root, "PIONEER/Artwork/00001/a2.jpg", []byte("fakejpg2"))

	mp3 := makeLAMEFixtureMP3(25000)
	testfixture.Write(t, root, "Contents/FixtureA/Track01.mp3", mp3)
	info, err := compat.AudioHeader(filepath.Join(root, "Contents", "FixtureA", "Track01.mp3"))
	if err != nil || info.RekordboxOffset() <= 0 {
		t.Fatalf("AudioHeader: info=%+v err=%v", info, err)
	}
	rbOffset := float64(info.RekordboxOffset())

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
			image_path varchar,
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
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, image_path, analysisDataFilePath, hasModified, cueUpdateCount, analysisDataUpdateCount)
		VALUES (1, 'Fixture Track 1', 12800, 650, '/Contents/FixtureA/Track01.mp3', 44100, '', '/PIONEER/Artwork/00001/a1.jpg', '/PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT', 1, 1, NULL);
		INSERT INTO cue(cue_id, content_id, kind, colorTableIndex, cueComment, isActiveLoop, beatLoopNumerator, beatLoopDenominator, inUsec, outUsec, in150FramePerSec, out150FramePerSec)
		VALUES (1, 1, 1, 33, 'Cue 1', 0, 0, 0, 28264000, -1, 4240, -1);
	`)
	_ = oneDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ConvertAtMount(ctx, root, MountOptions{OnDemand: true}); err != nil {
		t.Fatalf("initial ConvertAtMount(OnDemand): %v", err)
	}

	assertFilesUnchanged := func(label string, wantDAT, wantEXT, wantOne []byte) {
		t.Helper()
		if cur, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(datRel))); !bytes.Equal(cur, wantDAT) {
			t.Fatalf("%s: DAT was modified!", label)
		}
		if cur, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(extRel))); !bytes.Equal(cur, wantEXT) {
			t.Fatalf("%s: EXT was modified!", label)
		}
		if cur, _ := os.ReadFile(onePath); !bytes.Equal(cur, wantOne) {
			t.Fatalf("%s: exportLibrary.db was modified!", label)
		}
	}

	// Simulate the deck loading the imported track via OnDemandBuilder so trackData != NULL
	// (required for needsCueRead) while keeping the imported quickCues ("Cue 1" + palette 33).
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	mDB, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	odRes, err := NewOnDemandBuilder().Build(ctx, OnDemandRequest{
		Mount:      root,
		EnginePath: "../Contents/FixtureA/Track01.mp3",
		EngineID:   1,
	})
	if err != nil {
		_ = mDB.Close()
		t.Fatalf("OnDemandBuilder.Build: %v", err)
	}
	perf, err := rbstate.DecodePerf(odRes.RBXA1)
	if err != nil {
		_ = mDB.Close()
		t.Fatalf("DecodePerf: %v", err)
	}
	if _, err := mDB.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, loops=?, activeOnLoadLoops=?, thirdPartySourceId=NULL WHERE trackId=1`,
		perf.TrackData, perf.Overview, perf.Beats, perf.Loops, perf.ActiveLoops); err != nil {
		_ = mDB.Close()
		t.Fatal(err)
	}
	if _, err := mDB.Exec(`UPDATE Track SET isAnalyzed=1 WHERE id=1`); err != nil {
		_ = mDB.Close()
		t.Fatal(err)
	}
	var loadedEdit int64
	if err := mDB.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=1`).Scan(&loadedEdit); err != nil {
		_ = mDB.Close()
		t.Fatal(err)
	}
	_ = mDB.Close()

	// Case 1: Re-insert (mount update) on a previously synced PerfDeck row (including SetDirectCover + title update)
	// must NOT turn the row into a write-back candidate or trigger any writes in CueWatcher / WriteBackCues.
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	tr := snap.Tracks["contents/fixturea/track01.mp3"]
	if tr == nil || tr.Synced == nil {
		t.Fatalf("expected synced OnDemand track, got %+v", tr)
	}
	tr.Edit = loadedEdit
	tr.Perf = rbstate.PerfDeck
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}

	// Simulate metadata + artwork change in exportLibrary.db so ConvertAtMount runs updateRows + SetDirectCover.
	oneDB, err = sql.Open("sqlite", onePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = oneDB.Exec(`UPDATE content SET title='Fixture Track 1 Updated', djComment='ExportNote', image_path='/PIONEER/Artwork/00001/a2.jpg' WHERE content_id=1`)
	_ = oneDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(5 * time.Second)
	_ = os.Chtimes(onePath, future, future)

	updRes, err := ConvertAtMount(ctx, root, MountOptions{OnDemand: true})
	if err != nil || updRes.Updated != 1 {
		t.Fatalf("mount update re-insert failed: res=%+v err=%v", updRes, err)
	}

	snapDAT1 := repDAT
	snapEXT1 := repEXT
	snapOne1, err := os.ReadFile(onePath)
	if err != nil {
		t.Fatal(err)
	}

	watcher := NewCueWatcher(parentDir)
	wRes, err := watcher.Pass(ctx, root)
	if err != nil {
		t.Fatalf("CueWatcher.Pass after mount update: %v", err)
	}
	if wRes.TracksRead != 0 || wRes.TracksChanged != 0 || wRes.FilesUpdated != 0 || wRes.OneLibraryUpdated {
		t.Fatalf("mount update turned synced row into write-back candidate: %+v", wRes)
	}
	assertFilesUnchanged("Case 1 (after mount update)", snapDAT1, snapEXT1, snapOne1)

	// Case 2: Forced candidate read (Synced == nil and lastEditTime bumped in m.db) where Engine holds
	// the same hot cue position (28264 ms) — first straight from the imported exportLibrary cue row
	// (the exact repaired-track crash scenario), then with default name "Cue 1" and various Engine colors
	// (Pad-1 yellow #f4d338, Engine default green #00ff00, arbitrary #12ab34): ZERO writes!
	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	t.Run("Case2_ZeroWrite_from_imported_mdb", func(t *testing.T) {
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+10 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		s, err := rbstate.Load(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		s.Tracks["contents/fixturea/track01.mp3"].Synced = nil
		if err := s.Save(dbDir); err != nil {
			t.Fatal(err)
		}
		live, err := watcher.Pass(ctx, root)
		if err != nil || live.TracksRead != 1 || live.TracksChanged != 0 || live.FilesUpdated != 0 || live.OneLibraryUpdated {
			t.Fatalf("imported repaired track triggered write-back or skipped read: %+v err=%v", live, err)
		}
		assertFilesUnchanged("Case 2 from imported m.db", snapDAT1, snapEXT1, snapOne1)
	})

	sampleFor28264 := float64(cueMS)*44.1 - rbOffset
	for _, tc := range []struct {
		name  string
		color [4]byte
	}{
		{"pad1_yellow", [4]byte{255, 0xf4, 0xd3, 0x38}},
		{"engine_green", [4]byte{255, 0x00, 0xff, 0x00}},
		{"arbitrary_rgb", [4]byte{255, 0x12, 0xab, 0x34}},
	} {
		t.Run("Case2_ZeroWrite_"+tc.name, func(t *testing.T) {
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
			}{name: "Cue 1", samples: sampleFor28264, color: tc.color}
			if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=1`, buildEngineQuickCuesBlob(slots)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+10 WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			s, err := rbstate.Load(dbDir)
			if err != nil {
				t.Fatal(err)
			}
			s.Tracks["contents/fixturea/track01.mp3"].Synced = nil
			if err := s.Save(dbDir); err != nil {
				t.Fatal(err)
			}

			dry, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, DryRun: true})
			if err != nil || dry.TracksRead != 1 || dry.TracksChanged != 0 || dry.FilesUpdated != 0 {
				t.Fatalf("DryRun %s wanted 1 read / 0 changes, got %+v err=%v", tc.name, dry, err)
			}
			live, err := watcher.Pass(ctx, root)
			if err != nil || live.TracksRead != 1 || live.TracksChanged != 0 || live.FilesUpdated != 0 || live.OneLibraryUpdated {
				t.Fatalf("CueWatcher %s wanted 1 read / 0 writes, got %+v err=%v", tc.name, live, err)
			}
			assertFilesUnchanged("Case 2 "+tc.name, snapDAT1, snapEXT1, snapOne1)
		})
	}

	// Case 3: Genuine deck edit (move Hot Cue A to 45000 ms) still writes exact 44-byte minimal layout.
	const newCueMS = uint32(45000)
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
	}{name: "Cue 1", samples: float64(newCueMS)*44.1 - rbOffset, color: [4]byte{255, 0xf4, 0xd3, 0x38}}
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=1`, buildEngineQuickCuesBlob(slots)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	resEdit, err := watcher.Pass(ctx, root)
	if err != nil || resEdit.TracksChanged != 1 || resEdit.FilesUpdated < 2 {
		t.Fatalf("Case 3 genuine edit wanted 1 changed track, got %+v err=%v", resEdit, err)
	}
	gotDAT, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(datRel)))
	gotEXT, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(extRel)))
	if rekordbox.ANLZNeedsCueRepair(gotDAT, false) || rekordbox.ANLZNeedsCueRepair(gotEXT, true) {
		t.Fatal("Case 3 output triggered ANLZNeedsCueRepair")
	}
	aDat, err := rekordbox.ParseAnalysis(gotDAT)
	if err != nil {
		t.Fatal(err)
	}
	aExt, err := rekordbox.ParseAnalysis(gotEXT)
	if err != nil {
		t.Fatal(err)
	}
	aDat.Merge(aExt)
	if len(aDat.Cues) != 1 || aDat.Cues[0].HotCue != 1 || aDat.Cues[0].TimeMS != newCueMS || aDat.Cues[0].Name != "" || aDat.Cues[0].Color != ([4]byte{}) {
		t.Fatalf("Case 3 cues mismatch: %+v", aDat.Cues)
	}
}

func TestWriteBackZeroDecryptOnOnDemandLoad(t *testing.T) {
	ctx := context.Background()
	parentDir := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(parentDir, "stage"))
	root := filepath.Join(parentDir, "UNTITLED")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	rawDAT, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_compact.dat"))
	if err != nil {
		t.Fatal(err)
	}
	rawEXT, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_compact.ext"))
	if err != nil {
		t.Fatal(err)
	}
	const cueMS = uint32(10000)
	repDAT, _, err := rekordbox.RewriteANLZCues(rawDAT, []rekordbox.Cue{{HotCue: 1, TimeMS: cueMS}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	repEXT, _, err := rekordbox.RewriteANLZCues(rawEXT, []rekordbox.Cue{{HotCue: 1, TimeMS: cueMS}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	datRel := "PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT"
	extRel := "PIONEER/USBANLZ/P001/00000001/ANLZ0000.EXT"
	testfixture.Write(t, root, datRel, repDAT)
	testfixture.Write(t, root, extRel, repEXT)

	mp3 := makeLAMEFixtureMP3(25000)
	testfixture.Write(t, root, "Contents/FixtureA/Track01.mp3", mp3)
	testfixture.Write(t, root, "Contents/FixtureA/Track02.mp3", mp3)
	info, err := compat.AudioHeader(filepath.Join(root, "Contents", "FixtureA", "Track01.mp3"))
	if err != nil || info.RekordboxOffset() <= 0 {
		t.Fatalf("AudioHeader: info=%+v err=%v", info, err)
	}
	rbOffset := float64(info.RekordboxOffset())

	// Track 2 carries an inactive loop so toggling activeOnLoadLoops alone tests filled.jsonl "al".
	repDAT2, _, err := rekordbox.RewriteANLZCues(rawDAT, []rekordbox.Cue{{HotCue: 1, TimeMS: 2000, EndMS: 4000, Loop: true}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	repEXT2, _, err := rekordbox.RewriteANLZCues(rawEXT, []rekordbox.Cue{{HotCue: 1, TimeMS: 2000, EndMS: 4000, Loop: true}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/00000002/ANLZ0000.DAT", repDAT2)
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/00000002/ANLZ0000.EXT", repEXT2)

	// Create a SQLCipher 4 encrypted OneLibrary export matching complete_export_anlz0000.
	plainSource := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary_decrypted.db")
	encryptedSource := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary.db")
	plainRaw, err := os.ReadFile(plainSource)
	if err != nil {
		t.Fatal(err)
	}
	plainPath := filepath.Join(t.TempDir(), "one.db")
	if err := os.WriteFile(plainPath, plainRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	oneDB, err := sql.Open("sqlite", plainPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = oneDB.Exec(`
		PRAGMA journal_mode=DELETE;
		ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
		ALTER TABLE content ADD COLUMN genre_id INTEGER;
		ALTER TABLE content ADD COLUMN label_id INTEGER;
		ALTER TABLE content ADD COLUMN hasModified integer;
		ALTER TABLE content ADD COLUMN cueUpdateCount integer;
		ALTER TABLE content ADD COLUMN analysisDataUpdateCount integer;
		CREATE TABLE genre(genre_id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE label(label_id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO genre VALUES (20, 'House');
		INSERT INTO label VALUES (30, 'LabelA');
		DELETE FROM content;
		DELETE FROM cue;
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, analysisDataFilePath, genre_id, label_id, hasModified, cueUpdateCount)
		VALUES
			(1, 'Fixture Track 1', 12800, 25, '/Contents/FixtureA/Track01.mp3', 44100, 'InitNote', '/PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT', 20, 30, 0, NULL),
			(2, 'Fixture Track 2', 12800, 25, '/Contents/FixtureA/Track02.mp3', 44100, 'InitNote', '/PIONEER/USBANLZ/P001/00000002/ANLZ0000.DAT', 20, 30, 0, NULL);
		INSERT INTO cue(cue_id, content_id, kind, colorTableIndex, cueComment, isActiveLoop, beatLoopNumerator, beatLoopDenominator, inUsec, outUsec)
		VALUES
			(1, 1, 1, 0, '', 0, 0, 0, 10000000, -1),
			(2, 2, 1, 0, '', 0, 0, 0, 2000000, 4000000);
	`)
	_ = oneDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	salt, err := rekordbox.ReadSQLCipherSalt(encryptedSource)
	if err != nil {
		t.Fatal(err)
	}
	plainF, err := os.Open(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	plainStat, err := plainF.Stat()
	if err != nil {
		_ = plainF.Close()
		t.Fatal(err)
	}
	onePath := filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db")
	if err := os.MkdirAll(filepath.Dir(onePath), 0o755); err != nil {
		_ = plainF.Close()
		t.Fatal(err)
	}
	encF, err := os.Create(onePath)
	if err != nil {
		_ = plainF.Close()
		t.Fatal(err)
	}
	if err := rekordbox.EncryptSQLCipher4(ctx, plainF, plainStat.Size(), encF, salt, []byte("r8gddnr4k847830ar6cqzbkk0el6qytmb3trbbx805jm74vez64i5o8fnrqryqls")); err != nil {
		_ = encF.Close()
		_ = plainF.Close()
		t.Fatal(err)
	}
	_ = encF.Close()
	_ = plainF.Close()

	if res, err := ConvertAtMount(ctx, root, MountOptions{OnDemand: true}); err != nil || res.Added != 2 {
		t.Fatalf("ConvertAtMount(OnDemand): res=%+v err=%v", res, err)
	}
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	snapPath := filepath.Join(dbDir, rbstate.SnapshotName)
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	tr1 := snap.Tracks["contents/fixturea/track01.mp3"]
	tr2 := snap.Tracks["contents/fixturea/track02.mp3"]
	if tr1 == nil || len(tr1.MetaDigest) == 0 || tr2 == nil || len(tr2.MetaDigest) == 0 {
		t.Fatalf("expected OnDemand snapshot tracks with MetaDigest, got tr1=%+v tr2=%+v", tr1, tr2)
	}

	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sdir := filepath.Join(parentDir, "stage", snap.LibraryUUID)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	var jlines []byte
	for _, item := range []struct {
		tr   *rbstate.Track
		path string
	}{
		{tr1, "../Contents/FixtureA/Track01.mp3"},
		{tr2, "../Contents/FixtureA/Track02.mp3"},
	} {
		odRes, err := NewOnDemandBuilder().Build(ctx, OnDemandRequest{
			Mount:      root,
			EnginePath: item.path,
			EngineID:   item.tr.EngineID,
		})
		if err != nil {
			t.Fatalf("OnDemandBuilder.Build(%s): %v", item.path, err)
		}
		perf, err := rbstate.DecodePerf(odRes.RBXA1)
		if err != nil {
			t.Fatalf("DecodePerf: %v", err)
		}
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=?, thirdPartySourceId=NULL WHERE trackId=?`,
			perf.TrackData, perf.Overview, perf.Beats, perf.QuickCues, perf.Loops, perf.ActiveLoops, item.tr.EngineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET isAnalyzed=1, lastEditTime=COALESCE(lastEditTime,0)+5 WHERE id=?`, item.tr.EngineID); err != nil {
			t.Fatal(err)
		}
		jline, _ := json.Marshal(map[string]any{
			"id":   item.tr.EngineID,
			"k":    item.tr.Key,
			"wr":   base64.StdEncoding.EncodeToString(odRes.Written),
			"wrnm": base64.StdEncoding.EncodeToString(odRes.WrittenNoMain),
			"al":   int64(perf.ActiveLoops),
			"v":    rbstate.Rules,
			"w":    item.tr.Row,
		})
		jlines = append(jlines, append(jline, '\n')...)
	}
	if err := os.WriteFile(filepath.Join(sdir, "filled.jsonl"), jlines, 0o600); err != nil {
		t.Fatal(err)
	}

	var readLibsCalls int
	origHook := writeBackReadLibsHook
	writeBackReadLibsHook = func() { readLibsCalls++ }
	t.Cleanup(func() { writeBackReadLibsHook = origHook })

	snapBytesBefore, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}

	// Untouched on-demand loads must perform 0 export decryptions, 0 key derivations, and 0 snapshot writes
	// in both watcher mode and eject mode!
	keyDir := t.TempDir()
	wRes, err := WriteBackCues(ctx, root, WriteBackOptions{
		SkipPreparingCheck: true,
		Session:            &CueSession{keys: rekordbox.NewKeyCacheAt(keyDir)},
		Settled:            func() error { return nil },
	})
	if err != nil {
		t.Fatalf("watcher WriteBackCues: %v", err)
	}
	if readLibsCalls != 0 || wRes.TracksRead != 2 || wRes.TracksChanged != 0 || wRes.FilesUpdated != 0 ||
		wRes.BytesWritten != 0 || wRes.PhasesMS["decrypt"] != 0 || wRes.PhasesMS["key"] != 0 {
		t.Fatalf("untouched on-demand load triggered export read/decrypt/write: readLibs=%d res=%+v", readLibsCalls, wRes)
	}
	ejRes, err := WriteBackCues(ctx, root, WriteBackOptions{
		SkipPreparingCheck: true,
		OnEject:            true,
		Session:            &CueSession{keys: rekordbox.NewKeyCacheAt(keyDir)},
	})
	if err != nil {
		t.Fatalf("eject WriteBackCues: %v", err)
	}
	if readLibsCalls != 0 || ejRes.TracksRead != 2 || ejRes.TracksChanged != 0 || ejRes.FilesUpdated != 0 ||
		ejRes.PhasesMS["decrypt"] != 0 || ejRes.PhasesMS["key"] != 0 {
		t.Fatalf("eject on untouched on-demand load triggered export read/decrypt: readLibs=%d res=%+v", readLibsCalls, ejRes)
	}
	snapBytesAfter, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapBytesBefore, snapBytesAfter) {
		t.Fatal("untouched on-demand write-back rewrote .rb-bridge-snapshot on disk!")
	}

	// Adopted tracks with Synced == nil persist their baseline Synced once in 0 ms decrypt/key
	// so subsequent passes with a fresh session read 0 untouched rows and do 0 snapshot writes.
	adoptedSnap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	adoptedSnap.Tracks[tr1.Key].Synced = nil
	if err := adoptedSnap.SaveNext(dbDir); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.Promote(dbDir); err != nil {
		t.Fatal(err)
	}
	settle1, err := WriteBackCues(ctx, root, WriteBackOptions{
		SkipPreparingCheck: true,
		SettleBatch:        256,
		Session:            &CueSession{keys: rekordbox.NewKeyCacheAt(keyDir)},
	})
	if err != nil || readLibsCalls != 0 || settle1.TracksRead != 2 || settle1.TracksChanged != 0 ||
		settle1.PhasesMS["decrypt"] != 0 || settle1.PhasesMS["key"] != 0 {
		t.Fatalf("adopted Synced==nil settle pass = %+v, calls=%d err=%v", settle1, readLibsCalls, err)
	}
	afterSettleSnap, err := rbstate.Load(dbDir)
	if err != nil || len(afterSettleSnap.Tracks[tr1.Key].Synced) == 0 || len(afterSettleSnap.Tracks[tr2.Key].Synced) == 0 {
		t.Fatalf("adopted settle pass did not persist Synced digest: tr1=%+v tr2=%+v err=%v", afterSettleSnap.Tracks[tr1.Key], afterSettleSnap.Tracks[tr2.Key], err)
	}
	settleSnapBytes, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	settle2, err := WriteBackCues(ctx, root, WriteBackOptions{
		SkipPreparingCheck: true,
		OnEject:            true,
		Session:            &CueSession{keys: rekordbox.NewKeyCacheAt(keyDir)},
	})
	if err != nil || readLibsCalls != 0 || settle2.TracksRead != 0 || settle2.TracksChanged != 0 {
		t.Fatalf("post-settle second eject pass re-read tracks: %+v, calls=%d err=%v", settle2, readLibsCalls, err)
	}
	if curSnapBytes, err := os.ReadFile(snapPath); err != nil || !bytes.Equal(settleSnapBytes, curSnapBytes) {
		t.Fatalf("post-settle second eject pass modified .rb-bridge-snapshot")
	}

	// Playing/loading an adopted track after Synced is settled can update unadjusted (adj == 0)
	// defaultMainCue and lastEditTime without any DJ cue edit: SyncedDigest normalization must
	// still report 0 export decrypts, 0 key derivations, and 0 snapshot writes.
	var curQC []byte
	if err := db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=?`, tr1.EngineID).Scan(&curQC); err != nil {
		t.Fatal(err)
	}
	rawQC, err := enginedb.QUncompress(curQC, 1<<20)
	if err != nil || len(rawQC) < 17 || rawQC[len(rawQC)-9] != 0 {
		t.Fatalf("unexpected quickCues tail on tr1: err=%v len=%d", err, len(rawQC))
	}
	binary.BigEndian.PutUint64(rawQC[len(rawQC)-8:], binary.BigEndian.Uint64(rawQC[len(rawQC)-8:])^1)
	flippedQC, err := enginedb.QCompress(rawQC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, flippedQC, tr1.EngineID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+5 WHERE id=?`, tr1.EngineID); err != nil {
		t.Fatal(err)
	}
	settle3, err := WriteBackCues(ctx, root, WriteBackOptions{
		SkipPreparingCheck: true,
		OnEject:            true,
		Session:            &CueSession{keys: rekordbox.NewKeyCacheAt(keyDir)},
	})
	if err != nil || readLibsCalls != 0 || settle3.TracksRead != 1 || settle3.TracksChanged != 0 ||
		settle3.PhasesMS["decrypt"] != 0 || settle3.PhasesMS["key"] != 0 {
		t.Fatalf("post-settle unadjusted mainCue flip = %+v, calls=%d err=%v", settle3, readLibsCalls, err)
	}
	if curSnapBytes, err := os.ReadFile(snapPath); err != nil || !bytes.Equal(settleSnapBytes, curSnapBytes) {
		t.Fatalf("post-settle unadjusted mainCue flip rewrote .rb-bridge-snapshot")
	}

	// Now verify all 4 real mutations DO trigger write-back promptly, using warm KeyCache after initial derivation.
	sharedKeys := rekordbox.NewKeyCacheAt(keyDir)
	t.Run("1_CueEdit", func(t *testing.T) {
		prevCalls := readLibsCalls
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
		}{samples: float64(cueMS)*44.1 - rbOffset}
		slots[1] = struct {
			name    string
			samples float64
			color   [4]byte
		}{samples: float64(20000)*44.1 - rbOffset}
		if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, buildEngineQuickCuesBlob(slots), tr1.EngineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=?`, tr1.EngineID); err != nil {
			t.Fatal(err)
		}
		r, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, Session: &CueSession{keys: sharedKeys}})
		if err != nil || readLibsCalls != prevCalls+1 || r.TracksChanged != 1 {
			t.Fatalf("cue edit write-back = %+v, calls=%d err=%v", r, readLibsCalls-prevCalls, err)
		}
	})

	t.Run("2_ActiveLoopToggle", func(t *testing.T) {
		prevCalls := readLibsCalls
		if _, err := db.Exec(`UPDATE PerformanceData SET activeOnLoadLoops=1 WHERE trackId=?`, tr2.EngineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=?`, tr2.EngineID); err != nil {
			t.Fatal(err)
		}
		derivBefore := rekordbox.KeyDerivations()
		r, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, Session: &CueSession{keys: sharedKeys}})
		if err != nil || readLibsCalls != prevCalls+1 || r.TracksChanged != 1 {
			t.Fatalf("active loop toggle write-back = %+v, calls=%d err=%v", r, readLibsCalls-prevCalls, err)
		}
		if rekordbox.KeyDerivations() != derivBefore || r.PhasesMS["key"] != 0 {
			t.Fatalf("warm KeyCache re-derived key: derivDelta=%d keyMs=%d", rekordbox.KeyDerivations()-derivBefore, r.PhasesMS["key"])
		}
	})

	t.Run("3_BeatGridEdit", func(t *testing.T) {
		prevCalls := readLibsCalls
		const sr = 44100.0
		const step = sr * 60.0 / 125.0
		defGrid := []struct {
			pos      float64
			idx      int64
			numBeats uint32
		}{{pos: 4410, idx: 0, numBeats: 4}, {pos: 4410 + 4*step, idx: 4, numBeats: 0}}
		adjGrid := []struct {
			pos      float64
			idx      int64
			numBeats uint32
		}{{pos: 8820, idx: 0, numBeats: 8}, {pos: 8820 + 8*step, idx: 8, numBeats: 0}}
		if _, err := db.Exec(`UPDATE PerformanceData SET beatData=? WHERE trackId=?`,
			buildEngineBeatDataBlob(sr, 25*sr, defGrid, adjGrid, true), tr1.EngineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=?`, tr1.EngineID); err != nil {
			t.Fatal(err)
		}
		derivBefore := rekordbox.KeyDerivations()
		r, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, Session: &CueSession{keys: sharedKeys}})
		if err != nil || readLibsCalls != prevCalls+1 || r.TracksChanged != 1 {
			t.Fatalf("beat grid edit write-back = %+v, calls=%d err=%v", r, readLibsCalls-prevCalls, err)
		}
		if rekordbox.KeyDerivations() != derivBefore || r.PhasesMS["key"] != 0 {
			t.Fatalf("warm KeyCache re-derived key: derivDelta=%d keyMs=%d", rekordbox.KeyDerivations()-derivBefore, r.PhasesMS["key"])
		}
	})

	t.Run("4_MetadataEdit", func(t *testing.T) {
		prevCalls := readLibsCalls
		if _, err := db.Exec(`UPDATE Track SET comment='DJ Edited Comment', lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=?`, tr1.EngineID); err != nil {
			t.Fatal(err)
		}
		derivBefore := rekordbox.KeyDerivations()
		r, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, Session: &CueSession{keys: sharedKeys}})
		if err != nil || readLibsCalls != prevCalls+1 || r.TracksChanged != 1 {
			t.Fatalf("metadata edit write-back = %+v, calls=%d err=%v", r, readLibsCalls-prevCalls, err)
		}
		if rekordbox.KeyDerivations() != derivBefore || r.PhasesMS["key"] != 0 {
			t.Fatalf("warm KeyCache re-derived key: derivDelta=%d keyMs=%d", rekordbox.KeyDerivations()-derivBefore, r.PhasesMS["key"])
		}
	})
}
