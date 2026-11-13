package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// TestRealEntryPointsLostUpdateInterleaving_PostPaused drives the real write entry points
// (bpmConfigPost and pullBPMConfig) with controlled interleaving where bpmConfigPost is paused
// after reading the config while pullBPMConfig writes. On unpatched read-then-write code,
// bpmConfigPost overwrites and loses the relay credentials every time.
func TestRealEntryPointsLostUpdateInterleaving_PostPaused(t *testing.T) {
	state := t.TempDir()
	bpm := &providers.BPMStore{
		Path:       filepath.Join(state, "bpm.json"),
		ConfigPath: filepath.Join(state, "bpm-config.json"),
	}
	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"config":{"soundcloudClientId":"relay-client-id","soundcloudClientSecret":"relay-secret"}}`)
	}))
	defer relaySrv.Close()

	ui := New(Config{
		AccessToken: "test-token",
		UIDir:       t.TempDir(),
		StateDir:    state,
		RelayURL:    relaySrv.URL,
		BPM:         bpm,
	}, nil)
	ui.updateRelayConfig(nil, ptr("pw"))

	writer1Paused := make(chan struct{})
	resumeWriter1 := make(chan struct{})

	testBPMConfigPostHook = func() {
		select {
		case writer1Paused <- struct{}{}:
			<-resumeWriter1
		default:
		}
	}
	t.Cleanup(func() {
		testBPMConfigPostHook = nil
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("POST", "/api/bpm", strings.NewReader(`{"getSongBpmKey":"user-key"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		ui.bpmConfigPost(w, req)
	}()

	select {
	case <-writer1Paused:
	case <-time.After(2 * time.Second):
		t.Fatal("writer 1 did not reach hook")
	}

	writer2Done := make(chan error, 1)
	go func() {
		writer2Done <- ui.pullBPMConfig(context.Background())
	}()

	select {
	case err := <-writer2Done:
		if err != nil {
			t.Fatalf("writer 2 failed: %v", err)
		}
		close(resumeWriter1)
		wg.Wait()
	case <-time.After(30 * time.Millisecond):
		close(resumeWriter1)
		wg.Wait()
		if err := <-writer2Done; err != nil {
			t.Fatalf("writer 2 failed: %v", err)
		}
	}

	cfg := bpm.Config()
	if cfg.SoundCloudClientID != "relay-client-id" || cfg.GetSongBPMKey != "user-key" {
		t.Fatalf("lost update: got %+v, want soundcloudClientId=%q getSongBpmKey=%q",
			cfg, "relay-client-id", "user-key")
	}
}

// TestRealEntryPointsLostUpdateInterleaving_RelayPaused drives the real write entry points
// where pullRelayConfig is paused after reading config while bpmConfigPost writes. On unpatched
// read-then-write code, pullRelayConfig overwrites and loses the user's updated key every time.
func TestRealEntryPointsLostUpdateInterleaving_RelayPaused(t *testing.T) {
	state := t.TempDir()
	cfgPath := filepath.Join(state, "bpm-config.json")
	bpm := &providers.BPMStore{
		Path:       filepath.Join(state, "bpm.json"),
		ConfigPath: cfgPath,
	}
	if err := bpm.SaveConfig(providers.BPMConfig{GetSongBPMKey: "initial-key"}); err != nil {
		t.Fatal(err)
	}

	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"config":{"soundcloudClientId":"relay-client-id","soundcloudClientSecret":"relay-secret"}}`)
	}))
	defer relaySrv.Close()

	ui := New(Config{
		AccessToken: "test-token",
		UIDir:       t.TempDir(),
		StateDir:    state,
		RelayURL:    relaySrv.URL,
		BPM:         bpm,
	}, nil)
	ui.updateRelayConfig(nil, ptr("pw"))

	relayPaused := make(chan struct{})
	resumeRelay := make(chan struct{})

	testRelayPullHook = func() {
		select {
		case relayPaused <- struct{}{}:
			<-resumeRelay
		default:
		}
	}
	t.Cleanup(func() {
		testRelayPullHook = nil
	})

	var wg sync.WaitGroup
	wg.Add(1)
	var relayErr error
	go func() {
		defer wg.Done()
		relayErr = ui.pullBPMConfig(context.Background())
	}()

	select {
	case <-relayPaused:
	case <-time.After(2 * time.Second):
		t.Fatal("relay poll did not reach hook")
	}

	postDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest("POST", "/api/bpm", strings.NewReader(`{"getSongBpmKey":"updated-user-key"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		ui.bpmConfigPost(w, req)
		postDone <- w.Code
	}()

	select {
	case code := <-postDone:
		if code != http.StatusOK {
			t.Fatalf("POST returned %d", code)
		}
		close(resumeRelay)
		wg.Wait()
	case <-time.After(30 * time.Millisecond):
		close(resumeRelay)
		wg.Wait()
		if code := <-postDone; code != http.StatusOK {
			t.Fatalf("POST returned %d", code)
		}
	}

	if relayErr != nil {
		t.Fatalf("relay error: %v", relayErr)
	}

	cfg := bpm.Config()
	if cfg.SoundCloudClientID != "relay-client-id" || cfg.GetSongBPMKey != "updated-user-key" {
		t.Fatalf("lost update: got %+v, want soundcloudClientId=%q getSongBpmKey=%q",
			cfg, "relay-client-id", "updated-user-key")
	}
}

