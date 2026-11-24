package webui

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

// libraryStatusGet reports whether a mounted drive under mediaRoot holds a
// populated Engine Library database (Engine Library/Database2/m.db with valid
// Information and at least one Track row), distinguishing converted/populated
// libraries from stock Engine's empty skeleton m.db.
func (s *Server) libraryStatusGet(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("path"))
	if raw == "" || !filepath.IsAbs(raw) {
		respond(w, map[string]bool{"converted": false})
		return
	}
	clean := filepath.Clean(raw)
	root := filepath.Clean(s.mediaRoot())
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		respond(w, map[string]bool{"converted": false})
		return
	}
	respond(w, map[string]bool{"converted": bridge.HasPopulatedLibrary(clean)})
}
