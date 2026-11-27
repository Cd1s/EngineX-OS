package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/smbclient"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

var nasAllowLoopback = testing.Testing()

func disallowedNASIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if !nasAllowLoopback && ip.IsLoopback() {
		return true
	}
	return false
}

func validateNASURL(raw string) error {
	u, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "smb") {
		return errors.New("invalid url: must be http, https, or smb with host")
	}
	if u.User != nil {
		return errors.New("invalid url: use the separate username and password fields")
	}
	if u.Scheme == "smb" {
		if _, err := smbclient.Parse(raw); err != nil {
			return err
		}
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return errors.New("invalid url: missing host")
	}
	if !nasAllowLoopback && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return errors.New("invalid url: loopback host is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && disallowedNASIP(ip) {
		return errors.New("invalid url: target address is not allowed")
	}
	return nil
}

func (s *Server) nasHTTPClient() *http.Client {
	if nasAllowLoopback {
		return s.cfg.HTTP
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	if s.cfg.HTTP != nil {
		if t, ok := s.cfg.HTTP.Transport.(*http.Transport); ok && t != nil {
			base = t.Clone()
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			if err == nil {
				err = errors.New("nas: host resolution returned no addresses")
			}
			return nil, err
		}
		for _, ipa := range ips {
			if disallowedNASIP(ipa.IP) {
				return nil, errors.New("nas: target address is not allowed")
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
	timeout := 30 * time.Second
	if s.cfg.HTTP != nil && s.cfg.HTTP.Timeout > 0 {
		timeout = s.cfg.HTTP.Timeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: base,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("nas: too many redirects")
			}
			return validateNASURL(req.URL.String())
		},
	}
}

// NASConfigView is the desensitized view of NASConfig returned to the web console.
type NASConfigView struct {
	Protocol    string `json:"protocol"`
	URL         string `json:"url"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	Token       string `json:"token,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	HasToken    bool   `json:"hasToken"`
	Enabled     bool   `json:"enabled"`
}

func (s *Server) nasConfigPath() string {
	return filepath.Join(s.uiDir(), "nas.json")
}

func (s *Server) loadNASConfigLocked() providers.NASConfig {
	var cfg providers.NASConfig
	if b, err := os.ReadFile(s.nasConfigPath()); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	// Older configs could keep Basic Auth in the URL. Preserve that login without
	// returning its credentials in a URL or overwriting explicit auth fields.
	if u, err := url.Parse(strings.TrimSpace(cfg.URL)); err == nil && u.User != nil {
		if cfg.Username == "" {
			cfg.Username = u.User.Username()
		}
		if cfg.Password == "" {
			if pass, ok := u.User.Password(); ok && pass != "" {
				cfg.Password = pass
			}
		}
		u.User = nil
		cfg.URL = u.String()
	}
	return cfg
}

func (s *Server) loadNASConfig() providers.NASConfig {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadNASConfigLocked()
}

func toNASConfigView(cfg providers.NASConfig) NASConfigView {
	cleanURL := ""
	if raw := strings.TrimSpace(cfg.URL); raw != "" {
		if u, err := url.Parse(raw); err == nil {
			u.User, u.RawQuery, u.ForceQuery, u.Fragment = nil, "", false, ""
			cleanURL = u.String()
		}
	}
	return NASConfigView{
		Protocol:    cfg.Protocol,
		URL:         cleanURL,
		Username:    cfg.Username,
		Enabled:     cfg.Enabled,
		HasPassword: cfg.Password != "",
		HasToken:    cfg.Token != "",
	}
}

func (s *Server) handleNASConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleNASConfigGet(w, r)
	case http.MethodPost:
		s.handleNASConfigPost(w, r)
	case http.MethodDelete:
		s.uiMu.Lock()
		defer s.uiMu.Unlock()
		defer s.resetNASIfMoved(s.loadNASConfigLocked())
		if err := os.Remove(s.nasConfigPath()); err != nil && !os.IsNotExist(err) {
			fail(w, http.StatusInternalServerError, errors.New("could not remove NAS configuration"))
			return
		}
		respond(w, toNASConfigView(providers.NASConfig{}))
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleNASConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.loadNASConfig()
	respond(w, toNASConfigView(cfg))
}

func (s *Server) handleNASConfigPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Protocol    string `json:"protocol"`
		URL         string `json:"url"`
		Username    string `json:"username"`
		Password    string `json:"password"`
		Token       string `json:"token"`
		HasPassword bool   `json:"hasPassword"`
		HasToken    bool   `json:"hasToken"`
		Enabled     bool   `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		return
	}

	proto := strings.ToLower(strings.TrimSpace(in.Protocol))
	if proto != "webdav" && proto != "subsonic" && proto != "plex" && proto != "smb" {
		fail(w, http.StatusBadRequest, errors.New("invalid protocol: must be webdav, subsonic, plex, or smb"))
		return
	}

	targetURL := strings.TrimSpace(in.URL)
	if err := validateNASURL(targetURL); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}

	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	defer s.resetNASIfMoved(s.loadNASConfigLocked())

	existing := s.loadNASConfigLocked()
	newCfg := providers.NASConfig{
		Protocol: proto,
		URL:      targetURL,
		Username: strings.TrimSpace(in.Username),
		Enabled:  in.Enabled,
	}

	// Preserve existing password if incoming is empty or "***"
	if in.Password == "" || in.Password == "***" {
		newCfg.Password = existing.Password
	} else {
		newCfg.Password = in.Password
	}

	// Preserve existing token if incoming is empty or "***"
	if in.Token == "" || in.Token == "***" {
		newCfg.Token = existing.Token
	} else {
		newCfg.Token = in.Token
	}

	if err := providers.SaveJSON(s.nasConfigPath(), newCfg); err != nil {
		fail(w, http.StatusInternalServerError, errors.New("could not save nas configuration"))
		return
	}

	respond(w, toNASConfigView(newCfg))
}

func (s *Server) handleNASTest(w http.ResponseWriter, r *http.Request) {
	var cfg providers.NASConfig
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		var in struct {
			Protocol string `json:"protocol"`
			URL      string `json:"url"`
			Username string `json:"username"`
			Password string `json:"password"`
			Token    string `json:"token"`
			Enabled  bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err == nil && in.Protocol != "" && in.URL != "" {
			existing := s.loadNASConfig()
			cfg = providers.NASConfig{
				Protocol: strings.ToLower(strings.TrimSpace(in.Protocol)),
				URL:      strings.TrimSpace(in.URL),
				Username: strings.TrimSpace(in.Username),
				Enabled:  in.Enabled,
			}
			if in.Password == "" || in.Password == "***" {
				cfg.Password = existing.Password
			} else {
				cfg.Password = in.Password
			}
			if in.Token == "" || in.Token == "***" {
				cfg.Token = existing.Token
			} else {
				cfg.Token = in.Token
			}
		}
	}

	if cfg.Protocol == "" {
		cfg = s.loadNASConfig()
	}

	if cfg.Protocol == "" || cfg.URL == "" {
		fail(w, http.StatusBadRequest, errors.New("nas: no configuration provided or saved"))
		return
	}
	if cfg.Protocol != "webdav" && cfg.Protocol != "subsonic" && cfg.Protocol != "plex" && cfg.Protocol != "smb" {
		fail(w, http.StatusBadRequest, errors.New("invalid protocol: must be webdav, subsonic, plex, or smb"))
		return
	}
	if err := validateNASURL(cfg.URL); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}

	p := providers.NewSelfHostedProvider(cfg, s.nasHTTPClient())
	lists, err := p.Playlists(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}

	respond(w, map[string]any{
		"ok":        true,
		"playlists": len(lists),
	})
}

const nasScope = "nas"

func nasConfigReady(cfg providers.NASConfig) bool {
	return cfg.Enabled &&
		(cfg.Protocol == "webdav" || cfg.Protocol == "subsonic" || cfg.Protocol == "plex" || cfg.Protocol == "smb") &&
		cfg.URL != "" && validateNASURL(cfg.URL) == nil
}

func isNASProvider(p string) bool {
	return p == "webdav" || p == "subsonic" || p == "plex" || p == "smb"
}

// resetNASIfMoved runs after a write of nas.json, deferred after uiMu is taken,
// so it runs while the writer still holds uiMu and no one reads the new settings
// before it is done. When the server, share or root changed, it forgets every
// NAS track ID, download, Stems entry and cached search: an old ID could
// otherwise name the new root's file at the same path, or a new ID play the old
// root's file (#335).
//
// The generation and the stores it guards change in one step: stemsMu, then the
// import lock, then j.mu are held across the bump and the jobs and Stems resets.
// Every generation check is made under one of those locks, or under g.mu
// against rows the gateway reset then drops, together with the action it guards.
func (s *Server) resetNASIfMoved(before providers.NASConfig) {
	now := s.loadNASConfigLocked()
	if nasTarget(now) == nasTarget(before) {
		return
	}
	if s.jobs != nil && s.jobs.pause != nil {
		s.jobs.pause("reset")
	}
	root := nasRootID(now)
	s.stemsMu.Lock()
	s.jobs.resetNAS(root, func() { s.resetNASStemsLocked(root) })
	s.stemsMu.Unlock()
	s.tidal.resetNAS(root)
}

// nasRootID names the NAS root (protocol, server, port, share and root, as
// nasTarget) without credentials or a readable server name. Each saved NAS state
// file records the root its NAS entries belong to; entries of another root, or
// of none (files from before #335 round 6), are never used.
func nasRootID(cfg providers.NASConfig) string {
	sum := sha256.Sum256([]byte(nasTarget(cfg)))
	return hex.EncodeToString(sum[:16])
}

// nasAuthorize adds the NAS's credentials back to address when it is a URL of the
// configured NAS server's exact origin: saved cover URLs hold none (#335).
func (s *Server) nasAuthorize(address string) string {
	if s == nil {
		return address
	}
	if p := s.ownerSelfHosted(); p != nil {
		return p.AuthorizeURL(address)
	}
	return address
}

// onNASOrigin reports whether address is on the configured NAS's exact origin
// (scheme, host, effective port): its covers may be fetched over plain http when the
// NAS itself is http, as its audio is.
func (s *Server) onNASOrigin(address string) bool {
	if s == nil {
		return false
	}
	p := s.ownerSelfHosted()
	return p != nil && p.OnOrigin(address)
}

// stateIO is nil except in tests that make NAS state file reads or writes fail.
var stateIO atomic.Pointer[stateFiles]

type stateFiles struct {
	save func(path string, v any) error
	read func(path string) ([]byte, error)
}

func saveState(path string, v any) error {
	if f := stateIO.Load(); f != nil && f.save != nil {
		return f.save(path, v)
	}
	return providers.SaveJSON(path, v)
}

func readState(path string) ([]byte, error) {
	if f := stateIO.Load(); f != nil && f.read != nil {
		return f.read(path)
	}
	return os.ReadFile(path)
}

// gatewayProvider: an SMB NAS is its own source (#335); WebDAV, Subsonic and
// Plex keep their released place, the [NAS] playlists inside OmniSource.
// nasTarget is what NAS settings point at: protocol, host, effective port, share
// and root. A trailing or doubled slash does not count; the case of path
// components does.
func nasTarget(cfg providers.NASConfig) string {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || u.Host == "" {
		return cfg.Protocol + "\x00" + cfg.URL
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"smb": "445", "http": "80", "https": "443"}[u.Scheme]
	}
	return strings.Join([]string{strings.ToLower(cfg.Protocol), u.Scheme, strings.ToLower(u.Hostname()), port, path.Clean("/" + u.Path)}, "\x00")
}

func gatewayProvider(p string) string {
	if p == "smb" {
		return nasScope
	}
	return p
}

// smbNASConfigReady: only an SMB NAS gets the standalone NAS entry.
func smbNASConfigReady(cfg providers.NASConfig) bool {
	return cfg.Protocol == "smb" && nasConfigReady(cfg)
}

func (s *Server) nasAvailableLocked() bool {
	return smbNASConfigReady(s.loadNASConfigLocked())
}

func (s *Server) nasAvailable() bool {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.nasAvailableLocked()
}

func (s *Server) ownerSelfHosted() *providers.SelfHostedProvider {
	cfg := s.loadNASConfig()
	if !nasConfigReady(cfg) {
		return nil
	}
	return providers.NewSelfHostedProvider(cfg, s.nasHTTPClient())
}

// ownerSMBSelfHosted is ownerSelfHosted for the standalone NAS entry: nil
// unless the NAS is SMB.
func (s *Server) ownerSMBSelfHosted() *providers.SelfHostedProvider {
	if cfg := s.loadNASConfig(); smbNASConfigReady(cfg) {
		return providers.NewSelfHostedProvider(cfg, s.nasHTTPClient())
	}
	return nil
}

func (s *Server) nasPlaylists(ctx context.Context) ([]providers.Playlist, *providers.SelfHostedProvider) {
	p := s.ownerSelfHosted()
	if p == nil {
		return nil, nil
	}
	lists, err := p.Playlists(ctx)
	if err != nil {
		return nil, p
	}
	return lists, p
}

// nasPlaylistsPage serves a configured SMB NAS as its own source under Sources
// (independent of OmniSource, #335).
func (g *tidalGateway) nasPlaylistsPage(w http.ResponseWriter, r *http.Request) {
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
	if n := len(g.s.stemmed(nasScope)); n > 0 {
		add(playlistJSON(stemsListID(nasScope), "Stems", "", n))
	}
	listing := g.s.jobs.NASGen() + 1 // before the NAS settings are read (#335)
	p := g.s.ownerSMBSelfHosted()
	if p == nil {
		add(playlistJSON(hintPlaylist, sourceFailedTitle(nasScope, errors.New("not configured")), "http://"+lanHost()+":8080", 0))
		respond(w, page(r, 100, items))
		return
	}
	lists, err := p.Playlists(r.Context())
	if err != nil && len(lists) == 0 {
		add(playlistJSON(hintPlaylist, sourceFailedTitle(nasScope, err), err.Error(), 0))
		respond(w, page(r, 100, items))
		return
	}
	for _, pl := range lists {
		ref := playlistRef{session: nasScope, provider: nasScope, list: pl}
		if id := g.addPlaylistAt(ref, listing); id != "" {
			add(g.refJSON(id, ref))
		}
	}
	respond(w, page(r, 100, items))
}

func (g *tidalGateway) nasTracks(w http.ResponseWriter, r *http.Request, id string, ref playlistRef, role, order string) {
	listing := g.s.jobs.NASGen() + 1 // before the NAS settings are read (#335)
	nasProv := g.s.ownerSMBSelfHosted()
	if nasProv == nil {
		err := errors.New("nas provider is not enabled")
		g.logPlaylistOpenFailure(r, http.StatusNotFound, id, &ref, err)
		fail(w, http.StatusNotFound, err)
		return
	}
	tracks, err := nasProv.PlaylistTracks(r.Context(), ref.list.ID)
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
	g.s.cfg.BPM.Enrich(r.Context(), tracks)
	g.enrichRatingsFromDB(r.Context(), tracks)
	refs := make([]trackRef, 0, len(tracks))
	for _, t := range tracks {
		refs = append(refs, trackRef{track: t, session: nasScope, listing: listing})
	}
	if role != "" && role != "default" {
		sortStreamingTracks(refs, role, order)
	}
	respond(w, page(r, 500, g.refItems(refs)))
}
