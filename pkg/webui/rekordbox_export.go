package webui

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

type rekordboxExportReq struct {
	Drive  string `json:"drive"`
	OS     string `json:"os"`
	Letter string `json:"letter"`
}

type rekordboxExportResp struct {
	Success     bool   `json:"success"`
	Path        string `json:"path"`
	TotalTracks int    `json:"totalTracks"`
	CuesCount   int    `json:"cuesCount"`
	LoopsCount  int    `json:"loopsCount"`
	Playlists   int    `json:"playlists"`
}

func (s *Server) handleRekordboxExport(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleRekordboxExportGet(w, r)
	case http.MethodPost:
		s.handleRekordboxExportPost(w, r)
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleRekordboxExportGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	drive := q.Get("drive")
	driveDir, err := validateDrive(s.mediaRoot(), drive)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}

	targetOS := q.Get("os")
	letter := q.Get("letter")

	xmlDB, _, err := bridge.BuildRekordboxXML(r.Context(), bridge.XMLExportOptions{
		MountPath:   driveDir,
		DriveName:   drive,
		TargetOS:    targetOS,
		DriveLetter: letter,
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="rekordbox.xml"`)
	_, _ = xmlDB.WriteTo(w)
}

func (s *Server) handleRekordboxExportPost(w http.ResponseWriter, r *http.Request) {
	var req rekordboxExportReq
	if r.Header.Get("Content-Type") == "application/json" || r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Drive == "" {
		req.Drive = r.URL.Query().Get("drive")
	}
	if req.OS == "" {
		req.OS = r.URL.Query().Get("os")
	}
	if req.Letter == "" {
		req.Letter = r.URL.Query().Get("letter")
	}

	driveDir, err := validateDrive(s.mediaRoot(), req.Drive)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}

	summary, err := bridge.ExportRekordboxXML(r.Context(), bridge.XMLExportOptions{
		MountPath:   driveDir,
		DriveName:   req.Drive,
		TargetOS:    req.OS,
		DriveLetter: req.Letter,
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	respond(w, rekordboxExportResp{
		Success:     true,
		Path:        summary.Path,
		TotalTracks: summary.TotalTracks,
		CuesCount:   summary.CuesCount,
		LoopsCount:  summary.LoopsCount,
		Playlists:   summary.Playlists,
	})
}
