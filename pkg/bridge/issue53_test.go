package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #53: one unreadable file used to fail the whole first import and leave an
// empty m.db, which Engine showed as an empty library with no rekordbox prompt.
func TestIssue53OneBadTrackKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	testfixture.Write(t, root, "MUSIC/three.wav", []byte("not a wav file at all"))
	// What an earlier build left behind after such a failure.
	testfixture.Write(t, root, "Engine Library/Database2/m.db", nil)
	r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Added != 5 || r.Degraded != 1 {
		t.Fatalf("want 5 added, 1 degraded: %+v", r)
	}
	db := dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM Track`); n != 5 {
		t.Fatal("tracks", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PlaylistEntity`); n != 4 {
		t.Fatal("playlist entries", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.filename='three.wav' AND p.trackData IS NULL`); n != 1 {
		t.Fatal("the bad track should be left for Engine to analyse")
	}
	// The bad track is not read again while its file is unchanged.
	again, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, true)
	if err != nil || again.Degraded != 0 || again.Unchanged != 5 {
		t.Fatalf("%+v %v", again, err)
	}
}

func TestIssue53FailedFirstImportLeavesNoLibrary(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	testfixture.Write(t, root, "Engine Library/Database2/m.db", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err == nil {
		t.Fatal("cancelled import succeeded")
	}
	if _, err := os.Lstat(filepath.Join(root, "Engine Library")); !os.IsNotExist(err) {
		t.Fatal("a failed first import left an Engine Library behind:", err)
	}
}
