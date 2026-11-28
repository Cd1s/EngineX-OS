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

func TestReviewS10HealthyAuthFileOpens(t *testing.T) {
	for _, mode := range []string{"initialized_prepare", "initialized_HTTP", "cold_prepare"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "tidal-auth.json")
			if err := os.WriteFile(file, []byte(`{"accessToken":"review-access","refreshToken":"review-refresh"}`), 0600); err != nil {
				t.Fatal(err)
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"userId":42}`) }))
			defer up.Close()
			cfg := Config{AccessToken: "code", StateDir: dir, UIDir: t.TempDir(), TidalAPIURL: up.URL, TidalAuthURL: up.URL}
			var g *tidalGateway
			var serverURL string
			if mode == "cold_prepare" {
				g = &tidalGateway{s: &Server{cfg: cfg}}
			} else {
				s := New(cfg, nil)
				defer s.Close()
				g = s.tidal
				if mode == "initialized_HTTP" {
					srv := httptest.NewServer(s)
					defer srv.Close()
					serverURL = srv.URL
					uiPost(t, srv.URL, "scope=tidal")
				}
			}
			fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)
			if _, err = syscall.InotifyAddWatch(fd, file, syscall.IN_OPEN); err != nil {
				t.Fatal(err)
			}
			hits := 0
			for i := 0; i < 32; i++ {
				if mode == "initialized_HTTP" {
					r, err := http.NewRequest("GET", serverURL+"/v1/sessions", nil)
					if err != nil {
						t.Fatal(err)
					}
					r.Header.Set("Authorization", "Bearer review-access")
					res, err := http.DefaultClient.Do(r)
					if err != nil {
						t.Fatal(err)
					}
					io.Copy(io.Discard, res.Body)
					res.Body.Close()
					if res.StatusCode != 200 {
						t.Fatalf("HTTP %d", res.StatusCode)
					}
				} else {
					r := httptest.NewRequest("GET", "/v1/sessions", nil)
					r.Header.Set("Authorization", "Bearer review-access")
					if !g.prepareRealAuth(httptest.NewRecorder(), r, "/v1/sessions", nil) {
						t.Fatal("healthy request refused")
					}
				}
				var ev [4096]byte
				n, err := syscall.Read(fd, ev[:])
				if err != nil && !errors.Is(err, syscall.EAGAIN) {
					t.Fatal(err)
				}
				if n > 0 {
					hits++
				}
			}
			if mode == "cold_prepare" {
				if hits > 1 {
					t.Errorf("repeated cold opens: %d", hits)
				}
			} else if hits != 0 {
				t.Errorf("initialized gateway opened the credential file %d times", hits)
			}
		})
	}
}
