package webui

// Where the login relay is and the password that protects it. The password can be
// set and replaced but is never read back out.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

var lanStemsProbe = stems.ProbeLAN

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
	if st := s.probeRelayStems(relay, password, v.LANStems); st != nil {
		v.LANStems = st
	} else if relay == "" || password == "" || v.LANStems == nil || !v.LANStems.Online {
		lanSaved := v.LANStems
		if lanSaved != nil && relay != "" && strings.TrimRight(strings.TrimSpace(lanSaved.Endpoint), "/") == strings.TrimRight(strings.TrimSpace(relay), "/") {
			lanSaved = nil
		}
		s.scheduleLANStemsProbe(lanSaved)
	}
	return v
}

const (
	relayProbeEvery = 30 * time.Second
	lanProbeTimeout = 1200 * time.Millisecond
)

var (
	relayProbeMu     sync.Mutex
	relayProbed      = map[string]time.Time{} // by state dir: when the relay was last asked
	lanProbeEvery    = relayProbeEvery
	lanProbed        = map[string]time.Time{}
	lanProbeInFlight = map[string]bool{}
)

// scheduleLANStemsProbe asynchronously discovers or re-checks a zero-config LAN
// Stems renderer without blocking Engine QML's 3-second /v1/sc5000/ui poll
// (BH2-A9-1). It writes stems-lan.json only on a real state transition, keeping
// X-UI-Digest stable across steady-state polls.
func (s *Server) scheduleLANStemsProbe(saved *stems.LANState) {
	dir := s.cfg.StateDir
	if dir == "" {
		return
	}
	probe := lanStemsProbe
	if probe == nil {
		return
	}
	select {
	case <-s.closed:
		return
	default:
	}

	relayProbeMu.Lock()
	if lanProbeInFlight[dir] || time.Since(lanProbed[dir]) < lanProbeEvery {
		relayProbeMu.Unlock()
		return
	}
	lanProbeInFlight[dir] = true
	lanProbed[dir] = time.Now()
	relayProbeMu.Unlock()

	var prev *stems.LANState
	if saved != nil {
		cp := *saved
		prev = &cp
	}

	s.lanProbeWG.Add(1)
	go func() {
		defer s.lanProbeWG.Done()
		defer func() {
			relayProbeMu.Lock()
			delete(lanProbeInFlight, dir)
			relayProbeMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 3*lanProbeTimeout)
		defer cancel()
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-s.closed:
				cancel()
			case <-stop:
			}
		}()

		got, ok := probe(ctx, prev, lanProbeTimeout)
		select {
		case <-s.closed:
			return
		default:
		}

		path := filepath.Join(dir, stems.LANStateName)
		if ok {
			got.Online = true
			got.Checked = time.Now().UTC()
			if prev == nil || !prev.Online || prev.Endpoint != got.Endpoint || prev.Hostname != got.Hostname || prev.Backend != got.Backend {
				_ = providers.SaveJSON(path, &got)
			}
			return
		}
		if prev != nil && prev.Online {
			off := *prev
			off.Online = false
			off.Checked = time.Now().UTC()
			_ = providers.SaveJSON(path, &off)
		}
	}()
}

// probeRelayStems asks the cloud relay whether its Stems renderer is up, for a deck
// whose stems-lan.json is missing, offline or older than 30 s (nothing else keeps it
// current for a relay). Asked at most once per 30 s, answer or not, and the answer
// is saved to stems-lan.json. nil leaves the file's state as it is.
func (s *Server) probeRelayStems(relay, password string, saved *stems.LANState) *stems.LANState {
	if relay == "" || password == "" || !validRelayURL(relay) {
		return nil
	}
	if saved != nil && saved.Online && time.Since(saved.Checked) < relayProbeEvery {
		return nil
	}
	relayProbeMu.Lock()
	defer relayProbeMu.Unlock()
	if time.Since(relayProbed[s.cfg.StateDir]) < relayProbeEvery {
		return nil
	}
	relayProbed[s.cfg.StateDir] = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(relay, "/")+"/healthz/stems", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+password)
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var h struct {
		OK      bool   `json:"ok"`
		Backend string `json:"backend"`
		Device  string `json:"device"`
		Busy    bool   `json:"busy"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h) != nil || !h.OK {
		return nil
	}
	st := &stems.LANState{Endpoint: relay, Hostname: h.Device, Backend: h.Backend, Online: !h.Busy, Checked: time.Now().UTC()}
	providers.SaveJSON(filepath.Join(s.cfg.StateDir, stems.LANStateName), st) // best effort: the answer is returned anyway
	return st
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
