package webui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/dmx"
)

func TestDMXRoutesAuthPersistenceAndClock(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{AccessToken: "test-dmx-access", UIDir: dir, StateDir: t.TempDir()}, nil)
	defer s.Close()
	do := func(method, path, body, remote, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = remote
		r.Host = "127.0.0.1:8080"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/api/dmx", "", "192.0.2.1:1234", ""); w.Code != 401 {
		t.Fatal("unauthorized DMX API", w.Code)
	}
	if w := do("GET", "/v1/sc5000/dmx", "", "192.0.2.1:1234", ""); w.Code != 403 {
		t.Fatal("non-loopback DMX", w.Code)
	}
	w := do("GET", "/v1/sc5000/dmx", "", "127.0.0.1:1234", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) || strings.Contains(w.Body.String(), "totalNumberOfItems") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = do("GET", "/v1/sc5000/dmx.qml", "", "127.0.0.1:1234", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "import QtQuick 2.5") || strings.Contains(w.Body.String(), "QtQuick.Controls") {
		t.Fatal("standalone QML route", w.Code)
	}
	c := dmx.DefaultConfig()
	c.Enabled = true
	c.Blackout = false
	b, _ := json.Marshal(map[string]any{"action": "configure", "config": c, "revision": 0})
	w = do("POST", "/api/dmx", string(b), "192.0.2.1:1234", "test-dmx-access")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	b, _ = json.Marshal([]deckState{{Deck: 2, Master: true, BPM: 120, Pitch: 10, Beat: 2.5, Playing: true, At: time.Now().Add(-time.Millisecond).UnixMilli()}})
	if w = do("POST", "/v1/sc5000/dmx/clock", string(b), "127.0.0.1:1234", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if got := s.dmx.Status().Clock; got.BPM != 132 || got.Beat != 2.5 || got.Deck != 2 {
		t.Fatalf("master tempo/phase: %+v", got)
	}
	proxyBody, _ := json.Marshal([]deckState{{Deck: 1, Master: true, BPM: 100, Beat: 1, At: time.Now().UnixMilli()}})
	proxy := httptest.NewRequest("POST", "/v1/sc5000/decks", strings.NewReader(string(proxyBody)))
	proxy.RemoteAddr, proxy.Host = "127.0.0.1:1234", "127.0.0.1:8080"
	proxy.Header.Set("X-Forwarded-For", "192.0.2.1")
	proxyResult := httptest.NewRecorder()
	s.ServeHTTP(proxyResult, proxy)
	if proxyResult.Code != 200 || s.dmx.Status().Clock.Deck != 2 {
		t.Fatal("proxied legacy deck post affected DMX clock", proxyResult.Code)
	}
	// Native performance changes merge onto the latest patch, and do not
	// accept configuration or mutate other controls from a stale QML snapshot.
	w = do("POST", "/v1/sc5000/dmx", `{"action":"control","control":{"master":64}}`, "127.0.0.1:1234", "")
	if w.Code != 200 || s.dmx.Status().Config.Master != 64 {
		t.Fatal("native control", w.Code)
	}
	w = do("POST", "/v1/sc5000/dmx", string(b), "127.0.0.1:1234", "")
	if w.Code != 400 {
		t.Fatal("invalid native request", w.Code)
	}
	c.Master = 255
	b, _ = json.Marshal(map[string]any{"action": "configure", "config": c, "revision": 0})
	w = do("POST", "/v1/sc5000/dmx", string(b), "127.0.0.1:1234", "")
	if w.Code != 403 {
		t.Fatal("native patch accepted", w.Code)
	}
	w = do("POST", "/api/dmx", string(b), "192.0.2.1:1234", "test-dmx-access")
	if w.Code != 409 || s.dmx.Status().Config.Master != 64 {
		t.Fatal("stale full config accepted", w.Code)
	}
	w = do("POST", "/api/dmx", `{"action":"blackout"}`, "192.0.2.1:1234", "test-dmx-access")
	if w.Code != 200 || !s.dmx.Status().Config.Blackout {
		t.Fatal("blackout", w.Code)
	}
	s.Close()
	s2 := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	defer s2.Close()
	if got := s2.dmx.Status().Config; !got.Enabled || !got.Blackout {
		t.Fatal("saved opt-in and restart blackout", got)
	}
}

func TestDMXRejectsForwardedNativeRequests(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	for _, tc := range []struct{ host, header string }{{"public.example", ""}, {"127.0.0.1:8080", "X-Forwarded-For"}, {"127.0.0.1:8080", "Forwarded"}, {"127.0.0.1:8080", "CF-Connecting-IP"}} {
		for _, path := range []string{"/v1/sc5000/dmx", "/v1/sc5000/dmx.qml", "/v1/sc5000/dmx/clock"} {
			method := "GET"
			if strings.HasSuffix(path, "/clock") {
				method = "POST"
			}
			r := httptest.NewRequest(method, path, strings.NewReader("[]"))
			r.RemoteAddr = "127.0.0.1:1234"
			r.Host = tc.host
			if tc.header != "" {
				r.Header.Set(tc.header, "192.0.2.1")
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("proxy %s %s: %d", tc.header, path, w.Code)
			}
		}
	}
}

func TestDMXBlackoutAndDisableSurviveSaveFailure(t *testing.T) {
	badDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badDir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: badDir, StateDir: t.TempDir(), AccessToken: "test-dmx-access"}, nil)
	defer s.Close()
	for _, action := range []string{"blackout", "disable"} {
		c := dmx.DefaultConfig()
		c.Enabled = true
		if err := s.dmx.Apply(c); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/api/dmx", strings.NewReader(`{"action":"`+action+`"}`))
		r.Header.Set("Authorization", "Bearer test-dmx-access")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		got := s.dmx.Status().Config
		if w.Code != 500 || (action == "blackout" && !got.Blackout) || (action == "disable" && got.Enabled) {
			t.Fatalf("%s save failure: %d %+v", action, w.Code, got)
		}
	}
}
