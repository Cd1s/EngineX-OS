package webui

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

func (s *Server) handleRekordboxWriteback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	drive := strings.TrimSpace(r.URL.Query().Get("drive"))
	if drive == "" || drive == "." || drive == ".." || strings.ContainsAny(drive, `/\`) {
		fail(w, http.StatusBadRequest, errors.New("invalid drive name"))
		return
	}
	target := filepath.Join(s.mediaRoot(), drive)
	st, err := os.Lstat(target)
	if err != nil || !st.IsDir() {
		fail(w, http.StatusNotFound, errors.New("drive not found"))
		return
	}
	res, err := bridge.WriteBackCues(r.Context(), target, bridge.WriteBackOptions{})
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	respond(w, res)
}
