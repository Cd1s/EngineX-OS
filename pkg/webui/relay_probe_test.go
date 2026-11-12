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
