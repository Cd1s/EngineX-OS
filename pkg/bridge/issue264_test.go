package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// Issue #264: Large Rekordbox USB libraries with missing files or slow reconciliation
// previously consumed the 8s mountReadBudget during ReconcileContext before file reading
// began. SyncResolved then observed inTime=false and deferred every track on every mount,
// leaving Updated=0, Deferred=N stuck indefinitely at 100% deferred.
//
// With the fix:
// 1) Resolver caches path resolutions in memory, eliminating redundant directory walks.
// 2) Redundant os.Stat calls in ReconcileContext are removed.
// 3) opts.Deadline is budgeted specifically for file reading after ReconcileContext.
// 4) Resume mode guarantees forward progress on every mount.
func TestIssue264LargeExportWithMissingFilesResumesAndFinishes(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root) // baseline analysis files and artwork
	if err := os.Remove(filepath.Join(root, "PIONEER/rekordbox/export.pdb")); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE content(id INTEGER PRIMARY KEY,title TEXT,file_path TEXT,analysis_path TEXT,bpm INTEGER,key_name TEXT);
CREATE TABLE playlist(id INTEGER PRIMARY KEY,parent_id INTEGER,name TEXT,attribute INTEGER,seq INTEGER,smart_list TEXT);
CREATE TABLE playlist_content(playlist_id INTEGER,content_id INTEGER,track_no INTEGER);
INSERT INTO playlist VALUES(1,0,'Issue264 Set',0,1,'');`); err != nil {
		t.Fatal(err)
	}

	const totalTracks = 900
	const missingCount = 300
	wav := testfixture.WAV(true)

	for i := 1; i <= totalTracks; i++ {
		p := fmt.Sprintf("MUSIC/t%04d.wav", i)
		if i <= missingCount {
			// Missing audio file: do NOT write to disk (reproducing missing files in #264)
			p = fmt.Sprintf("MUSIC/missing/t%04d.wav", i)
		} else {
			testfixture.Write(t, root, p, wav)
		}
		if _, err = db.Exec(`INSERT INTO content VALUES(?,?,?,?,12800,'8A')`, i, fmt.Sprintf("Track %04d", i), "/"+p, "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO playlist_content VALUES(1,?,?)`, i, i); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	ctx := context.Background()

	// Mount 1: Fast staged import of the >500 track library without reading cues.
	r1, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil {
		t.Fatalf("mount 1: %v", err)
	}
	expectedExisting := totalTracks - missingCount
	if r1.Added != expectedExisting {
		t.Fatalf("mount 1 expected Added=%d, got %+v", expectedExisting, r1)
	}

	dir := filepath.Join(root, "Engine Library", "Database2")
	pendingFile := filepath.Join(dir, ".rb-bridge-pending")
	stampFile := filepath.Join(dir, ".rb-bridge-stamp")

	if _, err := os.Stat(pendingFile); err != nil {
		t.Fatalf("mount 1 must write pending mark: %v", err)
	}
	if _, err := os.Stat(stampFile); !os.IsNotExist(err) {
		t.Fatalf("mount 1 must not write completed stamp: %v", err)
	}

	// Mount 2: Default mount with mountReadBudget. Must make progress (Updated > 0)
	// and never stay stuck at Updated=0, Deferred=N.
	r2, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil {
		t.Fatalf("mount 2: %v", err)
	}
	if r2.Updated == 0 {
		t.Fatalf("mount 2 made 0 progress (Issue #264 reproduced): %+v", r2)
	}

	// Subsequent mounts until completion.
	totalUpdated := r2.Updated
	mounts := 2
	for r2.Deferred > 0 && mounts < 10 {
		mounts++
		r2, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
		if err != nil {
			t.Fatalf("mount %d: %v", mounts, err)
		}
		if r2.Updated == 0 && r2.Deferred > 0 {
			t.Fatalf("mount %d made 0 progress with %d remaining deferred", mounts, r2.Deferred)
		}
		totalUpdated += r2.Updated
	}

	if r2.Deferred != 0 {
		t.Fatalf("failed to complete after %d mounts: %+v", mounts, r2)
	}
	if totalUpdated != expectedExisting {
		t.Fatalf("expected total updated %d, got %d", expectedExisting, totalUpdated)
	}
	if _, err := os.Stat(stampFile); err != nil {
		t.Fatalf("completed stamp must exist: %v", err)
	}
	if _, err := os.Stat(pendingFile); !os.IsNotExist(err) {
		t.Fatalf("pending file must be removed on completion")
	}

	// Verify database content: all tracks have valid beat data, cues, and loops.
	mDB, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer mDB.Close()

	var analyzedCount int
	if err := mDB.QueryRow(`SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL AND quickCues IS NOT NULL AND loops IS NOT NULL`).Scan(&analyzedCount); err != nil {
		t.Fatal(err)
	}
	if analyzedCount != expectedExisting {
		t.Fatalf("expected %d tracks with performance data, got %d", expectedExisting, analyzedCount)
	}

	var loops []byte
	sampleTrackID := missingCount + 1
	if err := mDB.QueryRow(`SELECT p.loops FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, sampleTrackID).Scan(&loops); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(loops, []byte("H Loop")) || !bytes.Contains(loops, []byte("Memory Loop")) {
		t.Fatalf("loops of sample track %d do not contain expected cues: %q", sampleTrackID, loops)
	}
}

// TestIssue264SlowReconcileDoesNotExhaustFileReadingBudget verifies that even when
// export reading and reconciliation experience deliberate delay, the file-reading
// deadline starts after reconciliation and makes positive forward progress.
func TestIssue264SlowReconcileDoesNotExhaustFileReadingBudget(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)

	db, err := sql.Open("sqlite", filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE content(id INTEGER PRIMARY KEY,title TEXT,file_path TEXT,analysis_path TEXT,bpm INTEGER,key_name TEXT);
CREATE TABLE playlist(id INTEGER PRIMARY KEY,parent_id INTEGER,name TEXT,attribute INTEGER,seq INTEGER,smart_list TEXT);
CREATE TABLE playlist_content(playlist_id INTEGER,content_id INTEGER,track_no INTEGER);
INSERT INTO playlist VALUES(1,0,'Slow Set',0,1,'');`); err != nil {
		t.Fatal(err)
	}
	const n = 520
	wav := testfixture.WAV(true)
	for i := 1; i <= n; i++ {
		p := fmt.Sprintf("MUSIC/slow/t%03d.wav", i)
		testfixture.Write(t, root, p, wav)
		if _, err = db.Exec(`INSERT INTO content VALUES(?,?,?,?,12800,'8A')`, i, fmt.Sprintf("Track %03d", i), "/"+p, "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO playlist_content VALUES(1,?,?)`, i, i); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	_ = os.Remove(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))

	ctx := context.Background()
	r1, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || r1.Added != n {
		t.Fatalf("first mount: %+v %v", r1, err)
	}

	// Simulate slow export reading (1.2s delay) with a 1s mount budget.
	// On the old code, reading the export consumed the budget before Reconcile/Sync,
	// leading to inTime=false and 0 updated tracks.
	restore := bridge.SlowExportReadForTest(1200*time.Millisecond, 1000*time.Millisecond)
	defer restore()

	r2, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil {
		t.Fatalf("second mount: %v", err)
	}
	if r2.Updated == 0 {
		t.Fatalf("slow export reading starved mount progress (Issue #264): %+v", r2)
	}
}
