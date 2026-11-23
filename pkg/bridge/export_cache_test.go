package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

func TestReadExportCacheIsolatesSameSizeSameMtimeVolumes(t *testing.T) {
	t.Parallel()
	volA := t.TempDir()
	volB := t.TempDir()
	testfixture.LargeExport(t, volA, 2)
	testfixture.LargeExport(t, volB, 3)

	pdbA := filepath.Join(volA, "PIONEER/rekordbox/export.pdb")
	pdbB := filepath.Join(volB, "PIONEER/rekordbox/export.pdb")
	stA, err := os.Stat(pdbA)
	if err != nil {
		t.Fatal(err)
	}
	stB, err := os.Stat(pdbB)
	if err != nil {
		t.Fatal(err)
	}
	if stA.Size() != stB.Size() {
		t.Fatalf("fixture assumption broken: pdbA=%d pdbB=%d", stA.Size(), stB.Size())
	}
	sameTime := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(pdbA, sameTime, sameTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdbB, sameTime, sameTime); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	libA, err := bridge.ReadLibrary(ctx, volA)
	if err != nil {
		t.Fatalf("read volA: %v", err)
	}
	libB, err := bridge.ReadLibrary(ctx, volB)
	if err != nil {
		t.Fatalf("read volB: %v", err)
	}
	if len(libA.Tracks) != 2 || len(libB.Tracks) != 3 {
		t.Fatalf("cross-volume cache collision: volA=%d tracks, volB=%d tracks (want 2, 3)", len(libA.Tracks), len(libB.Tracks))
	}

	// Overwriting volA in place with a different 3-track PDB at the exact same
	// size and mtime must also miss the cache.
	testfixture.Write(t, volA, "PIONEER/rekordbox/export.pdb", testfixture.LargePDB(3))
	if err := os.Chtimes(pdbA, sameTime, sameTime); err != nil {
		t.Fatal(err)
	}
	libA2, err := bridge.ReadLibrary(ctx, volA)
	if err != nil {
		t.Fatalf("re-read volA: %v", err)
	}
	if len(libA2.Tracks) != 3 {
		t.Fatalf("in-place same-size/mtime overwrite served stale cache: got %d tracks, want 3", len(libA2.Tracks))
	}
}
