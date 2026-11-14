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
	for _, name := range []string{"exportLibrary.db", "exportOneLibrary.db"} {
		for _, diff := range []time.Duration{-6, -5, -3, 0, 3, 5, 6} {
			t.Run(fmt.Sprintf("%s_%ds", name, int(diff)), func(t *testing.T) {
				root := t.TempDir()
				testfixture.Export(t, root)
				dbPath := filepath.Join(root, "PIONEER/rekordbox", name)
				copyEncryptedOneLibraryFixture(t, dbPath)
				pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
				now := time.Unix(1730000000, 0)
				if err := os.Chtimes(pdbPath, now, now); err != nil {
					t.Fatal(err)
				}
				mtime := now.Add(diff * time.Second)
				if err := os.Chtimes(dbPath, mtime, mtime); err != nil {
					t.Fatal(err)
				}
				lib, err := bridge.ReadLibrary(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				wantID := uint32(101)
				if diff < -5 {
					wantID = 1
				}
				if len(lib.Tracks) == 0 || lib.Tracks[0].ID != wantID {
					t.Fatalf("first track ID: got %+v, want %d", lib.Tracks, wantID)
				}
			})
		}
	}

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

		// A tied but unreadable SQLCipher export must still fall back to PDB.
		pdbStat, err := os.Stat(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dbPath, pdbStat.ModTime(), pdbStat.ModTime()); err != nil {
			t.Fatal(err)
		}
		if lib, err := bridge.ReadLibrary(context.Background(), root); err != nil || len(lib.Tracks) != 5 {
			t.Fatalf("tied SQLCipher fallback: %+v %v", lib, err)
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

// BH-GF4-14: changing the preferred format must invalidate main's PDB-first
// dual-export stamp, then retain the normal bypass on the next mount.
func TestExportTieInvalidatesPDBFirstStamp(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	testfixture.Write(t, root, "MUSIC/one.wav", testfixture.WAV(true))
	testfixture.Write(t, root, "MUSIC/two.wav", testfixture.WAV(true))
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Added != 5 {
		t.Fatalf("initial PDB import: %+v %v", res, err)
	}
	pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	pdbStat, err := os.Stat(pdbPath)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, dbPath)
	if err := os.Chtimes(dbPath, pdbStat.ModTime(), pdbStat.ModTime()); err != nil {
		t.Fatal(err)
	}
	dbStat, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	stampPath := filepath.Join(root, "Engine Library/Database2/.rb-bridge-stamp")
	oldStamp := fmt.Sprintf("v2 PIONEER/rekordbox/export.pdb %d %d | PIONEER/rekordbox/exportLibrary.db %d %d\n",
		pdbStat.Size(), pdbStat.ModTime().UnixNano(), dbStat.Size(), dbStat.ModTime().UnixNano())
	if err := os.WriteFile(stampPath, []byte(oldStamp), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Bypassed || res.Deferred != 0 {
		t.Fatalf("upgrade PDB-first stamp: %+v %v", res, err)
	}
	engine := dbOpen(t, root)
	if n := scalar[int](t, engine, `SELECT count(*) FROM Track WHERE pdbImportKey IN (101,102,103)`); n != 3 {
		t.Fatalf("OneLibrary tracks imported: got %d, want 3", n)
	}
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !res.Bypassed {
		t.Fatalf("unchanged OneLibrary stamp: %+v %v", res, err)
	}
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
