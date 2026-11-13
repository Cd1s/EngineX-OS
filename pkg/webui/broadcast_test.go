package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestLiveWAVHeadersAndPCMFormat(t *testing.T) {
	s, dir := newTestServer(t)
	enableLiveWithPCM(t, s, dir)
	srv := httptest.NewServer(s)
	defer srv.Close()

	// 1. Standard WAV
	resp, err := http.Get(srv.URL + "/live.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("expected Cache-Control 'no-cache', got %q", cc)
	}
	if xf := resp.Header.Get("X-Audio-Format"); xf != "48000Hz-16bit-2ch-PCM" {
		t.Errorf("expected X-Audio-Format '48000Hz-16bit-2ch-PCM', got %q", xf)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("expected Content-Type 'audio/wav', got %q", ct)
	}

	// 2. Raw PCM stream via ?format=pcm (zero header overhead)
	respPCM, err := http.Get(srv.URL + "/live.wav?format=pcm")
	if err != nil {
		t.Fatal(err)
	}
	defer respPCM.Body.Close()

	if respPCM.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", respPCM.StatusCode)
	}
	if cc := respPCM.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("expected Cache-Control 'no-cache', got %q", cc)
	}
	if xf := respPCM.Header.Get("X-Audio-Format"); xf != "48000Hz-16bit-2ch-PCM" {
		t.Errorf("expected X-Audio-Format '48000Hz-16bit-2ch-PCM', got %q", xf)
	}
	if ct := respPCM.Header.Get("Content-Type"); ct != "audio/L16;rate=48000;channels=2" {
		t.Errorf("expected Content-Type 'audio/L16;rate=48000;channels=2', got %q", ct)
	}

	// Read first 4 bytes - must be raw PCM without any RIFF header
	buf := make([]byte, 4)
	if _, err := io.ReadFull(respPCM.Body, buf); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if !bytes.Equal(buf, []byte{1, 2, 3, 4}) {
		t.Errorf("expected raw PCM payload [1, 2, 3, 4], got %v", buf)
	}
}

