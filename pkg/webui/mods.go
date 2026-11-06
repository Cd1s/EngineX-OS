package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Cd1s/EngineX-OS/pkg/mods"
)

type modItemResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Author      string `json:"author"`
	Description string `json:"description"`
	Drive       string `json:"drive"`
	Enabled     bool   `json:"enabled"`
	Valid       bool   `json:"valid"`
	Error       string `json:"error"`
	QMLCount    int    `json:"qmlCount"`
}

type modsListResponse struct {
	Mods                []modItemResponse `json:"mods"`
	SafeModeQuarantined bool              `json:"safeModeQuarantined"`
}

type modPostRequest struct {
	ID      string `json:"id,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
	Action  string `json:"action,omitempty"`
}

func (s *Server) modsManager() *mods.Manager {
	return mods.NewManager(s.mediaRoot(), s.uiDir())
}

func (s *Server) activePackDir() string {
	active := filepath.Join(s.uiDir(), "active")
	if st, err := os.Stat(active); err == nil && st.IsDir() {
		return active
	}
	return ""
}

// handleMods handles GET and POST requests for /v1/sc5000/mods.
func (s *Server) handleMods(w http.ResponseWriter, r *http.Request) {
	mgr := s.modsManager()
	switch r.Method {
	case http.MethodGet:
		discovered, err := mgr.Discover()
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		res := modsListResponse{
			Mods:                make([]modItemResponse, 0, len(discovered)),
			SafeModeQuarantined: mgr.IsSafeModeQuarantined(),
		}
		for _, m := range discovered {
			res.Mods = append(res.Mods, modItemResponse{
				ID:          m.ID,
				Name:        m.Name,
				Version:     m.Version,
				Author:      m.Author,
				Description: m.Description,
				Drive:       m.Drive,
				Enabled:     m.Enabled,
				Valid:       m.Valid,
				Error:       m.Error,
				QMLCount:    m.QMLCount,
			})
		}
		respond(w, res)

	case http.MethodPost:
		var req modPostRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, errors.New("invalid JSON"))
			return
		}
		if req.Action == "quarantine_reset" {
			if err := mgr.ResetQuarantine(); err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
			if err := mgr.ApplyActiveOverlay(s.activePackDir()); err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
			respond(w, map[string]any{"ok": true, "safeModeQuarantined": false})
			return
		}
		if req.ID != "" && req.Enabled != nil {
			if err := mgr.SetModEnabled(req.ID, *req.Enabled); err != nil {
				fail(w, http.StatusBadRequest, err)
				return
			}
			if err := mgr.ApplyActiveOverlay(s.activePackDir()); err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
			respond(w, map[string]any{"ok": true})
			return
		}
		fail(w, http.StatusBadRequest, errors.New("missing mod id/enabled or action"))

	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}
