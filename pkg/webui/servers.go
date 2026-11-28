package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
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
	// ID is the server's identity, given when it is added or moved to another
	// address (newServerID). A server saved without one (earlier files) is
	// known by its address (serverRoot).
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Enabled  bool   `json:"enabled"`
}

// nasFile is nas.json: the NAS slot as before (its fields at the top level), the
// DJ's name for the server in that slot, its identity, and the two lists.
type nasFile struct {
	providers.NASConfig
	Name string `json:"name,omitempty"`
	// SlotID is the slot server's identity once its address changed in this
	// version (saveNASFileLocked); before that it is known by its address.
	SlotID string        `json:"slotId,omitempty"`
	WebDAV []savedServer `json:"webdav,omitempty"`
	SMB    []savedServer `json:"smb,omitempty"`
}

// newServerID is a new server identity, in the form of a root ID: a server added
// again, or moved to another address, is never the server it was, so nothing an
// earlier one left (a download, a cached search, a row a browse still adds) is
// used for it.
func newServerID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return hex.EncodeToString(b[:])
}

// idOf is a server's identity: the one saved, else its address's.
func idOf(saved string, k serverKind, raw string) string {
	if isRootID(saved) {
		return saved
	}
	return serverRoot(k, raw)
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

// errNASFile: nas.json exists but cannot be read or decoded. Nothing that would write
// it, or clean up after the servers it names, runs then (WebDAV round 4).
var errNASFile = errors.New("the network source settings cannot be read")

// readNASFileLocked is nas.json: an empty file when there is none, an error (errNASFile)
// when it cannot be read or decoded. Writers and clean-ups use it and stop on an
// error, so a file that cannot be read is never overwritten and the state of the
// servers it names is never dropped.
func (s *Server) readNASFileLocked() (nasFile, error) {
	var f nasFile
	b, err := readState(s.nasConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		return nasFile{}, fmt.Errorf("%w (%T)", errNASFile, err)
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
	return f, nil
}

// loadNASFileLocked is nas.json for reading only: no server at all when it cannot be
// read (logged), so nothing of its servers is used then.
func (s *Server) loadNASFileLocked() nasFile {
	f, err := s.readNASFileLocked()
	if err != nil && s.nasFileErrLogged.CompareAndSwap(false, true) {
		log.Printf("webui: %v: no network source is used until it can be read", err)
	}
	if err == nil {
		s.nasFileErrLogged.Store(false)
	}
	return f
}

// saveNASFileLocked writes nas.json, or removes it once it holds nothing. A slot
// that now names another server (another protocol or address, or none) gets a new
// identity: every writer of the slot (the console card, the deck and the phone,
// the mounts page) goes through here. What belonged to a server this save removed
// is dropped (forgetServersLocked). Caller holds uiMu.
func (s *Server) saveNASFileLocked(f *nasFile) error {
	old, err := s.readNASFileLocked()
	if err != nil {
		return err // a file that cannot be read is never overwritten
	}
	if nasTarget(old.NASConfig) != nasTarget(f.NASConfig) {
		f.SlotID = ""
		if _, ok := kindOfProvider(f.Protocol); ok && strings.TrimSpace(f.URL) != "" {
			f.SlotID = newServerID()
		}
	}
	if f.Protocol == "" && f.URL == "" && len(f.WebDAV) == 0 && len(f.SMB) == 0 {
		if err := os.Remove(s.nasConfigPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := providers.SaveJSON(s.nasConfigPath(), f); err != nil {
		return err
	}
	s.forgetServersLocked(old, *f)
	return nil
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
		root := serverRoot(k, e.cfg.URL)
		if strings.TrimSpace(e.cfg.URL) == "" || slices.ContainsFunc(out, func(v serverEntry) bool { return v.ID == e.ID || serverRoot(k, v.cfg.URL) == root }) {
			return
		}
		if e.Name == "" {
			e.Name = serverHost(e.cfg.URL)
		}
		out = append(out, e)
	}
	if f.Protocol == k.protocol {
		add(serverEntry{kind: k, ID: idOf(f.SlotID, k, f.URL), Name: strings.TrimSpace(f.Name), cfg: f.NASConfig, slot: true, idx: -1})
	}
	for i, v := range *f.list(k) {
		cfg := providers.NASConfig{Protocol: k.protocol, URL: v.URL, Username: v.Username, Password: v.Password, Enabled: v.Enabled}
		add(serverEntry{kind: k, ID: idOf(v.ID, k, v.URL), Name: strings.TrimSpace(v.Name), cfg: cfg, idx: i})
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
	// The NAS slot is off while its root is not known (nas.json could not be read at
	// start-up): its saved rows may belong to another root.
	unknown := s.slotUnknown.Load()
	return slices.DeleteFunc(serverEntries(k, f), func(e serverEntry) bool { return !nasConfigReady(e.cfg) || (unknown && e.slot) })
}

// connectedServers are the servers that are searched now (as servers), each with its
// connection. The settings are read and the connections made in one step under uiMu,
// the lock every save holds while it cancels the connections of the servers it changed
// (forgetServersLocked). So a connection that is still live later says its server is
// still set up and on as it was here, and nobody has to read the settings again to know.
func (s *Server) connectedServers(want func(serverEntry) bool) (entries []serverEntry, conns []*serverConn) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	f, v, unknown := s.loadNASFileLocked(), s.loadUI(), s.slotUnknown.Load()
	for _, k := range serverKinds {
		if !serversOn(k, v, f) {
			continue
		}
		for _, e := range serverEntries(k, f) {
			if nasConfigReady(e.cfg) && !(unknown && e.slot) && want(e) {
				entries, conns = append(entries, e), append(conns, s.serverConnOf(e))
			}
		}
	}
	return entries, conns
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
	cfg  providers.NASConfig
	kind serverKind
	p    *providers.SelfHostedProvider
	// ctx lives as long as the connection: every request to the server is tied
	// to it, so dropping the connection (the server removed or moved, its kind
	// switched off) cancels what is still running.
	ctx    context.Context
	cancel context.CancelFunc
}

// serverState keeps one provider per server: its track cache is what a search of
// that server looks through.
type serverState struct {
	serverMu    sync.Mutex
	serverConns map[string]*serverConn
}

func (s *Server) serverProvider(e serverEntry) *providers.SelfHostedProvider {
	return s.serverConnOf(e).p
}

func (s *Server) serverConnOf(e serverEntry) *serverConn {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	if c := s.serverConns[e.ID]; c != nil && c.cfg == e.cfg {
		return c
	}
	if c := s.serverConns[e.ID]; c != nil {
		c.cancel()
	}
	if s.serverConns == nil {
		s.serverConns = map[string]*serverConn{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &serverConn{cfg: e.cfg, kind: e.kind, ctx: ctx, cancel: cancel}
	hc := s.nasHTTPClient()
	bound := http.Client{Timeout: 30 * time.Second} // the provider's own default
	if hc != nil {
		bound = *hc
	}
	if e.kind.protocol == "smb" {
		// SMB dials through the client's transport (its address policy): every
		// connection it opens closes when this one is dropped (WebDAV round 4).
		bound.Transport = smbBoundTransport(ctx, transportOf(hc))
	} else {
		bound.Transport = serverTransport{ctx: ctx, next: transportOf(hc)}
	}
	c.p = providers.NewSelfHostedProvider(e.cfg, &bound)
	s.serverConns[e.ID] = c
	return c
}

// smbBoundTransport is next (an *http.Transport, else the default one) whose dials are
// tied to ctx, the connection of an SMB server: the SMB client dials through it (its
// address policy). None starts once ctx is done, and each connection it made closes
// when ctx is done, which ends a browse, search or download running on it.
func smbBoundTransport(ctx context.Context, next http.RoundTripper) *http.Transport {
	base, ok := next.(*http.Transport)
	if !ok || base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	t := base.Clone()
	dial := t.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	t.DialContext = func(dctx context.Context, network, addr string) (net.Conn, error) {
		if ctx.Err() != nil {
			return nil, errServerGone
		}
		dctx, cancel := context.WithCancel(dctx)
		stopDial := context.AfterFunc(ctx, cancel)
		conn, err := dial(dctx, network, addr)
		stopDial()
		cancel()
		if err != nil {
			return nil, err
		}
		return &boundConn{Conn: conn, stop: context.AfterFunc(ctx, func() { conn.Close() })}, nil
	}
	return t
}

// boundConn is a connection that smbBoundTransport closes when its server's
// connection is dropped.
type boundConn struct {
	net.Conn
	stop func() bool
}

func (c *boundConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

// dropServerConns cancels and forgets the connections drop picks.
func (s *Server) dropServerConns(drop func(id string, k serverKind) bool) {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	for id, c := range s.serverConns {
		if drop(id, c.kind) {
			c.cancel()
			delete(s.serverConns, id)
		}
	}
}

func transportOf(c *http.Client) http.RoundTripper {
	if c != nil && c.Transport != nil {
		return c.Transport
	}
	return http.DefaultTransport
}

// serverTransport ends a request when its server's connection is dropped, as well
// as when the caller gives up.
type serverTransport struct {
	ctx  context.Context
	next http.RoundTripper
}

func (t serverTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.ctx.Err() != nil {
		return nil, errServerGone
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	resp, err := t.next.RoundTrip(r.WithContext(ctx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	resp.Body = &serverBody{ReadCloser: resp.Body, done: func() { stop(); cancel() }}
	return resp, nil
}

// serverBody keeps a response's request alive until its body is closed.
type serverBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *serverBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
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
	errWebDAVNotDAV    = errors.New("this address is not a WebDAV server")
	errServerDuplicate = errors.New("this server is already added")
	errServerFull      = errors.New("no more servers can be added")
	errServerGoneList  = errors.New("this server is no longer in the list")
	// errServerGone: a row's server was removed, moved to another address, or its
	// kind is off. The row answers 404 and is browsed again.
	errServerGone = errors.New("the server of this row is not set up or not on")
)

const webdavSaveProbeTimeout = 3 * time.Second

func sanitizeLogErr(err error) string {
	err = providers.RedactURLError(err)
	errText := fmt.Sprint(err)
	if strings.ContainsAny(errText, "\r\n\x1b") {
		return fmt.Sprintf("%q", errText)
	}
	return errText
}

func serverChangeStatus(err error) int {
	switch {
	case errors.Is(err, errServerGoneList):
		return http.StatusNotFound
	case errors.Is(err, errServerDuplicate), errors.Is(err, errServerFull):
		return http.StatusConflict
	case errors.Is(err, errWebDAVNotDAV):
		return http.StatusUnprocessableEntity
	case errors.Is(err, errNASFile):
		return http.StatusInternalServerError
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

func webdavFallbackURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	p := strings.TrimRight(u.Path, "/")
	if strings.EqualFold(path.Base(p), "dav") {
		return "", false
	}
	u.Path = p + "/dav/"
	u.RawPath = ""
	return u.String(), true
}

func (s *Server) resolveWebDAVSaveURL(ctx context.Context, cfg providers.NASConfig) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, webdavSaveProbeTimeout)
	defer cancel()

	client := s.nasHTTPClient()
	switch providers.NewSelfHostedProvider(cfg, client).ProbeWebDAV(probeCtx) {
	case providers.WebDAVProbeOK, providers.WebDAVProbeUnreachable:
		return cfg.URL, nil
	case providers.WebDAVProbeWebPage:
		davURL, ok := webdavFallbackURL(cfg.URL)
		if !ok || validServerURL(kindWebDAV, davURL) != nil {
			return "", errWebDAVNotDAV
		}
		davCfg := cfg
		davCfg.URL = davURL
		if providers.NewSelfHostedProvider(davCfg, client).ProbeWebDAVStrict207(probeCtx) {
			return davURL, nil
		}
		return "", errWebDAVNotDAV
	default:
		return cfg.URL, nil
	}
}

func findServerEntry(entries []serverEntry, id string) *serverEntry {
	for i := range entries {
		if entries[i].ID == id {
			return &entries[i]
		}
	}
	return nil
}

// applyServer makes one change to a kind's servers in nas.json. An empty password on
// an edit keeps the saved one. The rows and Stems entries of a server that is
// removed or moved to another address go with it; a change of the slot's SMB server
// resets the NAS of #335 (resetNASIfMoved).
func (s *Server) applyServer(ctx context.Context, k serverKind, c serverChange) ([]serverView, error) {
	if c.Action == "remove" {
		s.uiMu.Lock()
		defer s.uiMu.Unlock()
		f, err := s.readNASFileLocked()
		if err != nil {
			return nil, err // never overwrite a file that cannot be read
		}
		defer s.resetNASIfMoved(f.NASConfig)
		cur := findServerEntry(serverEntries(k, f), c.ID)
		if cur == nil {
			return nil, errServerGoneList
		}
		list := f.list(k)
		if cur.slot {
			f.NASConfig, f.Name = providers.NASConfig{}, ""
		} else {
			*list = slices.Delete(*list, cur.idx, cur.idx+1)
		}
		if err := s.saveNASFileLocked(&f); err != nil {
			return nil, err
		}
		return serverViews(k, f), nil
	}
	if c.Action != "add" && c.Action != "edit" {
		return nil, errors.New("unknown action")
	}

	inputRaw := strings.TrimSpace(c.URL)
	if err := validServerURL(k, inputRaw); err != nil {
		return nil, err
	}
	name, user := strings.TrimSpace(c.Name), strings.TrimSpace(c.Username)
	if r := []rune(name); len(r) > 64 {
		name = string(r[:64])
	}
	probePass := c.Password
	if probePass == "***" {
		probePass = ""
	}

	raw := inputRaw
	if k == kindWebDAV {
		s.uiMu.Lock()
		f, err := s.readNASFileLocked()
		if err != nil {
			s.uiMu.Unlock()
			return nil, err
		}
		entries := serverEntries(k, f)
		all := append(serverEntries(kindWebDAV, f), serverEntries(kindNAS, f)...)
		var cur *serverEntry
		if c.Action == "edit" {
			cur = findServerEntry(entries, c.ID)
			if cur == nil {
				s.uiMu.Unlock()
				return nil, errServerGoneList
			}
		} else {
			if len(entries) >= maxServers {
				s.uiMu.Unlock()
				return nil, errServerFull
			}
			inRoot := serverRoot(k, inputRaw)
			if slices.ContainsFunc(all, func(e serverEntry) bool { return serverRoot(e.kind, e.cfg.URL) == inRoot }) {
				s.uiMu.Unlock()
				return nil, errServerDuplicate
			}
		}
		probeCfg := providers.NASConfig{
			Protocol: k.protocol,
			URL:      inputRaw,
			Username: user,
			Password: probePass,
			Enabled:  true,
		}
		// Reuse saved credentials only when probing the same configured address.
		if cur != nil && serverRoot(k, cur.cfg.URL) == serverRoot(k, inputRaw) && (c.Password == "" || c.Password == "***") {
			probeCfg.Password = cur.cfg.Password
			if cur.slot {
				probeCfg.Token = f.Token
			}
		}
		s.uiMu.Unlock()

		resolvedRaw, err := s.resolveWebDAVSaveURL(ctx, probeCfg)
		if err != nil {
			return nil, err
		}
		if err := validServerURL(k, resolvedRaw); err != nil {
			return nil, err
		}
		raw = resolvedRaw
	}

	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	f, err := s.readNASFileLocked()
	if err != nil {
		return nil, err // never overwrite a file that cannot be read
	}
	defer s.resetNASIfMoved(f.NASConfig)
	entries := serverEntries(k, f)
	all := append(serverEntries(kindWebDAV, f), serverEntries(kindNAS, f)...)
	var cur *serverEntry
	if c.Action == "edit" {
		cur = findServerEntry(entries, c.ID)
		if cur == nil {
			return nil, errServerGoneList
		}
	}
	list := f.list(k)
	root := serverRoot(k, raw)
	if slices.ContainsFunc(all, func(e serverEntry) bool {
		return serverRoot(e.kind, e.cfg.URL) == root && (cur == nil || e.ID != cur.ID)
	}) {
		return nil, errServerDuplicate
	}
	// An empty password field keeps the saved password (and the slot's token) only
	// for the same address (or same-origin auto-/dav/ upgrade from that address): a server
	// moved to another address never takes the old one's credentials along.
	password, token := c.Password, ""
	same := cur != nil && serverRoot(k, cur.cfg.URL) == root
	sameAutoDAV := cur != nil && k == kindWebDAV && raw != inputRaw && serverRoot(k, cur.cfg.URL) == serverRoot(k, inputRaw) && providers.SameOrigin(cur.cfg.URL, raw)
	keep := (same || sameAutoDAV) && (password == "" || password == "***")
	if password == "***" {
		password = ""
	}
	switch {
	case c.Action == "add":
		if len(entries) >= maxServers {
			return nil, errServerFull
		}
		*list = append(*list, savedServer{ID: newServerID(), Name: name, URL: raw, Username: user, Password: password, Enabled: true})
	case cur.slot:
		if keep {
			password = f.Password
		}
		if same || sameAutoDAV {
			token = f.Token
		}
		f.NASConfig = providers.NASConfig{Protocol: k.protocol, URL: raw, Username: user, Password: password, Token: token, Enabled: true}
		f.Name = name // a new address gets a new identity (saveNASFileLocked)
	default:
		v := (*list)[cur.idx]
		if keep {
			password = v.Password
		}
		id := v.ID
		if !same {
			id = newServerID()
		}
		(*list)[cur.idx] = savedServer{ID: id, Name: name, URL: raw, Username: user, Password: password, Enabled: true}
	}
	if err := s.saveNASFileLocked(&f); err != nil {
		return nil, err
	}
	return serverViews(k, f), nil
}

// forgetServersLocked drops what belonged to the servers a save removed: those old
// names and now does not (a server removed, or moved to another address, which is a
// new identity). Their connections (which cancels their requests still running),
// gateway rows and lists, Stems entries and downloads go. It is the only place that
// deletes a server's state: start-up deletes none, and a save that removes no server
// deletes none, whatever else the stores hold (WebDAV round 5). Caller holds uiMu.
func (s *Server) forgetServersLocked(old, now nasFile) {
	// A connection lives exactly as long as its server is set up as it was: removed,
	// moved, switched off or given other credentials, it is cancelled here, by the save.
	// So "is this server still the one that was searched" is asked of the connection
	// (serverSearch) and never needs the settings again (WebDAV round 13).
	set := map[string]providers.NASConfig{}
	for _, k := range serverKinds {
		for _, e := range serverEntries(k, now) {
			set[e.ID] = e.cfg
		}
	}
	s.serverMu.Lock()
	for id, c := range s.serverConns {
		if cfg, ok := set[id]; !ok || cfg != c.cfg {
			c.cancel()
			delete(s.serverConns, id)
		}
	}
	s.serverMu.Unlock()
	gone := serverIDs(old)
	for id := range set {
		delete(gone, id)
	}
	if len(gone) == 0 {
		return
	}
	s.stemsMu.Lock()
	s.dropServerStemsLocked(gone)
	s.stemsMu.Unlock()
	s.tidal.dropServers(gone)
	s.jobs.dropServers(gone)
}

// serverIDs are the identities of the servers f names (on or off).
func serverIDs(f nasFile) map[string]bool {
	ids := map[string]bool{}
	for _, k := range serverKinds {
		for _, e := range serverEntries(k, f) {
			ids[e.ID] = true
		}
	}
	return ids
}

// serverAddresses are the addresses (serverRoot) of a kind's servers set up: a
// server's identity is not its address (newServerID), so "is this address a
// server's" asks here.
func (s *Server) serverAddresses(k serverKind) map[string]bool {
	s.uiMu.Lock()
	f := s.loadNASFileLocked()
	s.uiMu.Unlock()
	out := map[string]bool{}
	for _, e := range serverEntries(k, f) {
		out[serverRoot(k, e.cfg.URL)] = true
	}
	return out
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
		f, err := s.readNASFileLocked()
		s.uiMu.Unlock()
		if err != nil {
			fail(w, http.StatusInternalServerError, errNASFile) // not an empty list: an edit would overwrite the file
			return
		}
		respond(w, map[string]any{"servers": serverViews(k, f)})
		return
	}
	var c serverChange
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		return
	}
	views, err := s.applyServer(r.Context(), k, c)
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
	if _, err := s.applyServer(r.Context(), k, c); err != nil {
		key := map[error]string{errWebDAVURL: "wdBadURL", errNASURL: "nasBadURL", errWebDAVNotDAV: "wdNotDAV", errServerDuplicate: "wdDuplicate", errServerFull: "wdFull", errServerGoneList: "wdGone"}[err]
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
	if err != nil {
		log.Printf("stream-sync: server list failed scope=%s provider=%s error=%s", scope, k.protocol, sanitizeLogErr(err))
	}
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
// (searchTimeout, through searchTracks and its cache under the server's own key), and
// returns once every server has answered. The NAS slot's SMB server is searched as the
// NAS source of #335 (its rows stamped with the NAS listing). Every row says which
// server it is from (album). only limits the search to one Sources ID; skip leaves one
// out (the source whose own search already has it).
//
// Each server's rows go to add, in the order of the servers (pos, of n), as soon as that
// server has answered: a caller that stops listening (OmniSource at its deadline) has
// the rows of every server that answered by then, and a server answering later is
// cached for the next search.
//
// Each server's search is tied to its connection (connectedServers): a server removed,
// moved or changed, or its kind switched off, cancels the connection. live, given with
// the rows, says whether the connection still stands: the caller asks it when it puts
// its answer together, so such a server adds nothing, also when it had answered before.
// live reads no settings, so an answer never waits for a save.
func (g *tidalGateway) serverSearch(ctx context.Context, query, only, skip string, add func(pos, n int, refs []trackRef, live func() bool)) {
	type source struct {
		e       serverEntry
		c       *serverConn
		session string
		listing uint64
	}
	var sources []source
	listing := g.s.jobs.NASGen() + 1 // before the NAS settings are read (#335)
	entries, conns := g.s.connectedServers(func(e serverEntry) bool {
		scope := e.Scope()
		return (only == "" || scope == only) && scope != skip
	})
	for i, e := range entries {
		if e.qualified() {
			sources = append(sources, source{e: e, c: conns[i], session: e.Scope()})
		} else { // the slot's SMB server, on the slot's own connection (ownerSMBConn)
			sources = append(sources, source{e, conns[i], fmt.Sprintf("%s@%d", nasScope, listing-1), listing})
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var searches sync.WaitGroup
	for i, v := range sources {
		searches.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("gateway: provider panic: %v", r)
				}
			}()
			tracks, err := g.searchTracks(ctx, v.e.kind.scope, v.session, connSearcher{v.c}, query)
			if err != nil {
				log.Printf("stream-sync: gateway source failed: %s", sanitizeLogErr(err))
			}
			live := func() bool { return v.c.ctx.Err() == nil }
			if !live() {
				return // removed, moved or switched off while its search ran
			}
			var refs []trackRef
			if v.listing != 0 { // the NAS of #335: bare IDs, its listing stamp
				for _, t := range tracks {
					httpsCover(&t)
					t.Album = v.e.Name
					refs = append(refs, trackRef{track: t, session: nasScope, listing: v.listing})
				}
			} else {
				tracks = serverTracksOf(v.e, tracks)
				g.s.cfg.BPM.Enrich(ctx, tracks)
				refs = serverRefs(v.e.Scope(), tracks)
			}
			add(i, len(sources), refs, live)
		})
	}
	searches.Wait()
}

// connSearcher searches through a server's connection: dropping the connection
// cancels the search, even one a cached search's flight runs on its own context.
type connSearcher struct{ c *serverConn }

func (v connSearcher) Search(ctx context.Context, query string) ([]providers.Track, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(v.c.ctx, cancel)
	defer stop()
	return v.c.p.Search(ctx, query)
}

// serverReadyWait is how long OmniSource's search waits for servers after it starts,
// once its other sources (the drives, djay) have answered: a server answering later
// is in the next search, from the cache.
const serverReadyWait = time.Second

// dropServerStemsLocked removes the Stems entries of the servers in gone (removed,
// or moved to another address). A file that does not fully decode is left as it
// is. Caller holds stemsMu.
func (s *Server) dropServerStemsLocked(gone map[string]bool) {
	all, err := s.readStemmed()
	if err != nil {
		return
	}
	changed := false
	for scope, list := range all {
		kept := slices.DeleteFunc(slices.Clone(list), func(e stemmedTrack) bool {
			_, server, ok := rowServer(e.Track)
			return ok && gone[server]
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
