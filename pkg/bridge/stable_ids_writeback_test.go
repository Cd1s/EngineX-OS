package bridge_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestQuickUpdateAndFullUpdateKeepStableIDsAndUUIDWhenSourceIDsShift(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	root := t.TempDir()
	testfixture.LargeExport(t, root, 4)
	writeOneLib := func(variant string, offset time.Duration, sqlScript string, args ...any) {
		t.Helper()
		export := encryptedOneLibrary(t, variant, sqlScript, args...)
		p := filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db")
		if err := os.WriteFile(p, export, 0o644); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(offset)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	writeOneLib("shifted-v1", 20*time.Second, `
		PRAGMA journal_mode=DELETE;
		ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
		CREATE TABLE playlist(playlist_id INTEGER PRIMARY KEY, name TEXT, playlist_id_parent INTEGER, attribute INTEGER);
		CREATE TABLE playlist_content(playlist_id INTEGER, content_id INTEGER, sequenceNo INTEGER);
		DELETE FROM content;
		DELETE FROM cue;
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, analysisDataFilePath) VALUES
			(1, 'T1', 12800, 4, ?, 44100, '/PIONEER/USBANLZ/P001/ANLZ0000.DAT'),
			(2, 'T2', 12400, 4, ?, 44100, '/PIONEER/USBANLZ/P001/ANLZ0000.DAT'),
			(3, 'T3', 12200, 4, ?, 44100, '/PIONEER/USBANLZ/P001/ANLZ0000.DAT'),
			(4, 'T4', 12600, 4, ?, 44100, '/PIONEER/USBANLZ/P001/ANLZ0000.DAT');
		INSERT INTO playlist(playlist_id, name, playlist_id_parent, attribute) VALUES (10, 'Set A', 0, 0);
		INSERT INTO playlist_content(playlist_id, content_id, sequenceNo) VALUES (10, 1, 1), (10, 2, 2), (10, 3, 3), (10, 4, 4);
	`, "/"+testfixture.LargeTrackPath(1), "/"+testfixture.LargeTrackPath(2), "/"+testfixture.LargeTrackPath(3), "/"+testfixture.LargeTrackPath(4))

	defer bridge.SetLargeExportForTest()()
	ctx := context.Background()

	// First import -> fast -> full publish.
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatalf("first mount: %v", err)
	}
	if _, err := bridge.ImportInBackground(ctx, root, nil); err != nil {
		t.Fatalf("fast build: %v", err)
	}
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatalf("fast publish: %v", err)
	}
	if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("full fill: %+v %v", r, err)
	}
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatalf("full publish: %v", err)
	}

	dir := filepath.Join(root, "Engine Library", "Database2")
	readIDsAndUUID := func() (string, map[string]int64) {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var uuid string
		if err := db.QueryRow(`SELECT uuid FROM Information LIMIT 1`).Scan(&uuid); err != nil {
			t.Fatal(err)
		}
		rows, err := db.Query(`SELECT id, path FROM Track`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var id int64
			var p string
			if err := rows.Scan(&id, &p); err != nil {
				t.Fatal(err)
			}
			out[p] = id
		}
		return uuid, out
	}

	uuidBefore, idsBefore := readIDsAndUUID()
	if len(idsBefore) != 4 {
		t.Fatalf("initial tracks=%d, want 4", len(idsBefore))
	}

	// Re-export: shift every source ID (including reusing old ID 1 for Track 2, old ID 2 for Track 3,
	// old ID 3 for Track 4, and assigning Track 1 a new ID 101) plus changed playlist!
	writeOneLib("shifted-v2", 40*time.Second, `
		PRAGMA journal_mode=DELETE;
		ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
		CREATE TABLE playlist(playlist_id INTEGER PRIMARY KEY, name TEXT, playlist_id_parent INTEGER, attribute INTEGER);
		CREATE TABLE playlist_content(playlist_id INTEGER, content_id INTEGER, sequenceNo INTEGER);
		DELETE FROM content;
		DELETE FROM cue;
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, analysisDataFilePath) VALUES
			(1,   'T2', 12400, 4, ?, 44100, '/PIONEER/USBANLZ/P001/ANLZ0000.DAT'),
			(2,   'T3', 12200, 4, ?, 44100, '/PIONEER/USBANLZ/P001/ANLZ0000.DAT'),
			(3,   'T4', 12600, 4, ?, 44100, '/PIONEER/USBANLZ/P001/ANLZ0000.DAT'),
			(101, 'T1', 12800, 4, ?, 44100, '/PIONEER/USBANLZ/P001/ANLZ0000.DAT');
		INSERT INTO playlist(playlist_id, name, playlist_id_parent, attribute) VALUES (10, 'Set A', 0, 0), (11, 'Set B', 0, 0);
		INSERT INTO playlist_content(playlist_id, content_id, sequenceNo) VALUES (10, 101, 1), (10, 1, 2), (11, 2, 1), (11, 3, 2);
	`, "/"+testfixture.LargeTrackPath(2), "/"+testfixture.LargeTrackPath(3), "/"+testfixture.LargeTrackPath(4), "/"+testfixture.LargeTrackPath(1))

	// Quick update at mount must keep exact Engine IDs and library UUID immediately.
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("re-export quick mount: %+v %v", r, err)
	}
	uuidQuick, idsQuick := readIDsAndUUID()
	if uuidQuick != uuidBefore {
		t.Fatalf("library uuid changed after quickUpdate: %q -> %q", uuidBefore, uuidQuick)
	}
	for p, wantID := range idsBefore {
		if gotID := idsQuick[p]; gotID != wantID {
			t.Fatalf("track %s Engine ID changed after quickUpdate: %d -> %d", p, wantID, gotID)
		}
	}

	// Background update + publish must also preserve every Engine ID, UUID, and RGB overview file.
	if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("re-export background: %+v %v", r, err)
	}
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatalf("re-export publish: %v", err)
	}
	uuidAfter, idsAfter := readIDsAndUUID()
	if uuidAfter != uuidBefore {
		t.Fatalf("library uuid changed after publish: %q -> %q", uuidBefore, uuidAfter)
	}
	for p, wantID := range idsBefore {
		if gotID := idsAfter[p]; gotID != wantID {
			t.Fatalf("track %s Engine ID changed after publish: %d -> %d", p, wantID, gotID)
		}
		rgbPath := filepath.Join(dir, "OverviewData", uuidAfter, strconv.FormatInt(wantID, 10)+".rgb")
		if st, err := os.Stat(rgbPath); err != nil || st.Size() <= 8 {
			t.Fatalf("RGB overview for %s (id=%d) lost after shifted-ID re-export: st=%v err=%v", p, wantID, st, err)
		}
	}
}

