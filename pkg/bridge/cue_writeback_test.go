package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// oneLibraryKey is rekordbox's public OneLibrary passphrase (pkg/rekordbox oneLibraryPassphrase),
// once for every test of this package.
const oneLibraryKey = "r8gddnr4k847830ar6cqzbkk0el6qytmb3trbbx805jm74vez64i5o8fnrqryqls"

// testKeys keeps the SQLCipher keys of this test process. Every fixture here is encrypted with
// oneLibraryKey and the salt of testdata/sqlcipher4_onelibrary.db, so the 256 000-round
// derivation (a second or more under -race) is made once, not once per read and write.
var testKeys = rekordbox.NewKeyCache()

var (
	oneLibraryMu       sync.Mutex
	oneLibraryFixtures = map[string][]byte{}
)

// encryptedOneLibrary is the OneLibrary test database with statements applied, encrypted with
// SQLCipher 4. Each variant is built once per test process (its encryption derives the key);
// every test writes its own copy of the bytes to its own stick and reads and writes only that.
func encryptedOneLibrary(t *testing.T, variant, statements string, args ...any) []byte {
	t.Helper()
	oneLibraryMu.Lock()
	defer oneLibraryMu.Unlock()
	if b, ok := oneLibraryFixtures[variant]; ok {
		return b
	}
	srcPlain := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary_decrypted.db")
	srcEnc := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary.db")
	salt, err := rekordbox.ReadSQLCipherSalt(srcEnc)
	if err != nil {
		t.Fatal(err)
	}
	tmpPlain := filepath.Join(t.TempDir(), "onelib-plain.db")
	rawPlain, err := os.ReadFile(srcPlain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpPlain, rawPlain, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: tmpPlain}).String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(statements, args...)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	plain, err := os.ReadFile(tmpPlain)
	if err != nil {
		t.Fatal(err)
	}
	var enc bytes.Buffer
	if err = rekordbox.EncryptSQLCipher4(context.Background(), bytes.NewReader(plain), int64(len(plain)), &enc, salt, []byte(oneLibraryKey)); err != nil {
		t.Fatal(err)
	}
	oneLibraryFixtures[variant] = enc.Bytes()
	return oneLibraryFixtures[variant]
}

