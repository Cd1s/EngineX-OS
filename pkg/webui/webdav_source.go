package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// WebDAV is a source of its own (maintainer, 2026-11-27): Settings -> Services
// switches it like the other services, the DJ adds several servers on the deck or
// on the phone page, and each is its own entry under WebDAV. The servers live with
// the NAS in nas.json, the owner settings file and the only place their passwords
// are kept. A released deck keeps its one WebDAV server in that file's NAS slot
// (what the console's network source edits); it stays there and is the first
// WebDAV server, so it carries over with no re-entry. Servers added on the deck or
// the phone go to the file's "webdav" list.
const webdavScope = "webdav"

// maxWebDAVServers bounds what one browse or search fans out to.
const maxWebDAVServers = 16

// webdavServer is one server in nas.json's "webdav" list.
type webdavServer struct {
	Name     string `json:"name,omitempty"`
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Enabled  bool   `json:"enabled"`
}

// nasFile is nas.json: the NAS slot as before (its fields at the top level), the
// DJ's name for a WebDAV server in that slot, and the WebDAV list.
type nasFile struct {
	providers.NASConfig
	Name   string         `json:"name,omitempty"`
	WebDAV []webdavServer `json:"webdav,omitempty"`
}

// webdavEntry is one server of the WebDAV source.
type webdavEntry struct {
	// ID is the #335 root identity of the server's address: it names the server in
	// its rows ("<id>:<path>"), so two servers with the same folders never share a
	// row, a download or a Stems entry, and a changed address leaves the old rows
	// unresolved (browsed again, as the maintainer allowed on 2026-11-27).
	ID   string
	Name string // the DJ's name, else the host
	cfg  providers.NASConfig
	slot bool // in the NAS slot (the released place); otherwise list[index]
	idx  int
}

// webdavView is a server as the deck, the console and the phone page see it:
// never its password.
type webdavView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Username    string `json:"username,omitempty"`
	HasPassword bool   `json:"hasPassword"`
}

func (s *Server) loadNASFileLocked() nasFile {
	var f nasFile
	if b, err := os.ReadFile(s.nasConfigPath()); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	// Older configs could keep Basic Auth in the URL. Preserve that login without
	// returning its credentials in a URL or overwriting explicit auth fields.
	if u, err := url.Parse(strings.TrimSpace(f.URL)); err == nil && u.User != nil {
		if f.Username == "" {
			f.Username = u.User.Username()
		}
		if f.Password == "" {
			if pass, ok := u.User.Password(); ok && pass != "" {
				f.Password = pass
			}
		}
		u.User = nil
		f.URL = u.String()
	}
	return f
}

