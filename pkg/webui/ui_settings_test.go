package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
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
	// Two earlier quick restarts: this deliberate one must not become the third.
	if err := os.WriteFile(filepath.Join(dir, "boot-count"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, v := uiPost(t, srv.URL, "lang=zh-Hant&restart=1"); code != 200 || v.Language != "zh-Hant" || v.SafeMode || restarts != 1 {
		t.Fatalf("%d %+v restarts=%d", code, v, restarts)
	}
	if _, err := os.Stat(filepath.Join(dir, "boot-count")); !os.IsNotExist(err) {
		t.Fatalf("boot-count survived the restart: %v", err)
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
	for _, q := range []string{"lang=../../etc", "waveform=x", "scope=evil", "sources=netease,evil", "neteaseMode=quota"} {
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

func TestQualitySettingAppliesWithoutARestart(t *testing.T) {
	restarted := false
	jobs := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}})
	defer jobs.Close()
	dir := t.TempDir()
	ui := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir(), RestartEngine: func() error { restarted = true; return nil }}, jobs)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if !jobs.p.Deemix.Lossless() {
		t.Fatal("FLAC should be the default")
	}
	if code, v := uiPost(t, srv.URL, "quality=320k"); code != 200 || v.Quality != "320k" {
		t.Fatalf("%d %+v", code, v)
	}
	if jobs.p.Deemix.Lossless() || restarted {
		t.Fatalf("not applied at once, restarted=%v", restarted)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if !strings.Contains(string(data), "quality=320k") {
		t.Fatalf("%s", data)
	}
	if code, _ := uiPost(t, srv.URL, "quality=128k"); code != 400 {
		t.Fatalf("bad quality accepted: %d", code)
	}
}

func TestNeteaseModeDefaultsToOfficialAndPersists(t *testing.T) {
	jobs := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}})
	defer jobs.Close()
	dir := t.TempDir()
	srv := httptest.NewServer(New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, jobs))
	defer srv.Close()
	// Without a Deezer account, NetEase tracks must work out of the box.
	if _, v := uiPost(t, srv.URL, ""); v.NeteaseMode != "official" || !jobs.neteaseDirect() {
		t.Fatalf("%+v", v)
	}
	if code, v := uiPost(t, srv.URL, "neteaseMode=deemix"); code != 200 || v.NeteaseMode != "deemix" || jobs.neteaseDirect() {
		t.Fatalf("%d %+v", code, v)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if !slices.Contains(splitLines(data), "netease_mode=deemix") {
		t.Fatalf("%s", data)
	}
	// An unknown value in the file falls back to the default.
	os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("netease_mode=other\n"), 0o644)
	if _, v := uiPost(t, srv.URL, ""); v.NeteaseMode != "official" {
		t.Fatalf("%+v", v)
	}
}

func TestPlayheadAndJogModePersist(t *testing.T) {
	dir := t.TempDir()
	ui := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if code, v := uiPost(t, srv.URL, "playhead=0.42&jog=rainbow"); code != 200 || v.Playhead != 0.42 || v.Jog != "rainbow" {
		t.Fatalf("%d %+v", code, v)
	}
	again := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	if got := again.loadUI(); got.Playhead != 0.42 || got.Jog != "rainbow" {
		t.Fatalf("%+v", got)
	}
	if code, v := uiPost(t, srv.URL, "jog=bpm"); code != 200 || v.Jog != "bpm" {
		t.Fatalf("%d %+v", code, v)
	}
	for _, q := range []string{"playhead=2", "playhead=x", "jog=strobe"} {
		if code, _ := uiPost(t, srv.URL, q); code != 400 {
			t.Fatalf("%s: %d", q, code)
		}
	}
}

func TestAccessTokenSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	first, created, err := LoadAccessToken(dir)
	if err != nil || !created || first == "" {
		t.Fatal(first, created, err)
	}
	again, created, err := LoadAccessToken(dir)
	if err != nil || created || again != first {
		t.Fatal(again, created, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "access-token")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
}

func TestUpgradedSettingsShowNewServicesOnce(t *testing.T) {
	dir := t.TempDir()
	// What 5.0.4.2 saves by default: no "known=" line, all five services of that time.
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("lang=zh-Hans\nsources=tidal,netease,spotify,deezer,soundcloud\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	if got := ui.loadUI().Sources; !slices.Equal(got, uiServices) {
		t.Fatalf("upgraded deck hides a new service: %v", got)
	}
	// The owner turns Beatport off: the saved "known=" keeps it off.
	v := ui.loadUI()
	v.Sources = []string{"netease", "spotify"}
	if err := ui.saveUI(v); err != nil {
		t.Fatal(err)
	}
	if got := ui.loadUI().Sources; !slices.Equal(got, []string{"netease", "spotify"}) {
		t.Fatalf("a service turned off came back: %v", got)
	}
}

func TestUIFourDeckAndProlinkWaveformSettings(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir()}
	srv := httptest.NewServer(New(cfg, nil))
	defer srv.Close()

	// Defaults
	if _, v := uiPost(t, srv.URL, ""); v.FourDeckWaveform != false || v.ProlinkWaveform != true {
		t.Fatalf("defaults wrong: %+v", v)
	}

	// Update fourDeckWaveform=1 and prolinkWaveform=0
	if code, v := uiPost(t, srv.URL, "fourDeckWaveform=1&prolinkWaveform=0"); code != 200 || !v.FourDeckWaveform || v.ProlinkWaveform {
		t.Fatalf("update failed: code=%d, v=%+v", code, v)
	}

	// Verify persistence in settings.conf
	data, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(data)
	if !slices.Contains(lines, "four_deck_waveform=1") || !slices.Contains(lines, "prolink_waveform=0") {
		t.Fatalf("settings.conf missing lines: %s", string(data))
	}

	// Re-reading in new server instance
	srv2 := httptest.NewServer(New(cfg, nil))
	defer srv2.Close()
	if _, v := uiPost(t, srv2.URL, ""); !v.FourDeckWaveform || v.ProlinkWaveform {
		t.Fatalf("did not reload from file: %+v", v)
	}

	// Rejects invalid boolean values
	for _, bad := range []string{"fourDeckWaveform=yes", "fourDeckWaveform=2", "prolinkWaveform=invalid"} {
		if code, _ := uiPost(t, srv.URL, bad); code != 400 {
			t.Errorf("expected 400 for %s, got %d", bad, code)
		}
	}
}

func TestUIBroadcastSetting(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir()}
	srv := httptest.NewServer(New(cfg, nil))
	defer srv.Close()

	if _, v := uiPost(t, srv.URL, ""); v.BroadcastEnabled != false {
		t.Fatalf("broadcast default wrong: %+v", v)
	}

	if code, v := uiPost(t, srv.URL, "broadcast=1"); code != 200 || !v.BroadcastEnabled {
		t.Fatalf("broadcast enable failed: code=%d, v=%+v", code, v)
	}

	if code, v := uiPost(t, srv.URL, "broadcast=false"); code != 200 || v.BroadcastEnabled {
		t.Fatalf("broadcast disable failed: code=%d, v=%+v", code, v)
	}

	if code, _ := uiPost(t, srv.URL, "broadcast=notabool"); code != 400 {
		t.Fatalf("expected 400 for invalid broadcast value")
	}
}
