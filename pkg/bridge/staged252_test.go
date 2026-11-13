package bridge_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestStaged252HiddenExportMergeAndStamp(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	staged := filepath.Join(root, ".prolink-cache", "CDJ 1 USB")
	testfixture.Export(t, staged)
	if err := os.Rename(filepath.Join(staged, "PIONEER"), filepath.Join(staged, ".PIONEER")); err != nil {
		t.Fatal(err)
	}
	l, err := bridge.ReadLibrary(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Tracks) != 10 {
		t.Fatalf("own 5 + hidden staged 5: got %d", len(l.Tracks))
	}
	for _, track := range l.Tracks {
		if track.ID>>24 != 0 && track.AnalysisPath != "" {
			if _, err := os.Stat(filepath.Join(root, track.AnalysisPath)); err != nil {
				t.Fatalf("hidden analysis path %q: %v", track.AnalysisPath, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "Engine Library")); !os.IsNotExist(err) {
		t.Fatal("read wrote an Engine database")
	}
	r, err := bridge.ConvertAtMount(t.Context(), root, enginedb.Options{}, true)
	if err != nil || r.Added != 10 {
		t.Fatalf("hidden staged import: %+v %v", r, err)
	}
	if r, err = bridge.ConvertAtMount(t.Context(), root, enginedb.Options{}, false); err != nil || !r.Bypassed {
		t.Fatalf("unchanged: %+v %v", r, err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(staged, ".PIONEER/rekordbox/export.pdb"), later, later); err != nil {
		t.Fatal(err)
	}
	if r, err = bridge.ConvertAtMount(t.Context(), root, enginedb.Options{}, false); err != nil || r.Bypassed || r.Added != 0 {
		t.Fatalf("restaged: %+v %v", r, err)
	}
}
