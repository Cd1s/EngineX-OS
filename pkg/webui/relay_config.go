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
	"sync/atomic"
	"time"

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

// relayStatusView is what the status handlers show. It never waits on the network: Engine's
// QML polls /v1/sc5000/ui every 3 s on its GUI thread, and an unreachable relay held that
// request for up to 1.5 s (BH-GC2-12). The probes run in refreshRelayStatus, in the
// background; their answers are in stems-lan.json (and in memory when it cannot be written).
func (s *Server) relayStatusView() relayConfigStatus {
	s.startRelayRefresh()
	relay, password := s.relaySettings()
	v := relayConfigStatus{RelayURL: relay, PasswordSet: password != ""}
	v.LANStems = s.savedLANStems()
	if st := s.relaySeen.Load(); st != nil && (v.LANStems == nil || st.Checked.After(v.LANStems.Checked)) {
		v.LANStems = st
	}
	return v
}

func (s *Server) savedLANStems() *stems.LANState {
	b, err := os.ReadFile(filepath.Join(s.cfg.StateDir, stems.LANStateName))
	if err != nil {
		return nil
	}
	st := new(stems.LANState)
	if json.Unmarshal(b, st) != nil {
		return nil
	}
	return st
}

// lanStemsSeen is the last answer of a status probe.
type lanStemsSeen struct{ atomic.Pointer[stems.LANState] }

// relayRefresher runs the status probes off the request path: when first needed, every
// relayProbeEvery, and at once after the relay settings change (kickRelayRefresh).
type relayRefresher struct {
	once    sync.Once
	kick    chan struct{}
	running atomic.Bool
}

func (s *Server) startRelayRefresh() {
	s.relayRefresh.once.Do(func() {
		s.relayRefresh.kick = make(chan struct{}, 1)
		s.relayRefresh.running.Store(true)
		go s.relayRefreshLoop()
	})
}

func (s *Server) relayRefreshLoop() {
	t := time.NewTicker(relayProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		default:
		}
		s.refreshRelayStatus()
		select {
		case <-s.closed:
			return
		case <-t.C:
		case <-s.relayRefresh.kick:
		}
	}
}

// refreshRelayStatus runs every network probe behind relayStatusView, each with its own
// once-per-30-s limit. Another probe joins with one more line here.
func (s *Server) refreshRelayStatus() {
	relay, password := s.relaySettings()
	saved := s.savedLANStems()
	if st := s.probeRelayStems(relay, password, saved); st != nil {
		s.relaySeen.Store(st)
	}
}

// kickRelayRefresh probes again now, not after the rest of the 30 s: the settings changed.
// It does not start the refresher: that waits for the first status request.
func (s *Server) kickRelayRefresh() {
	relayProbeMu.Lock()
	delete(relayProbed, s.cfg.StateDir)
	relayProbeMu.Unlock()
	if !s.relayRefresh.running.Load() {
		return
	}
	select {
	case s.relayRefresh.kick <- struct{}{}:
	default:
	}
}

const relayProbeEvery = 30 * time.Second

var (
	relayProbeMu sync.Mutex
	relayProbed  = map[string]time.Time{} // by state dir: when the relay was last asked
)

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
	if err := providers.SaveJSON(s.relayConfigPath(), v); err != nil {
		return err
	}
	s.kickRelayRefresh()
	return nil
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
