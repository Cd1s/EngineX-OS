package webui

// The patched Engine sends its built-in TidalAPI requests to
// http://127.0.0.1:8080/v1/ and /au//. This gateway answers them with the
// playlists of the logged-in NetEase/Spotify/Deezer sessions, so the deck's
// native TIDAL browser plays them. Audio comes from the Deemix pipeline.

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/google/uuid"
)

const hintPlaylist = "00000000-0000-5000-8000-000000005000"

// The deck shows NetEase, Spotify and Deezer as separate sources. They share
// Engine's single TIDAL client, so the source the user last opened (the UI
// "scope") decides what this gateway answers. Loaded tracks keep working after
// a switch because track IDs are global.
// The deck shows these hints; they name its LAN address, which phones can reach (#218).
func hintTitle(scope string) string { return "请登录：" + lanHost() + ":8080" }

// sourceFailedTitle stands in for a logged-in source whose playlists failed,
// so the menu says why instead of being empty. #70: Spotify answers 403 for an
// account the app is not allowed to serve. Data, not UI text, so in both
// languages.
func sourceFailedTitle(provider string, err error) string {
	label := providerLabels[provider]
	if label == "" {
		label = provider
	}
	var st providers.StatusError
	if !errors.As(err, &st) {
		return label + ": failed to load / 加载失败"
	}
	switch st {
	case 401:
		return label + ": log in again (401) / 请重新登录"
	case 403:
		return label + ": account not allowed (403) / 账号未获授权"
	case 429:
		// A limit the provider already waited on and that still holds (#227).
		var limited *providers.RateLimited
		if errors.As(err, &limited) && limited.RetryAfter > time.Minute {
			m := int(math.Ceil(limited.RetryAfter.Minutes()))
			return fmt.Sprintf("%s: busy, try again in %d min / 繁忙，请 %d 分钟后再试", label, m, m)
		}
		return label + ": busy, try again in a minute / 繁忙，请一分钟后再试"
	}
	return fmt.Sprintf("%s: failed to load (HTTP %d) / 加载失败", label, int(st))
}

var (
	tidalNS        = uuid.MustParse("6f1c5a52-3c55-4c2e-9d0b-5c5000000000")
	providerLabels = map[string]string{"netease": "网易云音乐", "spotify": "Spotify", "deezer": "Deezer", "soundcloud": "SoundCloud", "beatport": "Beatport", localProvider: "USB/SD", "applemusic": "Apple Music"}
	providerOrder  = map[string]int{"netease": 0, "spotify": 1, "deezer": 2, "soundcloud": 3, "beatport": 4, "applemusic": 5}
)

type trackRef struct {
	track   providers.Track
	session string // empty for the built-in Deezer catalog
	guest   bool
	// file is a drive track's audio on its drive (OmniSource, #62), drive the volume.
	file, drive string
}
type playlistRef struct {
	session  string // empty for the built-in Deezer catalog
	provider string
	list     providers.Playlist
	guest    bool
}
type trackSearcher interface {
	Search(context.Context, string) ([]providers.Track, error)
}
type playlistSearcher interface {
	SearchPlaylists(context.Context, string) ([]providers.Playlist, error)
}
type tidalGateway struct {
	authMu    sync.Mutex
	refreshMu sync.Mutex // serializes rotating-token grants, including Engine requests
	browse    *browseCache
	searches  *searchCache
	s         *Server
	catalog   *providers.Deezer
	mux       *http.ServeMux
	mu        sync.Mutex
	ids       map[string]int64
	tracks    map[int64]trackRef
	lists     map[string]playlistRef
	next      int64

	// Artwork for list rows, by what the row knows: title and artist.
	artURLs  map[string]string
	artMu    sync.Mutex
	artCache map[string][]byte
	artAge   []string
	warming  atomic.Bool // a prewarmArt run is going

	// Last-use sequence numbers for evicting the least recently used refs.
	seq       int64
	trackSeen map[int64]int64
	listSeen  map[string]int64

	saving bool       // a save of the track IDs is scheduled
	saveMu sync.Mutex // keeps saves in order
}

// savedRef is a trackRef on disk. Engine keeps a listing's IDs while stream-sync
// restarts, so without them its next load is a 404, "Track not found" (#73).
type savedRef struct {
	ID      int64           `json:"id"`
	Track   providers.Track `json:"track"`
	Session string          `json:"session,omitempty"`
	Guest   bool            `json:"guest,omitempty"`
	File    string          `json:"file,omitempty"`
	Drive   string          `json:"drive,omitempty"`
}
type savedList struct {
	ID       string             `json:"id"`
	Session  string             `json:"session,omitempty"`
	Provider string             `json:"provider"`
	List     providers.Playlist `json:"list"`
	Guest    bool               `json:"guest,omitempty"`
}
type savedTracks struct {
	Next   int64       `json:"next"`
	Tracks []savedRef  `json:"tracks"` // least recently used first
	Lists  []savedList `json:"lists"`  // least recently used first
}

const trackSaveDelay = 3 * time.Second

// owner names a ref's login for its stable ID. An owner session gets a new
// random ID at every start, so it is named by its service; a guest by its session.
func owner(session string, guest bool) string {
	if session != "" && !guest {
		return "owner"
	}
	return session
}
func refKey(ref trackRef) string {
	return owner(ref.session, ref.guest) + "\x00" + ref.track.Provider + "\x00" + ref.track.ID
}

func (g *tidalGateway) tracksPath() string {
	if g.s.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(g.s.cfg.StateDir, "gateway-tracks.json")
}

// loadTracks restores the IDs of the previous run, and its counter, so an old ID
// never names a different track.
func (g *tidalGateway) loadTracks() {
	b, err := os.ReadFile(g.tracksPath())
	var v savedTracks
	if err != nil || json.Unmarshal(b, &v) != nil {
		return
	}
	for _, r := range v.Tracks {
		ref := trackRef{track: r.Track, session: r.Session, guest: r.Guest, file: r.File, drive: r.Drive}
		g.tracks[r.ID], g.ids[refKey(ref)] = ref, r.ID
		g.seq++
		g.trackSeen[r.ID] = g.seq
		g.next = max(g.next, r.ID)
	}
	g.next = max(g.next, v.Next)
	for _, l := range v.Lists {
		g.lists[l.ID] = playlistRef{l.Session, l.Provider, l.List, l.Guest}
		g.seq++
		g.listSeen[l.ID] = g.seq
	}
}

// scheduleSave must be called with g.mu held.
func (g *tidalGateway) scheduleSave() {
	if !g.saving && g.tracksPath() != "" {
		g.saving = true
		time.AfterFunc(trackSaveDelay, g.saveTracks)
	}
}

// saveTracks writes the track IDs. Run by a timer after changes, and by Close.
func (g *tidalGateway) saveTracks() {
	path := g.tracksPath()
	if path == "" {
		return
	}
	g.saveMu.Lock()
	defer g.saveMu.Unlock()
	g.mu.Lock()
	g.saving = false
	v := savedTracks{Next: g.next, Tracks: make([]savedRef, 0, len(g.tracks))}
	for id, ref := range g.tracks {
		v.Tracks = append(v.Tracks, savedRef{id, ref.track, ref.session, ref.guest, ref.file, ref.drive})
	}
	sort.Slice(v.Tracks, func(a, b int) bool { return g.trackSeen[v.Tracks[a].ID] < g.trackSeen[v.Tracks[b].ID] })
	for id, ref := range g.lists {
		v.Lists = append(v.Lists, savedList{id, ref.session, ref.provider, ref.list, ref.guest})
	}
	sort.Slice(v.Lists, func(a, b int) bool { return g.listSeen[v.Lists[a].ID] < g.listSeen[v.Lists[b].ID] })
	g.mu.Unlock()
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return // the state dir is gone (a test that ended)
	}
	if err := providers.SaveJSON(path, v); err != nil {
		log.Printf("stream-sync: save gateway track IDs: %v", err)
	}
}

// Browse state is bounded. When full, the least recently used tenth is
// dropped; IDs keep increasing so a stale ID never names a different track.
// The track bound holds two of the largest accepted playlists, so a listing
// never evicts its own tracks.
const (
	maxPlaylistTracks = 20000
	maxLists          = 10000
	maxTracks         = 2 * maxPlaylistTracks
)

// evict must be called with g.mu held.
func evict[K comparable](seen map[K]int64, limit int, drop func(K)) {
	if len(seen) <= limit {
		return
	}
	keys := make([]K, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool { return seen[keys[a]] < seen[keys[b]] })
	for _, k := range keys[:len(keys)-limit*9/10] {
		drop(k)
		delete(seen, k)
	}
}

