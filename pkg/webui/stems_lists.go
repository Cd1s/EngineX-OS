package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// Each streaming service keeps its own Stems list: the tracks stemmed from
// that service's rows, listed first among its playlists, never mixed with the
// USB Stems list or with another service. Path is the download Engine
// imported and stemmed; a deck loading the streamed row swaps to it.
type stemmedTrack struct {
	Track   providers.Track `json:"track"`
	Session string          `json:"session,omitempty"`
	Guest   bool            `json:"guest,omitempty"`
	Path    string          `json:"path"`
}

const maxStemmed = 500

func stemsListID(scope string) string { return "sc5000-stems-" + scope }

func (s *Server) stemsFile() string {
	if s.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.StateDir, "streaming-stems.json")
}

func (s *Server) loadStemmed() map[string][]stemmedTrack {
	all := map[string][]stemmedTrack{}
	if data, err := os.ReadFile(s.stemsFile()); err == nil {
		json.Unmarshal(data, &all)
	}
	return all
}

func sameTrack(a, b providers.Track) bool { return a.Provider == b.Provider && a.ID == b.ID }

// addStemmed puts t first in scope's list.
func (s *Server) addStemmed(scope string, t stemmedTrack) error {
	s.stemsMu.Lock()
	defer s.stemsMu.Unlock()
	all := s.loadStemmed()
	list := slices.DeleteFunc(all[scope], func(e stemmedTrack) bool { return sameTrack(e.Track, t.Track) })
	all[scope] = append([]stemmedTrack{t}, list[:min(len(list), maxStemmed-1)]...)
	return providers.SaveJSON(s.stemsFile(), all)
}

func (s *Server) stemmed(scope string) []stemmedTrack {
	s.stemsMu.Lock()
	defer s.stemsMu.Unlock()
	return s.loadStemmed()[scope]
}

var statStemmedPath = os.Stat

// stemmedPath is the stemmed local copy of t, if it is still on disk.
func (s *Server) stemmedPath(t providers.Track) string {
	s.stemsMu.Lock()
	var candidates []string
	for _, list := range s.loadStemmed() {
		for _, e := range list {
			if sameTrack(e.Track, t) {
				candidates = append(candidates, e.Path)
			}
		}
	}
	s.stemsMu.Unlock()
	for _, p := range candidates {
		if st, err := statStemmedPath(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

func (g *tidalGateway) stemsListTracks(w http.ResponseWriter, r *http.Request, scope string) {
	items := []any{}
	for _, e := range g.s.stemmed(scope) {
		items = append(items, g.trackItems(e.Session, e.Guest, []providers.Track{e.Track})...)
	}
	respond(w, page(r, 500, items))
}

// stemsLocal answers whether a streamed row has a stemmed local copy.
func (g *tidalGateway) stemsLocal(w http.ResponseWriter, r *http.Request) {
	if _, ref, ok := g.lookup(w, r.URL.Query().Get("id")); ok {
		if p := g.s.stemmedPath(ref.track); p != "" {
			respond(w, map[string]any{"path": p})
		} else {
			http.NotFound(w, r)
		}
	}
}
