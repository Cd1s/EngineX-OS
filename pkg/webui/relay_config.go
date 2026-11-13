package webui

// Where the login relay is and the password that protects it. The password can be
// set and replaced but is never read back out.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net"
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
	if json.Unmarshal(b, st) != nil || (st.Endpoint == "" && !st.Online) {
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
	mu      sync.Mutex
	cancel  context.CancelFunc // the probe in flight
	gen     uint64             // bumped by a settings change: an older probe's answer is dropped
}

// relayProbeContext is a probe's context: 1.5 s at most, cancelled by a settings change
// (kickRelayRefresh) and by Close. gen is the settings generation it probes for.
func (s *Server) relayProbeContext() (ctx context.Context, gen uint64, done func()) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	rr := &s.relayRefresh
	rr.mu.Lock()
	rr.cancel, gen = cancel, rr.gen
	rr.mu.Unlock()
	go func() {
		select {
		case <-s.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, gen, cancel
}

// relayGen reports whether gen is still the current settings generation.
func (s *Server) relayGen(gen uint64) bool {
	s.relayRefresh.mu.Lock()
	defer s.relayRefresh.mu.Unlock()
	return s.relayRefresh.gen == gen
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
	s.relayRefresh.mu.Lock()
	gen := s.relayRefresh.gen
	s.relayRefresh.mu.Unlock()
	relay, password := s.relaySettings()
	saved := s.savedLANStems()
	if st := s.probeRelayStems(relay, password, saved); st != nil {
		s.storeRelaySeen(gen, st)
	} else if relay == "" || password == "" || saved == nil || !saved.Online {
		// The LAN renderer (#224 pairing, BH2-A9-1): at most once per 30 s, never the
		// configured relay, and stems-lan.json is written only when its state changes.
		lanSaved := saved
		if lanSaved != nil && relay != "" && strings.TrimRight(strings.TrimSpace(lanSaved.Endpoint), "/") == strings.TrimRight(strings.TrimSpace(relay), "/") {
			lanSaved = nil
		}
		s.scheduleLANStemsProbe(lanSaved)
	}
}

// storeRelaySeen keeps st unless the settings changed since gen.
func (s *Server) storeRelaySeen(gen uint64, st *stems.LANState) {
	s.relayRefresh.mu.Lock()
	defer s.relayRefresh.mu.Unlock()
	if s.relayRefresh.gen == gen {
		s.relaySeen.Store(st)
	}
}

// kickRelayRefresh probes again now, not after the rest of the 30 s: the settings changed.
// The probe in flight is cancelled and nothing it found is kept; what was found for the
// previous relay (oldRelay) is dropped. It does not start the refresher: that waits for
// the first status request.
func (s *Server) kickRelayRefresh(oldRelay, relay string) {
	rr := &s.relayRefresh
	rr.mu.Lock()
	rr.gen++
	if rr.cancel != nil {
		rr.cancel()
	}
	s.relaySeen.Store(nil)
	rr.mu.Unlock()
	if oldRelay != relay {
		if saved := s.savedLANStems(); saved != nil && saved.Endpoint == oldRelay && oldRelay != "" {
			os.Remove(filepath.Join(s.cfg.StateDir, stems.LANStateName))
		}
	}
	relayProbeMu.Lock()
	delete(relayProbed, s.cfg.StateDir)
	relayProbeMu.Unlock()
	if !rr.running.Load() {
		return
	}
	select {
	case s.relayRefresh.kick <- struct{}{}:
	default:
	}
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
	// A fresh answer of this relay, or of the home renderer stems-processor found (FindLAN
	// keeps its choice in the same file), needs no probe. One of another relay does.
	if saved != nil && saved.Online && time.Since(saved.Checked) < relayProbeEvery && (saved.Endpoint == relay || (!saved.Relay && lanRenderer(saved.Endpoint))) {
		return nil
	}
	// The lock guards the once-per-30-s bookkeeping only, never the request: a settings
	// change during a hung probe must not wait for it.
	relayProbeMu.Lock()
	if time.Since(relayProbed[s.cfg.StateDir]) < relayProbeEvery {
		relayProbeMu.Unlock()
		return nil
	}
	relayProbed[s.cfg.StateDir] = time.Now()
	relayProbeMu.Unlock()
	ctx, gen, cancel := s.relayProbeContext()
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
	st := &stems.LANState{Endpoint: relay, Hostname: h.Device, Backend: h.Backend, Online: !h.Busy, Relay: true, Checked: time.Now().UTC()}
	if !s.relayGen(gen) { // the settings changed while it was asked
		return nil
	}
	providers.SaveJSON(filepath.Join(s.cfg.StateDir, stems.LANStateName), st) // best effort: the answer is returned anyway
	return st
}

// lanRenderer: an endpoint on the local network (a home renderer), as discovery finds them.
func lanRenderer(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || (ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xC0 == 64))
}

// validRelayURL refuses to send the password in the clear across the internet,
// sharing the exact rule with stems.ValidEndpoint.
func validRelayURL(raw string) bool {
	return stems.ValidEndpoint(raw)
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
	old := v.RelayURL
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
	s.kickRelayRefresh(old, v.RelayURL)
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
	stems.RememberPeerHint(r.RemoteAddr)
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