func newTidalGateway(s *Server) *tidalGateway {
	g := &tidalGateway{browse: newBrowseCache(s.cfg.StateDir), artURLs: map[string]string{}, artCache: map[string][]byte{}, s: s, catalog: &providers.Deezer{BaseURL: s.cfg.DeezerURL, HTTP: s.cfg.HTTP}, mux: http.NewServeMux(), ids: map[string]int64{}, tracks: map[int64]trackRef{}, lists: map[string]playlistRef{}, trackSeen: map[int64]int64{}, listSeen: map[string]int64{}}
	g.searches = newSearchCache()
	g.seedFromStreamingAccounts()
	g.loadTracks()
	g.mux.HandleFunc("POST /au/oauth2/device_authorization", g.deviceAuthorization)
	g.mux.HandleFunc("POST /au/oauth2/token", g.token)
	g.mux.HandleFunc("POST /v1/enginex/login", s.loginStart)
	g.mux.HandleFunc("GET /v1/enginex/login", s.loginStatus)
	g.mux.HandleFunc("GET /v1/enginex/login/qr", s.loginQR)
	g.mux.HandleFunc("GET /v1/enginex/boot", s.bootGet)
	g.mux.HandleFunc("POST /v1/enginex/boot/ready", s.bootReady)
	g.mux.HandleFunc("GET /v1/enginex/ui", s.uiStatus)
	g.mux.HandleFunc("GET /v1/enginex/settings-copy", s.settingsCopyStatus)
	g.mux.HandleFunc("POST /v1/enginex/settings-copy", s.settingsCopyControl)
	g.mux.HandleFunc("POST /v1/enginex/settings-copy/prestart", s.settingsCopyPrestart)
	g.mux.HandleFunc("GET /v1/enginex/library-status", s.libraryStatusGet)
	g.mux.HandleFunc("GET /v1/enginex/update", s.update)
	g.mux.HandleFunc("POST /v1/enginex/ui", s.uiUpdate)
	g.mux.HandleFunc("GET /v1/enginex/views", s.viewsGet)
	g.mux.HandleFunc("POST /v1/enginex/views", s.viewsPost)
	g.mux.HandleFunc("POST /v1/enginex/relocate", s.relocate)
	g.mux.HandleFunc("GET /v1/enginex/bpm", s.bpmConfigGet)
	g.mux.HandleFunc("POST /v1/enginex/bpm", s.bpmConfigPost)
	g.mux.HandleFunc("GET /v1/enginex/art", g.art)
	g.mux.HandleFunc("GET /v1/enginex/hotcues", s.handleHotCues)
	g.mux.HandleFunc("POST /v1/enginex/hotcues", s.handleHotCues)
	g.mux.HandleFunc("GET /v1/enginex/bluetooth", s.handleBluetooth)
	g.mux.HandleFunc("POST /v1/enginex/bluetooth", s.handleBluetooth)
	g.mux.HandleFunc("GET /v1/enginex/nas", s.handleNASConfig)
	g.mux.HandleFunc("POST /v1/enginex/nas", s.handleNASConfig)
	g.mux.HandleFunc("POST /v1/enginex/nas/test", s.handleNASTest)
	g.mux.HandleFunc("GET /v1/enginex/mods", s.handleMods)
	g.mux.HandleFunc("POST /v1/enginex/mods", s.handleMods)
	g.mux.HandleFunc("POST /v1/enginex/mods/upload", s.handleModsUpload)
	g.mux.HandleFunc("GET /v1/enginex/ssh", s.handleSSH)
	g.mux.HandleFunc("POST /v1/enginex/ssh", s.handleSSH)
	// Every /v1/ path reaches this mux only (Server.ServeHTTP): a route
	// registered on s.mux alone answered with emptyPage (BH-GC2-3).
	g.mux.HandleFunc("GET /v1/enginex/unlock-gen2", s.handleUnlockGen2)
	g.mux.HandleFunc("POST /v1/enginex/unlock-gen2", s.handleUnlockGen2)
	g.mux.HandleFunc("POST /v1/enginex/uninstall-gen2", s.handleUninstallGen2)
	g.mux.HandleFunc("GET /v1/enginex/tailscale", s.handleTailscale)
	g.mux.HandleFunc("POST /v1/enginex/tailscale", s.handleTailscale)
	g.mux.HandleFunc("GET /v1/enginex/mounts", s.handleMounts)
	g.mux.HandleFunc("POST /v1/enginex/mounts", s.handleMounts)
	g.mux.HandleFunc("DELETE /v1/enginex/mounts", s.handleMounts)
	g.mux.HandleFunc("DELETE /v1/enginex/mounts/{id}", s.handleMounts)
	g.mux.HandleFunc("GET /v1/enginex/automix", s.handleAutomix)
	g.mux.HandleFunc("POST /v1/enginex/automix", s.handleAutomix)
	g.mux.HandleFunc("POST /v1/enginex/sampler/loop-to-pad", s.handleSamplerLoopToPad)
	g.mux.HandleFunc("GET /v1/enginex/usb-audio", s.handleUSBAudio)
	g.mux.HandleFunc("POST /v1/enginex/usb-audio", s.handleUSBAudio)
	g.mux.HandleFunc("GET /v1/enginex/stems/source", g.stemsSource)
	g.mux.HandleFunc("GET /v1/enginex/stems/local", g.stemsLocal)
	g.mux.HandleFunc("GET /v1/enginex/stems/track-id", g.stemsTrackID)
	g.mux.HandleFunc("GET /v1/enginex/relay", s.relayConfigGet)
	g.mux.HandleFunc("POST /v1/enginex/relay", s.relayConfigPost)
	g.mux.HandleFunc("GET /v1/enginex/prolink", s.prolinkGet)
	g.mux.HandleFunc("GET /v1/enginex/prolink/diag", s.prolinkDiagGet)
	g.mux.HandleFunc("POST /v1/enginex/prolink", s.prolinkPost)
	g.mux.HandleFunc("POST /v1/enginex/decks", s.decksPost)
	g.mux.HandleFunc("POST /v1/enginex/decks/alive", s.decksAlive)
	g.mux.HandleFunc("GET /v1/enginex/clock", s.deckClockGet)
	g.mux.HandleFunc("POST /v1/enginex/clock", s.deckClockPost)
	g.mux.HandleFunc("GET /v1/enginex/shadow", s.shadowGet)
	g.mux.HandleFunc("GET /v1/enginex/shadow/display", s.shadowDisplayGet)
	g.mux.HandleFunc("GET /v1/enginex/shadow/waveform.png", s.shadowWaveformGet)
	g.mux.HandleFunc("GET /v1/enginex/shadow/artwork", s.shadowArtworkGet)
	g.mux.HandleFunc("GET /v1/enginex/debug/shadow", s.shadowDebugGet)
	g.mux.HandleFunc("POST /v1/enginex/debug/shadow", s.shadowDebugPost)
	g.mux.HandleFunc("GET /v1/enginex/broadcast", s.broadcastGet)
	g.mux.HandleFunc("POST /v1/enginex/broadcast", s.broadcastPost)
	g.mux.HandleFunc("POST /v1/enginex/broadcast/metadata", s.broadcastMetadataPost)
	g.mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("OK")) })
	g.mux.HandleFunc("GET /v1/sessions", g.session)
	g.mux.HandleFunc("GET /v1/users/{id}/subscription", g.subscription)
	g.mux.HandleFunc("GET /v1/users/{id}/playlists", g.playlists)
	g.mux.HandleFunc("GET /v1/users/{id}/favorites/playlists", g.playlists)
	g.mux.HandleFunc("GET /v1/users/{id}/favorites/tracks", g.favoriteTracks)
	g.mux.HandleFunc("GET /v1/users/{id}/favorites/albums", g.favoriteAlbums)
	g.mux.HandleFunc("GET /v1/albums/{id}/tracks", g.albumTracks)
	g.mux.HandleFunc("GET /v1/albums/{id}/items", g.albumTracks)
	g.mux.HandleFunc("GET /v1/playlists/{uuid}", g.playlist)
	g.mux.HandleFunc("GET /v1/playlists/{uuid}/tracks", g.playlistTracks)
	g.mux.HandleFunc("GET /v1/playlists/{uuid}/items", g.playlistTracks)
	g.mux.HandleFunc("GET /v1/search", g.search)
	g.mux.HandleFunc("GET /v1/tracks/{id}", g.trackInfo)
	g.mux.HandleFunc("GET /v1/tracks/{id}/playbackinfopostpaywall", g.playbackInfo)
	g.mux.HandleFunc("GET /v1/tracks/{id}/streamurl", g.streamURL)
	g.mux.HandleFunc("GET /v1/stream/{file}", g.stream)
	// Engine reads a category list as a JSON array and fails on anything else.
	for _, category := range []string{"featured", "genres", "moods", "rising"} {
		g.mux.HandleFunc("GET /v1/"+category, func(w http.ResponseWriter, r *http.Request) { respond(w, []any{}) })
	}
	g.mux.HandleFunc("/v1/", g.emptyPage)
	return g
}

