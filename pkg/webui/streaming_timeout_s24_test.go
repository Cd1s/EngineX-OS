package webui

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// TestJobsFetchPrimaryCallerRespectsContextCancel verifies S24: the first caller
// starting a track fetch must unblock promptly when its wait context cancels or
// expires, while the shared background download still finishes for later loads.
func TestJobsFetchPrimaryCallerRespectsContextCancel(t *testing.T) {
	var audio bytes.Buffer
	if err := testaudio.FLAC(&audio, testaudio.Song(1)); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/audio":
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			w.Header().Set("Content-Type", "audio/flac")
			w.Write(audio.Bytes())
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	released := false
	unblock := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer unblock()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":200,"data":[{"id":77,"url":"`+srv.URL+`/audio","type":"flac","br":999000,"freeTrialInfo":null}]}`)
	}))
	defer api.Close()

	dir := t.TempDir()
	j := NewJobs(Pipeline{
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: dir},
	})
	defer j.Close()
	j.neteaseDirect = func() bool { return true }
	j.netease = func(string, bool) *providers.Netease {
		return &providers.Netease{BaseURL: api.URL, UserID: 1, Cookie: "MUSIC_U=x"}
	}

	tr := providers.Track{ID: "77", Provider: "netease", Title: "Slow Track", Artist: "DJ"}
	waitCtx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := j.Fetch(waitCtx, "s", tr, false)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("audio fetch never reached upstream")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected deadline error for cancelled primary caller")
		}
	case <-time.After(600 * time.Millisecond):
		t.Fatal("Jobs.Fetch primary caller ignored wait context cancellation and stayed blocked")
	}

	// Let the background download finish; the next caller must hit warm cache without a second upstream request.
	unblock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := j.File(tr); p != "" {
			if calls.Load() != 1 {
				t.Fatalf("expected 1 upstream audio request, got %d", calls.Load())
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("background fetch did not populate cache after primary caller gave up")
}

// TestKnownCodecUnblocksOnClientCancel verifies S24: cancelling a playback-info
// manifest request (Engine's "Fetching Track Info" / "Download Queued" phase)
// must unblock the gateway HTTP handler immediately instead of stranding it for
// the 30-minute download job timeout.
func TestKnownCodecUnblocksOnClientCancel(t *testing.T) {
	var audio bytes.Buffer
	if err := testaudio.FLAC(&audio, testaudio.Song(1)); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	audioSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		w.Header().Set("Content-Type", "audio/flac")
		w.Write(audio.Bytes())
	}))
	defer audioSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":200,"data":[{"id":88,"url":"`+audioSrv.URL+`/audio","type":"flac","br":999000,"freeTrialInfo":null}]}`)
	}))
	defer apiSrv.Close()

	dir := t.TempDir()
	j := NewJobs(Pipeline{
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: dir, AllowLossy: true},
	})
	defer j.Close()
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), NeteaseURL: apiSrv.URL}, j)
	defer s.Close()
	j.neteaseDirect = func() bool { return true }
	j.netease = func(string, bool) *providers.Netease {
		return &providers.Netease{BaseURL: apiSrv.URL, UserID: 1, Cookie: "MUSIC_U=x"}
	}

	tr := providers.Track{ID: "88", Provider: "netease", Title: "Wedged", Artist: "DJ"}
	id := s.tidal.addTrack(trackRef{track: tr, session: "owner"})

	handlerDone := make(chan struct{})
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		s.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()
	defer close(release) // LIFO: unblock upstream BEFORE srv.Close / j.Close / audioSrv.Close

	reqCtx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(reqCtx, "GET", srv.URL+"/v1/tracks/"+strconv.FormatInt(id, 10)+"/playbackinfopostpaywall", nil)
	go func() {
		resp, err := (&http.Client{}).Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("manifest request never started upstream fetch")
	}

	cancel()

	select {
	case <-handlerDone:
	case <-time.After(600 * time.Millisecond):
		t.Fatal("playbackinfopostpaywall handler remained stuck after client cancelled")
	}
}

