package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	bridge "github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

func restoredImportAll(t *testing.T, mount string) {
	t.Helper()
	ctx := context.Background()
	if _, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{}); err != nil {
		t.Fatalf("ConvertAtMount(first): %v", err)
	}
	if _, err := bridge.FillInBackground(ctx, mount, bridge.FillOptions{}); err != nil {
		t.Fatalf("FillInBackground: %v", err)
	}
	if _, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{Publish: true}); err != nil {
		t.Fatalf("ConvertAtMount(publish): %v", err)
	}
}

func restoredWriteEncryptedOneLibrary(t *testing.T, outPath string, setupSQL string, args ...any) {
	t.Helper()
	plainSource := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary_decrypted.db")
	encryptedSource := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary.db")
	plainPath := filepath.Join(t.TempDir(), "onelibrary.db")
	raw, err := os.ReadFile(plainSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", plainPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(setupSQL, args...); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	salt, err := rekordbox.ReadSQLCipherSalt(encryptedSource)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := os.Open(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	info, err := plain.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := rekordbox.EncryptSQLCipher4(context.Background(), plain, info.Size(), out, salt, []byte(p3OneLibraryPassphrase)); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// Restored #178 (replaces pkg/bridge/one_offset_rule_test.go:TestOneOffsetRuleCompletedLibraryIsReadAgainOnce):
// bumping rbstate.Rules forces an unchanged already-imported stick to resync once, after which
// the stick is at the current Rules version and bypasses again.
// Guards pkg/bridge/mount.go (`head.Rules == rbstate.Rules`) and pkg/bridge/apply.go (`snap.Rules == rbstate.Rules`).
func TestRestored_Issue178_RulesBumpResyncsUnchangedStickOnce(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 4)
	restoredImportAll(t, mount)

	// Confirm unchanged stick bypasses at current Rules.
	res, err := bridge.ConvertAtMount(context.Background(), mount, bridge.MountOptions{})
	if err != nil || !res.Bypassed {
		t.Fatalf("expected initial unchanged bypass: res=%+v err=%v", res, err)
	}

	// Simulate an already-imported stick written under an older Rules revision by patching
	// the persisted snapshot header and updating its CRC32 trailer (snap.Save always writes rbstate.Rules).
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	snapPath := filepath.Join(dbDir, rbstate.SnapshotName)
	snapBytes, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := bytes.TrimSuffix(snapBytes, []byte("\n"))
	lastNL := bytes.LastIndexByte(trimmed, '\n')
	if lastNL < 0 {
		t.Fatal("snapshot missing trailer line")
	}
	body := append([]byte(nil), snapBytes[:lastNL+1]...)
	trailer := string(trimmed[lastNL+1:])
	oldRule := []byte(fmt.Sprintf(`"rules":%d`, rbstate.Rules))
	newRule := []byte(fmt.Sprintf(`"rules":%d`, rbstate.Rules-1))
	if !bytes.Contains(body, oldRule) {
		t.Fatalf("snapshot missing %q", oldRule)
	}
	body = bytes.Replace(body, oldRule, newRule, 1)
	fields := strings.Fields(trailer)
	if len(fields) != 3 || fields[0] != "E" {
		t.Fatalf("unexpected snapshot trailer %q", trailer)
	}
	patched := append(body, []byte(fmt.Sprintf("E %s %08x\n", fields[1], crc32.ChecksumIEEE(body)))...)
	if err := os.WriteFile(snapPath, patched, 0o600); err != nil {
		t.Fatal(err)
	}

	// Next mount MUST NOT bypass: it re-reads the export and marks perf pending for the new rules.
	res, err = bridge.ConvertAtMount(context.Background(), mount, bridge.MountOptions{})
	if err != nil {
		t.Fatalf("ConvertAtMount after rules bump: %v", err)
	}
	if res.Bypassed {
		t.Fatal("unchanged stick bypassed despite older snapshot Rules version")
	}
	if res.Pending.Perf == 0 {
		t.Fatalf("expected Pending.Perf > 0 after rules bump, got %+v", res.Pending)
	}

	// Re-fill and publish brings snapshot back to current Rules; subsequent insert bypasses once more.
	if _, err := bridge.FillInBackground(context.Background(), mount, bridge.FillOptions{}); err != nil {
		t.Fatalf("FillInBackground after rules bump: %v", err)
	}
	if _, err := bridge.ConvertAtMount(context.Background(), mount, bridge.MountOptions{Publish: true}); err != nil {
		t.Fatalf("publish after rules bump: %v", err)
	}
	head, err := rbstate.ReadHeader(dbDir)
	if err != nil || head.Rules != rbstate.Rules || !head.Pending.None() {
		t.Fatalf("snapshot after rules resync: head=%+v err=%v", head, err)
	}
	res, err = bridge.ConvertAtMount(context.Background(), mount, bridge.MountOptions{})
	if err != nil || !res.Bypassed {
		t.Fatalf("expected bypass after completing rules resync: res=%+v err=%v", res, err)
	}
}

func restoredOldMP3BeatData(beat0, beat1 float64) []byte {
	raw := make([]byte, 130)
	binary.BigEndian.PutUint64(raw[0:], math.Float64bits(44100))
	binary.BigEndian.PutUint64(raw[8:], math.Float64bits(441000))
	raw[16] = 1
	binary.BigEndian.PutUint64(raw[17:], 2)
	binary.BigEndian.PutUint64(raw[73:], 2)
	put := func(off int, pos float64, beat int64, num uint32) {
		binary.LittleEndian.PutUint64(raw[off:], math.Float64bits(pos))
		binary.LittleEndian.PutUint64(raw[off+8:], uint64(beat))
		binary.LittleEndian.PutUint32(raw[off+16:], num)
	}
	put(25, beat0, -4, 24)
	put(49, beat1, 20, 0)
	put(81, beat0, -4, 24)
	put(105, beat1, 20, 0)
	return raw
}

// Restored #246 (replaces pkg/bridge/mp3grid_at_mount_test.go:TestConvertAtMountFixesEngineXMP3Grids):
// on a v2 stick with .rb-bridge-snapshot (and NO legacy .rb-bridge-state.db), ConvertAtMount chores
// shifts an EngineX MP3 proxy beat grid while leaving snapshot-managed rekordbox MP3 tracks untouched.
// Guards pkg/bridge/mount.go (`chores` -> `fixMP3Grids`) and pkg/enginedb/mp3grid.go (`rbstate.Load`).
func TestRestored_Issue246_SnapshotBackedMP3GridAtMount(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 2)
	restoredImportAll(t, mount)

	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	if _, err := os.Stat(filepath.Join(dbDir, rbstate.SnapshotName)); err != nil {
		t.Fatalf("missing v2 snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dbDir, ".rb-bridge-state.db")); !os.IsNotExist(err) {
		t.Fatalf("unexpected legacy state db on v2 stick: %v", err)
	}

	// Add an EngineX MP3 proxy track alongside the snapshot-managed rekordbox MP3 tracks.
	testfixture.Write(t, mount, "Music/Stream/246.mp3", testfixture.MP3WithLAMETag())
	db := p3OpenDB(t, mount)
	trackRaw := make([]byte, 44)
	binary.BigEndian.PutUint64(trackRaw[0:], math.Float64bits(44100))
	binary.BigEndian.PutUint64(trackRaw[8:], 441000)
	trackData, err := enginedb.QCompress(trackRaw)
	if err != nil {
		t.Fatal(err)
	}
	beatData, err := enginedb.QCompress(restoredOldMP3BeatData(5000, 460000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(path,filename,fileType,comment,isAnalyzed,isAvailable,isMetadataImported,isBeatGridLocked)
		VALUES('../Music/Stream/246.mp3','246.mp3','mp3','[320k] MP3 44100 Hz; FLAC unavailable',1,1,1,0)`); err != nil {
		t.Fatal(err)
	}
	var proxyID int64
	if err := db.QueryRow(`SELECT id FROM Track WHERE path='../Music/Stream/246.mp3'`).Scan(&proxyID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,beatData=?,thirdPartySourceId=NULL WHERE trackId=?`, trackData, beatData, proxyID); err != nil {
		t.Fatal(err)
	}
	// Give snapshot-managed rekordbox track 1 an EngineX MP3 proxy comment & old beat grid so ONLY
	// FixMP3Grids's v2 rbstate.Load check prevents double-shifting it.
	rbID := p3FirstTrackID(t, db)
	if _, err := db.Exec(`UPDATE Track SET comment='[320k] MP3 44100 Hz; FLAC unavailable', isAnalyzed=1, isBeatGridLocked=0, pdbImportKey=NULL WHERE id=?`, rbID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, beatData=?, thirdPartySourceId=NULL WHERE trackId=?`, trackData, beatData, rbID); err != nil {
		t.Fatal(err)
	}
	var rbBeatsBefore []byte
	if err := db.QueryRow(`SELECT beatData FROM PerformanceData WHERE trackId=?`, rbID).Scan(&rbBeatsBefore); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	_ = os.Remove(filepath.Join(dbDir, enginedb.MP3GridStampFile))
	_ = os.Remove(filepath.Join(dbDir, enginedb.MP3GridCursorFile))

	res, err := bridge.ConvertAtMount(context.Background(), mount, bridge.MountOptions{})
	if err != nil || !res.Bypassed {
		t.Fatalf("ConvertAtMount unchanged with MP3 grid chore: res=%+v err=%v", res, err)
	}
	db = p3OpenDB(t, mount)
	var proxyBlob, rbBeatsAfter []byte
	if err := db.QueryRow(`SELECT beatData FROM PerformanceData WHERE trackId=?`, proxyID).Scan(&proxyBlob); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT beatData FROM PerformanceData WHERE trackId=?`, rbID).Scan(&rbBeatsAfter); err != nil {
		t.Fatal(err)
	}
	raw, err := enginedb.QUncompress(proxyBlob, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(raw[25:33])); math.Abs(got-2743) > 1e-6 {
		t.Fatalf("EngineX MP3 proxy beat0=%v, want 2743", got)
	}
	if !bytes.Equal(rbBeatsBefore, rbBeatsAfter) {
		t.Fatal("snapshot-managed rekordbox MP3 track beatData was unexpectedly altered by FixMP3Grids")
	}
	if _, err := os.Stat(filepath.Join(dbDir, enginedb.MP3GridStampFile)); err != nil {
		t.Fatalf("expected MP3GridStampFile after chore completion: %v", err)
	}
}

// Restored #253 (replaces pkg/bridge/issue253_anlz_test.go:TestPublishRemovesRGBOverviewsOfAnotherTrack):
// truncated (<= 4 byte) .rgb list waveforms are replaced on publish with valid waveform data, while
// already valid (> 4 byte) .rgb files are kept intact.
// Guards pkg/bridge/apply.go (`writeOverview` `st.Size() > 4` check).
func TestRestored_Issue253_ShortRGBOverviewReplacedOnPublish(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 2)
	ctx := context.Background()
	if _, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.FillInBackground(ctx, mount, bridge.FillOptions{}); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	// Plant a broken 3-byte .rgb for track 1 and a custom valid >4-byte .rgb for track 2.
	k1 := rbstate.FoldPath(testfixture.LargeTrackPath(1))
	k2 := rbstate.FoldPath(testfixture.LargeTrackPath(2))
	rgbDir := filepath.Join(dbDir, "OverviewData", snap.LibraryUUID)
	if err := os.MkdirAll(rgbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rgb1 := filepath.Join(rgbDir, "1.rgb")
	rgb2 := filepath.Join(rgbDir, "2.rgb")
	if err := os.WriteFile(rgb1, []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	validCustom := []byte("VALID-CUSTOM-RGB-DATA")
	if err := os.WriteFile(rgb2, validCustom, 0o644); err != nil {
		t.Fatal(err)
	}
	// Force applyStaged to hit writeOverview by changing staged EngineID mismatch (as on a re-row).
	st, err := rbstate.OpenStage(snap.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	entries, _, err := st.Entries()
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	for _, k := range []string{k1, k2} {
		e := entries[k]
		if e == nil {
			st.Close()
			t.Fatalf("missing staged entry for %s", k)
		}
		full, err := st.Read(e)
		if err != nil {
			st.Close()
			t.Fatal(err)
		}
		full.EngineID = 0
		if err := st.Put(full); err != nil {
			st.Close()
			t.Fatal(err)
		}
	}
	_ = st.Sync()
	_ = st.Close()

	if _, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{Publish: true}); err != nil {
		t.Fatalf("ConvertAtMount(Publish): %v", err)
	}
	got1, err := os.ReadFile(rgb1)
	if err != nil || len(got1) <= 4 {
		t.Fatalf("truncated .rgb was not replaced on publish: len=%d err=%v", len(got1), err)
	}
	got2, err := os.ReadFile(rgb2)
	if err != nil || !bytes.Equal(got2, validCustom) {
		t.Fatalf("valid .rgb was unexpectedly overwritten: len=%d err=%v", len(got2), err)
	}
}

// Restored #271/#321 (1) (replaces pkg/bridge/cue_writeback_test.go:TestWriteBackCuesDotPioneerMacOSStickAndExactPadSlots):
// hidden macOS .PIONEER stick with encrypted SQLCipher4 exportLibrary.db + ANLZ writes back
// exact pad slots A (1), active Hot Loop B (2), Hot Cue H (8), Memory Cue (0), and Memory Loop (0).
// Guards pkg/compat/resolver.go (hidden .PIONEER), pkg/bridge/writeback.go (`mergeEngineCues`),
// and pkg/rekordbox/sqlite_write.go (`applyCueUpdatesSQLite`).
func TestRestored_Issue271_DotPioneerMacOSOneLibraryExactPadSlotsAH(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 2)
	if err := os.Rename(filepath.Join(mount, "PIONEER"), filepath.Join(mount, ".PIONEER")); err != nil {
		t.Fatal(err)
	}
	onePath := filepath.Join(mount, ".PIONEER", "rekordbox", "exportLibrary.db")
	restoredWriteEncryptedOneLibrary(t, onePath, `
		PRAGMA journal_mode=DELETE;
		ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
		ALTER TABLE content ADD COLUMN hasModified integer;
		ALTER TABLE content ADD COLUMN cueUpdateCount integer;
		ALTER TABLE cue ADD COLUMN in150FramePerSec integer;
		ALTER TABLE cue ADD COLUMN out150FramePerSec integer;
		DELETE FROM content;
		DELETE FROM cue;
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, analysisDataFilePath, hasModified, cueUpdateCount) VALUES
			(1, 'Track 1', 12800, 4, ?, 44100, '', '/.PIONEER/USBANLZ/P001/ANLZ0000.DAT', 0, NULL),
			(2, 'Track 2', 12400, 4, ?, 44100, '', '/.PIONEER/USBANLZ/P001/ANLZ0000.DAT', 0, NULL);
	`, "/"+testfixture.LargeTrackPath(1), "/"+testfixture.LargeTrackPath(2))
	if pdbInfo, err := os.Stat(filepath.Join(mount, ".PIONEER", "rekordbox", "export.pdb")); err == nil {
		_ = os.Chtimes(onePath, pdbInfo.ModTime().Add(10*time.Second), pdbInfo.ModTime().Add(10*time.Second))
	}
	restoredImportAll(t, mount)

	audioInfo, err := compat.AudioHeader(filepath.Join(mount, filepath.FromSlash(testfixture.LargeTrackPath(1))))
	if err != nil {
		t.Fatal(err)
	}
	perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100, StartOffsetSamples: audioInfo.RekordboxOffset()}, &rekordbox.Analysis{
		Beats: []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 1000, Name: "Pad A", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
			{HotCue: 2, TimeMS: 2000, EndMS: 3000, Loop: true, Active: true, Name: "Loop B", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
			{HotCue: 8, TimeMS: 3500, Name: "Pad H", Color: [4]byte{255, 0xff, 0x37, 0x6f}, ColorCode: 45},
			{HotCue: 0, TimeMS: 250, Name: "Memoire", ColorID: 5},
			{HotCue: 0, TimeMS: 400, EndMS: 800, Loop: true, Name: "Saved Loop", ColorID: 3},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	db := p3OpenDB(t, mount)
	id := p3FirstTrackID(t, db)
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=?, loops=?, activeOnLoadLoops=? WHERE trackId=?`,
		perf.TrackData, perf.QuickCues, perf.Loops, perf.ActiveLoops, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	res, err := bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("WriteBackCues(.PIONEER): res=%+v err=%v", res, err)
	}

	one, err := rekordbox.ReadSQLite(ctx, onePath, nil)
	if err != nil || len(one.Tracks) < 1 {
		t.Fatalf("ReadSQLite(.PIONEER): %v", err)
	}
	checkCues := func(where string, cues []rekordbox.Cue) {
		t.Helper()
		var gotA, gotB, gotH, gotMemCue, gotMemLoop bool
		for _, c := range cues {
			switch {
			case c.HotCue == 1 && !c.Loop && c.Name == "Pad A":
				gotA = true
			case c.HotCue == 2 && c.Loop && c.Active && c.Name == "Loop B":
				gotB = true
			case c.HotCue == 8 && !c.Loop && c.Name == "Pad H":
				gotH = true
			case c.HotCue == 0 && !c.Loop && c.TimeMS >= 245 && c.TimeMS <= 255:
				gotMemCue = true
			case c.HotCue == 0 && c.Loop && c.Name == "Saved Loop":
				gotMemLoop = true
			}
		}
		if !gotA || !gotB || !gotH || !gotMemCue || !gotMemLoop {
			t.Fatalf("%s missing exact pad slots: A=%v B(active loop)=%v H=%v memCue=%v memLoop=%v (cues=%+v)",
				where, gotA, gotB, gotH, gotMemCue, gotMemLoop, cues)
		}
	}
	checkCues("OneLibrary", one.Tracks[0].Cues)
	anlz, err := rekordbox.ReadAnalysis(filepath.Join(mount, ".PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}
	checkCues("ANLZ.EXT", anlz.Cues)
}

// Restored #271/#321 (2) (replaces pkg/bridge/issue321_auto_test.go:TestIssue321RestoringTheImportsCuesAfterAWriteIsWrittenBack):
// after Pass 1 writes a new deck cue to encrypted exportLibrary.db + ANLZ, Pass 2 deleting that
// cue (restoring the original import's cue set) writes the deletion back to both exportLibrary.db and ANLZ.
// Guards pkg/bridge/writeback.go (`NoteWriteBack`) and pkg/bridge/writeback_files.go (`hashes`).
func TestRestored_Issue321_RestoringImportedCuesAfterWriteIsWrittenBack(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()
	mount := t.TempDir()
	testfixture.Export(t, mount)
	pdb, err := rekordbox.ReadPDB(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil || len(pdb.Tracks) < 2 {
		t.Fatalf("ReadPDB: %v", err)
	}
	onePath := filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")
	restoredWriteEncryptedOneLibrary(t, onePath, `
		PRAGMA journal_mode=DELETE;
		ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
		DELETE FROM content;
		DELETE FROM cue;
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, analysisDataFilePath) VALUES
			(1, 'Track 1', 12000, 2, ?, 44100, '', '/PIONEER/USBANLZ/P001/ANLZ0000.DAT'),
			(2, 'Track 2', 12000, 2, ?, 44100, '', '/PIONEER/USBANLZ/P001/ANLZ0000.DAT');
	`, pdb.Tracks[0].Path, pdb.Tracks[1].Path)
	restoredImportAll(t, mount)

	db := p3OpenDB(t, mount)
	id := p3FirstTrackID(t, db)
	var origTrack, origCues, origLoops []byte
	var origActive int64
	if err := db.QueryRow(`SELECT trackData, quickCues, loops, activeOnLoadLoops FROM PerformanceData WHERE trackId=?`, id).
		Scan(&origTrack, &origCues, &origLoops, &origActive); err != nil {
		t.Fatal(err)
	}

	// Pass 1: add deck cue "TempCue".
	p3DeckCue(t, db, id, "TempCue", 1, 1000)
	res1, err := bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res1.TracksChanged != 1 || !res1.OneLibraryUpdated {
		t.Fatalf("pass 1: res=%+v err=%v", res1, err)
	}
	if !p3AnalysisHas(t, mount, "TempCue") {
		t.Fatal("pass 1 cue missing from ANLZ")
	}

	// Pass 2: restore the original imported performance blobs (deleting "TempCue").
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=?, loops=?, activeOnLoadLoops=? WHERE trackId=?`,
		origTrack, origCues, origLoops, origActive, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+200 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	res2, err := bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res2.TracksChanged != 1 || !res2.OneLibraryUpdated {
		t.Fatalf("pass 2 (restore original cues): res=%+v err=%v", res2, err)
	}
	if p3AnalysisHas(t, mount, "TempCue") {
		t.Fatal("deleted cue still present in ANLZ after pass 2")
	}
	one, err := rekordbox.ReadSQLite(ctx, filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range one.Tracks[0].Cues {
		if c.Name == "TempCue" {
			t.Fatalf("deleted cue still present in OneLibrary after pass 2: %+v", one.Tracks[0].Cues)
		}
	}
}

// Restored #271/#321 (3) (replaces pkg/bridge/cue_writeback_test.go:TestWriteBackCuesRestoresMP3RekordboxOffsetAndSkipsUntouched):
// MP3 encoder start offset is restored on write-back so untouched tracks produce zero spurious diffs
// (even when Written digest is cleared) and new deck cues land at the unshifted rekordbox timestamp.
// Guards pkg/bridge/writeback.go (`trackRekordboxOffset` and `mergeEngineCues`).
func TestRestored_Issue321_MP3OffsetRestoredAndUntouchedSkipped(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()
	mount := t.TempDir()
	const trackRel = "Contents/mp3/offset.mp3"
	testfixture.Write(t, mount, trackRel, testfixture.MP3WithLAMETag())
	audioInfo, err := compat.AudioHeader(filepath.Join(mount, filepath.FromSlash(trackRel)))
	if err != nil {
		t.Fatal(err)
	}
	offset := float64(audioInfo.RekordboxOffset())
	if offset <= 0 {
		t.Fatalf("expected positive MP3 RekordboxOffset, got %v", offset)
	}
	anlzRel := "PIONEER/USBANLZ/P001/ANLZ0000.DAT"
	initCues := []rekordbox.Cue{{HotCue: 1, TimeMS: 40, Name: "OrigA", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1}}
	datBytes, _, err := rekordbox.RewriteANLZCues(testfixture.DAT(), initCues, false, false)
	if err != nil {
		t.Fatal(err)
	}
	extBytes, _, err := rekordbox.RewriteANLZCues(testfixture.EXT(), initCues, true, false)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, mount, anlzRel, datBytes)
	testfixture.Write(t, mount, "PIONEER/USBANLZ/P001/ANLZ0000.EXT", extBytes)
	expPath := filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")
	if err := os.MkdirAll(filepath.Dir(expPath), 0o755); err != nil {
		t.Fatal(err)
	}
	expDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: expPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = expDB.Exec(`
		PRAGMA journal_mode=DELETE;
		CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT, bpmx100 INTEGER, length INTEGER, samplingRate INTEGER, analysisDataFilePath TEXT, hasModified INTEGER, cueUpdateCount INTEGER);
		CREATE TABLE cue(cue_id INTEGER PRIMARY KEY, content_id INTEGER, kind INTEGER, colorTableIndex INTEGER, cueComment TEXT, isActiveLoop INTEGER, beatLoopNumerator INTEGER, beatLoopDenominator INTEGER, inUsec INTEGER, outUsec INTEGER, in150FramePerSec INTEGER, out150FramePerSec INTEGER);
		INSERT INTO content VALUES(1, 'OffsetMP3', ?, 12000, 10, ?, ?, 0, NULL);
		INSERT INTO cue VALUES(1, 1, 1, 1, 'OrigA', 0, 0, 0, 40000, -1, 6, -1);
	`, "/"+trackRel, audioInfo.SampleRate, "/"+anlzRel)
	_ = expDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	restoredImportAll(t, mount)

	// Clear Written + Synced in snapshot and bump lastEditTime without changing cues:
	// restoring RekordboxOffset must match OrigA at 40 ms with zero spurious diff.
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range snap.Tracks {
		tr.Written = nil
		tr.Synced = nil
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	db := p3OpenDB(t, mount)
	id := p3FirstTrackID(t, db)
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}

	resUntouched, err := bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || resUntouched.TracksChanged != 0 {
		t.Fatalf("untouched MP3 produced spurious write-back: res=%+v err=%v", resUntouched, err)
	}

	// Now add Hot Cue B at 60 ms in rekordbox time (within the short MP3 fixture's duration,
	// shifted earlier by RekordboxOffset in Engine's millisecond frame).
	posB := 60.0 - offset*1000.0/float64(audioInfo.SampleRate)
	if err := enginedb.SetQuickCue(ctx, db, id, 1, "DeckB", "#00ff00", &posB); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+200 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	resEdit, err := bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || resEdit.TracksChanged != 1 {
		t.Fatalf("MP3 cue edit write-back failed: res=%+v err=%v", resEdit, err)
	}
	one, err := rekordbox.ReadSQLite(ctx, expPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	foundB := false
	for _, c := range one.Tracks[0].Cues {
		if c.HotCue == 2 && c.Name == "DeckB" {
			foundB = true
			if c.TimeMS < 58 || c.TimeMS > 62 {
				t.Fatalf("DeckB written at %d ms, want 60 ms +/- 2 ms (offset not subtracted)", c.TimeMS)
			}
		}
	}
	if !foundB {
		t.Fatalf("DeckB missing from exportLibrary.db: %+v", one.Tracks[0].Cues)
	}
}

// Restored #271/#321 (4) (replaces pkg/bridge/cue_writeback_safety_test.go:TestWriteBackStampReason):
// missing snapshot, mismatched export stamp, or pending work each refuse write-back with a clear reason
// and leave export files untouched.
// Guards pkg/bridge/writeback.go (`ImportState` / `st.Matches()` / `st.Header.Pending.None()`).
func TestRestored_Issue321_StampMismatchRefusesWriteBack(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 2)
	restoredImportAll(t, mount)
	db := p3OpenDB(t, mount)
	p3DeckCue(t, db, p3FirstTrackID(t, db), "RefusedCue", 1, 800)
	_ = db.Close()

	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	snapPath := filepath.Join(dbDir, rbstate.SnapshotName)
	savedSnap, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}

	// 1) Missing snapshot refuses write-back before checking any tracks.
	if err := os.Remove(snapPath); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.ImportState(mount); err == nil {
		t.Fatal("expected ImportState error when snapshot is missing")
	}
	res, err := bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res.TracksChecked != 0 || res.TracksChanged != 0 || res.FilesUpdated != 0 {
		t.Fatalf("missing snapshot: res=%+v err=%v", res, err)
	}
	if err := os.WriteFile(snapPath, savedSnap, 0o600); err != nil {
		t.Fatal(err)
	}

	// 2) Export modified on computer (stamp mismatch) refuses write-back.
	pdbPath := filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb")
	origInfo, err := os.Stat(pdbPath)
	if err != nil {
		t.Fatal(err)
	}
	later := origInfo.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(pdbPath, later, later); err != nil {
		t.Fatal(err)
	}
	st, err := bridge.ImportState(mount)
	if err != nil || st.Matches() {
		t.Fatalf("expected ImportState mismatch after export mtime change: st=%+v err=%v", st, err)
	}
	res, err = bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res.TracksChecked != 0 || res.TracksChanged != 0 || res.FilesUpdated != 0 {
		t.Fatalf("stamp mismatch not refused: res=%+v err=%v", res, err)
	}
	if p3AnalysisHas(t, mount, "RefusedCue") {
		t.Fatal("write-back modified ANLZ despite stamp mismatch")
	}
	_ = os.Chtimes(pdbPath, origInfo.ModTime(), origInfo.ModTime())

	// 3) Pending work refuses write-back.
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	snap.Pending.Perf = 1
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	if st, err := bridge.ImportState(mount); err != nil || st.Header.Pending.None() {
		t.Fatalf("expected ImportState pending work: st=%+v err=%v", st, err)
	}
	res, err = bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res.TracksChecked != 0 || res.TracksChanged != 0 || res.FilesUpdated != 0 {
		t.Fatalf("pending work not refused: res=%+v err=%v", res, err)
	}
}

// Restored #271/#321 (5) (replaces pkg/bridge/cue_writeback_test.go:TestWriteBackCuesEndToEndAndBridgeFastReinsert):
// after WriteBackCues updates ANLZ, exportLibrary.db, and export.pdb, reinserting the stick hits
// the unchanged fast-bypass path (Bypassed == true, 0 rows rewritten).
// Guards pkg/bridge/writeback.go (`NoteWriteBack` updating snapshot Stamp/Sources).
func TestRestored_Issue321_PostWriteBackReinsertIsFastBypass(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 2)
	p3EncryptedOneLibrary(t, mount)
	restoredImportAll(t, mount)

	db := p3OpenDB(t, mount)
	id := p3FirstTrackID(t, db)
	p3DeckCue(t, db, id, "FastReinsertCue", 1, 900)
	if _, err := db.Exec(`UPDATE Track SET comment='AfterWB' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	wb, err := bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || wb.TracksChanged != 1 || !wb.OneLibraryUpdated {
		t.Fatalf("WriteBackCues: res=%+v err=%v", wb, err)
	}

	reinsert, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{})
	if err != nil {
		t.Fatalf("ConvertAtMount after write-back: %v", err)
	}
	if !reinsert.Bypassed || reinsert.Added != 0 || reinsert.Updated != 0 || reinsert.Removed != 0 {
		t.Fatalf("reinsert after write-back was not fast-bypassed: %+v", reinsert)
	}
}

// Restored #271/#321 (6a) (replaces issue321_auto_test.go:TestIssue321UntouchedTracksAreNotRead,
// TestIssue321EditIsFoundAndOnlyItIsRead, and issue321_keys_test.go:TestIssue321OneKeyDerivationPerRun):
// untouched tracks are never read, editing 1 track reads only 1 track with 1 PBKDF2 key derivation,
// and reusing a CueSession across passes derives 0 additional keys.
// Guards pkg/bridge/writeback.go (`readCandidates` synced skip and `Session.keys` reuse).
func TestRestored_Issue321_UntouchedTracksNotReadAndKeyDerivationReused(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	t.Setenv("ENGINEX_RB_SHARED_KEY_CACHE", t.TempDir())
	ctx := context.Background()
	mount := t.TempDir()
	const nTracks = 8
	testfixture.LargeExport(t, mount, nTracks)

	var values []string
	var args []any
	for i := 1; i <= nTracks; i++ {
		values = append(values, "(?, ?, 12800, 4, ?, 44100, '', '/PIONEER/USBANLZ/P001/ANLZ0000.DAT')")
		args = append(args, i, "Track", "/"+testfixture.LargeTrackPath(i))
	}
	onePath := filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")
	restoredWriteEncryptedOneLibrary(t, onePath, `
		PRAGMA journal_mode=DELETE;
		ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
		DELETE FROM content;
		DELETE FROM cue;
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, analysisDataFilePath) VALUES `+strings.Join(values, ","), args...)
	restoredImportAll(t, mount)

	// Pass 0: no edits -> 0 tracks read, 0 key derivations.
	d0 := rekordbox.KeyDerivations()
	res0, err := bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res0.TracksChecked != nTracks || res0.TracksRead != 0 || res0.TracksChanged != 0 {
		t.Fatalf("untouched pass: res=%+v err=%v", res0, err)
	}
	if got := rekordbox.KeyDerivations() - d0; got != 0 {
		t.Fatalf("untouched pass derived keys %d times, want 0", got)
	}

	// Pass 1: edit ONLY track 1 with a fresh CueSession -> TracksRead == 1, 1 key derivation.
	db := p3OpenDB(t, mount)
	p3DeckCue(t, db, 1, "OnlyTrack1", 1, 700)
	var session bridge.CueSession
	t.Setenv("ENGINEX_RB_SHARED_KEY_CACHE", t.TempDir()) // cold cache to measure exact PBKDF2 count
	d1 := rekordbox.KeyDerivations()
	res1, err := bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true, Session: &session})
	if err != nil || res1.TracksChecked != nTracks || res1.TracksRead != 1 || res1.TracksChanged != 1 || !res1.OneLibraryUpdated {
		t.Fatalf("pass 1: res=%+v err=%v", res1, err)
	}
	if got := rekordbox.KeyDerivations() - d1; got != 1 {
		t.Fatalf("single write-back pass derived SQLCipher key %d times, want 1", got)
	}

	// Pass 2: edit ONLY track 2 reusing the same CueSession -> TracksRead == 1, 0 key derivations.
	p3DeckCue(t, db, 2, "OnlyTrack2", 1, 900)
	_ = db.Close()
	d2 := rekordbox.KeyDerivations()
	res2, err := bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true, Session: &session})
	if err != nil || res2.TracksRead != 1 || res2.TracksChanged != 1 || !res2.OneLibraryUpdated {
		t.Fatalf("pass 2 (reused session): res=%+v err=%v", res2, err)
	}
	if got := rekordbox.KeyDerivations() - d2; got != 0 {
		t.Fatalf("reused session derived SQLCipher key %d times, want 0", got)
	}
}

// Restored #328 (replaces pkg/bridge/issue328_reexport_test.go:TestIssue328BReExportTakesTheRatingsTheDeckDidNotChange
// and pkg/bridge/issue328_progress_test.go:TestIssue328BackgroundJobCountsThroughout):
// FillInBackground reports monotonically increasing progress up to N/N, and on re-export a degraded
// track preserves its deck-edited rating while an untouched track takes the new export rating.
// Guards pkg/bridge/fill.go (`filler.report`) and pkg/enginedb/library.go (`UpdateTrack` prevRating guard).
func TestRestored_Issue328_DegradedReExportKeepsDeckRatingAndMonotonicProgress(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()
	mount := t.TempDir()
	const nTracks = 8
	testfixture.LargeExport(t, mount, nTracks)
	_ = os.Remove(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"))
	expPath := filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")
	expDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: expPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = expDB.Exec(`PRAGMA journal_mode=DELETE;
		CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT, bpmx100 INTEGER, length INTEGER, samplingRate INTEGER, djComment TEXT, rating INTEGER, analysisDataFilePath TEXT);`)
	for i := 1; err == nil && i <= nTracks; i++ {
		_, err = expDB.Exec(`INSERT INTO content VALUES(?, ?, ?, 12800, 4, 44100, '', 0, '/PIONEER/USBANLZ/P001/ANLZ0000.DAT')`,
			i, "Track", "/"+testfixture.LargeTrackPath(i))
	}
	_ = expDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{}); err != nil {
		t.Fatal(err)
	}
	type prog struct{ done, total int }
	var reports []prog
	if _, err := bridge.FillInBackground(ctx, mount, bridge.FillOptions{
		Progress: func(done, total int) { reports = append(reports, prog{done, total}) },
	}); err != nil {
		t.Fatalf("FillInBackground: %v", err)
	}
	if len(reports) < nTracks {
		t.Fatalf("expected >= %d progress reports, got %d: %v", nTracks, len(reports), reports)
	}
	for i, r := range reports {
		if r.total != nTracks {
			t.Fatalf("report %d total=%d, want %d", i, r.total, nTracks)
		}
		if i > 0 && r.done < reports[i-1].done {
			t.Fatalf("progress went backwards: %v", reports)
		}
	}
	if last := reports[len(reports)-1]; last.done != nTracks {
		t.Fatalf("final progress=%+v, want %d/%d", last, nTracks, nTracks)
	}
	if _, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{Publish: true}); err != nil {
		t.Fatal(err)
	}

	// Deck edits track 1's rating to 100 (5 stars); track 2 stays untouched.
	db := p3OpenDB(t, mount)
	if _, err := db.Exec(`UPDATE Track SET rating=100 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// Corrupt ANLZ so re-staging track 1 degrades, and re-export track 1 (rating=1 star=20, new comment)
	// and track 2 (rating=4 stars=80, new comment).
	if err := os.WriteFile(filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT"), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	expDB, err = sql.Open("sqlite", (&url.URL{Scheme: "file", Path: expPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expDB.Exec(`UPDATE content SET djComment='Reexp1', rating=20 WHERE content_id=1; UPDATE content SET djComment='Reexp2', rating=80 WHERE content_id=2;`); err != nil {
		_ = expDB.Close()
		t.Fatal(err)
	}
	_ = expDB.Close()
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(expPath, later, later)

	restoredImportAll(t, mount)

	db = p3OpenDB(t, mount)
	var r1, r2 int
	var c1 string
	if err := db.QueryRow(`SELECT rating, COALESCE(comment,'') FROM Track WHERE id=1`).Scan(&r1, &c1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT rating FROM Track WHERE id=2`).Scan(&r2); err != nil {
		t.Fatal(err)
	}
	if r1 != 100 {
		t.Fatalf("degraded re-export overwrote deck rating on track 1: got %d, want 100", r1)
	}
	if c1 != "Reexp1" {
		t.Fatalf("track 1 comment=%q, want Reexp1", c1)
	}
	if r2 != 80 {
		t.Fatalf("untouched track 2 did not take re-exported rating: got %d, want 80", r2)
	}
}

// Restored CF049 + GL-010 (replaces pkg/bridge/delta_test.go:TestCF049Bogus500BPMAndGL010InformationGuard):
// mount-time row insert and row update both drop rekordbox's bogus 500 BPM clamp to 0, and a damaged
// Information row (id != 1, empty uuid, zero currentPlayedIndiciator) is repaired on open.
// Guards pkg/enginedb/library.go:137-145 (GL-010) and pkg/enginedb/library.go:342,750 (CF049).
func TestRestored_CF049_500BPMClampOnMountAndGL010InformationGuard(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 2)
	_ = os.Remove(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"))
	expPath := filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")
	expDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: expPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = expDB.Exec(`
		PRAGMA journal_mode=DELETE;
		CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT, bpmx100 INTEGER, length INTEGER, samplingRate INTEGER, djComment TEXT);
		INSERT INTO content VALUES
			(1, 'ClampedOnInsert', ?, 50000, 4, 44100, ''),
			(2, 'NormalThenClamped', ?, 12400, 4, 44100, '');
	`, "/"+testfixture.LargeTrackPath(1), "/"+testfixture.LargeTrackPath(2))
	_ = expDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Mount-time InsertTracks MUST drop 500 BPM clamp on track 1 to 0.
	if _, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{}); err != nil {
		t.Fatalf("ConvertAtMount(insert): %v", err)
	}
	db := p3OpenDB(t, mount)
	var bpm1, bpm2 int
	var bpmA1 float64
	if err := db.QueryRow(`SELECT bpm, bpmAnalyzed FROM Track WHERE title='ClampedOnInsert'`).Scan(&bpm1, &bpmA1); err != nil {
		t.Fatal(err)
	}
	if bpm1 != 0 || bpmA1 != 0 {
		t.Fatalf("CF049 insert: got bpm=%d bpmAnalyzed=%v, want 0/0", bpm1, bpmA1)
	}
	if err := db.QueryRow(`SELECT bpm FROM Track WHERE title='NormalThenClamped'`).Scan(&bpm2); err != nil {
		t.Fatal(err)
	}
	if bpm2 != 124 {
		t.Fatalf("track 2 bpm=%d, want 124", bpm2)
	}
	_ = db.Close()

	// Re-export track 2 with 500 BPM clamp -> UpdateTrack MUST also drop it to 0.
	expDB, err = sql.Open("sqlite", (&url.URL{Scheme: "file", Path: expPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expDB.Exec(`UPDATE content SET bpmx100=50000 WHERE content_id=2`); err != nil {
		_ = expDB.Close()
		t.Fatal(err)
	}
	_ = expDB.Close()
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(expPath, later, later)
	if _, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{}); err != nil {
		t.Fatalf("ConvertAtMount(update): %v", err)
	}
	db = p3OpenDB(t, mount)
	var bpm2After int
	if err := db.QueryRow(`SELECT bpm FROM Track WHERE title='NormalThenClamped'`).Scan(&bpm2After); err != nil {
		t.Fatal(err)
	}
	if bpm2After != 0 {
		t.Fatalf("CF049 update: got bpm=%d, want 0", bpm2After)
	}

	// GL-010: damage Information row and verify OpenLibrary / ConvertAtMount repairs it.
	if _, err := db.Exec(`UPDATE Information SET id=7, uuid='', currentPlayedIndiciator=0`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	lib, err := enginedb.OpenLibrary(ctx, filepath.Join(mount, "Engine Library", "Database2"))
	if err != nil {
		t.Fatalf("OpenLibrary after Information damage: %v", err)
	}
	_ = lib.Close()
	db = p3OpenDB(t, mount)
	var infoID, played int
	var infoUUID string
	if err := db.QueryRow(`SELECT id, uuid, currentPlayedIndiciator FROM Information`).Scan(&infoID, &infoUUID, &played); err != nil {
		t.Fatal(err)
	}
	if infoID != 1 || strings.TrimSpace(infoUUID) == "" || played == 0 {
		t.Fatalf("GL-010 Information not repaired: id=%d uuid=%q played=%d", infoID, infoUUID, played)
	}
}
