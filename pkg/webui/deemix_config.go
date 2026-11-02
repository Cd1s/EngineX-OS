package webui

// Where the remote Deemix backend is and the password that protects it. The
// password can be set and replaced but is never read back out.

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
)

type deemixConfig struct {
	URL      string `json:"deemixUrl,omitempty"`
	Password string `json:"deemixPassword,omitempty"`
	RelayURL string `json:"relayUrl,omitempty"`
}

type deemixStatus struct {
	URL         string `json:"deemixUrl"`
	PasswordSet bool   `json:"deemixPasswordSet"`
}

func (s *Server) deemixConfigPath() string {
	return filepath.Join(s.cfg.StateDir, "deemix-config.json")
}

func (s *Server) loadDeemixConfig() deemixConfig {
	var v deemixConfig
	if b, err := os.ReadFile(s.deemixConfigPath()); err == nil {
		json.Unmarshal(b, &v)
	}
	if v.URL == "" {
		v.URL = s.cfg.RemoteDeemixURL
	}
	if v.RelayURL == "" {
		v.RelayURL = s.cfg.RelayURL
	}
	return v
}

func (s *Server) deemixRemote() (string, string) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	v := s.loadDeemixConfig()
	return v.URL, v.Password
}

// relaySettings returns the relay, and the password only if the same host runs
// the Deemix backend it belongs to.
func (s *Server) relaySettings() (relay, password string) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	v := s.loadDeemixConfig()
	a, errA := url.Parse(v.RelayURL)
	b, errB := url.Parse(v.URL)
	if errA == nil && errB == nil && a.Host == b.Host {
		password = v.Password
	}
	return v.RelayURL, password
}

func (s *Server) deemixStatus() deemixStatus {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	v := s.loadDeemixConfig()
	return deemixStatus{v.URL, v.Password != ""}
}

// validDeemixURL refuses to send the password in the clear across the internet.
func validDeemixURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// updateDeemixConfig changes the fields given; an empty password clears it.
func (s *Server) updateDeemixConfig(address, password *string) error {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	v := s.loadDeemixConfig()
	if address != nil {
		v.URL = *address
	}
	if password != nil {
		v.Password = *password
	}
	return providers.SaveJSON(s.deemixConfigPath(), v)
}

// optional turns an empty form field into "leave this setting alone".
func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func (s *Server) deemixGet(w http.ResponseWriter, r *http.Request) { respond(w, s.deemixStatus()) }

func (s *Server) deemixPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL      *string `json:"deemixUrl"`
		Password *string `json:"deemixPassword"`
	}
	if !input(w, r, &in) {
		return
	}
	if in.URL != nil && !validDeemixURL(*in.URL) {
		fail(w, 400, errors.New("Deemix address must be https, or http on a private network"))
		return
	}
	if err := s.updateDeemixConfig(in.URL, in.Password); err != nil {
		fail(w, 500, errors.New("could not save the Deemix settings"))
		return
	}
	s.deemixGet(w, r)
}
