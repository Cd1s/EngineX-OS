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
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("invalid url: must be http or https with host")
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
		URL:         cfg.URL,
		Username:    cfg.Username,
		Enabled:     cfg.Enabled,
		HasPassword: cfg.Password != "",
		HasToken:    cfg.Token != "",
	}
	if cfg.Password != "" {
		v.Password = "***"
	}
	if cfg.Token != "" {
		v.Token = "***"
	}
	return v
}

func (s *Server) handleNASConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleNASConfigGet(w, r)
	case http.MethodPost:
		s.handleNASConfigPost(w, r)
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
	if proto != "webdav" && proto != "subsonic" && proto != "plex" {
		fail(w, http.StatusBadRequest, errors.New("invalid protocol: must be webdav, subsonic, or plex"))
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
	if cfg.Protocol != "webdav" && cfg.Protocol != "subsonic" && cfg.Protocol != "plex" {
		fail(w, http.StatusBadRequest, errors.New("invalid protocol: must be webdav, subsonic, or plex"))
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

func (s *Server) ownerSelfHosted() *providers.SelfHostedProvider {
	cfg := s.loadNASConfig()
	if !cfg.Enabled || cfg.Protocol == "" || cfg.URL == "" || validateNASURL(cfg.URL) != nil {
		return nil
	}
	return providers.NewSelfHostedProvider(cfg, s.nasHTTPClient())
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