func TestBroadcastPostLANOptions(t *testing.T) {
	s, _ := newTestServer(t)

	payload := map[string]any{
		"lanMode":   "rtp",
		"lanTarget": "239.255.0.1:5004",
	}
	body, _ := json.Marshal(payload)

	resp := doAuthRequest(t, s, "POST", "/api/broadcast", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var st broadcast.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if st.LANMode != "rtp" {
		t.Errorf("expected lanMode 'rtp', got %q", st.LANMode)
	}
	if st.LANTarget != "239.255.0.1:5004" {
		t.Errorf("expected lanTarget '239.255.0.1:5004', got %q", st.LANTarget)
	}
}

func TestUSBAudioEndpoints(t *testing.T) {
	s, _ := newTestServer(t)

	// 1. GET /api/usb-audio
	resp := doAuthRequest(t, s, "GET", "/api/usb-audio", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var getRes struct {
		HostDACMode string             `json:"hostDacMode"`
		GadgetUAC2  bool               `json:"gadgetUac2"`
		SampleRate  int                `json:"sampleRate"`
		DACs        []broadcast.USBDAC `json:"dacs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if getRes.HostDACMode != "default" {
		t.Errorf("expected default hostDacMode, got %s", getRes.HostDACMode)
	}
	if getRes.SampleRate != 48000 {
		t.Errorf("expected default sampleRate 48000, got %d", getRes.SampleRate)
	}

	// 2. POST /api/usb-audio
	postPayload := map[string]any{
		"hostDacMode": "external",
		"hostDacCard": "hw:2,0",
		"sampleRate":  96000,
		"gadgetUac2":  true,
	}
	body, _ := json.Marshal(postPayload)
	resp = doAuthRequest(t, s, "POST", "/api/usb-audio", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var postRes struct {
		HostDACMode string             `json:"hostDacMode"`
		HostDACCard string             `json:"hostDacCard"`
		GadgetUAC2  bool               `json:"gadgetUac2"`
		SampleRate  int                `json:"sampleRate"`
		DACs        []broadcast.USBDAC `json:"dacs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&postRes); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if postRes.HostDACMode != "external" || postRes.HostDACCard != "hw:2,0" || postRes.SampleRate != 96000 || !postRes.GadgetUAC2 {
		t.Errorf("unexpected POST response: %+v", postRes)
	}

	// 3. Direct handleUSBAudio call simulating /v1/enginex/usb-audio
	req := httptest.NewRequest("GET", "/v1/enginex/usb-audio", nil)
	rec := httptest.NewRecorder()
	s.handleUSBAudio(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleUSBAudio expected 200, got %d", rec.Code)
	}
}

func TestBroadcastUsernamePersistsAcrossReload(t *testing.T) {
	s, stateDir := newTestServer(t)
	body, _ := json.Marshal(map[string]any{"serverUrl": "http://10.1.1.1:8000", "username": "admin", "password": "pw1"})
	if resp := doAuthRequest(t, s, "POST", "/api/broadcast", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	// Saving the form again with an empty password field omits it: stored password stays.
	body, _ = json.Marshal(map[string]any{"serverUrl": "http://10.1.1.1:8000", "username": "admin2"})
	doAuthRequest(t, s, "POST", "/api/broadcast", body)

	// A fresh Server on the same state dir (= restart / page reload) sees the saved values.
	raw, err := os.ReadFile(filepath.Join(stateDir, "broadcast-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved broadcast.Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Username != "admin2" || saved.Password != "pw1" || saved.ServerURL != "http://10.1.1.1:8000" {
		t.Fatalf("saved config = %+v", saved)
	}
	var st broadcast.Status
	json.NewDecoder(doAuthRequest(t, s, "GET", "/api/broadcast", nil).Body).Decode(&st)
	if st.Username != "admin2" || !st.PasswordSet || st.ServerURL != saved.ServerURL {
		t.Fatalf("GET status = %+v", st)
	}
}

// readFor reads what arrives within d (whole stream bytes, any size).
func readFor(t *testing.T, conn net.Conn, d time.Duration) []byte {
	t.Helper()
	var got []byte
	buf := make([]byte, 64<<10)
	_ = conn.SetReadDeadline(time.Now().Add(d))
	for {
		n, err := conn.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			return got
		}
	}
}

// openLive sends a raw GET for /live.wav from a Wi-Fi-like client (MSS 1448, 32 KB
// receive buffer) and returns the connection after the headers.
func openLive(t *testing.T, srvURL, query string) (net.Conn, []byte) {
	t.Helper()
	u, _ := url.Parse(srvURL)
	// Loopback's 64 KB MSS exceeds a small receive window and stalls TCP (window
	// updates are withheld); use a Wi-Fi-like MSS with a 32 KB receive buffer.
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, 1448)
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 32<<10)
		})
	}}
	conn, err := d.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "GET /live.wav?%s HTTP/1.1\r\nHost: x\r\n\r\n", query); err != nil {
		t.Fatal(err)
	}
	var head []byte
	one := make([]byte, 1)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		if _, err := conn.Read(one); err != nil {
			t.Fatalf("headers: %v (%q)", err, head)
		}
		head = append(head, one[0])
	}
	return conn, head
}

// ?buffer=300: the player gets 300 ms of recent audio at once (a cushion for Wi-Fi
// gaps); the old handler sent only live audio, about 19 KB per 100 ms.
func TestLiveWAVPrefillsRequestedBuffer(t *testing.T) {
	s, dir := newTestServer(t)
	enableLiveWithPCM(t, s, dir)
	srv := httptest.NewServer(s)
	defer srv.Close()
	time.Sleep(700 * time.Millisecond) // let the feed build history
	conn, head := openLive(t, srv.URL, "format=pcm&buffer=300")
	defer conn.Close()
	if !bytes.Contains(head, []byte("200 OK")) || !bytes.Contains(head, []byte("audio/L16;rate=48000;channels=2")) {
		t.Fatalf("headers: %q", head)
	}
	got := readFor(t, conn, 120*time.Millisecond)
	if len(got) < 300*192 {
		t.Fatalf("first 120 ms brought %d bytes, want the %d-byte cushion at once", len(got), 300*192)
	}
	if !bytes.Equal(got[:4], []byte{1, 2, 3, 4}) || len(got) < 8 || !bytes.Equal(got[len(got)&^3-4:len(got)&^3], []byte{1, 2, 3, 4}) {
		t.Fatal("cushion is not on frame boundaries")
	}
}

