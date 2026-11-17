package rekordbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StagedDir holds libraries copied from other players, in the export's layout.
const StagedDir = ".prolink-cache"

// GeneratedMarker identifies a CDJ export made from an Engine library.
const GeneratedMarker = "PIONEER/rekordbox/.enginex-generated"

// GeneratedExport is false again as soon as rekordbox rewrites export.pdb.
func GeneratedExport(root string) bool {
	st, err := os.Stat(filepath.Join(root, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(root, GeneratedMarker))
	return err == nil && string(b) == fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
}

// SafeRel keeps paths from a staged export inside its root.
func SafeRel(p string) (string, bool) {
	rel := filepath.Clean(strings.TrimPrefix(p, "/"))
	return rel, p != "" && filepath.IsLocal(rel)
}