func TestWriteBackAnalysisOnlyBeatDataChangeSkipsDecrypt(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	stick := setupWriteBackStick(t, true)
	if _, err := convertWriteBackAtTest(context.Background(), stick); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(stick, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Simulate Engine OfflineAnalyzer updating beatData, trackData, overviewWaveFormData,
	// bpmAnalyzed, and lastEditTime without modifying cues, loops, or editable metadata.
	fakeBlob, err := enginedb.QCompress([]byte("engine-reanalyzed-grid-payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET beatData=?, trackData=?, overviewWaveFormData=? WHERE trackId=1`, fakeBlob, fakeBlob, fakeBlob); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET bpmAnalyzed=bpmAnalyzed+1, lastEditTime=strftime('%s','now')-10 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	keys := rekordbox.NewKeyCache()
	defer keys.Clear()
	d0 := rekordbox.KeyDerivations()
	res, err := bridge.WriteBackCues(context.Background(), stick, bridge.WriteBackOptions{
		Keys:               keys,
		SkipPreparingCheck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TracksChanged != 0 || res.FilesUpdated != 0 {
		t.Fatalf("analysis-only change wrote tracks: %+v", res)
	}
	if got := rekordbox.KeyDerivations() - d0; got != 0 {
		t.Fatalf("analysis-only beatData change triggered %d key derivations, want 0 (res=%+v)", got, res)
	}
	if res.PhasesMS["decrypt"] != 0 {
		t.Fatalf("analysis-only beatData change spent %d ms decrypting, want 0", res.PhasesMS["decrypt"])
	}
}
