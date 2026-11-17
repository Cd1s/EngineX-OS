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
)

// rb-bridge and deemixd are EngineX services, not hook sites: the boot script leaves
// their units out, so they wait for a reboot like mount-hook.
func TestServiceFeaturesAreInTheMenu(t *testing.T) {
	for _, name := range []string{"rb-bridge", "deemixd"} {
		i := slices.IndexFunc(hookFeatures, func(f hookFeature) bool { return f.Name == name })
		if i < 0 {
			t.Fatalf("%s not in the menu", name)
		}
		if f := hookFeatures[i]; !f.Service || !f.Reboot {
			t.Fatalf("%s: %+v, want a service that waits for a reboot", name, f)
		}
	}
}

// Every need names a known feature listed before it (the menu indents it under that
// feature), and needs form no cycle.
func TestHookFeatureNeedsAreWellFormed(t *testing.T) {
	for i, f := range hookFeatures {
		for _, n := range f.Needs {
			j := slices.IndexFunc(hookFeatures, func(g hookFeature) bool { return g.Name == n })
			if j < 0 || j >= i {
				t.Fatalf("%s needs %q: unknown or listed after it", f.Name, n)
			}
		}
	}
	want := map[string][]string{"gateway": {"sso"}, "deemixd": nil}
	for name, needs := range want {
		i := slices.IndexFunc(hookFeatures, func(f hookFeature) bool { return f.Name == name })
		if i < 0 || !slices.Equal(hookFeatures[i].Needs, needs) {
			t.Fatalf("%s needs %v, want %v", name, hookFeatures[i].Needs, needs)
		}
	}
}

