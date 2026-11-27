package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// Server sources (maintainer, 2026-11-27): WebDAV servers and NAS (SMB) servers,
// several of each. Settings -> Services switches each kind; the DJ adds, edits and
// removes servers in the QR panel (on the phone page, or typed on the deck), names
// them, and every enabled server is its own entry in Sources under its name.
//
// The servers live in nas.json, the owner settings file and the only place their
// passwords are kept. The file's top level is the NAS slot the console's "Network
// source" card edits; a server there stays there: a released deck's WebDAV server
// is WebDAV server 1 and its SMB NAS is NAS server 1, with nothing to re-enter.
// Servers added on the deck or the phone go to the file's "webdav" and "smb" lists.
//
// The SMB server in the slot keeps the NAS source of #335 (Sources ID "nas", its
// rows under the NAS generation and root tags). Every other server's rows name it
// in their IDs ("<server id>:<path>", the server id being the #335 root identity of
// its address), so two servers with the same folders never share a row, a
// download, a tempo or a Stems entry, and a changed address leaves the old rows
// unresolved: they answer 404 and are browsed again (maintainer rule, 2026-11-27).
const webdavScope = "webdav"

// maxServers bounds the servers of one kind: what one search fans out to.
const maxServers = 16

// serverListTimeout bounds one server's folder listing.
const serverListTimeout = 10 * time.Second

// serverKind is WebDAV or NAS.
type serverKind struct {
	scope    string // the Services switch key, the Sources ID prefix, the QR panel and phone page
	protocol string // the provider's protocol and the rows' provider
}

var (
	kindWebDAV  = serverKind{scope: webdavScope, protocol: "webdav"}
	kindNAS     = serverKind{scope: nasScope, protocol: "smb"}
	serverKinds = []serverKind{kindWebDAV, kindNAS}
)

func kindOf(scope string) (serverKind, bool) {
	for _, k := range serverKinds {
		if k.scope == scope {
			return k, true
		}
	}
	return serverKind{}, false
}

func kindOfProvider(provider string) (serverKind, bool) {
	for _, k := range serverKinds {
		if k.protocol == provider {
			return k, true
		}
	}
	return serverKind{}, false
}

// savedServer is one server in nas.json's "webdav" or "smb" list.
type savedServer struct {
	Name     string `json:"name,omitempty"`
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Enabled  bool   `json:"enabled"`
}

// nasFile is nas.json: the NAS slot as before (its fields at the top level), the
// DJ's name for the server in that slot, and the two lists.
type nasFile struct {
	providers.NASConfig
	Name   string        `json:"name,omitempty"`
	WebDAV []savedServer `json:"webdav,omitempty"`
	SMB    []savedServer `json:"smb,omitempty"`
}

func (f *nasFile) list(k serverKind) *[]savedServer {
	if k == kindWebDAV {
		return &f.WebDAV
	}
	return &f.SMB
}

// serverEntry is one server of a kind.
type serverEntry struct {
	kind serverKind
	ID   string // the #335 root identity of its address
	Name string // the DJ's name, else the host
	cfg  providers.NASConfig
	slot bool // in the NAS slot (the released place); otherwise the list's idx
	idx  int
}

// Scope is the entry's Sources ID: "nas" for the SMB server in the slot (#335's NAS
// source), "<kind>:<id>" for every other server.
func (e serverEntry) Scope() string {
	if !e.qualified() {
		return nasScope
	}
	return e.kind.scope + ":" + e.ID
}

// qualified: its rows name the server in their IDs (all but the slot's SMB server).
func (e serverEntry) qualified() bool { return !(e.slot && e.kind == kindNAS) }

