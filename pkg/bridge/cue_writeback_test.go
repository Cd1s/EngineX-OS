package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func setupWriteBackStick(t *testing.T, withOneLibrary bool) string {
	t.Helper()
	stick := t.TempDir()
	testfixture.LargeExport(t, stick, 2)
	if withOneLibrary {
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
		_, err = db.Exec(`
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
		_ = db.Close()
		if err != nil {
			t.Fatal(err)
		}
		in, err := os.Open(tmpPlain)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		st, err := in.Stat()
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(filepath.Join(stick, "PIONEER", "rekordbox", "exportLibrary.db"))
		if err != nil {
			t.Fatal(err)
		}
		err = rekordbox.EncryptSQLCipher4(context.Background(), in, st.Size(), out, salt, []byte("r8gddnr4k847830ar6cqzbkk0el6qytmb3trbbx805jm74vez64i5o8fnrqryqls"))
		_ = out.Close()
		if err != nil {
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
	for _, tc := range []struct {
		name       string
		withOneLib bool
	}{
		{name: "classic PDB", withOneLib: false},
		{name: "rekordbox 7 OneLibrary", withOneLib: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stick := setupWriteBackStick(t, tc.withOneLib)
			if _, err := bridge.Convert(context.Background(), stick, enginedb.Options{}); err != nil {
				t.Fatal(err)
			}
			injectDeckMetadataEdit(t, stick)
			res, err := bridge.WriteBackCues(context.Background(), stick, bridge.WriteBackOptions{DateDir: "2026-11-19", SkipPreparingCheck: true})
			if err != nil {
				t.Fatal(err)
			}
			if res.TracksChanged != 1 {
				t.Fatalf("TracksChanged=%d, want 1: %+v", res.TracksChanged, res)
			}
			var lib *rekordbox.Library
			if tc.withOneLib {
				lib, err = rekordbox.ReadSQLite(context.Background(), filepath.Join(stick, "PIONEER/rekordbox/exportLibrary.db"), nil)
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
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)

	if _, err := bridge.Convert(ctx, stick, enginedb.Options{}); err != nil {
		t.Fatalf("initial Convert: %v", err)
	}
	injectDeckCueEdit(t, stick)

	anlzRel := filepath.Join("PIONEER", "USBANLZ", "P001", "ANLZ0000.DAT")
	origDAT, err := os.ReadFile(filepath.Join(stick, anlzRel))
	if err != nil {
		t.Fatal(err)
	}

	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{DateDir: "2026-11-16", SkipPreparingCheck: true})
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
	res2, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{DateDir: "2026-11-17", SkipPreparingCheck: true})
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
			"PRAGMA key = 'r8gddnr4k847830ar6cqzbkk0el6qytmb3trbbx805jm74vez64i5o8fnrqryqls';",
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
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := bridge.Convert(ctx, stick, enginedb.Options{}); err != nil {
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
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := bridge.Convert(ctx, stick, enginedb.Options{}); err != nil {
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
	ctx := context.Background()
	stick := setupWriteBackStick(t, false)
	if _, err := bridge.Convert(ctx, stick, enginedb.Options{}); err != nil {
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
	ctx := context.Background()
	stick := setupWriteBackStick(t, false)
	// Write a corrupt exportLibrary.db alongside the valid export.pdb.
	// bridge.Convert falls back to export.pdb; WriteBackCues must do the same.
	oneLibPath := filepath.Join(stick, "PIONEER", "rekordbox", "exportLibrary.db")
	if err := os.WriteFile(oneLibPath, []byte("not-a-sqlcipher-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Convert(ctx, stick, enginedb.Options{}); err != nil {
		t.Fatalf("Convert with corrupt exportLibrary.db: %v", err)
	}
	injectDeckCueEdit(t, stick)

	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{
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
