package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

func init() {
	// Disable background UDP 17891 LAN probes during unrelated webui unit tests;
	// LAN probe tests install their own lanStemsProbe hook.
	lanStemsProbe = nil
}

func TestProbeRelayStemsAnswersAndCaches(t *testing.T) {
	hits := 0
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") != "Bearer pw" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"ok":true,"device":"Claw-ai","backend":"hpss"}`))
	}))
	defer relay.Close()
	state := t.TempDir()
	s := New(Config{UIDir: t.TempDir(), StateDir: state}, nil)
	defer s.Close()
	st := s.probeRelayStems(relay.URL, "pw", nil) // 127.0.0.1 over http is a private network
	if st == nil || !st.Online || st.Hostname != "Claw-ai" || st.Backend != "hpss" || st.Endpoint != relay.URL {
		t.Fatalf("probe: %+v", st)
	}
	var onDisk stems.LANState
	b, err := os.ReadFile(filepath.Join(state, stems.LANStateName))
	if err != nil || json.Unmarshal(b, &onDisk) != nil || !onDisk.Online {
		t.Fatalf("stems-lan.json: %s %v", b, err)
	}
	if again := s.probeRelayStems(relay.URL, "pw", &onDisk); again != nil || hits != 1 {
		t.Fatalf("asked again within 30 s: %+v hits %d", again, hits)
	}
	if s.probeRelayStems(relay.URL, "", nil) != nil || s.probeRelayStems("", "pw", nil) != nil {
		t.Fatal("probed without a relay or password")
	}
}

func TestEnsureSpotifyConnectService(t *testing.T) {
	dir := t.TempDir()
	if err := ensureSpotifyConnectService(filepath.Join(dir, "missing")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("created the avahi directory")
	}
	if err := ensureSpotifyConnectService(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "spotify-connect.service"))
	overlay, err := os.ReadFile("../../rootfs-overlay/etc/avahi/services/spotify-connect.service")
	if err != nil || string(b) != string(overlay) || !strings.Contains(string(b), "_spotify-connect._tcp") {
		t.Fatalf("service file differs from the overlay: %v", err)
	}
}

