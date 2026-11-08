package webui

import (
	"encoding/json"
	"net/http/httptest"
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
	b, _ := json.Marshal(map[string]any{"action": "configure", "config": c})
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
