package bridge_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// Issue #272: Large Rekordbox USB exports (e.g. 21k tracks on FAT32) produced a
// 7.2 GB .rb-bridge-state.db exceeding FAT32's 4 GiB single-file limit, rolling back
// imports. In addition, Resolver incurred thousands of redundant Lstat calls.
//
// Regression test verifies:
// 1) When MaxBridgeStateBytes is set, .rb-bridge-state.db caps BridgeAnalysis rows.
// 2) Exceeding tracks still import in full into Track & PerformanceData (cues, loops, beats, overview).
// 3) Result.AnalysisSkipped reflects omitted shadow rows.
// 4) Multi-mount resume continues forward progress and completes.
func TestIssue272BridgeAnalysisCappedAndResumed(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
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
INSERT INTO playlist VALUES(1,0,'Issue272 Set',0,1,'');`); err != nil {
		t.Fatal(err)
	}

	const totalTracks = 600
	wav := testfixture.WAV(true)
	for i := 1; i <= totalTracks; i++ {
		p := fmt.Sprintf("MUSIC/t%04d.wav", i)
		testfixture.Write(t, root, p, wav)
		if _, err = db.Exec(`INSERT INTO content VALUES(?,?,?,?,12800,'8A')`, i, fmt.Sprintf("Track %04d", i), "/"+p, "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO playlist_content VALUES(1,?,?)`, i, i); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	ctx := context.Background()

	// Mount 1: Fast staged import (>500 tracks).
	// Set MaxBridgeStateBytes to 20 KB so that during resume it caps BridgeAnalysis quickly.
	opts := enginedb.Options{
		MaxBridgeStateBytes: 20480,
	}
	r1, err := bridge.ConvertAtMount(ctx, root, opts, false)
	if err != nil {
		t.Fatalf("mount 1: %v", err)
	}
	if r1.Added != totalTracks {
		t.Fatalf("mount 1 expected %d tracks added, got %+v", totalTracks, r1)
	}

	dir := filepath.Join(root, "Engine Library", "Database2")
	pendingFile := filepath.Join(dir, ".rb-bridge-pending")
	if _, err := os.Stat(pendingFile); err != nil {
		t.Fatalf("mount 1 must write pending mark: %v", err)
	}

	// Mount 2: Full in-place resume with MaxBridgeStateBytes cap.
	r2, err := bridge.ConvertAtMount(ctx, root, opts, true)
	if err != nil {
		t.Fatalf("mount 2 full resume: %v", err)
	}
	if r2.AnalysisSkipped == 0 {
		t.Fatalf("expected AnalysisSkipped > 0 due to 20KB cap, got %+v", r2)
	}
	if r2.Updated != totalTracks {
		t.Fatalf("expected all %d tracks updated with cues/waveforms, got %+v", totalTracks, r2)
	}

	// Check m.db: all 600 tracks must have full PerformanceData
	mDb, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer mDb.Close()

	var perfCount int
	if err := mDb.QueryRow("SELECT count(*) FROM PerformanceData WHERE length(overviewWaveFormData) > 0").Scan(&perfCount); err != nil || perfCount != totalTracks {
		t.Fatalf("expected %d tracks in PerformanceData, got %d (err: %v)", totalTracks, perfCount, err)
	}

	// Check .rb-bridge-state.db: BridgeAnalysis has fewer rows than totalTracks due to cap
	stateDb, err := sql.Open("sqlite", filepath.Join(dir, ".rb-bridge-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateDb.Close()

	var analysisCount, stateCount int
	if err := stateDb.QueryRow("SELECT count(*) FROM BridgeAnalysis").Scan(&analysisCount); err != nil {
		t.Fatal(err)
	}
	if err := stateDb.QueryRow("SELECT count(*) FROM BridgeState").Scan(&stateCount); err != nil {
		t.Fatal(err)
	}
	if stateCount != totalTracks {
		t.Fatalf("expected all %d tracks in BridgeState, got %d", totalTracks, stateCount)
	}
	if analysisCount >= totalTracks {
		t.Fatalf("expected BridgeAnalysis rows < %d, got %d", totalTracks, analysisCount)
	}
	if analysisCount+r2.AnalysisSkipped != totalTracks {
		t.Fatalf("analysisCount (%d) + AnalysisSkipped (%d) != %d", analysisCount, r2.AnalysisSkipped, totalTracks)
	}
}