// saveNASFileLocked writes nas.json, or removes it once it holds nothing.
func (s *Server) saveNASFileLocked(f nasFile) error {
	if f.Protocol == "" && f.URL == "" && len(f.WebDAV) == 0 {
		if err := os.Remove(s.nasConfigPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return providers.SaveJSON(s.nasConfigPath(), f)
}

func webdavRoot(raw string) string {
	return nasRootID(providers.NASConfig{Protocol: webdavScope, URL: strings.TrimSpace(raw)})
}

// webdavHost is a server's name when the DJ gave none.
func webdavHost(raw string) string {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimSpace(raw)
}

func webdavEntries(f nasFile) []webdavEntry {
	var out []webdavEntry
	add := func(e webdavEntry) {
		if strings.TrimSpace(e.cfg.URL) == "" || slices.ContainsFunc(out, func(v webdavEntry) bool { return v.ID == e.ID }) {
			return
		}
		if e.Name == "" {
			e.Name = webdavHost(e.cfg.URL)
		}
		out = append(out, e)
	}
	if f.Protocol == webdavScope {
		add(webdavEntry{ID: webdavRoot(f.URL), Name: strings.TrimSpace(f.Name), cfg: f.NASConfig, slot: true, idx: -1})
	}
	for i, w := range f.WebDAV {
		cfg := providers.NASConfig{Protocol: webdavScope, URL: w.URL, Username: w.Username, Password: w.Password, Enabled: w.Enabled}
		add(webdavEntry{ID: webdavRoot(w.URL), Name: strings.TrimSpace(w.Name), cfg: cfg, idx: i})
	}
	return out
}

func webdavViews(f nasFile) []webdavView {
	views := []webdavView{}
	for _, e := range webdavEntries(f) {
		views = append(views, webdavView{ID: e.ID, Name: e.Name, URL: toNASConfigView(e.cfg).URL, Username: e.cfg.Username, HasPassword: e.cfg.Password != ""})
	}
	return views
}

// webdavTrackID names a server's track or folder in a row: "<server id>:<path>".
func webdavTrackID(server, raw string) string { return server + ":" + raw }

// webdavSplit undoes webdavTrackID. A released [NAS] row (a bare path) is none.
func webdavSplit(id string) (server, raw string, ok bool) {
	server, raw, ok = strings.Cut(id, ":")
	return server, raw, ok && isRootID(server)
}

func isRootID(v string) bool {
	if len(v) != 32 {
		return false
	}
	for _, c := range v {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isNASTrack: a track of the NAS slot (SMB, Subsonic, Plex, or a released WebDAV
// row), which the NAS generation and root guard (#335). A WebDAV source row names
// its server in its ID instead.
func isNASTrack(provider, id string) bool {
	if provider == webdavScope {
		if _, _, ok := webdavSplit(id); ok {
			return false
		}
	}
	return isNASProvider(provider)
}

// webdavRowServer is the server of a WebDAV source track.
func webdavRowServer(t providers.Track) (string, bool) {
	if t.Provider != webdavScope {
		return "", false
	}
	server, _, ok := webdavSplit(t.ID)
	return server, ok
}

// webdavOn: the WebDAV row in Settings -> Services; until the DJ uses it, WebDAV
// is on while a server is set up.
func webdavOn(v uiSettings, f nasFile) bool {
	return v.WebDAVSwitch == "1" || (v.WebDAVSwitch == "" && len(webdavEntries(f)) > 0)
}

// webdavServers are the servers that browse, play and are searched: the enabled
// ones, and none while WebDAV is off.
func (s *Server) webdavServers() []webdavEntry {
	s.uiMu.Lock()
	f := s.loadNASFileLocked()
	on := webdavOn(s.loadUI(), f)
	s.uiMu.Unlock()
	if !on {
		return nil
	}
	return slices.DeleteFunc(webdavEntries(f), func(e webdavEntry) bool { return !nasConfigReady(e.cfg) })
}

// webdavLookup is the server a row names, or nil when it is gone, disabled, or
// WebDAV is off: such a row answers 404 and is browsed again.
func (s *Server) webdavLookup(server string) *providers.SelfHostedProvider {
	for _, e := range s.webdavServers() {
		if e.ID == server {
			return s.webdavProvider(e)
		}
	}
	return nil
}

type webdavConn struct {
	cfg providers.NASConfig
	p   *providers.SelfHostedProvider
}

// webdavProvider keeps one provider per server: its track cache is what a search
// of that server looks through.
func (s *Server) webdavProvider(e webdavEntry) *providers.SelfHostedProvider {
	s.webdavMu.Lock()
	defer s.webdavMu.Unlock()
	if c := s.webdavConns[e.ID]; c != nil && c.cfg == e.cfg {
		return c.p
	}
	if s.webdavConns == nil {
		s.webdavConns = map[string]*webdavConn{}
	}
	p := providers.NewSelfHostedProvider(e.cfg, s.nasHTTPClient())
	s.webdavConns[e.ID] = &webdavConn{e.cfg, p}
	return p
}

// webdavChange is one add, edit or remove, from the deck or the phone page.
type webdavChange struct {
	Action   string `json:"action"` // add, edit, remove
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

var (
	errWebDAVURL       = errors.New("enter the server address with http:// or https://")
	errWebDAVDuplicate = errors.New("this server is already added")
	errWebDAVFull      = errors.New("no more servers can be added")
	errWebDAVGone      = errors.New("this server is no longer in the list")
	// errWebDAVServerGone: a row's server was removed, moved to another address,
	// or WebDAV is off. The row answers 404 and is browsed again.
	errWebDAVServerGone = errors.New("webdav: the server of this row is not set up or not on")
)

func webdavChangeStatus(err error) int {
	switch {
	case errors.Is(err, errWebDAVGone):
		return http.StatusNotFound
	case errors.Is(err, errWebDAVDuplicate), errors.Is(err, errWebDAVFull):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// applyWebDAV makes one change to the servers in nas.json. An empty password on an
// edit keeps the saved one. The rows and Stems entries of a server that is removed
// or moved to another address go with it.
func (s *Server) applyWebDAV(c webdavChange) ([]webdavView, error) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	defer s.resetNASIfMoved(s.loadNASConfigLocked())
	f := s.loadNASFileLocked()
	entries := webdavEntries(f)
	var cur *webdavEntry
	if c.Action != "add" {
		for i := range entries {
			if entries[i].ID == c.ID {
				cur = &entries[i]
			}
		}
		if cur == nil {
			return nil, errWebDAVGone
		}
	}
	if c.Action == "remove" {
		if cur.slot {
			f.NASConfig, f.Name = providers.NASConfig{}, ""
		} else {
			f.WebDAV = slices.Delete(f.WebDAV, cur.idx, cur.idx+1)
		}
		if err := s.saveNASFileLocked(f); err != nil {
			return nil, err
		}
		s.forgetWebDAV(webdavEntries(f))
		return webdavViews(f), nil
	}
	if c.Action != "add" && c.Action != "edit" {
		return nil, errors.New("unknown action")
	}
	raw := strings.TrimSpace(c.URL)
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || validateNASURL(raw) != nil {
		return nil, errWebDAVURL
	}
	id := webdavRoot(raw)
	if slices.ContainsFunc(entries, func(e webdavEntry) bool { return e.ID == id && (cur == nil || e.ID != cur.ID) }) {
		return nil, errWebDAVDuplicate
	}
	name, user := strings.TrimSpace(c.Name), strings.TrimSpace(c.Username)
	if r := []rune(name); len(r) > 64 {
		name = string(r[:64])
	}
	password := c.Password
	switch {
	case c.Action == "add":
		if len(entries) >= maxWebDAVServers {
			return nil, errWebDAVFull
		}
		f.WebDAV = append(f.WebDAV, webdavServer{Name: name, URL: raw, Username: user, Password: password, Enabled: true})
	case cur.slot:
		if password == "" || password == "***" {
			password = f.Password
		}
		f.NASConfig = providers.NASConfig{Protocol: webdavScope, URL: raw, Username: user, Password: password, Token: f.Token, Enabled: true}
		f.Name = name
	default:
		if password == "" || password == "***" {
			password = f.WebDAV[cur.idx].Password
		}
		f.WebDAV[cur.idx] = webdavServer{Name: name, URL: raw, Username: user, Password: password, Enabled: true}
	}
	if err := s.saveNASFileLocked(f); err != nil {
		return nil, err
	}
	s.forgetWebDAV(webdavEntries(f))
	return webdavViews(f), nil
}

// forgetWebDAV drops what belonged to servers that are no longer in the list
// (removed, or moved to another address): their gateway rows and lists, their
// Stems entries and their connections. Caller holds uiMu.
func (s *Server) forgetWebDAV(now []webdavEntry) {
	keep := map[string]bool{}
	for _, e := range now {
		keep[e.ID] = true
	}
	s.webdavMu.Lock()
	for id := range s.webdavConns {
		if !keep[id] {
			delete(s.webdavConns, id)
		}
	}
	s.webdavMu.Unlock()
	s.stemsMu.Lock()
	s.dropWebDAVStemsLocked(keep)
	s.stemsMu.Unlock()
	s.tidal.dropWebDAV(keep)
}

// handleWebDAV is the deck's list (GET) and change (POST) of WebDAV servers.
func (s *Server) handleWebDAV(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.uiMu.Lock()
		f := s.loadNASFileLocked()
		s.uiMu.Unlock()
		respond(w, map[string]any{"servers": webdavViews(f)})
		return
	}
	var c webdavChange
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		return
	}
	views, err := s.applyWebDAV(c)
	if err != nil {
		fail(w, webdavChangeStatus(err), err)
		return
	}
	respond(w, map[string]any{"servers": views})
}

// webdavLoginStart opens the phone page where WebDAV servers are added, edited and
// removed, as the QR login of the other services does. The QR code holds the
// page's address with a one-time token, never a password, and never the relay's
// (a third party).
func (s *Server) webdavLoginStart(w http.ResponseWriter) {
	flow := &loginFlow{Provider: webdavScope, Status: "waiting", token: providers.RandomID(), expires: time.Now().Add(loginLifetime)}
	s.uiMu.Lock()
	lang := s.loadUI().Language
	s.uiMu.Unlock()
	q := "?t=" + url.QueryEscape(flow.token) + "&lang=" + url.QueryEscape(lang)
	if pub := s.PublicTunnelURL(); pub != "" {
		flow.Target = strings.TrimRight(pub, "/") + "/m/" + webdavScope + q
	} else {
		flow.Target = "http://" + lanHost() + ":8080/m/" + webdavScope + q
	}
	s.mu.Lock()
	s.logins[webdavScope] = flow
	s.mu.Unlock()
	respond(w, flow)
}

// webdavLoginStatus: done once the phone page saved a change (its token is spent).
func (s *Server) webdavLoginStatus(w http.ResponseWriter, flow *loginFlow) {
	s.mu.Lock()
	if flow.Status == "linked" {
		flow.Status = "done"
	}
	if flow.Status != "done" && time.Now().After(flow.expires) {
		flow.Status = "expired"
	}
	snapshot := *flow
	s.mu.Unlock()
	respond(w, &snapshot)
}

// webdavPhonePost is one add, edit or remove from the phone page. A change spends
// the token, as a phone login does; the deck closes its QR code.
func (s *Server) webdavPhonePost(w http.ResponseWriter, r *http.Request, flow *loginFlow) {
	lang := phoneLang(r)
	c := webdavChange{Action: r.Form.Get("action"), ID: r.Form.Get("id"), Name: r.Form.Get("name"), URL: r.Form.Get("url"), Username: r.Form.Get("user"), Password: r.Form.Get("password")}
	if _, err := s.applyWebDAV(c); err != nil {
		key := map[error]string{errWebDAVURL: "wdBadURL", errWebDAVDuplicate: "wdDuplicate", errWebDAVFull: "wdFull", errWebDAVGone: "wdGone"}[err]
		if key == "" {
			key = "wdFailed"
		}
		phoneMessage(w, r, flow, tr(lang, key), "")
		return
	}
	s.mu.Lock()
	flow.Status, flow.token = "linked", ""
	s.mu.Unlock()
	phoneMessage(w, r, nil, tr(lang, "wdSaved"), tr(lang, "wdSavedHint"))
}

// nasSwitch is the NAS row in Settings -> Services: it turns the SMB NAS the
// console set up on or off. That is nas.json's "enabled", the flag the console
// saves, so the deck and the console share one setting. Off hides NAS from the
// Source list and stops its browsing, playing and searching (no NAS provider).
func (s *Server) nasSwitch(w http.ResponseWriter, r *http.Request, val string) {
	if val != "0" && val != "1" {
		fail(w, http.StatusBadRequest, errors.New("invalid setting"))
		return
	}
	s.uiMu.Lock()
	f := s.loadNASFileLocked()
	if f.Protocol != "smb" || f.URL == "" {
		s.uiMu.Unlock()
		fail(w, http.StatusConflict, errors.New("no NAS is set up"))
		return
	}
	f.Enabled = val == "1"
	err := s.saveNASFileLocked(f)
	s.uiMu.Unlock()
	if err != nil {
		fail(w, http.StatusInternalServerError, errors.New("could not save the NAS setting"))
		return
	}
	s.uiStatus(w, r)
}

// webdavMu guards webdavConns; kept beside the Server fields it serves.
type webdavState struct {
	webdavMu    sync.Mutex
	webdavConns map[string]*webdavConn
}

// omniNASPlaylists are OmniSource's [NAS] lists: none for a WebDAV server in the
// NAS slot, which the WebDAV source lists.
func (s *Server) omniNASPlaylists(ctx context.Context) ([]providers.Playlist, *providers.SelfHostedProvider) {
	if s.loadNASConfig().Protocol == webdavScope {
		return nil, nil
	}
	return s.nasPlaylists(ctx)
}

// webdavListTimeout bounds one server's folder listing, so a slow server shows a
// row saying so instead of holding up the others.
const webdavListTimeout = 10 * time.Second

// webdavKept are the IDs of the servers set up (on or off): what their rows,
// Stems entries and searches may still name.
func (s *Server) webdavKept() map[string]bool {
	s.uiMu.Lock()
	f := s.loadNASFileLocked()
	s.uiMu.Unlock()
	keep := map[string]bool{}
	for _, e := range webdavEntries(f) {
		keep[e.ID] = true
	}
	return keep
}

// webdavPlaylistsPage is the WebDAV source: every enabled server's folders, each
// titled with its server's name ("[Studio] House") the way OmniSource names each
// source's lists. The servers are asked at once.
func (g *tidalGateway) webdavPlaylistsPage(w http.ResponseWriter, r *http.Request) {
	favorites := strings.HasSuffix(r.URL.Path, "/favorites/playlists")
	items := []any{}
	add := func(v map[string]any) {
		if favorites {
			flat := map[string]any{"item": v}
			for k, f := range v {
				flat[k] = f
			}
			v = flat
		}
		items = append(items, v)
	}
	if n := len(g.s.stemmed(webdavScope)); n > 0 {
		add(playlistJSON(stemsListID(webdavScope), "Stems", "", n))
	}
	servers := g.s.webdavServers()
	if len(servers) == 0 {
		add(playlistJSON(hintPlaylist, sourceFailedTitle(webdavScope, errors.New("not configured")), "", 0))
		respond(w, page(r, 100, items))
		return
	}
	lists := make([][]providers.Playlist, len(servers))
	failed := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, e := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), webdavListTimeout)
			defer cancel()
			lists[i], failed[i] = g.s.webdavProvider(e).Playlists(ctx)
		}()
	}
	wg.Wait()
	for i, e := range servers {
		if failed[i] != nil && len(lists[i]) == 0 {
			ref := playlistRef{provider: listNoticeProvider, list: providers.Playlist{ID: webdavScope + "-" + e.ID, Title: "[" + e.Name + "] " + sourceFailedTitle(webdavScope, failed[i])}}
			add(g.refJSON(g.addPlaylist(ref), ref))
			continue
		}
		for _, pl := range lists[i] {
			pl.ID = webdavTrackID(e.ID, pl.ID)
			pl.Title = "[" + e.Name + "] " + pl.Title
			ref := playlistRef{session: webdavScope, provider: webdavScope, list: pl}
			add(g.refJSON(g.addPlaylist(ref), ref))
		}
	}
	respond(w, page(r, 100, items))
}

// webdavTracks lists one folder of the server the list names. A list of a server
// that is gone, disabled or switched off answers 404.
func (g *tidalGateway) webdavTracks(w http.ResponseWriter, r *http.Request, id string, ref playlistRef, role, order string) {
	server, raw, _ := webdavSplit(ref.list.ID)
	var e webdavEntry
	for _, v := range g.s.webdavServers() {
		if v.ID == server {
			e = v
		}
	}
	if e.ID == "" {
		err := errors.New("webdav server is not enabled")
		g.logPlaylistOpenFailure(r, http.StatusNotFound, id, &ref, err)
		fail(w, http.StatusNotFound, err)
		return
	}
	tracks, err := g.s.webdavProvider(e).PlaylistTracks(r.Context(), raw)
	if err != nil {
		g.logPlaylistOpenFailure(r, http.StatusBadGateway, id, &ref, err)
		fail(w, http.StatusBadGateway, err)
		return
	}
	if len(tracks) > maxPlaylistTracks {
		err := errors.New("playlist too large")
		g.logPlaylistOpenFailure(r, http.StatusBadRequest, id, &ref, err)
		fail(w, http.StatusBadRequest, err)
		return
	}
	tracks = webdavTracksOf(e, tracks) // named before tempo and ratings are looked up by ID
	g.s.cfg.BPM.Enrich(r.Context(), tracks)
	g.enrichRatingsFromDB(r.Context(), tracks)
	refs := webdavRefs(tracks)
	if role != "" && role != "default" {
		sortStreamingTracks(refs, role, order)
	}
	respond(w, page(r, 500, g.refItems(refs)))
}

// webdavTracksOf are a server's tracks as its rows carry them: the ID names the
// server (so tempo, ratings, downloads and Stems never cross to another server
// with the same folders), the album column shows its name.
func webdavTracksOf(e webdavEntry, tracks []providers.Track) []providers.Track {
	out := make([]providers.Track, len(tracks))
	for i, t := range tracks {
		t.ID = webdavTrackID(e.ID, t.ID)
		t.Album = e.Name
		out[i] = t
	}
	return out
}

func webdavRefs(tracks []providers.Track) []trackRef {
	refs := make([]trackRef, 0, len(tracks))
	for _, t := range tracks {
		refs = append(refs, trackRef{track: t, session: webdavScope})
	}
	return refs
}

// webdavSearchRefs searches every enabled server at once, each within the NAS
// search's time budget (searchTimeout, through searchTracks and its cache under
// the server's own key): a slow server adds nothing instead of holding up the
// others, and a removed server's results are never asked for again.
func (g *tidalGateway) webdavSearchRefs(ctx context.Context, query string) []trackRef {
	servers := g.s.webdavServers()
	found := each(ctx, servers, func(ctx context.Context, e webdavEntry) ([]providers.Track, error) {
		return g.searchTracks(ctx, webdavScope, webdavScope+":"+e.ID, g.s.webdavProvider(e), query)
	})
	var refs []trackRef
	for i, e := range servers {
		tracks := webdavTracksOf(e, found[i])
		g.s.cfg.BPM.Enrich(ctx, tracks)
		refs = append(refs, webdavRefs(tracks)...)
	}
	return refs
}
