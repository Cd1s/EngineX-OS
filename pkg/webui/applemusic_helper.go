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
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
	qrcode "github.com/skip2/go-qrcode"
)

func (s *Server) appleMusicHelperPath() string {
	return filepath.Join(s.cfg.StateDir, "applemusic-helper.json")
}

type appleMusicHelperSettings struct {
	providers.AppleMusicHelperConfig
	TokenCleared bool `json:"tokenCleared,omitempty"`
}

func (s *Server) loadAppleMusicHelper() {
	var saved appleMusicHelperSettings
	if b, err := os.ReadFile(s.appleMusicHelperPath()); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	cfg := saved.AppleMusicHelperConfig
	s.appleMusicHelperTokenCleared = saved.TokenCleared
	if cfg.URL == "" {
		cfg.URL = s.cfg.AppleMusicHelperURL
	}
	if cfg.Token == "" && !saved.TokenCleared {
		cfg.Token = s.cfg.AppleMusicHelperToken
	}
	s.appleMusic.SetHelper(cfg.URL, cfg.LoginURL, cfg.Token)
}

func (s *Server) helperLoginPageURL() string {
	cfg := s.appleMusic.HelperConfig()
	if cfg.LoginURL != "" {
		u, err := url.Parse(cfg.LoginURL)
		if err != nil || u.Hostname() == "" {
			return cfg.LoginURL
		}
		if cfg.Token != "" && u.Query().Get("token") == "" {
			q := u.Query()
			q.Set("token", cfg.Token)
			u.RawQuery = q.Encode()
		}
		return u.String()
	}
	if cfg.URL == "" {
		return ""
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := u.Hostname()
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		host = lanHost()
	}
	u.Host = net.JoinHostPort(host, "12342")
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "/", "", "", ""
	if cfg.Token != "" {
		q := u.Query()
		q.Set("token", cfg.Token)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func validAppleMusicHelperURL(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func (s *Server) appleMusicHelperStatus(w http.ResponseWriter, r *http.Request) {
	status := s.appleMusic.HelperStatus(r.Context())
	cfg := s.appleMusic.HelperConfig()
	// Credential-bearing URLs are served only by the authenticated redirect / QR.
	loginPage := ""
	if cfg.URL != "" {
		loginPage = strings.TrimSuffix(r.URL.Path, "/") + "/helper-login"
	}
	respond(w, map[string]any{"configured": status.Configured, "online": status.Online, "helperUrl": cfg.URL, "helperLoginUrl": cfg.LoginURL, "loginPageUrl": loginPage, "tokenSet": cfg.Token != "", "signedIn": status.SignedIn, "loginState": status.LoginState, "loginMessage": status.Message})
}

func (s *Server) appleMusicHelperSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		HelperURL      string `json:"helperUrl"`
		HelperLoginURL string `json:"helperLoginUrl"`
		HelperToken    string `json:"helperToken"`
		ClearToken     bool   `json:"clearToken"`
	}
	if !input(w, r, &in) {
		return
	}
	in.HelperURL, in.HelperLoginURL = strings.TrimSpace(in.HelperURL), strings.TrimSpace(in.HelperLoginURL)
	if !validAppleMusicHelperURL(in.HelperURL) || !validAppleMusicHelperURL(in.HelperLoginURL) {
		fail(w, 400, errors.New("applemusic: use an http or https URL without credentials, query or fragment"))
		return
	}
	s.appleMusicHelperMu.Lock()
	cfg := s.appleMusic.HelperConfig()
	tokenCleared := s.appleMusicHelperTokenCleared
	cfg.URL, cfg.LoginURL = in.HelperURL, in.HelperLoginURL
	if in.ClearToken {
		cfg.Token = ""
		tokenCleared = true
	} else if in.HelperToken != "" {
		cfg.Token = in.HelperToken
		tokenCleared = false
	}
	err := providers.SaveJSON(s.appleMusicHelperPath(), appleMusicHelperSettings{cfg, tokenCleared})
	if err == nil {
		s.appleMusicHelperTokenCleared = tokenCleared
		s.appleMusic.SetHelper(cfg.URL, cfg.LoginURL, cfg.Token)
	}
	s.appleMusicHelperMu.Unlock()
	if err != nil {
		fail(w, 500, errors.New("applemusic: could not save helper settings"))
		return
	}
	s.appleMusicHelperStatus(w, r)
}

func (s *Server) appleMusicHelperLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !input(w, r, &in) {
		return
	}
	status, err := s.appleMusic.HelperLoginBegin(r.Context(), in.Username, in.Password)
	if err != nil {
		fail(w, 400, err)
		return
	}
	respond(w, status)
}

func (s *Server) appleMusicHelper2FA(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if !input(w, r, &in) {
		return
	}
	status, err := s.appleMusic.HelperLogin2FA(r.Context(), strings.TrimSpace(in.Code))
	if err != nil {
		fail(w, 400, err)
		return
	}
	respond(w, status)
}

func (s *Server) appleMusicHelperQR(w http.ResponseWriter, r *http.Request) {
	target := s.helperLoginPageURL()
	if target == "" {
		fail(w, 404, errors.New("applemusic: helper is not configured"))
		return
	}
	png, err := qrcode.Encode(target, qrcode.Medium, 320)
	if err != nil {
		fail(w, 500, errors.New("applemusic: could not generate QR"))
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

func (s *Server) appleMusicHelperRedirect(w http.ResponseWriter, r *http.Request) {
	target := s.helperLoginPageURL()
	if target == "" {
		fail(w, 404, errors.New("applemusic: helper is not configured"))
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) appleMusicPhoneHelperQR(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("provider", "applemusic")
	if s.phoneFlow(w, r) == nil {
		return
	}
	s.appleMusicHelperQR(w, r)
}

func (s *Server) registerAppleMusicHelper(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix, s.appleMusicHelperStatus)
	mux.HandleFunc("POST "+prefix, s.appleMusicHelperSave)
	mux.HandleFunc("POST "+prefix+"/login", s.appleMusicHelperLogin)
	mux.HandleFunc("POST "+prefix+"/2fa", s.appleMusicHelper2FA)
	mux.HandleFunc("GET "+prefix+"/helper-qr", s.appleMusicHelperQR)
	mux.HandleFunc("GET "+prefix+"/helper-login", s.appleMusicHelperRedirect)
}

func (s *Server) appleMusicPhoneHelper(ctx context.Context, lang int, action string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return appleMusicHelperPhonePage(lang, action, s.appleMusic.HelperStatus(ctx), s.helperLoginPageURL())
}