// TestBPMConfigPostConcurrentPreservesFields tests that concurrent POST /api/bpm requests
// modifying different fields do not overwrite or lose each other's updates.
func TestBPMConfigPostConcurrentPreservesFields(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		state := t.TempDir()
		bpm := &providers.BPMStore{
			Path:       filepath.Join(state, "bpm.json"),
			ConfigPath: filepath.Join(state, "bpm-config.json"),
		}
		ui := New(Config{
			AccessToken: "test-token",
			UIDir:       t.TempDir(),
			StateDir:    state,
			BPM:         bpm,
		}, nil)

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)

		postReq := func(body string) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest("POST", "/api/bpm", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			ui.bpmConfigPost(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, body = %s", w.Code, w.Body.String())
			}
		}

		go postReq(fmt.Sprintf(`{"getSongBpmKey":"key-%d"}`, iter))
		go postReq(fmt.Sprintf(`{"soundcloudClientId":"cid-%d","soundcloudClientSecret":"sec-%d"}`, iter, iter))
		go postReq(fmt.Sprintf(`{"soundcloudToken":"tok-%d"}`, iter))

		close(start)
		wg.Wait()

		cfg := bpm.Config()
		wantKey := fmt.Sprintf("key-%d", iter)
		wantCID := fmt.Sprintf("cid-%d", iter)
		wantSec := fmt.Sprintf("sec-%d", iter)
		wantTok := fmt.Sprintf("tok-%d", iter)

		if cfg.GetSongBPMKey != wantKey || cfg.SoundCloudClientID != wantCID ||
			cfg.SoundCloudSecret != wantSec || cfg.SoundCloudToken != wantTok {
			t.Fatalf("iter %d: lost update detected in BPMConfig: got %+v, want key=%q cid=%q sec=%q tok=%q",
				iter, cfg, wantKey, wantCID, wantSec, wantTok)
		}
	}
}