// A feature whose need is off is off too: the file names it, so the hook and the boot
// script (which know no needs) leave it out as well.
func TestHookFeatureOffTakesItsDependentsAlong(t *testing.T) {
	conf, _ := hookFeatureFiles(t, false)
	s := New(Config{AccessToken: "t", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	w := postHookFeatures(t, s, `{"off":["sso"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	b, _ := os.ReadFile(conf)
	if got := parseHookFeatureNames(string(b)); !slices.Equal(got, []string{"sso", "gateway"}) {
		t.Fatalf("file switches off %v:\n%s", got, b)
	}
	var v hookFeaturesView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Features {
		switch f.Name {
		case "gateway":
			if !f.Off || !f.Blocked || !slices.Equal(f.Needs, []string{"sso"}) || f.Depth != 1 {
				t.Fatalf("gateway: %+v", f)
			}
		case "deemixd":
			// The console's Deezer job queue uses deemixd without the gateway.
			if f.Off || f.Blocked || len(f.Needs) != 0 || f.Depth != 0 {
				t.Fatalf("deemixd: %+v", f)
			}
		case "sso":
			if !f.Off || f.Blocked || f.Depth != 0 {
				t.Fatalf("sso: %+v", f)
			}
		}
	}
	if f := hookFeatureViewNamed(t, w, "gateway"); !f.Along {
		t.Fatalf("gateway went off with sso, not marked so: %+v", f)
	}
}

// A feature that went off only with its need comes back with it, even when the
// client still lists it (an old page, the API); one switched off by hand stays off.
func TestHookFeatureComesBackWithItsNeed(t *testing.T) {
	conf, _ := hookFeatureFiles(t, false)
	s := New(Config{AccessToken: "t", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	postHookFeatures(t, s, `{"off":["sso"]}`)
	w := postHookFeatures(t, s, `{"off":["gateway"]}`) // sso back on
	if f := hookFeatureViewNamed(t, w, "gateway"); f.Off || f.Blocked || f.Along {
		t.Fatalf("gateway did not come back with sso: %+v", f)
	}
	b, _ := os.ReadFile(conf)
	if got := parseHookFeatureNames(string(b)); len(got) != 0 {
		t.Fatalf("file still switches off %v:\n%s", got, b)
	}

	// Switched off by hand first: sso off and on again leaves it off.
	postHookFeatures(t, s, `{"off":["gateway"]}`)
	if f := hookFeatureViewNamed(t, postHookFeatures(t, s, `{"off":["sso","gateway"]}`), "gateway"); !f.Off || !f.Blocked || f.Along {
		t.Fatalf("hand-off gateway under sso: %+v", f)
	}
	if f := hookFeatureViewNamed(t, postHookFeatures(t, s, `{"off":["gateway"]}`), "gateway"); !f.Off || f.Along {
		t.Fatalf("hand-off gateway came back with sso: %+v", f)
	}

	// Still off with its need: it stays marked while sso stays off.
	postHookFeatures(t, s, `{"off":[]}`)
	postHookFeatures(t, s, `{"off":["sso"]}`)
	if f := hookFeatureViewNamed(t, postHookFeatures(t, s, `{"off":["sso","gateway","stems"]}`), "gateway"); !f.Off || !f.Along {
		t.Fatalf("gateway lost its mark while sso stayed off: %+v", f)
	}
}

func hookFeatureViewNamed(t *testing.T, w *httptest.ResponseRecorder, name string) hookFeatureView {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var v hookFeaturesView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Features {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("%s not in the menu", name)
	return hookFeatureView{}
}

// The hook knows no service names: QML hides the Pro DJ Link rows by the file instead,
// added to what the running Engine reports.
func TestServiceFeaturesOffReachQML(t *testing.T) {
	_, state := hookFeatureFiles(t, true)
	s := New(Config{AccessToken: "t", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	if err := os.WriteFile(state, []byte("pid=4242\nbuild=\noff=stems\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeHookDisableFile([]string{"stems", "rb-bridge"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/enginex/ui", nil)
	req.RemoteAddr = "127.0.0.1:4000"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if body := w.Body.String(); !strings.Contains(body, `"featuresOff":["stems","rb-bridge"]`) {
		t.Fatalf("featuresOff: %s", body)
	}
	// A service switched off waits for a reboot, never asks for an Engine restart.
	req = httptest.NewRequest(http.MethodGet, "/api/hook-features", nil)
	req.Header.Set("Authorization", "Bearer t")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	var v hookFeaturesView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.RestartNeeded {
		t.Fatalf("restart asked for a service: %s", w.Body.String())
	}
}

// Engine 5.3.0 names its main thread EMain, so /proc/<pid>/comm says EMain, not
// Engine (real SC LIVE 4): the state file was taken for stale and no row ever hid.
// The executable's name counts.
func TestEngineAliveByItsExecutable(t *testing.T) {
	proc := t.TempDir()
	write := func(pid, comm, cmdline string) {
		dir := filepath.Join(proc, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644)
	}
	write("835", "EMain", "/usr/Engine/Engine\x00-d0\x00")
	write("900", "Engine", "/usr/Engine/Engine\x00")
	write("901", "sh", "/bin/sh\x00/usr/Engine/Engine\x00")
	write("902", "EMain", "/usr/bin/other\x00")
	// Under qemu-user (binfmt, x86 gate host) the interpreter comes first.
	write("904", "EMain", "/usr/bin/qemu-arm\x00/usr/Engine/Engine\x00-d0\x00")
	write("905", "sh", "/usr/bin/qemu-arm-static\x00/bin/sh\x00/usr/Engine/Engine\x00")
	for pid, want := range map[int]bool{835: true, 900: true, 901: false, 902: false, 903: false, 904: true, 905: false} {
		if got := engineAliveIn(proc, pid); got != want {
			t.Errorf("pid %d: alive %v, want %v", pid, got, want)
		}
	}
}

// stream-sync asks the same file whether a service is switched off (gen-1 deemixd).
func TestHookFeatureOffReadsTheFile(t *testing.T) {
	hookFeatureFiles(t, false)
	if HookFeatureOff("deemixd") {
		t.Fatal("no file: deemixd off")
	}
	if err := writeHookDisableFile([]string{"stems", "deemixd"}); err != nil {
		t.Fatal(err)
	}
	if !HookFeatureOff("deemixd") || !HookFeatureOff("stems") || HookFeatureOff("gateway") {
		t.Fatal("HookFeatureOff does not follow hook-disable.conf")
	}
}
