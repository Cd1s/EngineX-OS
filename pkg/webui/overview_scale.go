package webui

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// overviewScaleGet tells the deck's overview whether Engine scales the overview of the
// track at ?path=. An import by an earlier build ends in a zero triple where Engine
// expects each band's maximum, and Engine draws it at a third of the height. rb-bridge
// completes those when the stick is inserted, but not in a library it cannot write, on a
// stick that stayed in, or after a scan that failed. The overview keeps the stretch it
// had before for every track this does not vouch for.
func (s *Server) overviewScaleGet(w http.ResponseWriter, r *http.Request) {
	f := r.URL.Query().Get("path")
	lib := engineLibrary(f, s.shadow.dataRoot, s.shadow.mediaRoot)
	rel, err := filepath.Rel(lib, f)
	if err != nil || !overviewScaled(r.Context(), filepath.Join(lib, "Database2", "m.db"), filepath.ToSlash(rel)) {
		http.Error(w, "no band maxima", http.StatusNotFound)
		return
	}
	w.Write([]byte("scaled"))
}

// overviewScaled reports whether the library lists the track with an overview that has
// its band maxima, or with none yet: Engine then builds its own. Read-only: Engine owns
// the database (#49).
func overviewScaled(ctx context.Context, mdb, rel string) bool {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
	if err != nil {
		return false
	}
	defer db.Close()
	var blob []byte
	if db.QueryRowContext(ctx, `SELECT substr(p.overviewWaveFormData,1,8388609) FROM Track t LEFT JOIN PerformanceData p ON p.trackId=t.id WHERE t.path=?`, rel).Scan(&blob) != nil {
		return false
	}
	if len(blob) == 0 {
		return true
	}
	raw, err := enginedb.QUncompress(blob, 8<<20)
	n := len(raw)
	return err == nil && n >= 27 && raw[n-1]|raw[n-2]|raw[n-3] != 0
}