// emptyPage answers what this gateway has no content for (albums, artists,
// category items). Engine reports any body it cannot parse as "Failed to parse
// server response", which interrupts the browser, so every other /v1 path is a
// valid empty listing.
func (g *tidalGateway) emptyPage(w http.ResponseWriter, r *http.Request) {
	log.Printf("stream-sync: gateway has no content for %s %s", r.Method, r.URL.Path)
	respond(w, page(r, 100, []any{}))
}

// proxyToTidal passes a request on untouched when the deck is showing TIDAL itself,
// so Engine talks to TIDAL's own servers with its own login and playback.
func (g *tidalGateway) proxyToTidal(w http.ResponseWriter, r *http.Request, clean string) {
	target := g.s.cfg.TidalAPIURL
	if target == "" {
		target = "https://api.tidal.com"
	}
	upstream := clean
	if rest, ok := strings.CutPrefix(clean, "/au"); ok {
		target = g.s.cfg.TidalAuthURL
		if target == "" {
			target = "https://auth.tidal.com"
		}
		upstream = "/v1" + rest
	}
	if strings.HasSuffix(r.URL.Path, "/") {
		upstream += "/"
	}
	base, err := url.Parse(target)
	if err != nil {
		fail(w, 500, errors.New("invalid upstream"))
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(out *httputil.ProxyRequest) {
			out.SetURL(base)
			out.Out.URL.Path, out.Out.URL.RawPath = upstream, ""
			out.Out.Host = base.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			if strings.HasSuffix(clean, "/oauth2/token") {
				return g.tokenResponse(resp, r.Form.Get("refresh_token"), r.Form.Get("client_id"))
			}
			// Retry an expired bearer once. Token grants use the original form
			// path above, preserving Engine's refresh-token error response.
			if resp.StatusCode == http.StatusUnauthorized && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				a, err := g.refreshRealAuth(r.Context(), strings.TrimPrefix(resp.Request.Header.Get("Authorization"), "Bearer "))
				if err != nil {
					return nil
				}
				retry := resp.Request.Clone(r.Context())
				retry.Header.Set("Authorization", "Bearer "+a.AccessToken)
				next, err := http.DefaultTransport.RoundTrip(retry)
				if err != nil {
					return errors.New("could not retry TIDAL request")
				}
				resp.Body.Close()
				*resp = *next
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Print("stream-sync: TIDAL upstream request failed")
			fail(w, 502, errors.New("could not reach TIDAL"))
		},
	}
	http.NewResponseController(w).SetWriteDeadline(time.Time{})
	proxy.ServeHTTP(w, r)
}

// labelWithBPM carries tempo and key to each list row. Engine's TIDAL client reads
// no tempo field, and it copies a list row's title to the deck when a track loads,
// so they travel in the copyright field, which it only keeps for the list. The track
// list QML moves them to its BPM and Key columns. Display text only: this reaches
// neither the beat grid nor the library.
func labelWithBPM(t providers.Track) string {
	label := providerLabels[t.Provider]
	if t.BPM <= 0 {
		return label
	}
	tempo := strconv.Itoa(int(math.Round(t.BPM))) + " BPM"
	if t.Key != "" {
		tempo += " " + t.Key
	}
	return label + " [" + tempo + "]"
}

// tidalNumericID hashes a name to an ID Engine can read: it parses them as signed
// 32-bit integers.
func tidalNumericID(name string) uint32 { return crc32.ChecksumIEEE([]byte(name)) & 0x7fffffff }

func isTidal(p string) bool { return strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/au/") }

// canonicalV1 maps /v1/sc5000/..., the deck API's name before 5.3.0.5 (#234), onto the
// /v1/enginex/... routes. Both prefixes reach the same handlers behind the same rules (the
// loopback check above runs first); /v1/sc5000/ stays for good, since installers, UI packs,
// Mods and users' scripts of any release call it. One mapping here, not a second copy of
// each route.
func canonicalV1(p string) string {
	if rest, ok := strings.CutPrefix(p, "/v1/sc5000"); ok && (rest == "" || rest[0] == '/') {
		return "/v1/enginex" + rest
	}
	return p
}

// relayedFromOutside reports a request a tunnel on the deck carried in from elsewhere: it
// arrives from loopback, but with headers the tunnel's edge always adds and a caller cannot
// remove. Engine and the deck's own tools never send them.
func relayedFromOutside(r *http.Request) bool {
	for _, h := range []string{"Cf-Connecting-Ip", "Cf-Ray", "Cdn-Loop", "Tailscale-User-Login"} {
		if _, ok := r.Header[h]; ok {
			return true
		}
	}
	return false
}

// ServeHTTP folds the "/au//" and "/v1//" joins produced by the patched base
// URLs. The gateway has no console login, so only the deck itself may use it.
func (g *tidalGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() || relayedFromOutside(r) {
		fail(w, 403, errors.New("the streaming gateway is local to the deck"))
		return
	}
	clean := canonicalV1(path.Clean(r.URL.Path))
	if !isTidal(clean + "/") {
		http.NotFound(w, r)
		return
	}
	// An unpaired TIDAL browser must not turn a known gateway row into an
	// authentication failure. Paired TIDAL keeps its own numeric ID namespace.
	localTrack := false
	if rest, ok := strings.CutPrefix(clean, "/v1/tracks/"); ok && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		raw, endpoint, _ := strings.Cut(rest, "/")
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && (endpoint == "" || endpoint == "playbackinfopostpaywall" || endpoint == "streamurl") {
			g.mu.Lock()
			_, localTrack = g.tracks[id]
			g.mu.Unlock()
			log.Printf("stream-sync: load track=%d scope=%s step=request local=%t", id, g.s.scope(), localTrack)
		}
	}
	if !strings.HasPrefix(clean, "/v1/enginex/") && !strings.HasPrefix(clean, "/v1/stream/") {
		// Clone before changing headers or form bodies used by Engine.
		r = r.Clone(r.Context())
		form, err := g.captureAuth(r, clean)
		if err != nil {
			fail(w, 500, err)
			return
		}
		g.s.uiMu.Lock()
		v := g.s.loadUI()
		g.s.uiMu.Unlock()
		if v.Scope == "tidal" && slices.Contains(v.Sources, "tidal") && (!localTrack || g.hasRealAuth()) {
			// Only real TIDAL grants rotate tokens. Keep synthetic logins independent.
			if r.Method == http.MethodPost && strings.HasSuffix(clean, "/oauth2/token") {
				g.refreshMu.Lock()
				defer g.refreshMu.Unlock()
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				defer cancel()
				r = r.WithContext(ctx)
			}
			if !g.prepareRealAuth(w, r, clean, form) {
				return
			}
			r.Form = form
			g.proxyToTidal(w, r, clean)
			return
		}
	}
	u := *r.URL
	u.Path, u.RawPath = clean, ""
	r2 := r.Clone(r.Context())
	r2.URL = &u
	g.mux.ServeHTTP(w, r2)
}
func (g *tidalGateway) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]any{"deviceCode": "enginex-local", "userCode": "ENGINEX", "verificationUri": lanHost() + ":8080", "verificationUriComplete": "http://" + lanHost() + ":8080", "expiresIn": 600, "interval": 1})
}
func (g *tidalGateway) token(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]any{"access_token": "enginex-token", "refresh_token": "enginex-refresh", "token_type": "Bearer", "expires_in": 86400, "user": map[string]any{"userId": 1, "countryCode": "US"}})
}
func (g *tidalGateway) session(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]any{"sessionId": "enginex-session", "userId": 1, "countryCode": "US"})
}
func (g *tidalGateway) subscription(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]any{"status": "ACTIVE", "subscription": map[string]any{"type": "PREMIUM_PLUS"}, "highestSoundQuality": "LOSSLESS"})
}

// sources returns ready sessions, restoring persisted owner logins after a
// restart so the deck works without opening the phone console first.
func (g *tidalGateway) sources() []*Session {
	return g.sourcesFor("")
}