func TestUIStatusDiscoversIdleLANStemsRenderer(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	prev := lanStemsProbe
	t.Cleanup(func() { lanStemsProbe = prev })

	online := true
	lanStemsProbe = func(_ context.Context, _ *stems.LANState, _ time.Duration) (stems.LANState, bool) {
		if !online {
			return stems.LANState{}, false
		}
		return stems.LANState{
			Endpoint: "http://192.168.1.42:18765",
			Hostname: "studio-pc",
			Backend:  "htdemucs",
		}, true
	}

	state := t.TempDir()
	s := New(Config{UIDir: t.TempDir(), StateDir: state}, nil)
	defer s.Close()

	pollUI := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/sc5000/ui", nil)
		req.RemoteAddr = "127.0.0.1:42000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /v1/sc5000/ui status=%d body=%s", w.Code, w.Body.String())
		}
		return w
	}

	// First idle poll returns immediately without blocking and triggers background LAN discovery.
	_ = pollUI()

	var body string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w := pollUI()
		body = w.Body.String()
		if strings.Contains(body, `"hostname":"studio-pc"`) && strings.Contains(body, `"online":true`) {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !strings.Contains(body, `"hostname":"studio-pc"`) || !strings.Contains(body, `"online":true`) {
		t.Fatalf("idle /v1/sc5000/ui never discovered LAN Stems worker: %s", body)
	}

	// Digest stays stable across polls while the discovered worker stays unchanged.
	d1 := pollUI().Header().Get("X-UI-Digest")
	d2 := pollUI().Header().Get("X-UI-Digest")
	if d1 == "" || d1 != d2 {
		t.Fatalf("X-UI-Digest changed across steady polls: %q vs %q", d1, d2)
	}

	// When the worker turns off and the probe interval elapses, status flips to offline.
	prevEvery := lanProbeEvery
	lanProbeEvery = 10 * time.Millisecond
	t.Cleanup(func() { lanProbeEvery = prevEvery })
	online = false
	time.Sleep(20 * time.Millisecond)
	_ = pollUI()

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body = pollUI().Body.String()
		if strings.Contains(body, `"online":false`) {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !strings.Contains(body, `"online":false`) {
		t.Fatalf("offline LAN Stems worker remained reported online: %s", body)
	}
}

func TestLANProbeBudgetSurvivesStuckCacheAndSkipsConfiguredRelay(t *testing.T) {
	prevProbe := lanStemsProbe
	prevEvery := lanProbeEvery
	lanProbeEvery = 5 * time.Millisecond
	t.Cleanup(func() {
		lanStemsProbe = prevProbe
		lanProbeEvery = prevEvery
	})

	poll := func(srv *Server) string {
		req := httptest.NewRequest(http.MethodGet, "/v1/sc5000/ui", nil)
		req.RemoteAddr = "127.0.0.1:42000"
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Body.String()
	}

	// (b) A configured cloud relay whose /healthz answers {"ok":true} without auth while
	// /healthz/stems reports busy:true must NEVER be handed to ProbeLAN as a cached LAN worker.
	busyRelay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte(`{"ok":true}`))
		case "/healthz/stems":
			w.Write([]byte(`{"ok":true,"device":"Claw-ai","backend":"htdemucs","busy":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer busyRelay.Close()

	var passedLast *stems.LANState
	called := make(chan struct{}, 4)
	lanStemsProbe = func(ctx context.Context, last *stems.LANState, step time.Duration) (stems.LANState, bool) {
		passedLast = last
		if last != nil && last.Endpoint != "" {
			st, ok := stems.ProbeLAN(ctx, last, step)
			called <- struct{}{}
			return st, ok
		}
		called <- struct{}{}
		return stems.LANState{}, false
	}

	uiDir2, state2 := t.TempDir(), t.TempDir()
	s2 := New(Config{UIDir: uiDir2, StateDir: state2}, nil)
	defer s2.Close()
	relayURL, relayPW := busyRelay.URL, "secret-pw"
	if err := s2.updateRelayConfig(&relayURL, &relayPW); err != nil {
		t.Fatal(err)
	}
	_ = poll(s2)
	time.Sleep(10 * time.Millisecond)
	_ = poll(s2)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("background LAN probe was not triggered when cloud relay was busy")
	}
	if passedLast != nil && strings.TrimRight(passedLast.Endpoint, "/") == strings.TrimRight(busyRelay.URL, "/") {
		t.Errorf("configured relay endpoint %q was passed to ProbeLAN", passedLast.Endpoint)
	}
	time.Sleep(25 * time.Millisecond)
	body2 := poll(s2)
	if strings.Contains(body2, `"online":true`) {
		t.Errorf("busy cloud relay was overwritten as online via /healthz: %s", body2)
	}

	// (a) A stuck cached LAN endpoint consumes one full step timeout; scheduleLANStemsProbe
	// must give ProbeLAN a 3*step context budget so rediscovery still completes.
	lanStemsProbe = func(ctx context.Context, last *stems.LANState, step time.Duration) (stems.LANState, bool) {
		dl, ok := ctx.Deadline()
		if !ok || time.Until(dl) < 2*step {
			// Simulate single-step cached endpoint timeout exhausting a 1*step context budget.
			stepCtx, cancel := context.WithTimeout(ctx, step)
			<-stepCtx.Done()
			cancel()
		}
		if ctx.Err() != nil {
			return stems.LANState{}, false
		}
		return stems.LANState{
			Endpoint: "http://192.168.1.88:18765",
			Hostname: "recovered-pc",
			Backend:  "htdemucs",
		}, true
	}

	state1 := t.TempDir()
	if err := os.WriteFile(filepath.Join(state1, stems.LANStateName), []byte(`{"endpoint":"http://192.168.1.9:18765","hostname":"stale-pc","backend":"htdemucs","online":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s1 := New(Config{UIDir: t.TempDir(), StateDir: state1}, nil)
	defer s1.Close()

	_ = poll(s1)
	var got1 string
	for deadline := time.Now().Add(2500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		got1 = poll(s1)
		if strings.Contains(got1, `"hostname":"recovered-pc"`) && strings.Contains(got1, `"online":true`) {
			break
		}
	}
	if !strings.Contains(got1, `"hostname":"recovered-pc"`) || !strings.Contains(got1, `"online":true`) {
		t.Errorf("probe context expired after stuck cached endpoint before rediscovery: %s", got1)
	}
}
