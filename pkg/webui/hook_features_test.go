package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// hookFeatureFiles points the feature files into a temporary directory.
func hookFeatureFiles(t *testing.T, alive bool) (conf, state string) {
	t.Helper()
	dir := t.TempDir()
	oldConf, oldState, oldAlive := hookDisablePath, hookFeatureStatePath, engineAlive
	hookDisablePath = filepath.Join(dir, "enginex", "hook-disable.conf")
	hookFeatureStatePath = filepath.Join(dir, "run", "hook-features")
	engineAlive = func(pid int) bool { return alive && pid == 4242 }
	t.Cleanup(func() { hookDisablePath, hookFeatureStatePath, engineAlive = oldConf, oldState, oldAlive })
	if err := os.MkdirAll(filepath.Dir(hookFeatureStatePath), 0o755); err != nil {
		t.Fatal(err)
	}
	return hookDisablePath, hookFeatureStatePath
}

func TestHookFeatureNamesMatchTheHook(t *testing.T) {
	b, err := os.ReadFile("../../tools/ui-mod/hook_features.h")
	if err != nil {
		t.Fatal(err)
	}
	var hook []string
	for _, m := range regexp.MustCompile(`(?m)^ *X\([A-Z0-9_]+, "([a-z0-9-]+)"\)`).FindAllStringSubmatch(string(b), -1) {
		hook = append(hook, m[1])
	}
	var menu []string
	for _, f := range hookFeatures {
		menu = append(menu, f.Name)
	}
	slices.Sort(hook)
	slices.Sort(menu)
	if len(hook) == 0 || !slices.Equal(hook, menu) {
		t.Fatalf("hook features %v, console menu %v", hook, menu)
	}
}