// A search only needs the selected service. Do not restore unrelated accounts
// (or their credentials) on its critical path.
func (g *tidalGateway) sourcesFor(scope string) []*Session {
	s := g.s
	// Restoring a login can refresh tokens over the network (ready), so it runs
	// outside s.mu; only the session map is touched under the lock.
	s.prune()
	s.mu.Lock()
	var restore []string
	for _, name := range loginServices {
		if scope != "" && name != scope {
			continue
		}
		owned := false
		for _, v := range s.sessions {
			owned = owned || (!v.Guest && v.Provider == name)
		}
		if !(owned || s.ownerDeleted[name]) {
			restore = append(restore, name)
		}
	}
	s.mu.Unlock()
	type restored struct {
		name string
		p    providers.Provider
	}
	var found []restored
	for _, name := range restore {
		if p, err := s.makeProvider(name, false, ""); err == nil && ready(p) {
			found = append(found, restored{name, p})
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range found {
		owned := s.ownerDeleted[f.name]
		for _, v := range s.sessions {
			owned = owned || (!v.Guest && v.Provider == f.name)
		}
		if owned || len(s.sessions) >= 16 {
			continue
		}
		id := providers.RandomID()
		s.sessions[id] = &Session{ID: id, Provider: f.name, Ready: true, p: f.p, tracks: map[string]providers.Track{}, expires: time.Now().Add(8 * time.Hour)}
	}
	var out []*Session
	for _, v := range s.sessions {
		if v.Ready && (scope == "" || v.Provider == scope) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Provider != b.Provider {
			return providerOrder[a.Provider] < providerOrder[b.Provider]
		}
		if a.Guest != b.Guest {
			return !a.Guest
		}
		return a.ID < b.ID
	})
	return out
}
func ready(p providers.Provider) bool {
	switch v := p.(type) {
	case *providers.Spotify:
		return v.HasToken() || v.HasStoredCredentials() || v.HasLinks()
	case *providers.Netease:
		return v.LoggedIn()
	case *providers.Deezer:
		return v.UserID != ""
	case *providers.SoundCloud:
		// Searching needs no account, so only a bound profile or a user's token is a
		// login: until then the deck still offers the QR code.
		return v.UserID != 0 || v.Token != ""
	case *providers.Beatport:
		return v.Ready()
	case *providers.AppleMusic:
		return v.Ready()
	}
	return false
}
func (g *tidalGateway) sessionByID(id string) *Session {
	g.s.prune()
	g.s.mu.Lock()
	defer g.s.mu.Unlock()
	return g.s.sessions[id]
}

// each runs fn for every input concurrently and keeps the successful results in
// input order. One provider failing must not empty the deck's browser.
func each[T, R any](ctx context.Context, in []T, fn func(context.Context, T) ([]R, error)) [][]R {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out := make([][]R, len(in))
	var wg sync.WaitGroup
	for i, v := range in {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("gateway: provider panic: %v", r)
				}
			}()
			r, err := fn(ctx, v)
			if err != nil {
				log.Printf("stream-sync: gateway source failed: %v", err)
				return
			}
			out[i] = r
		}()
	}
	wg.Wait()
	return out
}
func (g *tidalGateway) addPlaylist(ref playlistRef) string {
	id := uuid.NewSHA1(tidalNS, []byte(owner(ref.session, ref.guest)+"\x00"+ref.provider+"\x00"+ref.list.ID)).String()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.scheduleSave()
	g.lists[id] = ref
	g.seq++
	g.listSeen[id] = g.seq
	evict(g.listSeen, maxLists, func(id string) { delete(g.lists, id) })
	return id
}
func (g *tidalGateway) list(id string) (playlistRef, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ref, ok := g.lists[id]
	if ok {
		g.seq++
		g.listSeen[id] = g.seq
	}
	return ref, ok
}

// addTrack assigns stable positive IDs per source track, kept across restarts.
func (g *tidalGateway) addTrack(ref trackRef) int64 {
	httpsCover(&ref.track)
	key := refKey(ref)
	g.mu.Lock()
	defer g.mu.Unlock()
	id := g.ids[key]
	if id == 0 {
		g.next++
		id = g.next
		g.ids[key] = id
	}
	g.scheduleSave()
	g.tracks[id] = ref
	if address := thumbURL(ref.track); address != "" {
		if len(g.artURLs) > maxTracks {
			g.artURLs = map[string]string{}
		}
		g.artURLs[artKey(ref.track.Title, ref.track.Artist)] = address
		g.artURLs[artKey(ref.track.Title, "")] = address
	}
	g.seq++
	g.trackSeen[id] = g.seq
	evict(g.trackSeen, maxTracks, func(id int64) {
		r := g.tracks[id]
		delete(g.ids, refKey(r))
		delete(g.tracks, id)
	})
	return id
}
func artKey(title, artist string) string { return title + "\x00" + artist }

// thumbURL asks each service for its small size, so a list stays quick and light.
func thumbURL(t providers.Track) string {
	if t.ThumbURL != "" {
		return t.ThumbURL
	}
	address := strings.Replace(t.CoverURL, "?param=500y500", "?param=120y120", 1)
	address = strings.Replace(address, "/500x500-", "/120x120-", 1)
	return strings.Replace(address, "-t500x500.", "-t120x120.", 1)
}

const (
	maxArtEntries = 128
	maxArtBytes   = 512 << 10
)

// art serves a list row's thumbnail, keeping the most recently fetched in memory.
func (g *tidalGateway) art(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	g.mu.Lock()
	address := g.artURLs[artKey(q.Get("song"), q.Get("artist"))]
	if address == "" {
		address = g.artURLs[artKey(q.Get("song"), "")]
	}
	g.mu.Unlock()
	data := g.artBytes(r.Context(), address)
	if data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Write(data)
}

// artClient pools connections: a list opens a dozen covers from one CDN at once.
var artClient = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 8
	return &http.Client{Timeout: 6 * time.Second, Transport: t}
}()

// artBytes is the cover at address (https only), from memory or fetched; nil on any failure.
func (g *tidalGateway) artBytes(ctx context.Context, address string) []byte {
	if !strings.HasPrefix(address, "https://") {
		return nil
	}
	g.artMu.Lock()
	data := g.artCache[address]
	g.artMu.Unlock()
	if data != nil {
		return data
	}
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil
	}
	resp, err := artClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxArtBytes+1))
	if err != nil || resp.StatusCode != http.StatusOK || len(data) > maxArtBytes {
		return nil
	}
	g.artMu.Lock()
	defer g.artMu.Unlock()
	if len(g.artAge) >= maxArtEntries {
		delete(g.artCache, g.artAge[0])
		g.artAge = append(g.artAge[:0], g.artAge[1:]...) // keep the backing array bounded
	}
	if _, ok := g.artCache[address]; !ok {
		g.artCache[address] = data
		g.artAge = append(g.artAge, address)
	}
	return data
}

// prewarmArt fetches the first covers of a listing in the background so the rows
// and the jog wheel find them cached. One run at a time: a busy deck skips it.
func (g *tidalGateway) prewarmArt(refs []trackRef) {
	if !g.warming.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer g.warming.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, ref := range refs[:min(len(refs), 12)] {
			g.artBytes(ctx, thumbURL(ref.track))
		}
	}()
}

