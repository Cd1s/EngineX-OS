package webui

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// overviewScaleGet tells the deck's overview whether Engine scales the overview of the
// track at ?path=. An import by an earlier build ends in a zero triple where Engine
// expects each band's maximum, and Engine draws it at a third of the height. rb-bridge
// completes those when the stick is inserted and vouches for the library in a file of its
// own (enginedb.VouchOverviews). The answer comes from that file alone: m.db belongs to
// Engine while the stick is in. The overview keeps the stretch it had before for every
// track this does not vouch for: a library rb-bridge could not write, a stick that stayed
// in, an import by an earlier build since.
func (s *Server) overviewScaleGet(w http.ResponseWriter, r *http.Request) {
	if !overviewVouched(s.shadow.mediaRoot, r.URL.Query().Get("path")) {
		http.Error(w, "not vouched for", http.StatusNotFound)
		return
	}
	w.Write([]byte("scaled"))
}

// overviewVouched reports whether f, a track's path as Engine gives it, is on a volume
// under mediaRoot whose library rb-bridge vouches for. Engine's name for the volume is
// its directory there or a name of its own; then the track is on every volume that holds
// the file, and each of them has to vouch.
func overviewVouched(mediaRoot, f string) bool {
	mediaRoot = filepath.Clean(mediaRoot)
	rel, err := filepath.Rel(mediaRoot, f)
	if err != nil || !filepath.IsLocal(rel) {
		return false
	}
	name, track, ok := strings.Cut(rel, string(filepath.Separator))
	if !ok {
		return false
	}
	volumes := []string{name}
	if _, err := os.Lstat(filepath.Join(mediaRoot, name)); err != nil {
		volumes = nil
		entries, _ := os.ReadDir(mediaRoot)
		for _, e := range entries {
			if st, err := os.Lstat(filepath.Join(mediaRoot, e.Name(), track)); e.IsDir() && err == nil && st.Mode().IsRegular() {
				volumes = append(volumes, e.Name())
			}
		}
	}
	for _, volume := range volumes {
		if !volumeVouches(mediaRoot, volume, track) {
			return false
		}
	}
	return len(volumes) > 0
}

// volumeVouches reports whether the library of the volume vouches for the track's
// overview. No symlink may stand between mediaRoot and the vouch: it could lead to a
// library elsewhere.
func volumeVouches(mediaRoot, volume, track string) bool {
	dir := filepath.Join(mediaRoot, volume, "Engine Library", "Database2")
	for p := dir; p != mediaRoot; p = filepath.Dir(p) {
		if st, err := os.Lstat(p); err != nil || !st.IsDir() {
			return false
		}
	}
	raw, ok := enginedb.OverviewVouch(dir)
	return ok && !slices.Contains(raw, "../"+filepath.ToSlash(track)) // Track.path, from "Engine Library"
}
