package bridge_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

// An export.pdb the CDJ bridge made from the drive's own Engine library (#63) is
// not imported back into it; once rekordbox rewrites it, it is imported again.
func TestGeneratedExportIsNotImported(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	pdb := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
	st, err := os.Stat(pdb)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "PIONEER", "rekordbox", ".enginex-generated")
	os.WriteFile(marker, []byte(fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())), 0o644)
	// As for a drive with no export at all: nothing is read.
	if l, err := bridge.ReadLibrary(t.Context(), root); !errors.Is(err, os.ErrNotExist) && (err != nil || len(l.Tracks) != 0) {
		t.Fatalf("generated export imported: %v %v", l, err)
	}
	os.WriteFile(marker, []byte("0:0"), 0o644) // rekordbox exported to the stick since
	if l, err := bridge.ReadLibrary(t.Context(), root); err != nil || len(l.Tracks) == 0 {
		t.Fatalf("real export skipped: %v", err)
	}
}