// syncStmAlbumArt gives the streaming row Engine inserted into stm.db a real
// cover. Engine writes it as hash "image://streaming/" with no image, never reads
// the file's embedded picture for a row that already has an AlbumArt id, and so the
// jog wheel (GET_ALBUM_ART) shows nothing. Writing Engine's live database is how
// m.db got corrupted before (sc5000-shadow-decks), so this stays one UPDATE on a
// PERSIST-journal connection: the default DELETE mode unlinks the stm.db-journal
// Engine keeps open. Only a still-empty row is touched.
func (g *tidalGateway) syncStmAlbumArt(ctx context.Context, id int64, t providers.Track) {
	address := t.CoverURL
	if address == "" {
		address = thumbURL(t)
	}
	data := g.artBytes(ctx, address)
	if len(data) < 3 || data[0] != 0xff || data[1] != 0xd8 { // Engine's Artwork files are JPEG
		return
	}
	sum := sha1.Sum(data)
	lib := filepath.Join(shadowDataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	stm := filepath.Join(lib, "Database2", "stm.db")
	if _, err := os.Stat(stm); err != nil {
		return
	}
	file := filepath.Join(lib, "Artwork", base64.RawURLEncoding.EncodeToString(sum[:])+".jpg")
	if _, err := os.Stat(file); err != nil {
		if os.MkdirAll(filepath.Dir(file), 0o755) != nil {
			return
		}
		tmp := file + ".part"
		if os.WriteFile(tmp, data, 0o644) != nil || os.Rename(tmp, file) != nil {
			os.Remove(tmp)
			return
		}
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: stm}).String()+"?_pragma=busy_timeout(2000)&_pragma=journal_mode(persist)")
	if err != nil {
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	res, err := db.ExecContext(ctx, `UPDATE AlbumArt SET hash = ?, albumArt = ? WHERE albumArt IS NULL AND id = (SELECT albumArtId FROM Track WHERE uri = ? ORDER BY id DESC LIMIT 1)`, sum[:], data, fmt.Sprintf("streaming://TIDAL/Track/%d", id))
	if err != nil {
		log.Printf("stream-sync: stm.db cover for track %d: %v", id, err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("stream-sync: stm.db cover set for track %d", id)
	}
}

// httpsCover upgrades http:// cover links (NetEase serves them on both): the
// cover embed and the thumbnail proxy accept https only.
func httpsCover(t *providers.Track) {
	for _, p := range []*string{&t.CoverURL, &t.ThumbURL} {
		if rest, ok := strings.CutPrefix(*p, "http://"); ok {
			*p = "https://" + rest
		}
	}
}

func tidalTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000-0700") }
func playlistJSON(id, title, description string, count int) map[string]any {
	now := tidalTime(time.Now())
	return map[string]any{"uuid": id, "title": title, "numberOfTracks": count, "numberOfVideos": 0, "description": description, "duration": 0, "lastUpdated": now, "created": now, "type": "USER", "publicPlaylist": false, "url": "http://sc5000.lan:8080", "image": "", "squareImage": "", "popularity": 0}
}
func (g *tidalGateway) refJSON(id string, ref playlistRef) map[string]any {
	label := providerLabels[ref.provider]
	return playlistJSON(id, ref.list.Title, label+" 歌单，经 EngineX OS 下载播放", ref.list.Count)
}
func page(r *http.Request, def int, items []any) map[string]any {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = def
	}
	offset = max(0, min(offset, len(items)))
	end := min(len(items), offset+min(limit, 10000))
	return map[string]any{"limit": limit, "offset": offset, "totalNumberOfItems": len(items), "items": items[offset:end]}
}
func (g *tidalGateway) playlists(w http.ResponseWriter, r *http.Request) {
	scope := g.s.scope()
	sessions := []*Session{}
	for _, v := range g.sources() {
		if v.Provider == scope {
			sessions = append(sessions, v)
		}
	}
	var failedMu sync.Mutex
	failed := map[*Session]error{}
	lists := each(r.Context(), sessions, func(ctx context.Context, v *Session) ([]providers.Playlist, error) {
		l, err := g.browseLists(ctx, v)
		if err != nil {
			failedMu.Lock()
			failed[v] = err
			failedMu.Unlock()
		}
		return l, err
	})
	favorites := strings.HasSuffix(r.URL.Path, "/favorites/playlists")
	items := []any{}
	add := func(v map[string]any) {
		if favorites {
			// Favourites wrap the playlist in "item"; flat fields also satisfy flat parsers.
			flat := map[string]any{"item": v}
			for k, f := range v {
				flat[k] = f
			}
			v = flat
		}
		items = append(items, v)
	}
	if n := len(g.s.stemmed(scope)); n > 0 {
		add(playlistJSON(stemsListID(scope), "Stems", "", n))
	}
	for i, v := range sessions {
		if err := failed[v]; err != nil {
			add(playlistJSON(hintPlaylist, sourceFailedTitle(v.Provider, err), err.Error(), 0))
		}
		for _, p := range lists[i] {
			ref := playlistRef{v.ID, v.Provider, p, v.Guest}
			add(g.refJSON(g.addPlaylist(ref), ref))
		}
	}
	// Deezer's public charts need no login; the others explain how to log in.
	if scope == "deezer" {
		defaults := each(r.Context(), []*providers.Deezer{g.catalog}, func(ctx context.Context, d *providers.Deezer) ([]providers.Playlist, error) {
			data, err := g.browse.get(ctx, browseKey("deezer", "public", "charts", ""), func(ctx context.Context) (browseData, error) {
				lists, err := d.ChartPlaylists(ctx)
				return browseData{Lists: lists}, err
			})
			return data.Lists, err
		})
		for _, p := range defaults[0] {
			ref := playlistRef{"", "deezer", p, false}
			add(g.refJSON(g.addPlaylist(ref), ref))
		}
	}
	if g.s.omniSource() && !favorites {
		g.omniPlaylists(r.Context(), scope, add)
	}
	if scope != "deezer" && len(sessions) == 0 {
		add(playlistJSON(hintPlaylist, hintTitle(scope), "手机打开 http://"+lanHost()+":8080 登录后，歌单会出现在这里", 0))
	}
	respond(w, page(r, 100, items))
}
func (g *tidalGateway) playlist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("uuid")
	if id == hintPlaylist {
		respond(w, playlistJSON(hintPlaylist, hintTitle(g.s.scope()), "", 0))
		return
	}
	if scope := g.s.scope(); id == stemsListID(scope) {
		respond(w, playlistJSON(id, "Stems", "", len(g.s.stemmed(scope))))
		return
	}
	ref, ok := g.list(id)
	if !ok {
		fail(w, 404, errors.New("playlist not found"))
		return
	}
	respond(w, g.refJSON(id, ref))
}
func (g *tidalGateway) trackJSON(id int64, t providers.Track) map[string]any {
	names := t.Artists
	if len(names) == 0 && t.Artist != "" {
		names = []string{t.Artist}
	}
	artists := []any{}
	for i, name := range names {
		kind := "FEATURED"
		if i == 0 {
			kind = "MAIN"
		}
		artists = append(artists, map[string]any{"id": tidalNumericID(name), "name": name, "type": kind, "picture": nil})
	}
	var artist any
	if len(artists) > 0 {
		artist = artists[0]
	}
	return map[string]any{"id": id, "title": t.Title, "bpm": int(math.Round(t.BPM)), "duration": t.DurationMS / 1000, "replayGain": 0, "peak": 1, "allowStreaming": true, "streamReady": true, "premiumStreamingOnly": false, "trackNumber": 1, "volumeNumber": 1, "version": nil, "popularity": 0, "copyright": labelWithBPM(t), "url": "http://sc5000.lan:8080", "isrc": t.ISRC, "editable": false, "explicit": false, "audioQuality": "LOSSLESS", "audioModes": []string{"STEREO"}, "artist": artist, "artists": artists, "album": map[string]any{"id": tidalNumericID(t.Album), "title": t.Album, "cover": nil, "vibrantColor": nil, "videoCover": nil}, "mixes": map[string]any{}}
}

// trackItems wraps each track as {"item":…,"type":"track"} (playlist items and
// favourites) and repeats its fields flat, as plain /tracks listings return them.
func (g *tidalGateway) trackItems(session string, guest bool, tracks []providers.Track) []any {
	refs := make([]trackRef, 0, len(tracks))
	for _, t := range tracks {
		refs = append(refs, trackRef{track: t, session: session, guest: guest})
	}
	return g.refItems(refs)
}
func (g *tidalGateway) refItems(refs []trackRef) []any {
	items := make([]any, 0, len(refs))
	for _, ref := range refs {
		v := g.trackJSON(g.addTrack(ref), ref.track)
		flat := map[string]any{"item": v, "type": "track"}
		for k, f := range v {
			flat[k] = f
		}
		items = append(items, flat)
	}
	g.prewarmArt(refs)
	return items
}
func camelotRank(k string) (int, bool) {
	n, m, ok := enginedb.Camelot(k)
	if !ok || n < 1 || n > 12 {
		return -1, false
	}
	offset := 0
	if m == 'B' {
		offset = 1
	}
	return (n-1)*2 + offset, true
}

