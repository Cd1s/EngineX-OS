package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

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
	u, err := url.ParseRequestURI(targetURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		fail(w, http.StatusBadRequest, errors.New("invalid url: must be http or https with host"))
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

	p := providers.NewSelfHostedProvider(cfg, s.cfg.HTTP)
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

func (s *Server) nasPlaylists(ctx context.Context) ([]providers.Playlist, *providers.SelfHostedProvider) {
	cfg := s.loadNASConfig()
	if !cfg.Enabled || cfg.Protocol == "" || cfg.URL == "" {
		return nil, nil
	}
	p := providers.NewSelfHostedProvider(cfg, s.cfg.HTTP)
	lists, err := p.Playlists(ctx)
	if err != nil {
		return nil, p
	}
	return lists, p
}
