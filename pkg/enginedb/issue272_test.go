package enginedb_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Issue #272: .rb-bridge-state.db must not grow past FAT32's 4 GiB limit.
// When MaxBridgeStateBytes is reached, tracks still import in full into Track
// and PerformanceData (Hot Cues, Saved Loops, beat grid, overview waveform),
// but their BridgeAnalysis row is omitted and counted in Result.AnalysisSkipped.
func TestIssue272BridgeAnalysisSizeCapped(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 10)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	ctx := context.Background()

	// Set a low cap (16 KB) so the cap is reached during the 10-track import.
	opts := enginedb.Options{
		DatabaseDir:         dir,
		MaxBridgeStateBytes: 16384,
	}

	res, err := enginedb.Sync(ctx, root, l, opts)
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if res.Added != 10 {
		t.Fatalf("expected 10 tracks added, got %d", res.Added)
	}
	if res.AnalysisSkipped == 0 {
		t.Fatalf("expected AnalysisSkipped > 0 with low cap, got 0")
	}

	// Verify that ALL 10 tracks have rows in Track and PerformanceData in m.db
	mDbPath := filepath.Join(dir, "m.db")
	db, err := sql.Open("sqlite", mDbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var trackCount, perfCount int
	if err := db.QueryRow("SELECT count(*) FROM Track").Scan(&trackCount); err != nil || trackCount != 10 {
		t.Fatalf("expected 10 tracks in Track, got %d (err: %v)", trackCount, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM PerformanceData WHERE length(overviewWaveFormData) > 0").Scan(&perfCount); err != nil || perfCount != 10 {
		t.Fatalf("expected 10 tracks with overview waveform in PerformanceData, got %d (err: %v)", perfCount, err)
	}

	// Verify that BridgeAnalysis count is less than 10 (omitted tracks)
	stateDbPath := filepath.Join(dir, ".rb-bridge-state.db")
	stateDb, err := sql.Open("sqlite", stateDbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateDb.Close()

	var analysisCount int
	if err := stateDb.QueryRow("SELECT count(*) FROM BridgeAnalysis").Scan(&analysisCount); err != nil {
		t.Fatalf("failed to count BridgeAnalysis: %v", err)
	}
	if analysisCount >= 10 {
		t.Fatalf("expected fewer than 10 BridgeAnalysis rows due to cap, got %d", analysisCount)
	}
	if analysisCount+res.AnalysisSkipped != 10 {
		t.Fatalf("expected analysisCount (%d) + skipped (%d) == 10", analysisCount, res.AnalysisSkipped)
	}
}