// A listener that stalls (Wi-Fi) must not come back to seconds of backlog: what it
// can read at once stays near buffer+200 ms plus the capped socket buffers. The old
// handler queued 64 chunks (1.5 s) plus an unbounded kernel send buffer.
func TestLiveWAVStalledListenerLatencyIsBounded(t *testing.T) {
	s, dir := newTestServer(t)
	enableLiveWithPCM(t, s, dir)
	srv := httptest.NewServer(s)
	defer srv.Close()
	conn, _ := openLive(t, srv.URL, "format=pcm&buffer=300")
	defer conn.Close()
	time.Sleep(2500 * time.Millisecond) // stalled: nothing read
	backlog := readFor(t, conn, 150*time.Millisecond)
	// queue (<= 500 ms = 96000 B) + capped send buffer (kernel doubles it) + 32 KB receive
	// buffer (doubled) + live during the read + ~100 ms of scheduling/accounting slack.
	if limit := 96000 + 2*16384 + 64<<10 + 150*192 + 100*192; len(backlog) > limit {
		t.Fatalf("backlog after a 2.5 s stall: %d bytes (%.0f ms), want <= %d", len(backlog), float64(len(backlog))/192, limit)
	}
	more := readFor(t, conn, 300*time.Millisecond)
	all := append(backlog, more...)
	for i := 0; i+4 <= len(all)&^3; i += 4 {
		if !bytes.Equal(all[i:i+4], []byte{1, 2, 3, 4}) {
			t.Fatalf("frame %d after the stall is misaligned: %v", i/4, all[i:i+4])
		}
	}
	if len(more) < 20*192 {
		t.Fatalf("stream did not resume: %d bytes in 300 ms", len(more))
	}
}

// Without ?buffer= the cushion is the wired default, 120 ms (the first version sent
// 300 ms for Wi-Fi): the stream no longer drifts, so the cushion only covers the
// deck's own jitter. ?buffer= still overrides it within 0..2000.
func TestLiveWAVDefaultBufferIsWiredTuned(t *testing.T) {
	if broadcast.DefaultBufferMS != 120 {
		t.Fatalf("default buffer %d ms, want 120", broadcast.DefaultBufferMS)
	}
	for query, want := range map[string]int{"": 120, "buffer=0": 0, "buffer=2000": 2000, "buffer=45": 45, "buffer=2001": 120, "buffer=-1": 120, "buffer=x": 120} {
		if got := liveBufferMS(httptest.NewRequest("GET", "/live.wav?"+query, nil)); got != want {
			t.Errorf("?%s: buffer %d ms, want %d", query, got, want)
		}
	}
	s, dir := newTestServer(t)
	enableLiveWithPCM(t, s, dir)
	srv := httptest.NewServer(s)
	defer srv.Close()
	time.Sleep(700 * time.Millisecond) // let the feed build history
	conn, _ := openLive(t, srv.URL, "format=pcm")
	defer conn.Close()
	got := readFor(t, conn, 60*time.Millisecond)
	// The cushion at once, then live audio for the 60 ms of the read (plus slack).
	if lo, hi := 120*192, (120+60+70)*192; len(got) < lo || len(got) > hi {
		t.Fatalf("first 60 ms brought %d bytes (%.0f ms), want the 120 ms cushion: %d..%d", len(got), float64(len(got))/192, lo, hi)
	}
}