// serverView is a server as the deck and the phone page see it: never its password.
type serverView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Username    string `json:"username,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	Scope       string `json:"scope"`
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
	if f.Protocol == "" && f.URL == "" && len(f.WebDAV) == 0 && len(f.SMB) == 0 {
		if err := os.Remove(s.nasConfigPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return providers.SaveJSON(s.nasConfigPath(), f)
}

func serverRoot(k serverKind, raw string) string {
	return nasRootID(providers.NASConfig{Protocol: k.protocol, URL: strings.TrimSpace(raw)})
}

// serverHost is a server's name when the DJ gave none.
func serverHost(raw string) string {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimSpace(raw)
}

func serverEntries(k serverKind, f nasFile) []serverEntry {
	var out []serverEntry
	add := func(e serverEntry) {
		if strings.TrimSpace(e.cfg.URL) == "" || slices.ContainsFunc(out, func(v serverEntry) bool { return v.ID == e.ID }) {
			return
		}
		if e.Name == "" {
			e.Name = serverHost(e.cfg.URL)
		}
		out = append(out, e)
	}
	if f.Protocol == k.protocol {
		add(serverEntry{kind: k, ID: serverRoot(k, f.URL), Name: strings.TrimSpace(f.Name), cfg: f.NASConfig, slot: true, idx: -1})
	}
	for i, v := range *f.list(k) {
		cfg := providers.NASConfig{Protocol: k.protocol, URL: v.URL, Username: v.Username, Password: v.Password, Enabled: v.Enabled}
		add(serverEntry{kind: k, ID: serverRoot(k, v.URL), Name: strings.TrimSpace(v.Name), cfg: cfg, idx: i})
	}
	return out
}

func serverViews(k serverKind, f nasFile) []serverView {
	views := []serverView{}
	for _, e := range serverEntries(k, f) {
		views = append(views, serverView{ID: e.ID, Name: e.Name, URL: toNASConfigView(e.cfg).URL, Username: e.cfg.Username, HasPassword: e.cfg.Password != "", Scope: e.Scope()})
	}
	return views
}

// serverTrackID names a server's track or folder in a row: "<server id>:<path>".
func serverTrackID(server, raw string) string { return server + ":" + raw }

// splitServerID undoes serverTrackID. A row of the NAS slot (a bare path) is none.
func splitServerID(id string) (server, raw string, ok bool) {
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

// serverScopeOf parses a server's Sources ID ("<kind>:<id>").
func serverScopeOf(scope string) (serverKind, string, bool) {
	prefix, id, ok := strings.Cut(scope, ":")
	k, known := kindOf(prefix)
	return k, id, ok && known && isRootID(id)
}

// isServerScope: a server's Sources ID ("<kind>:<id>").
func isServerScope(scope string) bool {
	_, _, ok := serverScopeOf(scope)
	return ok
}

// isNASTrack: a track of the NAS slot (SMB, Subsonic, Plex, or a released WebDAV
// row), which the NAS generation and root guard (#335). A server's row names its
// server in its ID instead.
func isNASTrack(provider, id string) bool {
	if _, ok := kindOfProvider(provider); ok {
		if _, _, named := splitServerID(id); named {
			return false
		}
	}
	return isNASProvider(provider)
}

// rowServer is the kind and server of a row that names its server.
func rowServer(t providers.Track) (serverKind, string, bool) {
	k, ok := kindOfProvider(t.Provider)
	if !ok {
		return serverKind{}, "", false
	}
	server, _, named := splitServerID(t.ID)
	return k, server, named
}

// rowScope is the Sources ID of a row that names its server: its Stems list.
func rowScope(t providers.Track) string {
	if k, server, ok := rowServer(t); ok {
		return k.scope + ":" + server
	}
	return ""
}

// serversOn: the kind's row in Settings -> Services; until the DJ uses it, on while
// a server of that kind is set up.
func serversOn(k serverKind, v uiSettings, f nasFile) bool {
	sw := v.WebDAVSwitch
	if k == kindNAS {
		sw = v.NASSwitch
	}
	return sw == "1" || (sw == "" && len(serverEntries(k, f)) > 0)
}

// servers are a kind's servers that browse, play and are searched: the enabled
// ones, and none while the kind is off.
func (s *Server) servers(k serverKind) []serverEntry {
	s.uiMu.Lock()
	f := s.loadNASFileLocked()
	on := serversOn(k, s.loadUI(), f)
	s.uiMu.Unlock()
	if !on {
		return nil
	}
	return slices.DeleteFunc(serverEntries(k, f), func(e serverEntry) bool { return !nasConfigReady(e.cfg) })
}

// serverByScope is the enabled server a Sources ID names.
func (s *Server) serverByScope(scope string) (serverEntry, bool) {
	k, id, ok := serverScopeOf(scope)
	if !ok {
		return serverEntry{}, false
	}
	for _, e := range s.servers(k) {
		if e.ID == id && e.qualified() {
			return e, true
		}
	}
	return serverEntry{}, false
}

// serverLookup is the server a row names, or nil when it is gone, disabled or its
// kind is off: such a row answers 404 and is browsed again.
func (s *Server) serverLookup(k serverKind, id string) *providers.SelfHostedProvider {
	for _, e := range s.servers(k) {
		if e.ID == id && e.qualified() {
			return s.serverProvider(e)
		}
	}
	return nil
}

type serverConn struct {
	cfg providers.NASConfig
	p   *providers.SelfHostedProvider
}

// serverState keeps one provider per server: its track cache is what a search of
// that server looks through.
type serverState struct {
	serverMu    sync.Mutex
	serverConns map[string]*serverConn
}

func (s *Server) serverProvider(e serverEntry) *providers.SelfHostedProvider {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	if c := s.serverConns[e.ID]; c != nil && c.cfg == e.cfg {
		return c.p
	}
	if s.serverConns == nil {
		s.serverConns = map[string]*serverConn{}
	}
	p := providers.NewSelfHostedProvider(e.cfg, s.nasHTTPClient())
	s.serverConns[e.ID] = &serverConn{e.cfg, p}
	return p
}

// serverChange is one add, edit or remove, from the deck or the phone page.
type serverChange struct {
	Action   string `json:"action"` // add, edit, remove
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

var (
	errWebDAVURL       = errors.New("enter the server address with http:// or https://")
	errNASURL          = errors.New("enter the server address with smb://")
	errServerDuplicate = errors.New("this server is already added")
	errServerFull      = errors.New("no more servers can be added")
	errServerGoneList  = errors.New("this server is no longer in the list")
	// errServerGone: a row's server was removed, moved to another address, or its
	// kind is off. The row answers 404 and is browsed again.
	errServerGone = errors.New("the server of this row is not set up or not on")
)

func serverChangeStatus(err error) int {
	switch {
	case errors.Is(err, errServerGoneList):
		return http.StatusNotFound
	case errors.Is(err, errServerDuplicate), errors.Is(err, errServerFull):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// validServerURL: WebDAV is http(s); NAS is smb with a share.
func validServerURL(k serverKind, raw string) error {
	u, err := url.Parse(raw)
	bad := errWebDAVURL
	if k == kindNAS {
		bad = errNASURL
	}
	if err != nil || validateNASURL(raw) != nil {
		return bad
	}
	if (k == kindWebDAV && u.Scheme != "http" && u.Scheme != "https") || (k == kindNAS && u.Scheme != "smb") {
		return bad
	}
	return nil
}

// applyServer makes one change to a kind's servers in nas.json. An empty password on
// an edit keeps the saved one. The rows and Stems entries of a server that is
// removed or moved to another address go with it; a change of the slot's SMB server
// resets the NAS of #335 (resetNASIfMoved).
func (s *Server) applyServer(k serverKind, c serverChange) ([]serverView, error) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	defer s.resetNASIfMoved(s.loadNASConfigLocked())
	f := s.loadNASFileLocked()
	entries := serverEntries(k, f)
	all := append(serverEntries(kindWebDAV, f), serverEntries(kindNAS, f)...)
	var cur *serverEntry
	if c.Action != "add" {
		for i := range entries {
			if entries[i].ID == c.ID {
				cur = &entries[i]
			}
		}
		if cur == nil {
			return nil, errServerGoneList
		}
	}
	list := f.list(k)
	if c.Action == "remove" {
		if cur.slot {
			f.NASConfig, f.Name = providers.NASConfig{}, ""
		} else {
			*list = slices.Delete(*list, cur.idx, cur.idx+1)
		}
		if err := s.saveNASFileLocked(f); err != nil {
			return nil, err
		}
		s.forgetServersLocked(f)
		return serverViews(k, f), nil
	}
	if c.Action != "add" && c.Action != "edit" {
		return nil, errors.New("unknown action")
	}
	raw := strings.TrimSpace(c.URL)
	if err := validServerURL(k, raw); err != nil {
		return nil, err
	}
	id := serverRoot(k, raw)
	if slices.ContainsFunc(all, func(e serverEntry) bool { return e.ID == id && (cur == nil || e.ID != cur.ID) }) {
		return nil, errServerDuplicate
	}
	name, user := strings.TrimSpace(c.Name), strings.TrimSpace(c.Username)
	if r := []rune(name); len(r) > 64 {
		name = string(r[:64])
	}
	password := c.Password
	switch {
	case c.Action == "add":
		if len(entries) >= maxServers {
			return nil, errServerFull
		}
		*list = append(*list, savedServer{Name: name, URL: raw, Username: user, Password: password, Enabled: true})
	case cur.slot:
		if password == "" || password == "***" {
			password = f.Password
		}
		f.NASConfig = providers.NASConfig{Protocol: k.protocol, URL: raw, Username: user, Password: password, Token: f.Token, Enabled: true}
		f.Name = name
	default:
		if password == "" || password == "***" {
			password = (*list)[cur.idx].Password
		}
		(*list)[cur.idx] = savedServer{Name: name, URL: raw, Username: user, Password: password, Enabled: true}
	}
	if err := s.saveNASFileLocked(f); err != nil {
		return nil, err
	}
	s.forgetServersLocked(f)
	return serverViews(k, f), nil
}

// forgetServersLocked drops what belonged to servers that are no longer set up
// (removed, or moved to another address): their gateway rows and lists, their
// Stems entries and their connections. Caller holds uiMu.
func (s *Server) forgetServersLocked(f nasFile) {
	keep := keptServers(f)
	s.serverMu.Lock()
	for id := range s.serverConns {
		if !keep[id] {
			delete(s.serverConns, id)
		}
	}
	s.serverMu.Unlock()
	s.stemsMu.Lock()
	s.dropServerStemsLocked(keep)
	s.stemsMu.Unlock()
	s.tidal.dropServers(keep)
}

func keptServers(f nasFile) map[string]bool {
	keep := map[string]bool{}
	for _, k := range serverKinds {
		for _, e := range serverEntries(k, f) {
			keep[e.ID] = true
		}
	}
	return keep
}

// serversKept are the IDs of the servers set up (on or off): what rows, Stems
// entries and searches may still name.
func (s *Server) serversKept() map[string]bool {
	s.uiMu.Lock()
	f := s.loadNASFileLocked()
	s.uiMu.Unlock()
	return keptServers(f)
}

// handleServers is the deck's list (GET) and change (POST) of a kind's servers:
// /v1/enginex/servers/{webdav,nas}.
func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	k, ok := kindOf(r.PathValue("kind"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("unknown kind of server"))
		return
	}
	if r.Method == http.MethodGet {
		s.uiMu.Lock()
		f := s.loadNASFileLocked()
		s.uiMu.Unlock()
		respond(w, map[string]any{"servers": serverViews(k, f)})
		return
	}
	var c serverChange
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		return
	}
	views, err := s.applyServer(k, c)
	if err != nil {
		fail(w, serverChangeStatus(err), err)
		return
	}
	respond(w, map[string]any{"servers": views})
}

// serversLoginStart opens the phone page where a kind's servers are added, edited
// and removed, as the QR login of the other services does. The QR code holds the
// page's address with a one-time token, never a password, and never the relay's (a
// third party).
func (s *Server) serversLoginStart(w http.ResponseWriter, k serverKind) {
	flow := &loginFlow{Provider: k.scope, Status: "waiting", token: providers.RandomID(), expires: time.Now().Add(loginLifetime)}
	s.uiMu.Lock()
	lang := s.loadUI().Language
	s.uiMu.Unlock()
	q := "?t=" + url.QueryEscape(flow.token) + "&lang=" + url.QueryEscape(lang)
	if pub := s.PublicTunnelURL(); pub != "" {
		flow.Target = strings.TrimRight(pub, "/") + "/m/" + k.scope + q
	} else {
		flow.Target = "http://" + lanHost() + ":8080/m/" + k.scope + q
	}
	s.mu.Lock()
	s.logins[k.scope] = flow
	s.mu.Unlock()
	respond(w, flow)
}

// serversLoginStatus: done once the phone page saved a change (its token is spent).
func (s *Server) serversLoginStatus(w http.ResponseWriter, flow *loginFlow) {
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

// serversPhonePost is one add, edit or remove from the phone page. A change spends
// the token, as a phone login does; the deck closes its QR code.
func (s *Server) serversPhonePost(w http.ResponseWriter, r *http.Request, flow *loginFlow, k serverKind) {
	lang := phoneLang(r)
	c := serverChange{Action: r.Form.Get("action"), ID: r.Form.Get("id"), Name: r.Form.Get("name"), URL: r.Form.Get("url"), Username: r.Form.Get("user"), Password: r.Form.Get("password")}
	if _, err := s.applyServer(k, c); err != nil {
		key := map[error]string{errWebDAVURL: "wdBadURL", errNASURL: "nasBadURL", errServerDuplicate: "wdDuplicate", errServerFull: "wdFull", errServerGoneList: "wdGone"}[err]
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

// omniNASPlaylists are OmniSource's [NAS] lists: none for a WebDAV server in the
// NAS slot, which is its own entry in Sources.
func (s *Server) omniNASPlaylists(ctx context.Context) ([]providers.Playlist, *providers.SelfHostedProvider) {
	if s.loadNASConfig().Protocol == webdavScope {
		return nil, nil
	}
	return s.nasPlaylists(ctx)
}

// serverPlaylistsPage is one server's entry in Sources: its Stems list and its
// folders. A server that is gone, disabled or off shows a row saying so.
func (g *tidalGateway) serverPlaylistsPage(w http.ResponseWriter, r *http.Request, scope string) {
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
	if n := len(g.s.stemmed(scope)); n > 0 {
		add(playlistJSON(stemsListID(scope), "Stems", "", n))
	}
	k, _, _ := serverScopeOf(scope)
	e, ok := g.s.serverByScope(scope)
	if !ok {
		add(playlistJSON(hintPlaylist, sourceFailedTitle(k.scope, errors.New("not configured")), "", 0))
		respond(w, page(r, 100, items))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), serverListTimeout)
	lists, err := g.s.serverProvider(e).Playlists(ctx)
	cancel()
	if err != nil && len(lists) == 0 {
		add(playlistJSON(hintPlaylist, sourceFailedTitle(k.scope, err), "", 0))
		respond(w, page(r, 100, items))
		return
	}
	for _, pl := range lists {
		pl.ID = serverTrackID(e.ID, pl.ID)
		ref := playlistRef{session: scope, provider: k.protocol, list: pl}
		add(g.refJSON(g.addPlaylist(ref), ref))
	}
	respond(w, page(r, 100, items))
}

// serverTracks lists one folder of the server the list names. A list of a server
// that is gone, disabled or off answers 404.
func (g *tidalGateway) serverTracks(w http.ResponseWriter, r *http.Request, id string, ref playlistRef, role, order string) {
	server, raw, _ := splitServerID(ref.list.ID)
	k, _ := kindOfProvider(ref.provider)
	var e serverEntry
	for _, v := range g.s.servers(k) {
		if v.ID == server && v.qualified() {
			e = v
		}
	}
	if e.ID == "" {
		err := errServerGone
		g.logPlaylistOpenFailure(r, http.StatusNotFound, id, &ref, err)
		fail(w, http.StatusNotFound, err)
		return
	}
	tracks, err := g.s.serverProvider(e).PlaylistTracks(r.Context(), raw)
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
	tracks = serverTracksOf(e, tracks) // named before tempo and ratings are looked up by ID
	g.s.cfg.BPM.Enrich(r.Context(), tracks)
	g.enrichRatingsFromDB(r.Context(), tracks)
	refs := serverRefs(e.Scope(), tracks)
	if role != "" && role != "default" {
		sortStreamingTracks(refs, role, order)
	}
	respond(w, page(r, 500, g.refItems(refs)))
}

// serverTracksOf are a server's tracks as its rows carry them: the ID names the
// server (so tempo, ratings, downloads and Stems never cross to another server
// with the same folders), the album column shows its name.
func serverTracksOf(e serverEntry, tracks []providers.Track) []providers.Track {
	out := make([]providers.Track, len(tracks))
	for i, t := range tracks {
		t.ID = serverTrackID(e.ID, t.ID)
		t.Album = e.Name
		out[i] = t
	}
	return out
}

func serverRefs(scope string, tracks []providers.Track) []trackRef {
	refs := make([]trackRef, 0, len(tracks))
	for _, t := range tracks {
		refs = append(refs, trackRef{track: t, session: scope})
	}
	return refs
}

// serverSearch searches servers at once, each within the NAS search's time budget
// (searchTimeout, through searchTracks and its cache under the server's own key):
// a slow server adds nothing instead of holding up the others, and a removed
// server's results are never asked for again. The NAS slot's SMB server is searched
// as the NAS source of #335 (its rows stamped with the NAS listing). Every row says
// which server it is from (album). only limits the search to one Sources ID; skip
// leaves one out (the source whose own search already has it).
func (g *tidalGateway) serverSearch(ctx context.Context, query, only, skip string) []trackRef {
	type source struct {
		e       serverEntry
		p       *providers.SelfHostedProvider
		session string
		listing uint64
	}
	var sources []source
	listing := g.s.jobs.NASGen() + 1 // before the NAS settings are read (#335)
	for _, k := range serverKinds {
		for _, e := range g.s.servers(k) {
			scope := e.Scope()
			if (only != "" && scope != only) || scope == skip {
				continue
			}
			if !e.qualified() {
				if p := g.s.ownerSMBSelfHosted(); p != nil {
					sources = append(sources, source{e, p, fmt.Sprintf("%s@%d", nasScope, listing-1), listing})
				}
				continue
			}
			sources = append(sources, source{e: e, p: g.s.serverProvider(e), session: scope})
		}
	}
	found := each(ctx, sources, func(ctx context.Context, v source) ([]providers.Track, error) {
		return g.searchTracks(ctx, v.e.kind.scope, v.session, v.p, query)
	})
	var refs []trackRef
	for i, v := range sources {
		if v.listing != 0 { // the NAS of #335: bare IDs, its listing stamp
			for _, t := range found[i] {
				httpsCover(&t)
				t.Album = v.e.Name
				refs = append(refs, trackRef{track: t, session: nasScope, listing: v.listing})
			}
			continue
		}
		tracks := serverTracksOf(v.e, found[i])
		g.s.cfg.BPM.Enrich(ctx, tracks)
		refs = append(refs, serverRefs(v.e.Scope(), tracks)...)
	}
	return refs
}

// dropServerStemsLocked removes the Stems entries of servers that are gone
// (removed, or moved to another address). A file that does not fully decode is
// left as it is. Caller holds stemsMu.
func (s *Server) dropServerStemsLocked(keep map[string]bool) {
	all, err := s.readStemmed()
	if err != nil {
		return
	}
	changed := false
	for scope, list := range all {
		kept := slices.DeleteFunc(slices.Clone(list), func(e stemmedTrack) bool {
			_, server, ok := rowServer(e.Track)
			return ok && !keep[server]
		})
		if len(kept) != len(list) {
			all[scope], changed = kept, true
		}
	}
	if changed {
		if err := saveState(s.stemsFile(), stripStemsAuth(all)); err != nil {
			log.Printf("webui: servers changed: Stems lists not saved (%T)", err)
		}
	}
}