// TestKnownCodecReturns502OnManifestFetchTimeout verifies S24: when a cold
// track fetch exceeds manifestFetchTimeout, playbackinfopostpaywall returns a
// clean HTTP 502 before Engine's curl timeout, while the background fetch still
// finishes and populates the cache for the next tap.
func TestKnownCodecReturns502OnManifestFetchTimeout(t *testing.T) {
	prev := manifestFetchTimeout
	manifestFetchTimeout = 120 * time.Millisecond
	defer func() { manifestFetchTimeout = prev }()

	var audio bytes.Buffer
	if err := testaudio.FLAC(&audio, testaudio.Song(1)); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	audioSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "audio/flac")
		w.Write(audio.Bytes())
	}))
	defer audioSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":200,"data":[{"id":89,"url":"`+audioSrv.URL+`/audio","type":"flac","br":999000,"freeTrialInfo":null}]}`)
	}))
	defer apiSrv.Close()

	j := NewJobs(Pipeline{
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: t.TempDir(), AllowLossy: true},
	})
	defer j.Close()
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), NeteaseURL: apiSrv.URL}, j)
	defer s.Close()
	j.neteaseDirect = func() bool { return true }
	j.netease = func(string, bool) *providers.Netease {
		return &providers.Netease{BaseURL: apiSrv.URL, UserID: 1, Cookie: "MUSIC_U=x"}
	}

	tr := providers.Track{ID: "89", Provider: "netease", Title: "SlowManifest", Artist: "DJ"}
	id := s.tidal.addTrack(trackRef{track: tr, session: "owner"})
	srv := httptest.NewServer(s)
	defer srv.Close()

	released := false
	unblock := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer unblock()

	start := time.Now()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(srv.URL + "/v1/tracks/" + strconv.FormatInt(id, 10) + "/playbackinfopostpaywall")
	if err != nil {
		t.Fatalf("manifest request failed at transport layer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502 on slow manifest fetch, got %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Fatalf("manifest timeout took too long: %s", elapsed)
	}

	unblock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if j.File(tr) != "" {
			resp2, err := (&http.Client{Timeout: 2 * time.Second}).Get(srv.URL + "/v1/tracks/" + strconv.FormatInt(id, 10) + "/playbackinfopostpaywall")
			if err != nil {
				t.Fatalf("warm retry failed: %v", err)
			}
			resp2.Body.Close()
			if resp2.StatusCode != http.StatusOK {
				t.Fatalf("warm retry expected 200, got %d", resp2.StatusCode)
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("background fetch never finished after manifest 502")
}

// TestJobsAddCoverStalledUpstreamDoesNotDelayFetch verifies S24: a hanging
// artwork server must be bounded by coverEmbedTimeout so track loading never
// stalls waiting on optional cover embedding.
func TestJobsAddCoverStalledUpstreamDoesNotDelayFetch(t *testing.T) {
	prev := coverEmbedTimeout
	coverEmbedTimeout = 100 * time.Millisecond
	defer func() { coverEmbedTimeout = prev }()

	var audio bytes.Buffer
	if err := testaudio.FLAC(&audio, testaudio.Song(1)); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	coverSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer coverSrv.Close()

	audioSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		w.Write(audio.Bytes())
	}))
	defer audioSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":200,"data":[{"id":90,"url":"`+audioSrv.URL+`/audio","type":"flac","br":999000,"freeTrialInfo":null}]}`)
	}))
	defer apiSrv.Close()

	j := NewJobs(Pipeline{
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: t.TempDir(), Media: coverSrv.Client()},
	})
	defer j.Close()
	defer close(release) // LIFO: unblock coverSrv before j.Close & coverSrv.Close
	j.neteaseDirect = func() bool { return true }
	j.netease = func(string, bool) *providers.Netease {
		return &providers.Netease{BaseURL: apiSrv.URL, UserID: 1, Cookie: "MUSIC_U=x"}
	}

	tr := providers.Track{ID: "90", Provider: "netease", Title: "CoverStall", Artist: "DJ", CoverURL: coverSrv.URL + "/cover.jpg"}
	start := time.Now()
	p, err := j.Fetch(context.Background(), "s", tr, false)
	if err != nil || p == "" {
		t.Fatalf("Fetch failed when only cover stalled: path=%q err=%v", p, err)
	}
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Fatalf("stalled cover delayed Fetch for %s", elapsed)
	}
}

// TestTidalProxyRespectsBoundedWriteDeadline verifies S24: proxyToTidal must not
// strip the HTTP server's write deadline into an unbounded infinite wait when a
// slow-trickle TIDAL upstream writes past the server's WriteTimeout.
func TestTidalProxyRespectsBoundedWriteDeadline(t *testing.T) {
	release := make(chan struct{})
	chunk := bytes.Repeat([]byte("a"), 8192)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-ticker.C:
			case <-r.Context().Done():
				return
			case <-release:
				return
			}
		}
	}))
	defer up.Close()

	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), TidalAPIURL: up.URL}, nil)
	defer s.Close()
	s.uiMu.Lock()
	v := s.loadUI()
	v.Scope = "tidal"
	_ = s.saveUI(v)
	s.uiMu.Unlock()

	srv := httptest.NewUnstartedServer(s)
	srv.Config.WriteTimeout = 150 * time.Millisecond
	srv.Start()
	defer srv.Close()
	defer close(release) // LIFO: unblock upstream BEFORE srv.Close & up.Close

	req, _ := http.NewRequest("GET", srv.URL+"/v1/tracks/12345/playbackinfopostpaywall", nil)
	req.Header.Set("Authorization", "Bearer real-tidal-token")

	done := make(chan error, 1)
	go func() {
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			done <- err
			return
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- err
	}()

	select {
	case <-done:
	case <-time.After(700 * time.Millisecond):
		t.Fatal("proxyToTidal cleared server WriteTimeout and hung indefinitely on slow upstream")
	}
}

