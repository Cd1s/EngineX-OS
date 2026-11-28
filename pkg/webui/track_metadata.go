package webui

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

type trackMetadataPostReq struct {
	Drive       string `json:"drive"`
	TrackID     int64  `json:"trackId"`
	TrackURI    string `json:"trackUri,omitempty"`
	NetworkPath string `json:"trackNetworkPath,omitempty"`
	StockEdit   bool   `json:"stockEdit,omitempty"`
	Genre       string `json:"genre"`
	Comment     string `json:"comment"`
	Label       string `json:"label"`
}

type trackMetadataResponse struct {
	Drive   string `json:"drive"`
	TrackID int64  `json:"trackId"`
	Genre   string `json:"genre"`
	Comment string `json:"comment"`
	Label   string `json:"label"`
	Pending bool   `json:"pending,omitempty"`
}

func (s *Server) handleTrackMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req trackMetadataPostReq
	if !input(w, r, &req) {
		return
	}
	if len(req.Genre) > 1024 || len(req.Comment) > 4096 || len(req.Label) > 1024 {
		fail(w, http.StatusBadRequest, errors.New("metadata field is too long"))
		return
	}
	driveDir, drive, err := s.resolveTrackMetadataDrive(req)
	if err != nil {
		if req.StockEdit && (req.Drive == "" || !strings.ContainsAny(req.Drive, `/\\`)) {
			respond(w, trackMetadataResponse{Drive: req.Drive, TrackID: req.TrackID, Genre: req.Genre, Comment: req.Comment, Label: req.Label, Pending: true})
			return
		}
		fail(w, http.StatusBadRequest, err)
		return
	}
	trackID := req.TrackID
	if trackID <= 0 {
		trackID, err = findTrackID(context.Background(), driveDir, req.TrackURI, req.NetworkPath)
		if err != nil {
			fail(w, http.StatusConflict, err)
			return
		}
	}
	dbPath := filepath.Join(driveDir, "Engine Library", "Database2", "m.db")
	if st, err := os.Stat(dbPath); err != nil || !st.Mode().IsRegular() {
		if req.StockEdit {
			respond(w, trackMetadataResponse{Drive: drive, TrackID: trackID, Genre: req.Genre, Comment: req.Comment, Label: req.Label, Pending: true})
			return
		}
		fail(w, http.StatusConflict, errors.New("Engine library is unavailable"))
		return
	}
	if !trackExists(r.Context(), dbPath, trackID) {
		if req.StockEdit {
			respond(w, trackMetadataResponse{Drive: drive, TrackID: trackID, Genre: req.Genre, Comment: req.Comment, Label: req.Label, Pending: true})
			return
		}
		fail(w, http.StatusConflict, errors.New("track is not in the Engine library"))
		return
	}
	// The edit is written, or queued, under the volume's lock. Busy: nothing is written and the
	// caller is told so (#336 Go review R13).
	ctx, release, err := enginedb.HoldForEdit(r.Context(), driveDir)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	defer release()
	pending := false
	if enginedb.LibraryInUse(driveDir) {
		err := enginedb.QueuePending(ctx, driveDir, func(p *enginedb.Pending) {
			p.TrackMetadata = append(p.TrackMetadata, enginedb.PendingTrackMetadata{
				TrackID: trackID,
				Genre:   req.Genre,
				Comment: req.Comment,
				Label:   req.Label,
			})
		})
		if err != nil {
			fail(w, http.StatusConflict, err)
			return
		}
		pending = true
	} else {
		db, err := enginedb.OpenLibraryRW(ctx, driveDir, "_pragma=busy_timeout(1000)")
		if err != nil {
			fail(w, http.StatusConflict, err)
			return
		}
		db.SetMaxOpenConns(1)
		result, execErr := db.ExecContext(ctx, `UPDATE Track SET genre=?, comment=?, label=? WHERE id=?`, req.Genre, req.Comment, req.Label, trackID)
		_ = db.Close()
		if execErr != nil {
			fail(w, http.StatusConflict, execErr)
			return
		}
		if changed, changedErr := result.RowsAffected(); changedErr != nil || changed != 1 {
			fail(w, http.StatusConflict, errors.New("track metadata was not updated"))
			return
		}
	}
	respond(w, trackMetadataResponse{Drive: drive, TrackID: trackID, Genre: req.Genre, Comment: req.Comment, Label: req.Label, Pending: pending})
}

func trackExists(ctx context.Context, dbPath string, trackID int64) bool {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath}).String()+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return false
	}
	defer db.Close()
	var one int
	return db.QueryRowContext(ctx, `SELECT 1 FROM Track WHERE id=? LIMIT 1`, trackID).Scan(&one) == nil && one == 1
}

func (s *Server) resolveTrackMetadataDrive(req trackMetadataPostReq) (string, string, error) {
	if req.Drive != "" {
		dir, err := validateDrive(s.mediaRoot(), req.Drive)
		if err != nil {
			return "", "", err
		}
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			return "", "", errors.New("drive not found")
		}
		return dir, req.Drive, nil
	}
	if req.TrackID > 0 && strings.TrimSpace(req.TrackURI) == "" && strings.TrimSpace(req.NetworkPath) == "" {
		var matchDir, matchName string
		for _, entry := range s.mediaEntries() {
			dir, err := validateDrive(s.mediaRoot(), entry)
			if err != nil {
				continue
			}
			dbPath := filepath.Join(dir, "Engine Library", "Database2", "m.db")
			db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath}).String()+"?mode=ro&_pragma=query_only(1)")
			if err != nil {
				continue
			}
			var found int
			_ = db.QueryRow(`SELECT 1 FROM Track WHERE id=? LIMIT 1`, req.TrackID).Scan(&found)
			_ = db.Close()
			if found != 1 {
				continue
			}
			if matchDir != "" {
				return "", "", errors.New("track identity matches multiple drives")
			}
			matchDir, matchName = dir, entry
		}
		if matchDir != "" {
			return matchDir, matchName, nil
		}
	}
	for _, entry := range s.mediaEntries() {
		dir, err := validateDrive(s.mediaRoot(), entry)
		if err != nil {
			continue
		}
		if _, err := findTrackID(context.Background(), dir, req.TrackURI, req.NetworkPath); err == nil {
			return dir, entry, nil
		}
	}
	return "", "", errors.New("track drive not found")
}

func (s *Server) mediaEntries() []string {
	entries, _ := os.ReadDir(s.mediaRoot())
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

func findTrackID(ctx context.Context, driveDir, trackURI, networkPath string) (int64, error) {
	dbPath := filepath.Join(driveDir, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath}).String()+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var id int64
	for _, identity := range []string{trackURI, networkPath} {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			continue
		}
		path := identity
		if u, err := url.Parse(identity); err == nil && u.Path != "" {
			path = u.Path
		}
		path, _ = url.PathUnescape(path)
		base := filepath.Base(path)
		if err := db.QueryRowContext(ctx, `SELECT id FROM Track WHERE path=? OR filename=? OR path LIKE ? ORDER BY id DESC LIMIT 1`, path, base, "%"+base).Scan(&id); err == nil {
			return id, nil
		}
	}
	return 0, errors.New("track identity not found in Engine library")
}
