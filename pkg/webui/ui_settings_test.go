package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func uiPost(t *testing.T, base, query string) (int, uiSettings) {
	t.Helper()
	resp, err := http.Post(base+"/v1/sc5000/ui?"+query, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v uiSettings
	json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func TestUISettingsPersistAndRestart(t *testing.T) {
	dir := t.TempDir()
	restarts := 0
	cfg := Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir(), RestartEngine: func() error { restarts++; return nil }}
	srv := httptest.NewServer(New(cfg, nil))
	defer srv.Close()

	if code, v := uiPost(t, srv.URL, "waveform=3band&sources=spotify,netease&scope=spotify"); code != 200 || v.Waveform != "3band" || v.Scope != "spotify" || !slices.Equal(v.Sources, []string{"netease", "spotify"}) || restarts != 0 {
		t.Fatalf("%d %+v restarts=%d", code, v, restarts)
	}
	if err := os.WriteFile(filepath.Join(dir, "safe-mode"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, v := uiPost(t, srv.URL, "lang=zh-Hant&restart=1"); code != 200 || v.Language != "zh-Hant" || v.SafeMode || restarts != 1 {
		t.Fatalf("%d %+v restarts=%d", code, v, restarts)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil || !slices.Contains(splitLines(data), "lang=zh-Hant") {
		t.Fatalf("hook cannot read the language: %q %v", data, err)
	}
	// A second server, as after a reboot, sees the same settings.
	again := httptest.NewServer(New(cfg, nil))
	defer again.Close()
	if _, v := uiPost(t, again.URL, ""); v.Waveform != "3band" || v.Language != "zh-Hant" {
		t.Fatalf("not persisted: %+v", v)
	}
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	return out
}

func TestUISettingsRejectBadValues(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(New(Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir()}, nil))
	defer srv.Close()
	for _, q := range []string{"lang=../../etc", "waveform=x", "scope=evil", "sources=netease,evil"} {
		if code, _ := uiPost(t, srv.URL, q); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.conf")); err == nil {
		t.Error("rejected values were written")
	}
}

func TestUISettingsConsoleNeedsLoginAndRemoteGatewayIsRefused(t *testing.T) {
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/sc5000/ui?lang=en&restart=1", nil)
	r.RemoteAddr = "192.0.2.10:4000"
	ui.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("remote restart: %d", w.Code)
	}
	w = httptest.NewRecorder()
	ui.ServeHTTP(w, httptest.NewRequest("POST", "/api/ui?lang=en&restart=1", nil))
	if w.Code != 401 {
		t.Fatalf("console without login: %d", w.Code)
	}
}