// The console address and QR code use the wired address when the deck has a cable
// and Wi-Fi at once, whatever order the system lists them in (a deck listed wlan0
// first, and the stream opened from that address ran over Wi-Fi).
func TestLANHostPrefersWired(t *testing.T) {
	both := []lanAddr{{"wlan0", "10.0.0.108", true}, {"eth0", "10.0.0.224", true}}
	if got := pickLANHost(both); got != "10.0.0.224" {
		t.Fatalf("cable and Wi-Fi: %s, want the wired address", got)
	}
	// A cable that is unplugged (no link) must not hide the working Wi-Fi address.
	if got := pickLANHost([]lanAddr{{"eth0", "10.0.0.224", false}, {"wlan0", "10.0.0.108", true}}); got != "10.0.0.108" {
		t.Fatalf("unplugged cable: %s, want the Wi-Fi address", got)
	}
	if got := pickLANHost([]lanAddr{{"wlan0", "10.0.0.7", true}}); got != "10.0.0.7" {
		t.Fatalf("Wi-Fi only: %s", got)
	}
	if got := pickLANHost([]lanAddr{{"wlan0", "10.0.0.7", false}}); got != "10.0.0.7" {
		t.Fatalf("no link reported: %s, want the only address", got)
	}
	if got := pickLANHost(nil); got != "enginex.lan" {
		t.Fatalf("no address: %s, want the name", got)
	}
}

// smallSendBuffer gives every accepted connection a small kernel send buffer, so a
// listener that stops reading blocks the handler's Write within a second of audio.
type smallSendBuffer struct{ net.Listener }

func (l smallSendBuffer) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(8 << 10)
	}
	return c, err
}

// BH2-A3-2: /live.mp3 had no write deadline at all. A listener that went away
// without closing kept its handler, encoder process and hub subscription until
// TCP gave up (about fifteen minutes).
func TestLiveMP3DropsAListenerThatStoppedReading(t *testing.T) {
	old := liveMP3Stall
	liveMP3Stall = 300 * time.Millisecond
	t.Cleanup(func() { liveMP3Stall = old })
	s, dir := newTestServer(t)
	enableLiveWithPCM(t, s, dir)
	bin := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "encoder.pid")
	if err := os.WriteFile(filepath.Join(bin, "lame"), []byte("#!/bin/sh\necho $$ > '"+pidFile+"'\nexec cat\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/bin:/usr/bin")
	srv := httptest.NewUnstartedServer(s)
	srv.Listener = smallSendBuffer{srv.Listener}
	srv.Start()
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, 1448)
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 32<<10)
		})
	}}
	conn, err := d.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET /live.mp3 HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// The stream is flowing: status line and some audio. Then the listener reads no more.
	if got := readFor(t, conn, 500*time.Millisecond); !bytes.HasPrefix(got, []byte("HTTP/1.1 200")) || len(got) < 4096 {
		t.Fatalf("no stream before the stall: %d bytes %q", len(got), got[:min(len(got), 40)])
	}
	if n := s.broadcaster().Status().Listeners; n != 1 {
		t.Fatalf("listeners while streaming = %d, want 1", n)
	}
	pid, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.broadcaster().Status().Listeners != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the listener stopped reading 5 s ago and still holds its place (stall limit %v)", liveMP3Stall)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat("/proc/" + strings.TrimSpace(string(pid))); err == nil {
		t.Fatalf("encoder process %s outlived its listener", strings.TrimSpace(string(pid)))
	}
}

// Real deck, r3: a network outage of more than ~10 s to a /live.wav listener
// made the deck close it at its 10 s write deadline; the FIN was lost in the
// outage, so OBS stayed ESTABLISHED with no data and played silence until the
// source was restarted. A 15 s stall must be ridden out: the stream resumes,
// frame-aligned, with the backlog still bounded by the listener's trim.
func TestLiveWAVSurvivesA15SecondStall(t *testing.T) {
	if testing.Short() {
		t.Skip("15 s stall")
	}
	t.Parallel()
	s, dir := newTestServer(t)
	enableLiveWithPCM(t, s, dir)
	srv := httptest.NewServer(s)
	defer srv.Close()
	conn, _ := openLive(t, srv.URL, "format=pcm&buffer=300")
	defer conn.Close()
	if got := readFor(t, conn, 200*time.Millisecond); len(got) == 0 {
		t.Fatal("no audio before the stall")
	}
	time.Sleep(15 * time.Second) // the outage: nothing read
	backlog := readFor(t, conn, 150*time.Millisecond)
	if limit := 96000 + 2*16384 + 64<<10 + 150*192 + 100*192; len(backlog) > limit {
		t.Fatalf("backlog after a 15 s stall: %d bytes (%.0f ms), want <= %d", len(backlog), float64(len(backlog))/192, limit)
	}
	more := readFor(t, conn, 500*time.Millisecond)
	if len(more) < 50*192 {
		t.Fatalf("stream did not resume after a 15 s stall: %d bytes in 500 ms (backlog %d)", len(more), len(backlog))
	}
	all := append(backlog, more...)
	for i := 0; i+4 <= len(all)&^3; i += 4 {
		if !bytes.Equal(all[i:i+4], []byte{1, 2, 3, 4}) {
			t.Fatalf("frame %d after the stall is misaligned: %v", i/4, all[i:i+4])
		}
	}
}

