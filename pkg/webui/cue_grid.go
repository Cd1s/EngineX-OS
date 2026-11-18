package webui

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// cueGridReply is the saved beat grid of a deck's track for the hot cue countdown (#151): sample rate and the grid markers (the
// sample position and Engine's beat index at each tempo anchor). The UI pack maps hot cue and playhead positions to beats on it,
// exact across tempo changes, which Engine's QML (tempo at the playhead only) cannot give.
type cueGridReply struct {
	Rate    float64               `json:"rate"`
	Markers []enginedb.GridMarker `json:"markers"`
}

// cueGridGet answers GET /v1/enginex/grid?net=<the deck's TrackNetworkPath>. Read-only on Engine's library; one bounded query.
func (s *Server) cueGridGet(w http.ResponseWriter, r *http.Request) {
	np := r.URL.Query().Get("net")
	f := ""
	if len(np) <= 1024 {
		f = resolveNetPathFile(np, s.shadow.dataRoot, s.shadow.mediaRoot, false)
	}
	if f == "" {
		http.Error(w, "no file", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 750*time.Millisecond)
	defer cancel()
	rate, markers, err := libraryGrid(ctx, f, s.shadow.dataRoot, s.shadow.mediaRoot)
	if err != nil {
		http.Error(w, "no grid", http.StatusNotFound)
		return
	}
	if markers == nil {
		markers = []enginedb.GridMarker{}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cueGridReply{Rate: rate, Markers: markers})
}

func libraryGrid(ctx context.Context, f, dataRoot, mediaRoot string) (float64, []enginedb.GridMarker, error) {
	lib := engineLibrary(f, dataRoot, mediaRoot)
	rel, err := filepath.Rel(lib, f)
	if err != nil {
		return 0, nil, err
	}
	for _, name := range []string{"m.db", "stm.db"} {
		var rate float64
		var markers []enginedb.GridMarker
		if rate, markers, err = dbGrid(ctx, filepath.Join(lib, "Database2", name), filepath.ToSlash(rel)); err == nil {
			return rate, markers, nil
		}
	}
	return 0, nil, err
}

func dbGrid(ctx context.Context, file, rel string) (float64, []enginedb.GridMarker, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: file}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(100)")
	if err != nil {
		return 0, nil, err
	}
	defer db.Close()
	var track, beats []byte
	err = db.QueryRowContext(ctx, `SELECT substr(p.trackData,1,4097), substr(p.beatData,1,8388609) FROM Track t JOIN PerformanceData p ON p.trackId=t.id
		WHERE t.path=? AND length(p.trackData)>4 AND length(p.beatData)>4`, rel).Scan(&track, &beats)
	if err != nil {
		return 0, nil, err
	}
	return enginedb.ParseDisplayGrid(track, beats)
}
