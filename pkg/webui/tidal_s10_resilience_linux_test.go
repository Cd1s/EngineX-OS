//go:build linux

package webui

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func countInotifyOpens(t *testing.T, fd int) int {
	t.Helper()
	var events [4096]byte
	n, err := syscall.Read(fd, events[:])
	if err != nil && !errors.Is(err, syscall.EAGAIN) {
		t.Fatal(err)
	}
	if n <= 0 {
		return 0
	}
	return n / syscall.SizeofInotifyEvent
}

// S10: Healthy real-TIDAL requests must not synchronously open and decode
// tidal-auth.json on every request. An initialized gateway performs 0 file opens
// across repeated healthy prepareRealAuth and ServeHTTP requests, and a cold
// uninitialized struct loads at most once.
func TestS10HealthyAuthFileOpens(t *testing.T) {
	t.Run("initialized_prepare_zero_opens", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tidal-auth.json")
		if err := os.WriteFile(path, []byte(`{"accessToken":"test-access","refreshToken":"test-refresh"}`), 0600); err != nil {
			t.Fatal(err)
		}
		s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: dir}, nil)
		defer s.Close()

		fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
		if err != nil {
			t.Fatal(err)
		}
		defer syscall.Close(fd)
		if _, err := syscall.InotifyAddWatch(fd, path, syscall.IN_OPEN); err != nil {
			t.Fatal(err)
		}
		var opens int
		for i := 0; i < 32; i++ {
			r := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
			r.Header.Set("Authorization", "Bearer test-access")
			if !s.tidal.prepareRealAuth(httptest.NewRecorder(), r, "/v1/sessions", nil) {
				t.Fatal("healthy request refused")
			}
			opens += countInotifyOpens(t, fd)
		}
		t.Logf("initialized_prepare requests=32 auth_file_opens=%d", opens)
		if opens != 0 {
			t.Fatalf("healthy prepareRealAuth opened tidal-auth.json %d times in 32 requests, want 0", opens)
		}
	})

	t.Run("initialized_servehttp_zero_opens", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"userId":42}`)
		}))
		defer up.Close()

		dir := t.TempDir()
		path := filepath.Join(dir, "tidal-auth.json")
		if err := os.WriteFile(path, []byte(`{"accessToken":"test-access","refreshToken":"test-refresh"}`), 0600); err != nil {
			t.Fatal(err)
		}
		s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: dir, TidalAPIURL: up.URL, TidalAuthURL: up.URL}, nil)
		defer s.Close()
		srv := httptest.NewServer(s)
		defer srv.Close()
		uiPost(t, srv.URL, "scope=tidal")

		fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
		if err != nil {
			t.Fatal(err)
		}
		defer syscall.Close(fd)
		if _, err := syscall.InotifyAddWatch(fd, path, syscall.IN_OPEN); err != nil {
			t.Fatal(err)
		}
		var opens int
		for i := 0; i < 32; i++ {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
			req.Header.Set("Authorization", "Bearer test-access")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("req %d: status=%d, want 200", i, resp.StatusCode)
			}
			opens += countInotifyOpens(t, fd)
		}
		t.Logf("initialized_servehttp requests=32 auth_file_opens=%d", opens)
		if opens != 0 {
			t.Fatalf("healthy ServeHTTP opened tidal-auth.json %d times in 32 requests, want 0", opens)
		}
	})

	t.Run("cold_struct_single_load_only", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "tidal-auth.json")
		if err := os.WriteFile(path, []byte(`{"accessToken":"test-access","refreshToken":"test-refresh"}`), 0600); err != nil {
			t.Fatal(err)
		}
		g := &tidalGateway{s: &Server{cfg: Config{StateDir: dir}}}
		fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
		if err != nil {
			t.Fatal(err)
		}
		defer syscall.Close(fd)
		if _, err := syscall.InotifyAddWatch(fd, path, syscall.IN_OPEN); err != nil {
			t.Fatal(err)
		}
		var opens int
		for i := 0; i < 32; i++ {
			r := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
			r.Header.Set("Authorization", "Bearer test-access")
			if !g.prepareRealAuth(httptest.NewRecorder(), r, "/v1/sessions", nil) {
				t.Fatal("healthy request refused")
			}
			opens += countInotifyOpens(t, fd)
		}
		t.Logf("cold_struct_prepare requests=32 auth_file_opens=%d", opens)
		if opens > 1 {
			t.Fatalf("cold prepareRealAuth opened tidal-auth.json %d times in 32 requests, want <= 1", opens)
		}
	})
}

// S10: The in-memory auth cache must remain coherent across in-process token
// rotations (with 0 extra file opens), external atomic file replacements,
// clearRealAuth logouts, and external file deletions.
func TestS10AuthCacheCoherence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tidal-auth.json")
	if err := os.WriteFile(path, []byte(`{"accessToken":"token-1","refreshToken":"refresh-1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: dir}, nil)
	defer s.Close()

	// 1. In-process rotation updates cache coherently; subsequent upgrades open 0 files.
	if err := s.tidal.saveRealAuth(tidalAuth{AccessToken: "token-2", RefreshToken: "refresh-2"}); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, path, syscall.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		r.Header.Set("Authorization", "Bearer token-1")
		if !s.tidal.prepareRealAuth(httptest.NewRecorder(), r, "/v1/sessions", nil) {
			t.Fatal("prepareRealAuth failed after rotation")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-2" {
			t.Fatalf("Authorization = %q, want Bearer token-2", got)
		}
	}
	if opens := countInotifyOpens(t, fd); opens != 0 {
		t.Fatalf("post-rotation bearer upgrades opened tidal-auth.json %d times, want 0", opens)
	}

	// 2. External atomic replacement on disk invalidates the cached snapshot.
	tmp := filepath.Join(dir, ".external-auth")
	if err := os.WriteFile(tmp, []byte(`{"accessToken":"token-3-external","refreshToken":"refresh-3"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	r.Header.Set("Authorization", "Bearer token-1")
	if !s.tidal.prepareRealAuth(httptest.NewRecorder(), r, "/v1/sessions", nil) {
		t.Fatal("prepareRealAuth failed after external update")
	}
	if got := r.Header.Get("Authorization"); got != "Bearer token-3-external" {
		t.Fatalf("Authorization after external replace = %q, want Bearer token-3-external", got)
	}

	// 3. External deletion clears cached credentials immediately.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if s.tidal.hasRealAuth() {
		t.Fatal("hasRealAuth() = true after external deletion, want false")
	}

	// 4. Re-save then clearRealAuth clears cached credentials immediately.
	if err := s.tidal.saveRealAuth(tidalAuth{AccessToken: "token-4", RefreshToken: "refresh-4"}); err != nil {
		t.Fatal(err)
	}
	if !s.tidal.hasRealAuth() {
		t.Fatal("hasRealAuth() = false after saveRealAuth, want true")
	}
	if err := s.tidal.clearRealAuth(); err != nil {
		t.Fatal(err)
	}
	if s.tidal.hasRealAuth() {
		t.Fatal("hasRealAuth() = true after clearRealAuth, want false")
	}
}
