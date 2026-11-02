package webui

// The patched Engine sends its built-in TidalAPI requests to
// http://127.0.0.1:8080/v1/ and /au//. This gateway answers them with the
// playlists of the logged-in NetEase/Spotify/Deezer sessions, so the deck's
// native TIDAL browser plays them. Audio comes from the Deemix pipeline.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/crc32"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
	"github.com/google/uuid"
)

const hintPlaylist = "00000000-0000-5000-8000-000000005000"

// The deck shows NetEase, Spotify and Deezer as separate sources. They share
// Engine's single TIDAL client, so the source the user last opened (the UI
// "scope") decides what this gateway answers. Loaded tracks keep working after
// a switch because track IDs are global.
func hintTitle(scope string) string { return "请登录：sc5000.lan:8080" }

var (
	tidalNS        = uuid.MustParse("6f1c5a52-3c55-4c2e-9d0b-5c5000000000")
	providerLabels = map[string]string{"netease": "网易云音乐", "spotify": "Spotify", "deezer": "Deezer"}
	providerOrder  = map[string]int{"netease": 0, "spotify": 1, "deezer": 2}
)

type trackRef struct {
	track   providers.Track
	session string // empty for the built-in Deezer catalog
	guest   bool
}
type playlistRef struct {
	session  string // empty for the built-in Deezer catalog
	provider string
	list     providers.Playlist
}
type trackSearcher interface {
	Search(context.Context, string) ([]providers.Track, error)
}
type playlistSearcher interface {
	SearchPlaylists(context.Context, string) ([]providers.Playlist, error)
}
type tidalGateway struct {
	s       *Server
	catalog *providers.Deezer
	mux     *http.ServeMux
	mu      sync.Mutex
	ids     map[string]int64
	tracks  map[int64]trackRef
	lists   map[string]playlistRef
	next    int64

	// Last-use sequence numbers for evicting the least recently used refs.
	seq       int64
	trackSeen map[int64]int64
	listSeen  map[string]int64
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
	g := &tidalGateway{s: s, catalog: &providers.Deezer{BaseURL: s.cfg.DeezerURL, HTTP: s.cfg.HTTP}, mux: http.NewServeMux(), ids: map[string]int64{}, tracks: map[int64]trackRef{}, lists: map[string]playlistRef{}, trackSeen: map[int64]int64{}, listSeen: map[string]int64{}}
	g.mux.HandleFunc("POST /au/oauth2/device_authorization", g.deviceAuthorization)
	g.mux.HandleFunc("POST /au/oauth2/token", g.token)
	g.mux.HandleFunc("POST /v1/sc5000/login", s.loginStart)
	g.mux.HandleFunc("GET /v1/sc5000/login", s.loginStatus)
	g.mux.HandleFunc("GET /v1/sc5000/login/qr", s.loginQR)
	g.mux.HandleFunc("GET /v1/sc5000/ui", s.uiStatus)
	g.mux.HandleFunc("POST /v1/sc5000/ui", s.uiUpdate)
	g.mux.HandleFunc("GET /v1/sc5000/bpm", s.bpmConfigGet)
	g.mux.HandleFunc("POST /v1/sc5000/bpm", s.bpmConfigPost)
	g.mux.HandleFunc("GET /v1/sc5000/relay", s.relayConfigGet)
	g.mux.HandleFunc("POST /v1/sc5000/relay", s.relayConfigPost)
	g.mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("OK")) })
	g.mux.HandleFunc("GET /v1/sessions", g.session)
	g.mux.HandleFunc("GET /v1/users/{id}/subscription", g.subscription)
	g.mux.HandleFunc("GET /v1/users/{id}/playlists", g.playlists)
	g.mux.HandleFunc("GET /v1/users/{id}/favorites/playlists", g.playlists)
	g.mux.HandleFunc("GET /v1/users/{id}/favorites/tracks", g.favoriteTracks)
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
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("stream-sync: TIDAL unreachable: %v", err)
			fail(w, 502, errors.New("could not reach TIDAL"))
		},
	}
	http.NewResponseController(w).SetWriteDeadline(time.Time{})
	proxy.ServeHTTP(w, r)
}

// withBPM shows tempo and key on each list row. Engine's TIDAL client reads no
// tempo field, so the title is the only place the deck can display them. This is
// display text only and reaches neither the beat grid nor the library.
func withBPM(t providers.Track) string {
	if t.BPM <= 0 {
		return t.Title
	}
	label := strconv.Itoa(int(math.Round(t.BPM)))
	if t.Key != "" {
		label += " " + t.Key
	}
	return t.Title + " [" + label + "]"
}

