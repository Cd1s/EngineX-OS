package webui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONSettingsDispatchSpecialFields(t *testing.T) {
	for _, prefix := range []string{"/api", "/v1/enginex", "/v1/sc5000"} {
		t.Run(prefix, func(t *testing.T) {
			state, ui := t.TempDir(), t.TempDir()
			s := New(Config{AccessToken: "test-code", StateDir: state, UIDir: ui, ZoneinfoDir: seedZoneinfoFixture(t)}, nil)
			defer s.Close()
			post := func(body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", prefix+"/ui", strings.NewReader(body))
				r.RemoteAddr = "127.0.0.1:1234"
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer test-code")
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				return w
			}
			w := post(`{"timezone":"Asia/Tokyo"}`)
			var result struct {
				Saved bool `json:"timezoneSaved"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.Saved {
				t.Errorf("timezone JSON: %d %s", w.Code, w.Body.String())
			}
			if b, err := os.ReadFile(filepath.Join(state, "timezone")); err != nil || string(b) != "Asia/Tokyo\n" {
				t.Errorf("timezone did not persist: %q %v", b, err)
			}
			// A missing FX hook must be reported, not treated as a saved setting.
			if w := post(`{"fxUnit":1,"fxMode":2}`); w.Code != 503 {
				t.Errorf("FX JSON without hook: %d %s", w.Code, w.Body.String())
			}
			if _, err := os.Stat(filepath.Join(ui, "settings.conf")); !os.IsNotExist(err) {
				t.Errorf("special requests wrote general settings: %v", err)
			}
		})
	}
}

func TestMalformedJSONSettingsDoNotApplyOrRestart(t *testing.T) {
	dir, restarts := t.TempDir(), 0
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), RestartEngine: func() error { restarts++; return nil }}, nil)
	defer s.Close()
	before := []byte("lang=en\nwaveform=rgb\n")
	path := filepath.Join(dir, "settings.conf")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"lang":`, `{"junk":"` + strings.Repeat("x", 2*jsonBodyLimit)} {
		r := httptest.NewRequest("POST", "/api/ui?lang=es&restart=1", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.uiUpdate(w, r)
		if w.Code != 400 {
			t.Errorf("malformed JSON status = %d, want 400", w.Code)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != string(before) || restarts != 0 {
			t.Errorf("invalid request changed settings or restarted Engine: unchanged=%t, restarts=%d, error=%v", string(b) == string(before), restarts, err)
		}
	}
}
