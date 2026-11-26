package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

func validateNASRedirectURL(raw string) error {
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

func validateNASURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if err := validateNASRedirectURL(trimmed); err != nil {
		return err
	}
	u, err := url.ParseRequestURI(trimmed)
	if err != nil {
		return errors.New("invalid url: must be http, https, or smb with host")
	}
	if u.RawQuery != "" || strings.Contains(trimmed, "?") {
		return errors.New("invalid url: query parameters are not allowed")
	}
	if u.Fragment != "" || strings.Contains(trimmed, "#") {
		return errors.New("invalid url: fragments are not allowed")
	}
	return nil
}

func sameOriginURL(a, b *url.URL) bool {
	if a == nil || b == nil || !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	norm := func(scheme, host string) string {
		h := strings.ToLower(strings.TrimSpace(host))
		if hn, port, err := net.SplitHostPort(h); err == nil {
			if (strings.EqualFold(scheme, "http") && port == "80") ||
				(strings.EqualFold(scheme, "https") && port == "443") {
				if strings.Contains(hn, ":") {
					return "[" + hn + "]"
				}
				return hn
			}
		}
		return h
	}
	return norm(a.Scheme, a.Host) == norm(b.Scheme, b.Host)
}

func sanitizeNASBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
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
			req.Header.Del("Referer")
			if len(via) > 0 && (req.URL.User != nil || !sameOriginURL(req.URL, via[0].URL)) {
				req.Header.Del("Authorization")
				req.Header.Del("X-Plex-Token")
				req.Header.Del("Cookie")
				return errors.New("nas: cross-origin redirect is not allowed")
			}
			if len(via) >= 5 {
				return errors.New("nas: too many redirects")
			}
			return validateNASRedirectURL(req.URL.String())
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
	hadFile := false
	if b, err := os.ReadFile(s.nasConfigPath()); err == nil {
		if json.Unmarshal(b, &cfg) == nil {
			hadFile = true
		}
	}
	// Older configs could keep credentials in URL userinfo or query parameters.
	// Preserve explicit login fields where empty, strip them from the base URL,
	// and migrate the on-disk record so the settings API never echoes them.
	if u, err := url.Parse(cfg.URL); err == nil {
		migrated := false
		if u.User != nil {
			if cfg.Username == "" {
				cfg.Username = u.User.Username()
				cfg.Password, _ = u.User.Password()
			}
			u.User = nil
			migrated = true
		}
		if u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.URL, "?") {
			if q, qerr := url.ParseQuery(u.RawQuery); qerr == nil {
				switch strings.ToLower(strings.TrimSpace(cfg.Protocol)) {
				case "plex":
					if cfg.Token == "" {
						for k, vals := range q {
							if strings.EqualFold(k, "x-plex-token") && len(vals) > 0 && vals[0] != "" {
								cfg.Token = vals[0]
								break
							}
						}
					}
				case "subsonic":
					if cfg.Username == "" && q.Get("u") != "" {
						cfg.Username = q.Get("u")
					}
					if cfg.Password == "" && q.Get("p") != "" {
						cfg.Password = q.Get("p")
					}
				}
			}
			u.RawQuery = ""
			u.ForceQuery = false
			migrated = true
		}
		if u.Fragment != "" || strings.Contains(cfg.URL, "#") {
			u.Fragment = ""
			migrated = true
		}
		if migrated {
			cfg.URL = u.String()
			if hadFile {
				_ = providers.SaveJSON(s.nasConfigPath(), cfg)
			}
		}
	}
	return cfg
}

func (s *Server) loadNASConfig() providers.NASConfig {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadNASConfigLocked()
}

func toNASConfigView(cfg providers.NASConfig) NASConfigView {
	v := NASConfigView{
		Protocol:    cfg.Protocol,
		URL:         sanitizeNASBaseURL(cfg.URL),
		Username:    cfg.Username,
		Enabled:     cfg.Enabled,
		HasPassword: cfg.Password != "",
		HasToken:    cfg.Token != "",
	}
	return v
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
		if err := os.Remove(s.nasConfigPath()); err != nil && !os.IsNotExist(err) {
			fail(w, http.StatusInternalServerError, errors.New("could not remove NAS configuration"))
			return
		}
		s.nasProv = nil
		s.nasProvCfg = providers.NASConfig{}
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

	s.nasProv = nil
	s.nasProvCfg = providers.NASConfig{}
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
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "webdav", "subsonic", "plex", "smb", nasScope:
		return true
	default:
		return false
	}
}

func gatewayProvider(p string) string {
	switch p {
	case "webdav", "subsonic", "plex", "smb":
		return nasScope
	default:
		return p
	}
}

func (s *Server) nasAvailableLocked() bool {
	return nasConfigReady(s.loadNASConfigLocked())
}

func (s *Server) nasAvailable() bool {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.nasAvailableLocked()
}

func (s *Server) ownerSelfHosted() *providers.SelfHostedProvider {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	cfg := s.loadNASConfigLocked()
	if !nasConfigReady(cfg) {
		s.nasProv = nil
		s.nasProvCfg = providers.NASConfig{}
		return nil
	}
	if s.nasProv != nil && s.nasProvCfg == cfg && s.nasProvLoopback == nasAllowLoopback {
		return s.nasProv
	}
	s.nasProv = providers.NewSelfHostedProvider(cfg, s.nasHTTPClient())
	s.nasProvCfg = cfg
	s.nasProvLoopback = nasAllowLoopback
	return s.nasProv
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

// nasPlaylistsPage serves a configured NAS as its own source under Sources
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
	p := g.s.ownerSelfHosted()
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
		add(g.refJSON(g.addPlaylist(ref), ref))
	}
	respond(w, page(r, 100, items))
}

func (g *tidalGateway) nasTracks(w http.ResponseWriter, r *http.Request, id string, ref playlistRef, role, order string) {
	nasProv := g.s.ownerSelfHosted()
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
		refs = append(refs, trackRef{track: t, session: nasScope})
	}
	if role != "" && role != "default" {
		sortStreamingTracks(refs, role, order)
	}
	respond(w, page(r, 500, g.refItems(refs)))
}