// tidalNumericID hashes a name to an ID Engine can read: it parses them as signed
// 32-bit integers.
func tidalNumericID(name string) uint32 { return crc32.ChecksumIEEE([]byte(name)) & 0x7fffffff }

func isTidal(p string) bool { return strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/au/") }

// ServeHTTP folds the "/au//" and "/v1//" joins produced by the patched base
// URLs. The gateway has no console login, so only the deck itself may use it.
func (g *tidalGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		fail(w, 403, errors.New("the streaming gateway is local to the deck"))
		return
	}
	clean := path.Clean(r.URL.Path)
	if !isTidal(clean + "/") {
		http.NotFound(w, r)
		return
	}
	if g.s.scope() == "tidal" && !strings.HasPrefix(clean, "/v1/sc5000/") && !strings.HasPrefix(clean, "/v1/stream/") {
		g.proxyToTidal(w, r, clean)
		return
	}
	u := *r.URL
	u.Path, u.RawPath = clean, ""
	r2 := r.Clone(r.Context())
	r2.URL = &u
	g.mux.ServeHTTP(w, r2)
}
func (g *tidalGateway) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]any{"deviceCode": "sc5000-local", "userCode": "SC5000", "verificationUri": "sc5000.lan:8080", "verificationUriComplete": "http://sc5000.lan:8080", "expiresIn": 600, "interval": 1})
}
func (g *tidalGateway) token(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]any{"access_token": "sc5000-token", "refresh_token": "sc5000-refresh", "token_type": "Bearer", "expires_in": 86400, "user": map[string]any{"userId": 1, "countryCode": "US"}})
}
func (g *tidalGateway) session(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]any{"sessionId": "sc5000-session", "userId": 1, "countryCode": "US"})
}
func (g *tidalGateway) subscription(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]any{"status": "ACTIVE", "subscription": map[string]any{"type": "PREMIUM_PLUS"}, "highestSoundQuality": "LOSSLESS"})
}