// TestTidalProxyHeaderTimeoutReturns502 verifies S24: real-TIDAL passthrough
// requests (track info, playbackinfopostpaywall, playlists) fail fast with HTTP
// 502 when upstream accepts a connection and stalls before sending headers.
func TestTidalProxyHeaderTimeoutReturns502(t *testing.T) {
	prevHdr, prevTot := tidalProxyHeaderTimeout, tidalProxyTimeout
	tidalProxyHeaderTimeout = 120 * time.Millisecond
	tidalProxyTimeout = 500 * time.Millisecond
	defer func() {
		tidalProxyHeaderTimeout = prevHdr
		tidalProxyTimeout = prevTot
	}()

	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer up.Close()

	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), TidalAPIURL: up.URL}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	defer close(release)

	uiPost(t, srv.URL, "scope=tidal")

	req, _ := http.NewRequest("GET", srv.URL+"/v1/tracks/42/playbackinfopostpaywall", nil)
	req.Header.Set("Authorization", "Bearer real-tidal-token")

	start := time.Now()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("proxy request errored instead of returning 502: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502 on stalled TIDAL headers, got %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Fatalf("TIDAL header timeout took too long: %s", elapsed)
	}
}

// TestTidalProxy401RetryHeaderTimeoutReturns502 verifies S24: after a 401
// triggers real-TIDAL token rotation, the retry uses the bounded proxy
// transport (not bare http.DefaultTransport) and returns HTTP 502 if upstream
// stalls while preserving the rotated credentials on disk.
func TestTidalProxy401RetryHeaderTimeoutReturns502(t *testing.T) {
	prevHdr, prevTot := tidalProxyHeaderTimeout, tidalProxyTimeout
	tidalProxyHeaderTimeout = 120 * time.Millisecond
	tidalProxyTimeout = 800 * time.Millisecond
	defer func() {
		tidalProxyHeaderTimeout = prevHdr
		tidalProxyTimeout = prevTot
	}()

	release := make(chan struct{})
	var apiCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh"}`)
			return
		}
		if apiCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		<-release // retry stalls before sending response headers
	}))
	defer up.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tidal-auth.json"), []byte(`{"accessToken":"fixture-old-access","refreshToken":"fixture-refresh","clientId":"fixture-client"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: dir, TidalAPIURL: up.URL, TidalAuthURL: up.URL}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	defer close(release)

	uiPost(t, srv.URL, "scope=tidal")

	req, _ := http.NewRequest("GET", srv.URL+"/v1/tracks/42/playbackinfopostpaywall", nil)
	req.Header.Set("Authorization", "Bearer enginex-token")

	start := time.Now()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("proxy 401 retry failed at transport layer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502 when 401 retry stalls, got %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Fatalf("TIDAL 401 retry timeout took too long: %s", elapsed)
	}
	if apiCalls.Load() != 2 {
		t.Fatalf("expected 2 API calls (initial 401 + retry), got %d", apiCalls.Load())
	}
	stored, err := os.ReadFile(filepath.Join(dir, "tidal-auth.json"))
	if err != nil || !strings.Contains(string(stored), "fixture-new-refresh") {
		t.Fatalf("rotated TIDAL credentials were not preserved: %q (%v)", stored, err)
	}
}

// TestTidalProxyBodyStallAbortsWithinProxyTimeout verifies S24: if real-TIDAL
// upstream sends 200 OK headers and then stalls mid-body, tidalProxyTimeout
// cancels the proxy context and aborts the transfer cleanly.
func TestTidalProxyBodyStallAbortsWithinProxyTimeout(t *testing.T) {
	prevHdr, prevTot := tidalProxyHeaderTimeout, tidalProxyTimeout
	tidalProxyHeaderTimeout = 500 * time.Millisecond
	tidalProxyTimeout = 150 * time.Millisecond
	defer func() {
		tidalProxyHeaderTimeout = prevHdr
		tidalProxyTimeout = prevTot
	}()

	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"trackId":42`))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer up.Close()

	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), TidalAPIURL: up.URL}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	defer close(release)

	uiPost(t, srv.URL, "scope=tidal")

	req, _ := http.NewRequest("GET", srv.URL+"/v1/tracks/42/playbackinfopostpaywall", nil)
	req.Header.Set("Authorization", "Bearer real-tidal-token")

	start := time.Now()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected stream error on stalled TIDAL body")
	}
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Fatalf("TIDAL mid-body stall took too long to abort: %s", elapsed)
	}
}

// TestPreviewAnalyzerHasBoundedHTTPClient verifies S24: BPM preview fetching
// never relies on bare http.DefaultClient (which has no timeout).
func TestPreviewAnalyzerHasBoundedHTTPClient(t *testing.T) {
	if previewHTTPClient == http.DefaultClient {
		t.Fatal("previewHTTPClient must not be http.DefaultClient")
	}
	if previewHTTPClient.Timeout <= 0 {
		t.Fatalf("previewHTTPClient.Timeout=%s, want > 0", previewHTTPClient.Timeout)
	}
	tr, ok := previewHTTPClient.Transport.(*http.Transport)
	if !ok || tr == nil || tr.ResponseHeaderTimeout <= 0 {
		t.Fatalf("previewHTTPClient transport lacks ResponseHeaderTimeout: %#v", previewHTTPClient.Transport)
	}
}
