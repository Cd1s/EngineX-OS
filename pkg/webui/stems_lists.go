package webui

import (
	"context"
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
//
// The stream cache keeps reserve bytes of its volume free by evicting its own
// files. Nothing may evict a file Engine has imported, so none is added that
// would leave less: no copy that does not fit above the reserve (the name is
// then a link, without tags), and below the reserve not even a link, which
// would keep a download's bytes after the cache has dropped it.
func stemsImportFile(cache string, t providers.Track, reserve int64) string {
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
	room := func(need int64) bool {
		free, _, _, err := stemsFreeSpace(filepath.Dir(cache))
		return reserve <= 0 || err != nil || free-need >= reserve
	}
	if !room(0) {
		return cache
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return cache
	}
	if st, err := os.Stat(cache); err == nil && room(st.Size()) {
		tagged, err := deemixclient.CopyTagged(cache, named, t.Title, artist, t.Album)
		if err != nil {
			log.Printf("stream-sync: stems source tags: %v", err)
		}
		if tagged && err == nil {
			return named
		}
	}
	if err := os.Link(cache, named); err != nil {
		log.Printf("stream-sync: stems source name: %v", err)
		os.Remove(dir) // only when nothing is in it
		return cache
	}
	return named
}

var stemsFreeSpace = freeSpace

// sweepStemsSources removes the imported files under root that nothing refers
// to any more: in no service's Stems list (the record dropped off the end of
// its list, the track was imported again under another name, or a copy was
// left half written) and not a track in Engine's library. A file Engine has
// as a track stays, whatever the lists say: its library row and its Stems
// belong to it.
//
// It only looks at regular files one directory below root, which is the tree
// stemsImportFile writes, and only on the deck's own storage: a file there is
// in the internal library, while a file on a drive is in that drive's library,
// which is not read here. When the library cannot be read nothing is removed.
func (s *Server) sweepStemsSources(ctx context.Context, root string) {
	if rel, err := filepath.Rel(shadowDataRoot, root); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	recorded := map[string]bool{}
	s.stemsMu.Lock()
	for _, list := range s.loadStemmed() {
		for _, e := range list {
			recorded[e.Path] = true
			if real, err := filepath.EvalSymlinks(e.Path); err == nil {
				recorded[real] = true
			}
		}
	}
	s.stemsMu.Unlock()
	var loose []string
	dirs, _ := os.ReadDir(root)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, d.Name()))
		for _, f := range files {
			p := filepath.Join(root, d.Name(), f.Name())
			if real, err := filepath.EvalSymlinks(p); f.Type().IsRegular() && !recorded[p] && (err != nil || !recorded[real]) {
				loose = append(loose, p)
			}
		}
	}
	if len(loose) == 0 {
		return
	}
	tracks, err := internalTrackIDs(ctx, loose...)
	if err != nil {
		return
	}
	for _, p := range loose {
		if _, imported := tracks[p]; imported {
			continue
		}
		if err := os.Remove(p); err == nil {
			log.Printf("stream-sync: removed a stems source nothing refers to: %s", filepath.Base(filepath.Dir(p)))
			os.Remove(filepath.Dir(p)) // only when it is empty now
		}
	}
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
