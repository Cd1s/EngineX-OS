package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func copyEncryptedOneLibraryFixture(t *testing.T, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExportSelectionMTimeAndFallback(t *testing.T) {
	t.Run("prefers_pdb_when_mtimes_within_5s_and_preserves_single_candidate_stamps", func(t *testing.T) {
		// Verify single-candidate stamps stay byte-identical to dc4d52d / 6441ae6.
		pdbOnlyRoot := t.TempDir()
		testfixture.Export(t, pdbOnlyRoot)
		if _, err := bridge.ConvertAtMount(context.Background(), pdbOnlyRoot, enginedb.Options{}, true); err != nil {
			t.Fatal(err)
		}
		pdbSt, err := os.Stat(filepath.Join(pdbOnlyRoot, "PIONEER/rekordbox/export.pdb"))
		if err != nil {
			t.Fatal(err)
		}
		gotStamp, err := os.ReadFile(filepath.Join(pdbOnlyRoot, "Engine Library/Database2/.rb-bridge-stamp"))
		if err != nil {
			t.Fatal(err)
		}
		wantStamp := fmt.Sprintf("v2 PIONEER/rekordbox/export.pdb %d %d\n", pdbSt.Size(), pdbSt.ModTime().UnixNano())
		if string(gotStamp) != wantStamp {
			t.Fatalf("single-candidate PDB stamp = %q, want %q", string(gotStamp), wantStamp)
		}

		// When export.pdb and exportLibrary.db mtimes are within <= 5s, export.pdb comes first.
		root := t.TempDir()
		testfixture.Export(t, root)
		testfixture.Write(t, root, "MUSIC/one.wav", testfixture.WAV(true))
		testfixture.Write(t, root, "MUSIC/two.wav", testfixture.WAV(true))
		dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
		copyEncryptedOneLibraryFixture(t, dbPath)
		pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")

		now := time.Unix(1730000000, 0)
		for _, delta := range []time.Duration{-4 * time.Second, 0, 4 * time.Second, 5 * time.Second} {
			if err := os.Chtimes(pdbPath, now, now); err != nil {
				t.Fatal(err)
			}
			dbTime := now.Add(delta)
			if err := os.Chtimes(dbPath, dbTime, dbTime); err != nil {
				t.Fatal(err)
			}
			lib, err := bridge.ReadLibrary(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if len(lib.Tracks) != 5 || lib.Tracks[0].ID != 1 {
				t.Fatalf("delta=%v: expected export.pdb first within <=5s tie-break, got %d tracks (first ID=%d)", delta, len(lib.Tracks), lib.Tracks[0].ID)
			}
		}
	})

	t.Run("prefers_newer_export_by_more_than_5s_and_invalidates_stamp", func(t *testing.T) {
		root := t.TempDir()
		testfixture.Export(t, root)
		testfixture.Write(t, root, "MUSIC/one.wav", testfixture.WAV(true))
		testfixture.Write(t, root, "MUSIC/two.wav", testfixture.WAV(true))
		pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
		t0 := time.Unix(1730000000, 0)
		if err := os.Chtimes(pdbPath, t0, t0); err != nil {
			t.Fatal(err)
		}

		// Initial mount with only Rekordbox 6 export.pdb writes .rb-bridge-stamp.
		res, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, true)
		if err != nil || res.Added != 5 {
			t.Fatalf("initial PDB mount: %+v %v", res, err)
		}
		if res, err = bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !res.Bypassed {
			t.Fatalf("expected unchanged PDB stamp bypass: %+v %v", res, err)
		}

		// User updates the stick in Rekordbox 7 Device Library Plus (exportLibrary.db > 5s newer; export.pdb untouched).
		dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
		copyEncryptedOneLibraryFixture(t, dbPath)
		t1 := t0.Add(30 * time.Second)
		if err := os.Chtimes(dbPath, t1, t1); err != nil {
			t.Fatal(err)
		}

		res, err = bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
		if err != nil || res.Bypassed {
			t.Fatalf("ConvertAtMount bypassed new Rekordbox 7 exportLibrary.db: %+v %v", res, err)
		}
		engine := dbOpen(t, root)
		if n := scalar[int](t, engine, `SELECT count(*) FROM Track WHERE pdbImportKey IN (101,102,103)`); n != 3 {
			t.Fatalf("expected 3 OneLibrary tracks imported, got %d", n)
		}

		// Conversely, if export.pdb is later rewritten > 5s newer than exportLibrary.db, ReadLibrary picks export.pdb.
		t2 := t1.Add(15 * time.Second)
		if err := os.Chtimes(pdbPath, t2, t2); err != nil {
			t.Fatal(err)
		}
		lib, err := bridge.ReadLibrary(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if len(lib.Tracks) != 5 || lib.Tracks[0].ID != 1 {
			t.Fatalf("expected newer export.pdb to win (>5s newer), got %+v", lib.Tracks)
		}
	})

	t.Run("falls_back_when_preferred_export_fails", func(t *testing.T) {
		root := t.TempDir()
		testfixture.Export(t, root)
		dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
		if err := os.WriteFile(dbPath, []byte("corrupt encrypted exportLibrary.db payload"), 0o644); err != nil {
			t.Fatal(err)
		}
		// exportLibrary.db is newer by > 5s, so it is tried first, fails, and falls back to export.pdb.
		future := time.Now().Add(1 * time.Minute)
		if err := os.Chtimes(dbPath, future, future); err != nil {
			t.Fatal(err)
		}
		lib, err := bridge.ReadLibrary(context.Background(), root)
		if err != nil || len(lib.Tracks) != 5 {
			t.Fatalf("expected fallback to valid export.pdb, got %v (err=%v)", lib, err)
		}

		// Now make export.pdb newer by > 5s and corrupt, while exportLibrary.db is valid SQLCipher 4.
		copyEncryptedOneLibraryFixture(t, dbPath)
		pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
		if err := os.WriteFile(pdbPath, []byte("corrupt pdb"), 0o644); err != nil {
			t.Fatal(err)
		}
		future2 := future.Add(1 * time.Minute)
		if err := os.Chtimes(pdbPath, future2, future2); err != nil {
			t.Fatal(err)
		}
		lib, err = bridge.ReadLibrary(context.Background(), root)
		if err != nil || len(lib.Tracks) != 3 || lib.Tracks[0].ID != 101 {
			t.Fatalf("expected fallback to valid SQLCipher 4 exportLibrary.db, got %v (err=%v)", lib, err)
		}
	})
}

func TestMacHiddenDotPioneerOneLibraryCuesAndIncrementalDelta(t *testing.T) {
	root := t.TempDir()
	testfixture.Write(t, root, "MUSIC/one.wav", testfixture.WAV(true))
	testfixture.Write(t, root, "MUSIC/two.wav", testfixture.WAV(true))
	testfixture.Write(t, root, "MUSIC/three.wav", testfixture.WAV(true))

	// Stage encrypted SQLCipher 4 exportLibrary.db inside hidden macOS .PIONEER folder.
	dotDB := filepath.Join(root, ".PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, dotDB)
	dotBefore := snapshot(t, filepath.Join(root, ".PIONEER"))

	res, err := bridge.Convert(context.Background(), root, enginedb.Options{})
	if err != nil || res.Added != 3 {
		t.Fatalf("Convert from .PIONEER SQLCipher 4 exportLibrary.db: %+v %v", res, err)
	}
	if !reflect.DeepEqual(dotBefore, snapshot(t, filepath.Join(root, ".PIONEER"))) {
		t.Fatal(".PIONEER directory was modified during SQLCipher 4 import")
	}

	engine := dbOpen(t, root)
	trackID := scalar[int64](t, engine, `SELECT id FROM Track WHERE pdbImportKey=101`)
	qBlob := scalar[[]byte](t, engine, `SELECT quickCues FROM PerformanceData WHERE trackId=?`, trackID)
	qRaw, err := enginedb.QUncompress(qBlob, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(qRaw, []byte("Hot A")) {
		t.Fatalf("Hot Cue A from exportLibrary.db cue table missing in quickCues")
	}
	loopsRaw := scalar[[]byte](t, engine, `SELECT loops FROM PerformanceData WHERE trackId=?`, trackID)
	if !bytes.Contains(loopsRaw, []byte("Loop")) {
		t.Fatalf("Memory Loop from exportLibrary.db cue table missing in PerformanceData.loops")
	}
	activeBits := scalar[int](t, engine, `SELECT activeOnLoadLoops FROM PerformanceData WHERE trackId=?`, trackID)
	if activeBits == 0 {
		t.Fatalf("expected activeOnLoadLoops bit set for isActiveLoop=1 cue")
	}

	// Verify incremental cue delta re-sync when a cue row changes in exportLibrary.db.
	root2 := t.TempDir()
	testfixture.Write(t, root2, "MUSIC/a.wav", testfixture.WAV(true))
	dbFile := filepath.Join(root2, "PIONEER/rekordbox/exportLibrary.db")
	if err := os.MkdirAll(filepath.Dir(dbFile), 0o755); err != nil {
		t.Fatal(err)
	}
	sdb, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sdb.Exec(`
		CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT, bpmx100 INTEGER, length INTEGER, samplingRate INTEGER);
		INSERT INTO content VALUES(1,'Cue Delta','/MUSIC/a.wav',12400,180,48000);
		CREATE TABLE cue(cue_id INTEGER PRIMARY KEY, content_id INTEGER, kind INTEGER, colorTableIndex INTEGER, cueComment TEXT, isActiveLoop INTEGER, beatLoopNumerator INTEGER, beatLoopDenominator INTEGER, inUsec INTEGER, outUsec INTEGER);
		INSERT INTO cue VALUES(1,1,1,49,'First Cue',0,0,0,200000,0);
	`)
	_ = sdb.Close()
	if err != nil {
		t.Fatal(err)
	}
	if r1 := convert(t, root2, enginedb.Options{}); r1.Added != 1 {
		t.Fatalf("first import: %+v", r1)
	}
	if r2 := convert(t, root2, enginedb.Options{}); r2.Unchanged != 1 || r2.Updated != 0 {
		t.Fatalf("no-op import: %+v", r2)
	}

	sdb2, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sdb2.Exec(`UPDATE cue SET cueComment='Edited Cue', inUsec=500000 WHERE cue_id=1`)
	_ = sdb2.Close()
	if err != nil {
		t.Fatal(err)
	}
	if r3 := convert(t, root2, enginedb.Options{}); r3.Updated != 1 || r3.Unchanged != 0 {
		t.Fatalf("cue edit did not trigger incremental update: %+v", r3)
	}
	engine2 := dbOpen(t, root2)
	qBlob2 := scalar[[]byte](t, engine2, `SELECT quickCues FROM PerformanceData WHERE trackId=1`)
	qRaw2, err := enginedb.QUncompress(qBlob2, 64<<10)
	if err != nil || !bytes.Contains(qRaw2, []byte("Edited Cue")) {
		t.Fatalf("updated cue label missing after incremental sync: %v", err)
	}
}

func TestConvertAtMountReadsOnceAndStartsBudgetAfterRead(t *testing.T) {
	root := t.TempDir()
	testfixture.Write(t, root, "MUSIC/one.wav", testfixture.WAV(true))
	testfixture.Write(t, root, "MUSIC/two.wav", testfixture.WAV(true))
	testfixture.Write(t, root, "MUSIC/three.wav", testfixture.WAV(true))
	dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, dbPath)

	// First mount where library Read/Decrypt takes 2.6s while mountReadBudget is 2.5s:
	// the 2.5s file-read budget must start AFTER readLibrary finishes, so 0 tracks are deferred
	// (including under -race instrumentation overhead on modernc.org/sqlite).
	reads := 0
	bridge.WithReadExportHookForTest(t, 2500*time.Millisecond, func() {
		reads++
		time.Sleep(2600 * time.Millisecond)
	})
	res, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("first ConvertAtMount called readExport %d times, want 1", reads)
	}
	if res.Deferred != 0 {
		t.Fatalf("mountReadBudget started before readLibrary finished: %d tracks deferred", res.Deferred)
	}

	// Second mount on <=500 tracks with existing m.db (hasLibrary(dir) == true) and updated mtime:
	// must read/decrypt exportLibrary.db once, not twice.
	later := time.Now().Add(1 * time.Minute)
	if err := os.Chtimes(dbPath, later, later); err != nil {
		t.Fatal(err)
	}
	reads = 0
	res, err = bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("incremental ConvertAtMount with existing m.db called readExport %d times, want 1", reads)
	}
	if res.Deferred != 0 {
		t.Fatalf("unexpected deferred tracks on incremental mount: %+v", res)
	}
}