func setupWriteBackStick(t *testing.T, withOneLibrary bool) string {
	t.Helper()
	stick := t.TempDir()
	testfixture.LargeExport(t, stick, 2)
	if withOneLibrary {
		export := encryptedOneLibrary(t, "classic", `
			PRAGMA journal_mode=DELETE;
			ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
			ALTER TABLE content ADD COLUMN genre_id INTEGER;
			ALTER TABLE content ADD COLUMN label_id INTEGER;
			CREATE TABLE genre(genre_id INTEGER PRIMARY KEY, name TEXT);
			CREATE TABLE label(label_id INTEGER PRIMARY KEY, name TEXT);
			INSERT INTO genre VALUES (20, 'Old Genre');
			INSERT INTO label VALUES (30, 'Old Label');
			DELETE FROM content;
			DELETE FROM cue;
			INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, analysisDataFilePath, genre_id, label_id) VALUES
				(1, 'Track 1', 12800, 4, ?, 44100, 'Old Comment', '/PIONEER/USBANLZ/P001/ANLZ0000.DAT', 20, 30),
				(2, 'Track 2', 12400, 4, ?, 44100, '', '/PIONEER/USBANLZ/P001/ANLZ0000.DAT', 20, 30);
		`, "/"+testfixture.LargeTrackPath(1), "/"+testfixture.LargeTrackPath(2))
		if err := os.WriteFile(filepath.Join(stick, "PIONEER", "rekordbox", "exportLibrary.db"), export, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return stick
}

func injectDeckMetadataEdit(t *testing.T, stick string) {
	t.Helper()
	mPath := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	genre := strings.Repeat("G", 140)
	label := strings.Repeat("标签", 100)
	if _, err := db.Exec(`UPDATE Track SET genre=?, comment='New Comment', label=? WHERE id=1`, genre, label); err != nil {
		t.Fatal(err)
	}
}

func TestWriteBackTrackMetadataToPDBAndOneLibrary(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	for _, tc := range []struct {
		name       string
		withOneLib bool
	}{
		{name: "classic PDB", withOneLib: false},
		{name: "rekordbox 7 OneLibrary", withOneLib: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stick := setupWriteBackStick(t, tc.withOneLib)
			if _, err := convertWriteBackAtTest(context.Background(), stick); err != nil {
				t.Fatal(err)
			}
			injectDeckMetadataEdit(t, stick)
			res, err := bridge.WriteBackCues(context.Background(), stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-19", SkipPreparingCheck: true})
			if err != nil {
				t.Fatal(err)
			}
			if res.TracksChanged != 1 {
				t.Fatalf("TracksChanged=%d, want 1: %+v", res.TracksChanged, res)
			}
			var lib *rekordbox.Library
			if tc.withOneLib {
				lib, err = rekordbox.ReadSQLiteKeys(context.Background(), filepath.Join(stick, "PIONEER/rekordbox/exportLibrary.db"), nil, testKeys)
			} else {
				lib, err = rekordbox.ReadPDB(filepath.Join(stick, "PIONEER/rekordbox/export.pdb"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(lib.Tracks) == 0 || lib.Tracks[0].Genre != strings.Repeat("G", 140) || lib.Tracks[0].Comment != "New Comment" || lib.Tracks[0].Label != strings.Repeat("标签", 100) {
				t.Fatalf("track metadata=%+v", lib.Tracks)
			}
		})
	}
}

func injectDeckCueEdit(t *testing.T, stick string) {
	t.Helper()
	mPath := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	u := url.URL{Scheme: "file", Path: mPath}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{
		Beats: []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 1000, Name: "DeckCueA", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
			{HotCue: 2, TimeMS: 2000, EndMS: 3000, Loop: true, Name: "DeckLoopB", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=?, loops=? WHERE trackId=1`, perf.TrackData, perf.QuickCues, perf.Loops); err != nil {
		t.Fatal(err)
	}
}

func TestWriteBackCuesEndToEndAndBridgeFastReinsert(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)

	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatalf("initial Convert: %v", err)
	}
	injectDeckCueEdit(t, stick)

	anlzRel := filepath.Join("PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT")
	origDAT, err := os.ReadFile(filepath.Join(stick, anlzRel))
	if err != nil {
		t.Fatal(err)
	}

	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-16", SkipPreparingCheck: true})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if res.TracksChecked != 2 || res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("unexpected WriteBackResult: %+v", res)
	}

	// Backup exists in /.enginex-backup/2026-11-16/ and mirrors relative paths; no .orig in PIONEER/.
	bakDAT, err := os.ReadFile(filepath.Join(stick, ".enginex-backup", "2026-11-16", anlzRel))
	if err != nil || !bytes.Equal(bakDAT, origDAT) {
		t.Fatalf("backup DAT mismatch: err=%v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(stick, "PIONEER", "USBANLZ", "P001"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".orig") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("stray file in PIONEER/: %s", e.Name())
		}
	}

	// Second run with no new edits is a no-op (TracksChanged == 0).
	res2, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-17", SkipPreparingCheck: true})
	if err != nil {
		t.Fatalf("second WriteBackCues: %v", err)
	}
	if res2.TracksChanged != 0 {
		t.Fatalf("idempotent second run TracksChanged=%d, want 0", res2.TracksChanged)
	}
	if _, err := os.Stat(filepath.Join(stick, ".enginex-backup", "2026-11-17", anlzRel)); !os.IsNotExist(err) {
		t.Fatalf("expected only first original backup to be kept, got err=%v", err)
	}

	// Verify updated ANLZ files (.DAT, .EXT) and intact .2EX.
	for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
		b, err := os.ReadFile(filepath.Join(stick, "PIONEER", "USBANLZ", "P001", "ANLZ0000"+ext))
		if err != nil {
			t.Fatal(err)
		}
		a, err := rekordbox.ParseAnalysis(b)
		if err != nil {
			t.Fatalf("ParseAnalysis %s: %v", ext, err)
		}
		if ext != ".2EX" && len(a.Cues) < 2 {
			t.Fatalf("%s expected >=2 cues, got %d", ext, len(a.Cues))
		}
	}

	// Verify re-encrypted exportLibrary.db via sqlcipher CLI and via our own bridge re-insert.
	oneLibPath := filepath.Join(stick, "PIONEER", "rekordbox", "exportLibrary.db")
	if sqlcipherBin, err := exec.LookPath("sqlcipher"); err == nil {
		out, err := exec.Command(sqlcipherBin, oneLibPath,
			"PRAGMA key = '"+oneLibraryKey+"';",
			"PRAGMA integrity_check;",
		).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "ok") {
			t.Fatalf("sqlcipher integrity_check failed: %v\n%s", err, out)
		}
	}

	// Next insert on a Denon deck must still read the stick cleanly and stay fast.
	rNext, err := bridge.Convert(ctx, stick, enginedb.Options{})
	if err != nil {
		t.Fatalf("re-insert Convert after writeback: %v", err)
	}
	if rNext.Added+rNext.Updated+rNext.Unchanged != 2 {
		t.Fatalf("expected re-insert Convert to see 2 tracks, got %+v", rNext)
	}
}

func TestWriteBackCuesPullOutMidWriteLeavesOriginalIntact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatal(err)
	}
	injectDeckCueEdit(t, stick)

	datPath := filepath.Join(stick, "PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT")
	origDAT, err := os.ReadFile(datPath)
	if err != nil {
		t.Fatal(err)
	}
	simErr := errors.New("simulated pull-out")
	_, err = bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{
		Keys:               testKeys,
		DateDir:            "2026-11-16",
		SkipPreparingCheck: true,
		BeforeRename: func(dst string) error {
			return simErr
		},
	})
	if !errors.Is(err, simErr) {
		t.Fatalf("expected simulated pull-out error, got %v", err)
	}
	afterDAT, err := os.ReadFile(datPath)
	if err != nil || !bytes.Equal(afterDAT, origDAT) {
		t.Fatalf("original DAT was modified after aborted write")
	}
}

func TestWriteBackCuesFailureBeforeSecondRenameLeavesStickUntouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatal(err)
	}
	injectDeckCueEdit(t, stick)

	paths := []string{
		filepath.Join(stick, "PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT"),
		filepath.Join(stick, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"),
		filepath.Join(stick, "PIONEER", "USBANLZ", "P001", "ANLZ0000.2EX"),
		filepath.Join(stick, "PIONEER", "rekordbox", "exportLibrary.db"),
	}
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}

	mDB := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mDB}).String())
	if err != nil {
		t.Fatal(err)
	}
	var engineBefore struct{ genre, comment, label string }
	if err := db.QueryRow(`SELECT genre, comment, label FROM Track WHERE id=1`).Scan(&engineBefore.genre, &engineBefore.comment, &engineBefore.label); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	calls := 0
	simErr := errors.New("simulated pull-out after staging")
	_, err = bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{
		Keys:               testKeys,
		DateDir:            "2026-11-19",
		SkipPreparingCheck: true,
		BeforeRename: func(string) error {
			calls++
			if calls == 2 {
				return simErr
			}
			return nil
		},
	})
	if !errors.Is(err, simErr) {
		t.Fatalf("expected simulated pull-out error, got %v", err)
	}
	if calls < 2 {
		t.Fatalf("BeforeRename calls=%d, want at least 2", calls)
	}
	for path, want := range before {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s changed after aborted write: err=%v", path, readErr)
		}
	}
	var genre, comment, label string
	db, err = sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mDB}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT genre, comment, label FROM Track WHERE id=1`).Scan(&genre, &comment, &label); err != nil {
		t.Fatal(err)
	}
	if genre != engineBefore.genre || comment != engineBefore.comment || label != engineBefore.label {
		t.Fatalf("Engine database changed unexpectedly: %q/%q/%q", genre, comment, label)
	}
}

func TestWriteBackCuesReadsUncheckpointedWAL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stick := setupWriteBackStick(t, false)
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatal(err)
	}

	mPath := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{
		Beats: []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 1000, Name: "WALCue", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
			{HotCue: 3, TimeMS: 1500, Name: "WALCue3", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
		},
	})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=?, loops=? WHERE trackId = 1`, perf.TrackData, perf.QuickCues, perf.Loops); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	defer db.Close()

	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{
		Keys:               testKeys,
		DateDir:            "2026-11-17",
		SkipPreparingCheck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TracksChanged != 1 {
		t.Fatalf("TracksChanged = %d, want 1 (uncheckpointed WAL edit missed)", res.TracksChanged)
	}
}

func TestWriteBackCuesFallsBackToPDBWhenOneLibUnreadable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stick := setupWriteBackStick(t, false)
	// Write a corrupt exportLibrary.db alongside the valid export.pdb.
	// bridge.Convert falls back to export.pdb; WriteBackCues must do the same.
	oneLibPath := filepath.Join(stick, "PIONEER", "rekordbox", "exportLibrary.db")
	if err := os.WriteFile(oneLibPath, []byte("not-a-sqlcipher-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatalf("Convert with corrupt exportLibrary.db: %v", err)
	}
	injectDeckCueEdit(t, stick)

	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{
		Keys:               testKeys,
		DateDir:            "2026-11-17",
		SkipPreparingCheck: true,
	})
	if err != nil {
		t.Fatalf("WriteBackCues should fall back to export.pdb: %v", err)
	}
	if res.TracksChanged != 1 || res.FilesUpdated == 0 || res.OneLibraryUpdated {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestWriteBackCuesDotPioneerMacOSStickAndExactPadSlots(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	stick := t.TempDir()
	testfixture.LargeExport(t, stick, 2)

	// Simulate a real macOS HFS+ rekordbox 7 stick: hidden .PIONEER root and
	// analysisDataFilePath starting with /.PIONEER/USBANLZ/... plus full
	// Device Library Plus change-tracking and 150fps cue columns.
	if err := os.Rename(filepath.Join(stick, "PIONEER"), filepath.Join(stick, ".PIONEER")); err != nil {
		t.Fatal(err)
	}
	oneLibPath := filepath.Join(stick, ".PIONEER", "rekordbox", "exportLibrary.db")
	export := encryptedOneLibrary(t, "macos", `
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
	if err := os.WriteFile(oneLibPath, export, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatalf("ConvertAtMount(.PIONEER): %v", err)
	}

	// Simulate deck edits on track 1 (using the MP3 file's real RekordboxOffset
	// so Engine's decoded sample positions match what an Engine deck writes):
	// - Hot Cue A (pad 1) at 1000ms
	// - Hot Loop B (pad 2) at 2000..3000ms, activeOnLoadLoops bit 1 set
	// - Hot Cue H (pad 8) at 3500ms
	// - Memory Cue at 250ms (sets MainCue = 250ms in quickCues)
	// - Independent Saved Loop (memory loop) at 400..800ms
	audioInfo, err := compat.AudioHeader(filepath.Join(stick, filepath.FromSlash(testfixture.LargeTrackPath(1))))
	if err != nil {
		t.Fatal(err)
	}
	perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100, StartOffsetSamples: audioInfo.RekordboxOffset()}, &rekordbox.Analysis{
		Beats: []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 1000, Name: "Pad A", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
			{HotCue: 2, TimeMS: 2000, EndMS: 3000, Loop: true, Active: true, Name: "Loop B", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
			{HotCue: 8, TimeMS: 3500, Name: "Pad H", Color: [4]byte{255, 0xff, 0x37, 0x6f}, ColorCode: 45},
			{HotCue: 0, TimeMS: 250, Name: "Mémoire", ColorID: 5},
			{HotCue: 0, TimeMS: 400, EndMS: 800, Loop: true, Name: "Saved Loop", ColorID: 3},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mPath := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	mdb, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = mdb.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=?, loops=?, activeOnLoadLoops=? WHERE trackId=1`,
		perf.TrackData, perf.QuickCues, perf.Loops, perf.ActiveLoops)
	_ = mdb.Close()
	if err != nil {
		t.Fatal(err)
	}

	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-21", SkipPreparingCheck: true})
	if err != nil {
		t.Fatalf("WriteBackCues(.PIONEER): %v", err)
	}
	if res.TracksChecked != 2 || res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("unexpected WriteBackResult: %+v", res)
	}

	// Verify .PIONEER/USBANLZ/P001/ANLZ0000.EXT has exact Hot Cue pads 1, 2 (active loop), 8 — no shift, no duplicates, no synthetic "CUE" hot cue.
	extBytes, err := os.ReadFile(filepath.Join(stick, ".PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}
	aExt, err := rekordbox.ParseAnalysis(extBytes)
	if err != nil {
		t.Fatal(err)
	}
	hotByPad := map[uint32][]rekordbox.Cue{}
	var memLoops []rekordbox.Cue
	for _, c := range aExt.Cues {
		if c.HotCue > 0 {
			hotByPad[c.HotCue] = append(hotByPad[c.HotCue], c)
		} else if c.Loop {
			memLoops = append(memLoops, c)
		}
		if c.Name == "CUE" {
			t.Fatalf("synthetic Main CUE leaked into written cues: %+v", aExt.Cues)
		}
	}
	if len(hotByPad) != 3 {
		t.Fatalf("expected exactly 3 hot cue pads (1,2,8), got %+v", hotByPad)
	}
	if len(hotByPad[1]) != 1 || hotByPad[1][0].Loop || hotByPad[1][0].Name != "Pad A" || hotByPad[1][0].TimeMS != 1000 {
		t.Fatalf("Hot Cue A mismatch: %+v", hotByPad[1])
	}
	if len(hotByPad[2]) != 1 || !hotByPad[2][0].Loop || !hotByPad[2][0].Active || hotByPad[2][0].Name != "Loop B" || hotByPad[2][0].TimeMS != 2000 || hotByPad[2][0].EndMS != 3000 {
		t.Fatalf("Hot Loop B mismatch: %+v", hotByPad[2])
	}
	if len(hotByPad[8]) != 1 || hotByPad[8][0].Loop || hotByPad[8][0].Name != "Pad H" || hotByPad[8][0].TimeMS != 3500 {
		t.Fatalf("Hot Cue H mismatch: %+v", hotByPad[8])
	}
	var foundSavedLoop bool
	for _, ml := range memLoops {
		if ml.TimeMS == 400 && ml.EndMS == 800 && ml.Name == "Saved Loop" {
			foundSavedLoop = true
		}
	}
	if !foundSavedLoop {
		t.Fatalf("independent Saved Loop missing from memory loops: %+v", memLoops)
	}

	// Verify exportLibrary.db has exact cues and round-trips through ReadSQLite.
	lib, err := rekordbox.ReadSQLiteKeys(ctx, oneLibPath, nil, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	var tr1 rekordbox.Track
	for _, tr := range lib.Tracks {
		if tr.ID == 1 {
			tr1 = tr
		}
	}
	var sawA, sawB, sawH bool
	for _, c := range tr1.Cues {
		switch c.HotCue {
		case 1:
			sawA = !c.Loop && c.Name == "Pad A" && c.TimeMS == 1000
		case 2:
			sawB = c.Loop && c.Active && c.Name == "Loop B" && c.TimeMS == 2000 && c.EndMS == 3000
		case 8:
			sawH = !c.Loop && c.Name == "Pad H" && c.TimeMS == 3500
		}
	}
	if !sawA || !sawB || !sawH {
		t.Fatalf("exportLibrary.db track 1 cues mismatch (sawA=%v sawB=%v sawH=%v): %+v", sawA, sawB, sawH, tr1.Cues)
	}

	// Re-inserting the stick without rekordbox edits must fast-bypass (stamp refreshed).
	rMount, err := bridge.ConvertAtMount(ctx, stick, enginedb.Options{}, false)
	if err != nil {
		t.Fatalf("re-insert ConvertAtMount: %v", err)
	}
	if !rMount.Bypassed {
		t.Fatalf("expected re-insert ConvertAtMount to bypass after writeback stamp refresh, got %+v", rMount)
	}

	// Second write-back with no new edits must be a no-op.
	res2, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true})
	if err != nil {
		t.Fatalf("second WriteBackCues: %v", err)
	}
	if res2.TracksChanged != 0 {
		t.Fatalf("second WriteBackCues TracksChanged=%d, want 0", res2.TracksChanged)
	}
}

func TestWriteBackCuesRestoresMP3RekordboxOffsetAndSkipsUntouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stick := t.TempDir()
	const trackRel = "Contents/mp3/offset.mp3"
	copyMP3Fixture(t, stick, trackRel, "01-lame-cbr-info-44100.mp3")
	audioInfo, err := compat.AudioHeader(filepath.Join(stick, filepath.FromSlash(trackRel)))
	if err != nil {
		t.Fatal(err)
	}
	offset := float64(audioInfo.RekordboxOffset())
	if offset <= 0 {
		t.Fatalf("expected positive MP3 RekordboxOffset, got %v", offset)
	}

	// Build a minimal exportLibrary.db + ANLZ (.DAT and .EXT) with Hot Cue A at 40ms
	// (within the short 1878-byte MP3 test fixture's sample count, and > 25ms encoder offset).
	anlzRel := "PIONEER/USBANLZ/P001/ANLZ0000.DAT"
	baseANLZ := testfixture.DAT()
	initCues := []rekordbox.Cue{
		{HotCue: 1, TimeMS: 40, Name: "OrigA", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
	}
	datBytes, _, err := rekordbox.RewriteANLZCues(baseANLZ, initCues, false, false)
	if err != nil {
		t.Fatal(err)
	}
	extBytes, _, err := rekordbox.RewriteANLZCues(testfixture.EXT(), initCues, true, false)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, stick, anlzRel, datBytes)
	testfixture.Write(t, stick, "PIONEER/USBANLZ/P001/ANLZ0000.EXT", extBytes)
	expPath := filepath.Join(stick, "PIONEER", "rekordbox", "exportLibrary.db")
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
	`, "/"+trackRel, audioInfo.SampleRate, "/"+anlzRel)
	_ = expDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatalf("ConvertAtMount: %v", err)
	}

	// Even if BridgeWritten is cleared (simulating older bridge state), restoring
	// RekordboxOffset must match the original 40ms cue with zero spurious diff.
	statePath := filepath.Join(stick, "Engine Library", "Database2", ".rb-bridge-state.db")
	sdb, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: statePath}).String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sdb.Exec(`DELETE FROM BridgeWritten`)
	_ = sdb.Close()

	resUntouched, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-21", SkipPreparingCheck: true})
	if err != nil {
		t.Fatalf("WriteBackCues untouched MP3: %v", err)
	}
	if resUntouched.TracksChanged != 0 {
		t.Fatalf("untouched MP3 falsely marked changed (missing RekordboxOffset restoration?): %+v", resUntouched)
	}

	// Now add Hot Cue B on the deck at Engine's 65ms-minus-offset position.
	perf, err := enginedb.EncodePerformance(
		rekordbox.Track{Duration: 10, SampleRate: audioInfo.SampleRate, SampleCount: audioInfo.Samples, StartOffsetSamples: audioInfo.RekordboxOffset()},
		&rekordbox.Analysis{
			Beats: []rekordbox.Beat{{TimeMS: 40, BPM: 120, Number: 1}},
			Cues: []rekordbox.Cue{
				{HotCue: 1, TimeMS: 40, Name: "OrigA", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
				{HotCue: 2, TimeMS: 65, Name: "NewB", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	mPath := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	mdb, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = mdb.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=?, loops=? WHERE trackId=1`,
		perf.TrackData, perf.QuickCues, perf.Loops)
	_ = mdb.Close()
	if err != nil {
		t.Fatal(err)
	}

	resEdit, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-21", SkipPreparingCheck: true})
	if err != nil {
		t.Fatalf("WriteBackCues edited MP3: %v", err)
	}
	if resEdit.TracksChanged != 1 {
		t.Fatalf("TracksChanged=%d, want 1", resEdit.TracksChanged)
	}
	afterExt, err := os.ReadFile(filepath.Join(stick, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}
	aAfter, err := rekordbox.ParseAnalysis(afterExt)
	if err != nil {
		t.Fatal(err)
	}
	gotMS := map[uint32]uint32{}
	for _, c := range aAfter.Cues {
		if c.HotCue > 0 {
			gotMS[c.HotCue] = c.TimeMS
		}
	}
	if math.Abs(float64(gotMS[1])-40) > 1 || math.Abs(float64(gotMS[2])-65) > 1 {
		t.Fatalf("MP3 cues shifted by encoder offset: got pad1=%dms pad2=%dms, want ~40ms and ~65ms", gotMS[1], gotMS[2])
	}
}

func TestWriteBackCuesDetectsActiveLoopDeactivation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stick := t.TempDir()
	testfixture.Export(t, stick)
	// Seed track 1 (2s WAV) with an active hot loop in .DAT and .EXT, then import into Engine.
	anlzBase := filepath.Join(stick, "PIONEER", "USBANLZ", "P001", "ANLZ0000")
	initCues := []rekordbox.Cue{
		{HotCue: 1, TimeMS: 500, EndMS: 1500, Loop: true, Active: true, Name: "ActLoop", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
	}
	for _, tc := range []struct {
		ext      string
		extended bool
	}{
		{".DAT", false},
		{".EXT", true},
	} {
		raw, err := os.ReadFile(anlzBase + tc.ext)
		if err != nil {
			t.Fatal(err)
		}
		updated, _, err := rekordbox.RewriteANLZCues(raw, initCues, tc.extended, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(anlzBase+tc.ext, updated, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatalf("ConvertAtMount: %v", err)
	}
	// Deactivate the active loop on the deck without altering beatData/quickCues/loops
	// (so BridgeWritten digest still matches).
	mPath := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	mdb, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE PerformanceData SET activeOnLoadLoops = 0 WHERE trackId = 1`); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE Track SET lastEditTime = lastEditTime + 2 WHERE id = 1`); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	_ = mdb.Close()

	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-21", SkipPreparingCheck: true})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if res.TracksChanged != 1 {
		t.Fatalf("expected active-loop deactivation to be detected, got %+v", res)
	}
	afterExt, err := os.ReadFile(anlzBase + ".EXT")
	if err != nil {
		t.Fatal(err)
	}
	aAfter, err := rekordbox.ParseAnalysis(afterExt)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range aAfter.Cues {
		if c.HotCue == 1 && c.Loop {
			if c.Active {
				t.Fatalf("expected Hot Loop A Active=false after deactivation, got %+v", c)
			}
			return
		}
	}
	t.Fatalf("Hot Loop A missing after writeback: %+v", aAfter.Cues)
}

func TestWriteBackCuesRoundTripAddThenDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stick := t.TempDir()
	testfixture.Export(t, stick)
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatalf("ConvertAtMount: %v", err)
	}
	mPath := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	mdb, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	var origQuick []byte
	if err := mdb.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId = 1`).Scan(&origQuick); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	// Session 1: set Hot Cue A on deck and write back on eject.
	perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 2, SampleRate: 44100}, &rekordbox.Analysis{
		Beats: []rekordbox.Beat{{TimeMS: 250, BPM: 120, Number: 1}},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 750, Name: "Drop", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
		},
	})
	if err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE PerformanceData SET quickCues = ? WHERE trackId = 1`, perf.QuickCues); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE Track SET lastEditTime = lastEditTime + 2 WHERE id = 1`); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	_ = mdb.Close()

	res1, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-21", OnEject: true})
	if err != nil || res1.TracksChanged != 1 {
		t.Fatalf("session 1 WriteBackCues: res=%+v err=%v", res1, err)
	}

	// Session 2: user deletes Hot Cue A on deck, restoring the exact original
	// import quickCues blob (and therefore the original BridgeWritten digest).
	mdb2, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mdb2.Exec(`UPDATE PerformanceData SET quickCues = ? WHERE trackId = 1`, origQuick); err != nil {
		_ = mdb2.Close()
		t.Fatal(err)
	}
	if _, err := mdb2.Exec(`UPDATE Track SET lastEditTime = lastEditTime + 2 WHERE id = 1`); err != nil {
		_ = mdb2.Close()
		t.Fatal(err)
	}
	_ = mdb2.Close()

	res2, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-21", OnEject: true})
	if err != nil || res2.TracksChanged != 1 {
		t.Fatalf("session 2 delete WriteBackCues: expected deletion to be written back, got res=%+v err=%v", res2, err)
	}
	extBytes, err := os.ReadFile(filepath.Join(stick, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := rekordbox.ParseAnalysis(extBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range a.Cues {
		if c.HotCue == 1 {
			t.Fatalf("Hot Cue A still present on stick after deletion writeback: %+v", c)
		}
	}
}
