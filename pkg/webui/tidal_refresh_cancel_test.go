package webui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Review of 9ae30ea: the refresh request carried the caller's context. When Engine gave up
// while TIDAL was rotating the tokens, the request was cancelled after upstream had already
// invalidated the old refresh token, the rotated pair was never stored, and the real TIDAL
// user was logged out at the next refresh. The refresh now runs to its end.
func TestTidalRefreshFinishesWhenTheCallerCancels(t *testing.T) {
	inFlight := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	sent := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		seen = append(seen, r.Form.Get("refresh_token"))
		first := len(seen) == 1
		mu.Unlock()
		if first {
			close(inFlight)
			<-release // upstream has rotated the pair; the old refresh token is now invalid
		}
		w.Header().Set("Content-Type", "application/json")
		if first {
			io.WriteString(w, `{"access_token":"fixture-rotated-access","refresh_token":"fixture-rotated-refresh"}`)
			return
		}
		io.WriteString(w, `{"access_token":"fixture-third-access","refresh_token":"fixture-third-refresh"}`)
	}))
	defer up.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tidal-auth.json"), []byte(`{"accessToken":"fixture-old-access","refreshToken":"fixture-old-refresh","clientId":"fixture-client"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: dir, TidalAuthURL: up.URL}, nil)
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.tidal.refreshRealAuth(ctx, "fixture-old-access")
		done <- err
	}()
	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh never reached upstream")
	}
	cancel() // Engine gives up mid-refresh
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh did not return")
	}

	stored, err := os.ReadFile(filepath.Join(dir, "tidal-auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored), "fixture-rotated-refresh") || !strings.Contains(string(stored), "fixture-rotated-access") {
		t.Fatalf("the rotated tokens were lost when the caller cancelled: %s", stored)
	}
	// The next refresh must present the rotated refresh token, not the invalidated one.
	if _, err := s.tidal.refreshRealAuth(context.Background(), "fixture-rotated-access"); err != nil {
		t.Fatal(err)
	}
	if got := sent(); len(got) != 2 || got[1] != "fixture-rotated-refresh" {
		t.Fatalf("refresh tokens sent upstream: %q", got)
	}
}
