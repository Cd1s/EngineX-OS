package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// r2 review: a mount of a small library with an m.db read (and, for a
// OneLibrary, decrypted) the export twice, and the file-reading budget started
// before the first read, so a slow read left tracks without cues.
func TestMountReadsTheExportOnceAndBudgetsAfterIt(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	l, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	// An earlier import that took only what the export holds: every track still needs its files read.
	if _, err = enginedb.Sync(context.Background(), root, l, enginedb.Options{Fast: true}); err != nil {
		t.Fatal(err)
	}
	restore := bridge.SlowExportReadForTest(300*time.Millisecond, 250*time.Millisecond)
	defer restore()
	before := bridge.ExportReadsForTest()
	r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := bridge.ExportReadsForTest() - before; n != 1 {
		t.Fatalf("export read %d times in one mount", n)
	}
	if r.Deferred != 0 {
		t.Fatalf("%d tracks deferred: the budget was spent on reading the export", r.Deferred)
	}
	if _, err = os.Stat(filepath.Join(root, "Engine Library/Database2/.rb-bridge-stamp")); err != nil {
		t.Fatal("complete import not stamped", err)
	}
}
