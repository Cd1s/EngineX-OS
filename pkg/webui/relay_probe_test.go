package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