func sortStreamingTracks(refs []trackRef, role, order string) {
	role = strings.ToLower(strings.TrimSpace(role))
	order = strings.ToLower(strings.TrimSpace(order))
	desc := order == "desc"
	switch role {
	case "bpm":
		sort.SliceStable(refs, func(i, j int) bool {
			a, b := refs[i].track.BPM, refs[j].track.BPM
			validA, validB := a > 0, b > 0
			if !validA && !validB {
				return false
			}
			if !validA {
				return false
			}
			if !validB {
				return true
			}
			if desc {
				return a > b
			}
			return a < b
		})
	case "key":
		sort.SliceStable(refs, func(i, j int) bool {
			rankA, okA := camelotRank(refs[i].track.Key)
			rankB, okB := camelotRank(refs[j].track.Key)
			if !okA && !okB {
				return false
			}
			if !okA {
				return false
			}
			if !okB {
				return true
			}
			if desc {
				return rankA > rankB
			}
			return rankA < rankB
		})
	case "rating":
		sort.SliceStable(refs, func(i, j int) bool {
			a, b := refs[i].track.Rating, refs[j].track.Rating
			if a == b {
				return strings.ToLower(refs[i].track.Title) < strings.ToLower(refs[j].track.Title)
			}
			if desc {
				return a > b
			}
			return a < b
		})
	case "length", "duration":
		sort.SliceStable(refs, func(i, j int) bool {
			a, b := refs[i].track.DurationMS, refs[j].track.DurationMS
			if desc {
				return a > b
			}
			return a < b
		})
	case "song", "title":
		sort.SliceStable(refs, func(i, j int) bool {
			a, b := strings.ToLower(refs[i].track.Title), strings.ToLower(refs[j].track.Title)
			if desc {
				return a > b
			}
			return a < b
		})
	case "artist":
		sort.SliceStable(refs, func(i, j int) bool {
			a, b := strings.ToLower(refs[i].track.Artist), strings.ToLower(refs[j].track.Artist)
			if desc {
				return a > b
			}
			return a < b
		})
	}
}

// ratingCacheTTL bounds how stale a rating may be; playlistTracks runs on every
// browse, and the two libraries are scanned in full (all rated rows), so the
// result is reused for a short while. Tests set it to 0.
var ratingCacheTTL = 30 * time.Second

var ratingCache struct {
	sync.Mutex
	dir          string
	at           time.Time
	ids, byNames map[string]int
}

// loadRatings reads every rated row of stm.db (by origin id and uri, then by
// title+artist) and m.db (by title+artist). ok is false when a library could
// not be read to the end, so the partial result is not cached.
func loadRatings(ctx context.Context, libDir string) (idRatings, nameRatings map[string]int, ok bool) {
	idRatings = make(map[string]int)
	nameRatings = make(map[string]int)
	ok = true

	readDB := func(dbName string, isStm bool) {
		p := filepath.Join(libDir, dbName)
		if _, err := os.Stat(p); err != nil {
			return
		}
		db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
		if err != nil {
			ok = false
			return
		}
		defer db.Close()

		if isStm {
			rows, err := db.QueryContext(ctx, `SELECT COALESCE(originTrackId, 0), COALESCE(uri, ''), COALESCE(title, ''), COALESCE(artist, ''), rating FROM Track WHERE rating > 0`)
			if err != nil {
				ok = false
			} else {
				defer rows.Close()
				for rows.Next() {
					var origID int64
					var uri, title, artist string
					var rating int
					if err := rows.Scan(&origID, &uri, &title, &artist, &rating); err == nil && rating > 0 {
						if origID > 0 {
							idRatings[strconv.FormatInt(origID, 10)] = rating
						}
						if uri != "" {
							idRatings[uri] = rating
						}
						key := strings.ToLower(strings.TrimSpace(title)) + "\x00" + strings.ToLower(strings.TrimSpace(artist))
						nameRatings[key] = rating
					}
				}
				if rows.Err() != nil {
					ok = false
				}
			}
		} else {
			rows, err := db.QueryContext(ctx, `SELECT title, artist, rating FROM Track WHERE rating > 0`)
			if err != nil {
				ok = false
			} else {
				defer rows.Close()
				for rows.Next() {
					var title, artist string
					var rating int
					if err := rows.Scan(&title, &artist, &rating); err == nil && rating > 0 {
						key := strings.ToLower(strings.TrimSpace(title)) + "\x00" + strings.ToLower(strings.TrimSpace(artist))
						// A streaming track's own rating (stm.db) wins over a local file of the same name.
						if _, has := nameRatings[key]; !has {
							nameRatings[key] = rating
						}
					}
				}
				if rows.Err() != nil {
					ok = false
				}
			}
		}
	}

	readDB("stm.db", true)
	readDB("m.db", false)
	return idRatings, nameRatings, ok
}

func cachedRatings(ctx context.Context) (map[string]int, map[string]int) {
	libDir := filepath.Join(shadowDataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library/Database2")
	ratingCache.Lock()
	defer ratingCache.Unlock()
	if ratingCache.dir == libDir && time.Since(ratingCache.at) < ratingCacheTTL {
		return ratingCache.ids, ratingCache.byNames
	}
	ids, names, ok := loadRatings(ctx, libDir)
	if ok {
		ratingCache.dir, ratingCache.at, ratingCache.ids, ratingCache.byNames = libDir, time.Now(), ids, names
	}
	return ids, names
}

func (g *tidalGateway) enrichRatingsFromDB(ctx context.Context, tracks []providers.Track) {
	idRatings, nameRatings := cachedRatings(ctx)
	for i := range tracks {
		if tracks[i].Rating > 0 {
			continue
		}
		if r, ok := idRatings[tracks[i].ID]; ok {
			tracks[i].Rating = r
			continue
		}
		uri := "streaming://TIDAL/Track/" + tracks[i].ID
		if r, ok := idRatings[uri]; ok {
			tracks[i].Rating = r
			continue
		}
		key := strings.ToLower(strings.TrimSpace(tracks[i].Title)) + "\x00" + strings.ToLower(strings.TrimSpace(tracks[i].Artist))
		if r, ok := nameRatings[key]; ok {
			tracks[i].Rating = r
		}
	}
}

func (g *tidalGateway) playlistTracks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("uuid")
	if id == hintPlaylist {
		respond(w, page(r, 500, []any{}))
		return
	}
	if scope := g.s.scope(); id == stemsListID(scope) {
		g.stemsListTracks(w, r, scope)
		return
	}
	ref, ok := g.list(id)
	if !ok {
		fail(w, 404, errors.New("playlist not found"))
		return
	}

	q := r.URL.Query()
	sortParam := strings.ToLower(q.Get("sort"))
	orderParam := strings.ToLower(q.Get("order"))
	role := sortParam
	order := orderParam
	if role == "" && orderParam != "" && orderParam != "asc" && orderParam != "desc" {
		role = orderParam
		order = ""
	}
	if role == "" || role == "default" {
		g.s.uiMu.Lock()
		ui := g.s.loadUI()
		g.s.uiMu.Unlock()
		if role == "" {
			role = strings.ToLower(ui.StreamSortRole)
		}
		if order == "" {
			order = strings.ToLower(ui.StreamSortOrder)
		}
	}
	if order == "" {
		order = "asc"
	}

	if ref.provider == localProvider {
		refs, err := g.s.driveListTracks(r.Context(), ref.list.ID)
		if err != nil {
			fail(w, 404, err)
			return
		}
		localTracks := make([]providers.Track, len(refs))
		for i := range refs {
			localTracks[i] = refs[i].track
		}
		g.enrichRatingsFromDB(r.Context(), localTracks)
		for i := range refs {
			refs[i].track.Rating = localTracks[i].Rating
		}
		if role != "" && role != "default" {
			sortStreamingTracks(refs, role, order)
		}
		respond(w, page(r, 500, g.refItems(refs)))
		return
	}
	var p providers.Provider = g.catalog
	guest, session := false, ref.session
	if ref.session == "" && ref.provider == "soundcloud" {
		sc := g.s.ownerSoundCloud()
		if sc == nil {
			fail(w, 404, errors.New("soundcloud: not connected"))
			return
		}
		p = sc
	} else if ref.session != "" {
		v := g.sessionByID(ref.session)
		if v == nil && !ref.guest { // the owner's login after a restart (#73)
			for _, o := range g.sources() {
				if !o.Guest && o.Provider == ref.provider {
					v = o
				}
			}
		}
		if v == nil {
			fail(w, 404, errors.New("login expired; sign in again at "+lanHost()+":8080"))
			return
		}
		p, guest, session = v.p, v.Guest, v.ID
	}
	tracks, err := g.browseTracks(r.Context(), ref.provider, session, p, ref.list.ID)
	if err != nil {
		status := 502
		if errors.Is(err, errBrowseTooLarge) {
			status = 400
		}
		fail(w, status, err)
		return
	}
	if len(tracks) > maxPlaylistTracks {
		fail(w, 400, errors.New("playlist too large"))
		return
	}
	g.s.cfg.BPM.Enrich(r.Context(), tracks)
	g.enrichRatingsFromDB(r.Context(), tracks)

	refs := make([]trackRef, 0, len(tracks))
	for _, t := range tracks {
		refs = append(refs, trackRef{track: t, session: session, guest: guest})
	}
	if role != "" && role != "default" {
		sortStreamingTracks(refs, role, order)
	}
	respond(w, page(r, 500, g.refItems(refs)))
}