// sources returns ready sessions, restoring persisted owner logins after a
// restart so the deck works without opening the phone console first.
func (g *tidalGateway) sources() []*Session {
	s := g.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	for _, name := range []string{"netease", "spotify", "deezer"} {
		owned := false
		for _, v := range s.sessions {
			owned = owned || (!v.Guest && v.Provider == name)
		}
		if owned || s.ownerDeleted[name] || len(s.sessions) >= 16 {
			continue
		}
		if p, err := s.makeProvider(name, false, ""); err == nil && ready(p) {
			id := providers.RandomID()
			s.sessions[id] = &Session{ID: id, Provider: name, Ready: true, p: p, tracks: map[string]providers.Track{}, expires: time.Now().Add(8 * time.Hour)}
		}
	}
	var out []*Session
	for _, v := range s.sessions {
		if v.Ready {
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
		return v.Token.AccessToken != ""
	case *providers.Netease:
		return v.UserID != 0 && v.Cookie != ""
	case *providers.Deezer:
		return v.UserID != ""
	}
	return false
}
func (g *tidalGateway) sessionByID(id string) *Session {
	g.s.mu.Lock()
	defer g.s.mu.Unlock()
	g.s.prune()
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
	id := uuid.NewSHA1(tidalNS, []byte(ref.session+"\x00"+ref.provider+"\x00"+ref.list.ID)).String()
	g.mu.Lock()
	defer g.mu.Unlock()
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

// addTrack assigns stable positive IDs per source track for this process.
func (g *tidalGateway) addTrack(ref trackRef) int64 {
	key := ref.session + "\x00" + ref.track.Provider + "\x00" + ref.track.ID
	g.mu.Lock()
	defer g.mu.Unlock()
	id := g.ids[key]
	if id == 0 {
		g.next++
		id = g.next
		g.ids[key] = id
	}
	g.tracks[id] = ref
	g.seq++
	g.trackSeen[id] = g.seq
	evict(g.trackSeen, maxTracks, func(id int64) {
		r := g.tracks[id]
		delete(g.ids, r.session+"\x00"+r.track.Provider+"\x00"+r.track.ID)
		delete(g.tracks, id)
	})
	return id
}
func tidalTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000-0700") }
func playlistJSON(id, title, description string, count int) map[string]any {
	now := tidalTime(time.Now())
	return map[string]any{"uuid": id, "title": title, "numberOfTracks": count, "numberOfVideos": 0, "description": description, "duration": 0, "lastUpdated": now, "created": now, "type": "USER", "publicPlaylist": false, "url": "http://sc5000.lan:8080", "image": "", "squareImage": "", "popularity": 0}
}
func (g *tidalGateway) refJSON(id string, ref playlistRef) map[string]any {
	label := providerLabels[ref.provider]
	return playlistJSON(id, ref.list.Title, label+" 歌单，经 SC5000 Stream Sync 下载播放", ref.list.Count)
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
	lists := each(r.Context(), sessions, func(ctx context.Context, v *Session) ([]providers.Playlist, error) { return v.p.Playlists(ctx) })
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
	for i, v := range sessions {
		for _, p := range lists[i] {
			ref := playlistRef{v.ID, v.Provider, p}
			add(g.refJSON(g.addPlaylist(ref), ref))
		}
	}
	// Deezer's public charts need no login; the others explain how to log in.
	if scope == "deezer" {
		defaults := each(r.Context(), []*providers.Deezer{g.catalog}, func(ctx context.Context, d *providers.Deezer) ([]providers.Playlist, error) {
			return d.ChartPlaylists(ctx)
		})
		for _, p := range defaults[0] {
			ref := playlistRef{"", "deezer", p}
			add(g.refJSON(g.addPlaylist(ref), ref))
		}
	} else if len(sessions) == 0 {
		add(playlistJSON(hintPlaylist, hintTitle(scope), "手机打开 http://sc5000.lan:8080 登录后，歌单会出现在这里", 0))
	}
	respond(w, page(r, 100, items))
}
func (g *tidalGateway) playlist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("uuid")
	if id == hintPlaylist {
		respond(w, playlistJSON(hintPlaylist, hintTitle(g.s.scope()), "", 0))
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
	return map[string]any{"id": id, "title": withBPM(t), "bpm": int(math.Round(t.BPM)), "duration": t.DurationMS / 1000, "replayGain": 0, "peak": 1, "allowStreaming": true, "streamReady": true, "premiumStreamingOnly": false, "trackNumber": 1, "volumeNumber": 1, "version": nil, "popularity": 0, "copyright": providerLabels[t.Provider], "url": "http://sc5000.lan:8080", "isrc": t.ISRC, "editable": false, "explicit": false, "audioQuality": "LOSSLESS", "audioModes": []string{"STEREO"}, "artist": artist, "artists": artists, "album": map[string]any{"id": tidalNumericID(t.Album), "title": t.Album, "cover": nil, "vibrantColor": nil, "videoCover": nil}, "mixes": map[string]any{}}
}

// trackItems wraps each track as {"item":…,"type":"track"} (playlist items and
// favourites) and repeats its fields flat, as plain /tracks listings return them.
func (g *tidalGateway) trackItems(session string, guest bool, tracks []providers.Track) []any {
	items := make([]any, 0, len(tracks))
	for _, t := range tracks {
		v := g.trackJSON(g.addTrack(trackRef{t, session, guest}), t)
		flat := map[string]any{"item": v, "type": "track"}
		for k, f := range v {
			flat[k] = f
		}
		items = append(items, flat)
	}
	return items
}
func (g *tidalGateway) playlistTracks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("uuid")
	if id == hintPlaylist {
		respond(w, page(r, 500, []any{}))
		return
	}
	ref, ok := g.list(id)
	if !ok {
		fail(w, 404, errors.New("playlist not found"))
		return
	}
	var p providers.Provider = g.catalog
	guest := false
	if ref.session != "" {
		v := g.sessionByID(ref.session)
		if v == nil {
			fail(w, 404, errors.New("login expired; sign in again at sc5000.lan:8080"))
			return
		}
		p, guest = v.p, v.Guest
	}
	tracks, err := p.Tracks(r.Context(), ref.list.ID)
	if err != nil {
		fail(w, 502, err)
		return
	}
	if len(tracks) > maxPlaylistTracks {
		fail(w, 400, errors.New("playlist too large"))
		return
	}
	g.s.cfg.BPM.Enrich(r.Context(), tracks)
	respond(w, page(r, 500, g.trackItems(ref.session, guest, tracks)))
}

// favoriteTracks maps to the owner's NetEase "liked songs", which NetEase
// always returns as the first playlist of /user/playlist.
func (g *tidalGateway) favoriteTracks(w http.ResponseWriter, r *http.Request) {
	items := []any{}
	for _, v := range g.sources() {
		if v.Provider != "netease" || v.Guest || g.s.scope() != "netease" {
			continue
		}
		if lists, err := v.p.Playlists(r.Context()); err == nil && len(lists) > 0 {
			if tracks, err := v.p.Tracks(r.Context(), lists[0].ID); err == nil && len(tracks) <= maxPlaylistTracks {
				g.s.cfg.BPM.Enrich(r.Context(), tracks)
				items = g.trackItems(v.ID, false, tracks)
			}
		}
		break
	}
	respond(w, page(r, 500, items))
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
	if scope == "deezer" {
		sources = append(sources, source{"", "deezer", false, g.catalog})
	}
	for _, v := range g.sources() {
		if v.Provider == scope && v.Provider != "deezer" {
			sources = append(sources, source{v.ID, v.Provider, v.Guest, v.p})
		}
	}
	tracks := each(r.Context(), sources, func(ctx context.Context, v source) ([]providers.Track, error) {
		if p, ok := v.p.(trackSearcher); ok {
			return p.Search(ctx, query)
		}
		return nil, nil
	})
	lists := each(r.Context(), sources, func(ctx context.Context, v source) ([]providers.Playlist, error) {
		if p, ok := v.p.(playlistSearcher); ok {
			return p.SearchPlaylists(ctx, query)
		}
		return nil, nil
	})
	trackItems, listItems := []any{}, []any{}
	for i, v := range sources {
		g.s.cfg.BPM.Enrich(r.Context(), tracks[i])
		for _, t := range tracks[i] {
			trackItems = append(trackItems, g.trackJSON(g.addTrack(trackRef{t, v.session, v.guest}), t))
		}
		for _, p := range lists[i] {
			ref := playlistRef{v.session, v.provider, p}
			listItems = append(listItems, g.refJSON(g.addPlaylist(ref), ref))
		}
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
	if g.s.jobs != nil && strings.HasSuffix(strings.ToLower(g.s.jobs.File(ref.track)), ".mp3") {
		codec = "mp3"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:8080"
	}
	return "http://" + host + "/v1/stream/" + strconv.FormatInt(id, 10) + "." + codec, codec
}

// knownCodec downloads first when MP3 fallback is allowed, because the codec
// is only known afterwards; otherwise Deemix delivers FLAC or fails.
func (g *tidalGateway) knownCodec(w http.ResponseWriter, r *http.Request, ref trackRef) bool {
	j := g.s.jobs
	if j == nil || !j.Lossy() || j.File(ref.track) != "" {
		return true
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
	if !g.knownCodec(w, r, ref) {
		return
	}
	address, codec := g.streamTarget(r, id, ref)
	mime := "audio/flac"
	if codec == "mp3" {
		mime = "audio/mpeg"
	}
	manifest, _ := json.Marshal(map[string]any{"mimeType": mime, "codecs": codec, "encryptionType": "NONE", "urls": []string{address}})
	respond(w, map[string]any{"trackId": id, "assetPresentation": "FULL", "audioMode": "STEREO", "audioQuality": "LOSSLESS", "manifestMimeType": "application/vnd.tidal.bts", "manifestHash": "", "manifest": base64.StdEncoding.EncodeToString(manifest), "albumReplayGain": 0, "albumPeakAmplitude": 1, "trackReplayGain": 0, "trackPeakAmplitude": 1})
}
func (g *tidalGateway) streamURL(w http.ResponseWriter, r *http.Request) {
	if id, ref, ok := g.lookup(w, r.PathValue("id")); ok && g.knownCodec(w, r, ref) {
		address, codec := g.streamTarget(r, id, ref)
		respond(w, map[string]any{"url": address, "codec": codec, "soundQuality": "LOSSLESS", "trackId": id})
	}
}

// stream serves a finished download (with Range support) or downloads it now;
// the CPU3 analysis/import job is queued by Jobs.Fetch and runs afterwards.
func (g *tidalGateway) stream(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	raw := strings.TrimSuffix(strings.TrimSuffix(file, ".flac"), ".mp3")
	if raw == file {
		http.NotFound(w, r)
		return
	}
	_, ref, ok := g.lookup(w, raw)
	if !ok {
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
		fail(w, 502, err)
		return
	}
	w.Header().Set("Content-Type", "audio/flac")
	if strings.HasSuffix(strings.ToLower(p), ".mp3") {
		w.Header().Set("Content-Type", "audio/mpeg")
	}
	http.ServeFile(w, r, p)
}
