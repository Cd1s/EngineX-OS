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

func enableLiveWithPCM(t *testing.T, s *Server, stateDir string) {
	t.Helper()
	pcmFile := filepath.Join(t.TempDir(), "live.pcm")
	pcm := bytes.Repeat([]byte{1, 2, 3, 4}, 1152)
	if err := os.WriteFile(pcmFile, pcm, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"inputPath": pcmFile})
	if err := os.WriteFile(filepath.Join(stateDir, "broadcast-config.json"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	// No ServerURL anywhere: enabling must still work (#104).
	if err := s.SetBroadcastEnabled(true); err != nil {
		t.Fatalf("enable without ServerURL: %v", err)
	}
	t.Cleanup(func() { _ = s.SetBroadcastEnabled(false) })
}

func TestLiveStreamOffReturns503(t *testing.T) {
	s, _ := newTestServer(t)
	for _, path := range []string{"/live.wav", "/live.mp3"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: expected 503 while Live Stream is off, got %d", path, rec.Code)
		}
	}
}

func TestLiveWAVStreamIsPublic(t *testing.T) {
	s, dir := newTestServer(t)
	enableLiveWithPCM(t, s, dir)
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/live.wav") // no Authorization header, no cookie
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("unexpected response: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	buf := make([]byte, 44+4608)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("short stream: %v", err)
	}
	if string(buf[:4]) != "RIFF" || string(buf[8:12]) != "WAVE" {
		t.Errorf("missing WAV header: %q", buf[:12])
	}
	if !bytes.Equal(buf[44:48], []byte{1, 2, 3, 4}) {
		t.Errorf("PCM payload not forwarded: %v", buf[44:48])
	}
}

func TestLiveMP3UsesExternalEncoder(t *testing.T) {
	s, dir := newTestServer(t)
	enableLiveWithPCM(t, s, dir)
	srv := httptest.NewServer(s)
	defer srv.Close()

	// Without lame/ffmpeg the endpoint says so instead of sending fake audio.
	t.Setenv("PATH", t.TempDir())
	resp, err := http.Get(srv.URL + "/live.mp3")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without an encoder, got %d", resp.StatusCode)
	}

	// A fake "lame" that just copies stdin to stdout proves the PCM goes through it.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "lame"), []byte("#!/bin/sh\nexec cat\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/bin:/usr/bin")
	resp, err = http.Get(srv.URL + "/live.mp3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("unexpected response: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || !bytes.Equal(buf, []byte{1, 2, 3, 4}) {
		t.Errorf("encoder output not streamed: %v %v", buf, err)
	}
}