// favoriteTracks maps to the owner's NetEase "liked songs", which NetEase
// always returns as the first playlist of /user/playlist.
func (g *tidalGateway) favoriteTracks(w http.ResponseWriter, r *http.Request) {
	items := []any{}
	scope := g.s.scope()
	for _, v := range g.sources() {
		if v.Guest || v.Provider != scope {
			continue
		}
		var tracks []providers.Track
		var err error
		switch scope {
		case "netease":
			var lists []providers.Playlist
			if lists, err = g.browseLists(r.Context(), v); err == nil && len(lists) > 0 {
				tracks, err = g.browseTracks(r.Context(), scope, v.ID, v.p, lists[0].ID)
			}
		case "spotify":
			tracks, err = g.browseTracks(r.Context(), scope, v.ID, v.p, providers.SpotifyLikedID)
		case "soundcloud":
			tracks, err = g.browseTracks(r.Context(), scope, v.ID, v.p, "likes")
		}
		if err == nil && len(tracks) <= maxPlaylistTracks {
			g.s.cfg.BPM.Enrich(r.Context(), tracks)
			items = g.trackItems(v.ID, false, tracks)
		}
		break
	}
	respond(w, page(r, 500, items))
}

// ownerNetease is the owner's NetEase session while the deck is showing NetEase.
func (g *tidalGateway) ownerNetease() (*Session, *providers.Netease) {
	if g.s.scope() != "netease" {
		return nil, nil
	}
	for _, v := range g.sources() {
		if n, ok := v.p.(*providers.Netease); ok && !v.Guest {
			return v, n
		}
	}
	return nil, nil
}

func (g *tidalGateway) favoriteAlbums(w http.ResponseWriter, r *http.Request) {
	items := []any{}
	if _, n := g.ownerNetease(); n != nil {
		albums, err := n.FavoriteAlbums(r.Context())
		if err != nil {
			log.Printf("stream-sync: gateway source failed: %v", err)
		}
		for _, a := range albums {
			names := []any{}
			for i, artist := range a.Artists {
				kind := "FEATURED"
				if i == 0 {
					kind = "MAIN"
				}
				names = append(names, map[string]any{"id": tidalNumericID(artist.Name), "name": artist.Name, "type": kind, "picture": nil})
			}
			var main any
			if len(names) > 0 {
				main = names[0]
			}
			v := map[string]any{"id": a.ID, "title": a.Name, "numberOfTracks": a.Size, "numberOfVideos": 0, "numberOfVolumes": 1, "duration": 0, "releaseDate": "2000-01-01", "streamStartDate": "2000-01-01T00:00:00.000+0000", "cover": nil, "streamReady": true, "allowStreaming": true, "premiumStreamingOnly": false, "copyright": providerLabels["netease"], "explicit": false, "popularity": 0, "audioQuality": "LOSSLESS", "audioModes": []string{"STEREO"}, "type": "ALBUM", "url": "http://sc5000.lan:8080", "artist": main, "artists": names}
			flat := map[string]any{"item": v, "created": tidalTime(time.Now())}
			for k, f := range v {
				flat[k] = f
			}
			items = append(items, flat)
		}
	}
	respond(w, page(r, 100, items))
}

func (g *tidalGateway) albumTracks(w http.ResponseWriter, r *http.Request) {
	session, n := g.ownerNetease()
	if n == nil {
		respond(w, page(r, 100, []any{}))
		return
	}
	tracks, err := g.browseTracks(r.Context(), "netease", session.ID, n, "album:"+r.PathValue("id"))
	if err != nil {
		fail(w, 502, err)
		return
	}
	g.s.cfg.BPM.Enrich(r.Context(), tracks)
	respond(w, page(r, 500, g.trackItems(session.ID, false, tracks)))
}

func (g *tidalGateway) search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" || len(query) > 200 {
		fail(w, 400, errors.New("query required"))
		return
	}
	// Providers return one page, but Engine asks for six per keystroke. Answering
	// the rest locally keeps fast typing from tripping NetEase's rate limit, which
	// would empty the results of the query that counts.
	if offset, _ := strconv.Atoi(r.URL.Query().Get("offset")); offset > 0 {
		none := page(r, 50, []any{})
		respond(w, map[string]any{"tracks": none, "playlists": none, "albums": none, "artists": none})
		return
	}
	type source struct {
		session, provider string
		guest             bool
		p                 any
	}
	scope := g.s.scope()
	sources := []source{}
	// Gateway search deliberately requires a login, including services whose
	// provider APIs also support anonymous/public search.
	for _, v := range g.sourcesFor(scope) {
		if v.Provider == scope {
			sources = append(sources, source{v.ID, v.Provider, v.Guest, v.p})
		}
	}
	var tracks [][]providers.Track
	var lists [][]providers.Playlist
	var searches sync.WaitGroup
	searches.Add(2)
	go func() {
		defer searches.Done()
		tracks = each(r.Context(), sources, func(ctx context.Context, v source) ([]providers.Track, error) {
			if p, ok := v.p.(trackSearcher); ok {
				return g.searchTracks(ctx, v.provider, v.session, p, query)
			}
			return nil, nil
		})
	}()
	go func() {
		defer searches.Done()
		lists = each(r.Context(), sources, func(ctx context.Context, v source) ([]providers.Playlist, error) {
			if p, ok := v.p.(playlistSearcher); ok {
				return g.searchLists(ctx, v.provider, v.session, p, query)
			}
			return nil, nil
		})
	}()
	searches.Wait()
	trackItems, listItems := []any{}, []any{}
	for i, v := range sources {
		g.s.cfg.BPM.Enrich(r.Context(), tracks[i])
		for _, t := range tracks[i] {
			trackItems = append(trackItems, g.trackJSON(g.addTrack(trackRef{track: t, session: v.session, guest: v.guest}), t))
		}
		for _, p := range lists[i] {
			ref := playlistRef{v.session, v.provider, p, v.guest}
			listItems = append(listItems, g.refJSON(g.addPlaylist(ref), ref))
		}
	}
	if g.s.omniSource() { // the DJ's own drives first
		trackItems = append(g.omniSearch(r.Context(), query), trackItems...)
	}
	empty := map[string]any{"limit": 10, "offset": 0, "totalNumberOfItems": 0, "items": []any{}}
	respond(w, map[string]any{"tracks": page(r, 50, trackItems), "playlists": page(r, 50, listItems), "albums": empty, "artists": empty})
}
func (g *tidalGateway) lookup(w http.ResponseWriter, raw string) (int64, trackRef, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	g.mu.Lock()
	ref, ok := g.tracks[id]
	if ok {
		g.seq++
		g.trackSeen[id] = g.seq
	}
	g.mu.Unlock()
	if err != nil || !ok {
		fail(w, 404, errors.New("track not found; browse it on the deck first"))
		return 0, trackRef{}, false
	}
	return id, ref, true
}

// streamedFile is the local file of a row Engine loaded from the gateway
// (streaming://TIDAL/Track/<id>): the drive file of an OmniSource row, or the
// finished download. "" when there is none yet.
func (g *tidalGateway) streamedFile(net string) string {
	rest, ok := strings.CutPrefix(net, "streaming://")
	if !ok {
		return ""
	}
	id, err := strconv.ParseInt(path.Base(rest), 10, 64)
	if err != nil {
		return ""
	}
	g.mu.Lock()
	ref, ok := g.tracks[id]
	g.mu.Unlock()
	switch {
	case !ok:
		return ""
	case ref.file != "":
		p, _ := localFile(ref)
		return p
	case g.s.jobs != nil:
		return g.s.jobs.File(ref.track)
	}
	return ""
}
func (g *tidalGateway) trackInfo(w http.ResponseWriter, r *http.Request) {
	if id, ref, ok := g.lookup(w, r.PathValue("id")); ok {
		// Lists preview a catalog tempo. A track on a deck is Engine's to analyse.
		t := ref.track
		t.BPM, t.Key = 0, ""
		respond(w, g.trackJSON(id, t))
	}
}

