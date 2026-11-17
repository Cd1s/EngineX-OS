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

func TestSmallLibraryDeferredMountWritesPendingAndResumes(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	dir := filepath.Join(root, "Engine Library", "Database2")
	// A first import goes on in its stage until it is complete (#301).
	pending := filepath.Join(dir, ".rb-bridge-import", ".rb-bridge-pending")

	restore := bridge.SlowExportReadForTest(0, time.Nanosecond)
	r1, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	restore()
	if err != nil {
		t.Fatalf("first mount: %v", err)
	}
	if r1.Deferred == 0 || !r1.Background {
		t.Fatalf("expected deferred tracks under 1ns budget, got %+v", r1)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("expected .rb-bridge-pending after <=500-track deferred mount, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "m.db")); !os.IsNotExist(err) {
		t.Fatalf("a first import without its cues was published: %v", err)
	}

	// After the mount the rest is read, and the next mount publishes the library.
	if _, err := bridge.ImportInBackground(context.Background(), root, nil); err != nil {
		t.Fatalf("background import: %v", err)
	}
	r2, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{Deadline: time.Now().Add(time.Minute)}, false)
	if err != nil || r2.Background || !r2.PlaylistsChanged {
		t.Fatalf("second resumed mount: %+v %v", r2, err)
	}
	for _, p := range []string{pending, filepath.Join(dir, ".rb-bridge-pending")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected .rb-bridge-pending removed once completed, got %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".rb-bridge-stamp")); err != nil {
		t.Fatalf("completed import not recorded: %v", err)
	}
}
