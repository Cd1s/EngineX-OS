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
	pending := filepath.Join(dir, ".rb-bridge-pending")

	restore := bridge.SlowExportReadForTest(0, time.Nanosecond)
	r1, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	restore()
	if err != nil {
		t.Fatalf("first mount: %v", err)
	}
	if r1.Deferred == 0 {
		t.Fatalf("expected deferred tracks under 1ns budget, got %+v", r1)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("expected .rb-bridge-pending after <=500-track deferred mount, got %v", err)
	}

	r2, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{Deadline: time.Now().Add(time.Minute)}, false)
	if err != nil || r2.Deferred != 0 || r2.Updated == 0 {
		t.Fatalf("second resumed mount: %+v %v", r2, err)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("expected .rb-bridge-pending removed once completed, got %v", err)
	}
}
