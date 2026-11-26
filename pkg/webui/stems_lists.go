package webui

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/Cd1s/EngineX-OS/internal/migration"
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

func stemsListID(scope string) string { return "enginex-stems-" + scope }

func isStemsListID(id, scope string) bool {
	return id == stemsListID(scope) || id == migration.StemsListPrefix+scope
}

func (s *Server) stemsFile() string {
	if s.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.StateDir, "streaming-stems.json")
}

func (s *Server) readStemmed() (map[string][]stemmedTrack, error) {
	path := s.stemsFile()
	if path == "" {
		return map[string][]stemmedTrack{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]stemmedTrack{}, nil
		}
		return nil, err
	}
	var all map[string][]stemmedTrack
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	if all == nil {
		all = map[string][]stemmedTrack{}
	}
	return all, nil
}

func (s *Server) loadStemmed() map[string][]stemmedTrack {
	all, err := s.readStemmed()
	if err != nil {
		return map[string][]stemmedTrack{}
	}
	return all
}

func sameTrack(a, b providers.Track) bool { return a.Provider == b.Provider && a.ID == b.ID }

func sanitizeTrackCovers(t *providers.Track) bool {
	return providers.SanitizeSelfHostedTrack(t)
}

func scrubStemmedMap(all map[string][]stemmedTrack) bool {
	changed := false
	for scope, list := range all {
		for i := range list {
			if sanitizeTrackCovers(&list[i].Track) {
				changed = true
			}
		}
		all[scope] = list
	}
	return changed
}

// scrubLegacyStems strips any pre-fix credentials from an existing
// streaming-stems.json on startup without rewriting clean or undecodable files.
func (s *Server) scrubLegacyStems() {
	if s.stemsFile() == "" {
		return
	}
	s.stemsMu.Lock()
	defer s.stemsMu.Unlock()
	all, err := s.readStemmed()
	if err != nil {
		log.Printf("stream-sync: skip stems state scrub: could not decode state (%T)", err)
		return
	}
	if scrubStemmedMap(all) {
		if err := providers.SaveJSON(s.stemsFile(), all); err != nil {
			log.Printf("stream-sync: scrub stems state: %v", err)
		}
	}
}

// addStemmed puts t first in scope's list.
func (s *Server) addStemmed(scope string, t stemmedTrack) error {
	s.stemsMu.Lock()
	defer s.stemsMu.Unlock()
	sanitizeTrackCovers(&t.Track)
	all, err := s.readStemmed()
	if err != nil {
		log.Printf("stream-sync: skip stems state update: could not decode state (%T)", err)
		return errors.New("stems state could not be decoded")
	}
	scrubStemmedMap(all)
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
// stemsImportFile writes, and only on the deck's own file system (deckStorage):
// a file there is in the internal library, while a file on a drive is in that
// drive's library, which is not read here. When the library cannot be read
// nothing is removed.
func (s *Server) sweepStemsSources(ctx context.Context, root string) {
	dev, ok := deckStorage(root)
	if !ok {
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
		dir := filepath.Join(root, d.Name())
		if !d.IsDir() || !onDevice(dir, dev) {
			continue
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			p := filepath.Join(dir, f.Name())
			if !f.Type().IsRegular() || !onDevice(p, dev) {
				continue
			}
			if real, err := filepath.EvalSymlinks(p); !recorded[p] && (err != nil || !recorded[real]) {
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

// deckStorage tells whether dir is really on the deck's own file system, and
// gives that file system's device. A download directory whose path is under
// /data can be a link to a drive, or have a drive mounted on it: the files
// there are tracks in that drive's library, not in the internal one. So the
// path is resolved first and must still be under the data root, and it must be
// on the data root's device, which a mount is not. When either cannot be told
// the answer is no.
func deckStorage(dir string) (uint64, bool) {
	data, err := filepath.EvalSymlinks(shadowDataRoot)
	if err != nil {
		return 0, false
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return 0, false
	}
	if rel, err := filepath.Rel(data, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return 0, false
	}
	dev, ok := deviceOf(data)
	return dev, ok && onDevice(real, dev)
}

// deviceOf is the device of the file system p itself is on (a link is not followed).
func deviceOf(p string) (uint64, bool) {
	st, err := os.Lstat(p)
	if err != nil {
		return 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(sys.Dev), true
}

func onDevice(p string, dev uint64) bool {
	d, ok := deviceOf(p)
	return ok && d == dev
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
	return g.trackItems("", false, []providers.Track{g.stemsNotice(t)})[0]
}

// stemsNotice is the row shown for t until its Stems exist. Its id is the
// same every time, so the UI can be told which row that is (stemsSource) and
// draw the render's progress on it.
func (g *tidalGateway) stemsNotice(t providers.Track) providers.Track {
	title, hint := "Stems not ready", "Rendering can take as long as the track. Go back and reopen this list."
	g.s.uiMu.Lock()
	lang := g.s.loadUI().Language
	g.s.uiMu.Unlock()
	switch lang {
	case "zh-Hans":
		title, hint = "Stems 尚未完成", "分轨耗时可能与曲长相当，请稍后返回并重新打开此列表。"
	case "zh-Hant":
		title, hint = "Stems 尚未完成", "分軌耗時可能與曲長相當，請稍後返回並重新開啟此清單。"
	case "es":
		title, hint = "Stems aún no listos", "El renderizado puede tardar tanto como la pista. Vuelve atrás y abre de nuevo esta lista."
	}
	return providers.Track{Provider: listNoticeProvider, ID: "stems-" + t.Provider + "-" + t.ID, Title: title + " — " + t.Title, Artist: hint}
}

// providerHeader names the service a streamed track comes from. Engine tells the UI nothing per loaded deck but the source "TIDAL" (every
// service rides that slot), and the UI already asks stemsLocal for each streaming track a deck loads, so the answer carries the service:
// the deck header draws that service's own mark (#305). Status and body are unchanged.
// The spelling is the one Go puts on the wire, so the UI reads it back the same whatever its HTTP client does with case.
const providerHeader = "X-Enginex-Provider"

// stemsLocal answers whether a streamed row has a stemmed local copy.
func (g *tidalGateway) stemsLocal(w http.ResponseWriter, r *http.Request) {
	if _, ref, ok := g.lookup(w, r.URL.Query().Get("id")); ok {
		w.Header().Set(providerHeader, gatewayProvider(ref.track.Provider))
		if p := g.s.stemmedPath(ref.track); p != "" {
			respond(w, map[string]any{"path": p})
		} else {
			http.NotFound(w, r)
		}
	}
}
