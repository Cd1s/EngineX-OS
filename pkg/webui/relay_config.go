package webui

// Where the login relay is and the password that protects it. The password can be
// set and replaced but is never read back out.

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/internal/stems"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

type relayConfig struct {
	RelayURL string `json:"relayUrl,omitempty"`
	Password string `json:"deemixPassword,omitempty"`
}

type relayConfigStatus struct {
	RelayURL    string `json:"relayUrl"`
	PasswordSet bool   `json:"deemixPasswordSet"`
	// LANStems is the home Stems renderer stems-processor last checked.
	LANStems *stems.LANState `json:"lanStems,omitempty"`
}

func (s *Server) relayConfigPath() string {
	return filepath.Join(s.cfg.StateDir, "deemix-config.json")
}

func (s *Server) loadRelayConfig() relayConfig {
	var v relayConfig
	if b, err := os.ReadFile(s.relayConfigPath()); err == nil {
		json.Unmarshal(b, &v)
	}
	if v.RelayURL == "" {
		v.RelayURL = s.cfg.RelayURL
	}
	return v
}

func (s *Server) relaySettings() (relay, password string) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	v := s.loadRelayConfig()
	return v.RelayURL, v.Password
}

func (s *Server) relayStatusView() relayConfigStatus {
	relay, password := s.relaySettings()
	v := relayConfigStatus{RelayURL: relay, PasswordSet: password != ""}
	if b, err := os.ReadFile(filepath.Join(s.cfg.StateDir, stems.LANStateName)); err == nil {
		v.LANStems = new(stems.LANState)
		if json.Unmarshal(b, v.LANStems) != nil {
			v.LANStems = nil
		}
	}
	return v
}

// validRelayURL refuses to send the password in the clear across the internet.
func validRelayURL(raw string) bool {
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

// updateRelayConfig changes the fields given; an empty password clears it.
func (s *Server) updateRelayConfig(relay, password *string) error {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.updateRelayConfigLocked(relay, password)
}

// updateRelayConfigLocked needs s.uiMu. Moving the relay to another host drops
// the saved password unless a new one comes with it: it was never meant for
// that host (#8).
func (s *Server) updateRelayConfigLocked(relay, password *string) error {
	v := s.loadRelayConfig()
	if relay != nil {
		if relayHost(*relay) != relayHost(v.RelayURL) {
			v.Password = ""
		}
		v.RelayURL = *relay
	}
	if password != nil {
		v.Password = *password
	}
	return providers.SaveJSON(s.relayConfigPath(), v)
}

func relayHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func (s *Server) relayConfigGet(w http.ResponseWriter, r *http.Request) {
	respond(w, s.relayStatusView())
}

func (s *Server) relayConfigPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RelayURL *string `json:"relayUrl"`
		Password *string `json:"deemixPassword"`
	}
	if !input(w, r, &in) {
		return
	}
	if in.RelayURL != nil && !validRelayURL(*in.RelayURL) {
		fail(w, 400, errInvalidRelayURL)
		return
	}
	if err := s.updateRelayConfig(in.RelayURL, in.Password); err != nil {
		fail(w, 500, errors.New("could not save the relay settings"))
		return
	}
	s.relayConfigGet(w, r)
}

var errInvalidRelayURL = errors.New("relay address must be https, or http on a private network")