// streamTarget reports MP3 only when an explicitly allowed lossy download is
// already on disk; otherwise Deemix is asked for FLAC.
func (g *tidalGateway) streamTarget(r *http.Request, id int64, ref trackRef) (address, codec string) {
	codec = "flac"
	ext := "flac"
	if ref.file != "" {
		codec = localCodecs[strings.ToLower(path.Ext(ref.file))]
		if codec == "mp3" {
			ext = "mp3"
		} else if codec != "flac" {
			ext = "mp4"
		}
	} else if ref.track.Provider == "spotify" {
		codec, ext = "vorbis", "ogg"
	} else if g.s.jobs != nil {
		switch strings.ToLower(path.Ext(g.s.jobs.File(ref.track))) {
		case ".mp3":
			codec, ext = "mp3", "mp3"
		case ".ogg":
			codec, ext = "vorbis", "ogg"
		case ".mp4", ".m4a", ".aac":
			// Engine saves this codec as .mp4, which its AAC reader opens.
			codec, ext = "mp4a.40.2", "mp4"
		case ".wav":
			codec, ext = "wav", "wav"
		}
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:8080"
	}
	return "http://" + host + "/v1/stream/" + strconv.FormatInt(id, 10) + "." + ext, codec
}

// Spotify always delivers native Vorbis. Other variable-codec sources download
// first; otherwise Deemix delivers FLAC or fails.
func (g *tidalGateway) knownCodec(w http.ResponseWriter, r *http.Request, ref trackRef) bool {
	j := g.s.jobs
	if ref.file != "" || ref.track.Provider == "spotify" || j == nil || j.File(ref.track) != "" {
		return true
	}
	// These providers can deliver different codecs regardless of the quality setting.
	switch ref.track.Provider {
	case "soundcloud", "beatport", "webdav", "subsonic", "plex":
	default:
		if !j.Lossy() {
			return true
		}
	}
	http.NewResponseController(w).SetWriteDeadline(time.Time{})
	if _, err := j.Fetch(r.Context(), ref.session, ref.track, ref.guest); err != nil {
		fail(w, 502, err)
		return false
	}
	return true
}
func (g *tidalGateway) playbackInfo(w http.ResponseWriter, r *http.Request) {
	id, ref, ok := g.lookup(w, r.PathValue("id"))
	if !ok {
		return
	}
	log.Printf("stream-sync: load track=%d provider=%s scope=%s step=request api=manifest", id, ref.track.Provider, g.s.scope())
	if !g.knownCodec(w, r, ref) {
		return
	}
	address, codec := g.streamTarget(r, id, ref)
	mime := "audio/flac"
	if codec == "mp3" {
		mime = "audio/mpeg"
	} else if ext := path.Ext(address); ext == ".mp4" {
		mime = "audio/mp4"
	} else if ext == ".ogg" {
		mime = "audio/ogg"
	}
	manifest, _ := json.Marshal(map[string]any{"mimeType": mime, "codecs": codec, "encryptionType": "NONE", "urls": []string{address}})
	log.Printf("stream-sync: load track=%d provider=%s step=manifest codec=%s", id, ref.track.Provider, codec)
	respond(w, map[string]any{"trackId": id, "assetPresentation": "FULL", "audioMode": "STEREO", "audioQuality": "LOSSLESS", "manifestMimeType": "application/vnd.tidal.bts", "manifestHash": "", "manifest": base64.StdEncoding.EncodeToString(manifest), "albumReplayGain": 0, "albumPeakAmplitude": 1, "trackReplayGain": 0, "trackPeakAmplitude": 1})
}
func (g *tidalGateway) streamURL(w http.ResponseWriter, r *http.Request) {
	if id, ref, ok := g.lookup(w, r.PathValue("id")); ok && g.knownCodec(w, r, ref) {
		address, codec := g.streamTarget(r, id, ref)
		log.Printf("stream-sync: load track=%d provider=%s scope=%s step=manifest api=streamurl codec=%s", id, ref.track.Provider, g.s.scope(), codec)
		respond(w, map[string]any{"url": address, "codec": codec, "soundQuality": "LOSSLESS", "trackId": id})
	}
}

// stemsSource returns the local file of a streamed row, downloading it if needed,
// so the deck can stem it as a local track. Only a Stems request calls this;
// playback keeps using stream.
func (g *tidalGateway) stemsSource(w http.ResponseWriter, r *http.Request) {
	_, ref, ok := g.lookup(w, r.URL.Query().Get("id"))
	if !ok {
		return
	}
	if ref.file != "" { // a drive track is already local
		if p, err := localFile(ref); err != nil {
			fail(w, 404, err)
		} else {
			respond(w, map[string]any{"path": p})
		}
		return
	}
	if g.s.jobs == nil {
		fail(w, 503, errors.New("worker unavailable"))
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Time{})
	p, err := g.s.jobs.Fetch(r.Context(), ref.session, ref.track, ref.guest)
	if err != nil {
		fail(w, 502, err)
		return
	}
	// The row joins the Stems list of the service it was stemmed from.
	stemsScope := ref.track.Provider
	if v := g.sessionByID(ref.session); v != nil && v.Provider != "" {
		stemsScope = v.Provider
	}
	if stemsScope == "" {
		stemsScope = g.s.scope()
	}
	if err = g.s.addStemmed(stemsScope, stemmedTrack{ref.track, ref.session, ref.guest, p}); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, map[string]any{"path": p})
}

// stemsTrackID answers the internal library id of an imported file, by file name.
// The UI cannot resolve it itself: while a streaming collection is open, its
// collectionController does not see the internal m.db. Read-only (#49); 0 = not imported yet.
func (g *tidalGateway) stemsTrackID(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Query().Get("path"))
	if name == "." || name == string(filepath.Separator) {
		fail(w, 400, errors.New("path required"))
		return
	}
	mdb := filepath.Join(shadowDataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer db.Close()
	var id int64
	err = db.QueryRowContext(r.Context(), `SELECT id FROM Track WHERE filename = ? ORDER BY id DESC LIMIT 1`, name).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		fail(w, 500, err)
		return
	}
	respond(w, map[string]any{"trackId": id})
}

// stream serves a finished download (with Range support) or downloads it now;
// the CPU3 analysis/import job is queued by Jobs.Fetch and runs afterwards.
func (g *tidalGateway) stream(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	raw := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(file, ".flac"), ".mp3"), ".mp4"), ".ogg"), ".wav")
	if raw == file {
		http.NotFound(w, r)
		return
	}
	id, ref, ok := g.lookup(w, raw)
	if !ok {
		return
	}
	started := time.Now()
	log.Printf("stream-sync: load track=%d provider=%s scope=%s step=stream result=request", id, ref.track.Provider, g.s.scope())
	if ref.file != "" { // OmniSource: straight from the drive, with Range
		p, err := localFile(ref)
		if err != nil {
			fail(w, 404, err)
			return
		}
		mime := map[string]string{"flac": "audio/flac", "mp3": "audio/mpeg"}[localCodecs[strings.ToLower(path.Ext(p))]]
		if mime == "" {
			mime = "audio/mp4"
		}
		w.Header().Set("Content-Type", mime)
		http.ServeFile(w, r, p)
		return
	}
	if g.s.jobs == nil {
		fail(w, 503, errors.New("worker unavailable"))
		return
	}
	// A first play waits for Deemix; lift the server's 2-minute write limit.
	http.NewResponseController(w).SetWriteDeadline(time.Time{})
	p, err := g.s.jobs.Fetch(r.Context(), ref.session, ref.track, ref.guest)
	if err != nil {
		log.Printf("stream-sync: load track=%d provider=%s step=stream result=failed elapsed=%s canceled=%t error_type=%T", id, ref.track.Provider, time.Since(started).Round(time.Millisecond), r.Context().Err() != nil, err)
		fail(w, 502, err)
		return
	}
	w.Header().Set("Content-Type", "audio/flac")
	if strings.HasSuffix(strings.ToLower(p), ".mp4") {
		w.Header().Set("Content-Type", "audio/mp4")
	} else if strings.HasSuffix(strings.ToLower(p), ".mp3") {
		w.Header().Set("Content-Type", "audio/mpeg")
	} else if strings.HasSuffix(strings.ToLower(p), ".ogg") {
		w.Header().Set("Content-Type", "audio/ogg")
	} else if strings.HasSuffix(strings.ToLower(p), ".wav") {
		w.Header().Set("Content-Type", "audio/wav")
	}
	log.Printf("stream-sync: load track=%d provider=%s step=stream result=ready elapsed=%s canceled=%t mime=%s", id, ref.track.Provider, time.Since(started).Round(time.Millisecond), r.Context().Err() != nil, w.Header().Get("Content-Type"))
	// Engine's stm.db row exists by now; it may still be settling, so retry a few times.
	go func() {
		for _, wait := range []time.Duration{0, 800 * time.Millisecond, 1700 * time.Millisecond, 3500 * time.Millisecond} {
			time.Sleep(wait)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			g.syncStmAlbumArt(ctx, id, ref.track)
			cancel()
		}
	}()
	http.ServeFile(w, r, p)
}
