package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/broadcast"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	guard := resourceguard.New(resourceguard.Config{Disabled: true})
	jobs := NewJobs(Pipeline{Guard: guard})
	t.Cleanup(func() { jobs.Close() })

	s := New(Config{
		AccessToken: "test-token-1234",
		StateDir:    dir,
	}, jobs)
	s.RegisterBroadcastRoutes()
	return s, dir
}

func doAuthRequest(t *testing.T, s *Server, method, path string, body []byte) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Authorization", "Bearer test-token-1234")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Result()
}

func TestBroadcastGetDefault(t *testing.T) {
	s, _ := newTestServer(t)

	resp := doAuthRequest(t, s, "GET", "/api/broadcast", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var st broadcast.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if st.Enabled {
		t.Errorf("expected default enabled=false")
	}
	if st.PasswordSet {
		t.Errorf("expected default passwordSet=false")
	}
	if st.Protocol != "icecast" {
		t.Errorf("expected default protocol icecast, got %s", st.Protocol)
	}
	if st.Mountpoint != "/live" {
		t.Errorf("expected default mountpoint /live, got %s", st.Mountpoint)
	}
}

func TestBroadcastPostConfigAndPasswordHandling(t *testing.T) {
	s, stateDir := newTestServer(t)

	// 1. Initial configuration with password
	cfgPayload := map[string]any{
		"protocol":   "icecast",
		"serverUrl":  "http://192.168.1.50:8000",
		"mountpoint": "/club.mp3",
		"username":   "dj",
		"password":   "supersecret",
		"format":     "mp3",
		"bitrate":    320,
		"streamName": "Main Stage",
	}
	body, _ := json.Marshal(cfgPayload)

	resp := doAuthRequest(t, s, "POST", "/api/broadcast", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var st broadcast.Status
	json.NewDecoder(resp.Body).Decode(&st)
	if !st.PasswordSet {
		t.Errorf("expected passwordSet=true")
	}
	if st.Bitrate != 320 {
		t.Errorf("expected bitrate=320, got %d", st.Bitrate)
	}
	if st.StreamName != "Main Stage" {
		t.Errorf("expected streamName='Main Stage', got %s", st.StreamName)
	}

	// Verify file saved
	configPath := filepath.Join(stateDir, "broadcast-config.json")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file not created: %v", err)
	}

	// 2. Update without password -> password preserved
	updatePayload := map[string]any{
		"streamName": "Main Stage Night",
	}
	body, _ = json.Marshal(updatePayload)
	resp = doAuthRequest(t, s, "POST", "/api/broadcast", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	if !st.PasswordSet {
		t.Errorf("expected password to remain set when omitted")
	}
	if st.StreamName != "Main Stage Night" {
		t.Errorf("expected updated streamName, got %s", st.StreamName)
	}

	// 3. Changing server URL to different host without password -> password cleared
	hostChangePayload := map[string]any{
		"serverUrl": "http://10.0.0.1:8000",
	}
	body, _ = json.Marshal(hostChangePayload)
	resp = doAuthRequest(t, s, "POST", "/api/broadcast", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	if st.PasswordSet {
		t.Errorf("expected password to be cleared when server host changed without new password")
	}
	if st.ServerURL != "http://10.0.0.1:8000" {
		t.Errorf("expected updated serverUrl, got %s", st.ServerURL)
	}
}

func TestBroadcastPostQueryParam(t *testing.T) {
	s, _ := newTestServer(t)

	// Toggle on via ?enabled=1
	resp := doAuthRequest(t, s, "POST", "/api/broadcast?enabled=1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var st broadcast.Status
	json.NewDecoder(resp.Body).Decode(&st)
	if !st.Enabled {
		t.Errorf("expected enabled=true")
	}

	// Toggle off via ?enabled=0
	resp = doAuthRequest(t, s, "POST", "/api/broadcast?enabled=0", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	if st.Enabled {
		t.Errorf("expected enabled=false")
	}

	// Toggle on via ?enabled=true
	resp = doAuthRequest(t, s, "POST", "/api/broadcast?enabled=true", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	if !st.Enabled {
		t.Errorf("expected enabled=true")
	}

	// Toggle off via ?enabled=false
	resp = doAuthRequest(t, s, "POST", "/api/broadcast?enabled=false", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	if st.Enabled {
		t.Errorf("expected enabled=false")
	}
}

func TestBroadcastMetadataPost(t *testing.T) {
	s, _ := newTestServer(t)

	// 1. Query parameters
	resp := doAuthRequest(t, s, "POST", "/api/broadcast/metadata?artist=The+Prodigy&title=Breathe", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var st broadcast.Status
	json.NewDecoder(resp.Body).Decode(&st)
	if st.CurrentTrack != "The Prodigy - Breathe" {
		t.Errorf("expected 'The Prodigy - Breathe', got %q", st.CurrentTrack)
	}

	// 2. JSON body
	payload := map[string]any{
		"artist": "Underworld",
		"title":  "Born Slippy",
		"deck":   1,
	}
	body, _ := json.Marshal(payload)
	resp = doAuthRequest(t, s, "POST", "/api/broadcast/metadata", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	if st.CurrentTrack != "Underworld - Born Slippy" {
		t.Errorf("expected 'Underworld - Born Slippy', got %q", st.CurrentTrack)
	}
}

func TestBroadcastEnabledHelpers(t *testing.T) {
	s, _ := newTestServer(t)

	if s.BroadcastEnabled() {
		t.Errorf("expected initially false")
	}

	if err := s.SetBroadcastEnabled(true); err != nil {
		t.Fatalf("failed to enable: %v", err)
	}
	if !s.BroadcastEnabled() {
		t.Errorf("expected BroadcastEnabled() == true")
	}

	if err := s.SetBroadcastEnabled(false); err != nil {
		t.Fatalf("failed to disable: %v", err)
	}
	if s.BroadcastEnabled() {
		t.Errorf("expected BroadcastEnabled() == false")
	}
}

func TestBroadcastRoutesAuthorization(t *testing.T) {
	s, _ := newTestServer(t)

	// Unauthenticated request
	req := httptest.NewRequest("GET", "/api/broadcast", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated request, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "console login required") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}
