package webui

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
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

// stemsImportFile is the file Engine imports for a streamed row's Stems: the
// download under a name that says what it is, with its tags. The cache names
// files by id and services send audio without tags, so the internal library
// showed the row as "<digits>.flac". The file is a tagged copy (FLAC, MP3) or
// a hard link (anything else) in a directory of its own per track, which cache
// eviction does not look at. One made earlier is kept: Engine's library row
// and its Stems belong to that file. When anything fails Engine gets the
// cache file, as before.
func stemsImportFile(cache string, t providers.Track) string {
	artist := t.Artist
	if artist == "" {
		artist = strings.Join(t.Artists, ", ")
	}
	ext := filepath.Ext(cache)
	name := stemsFileName(artist, t.Title, ext)
	if name == "" {
		return cache
	}
	dir := filepath.Join(filepath.Dir(cache), "Stems", strings.TrimSuffix(filepath.Base(cache), ext))
	named := filepath.Join(dir, name+ext)
	if st, err := os.Stat(named); err == nil && st.Mode().IsRegular() {
		return named
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return cache
	}
	tagged, err := deemixclient.CopyTagged(cache, named, t.Title, artist, t.Album)
	if err != nil {
		log.Printf("stream-sync: stems source tags: %v", err)
	}
	if tagged && err == nil {
		return named
	}
	if err = os.Link(cache, named); err != nil {
		log.Printf("stream-sync: stems source name: %v", err)
		return cache
	}
	return named
}

// stemsFileName is "Artist - Title" as a file name the UI can put in a file:
// URL as it is: no path separator, and none of '#', '?' and '%', which
// encodeURI leaves alone or which would start a fragment or query. With ext it
// is at most 255 bytes, the longest name a file system takes, and it is cut
// between characters: 80 emoji are 320 bytes, and the link then failed.
func stemsFileName(artist, title, ext string) string {
	clean := func(s string) string {
		s = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\#?%`, r) {
				return ' '
			}
			return r
		}, s)
		return strings.Join(strings.Fields(s), " ")
	}
	name := clean(title)
	if a := clean(artist); a != "" && name != "" {
		name = a + " - " + name
	}
	if limit := max(255-len(ext), 0); len(name) > limit {
		for limit > 0 && !utf8.RuneStart(name[limit]) {
			limit--
		}
		name = name[:limit]
	}
	return strings.Trim(name, ". ")
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

// A row joins the list when its download is handed to Engine, and Engine then
// needs about as long as the track to render the Stems on the deck. Until they
// exist the row is shown as stock shows an unavailable track, saying so:
// loaded as a track it had no Stems, and nothing said why.
func (g *tidalGateway) stemsListTracks(w http.ResponseWriter, r *http.Request, scope string) {
	list := g.s.stemmed(scope)
	paths := make([]string, len(list))
	for i, e := range list {
		paths[i] = e.Path
	}
	pending := stemsPending(r.Context(), paths)
	items := []any{}
	for _, e := range list {
		if pending[e.Path] {
			items = append(items, g.stemsPendingRow(e.Track))
			continue
		}
		items = append(items, g.trackItems(e.Session, e.Guest, []providers.Track{e.Track})...)
	}
	respond(w, page(r, 500, items))
}

func (g *tidalGateway) stemsPendingRow(t providers.Track) any {
	title, hint := "Stems not ready", "Rendering can take as long as the track. Go back and reopen this list."
	g.s.uiMu.Lock()
	lang := g.s.loadUI().Language
	g.s.uiMu.Unlock()
	switch lang {
	case "zh-Hans":
		title, hint = "Stems 尚未完成", "分轨耗时可能与曲长相当，请稍后返回并重新打开此列表。"
	case "zh-Hant":
		title, hint = "Stems 尚未完成", "分軌耗時可能與曲長相當，請稍後返回並重新開啟此清單。"
	}
	return g.trackItems("", false, []providers.Track{{Provider: listNoticeProvider, ID: "stems-" + t.Provider + "-" + t.ID, Title: title + " — " + t.Title, Artist: hint}})[0]
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