// The hijacked /live.wav connection probes an idle peer every 5 s after 15 s
// (4 probes), so a listener that went away for good is noticed in about 35 s;
// Go's listener default was 15 s, 15 s, 9 probes (about 2.5 min).
func TestLiveWAVConnectionHasKeepAlive(t *testing.T) {
	s, dir := newTestServer(t)
	enableLiveWithPCM(t, s, dir)
	srv := httptest.NewUnstartedServer(s)
	conns := make(chan net.Conn, 4)
	srv.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns <- c
		}
	}
	srv.Start()
	defer srv.Close()
	conn, _ := openLive(t, srv.URL, "format=pcm")
	defer conn.Close()
	readFor(t, conn, 100*time.Millisecond)
	tc, ok := (<-conns).(*net.TCPConn)
	if !ok {
		t.Fatal("not a TCP connection")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var on, idle, intvl, cnt int
	raw.Control(func(fd uintptr) {
		on, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
		idle, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
		intvl, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL)
		cnt, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT)
	})
	if on != 1 || idle != 15 || intvl != 5 || cnt != 4 {
		t.Fatalf("keepalive on=%d idle=%d intvl=%d cnt=%d, want 1/15/5/4", on, idle, intvl, cnt)
	}
}

// deadlineWriter is a ResponseWriter that cannot be hijacked (HTTP/2) and
// records the write deadlines set through http.ResponseController.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

// The non-hijack fallback kept the server's two-minute WriteTimeout for the
// whole stream (r3 review); it sets a per-write deadline like the hijacked path.
func TestLiveWAVFallbackSetsAPerWriteDeadline(t *testing.T) {
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	write, done := liveWriter(w)
	defer done()
	for i := 0; i < 2; i++ {
		if err := write([]byte{1, 2, 3, 4}); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.deadlines) != 2 {
		t.Fatalf("deadlines set: %d, want one per write", len(w.deadlines))
	}
	if d := time.Until(w.deadlines[1]); d < 50*time.Second || d > 61*time.Second {
		t.Fatalf("write deadline %v ahead, want the 60 s stall timeout", d)
	}
}

// #234 review R4: the hook's feed is never saved by name (a console that sends the whole status
// back would otherwise pin this release's name, and an older release after a downgrade would read
// a feed nobody writes); a feed of the user's own is saved as given.
func TestRename234BuiltinInputIsNotSaved(t *testing.T) {
	for in, saved := range map[string]string{
		"/tmp/enginex-broadcast.pcm": "",
		"/tmp/sc5000-broadcast.pcm":  "",
		"/tmp/my-mix.pcm":            "/tmp/my-mix.pcm",
	} {
		s, stateDir := newTestServer(t)
		body, _ := json.Marshal(map[string]any{"inputPath": in, "format": "wav"})
		if resp := doAuthRequest(t, s, "POST", "/api/broadcast", body); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", in, resp.StatusCode)
		}
		var cfg broadcast.Config
		b, err := os.ReadFile(filepath.Join(stateDir, "broadcast-config.json"))
		if err != nil || json.Unmarshal(b, &cfg) != nil {
			t.Fatalf("%s: %v %s", in, err, b)
		}
		if cfg.InputPath != saved {
			t.Errorf("%s saved as %q, want %q", in, cfg.InputPath, saved)
		}
	}
}
