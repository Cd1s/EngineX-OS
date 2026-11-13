package webui

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Cd1s/EngineX-OS/pkg/mods"
)

type modItemResponse struct {
	Category    string `json:"category,omitempty"`
	Official    bool   `json:"official,omitempty"`
	Installed   bool   `json:"installed"`
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

// modsResume re-runs the start script of every enabled mod after a reboot.
func (s *Server) modsResume() { s.modsManager().StartEnabled() }

func (s *Server) activePackDir() string {
	active := filepath.Join(s.uiDir(), "active")
	if st, err := os.Stat(active); err == nil && st.IsDir() {
		return active
	}
	return ""
}

// handleMods handles GET and POST requests for /v1/enginex/mods.
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
		res.Mods = append(res.Mods, modItemResponse{ID: "tailscale-remote", Name: "Tailscale / Headscale Remote Access", Version: tailscaleVersion, Author: "EngineX Official", Category: "network", Official: true, Drive: "system", Valid: true, Installed: s.tailscaleInstalled(), Enabled: s.loadTailscaleConfig().Enabled})
		for _, m := range discovered {
			if m.ID == "tailscale-remote" {
				continue
			}
			res.Mods = append(res.Mods, modItemResponse{
				Category: m.Category, Official: m.Official, Installed: true,
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
		if req.ID == "tailscale-remote" {
			tailscaleMu.Lock()
			defer tailscaleMu.Unlock()
			cfg := s.loadTailscaleConfig()
			if req.Action == "uninstall" {
				cfg.Enabled = false
				if err := s.saveTailscaleConfig(cfg); err != nil {
					fail(w, 500, err)
					return
				}
				s.tailscaleStop(r.Context())
				for _, path := range []string{s.tailscaleCLI(), s.tailscaleBin()} {
					if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
						fail(w, 500, err)
						return
					}
				}
			} else {
				if req.Action != "" && req.Action != "install" {
					fail(w, 400, errors.New("invalid action"))
					return
				}
				if req.Action == "install" || (req.Enabled != nil && *req.Enabled && !s.tailscaleInstalled()) {
					if err := s.tailscaleInstall(r.Context()); err != nil {
						fail(w, 500, err)
						return
					}
				}
				if req.Enabled != nil {
					cfg.Enabled = *req.Enabled
					if cfg.Enabled {
						if err := s.tailscaleStart(r.Context()); err != nil {
							fail(w, 500, err)
							return
						}
					} else {
						s.tailscaleStop(r.Context())
					}
					if err := s.saveTailscaleConfig(cfg); err != nil {
						fail(w, 500, err)
						return
					}
				} else if req.Action == "" {
					fail(w, 400, errors.New("missing action or enabled"))
					return
				}
			}
			respond(w, map[string]any{"ok": true})
			return
		}
		if req.Action == "uninstall" && req.ID != "" {
			if err := mgr.UninstallMod(req.ID); err != nil {
				fail(w, 400, err)
				return
			}
			if err := mgr.ApplyActiveOverlay(s.activePackDir()); err != nil {
				fail(w, 500, err)
				return
			}
			respond(w, map[string]any{"ok": true})
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

func (s *Server) handleModsUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, (16<<20)+(1<<20))
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	var input io.Reader = r.Body
	switch mediaType {
	case "multipart/form-data":
		file, _, err := r.FormFile("file")
		if err != nil {
			fail(w, 400, err)
			return
		}
		defer file.Close()
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		input = file
	case "application/gzip", "application/x-gzip", "application/octet-stream":
	default:
		fail(w, 415, errors.New("unsupported content type"))
		return
	}
	mgr := s.modsManager()
	mod, err := mgr.InstallArchive(input)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if err := mgr.ApplyActiveOverlay(s.activePackDir()); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, mod)
}