// TestBPMConfigPostAndRelayPullConcurrent tests that concurrent HTTP POST /api/bpm
// and pullBPMConfig (from relay) do not overwrite each other.
func TestBPMConfigPostAndRelayPullConcurrent(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		relayCID := fmt.Sprintf("relay-cid-%d", iter)
		relaySec := fmt.Sprintf("relay-sec-%d", iter)
		relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := fmt.Sprintf(`{"ok":true,"config":{"soundcloudClientId":%q,"soundcloudClientSecret":%q}}`, relayCID, relaySec)
			io.WriteString(w, resp)
		}))

		state := t.TempDir()
		bpm := &providers.BPMStore{
			Path:       filepath.Join(state, "bpm.json"),
			ConfigPath: filepath.Join(state, "bpm-config.json"),
		}
		ui := New(Config{
			AccessToken: "test-token",
			UIDir:       t.TempDir(),
			StateDir:    state,
			RelayURL:    relaySrv.URL,
			BPM:         bpm,
		}, nil)
		ui.updateRelayConfig(nil, ptr("pw"))

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			if err := ui.pullBPMConfig(context.Background()); err != nil {
				t.Errorf("pullBPMConfig: %v", err)
			}
		}()

		userKey := fmt.Sprintf("user-bpm-key-%d", iter)
		go func() {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"getSongBpmKey":%q}`, userKey)
			req := httptest.NewRequest("POST", "/api/bpm", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			ui.bpmConfigPost(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("bpmConfigPost: %d %s", w.Code, w.Body.String())
			}
		}()

		close(start)
		wg.Wait()
		relaySrv.Close()

		cfg := bpm.Config()
		if cfg.GetSongBPMKey != userKey || cfg.SoundCloudClientID != relayCID || cfg.SoundCloudSecret != relaySec {
			t.Fatalf("iter %d: lost update between relay pull and user post: got %+v, want key=%q cid=%q sec=%q",
				iter, cfg, userKey, relayCID, relaySec)
		}
	}
}

// TestBPMConfigResponseAndRedaction verifies that GET and POST /api/bpm never leak raw keys
// and correctly reflect presence flags (soundcloudSet and getSongBpmSet).
func TestBPMConfigResponseAndRedaction(t *testing.T) {
	state := t.TempDir()
	bpm := &providers.BPMStore{
		Path:       filepath.Join(state, "bpm.json"),
		ConfigPath: filepath.Join(state, "bpm-config.json"),
	}
	ui := New(Config{
		AccessToken: "test-token",
		UIDir:       t.TempDir(),
		StateDir:    state,
		BPM:         bpm,
	}, nil)

	type responseMap map[string]bool

	// 1. Initially nothing set
	req := httptest.NewRequest("GET", "/api/bpm", nil)
	w := httptest.NewRecorder()
	ui.bpmConfigGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/bpm: %d", w.Code)
	}
	var res responseMap
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res["soundcloudSet"] || res["getSongBpmSet"] {
		t.Fatalf("expected all false, got: %+v", res)
	}

	// 2. Set getSongBpmKey only
	postBody := `{"getSongBpmKey":"super-secret-key"}`
	req = httptest.NewRequest("POST", "/api/bpm", strings.NewReader(postBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	ui.bpmConfigPost(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/bpm: %d", w.Code)
	}
	bodyStr := w.Body.String()
	if strings.Contains(bodyStr, "super-secret-key") {
		t.Fatalf("response leaked secret key: %s", bodyStr)
	}
	res = responseMap{}
	if err := json.NewDecoder(strings.NewReader(bodyStr)).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if !res["getSongBpmSet"] || res["soundcloudSet"] {
		t.Fatalf("expected getSongBpmSet=true, soundcloudSet=false, got: %+v", res)
	}

	// 3. Set soundCloudClientId only (without secret or token -> soundcloudSet should still be false)
	postBody = `{"soundcloudClientId":"only-client-id"}`
	req = httptest.NewRequest("POST", "/api/bpm", strings.NewReader(postBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	ui.bpmConfigPost(w, req)
	bodyStr = w.Body.String()
	if strings.Contains(bodyStr, "only-client-id") {
		t.Fatalf("response leaked client ID: %s", bodyStr)
	}
	res = responseMap{}
	if err := json.NewDecoder(strings.NewReader(bodyStr)).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if !res["getSongBpmSet"] || res["soundcloudSet"] {
		t.Fatalf("client-id alone should not set soundcloudSet: %+v", res)
	}

	// 4. Set soundCloudClientSecret -> now soundcloudSet should be true
	postBody = `{"soundcloudClientSecret":"now-with-secret"}`
	req = httptest.NewRequest("POST", "/api/bpm", strings.NewReader(postBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	ui.bpmConfigPost(w, req)
	bodyStr = w.Body.String()
	if strings.Contains(bodyStr, "now-with-secret") {
		t.Fatalf("response leaked secret: %s", bodyStr)
	}
	res = responseMap{}
	if err := json.NewDecoder(strings.NewReader(bodyStr)).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if !res["getSongBpmSet"] || !res["soundcloudSet"] {
		t.Fatalf("expected both true: %+v", res)
	}
}

// TestBPMConfigErrors verifies error handling when BPM is disabled, input is invalid, or save fails.
func TestBPMConfigErrors(t *testing.T) {
	// BPM is nil
	uiNoBPM := New(Config{
		AccessToken: "test-token",
		UIDir:       t.TempDir(),
		StateDir:    t.TempDir(),
		BPM:         nil,
	}, nil)

	req := httptest.NewRequest("GET", "/api/bpm", nil)
	w := httptest.NewRecorder()
	uiNoBPM.bpmConfigGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET with nil BPM should return 200, got: %d", w.Code)
	}

	req = httptest.NewRequest("POST", "/api/bpm", strings.NewReader(`{"getSongBpmKey":"key"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	uiNoBPM.bpmConfigPost(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST with nil BPM should return 503, got: %d", w.Code)
	}

	// Invalid JSON input
	state := t.TempDir()
	bpm := &providers.BPMStore{
		Path:       filepath.Join(state, "bpm.json"),
		ConfigPath: filepath.Join(state, "bpm-config.json"),
	}
	ui := New(Config{
		AccessToken: "test-token",
		UIDir:       t.TempDir(),
		StateDir:    state,
		BPM:         bpm,
	}, nil)

	req = httptest.NewRequest("POST", "/api/bpm", strings.NewReader(`{malformed json}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	ui.bpmConfigPost(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON should return 400, got: %d", w.Code)
	}

	// Extra JSON input
	req = httptest.NewRequest("POST", "/api/bpm", strings.NewReader(`{"getSongBpmKey":"k"}{"extra":"value"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	ui.bpmConfigPost(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("extra JSON should return 400, got: %d", w.Code)
	}

	// Unwritable config path (ConfigPath is empty)
	bpmNoPath := &providers.BPMStore{
		Path:       filepath.Join(state, "bpm.json"),
		ConfigPath: "",
	}
	uiNoPath := New(Config{
		AccessToken: "test-token",
		UIDir:       t.TempDir(),
		StateDir:    state,
		BPM:         bpmNoPath,
	}, nil)

	req = httptest.NewRequest("POST", "/api/bpm", strings.NewReader(`{"getSongBpmKey":"key"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	uiNoPath.bpmConfigPost(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unwritable config should return 500, got: %d", w.Code)
	}
}

// TestPullRelayConfigEmptyDoesNotWipeLocal verifies that empty relay responses do not wipe local keys.
func TestPullRelayConfigEmptyDoesNotWipeLocal(t *testing.T) {
	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"config":{}}`)
	}))
	defer relaySrv.Close()

	state := t.TempDir()
	bpm := &providers.BPMStore{
		Path:       filepath.Join(state, "bpm.json"),
		ConfigPath: filepath.Join(state, "bpm-config.json"),
	}
	if err := bpm.SaveConfig(providers.BPMConfig{
		GetSongBPMKey:      "local-bpm-key",
		SoundCloudClientID: "local-cid",
	}); err != nil {
		t.Fatal(err)
	}

	ui := New(Config{
		AccessToken: "test-token",
		UIDir:       t.TempDir(),
		StateDir:    state,
		RelayURL:    relaySrv.URL,
		BPM:         bpm,
	}, nil)
	ui.updateRelayConfig(nil, ptr("pw"))

	if err := ui.pullBPMConfig(context.Background()); err != nil {
		t.Fatalf("pullBPMConfig: %v", err)
	}

	cfg := bpm.Config()
	if cfg.GetSongBPMKey != "local-bpm-key" || cfg.SoundCloudClientID != "local-cid" {
		t.Fatalf("expected local keys to be preserved, got: %+v", cfg)
	}
}

// TestPullRelayConfigReadOnlyDirNoOp proves that unchanged relay poll does not rewrite disk.
func TestPullRelayConfigReadOnlyDirNoOp(t *testing.T) {
	cfgDir := t.TempDir()
	if err := syscall.Mount("tmpfs", cfgDir, "tmpfs", 0, ""); err != nil {
		t.Skipf("mounting tmpfs not permitted: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Unmount(cfgDir, syscall.MNT_DETACH)
	})

	cfgPath := filepath.Join(cfgDir, "bpm-config.json")
	initialCfg := providers.BPMConfig{
		SoundCloudClientID: "existing-client",
		SoundCloudSecret:   "existing-secret",
	}
	data, _ := json.Marshal(initialCfg)
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := syscall.Mount("", cfgDir, "", uintptr(syscall.MS_REMOUNT|syscall.MS_RDONLY), ""); err != nil {
		t.Fatalf("remount ro failed: %v", err)
	}

	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"config":{"soundcloudClientId":"existing-client","soundcloudClientSecret":"existing-secret"}}`)
	}))
	defer relaySrv.Close()

	bpm := &providers.BPMStore{
		Path:       filepath.Join(cfgDir, "bpm.json"),
		ConfigPath: cfgPath,
	}
	ui := New(Config{
		AccessToken: "test-token",
		UIDir:       t.TempDir(),
		StateDir:    t.TempDir(),
		RelayURL:    relaySrv.URL,
		BPM:         bpm,
	}, nil)
	ui.updateRelayConfig(nil, ptr("pw"))

	err := ui.pullBPMConfig(context.Background())
	if err != nil {
		t.Fatalf("expected no-op skip on unchanged relay config in read-only dir, got error: %v", err)
	}
}
