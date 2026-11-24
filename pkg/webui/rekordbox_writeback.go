package webui

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	bridgev2 "github.com/Cd1s/EngineX-OS/pkg/bridge/v2"
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
	// final=1 is the Eject's pass (rb-bridge --writeback-cues --on-eject, from the edisksd
	// hook): it reuses what the watcher kept for the stick, its exportLibrary.db keys first. It
	// skips the check for a conversion at mount, so only the deck itself asks for it; a request
	// from elsewhere gets a plain pass (#321 review R2).
	final := r.URL.Query().Get("final") == "1" && writeBackFromDeck(r)
	var res bridgev2.WriteBackResult
	switch cw := s.cueWatch.Load(); {
	case cw != nil && final:
		res, err = cw.Final(r.Context(), target)
	case cw != nil:
		res, err = cw.Pass(r.Context(), target)
	default:
		res, err = bridgev2.WriteBackCues(r.Context(), target, bridgev2.WriteBackOptions{OnEject: final})
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	respond(w, res)
}

// writeBackFromDeck reports whether r comes from the deck itself (rb-bridge, on loopback, not
// relayed from outside).
func writeBackFromDeck(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback() && !relayedFromOutside(r)
}

// WatchCues writes a deck's cue edits back to the rekordbox sticks under the media root by
// itself, until ctx ends (#321, bridge.CueWatcher).
func (s *Server) WatchCues(ctx context.Context) {
	w := bridgev2.NewCueWatcher(s.mediaRoot())
	s.cueWatch.Store(w)
	defer s.cueWatch.Store(nil)
	if err := w.Run(ctx); err != nil {
		log.Printf("stream-sync: cue write-back watcher: %v", err)
	}
}
