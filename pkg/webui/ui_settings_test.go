package webui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
)

func uiPost(t *testing.T, base, query string) (int, uiSettings) {
	t.Helper()
	resp, err := http.Post(base+"/v1/enginex/ui?"+query, "", nil)
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
	// Settings restarts must preserve safety state; only /api/boot-reset clears it.
	if err := os.WriteFile(filepath.Join(dir, "boot-count"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, v := uiPost(t, srv.URL, "lang=zh-Hant&restart=1"); code != 200 || v.Language != "zh-Hant" || !v.SafeMode || restarts != 1 {
		t.Fatalf("%d %+v restarts=%d", code, v, restarts)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "boot-count")); err != nil || string(data) != "2\n" {
		t.Fatalf("settings restart changed boot-count: %q %v", data, err)
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
	r := httptest.NewRequest("POST", "/v1/enginex/ui?lang=en&restart=1", nil)
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

func TestPlayheadPersistAndLegacyJogIgnored(t *testing.T) {
	dir := t.TempDir()
	// Legacy settings.conf with jog= must load without error and ignore the removed key.
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("lang=zh-Hans\njog=rainbow\nplayhead=0.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	if got := ui.loadUI(); got.Language != "zh-Hans" || got.Playhead != 0.25 {
		t.Fatalf("legacy settings load: %+v", got)
	}
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if code, v := uiPost(t, srv.URL, "playhead=0.42"); code != 200 || v.Playhead != 0.42 {
		t.Fatalf("%d %+v", code, v)
	}
	again := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	if got := again.loadUI(); got.Playhead != 0.42 {
		t.Fatalf("%+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); strings.Contains(string(b), "jog=") {
		t.Fatalf("saved settings.conf must omit removed jog key: %q", b)
	}
	for _, q := range []string{"playhead=2", "playhead=x"} {
		if code, _ := uiPost(t, srv.URL, q); code != 400 {
			t.Fatalf("%s: %d", q, code)
		}
	}
}

// #55: the auto-analyze toggle is off until switched on, and persists.
func TestAutoAnalyzeTogglePersists(t *testing.T) {
	dir := t.TempDir()
	ui := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if ui.loadUI().AutoAnalyze {
		t.Fatal("auto-analyze must default to off")
	}
	if code, v := uiPost(t, srv.URL, "autoAnalyze=1"); code != 200 || !v.AutoAnalyze {
		t.Fatalf("%d %+v", code, v)
	}
	if !New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil).loadUI().AutoAnalyze {
		t.Fatal("not persisted")
	}
	if code, v := uiPost(t, srv.URL, "autoAnalyze=0"); code != 200 || v.AutoAnalyze {
		t.Fatalf("%d %+v", code, v)
	}
	if code, _ := uiPost(t, srv.URL, "autoAnalyze=yes"); code != 400 {
		t.Fatal(code)
	}
}

func TestAutoAnalyzeRowRemovedFromWebConsoleAndStoredValueRetained(t *testing.T) {
	hb, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	indexHTML := string(hb)
	if strings.Contains(indexHTML, "autoAnalyze") || strings.Contains(indexHTML, "tAnalyze") || strings.Contains(indexHTML, "sAnalyze") {
		t.Fatal("dead autoAnalyze row must not appear in the web console")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("lang=en\nauto_analyze=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if code, v := uiPost(t, srv.URL, "masterLimiter=1"); code != 200 || !v.MasterLimiter || !v.AutoAnalyze {
		t.Fatalf("%d %+v", code, v)
	}
	b, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil || !strings.Contains(string(b), "\nauto_analyze=1\n") {
		t.Fatalf("stored auto_analyze value was not retained: %q (%v)", b, err)
	}
}

// #58: the limiter toggle defaults off, persists as the key engine_ui_hook.c
// polls, and only takes 0 or 1.
func TestMasterLimiterToggle(t *testing.T) {
	dir := t.TempDir()
	ui := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if v := ui.loadUI(); v.MasterLimiter {
		t.Fatalf("defaults: %+v", v)
	}
	if code, v := uiPost(t, srv.URL, "masterLimiter=1"); code != 200 || !v.MasterLimiter {
		t.Fatalf("%d %+v", code, v)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !strings.Contains(string(b), "\nmaster_limiter=1\n") {
		t.Fatalf("settings.conf: %q", b)
	}
	if code, v := uiPost(t, srv.URL, "masterLimiter=0"); code != 200 || v.MasterLimiter {
		t.Fatalf("%d %+v", code, v)
	}
	if code, _ := uiPost(t, srv.URL, "masterLimiter=on"); code != 400 {
		t.Fatalf("bad value: %d", code)
	}
}

// #64: Auto-Gain defaults off, persists as auto_gain=1 next to the limiter
// key, is independent of it, and only takes 0 or 1.
func TestAutoGainToggle(t *testing.T) {
	dir := t.TempDir()
	ui := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if v := ui.loadUI(); v.AutoGain {
		t.Fatalf("defaults: %+v", v)
	}
	if code, v := uiPost(t, srv.URL, "autoGain=1"); code != 200 || !v.AutoGain || v.MasterLimiter {
		t.Fatalf("%d %+v", code, v)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !strings.Contains(string(b), "\nauto_gain=1\n") || strings.Contains(string(b), "master_limiter=1") {
		t.Fatalf("settings.conf: %q", b)
	}
	if v := ui.loadUI(); !v.AutoGain {
		t.Fatalf("reload: %+v", v)
	}
	if code, v := uiPost(t, srv.URL, "autoGain=0"); code != 200 || v.AutoGain {
		t.Fatalf("%d %+v", code, v)
	}
	if code, _ := uiPost(t, srv.URL, "autoGain=yes"); code != 400 {
		t.Fatalf("bad value: %d", code)
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

func TestUIUpfaderEchoSetting(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir()}
	srv := httptest.NewServer(New(cfg, nil))
	defer srv.Close()

	// Default
	if _, v := uiPost(t, srv.URL, ""); v.UpfaderEcho != false {
		t.Fatalf("default UpfaderEcho should be false, got %+v", v)
	}

	// Update upfaderEcho=1
	if code, v := uiPost(t, srv.URL, "upfaderEcho=1"); code != 200 || !v.UpfaderEcho {
		t.Fatalf("update upfaderEcho=1 failed: code=%d, v=%+v", code, v)
	}

	// Verify persistence in settings.conf
	data, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(splitLines(data), "upfader_echo=1") {
		t.Fatalf("settings.conf missing upfader_echo=1: %s", string(data))
	}

	// Update upfaderEcho=0
	if code, v := uiPost(t, srv.URL, "upfaderEcho=0"); code != 200 || v.UpfaderEcho {
		t.Fatalf("update upfaderEcho=0 failed: code=%d, v=%+v", code, v)
	}

	data, err = os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(splitLines(data), "upfader_echo=1") {
		t.Fatalf("settings.conf should not contain upfader_echo=1 after disabling: %s", string(data))
	}

	// Reject invalid values
	for _, bad := range []string{"upfaderEcho=bad", "upfaderEcho=2", "upfaderEcho=yes"} {
		if code, _ := uiPost(t, srv.URL, bad); code != 400 {
			t.Errorf("expected 400 for %s, got %d", bad, code)
		}
	}
}

// #59: the toggle defaults off, persists as the key rb-bridge reads, and only takes 0 or 1.
func TestProlinkShareSetting(t *testing.T) {
	dir := t.TempDir()
	ui := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if v := ui.loadUI(); v.ProlinkShare {
		t.Fatalf("defaults: %+v", v)
	}
	if code, v := uiPost(t, srv.URL, "prolinkShare=1"); code != 200 || !v.ProlinkShare {
		t.Fatalf("%d %+v", code, v)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !strings.Contains(string(b), "\nprolink_share=1\n") {
		t.Fatalf("settings.conf: %q", b)
	}
	if code, _ := uiPost(t, srv.URL, "prolinkShare=2"); code != 400 {
		t.Fatalf("bad value: %d", code)
	}
}

// #49: the 4-deck role is saved only when it is not the default, and checked.
func TestFourDeckRole(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(New(Config{UIDir: dir, StateDir: t.TempDir()}, nil))
	defer srv.Close()
	if code, v := uiPost(t, srv.URL, "waveform=rgb"); code != 200 || v.FourDeckRole != "auto" {
		t.Fatalf("default role: %d %q", code, v.FourDeckRole)
	}
	if code, v := uiPost(t, srv.URL, "fourDeckRole=24"); code != 200 || v.FourDeckRole != "24" {
		t.Fatalf("role 24: %d %q", code, v.FourDeckRole)
	}
	if code, v := uiPost(t, srv.URL, "fourDeckRole=34"); code != 200 || v.FourDeckRole != "34" {
		t.Fatalf("role 34 (#160): %d %q", code, v.FourDeckRole)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !strings.Contains(string(b), "four_deck_role=34\n") {
		t.Fatalf("not saved: %s", b)
	}
	if code, _ := uiPost(t, srv.URL, "fourDeckRole=3"); code != 400 {
		t.Fatalf("bad role accepted: %d", code)
	}
	// The UI locks loading by the role the guard applied (auto resolved), with 4 decks on.
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = "/run/enginex-assignments/role" }()
	os.WriteFile(fourDeckRolePath, []byte("13\n"), 0o644)
	if _, v := uiPost(t, srv.URL, "fourDeckWaveform=1"); v.FourDeckRoleActive != "13" {
		t.Fatalf("active role: %q", v.FourDeckRoleActive)
	}
	os.WriteFile(fourDeckRolePath, []byte("auto\n"), 0o644)
	if _, v := uiPost(t, srv.URL, "fourDeckWaveform=1"); v.FourDeckRoleActive != "" {
		t.Fatalf("unresolved role exposed: %q", v.FourDeckRoleActive)
	}
}

// UI Scale is saved only when it is not the stock 100%, and reaches Qt through the env
// file Engine's unit reads (engine.service.d/70-ui-scale.conf).
func TestUIScale(t *testing.T) {
	dir := t.TempDir()
	uiScaleMu.Lock()
	origPath := uiScaleEnvPath
	uiScaleEnvPath = filepath.Join(dir, "ui-scale.env")
	uiScaleMu.Unlock()
	defer func() {
		uiScaleMu.Lock()
		uiScaleEnvPath = origPath
		uiScaleMu.Unlock()
	}()
	srv := httptest.NewServer(New(Config{UIDir: dir, StateDir: t.TempDir()}, nil))
	defer srv.Close()

	if code, v := uiPost(t, srv.URL, "uiScale=75"); code != 200 || v.UIScale != 75 {
		t.Fatalf("scale 75: %d %d", code, v.UIScale)
	}
	if b, _ := os.ReadFile(uiScaleEnvPath); string(b) != "ENGINEX_UI_SCALE=0.75\nSC5000_UI_SCALE=0.75\n" {
		t.Fatalf("env file: %q", b)
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !strings.Contains(string(c), "ui_scale=75\n") {
		t.Fatalf("not saved: %s", c)
	}
	if code, v := uiPost(t, srv.URL, "uiScale=105"); code != 200 || v.UIScale != 105 {
		t.Fatalf("scale 105: %d %d", code, v.UIScale)
	}
	if b, _ := os.ReadFile(uiScaleEnvPath); string(b) != "ENGINEX_UI_SCALE=1.05\nSC5000_UI_SCALE=1.05\n" {
		t.Fatalf("env file: %q", b)
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !strings.Contains(string(c), "ui_scale=105\n") {
		t.Fatalf("not saved: %s", c)
	}
	if code, v := uiPost(t, srv.URL, "uiScale=110"); code != 200 || v.UIScale != 110 {
		t.Fatalf("scale 110: %d %d", code, v.UIScale)
	}
	if b, _ := os.ReadFile(uiScaleEnvPath); string(b) != "ENGINEX_UI_SCALE=1.1\nSC5000_UI_SCALE=1.1\n" {
		t.Fatalf("env file: %q", b)
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !strings.Contains(string(c), "ui_scale=110\n") {
		t.Fatalf("not saved: %s", c)
	}
	for _, bad := range []string{"uiScale=50", "uiScale=101", "uiScale=115", "uiScale=abc"} {
		if code, _ := uiPost(t, srv.URL, bad); code != 400 {
			t.Fatalf("%s accepted: %d", bad, code)
		}
	}

	// A server started over reads the saved scale back from disk.
	srv2 := httptest.NewServer(New(Config{UIDir: dir, StateDir: t.TempDir()}, nil))
	defer srv2.Close()
	resp, err := http.Get(srv2.URL + "/v1/enginex/ui")
	if err != nil {
		t.Fatal(err)
	}
	var got uiSettings
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.UIScale != 110 {
		t.Fatalf("reloaded scale: %d", got.UIScale)
	}

	// Back to the stock size drops both files, so nothing overrides Denon's layout.
	if code, v := uiPost(t, srv2.URL, "uiScale=100"); code != 200 || v.UIScale != 100 {
		t.Fatalf("scale 100: %d %d", code, v.UIScale)
	}
	if _, err := os.Stat(uiScaleEnvPath); err == nil {
		t.Fatal("env file left behind at 100%")
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); strings.Contains(string(c), "ui_scale") {
		t.Fatalf("stock scale saved: %s", c)
	}
}

func TestUIScaleConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	uiScaleMu.Lock()
	origPath := uiScaleEnvPath
	uiScaleEnvPath = filepath.Join(dir, "ui-scale.env")
	uiScaleMu.Unlock()
	defer func() {
		uiScaleMu.Lock()
		uiScaleEnvPath = origPath
		uiScaleMu.Unlock()
	}()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		pct := uiUIScales[i%len(uiUIScales)]
		go func(p int) {
			defer wg.Done()
			if err := writeUIScaleEnv(p); err != nil {
				t.Errorf("writeUIScaleEnv(%d) failed: %v", p, err)
			}
		}(pct)
	}
	wg.Wait()
}

func TestUISettingsQoLFXAndStreamingSort(t *testing.T) {
	dir := t.TempDir()
	token := "secret-code"
	cfg := Config{AccessToken: token, UIDir: dir, StateDir: t.TempDir()}
	srv := httptest.NewServer(New(cfg, nil))
	defer srv.Close()

	// 1. Verify defaults
	client := srv.Client()
	req, _ := http.NewRequest("GET", srv.URL+"/api/ui", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v uiSettings
	json.NewDecoder(resp.Body).Decode(&v)
	if v.DebugTelemetry {
		t.Fatal("DebugTelemetry must default to false")
	}
	if v.StreamSortRole != "default" || v.StreamSortOrder != "asc" || v.BrowserDualColumn != false ||
		!v.TracklistWaveform ||
		v.SyncGuardPct != 0 || v.SwapLoopOrder != false || v.LedBrightness != 100 || !v.CueCountdown ||
		v.StemsEqMode != false || v.BeatJumpPads != false || v.PioneerFx != "off" {
		t.Fatalf("unexpected defaults: %+v", v)
	}

	// 2. Update via POST /api/ui with query params and Bearer token
	updateParams := "streamSortRole=bpm&streamSortOrder=desc&browserDualColumn=1&tracklistWaveform=0&cueCountdown=0&syncGuardPct=15&swapLoopOrder=1&ledBrightness=60&stemsEqMode=1&beatJumpPads=1&pioneerFx=spiral"
	postReq, _ := http.NewRequest("POST", srv.URL+"/api/ui?"+updateParams, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postResp, err := client.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != 200 {
		t.Fatalf("POST /api/ui returned status %d", postResp.StatusCode)
	}
	var updated uiSettings
	json.NewDecoder(postResp.Body).Decode(&updated)
	if updated.StreamSortRole != "bpm" || updated.StreamSortOrder != "desc" || !updated.BrowserDualColumn || updated.CueCountdown ||
		updated.TracklistWaveform ||
		updated.SyncGuardPct != 15 || !updated.SwapLoopOrder || updated.LedBrightness != 60 ||
		!updated.StemsEqMode || !updated.BeatJumpPads || updated.PioneerFx != "spiral" {
		t.Fatalf("updated values mismatch: %+v", updated)
	}

	// 3. Verify GET /api/ui reflects the update
	getReq, _ := http.NewRequest("GET", srv.URL+"/api/ui", nil)
	getReq.Header.Set("Authorization", "Bearer "+token)
	getResp, err := client.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	var fetched uiSettings
	json.NewDecoder(getResp.Body).Decode(&fetched)
	if fetched.StreamSortRole != "bpm" || fetched.StreamSortOrder != "desc" || !fetched.BrowserDualColumn ||
		fetched.TracklistWaveform ||
		fetched.SyncGuardPct != 15 || !fetched.SwapLoopOrder || fetched.LedBrightness != 60 ||
		!fetched.StemsEqMode || !fetched.BeatJumpPads || fetched.PioneerFx != "spiral" {
		t.Fatalf("fetched values mismatch: %+v", fetched)
	}

	// 4. Verify settings.conf persistence
	confBytes, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(confBytes)
	for _, expected := range []string{
		"stream_sort_role=bpm",
		"stream_sort_order=desc",
		"browser_dual_column=1",
		"tracklist_waveform=0",
		"sync_guard_pct=15",
		"swap_loop_order=1",
		"led_brightness=60",
		"stems_eq_mode=1",
		"beat_jump_pads=1",
		"pioneer_fx=spiral",
	} {
		if !slices.Contains(lines, expected) {
			t.Errorf("settings.conf missing %q in:\n%s", expected, string(confBytes))
		}
	}

	// 5. Reload in new Server instance
	srv2 := httptest.NewServer(New(cfg, nil))
	defer srv2.Close()
	req2, _ := http.NewRequest("GET", srv2.URL+"/api/ui", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var reloaded uiSettings
	json.NewDecoder(resp2.Body).Decode(&reloaded)
	if reloaded.StreamSortRole != "bpm" || reloaded.StreamSortOrder != "desc" || !reloaded.BrowserDualColumn || reloaded.CueCountdown ||
		reloaded.TracklistWaveform ||
		reloaded.SyncGuardPct != 15 || !reloaded.SwapLoopOrder || reloaded.LedBrightness != 60 ||
		!reloaded.StemsEqMode || !reloaded.BeatJumpPads || reloaded.PioneerFx != "spiral" {
		t.Fatalf("reloaded values mismatch: %+v", reloaded)
	}

	// 6. Test bad values rejected
	badCases := []string{
		"streamSortRole=invalid",
		"streamSortOrder=invalid",
		"browserDualColumn=maybe",
		"tracklistWaveform=maybe",
		"cueCountdown=maybe",
		"syncGuardPct=25",
		"swapLoopOrder=notabool",
		"ledBrightness=10",
		"ledBrightness=120",
		"stemsEqMode=trueish",
		"beatJumpPads=2",
		"pioneerFx=echo_delay",
	}
	for _, bad := range badCases {
		badReq, _ := http.NewRequest("POST", srv.URL+"/api/ui?"+bad, nil)
		badReq.Header.Set("Authorization", "Bearer "+token)
		badResp, err := client.Do(badReq)
		if err != nil {
			t.Fatal(err)
		}
		badResp.Body.Close()
		if badResp.StatusCode != 400 {
			t.Errorf("expected 400 for %s, got %d", bad, badResp.StatusCode)
		}
	}
}

func TestUIStatusDigestFollowsTheBody(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	get := func() (string, string) {
		t.Helper()
		r, err := http.Get(srv.URL + "/v1/enginex/ui")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("body %q: %v", b, err)
		}
		return r.Header.Get("X-UI-Digest"), string(b)
	}
	d1, b1 := get()
	d2, b2 := get()
	if d1 == "" || d1 != d2 || b1 != b2 {
		t.Fatalf("same settings: %q %q", d1, d2)
	}
	if fmt.Sprintf("%x", fnvSum(strings.TrimSuffix(b1, "\n"))) != d1 {
		t.Fatalf("digest %q is not the body's", d1)
	}
	uiPost(t, srv.URL, "waveform=rgb")
	if d3, _ := get(); d3 == d1 {
		t.Fatal("a changed setting kept the digest")
	}
}

func TestBoardCapabilitiesAndSettingsCompatJP07vsJC11(t *testing.T) {
	oldCompat := deviceCompatiblePath
	defer func() { deviceCompatiblePath = oldCompat }()

	cases := []struct {
		name      string
		compat    string
		wantBoard string
		player    bool
		caps      uiCapabilities
	}{
		{
			name:      "JP07 SC5000",
			compat:    "inmusic,jp07\x00rockchip,rk3288\x00",
			wantBoard: "SC5000",
			player:    true,
			caps: uiCapabilities{
				Mixer: false, FourDeck: true, AutoLoopKnob: true, BeatJumpPads: false,
				TrackSkipKeys: true, StemsEq: false, LineInAux: false, Gen2: false, RemoteStems: true, JogWheel: true,
			},
		},
		{
			name:      "JC11 PRIME 4",
			compat:    "inmusic,jc11\x00rockchip,rk3288\x00",
			wantBoard: "PRIME4",
			player:    false,
			caps: uiCapabilities{
				Mixer: true, FourDeck: true, AutoLoopKnob: true, BeatJumpPads: false,
				TrackSkipKeys: true, StemsEq: true, LineInAux: false, Gen2: false, RemoteStems: true, JogWheel: true,
			},
		},
		{
			name:      "JP11 PRIME GO",
			compat:    "inmusic,jp11\x00",
			wantBoard: "PRIMEGO",
			player:    false,
			caps: uiCapabilities{
				Mixer: true, FourDeck: false, AutoLoopKnob: true, BeatJumpPads: false,
				TrackSkipKeys: false, StemsEq: true, LineInAux: false, Gen2: false, RemoteStems: true, JogWheel: true,
			},
		},
		{
			name:      "NH08 Mixstream Pro",
			compat:    "inmusic,nh08\x00",
			wantBoard: "MIXSTREAMPRO",
			player:    false,
			caps: uiCapabilities{
				Mixer: true, FourDeck: false, AutoLoopKnob: false, BeatJumpPads: true,
				TrackSkipKeys: false, StemsEq: true, LineInAux: false, Gen2: false, RemoteStems: true, JogWheel: true,
			},
		},
		{
			name:      "JP21 SC LIVE 4",
			compat:    "inmusic,jp21\x00",
			wantBoard: "SCLIVE4",
			player:    false,
			caps: uiCapabilities{
				Mixer: true, FourDeck: true, AutoLoopKnob: true, BeatJumpPads: false,
				TrackSkipKeys: true, StemsEq: true, LineInAux: true, Gen2: true, RemoteStems: true, JogWheel: true,
			},
		},
		{
			name:      "JP24 PRIME 4 G2",
			compat:    "inmusic,jp24\x00",
			wantBoard: "PRIME4G2",
			player:    false,
			caps: uiCapabilities{
				Mixer: true, FourDeck: true, AutoLoopKnob: true, BeatJumpPads: false,
				TrackSkipKeys: true, StemsEq: false, LineInAux: false, Gen2: true, RemoteStems: false, JogWheel: true,
			},
		},
		{
			name:      "JP08 SC5000M",
			compat:    "inmusic,jp08\x00",
			wantBoard: "SC5000M",
			player:    true,
			caps: uiCapabilities{
				Mixer: false, FourDeck: true, AutoLoopKnob: true, BeatJumpPads: false,
				TrackSkipKeys: true, StemsEq: false, LineInAux: false, Gen2: false, RemoteStems: true, JogWheel: false,
			},
		},
		{
			name:      "RMZ2 SYSTEM ONE",
			compat:    "inmusic,rmz2\x00",
			wantBoard: "SYSTEMONE",
			player:    false,
			caps: uiCapabilities{
				Mixer: true, FourDeck: false, AutoLoopKnob: true, BeatJumpPads: false,
				TrackSkipKeys: true, StemsEq: true, LineInAux: false, Gen2: true, RemoteStems: false, JogWheel: false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cf := filepath.Join(t.TempDir(), "compatible")
			if err := os.WriteFile(cf, []byte(tc.compat), 0o600); err != nil {
				t.Fatal(err)
			}
			deviceCompatiblePath = cf
			srv := httptest.NewServer(New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil))
			defer srv.Close()
			// Backend accepts and keeps settings compatible regardless of board.
			code, v := uiPost(t, srv.URL, "upfaderEcho=1&stemsEqMode=1&beatJumpPads=1&fourDeckWaveform=1")
			if code != 200 || !v.UpfaderEcho || !v.StemsEqMode || !v.BeatJumpPads || !v.FourDeckWaveform {
				t.Fatalf("compat POST failed: code=%d %+v", code, v)
			}
			if v.Board != tc.wantBoard || v.Player != tc.player || v.Capabilities != tc.caps {
				t.Fatalf("got board=%q player=%v caps=%+v; want board=%q player=%v caps=%+v",
					v.Board, v.Player, v.Capabilities, tc.wantBoard, tc.player, tc.caps)
			}
		})
	}
	b, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	indexHTML := string(b)
	if !strings.Contains(indexHTML, "caps.mixer!==undefined?!caps.mixer:!!v.player") {
		t.Fatal("web console missing mixer capability gate for upfaderEcho")
	}
	if !strings.Contains(indexHTML, "v.buildTag") {
		t.Fatal("web console missing buildTag display")
	}
}

func TestBuildTagStatusAndReleaseSuppression(t *testing.T) {
	oldTag, oldTrack, oldRel := BuildTag, BuildTrack, ReleaseBuild
	defer func() { BuildTag, BuildTrack, ReleaseBuild = oldTag, oldTrack, oldRel }()

	srv := httptest.NewServer(New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil))
	defer srv.Close()

	BuildTag = "sbx 085a437"
	ReleaseBuild = ""
	if _, v := uiPost(t, srv.URL, ""); v.BuildTag != "sbx 085a437" {
		t.Fatalf("expected sbx buildTag, got %q", v.BuildTag)
	}

	ReleaseBuild = "1"
	if _, v := uiPost(t, srv.URL, ""); v.BuildTag != "" {
		t.Fatalf("release build must suppress buildTag, got %q", v.BuildTag)
	}
}

func TestUISettingsFxTimeMode(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir()}
	srv := httptest.NewServer(New(cfg, nil))
	defer srv.Close()

	// 1. Verify defaults
	if code, v := uiPost(t, srv.URL, ""); code != 200 || v.FxTimeMode != "beats" || v.FxTimeMs != 500 {
		t.Fatalf("unexpected defaults: code=%d %+v", code, v)
	}

	// 2. Update to valid non-defaults
	if code, v := uiPost(t, srv.URL, "fxTimeMode=ms&fxTimeMs=250"); code != 200 || v.FxTimeMode != "ms" || v.FxTimeMs != 250 {
		t.Fatalf("update failed: code=%d %+v", code, v)
	}

	// Verify persistence in settings.conf
	confBytes, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(confBytes)
	for _, expected := range []string{"fx_time_mode=ms", "fx_time_ms=250"} {
		if !slices.Contains(lines, expected) {
			t.Errorf("settings.conf missing %q in:\n%s", expected, string(confBytes))
		}
	}

	// 3. Reload in new server instance
	srv2 := httptest.NewServer(New(cfg, nil))
	defer srv2.Close()
	if code, v := uiPost(t, srv2.URL, ""); code != 200 || v.FxTimeMode != "ms" || v.FxTimeMs != 250 {
		t.Fatalf("reloaded settings mismatch: code=%d %+v", code, v)
	}

	// 4. Test boundary values and rejections
	badQueries := []string{
		"fxTimeMode=invalid",
		"fxTimeMode=seconds",
		"fxTimeMs=0",
		"fxTimeMs=-1",
		"fxTimeMs=4001",
		"fxTimeMs=99999",
		"fxTimeMs=notanumber",
	}
	for _, bad := range badQueries {
		if code, _ := uiPost(t, srv.URL, bad); code != 400 {
			t.Errorf("expected 400 for %s, got %d", bad, code)
		}
	}

	// 5. Test valid boundary limits 10 and 4000
	if code, v := uiPost(t, srv.URL, "fxTimeMs=10"); code != 200 || v.FxTimeMs != 10 {
		t.Errorf("expected 200 for fxTimeMs=10, got %d %+v", code, v)
	}
	if code, v := uiPost(t, srv.URL, "fxTimeMs=4000"); code != 200 || v.FxTimeMs != 4000 {
		t.Errorf("expected 200 for fxTimeMs=4000, got %d %+v", code, v)
	}

	// 6. Reset back to defaults and verify omissions in settings.conf
	if code, v := uiPost(t, srv.URL, "fxTimeMode=beats&fxTimeMs=500"); code != 200 || v.FxTimeMode != "beats" || v.FxTimeMs != 500 {
		t.Fatalf("reset failed: code=%d %+v", code, v)
	}
	confBytes, _ = os.ReadFile(filepath.Join(dir, "settings.conf"))
	lines = splitLines(confBytes)
	for _, unexpected := range []string{"fx_time_mode=ms", "fx_time_ms="} {
		for _, line := range lines {
			if strings.HasPrefix(line, unexpected) {
				t.Errorf("settings.conf should not contain default %q, but has %q", unexpected, line)
			}
		}
	}
}

func TestUISettingsFxTimeFileReplacement(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	defer s.Close()
	for _, tc := range []struct {
		text, mode string
		ms         int
	}{
		{"fx_time_mode=ms\nfx_time_ms=4000\n", "ms", 4000},
		{"", "beats", 500},
		{"fx_time_mode=msjunk\nfx_time_ms=125ms\n", "beats", 500},
		{"fx_time_mode=ms\nfx_time_ms=4294967295\n", "ms", 4000},
		{"fx_time_ms=0\n", "beats", 10},
		{"  fx_time_mode=ms  \r\n\tfx_time_ms=250 \r\n", "ms", 250},
		{"fx_time_ms= 250\n", "beats", 500},
		{"fx_time_mode=ms\nfx_time_ms=125\nfx_time_mode=msjunk\nfx_time_ms=125ms\n", "beats", 500},
		{"fx_time_mode=ms\nfx_time_ms=4294967296\n", "ms", 500},
	} {
		if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte(tc.text), 0600); err != nil {
			t.Fatal(err)
		}
		s.uiMu.Lock()
		v := s.loadUI()
		s.uiMu.Unlock()
		if v.FxTimeMode != tc.mode || v.FxTimeMs != tc.ms {
			t.Fatalf("%q: got %s/%d", tc.text, v.FxTimeMode, v.FxTimeMs)
		}
	}
}

func TestEngineLightingDefaultOffAndPersistence(t *testing.T) {
	dir := t.TempDir()
	restarts := 0
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), RestartEngine: func() error { restarts++; return nil }}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	check := func(want bool) {
		t.Helper()
		s.uiMu.Lock()
		b, err := json.Marshal(s.loadUI())
		s.uiMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		var loaded map[string]any
		if err := json.Unmarshal(b, &loaded); err != nil {
			t.Fatal(err)
		}
		if got, ok := loaded["engineLighting"]; !ok || got != want {
			t.Errorf("loadUI engineLighting: got %v (present=%v), want %v", got, ok, want)
		}
		resp, err := http.Get(srv.URL + "/v1/enginex/ui")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		if got, ok := v["engineLighting"]; !ok || got != want {
			t.Errorf("GET engineLighting: got %v (present=%v), want %v", got, ok, want)
		}
	}
	check(false)
	for _, query := range []string{"engineLighting=1", "waveform=rgb"} {
		if code, _ := uiPost(t, srv.URL, query); code != 200 {
			t.Fatalf("%s: HTTP %d", query, code)
		}
		check(true)
		b, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(splitLines(b), "engine_lighting=1") {
			t.Errorf("%s did not preserve engine_lighting=1", query)
		}
	}
	resp, err := http.Post(srv.URL+"/v1/enginex/ui", "application/json", strings.NewReader(`{"engineLighting":false}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("JSON Off: HTTP %d", resp.StatusCode)
	}
	check(false)
	b, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "engine_lighting=1") {
		t.Error("Off left engine_lighting=1")
	}
	for _, key := range []string{"engineLighting", "engine_lighting"} {
		if code, _ := uiPost(t, srv.URL, key+"=invalid"); code != 400 {
			t.Errorf("invalid %s: HTTP %d, want 400", key, code)
		}
		if code, _ := uiPost(t, srv.URL, key+"=true"); code != 200 {
			t.Errorf("%s=true: HTTP %d", key, code)
		}
		check(true)
		if code, _ := uiPost(t, srv.URL, key+"=0"); code != 200 {
			t.Errorf("%s=0: HTTP %d", key, code)
		}
		check(false)
	}
	if restarts != 0 {
		t.Errorf("lighting caused %d Engine restarts", restarts)
	}
}

func TestUISettingsBpmDecimals(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir()}
	srv := httptest.NewServer(New(cfg, nil))
	defer srv.Close()

	if code, v := uiPost(t, srv.URL, ""); code != 200 || v.BpmDecimals != 2 {
		t.Fatalf("unexpected default BpmDecimals: code=%d got=%d", code, v.BpmDecimals)
	}

	if code, v := uiPost(t, srv.URL, "bpmDecimals=1"); code != 200 || v.BpmDecimals != 1 {
		t.Fatalf("bpmDecimals=1 failed: code=%d got=%d", code, v.BpmDecimals)
	}
	confBytes, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(splitLines(confBytes), "bpm_decimals=1") {
		t.Fatalf("settings.conf missing bpm_decimals=1:\n%s", string(confBytes))
	}

	srv2 := httptest.NewServer(New(cfg, nil))
	defer srv2.Close()
	if code, v := uiPost(t, srv2.URL, ""); code != 200 || v.BpmDecimals != 1 {
		t.Fatalf("reloaded BpmDecimals mismatch: code=%d got=%d", code, v.BpmDecimals)
	}

	for _, bad := range []string{"bpmDecimals=0", "bpmDecimals=3", "bpmDecimals=-1", "bpmDecimals=abc", "bpmDecimals=1.5", "bpm_decimals=9"} {
		if code, _ := uiPost(t, srv.URL, bad); code != 400 {
			t.Errorf("expected 400 for %s, got %d", bad, code)
		}
	}

	if code, v := uiPost(t, srv.URL, "bpmDecimals=2"); code != 200 || v.BpmDecimals != 2 {
		t.Fatalf("reset bpmDecimals=2 failed: code=%d got=%d", code, v.BpmDecimals)
	}
	confBytes, _ = os.ReadFile(filepath.Join(dir, "settings.conf"))
	for _, line := range splitLines(confBytes) {
		if strings.HasPrefix(line, "bpm_decimals=") {
			t.Errorf("settings.conf should omit default bpm_decimals=2, got %q", line)
		}
	}
}

// #224: disabling TIDAL must remove the stale upstream routing choice, on disk
// and in responses, whether the setting came from this UI or an older version.
func TestTidalDisabledClearsScope(t *testing.T) {
	for _, query := range []string{"sources=spotify,netease", "sources="} {
		t.Run(query, func(t *testing.T) {
			dir := t.TempDir()
			s := New(Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir()}, nil)
			srv := httptest.NewServer(s)
			defer srv.Close()
			uiPost(t, srv.URL, "scope=tidal")
			code, v := uiPost(t, srv.URL, query)
			if code != 200 || v.Scope != "" {
				t.Errorf("disabled scope = %q, HTTP %d", v.Scope, code)
			}
			b, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
			if err != nil || strings.Contains(string(b), "scope=tidal") {
				t.Errorf("stale scope persisted: %v", err)
			}
			var status map[string]any
			tidalGet(t, srv.URL, "/v1/sc5000/ui", 200, &status)
			if status["scope"] != "" {
				t.Errorf("GET stale scope = %v", status["scope"])
			}
			// A settings file written before this fix must also read safely.
			os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("scope=tidal\nsources=spotify,netease\n"), 0600)
			tidalGet(t, srv.URL, "/v1/sc5000/ui", 200, &status)
			if status["scope"] != "" {
				t.Errorf("legacy scope = %v", status["scope"])
			}
		})
	}
}

func TestUIUpdateIgnoresRemovedToggleParameter(t *testing.T) {
	srv := httptest.NewServer(New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil))
	defer srv.Close()
	uiPost(t, srv.URL, "sources=tidal,spotify&scope=tidal")
	code, v := uiPost(t, srv.URL, "toggle=tidal")
	if code != 200 || v.Scope != "tidal" || !slices.Contains(v.Sources, "tidal") {
		t.Error("removed toggle parameter still changes sources")
	}
}

func TestUISettingsJogInvert(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir()}
	srv := httptest.NewServer(New(cfg, nil))
	defer srv.Close()

	// 1. Default off
	if code, v := uiPost(t, srv.URL, ""); code != 200 || v.JogInvertLeft || v.JogInvertRight {
		t.Fatalf("unexpected defaults: code=%d %+v", code, v)
	}

	// 2. Enable left only, then both, verify settings.conf and reload
	if code, v := uiPost(t, srv.URL, "jogInvertLeft=1"); code != 200 || !v.JogInvertLeft || v.JogInvertRight {
		t.Fatalf("left enable failed: code=%d %+v", code, v)
	}
	confBytes, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(confBytes)
	if !slices.Contains(lines, "jog_invert_left=1") || slices.Contains(lines, "jog_invert_right=1") {
		t.Fatalf("unexpected settings.conf after left enable:\n%s", string(confBytes))
	}

	if code, v := uiPost(t, srv.URL, "jogInvertRight=1"); code != 200 || !v.JogInvertLeft || !v.JogInvertRight {
		t.Fatalf("right enable failed: code=%d %+v", code, v)
	}

	srv2 := httptest.NewServer(New(cfg, nil))
	defer srv2.Close()
	if code, v := uiPost(t, srv2.URL, ""); code != 200 || !v.JogInvertLeft || !v.JogInvertRight {
		t.Fatalf("reload failed: code=%d %+v", code, v)
	}

	// 3. Reject invalid values
	for _, bad := range []string{"jogInvertLeft=maybe", "jogInvertRight=2"} {
		if code, _ := uiPost(t, srv.URL, bad); code != 400 {
			t.Errorf("expected 400 for %s, got %d", bad, code)
		}
	}

	// 4. Reset to false omits keys from settings.conf
	if code, v := uiPost(t, srv.URL, "jogInvertLeft=0&jogInvertRight=0"); code != 200 || v.JogInvertLeft || v.JogInvertRight {
		t.Fatalf("reset failed: code=%d %+v", code, v)
	}
	confBytes, _ = os.ReadFile(filepath.Join(dir, "settings.conf"))
	for _, line := range splitLines(confBytes) {
		if strings.HasPrefix(line, "jog_invert_") {
			t.Errorf("unexpected default line in settings.conf: %q", line)
		}
	}

	// 5. Legacy 5.0.4 UI pack without ControlSurfaceModules/JogWheel.qml suppresses JogWheel capability
	pack504 := filepath.Join(dir, "packs", "en", "Skins", "EmbeddedCommon")
	if err := os.MkdirAll(pack504, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pack504, "EmbeddedMain.qml"), []byte("Item{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, v := uiPost(t, srv.URL, ""); code != 200 || v.Capabilities.JogWheel {
		t.Fatalf("expected JogWheel=false for legacy 5.0.4 pack, got code=%d %+v", code, v.Capabilities)
	}
	pack530 := filepath.Join(dir, "packs", "en", "ControlSurfaceModules", "ControlSurfaceModules")
	if err := os.MkdirAll(pack530, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pack530, "JogWheel.qml"), []byte("Item{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, v := uiPost(t, srv.URL, ""); code != 200 || !v.Capabilities.JogWheel {
		t.Fatalf("expected JogWheel=true when 5.3.0 JogWheel.qml is present, got code=%d %+v", code, v.Capabilities)
	}
}