func postHookFeatures(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/hook-features", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func TestHookFeatureMenuWritesTheFileTheHookReads(t *testing.T) {
	conf, _ := hookFeatureFiles(t, false)
	restarts := 0
	s := New(Config{AccessToken: "t", UIDir: t.TempDir(), StateDir: t.TempDir(), RestartEngine: func() error { restarts++; return nil }}, nil)

	w := postHookFeatures(t, s, `{"off":["dsp-guards","gateway","gateway"],"restart":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	b, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	// deemixd stays on: the console's Deezer job queue needs it without the gateway.
	// hifi-output goes off along with dsp-guards: hook_master_output is not installed without it.
	if got := parseHookFeatureNames(string(b)); !slices.Equal(got, []string{"gateway", "dsp-guards", "hifi-output"}) {
		t.Fatalf("file switches off %v:\n%s", got, b)
	}
	if along := hookFeaturesAlong(string(b)); !slices.Equal(along, []string{"hifi-output"}) {
		t.Fatalf("along = %v:\n%s", along, b)
	}
	// Every feature is documented in the file, the ones left on behind '#'.
	for _, f := range hookFeatures {
		if !regexp.MustCompile(`(?m)^(# )?` + regexp.QuoteMeta(f.Name) + ` +# `).Match(b) {
			t.Fatalf("%s missing from:\n%s", f.Name, b)
		}
	}
	if restarts != 1 {
		t.Fatalf("Engine restarts = %d, want 1", restarts)
	}
	if _, err := os.Stat(conf + ".new"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}

	// Unknown names are refused and the file stays as it was.
	if w := postHookFeatures(t, s, `{"off":["stemz"]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown name: %d %s", w.Code, w.Body.String())
	}
	if after, _ := os.ReadFile(conf); !bytes.Equal(after, b) {
		t.Fatalf("file changed by a refused request:\n%s", after)
	}

	// Everything on again, without a restart; hifi-output comes back with dsp-guards even if still listed.
	if w := postHookFeatures(t, s, `{"off":["hifi-output"]}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if b, _ := os.ReadFile(conf); len(parseHookFeatureNames(string(b))) != 0 || restarts != 1 {
		t.Fatalf("restarts=%d file:\n%s", restarts, b)
	}
}

func TestHookFeatureMenuNeedsTheConsoleLogin(t *testing.T) {
	conf, _ := hookFeatureFiles(t, false)
	s := New(Config{AccessToken: "t", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/hook-features", strings.NewReader(`{"off":["gateway"]}`))
	req.RemoteAddr = "192.168.1.50:1234"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("LAN request without login: %d", w.Code)
	}
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Fatal("file written without login")
	}
}

func TestHookFeatureMenuShowsWhatTheRunningEngineHas(t *testing.T) {
	_, state := hookFeatureFiles(t, true)
	s := New(Config{AccessToken: "t", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	if err := os.WriteFile(state, []byte("pid=4242\nbuild=zoom30\noff=stems,zoom30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeHookDisableFile([]string{"stems"}); err != nil {
		t.Fatal(err)
	}
	get := func() hookFeaturesView {
		req := httptest.NewRequest(http.MethodGet, "/api/hook-features", nil)
		req.Header.Set("Authorization", "Bearer t")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		var v hookFeaturesView
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		return v
	}
	find := func(v hookFeaturesView, name string) hookFeatureView {
		for _, f := range v.Features {
			if f.Name == name {
				return f
			}
		}
		t.Fatalf("%s not in the menu", name)
		return hookFeatureView{}
	}
	v := get()
	if !v.EngineKnown || v.RestartNeeded {
		t.Fatalf("file matches the Engine: %+v", v)
	}
	if f := find(v, "zoom30"); !f.Build || f.Off || f.Running != "off" {
		t.Fatalf("zoom30 left out by the build: %+v", f)
	}
	if f := find(v, "stems"); !f.Off || f.Running != "off" {
		t.Fatalf("stems: %+v", f)
	}
	if f := find(v, "gateway"); f.Off || f.Running != "on" {
		t.Fatalf("gateway: %+v", f)
	}
	// The file changed since Engine started: a restart is due.
	if err := writeHookDisableFile([]string{"stems", "gateway"}); err != nil {
		t.Fatal(err)
	}
	if v := get(); !v.RestartNeeded {
		t.Fatalf("file differs from the Engine, no restart asked: %+v", v)
	}
	// mount-hook waits for a reboot, not an Engine restart.
	if err := writeHookDisableFile([]string{"stems", "mount-hook"}); err != nil {
		t.Fatal(err)
	}
	if v := get(); v.RestartNeeded || !find(v, "mount-hook").Reboot {
		t.Fatalf("mount-hook asks for an Engine restart: %+v", v)
	}

	// QML gets the running Engine's list from the loopback status.
	ui := func() string {
		req := httptest.NewRequest(http.MethodGet, "/v1/enginex/ui", nil)
		req.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	if body := ui(); !strings.Contains(body, `"featuresOff":["stems","zoom30"]`) {
		t.Fatalf("featuresOff missing: %s", body)
	}
	// The paused shadow follow is on only when the running hook says it does it: a hook of
	// before it lists it nowhere, and the shadow deck must then play as before.
	if body := ui(); strings.Contains(body, `"shadowPausedFollow"`) {
		t.Fatalf("a hook that does not say it follows: %s", body)
	}
	for _, c := range []struct {
		state string
		want  bool
	}{
		{"pid=4242\nbuild=\noff=\non=shadow-paused-follow\n", true},
		{"pid=4242\nbuild=\noff=shadow-paused-follow\non=shadow-paused-follow\n", false},
		{"pid=4242\nbuild=shadow-paused-follow\noff=shadow-paused-follow\n", false},
		{"pid=99\nbuild=\noff=\non=shadow-paused-follow\n", false}, // an Engine that no longer runs
	} {
		if err := os.WriteFile(state, []byte(c.state), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(ui(), `"shadowPausedFollow":true`); got != c.want {
			t.Fatalf("%q: shadowPausedFollow %v, want %v", c.state, got, c.want)
		}
		// The answer to a setting the pack posts says the same: the pack applies it like a
		// poll's, and one without the hook's state un-paused the copy until the next poll.
		req := httptest.NewRequest(http.MethodPost, "/v1/enginex/ui", nil)
		req.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if got := strings.Contains(w.Body.String(), `"shadowPausedFollow":true`); w.Code != http.StatusOK || got != c.want {
			t.Fatalf("%q: a POST's answer (%d) has shadowPausedFollow %v, want %v", c.state, w.Code, got, c.want)
		}
	}
	// A state file of an Engine that no longer runs says nothing: every row stays.
	if err := os.WriteFile(state, []byte("pid=99\nbuild=\noff=dsp-guards\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body := ui(); strings.Contains(body, `"featuresOff"`) {
		t.Fatalf("stale state used: %s", body)
	}
	if v := get(); v.EngineKnown || v.RestartNeeded || find(v, "dsp-guards").Running != "" {
		t.Fatalf("stale state used: %+v", v)
	}
}

// The console's feature card looks names up as hfd_<name> and groups as hfg_<group>,
// which TestConsoleIsFullyTranslated cannot see: every one needs all four languages.
func TestHookFeatureMenuIsTranslated(t *testing.T) {
	b, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, f := range hookFeatures {
		for _, key := range []string{"hfd_" + strings.ReplaceAll(f.Name, "-", "_"), "hfg_" + f.Group} {
			re := regexp.MustCompile(`\b` + key + `:\['(?:[^'\\]|\\.)+','(?:[^'\\]|\\.)+','(?:[^'\\]|\\.)+','(?:[^'\\]|\\.)+'\]`)
			if !re.MatchString(page) {
				t.Errorf("%s: not in all four languages", key)
			}
		}
	}
}
