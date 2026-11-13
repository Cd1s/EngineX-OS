package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #234: the deck's state dir is recognised under the new path and through the
// /data/sc5000-custom compat link an older unit still passes during an update;
// a test's own dir is not the deck's.
func TestRename234DeckStateDirUnderBothNames(t *testing.T) {
	for dir, want := range map[string]bool{
		"/data/enginex/state":       true,
		"/data/sc5000-custom/state": true,
		"/data/enginex":             false,
		t.TempDir():                 false,
		"":                          false,
	} {
		if got := isDeckStateDir(dir); got != want {
			t.Errorf("isDeckStateDir(%q) = %v, want %v", dir, got, want)
		}
	}
}

// #234: shadow telemetry turns on with the new hand flag and, for two releases, the old one.
func TestRename234ShadowDebugFlagUnderBothNames(t *testing.T) {
	if shadowDebugFlags != [...]string{"/tmp/enginex-debug", "/tmp/sc5000-debug"} {
		t.Errorf("deck flag files %q", shadowDebugFlags)
	}
	dir := t.TempDir()
	flags := []string{filepath.Join(dir, "enginex-debug"), filepath.Join(dir, "sc5000-debug")}
	if anyFileExists(flags) {
		t.Fatal("telemetry on without a flag file")
	}
	for _, f := range flags {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if !anyFileExists(flags) {
			t.Errorf("%s does not turn the telemetry on", filepath.Base(f))
		}
		os.Remove(f)
	}
}

// #234: a Tailscale Remote Mod installed before 5.3.0.5 asks the old socket name; the console
// links it to the new socket, replaces its own stale link, and never touches a real socket file.
func TestRename234LegacyTailscaleSocketLink(t *testing.T) {
	dir := t.TempDir()
	old := legacyTailscaleSocket
	defer func() { legacyTailscaleSocket = old }()
	legacyTailscaleSocket = filepath.Join(dir, "sc5000-tailscaled.sock")
	for i := 0; i < 2; i++ {
		linkLegacyTailscaleSocket()
		if got, err := os.Readlink(legacyTailscaleSocket); err != nil || got != "enginex-tailscaled.sock" {
			t.Fatalf("run %d: link %q %v", i, got, err)
		}
	}
	os.Remove(legacyTailscaleSocket)
	if err := os.WriteFile(legacyTailscaleSocket, []byte("old daemon"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkLegacyTailscaleSocket()
	if b, err := os.ReadFile(legacyTailscaleSocket); err != nil || string(b) != "old daemon" {
		t.Fatalf("an old daemon's socket was replaced: %q %v", b, err)
	}
	if old != "/run/sc5000-tailscaled.sock" || tailscaleSocket != "/run/enginex-tailscaled.sock" || legacyTailscaleUnit != "sc5000-tailscaled" {
		t.Errorf("deck names %q %q %q", old, tailscaleSocket, legacyTailscaleUnit)
	}
}

// #234: /v1/enginex is the deck API's primary prefix. No route is registered under the old
// one, and every route answers under /v1/sc5000 too, through the one canonicalV1 mapping
// (the permanent alias for installers, UI packs, Mods and scripts of every release).
func TestRename234EveryRouteUnderBothPrefixes(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	route := regexp.MustCompile(`"((?:GET|POST|PUT|DELETE) )?(/v1/[a-z0-9]+/[^"]*)"`)
	registered := regexp.MustCompile(`(?:HandleFunc|safeRegisterRoute)\([^"]*"([A-Z]+) (/v1/[^"]+)"`)
	type r struct{ method, path string }
	var routes []r
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range registered.FindAllSubmatch(b, -1) {
			routes = append(routes, r{string(m[1]), string(m[2])})
		}
		for _, m := range route.FindAllSubmatch(b, -1) {
			if strings.HasPrefix(string(m[2]), "/v1/sc5000/") {
				t.Errorf("%s still names %s", f, m[2])
			}
		}
	}
	if len(routes) < 40 {
		t.Fatalf("found only %d /v1 routes", len(routes))
	}
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	defer s.Close()
	for _, rt := range routes {
		rest, ok := strings.CutPrefix(rt.path, "/v1/enginex/")
		if !ok {
			continue // the TIDAL-compatible gateway's own /v1 paths
		}
		concrete := regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(rest, "x")
		if got := canonicalV1("/v1/sc5000/" + concrete); got != "/v1/enginex/"+concrete {
			t.Errorf("canonicalV1(/v1/sc5000/%s) = %s", concrete, got)
		}
		if _, pattern := s.tidal.mux.Handler(httptest.NewRequest(rt.method, "/v1/enginex/"+concrete, nil)); pattern == "" {
			t.Errorf("%s /v1/enginex/%s is not served by the gateway", rt.method, concrete)
		}
	}
	for _, p := range []string{"/v1/sc5000x/ui", "/v1/sc50000/ui", "/v1/enginexx/ui", "/v2/sc5000/ui"} {
		if got := canonicalV1(p); got != p {
			t.Errorf("canonicalV1(%s) = %s; only the exact old prefix maps", p, got)
		}
	}
	if canonicalV1("/v1/sc5000") != "/v1/enginex" || canonicalV1("/v1/enginex/ui") != "/v1/enginex/ui" {
		t.Error("prefix itself or the new prefix mapped wrongly")
	}
}

// rename234Bundle is a minimal Data-Bundle.tar.gz whose install.sh only records its version.
func rename234Bundle(t *testing.T, version, tag string) []byte {
	t.Helper()
	files := map[string]string{
		"install.sh":  "#!/bin/sh\nset -eu\ncp \"$(dirname \"$0\")/version.txt\" \"$ENGINEX_CUSTOM/version.txt\"\n",
		"version.txt": version + "\n",
		"SHA256SUMS":  "", // checked by install.sh, which this fake one skips
	}
	if tag != "" {
		files[releaseTagName] = tag + "\n"
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	top := "SCLIVE4-" + version + "-EngineX-OS-Data-Bundle"
	tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	for n, c := range files {
		tw.WriteHeader(&tar.Header{Name: top + "/" + n, Mode: 0o755, Size: int64(len(c)), Typeflag: tar.TypeReg})
		tw.Write([]byte(c))
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

// #234: a bundle of an older release than the installed one (a downgrade by upload) is installed,
// but never activated in place: this release's leave step takes its units out of /run and
// reboots. A newer one is activated in place as before, after the daemons under either suite
// name have ended.
func TestRename234OlderBundleLeavesAndRebootsInsteadOfActivating(t *testing.T) {
	defer func(a func(string) bool, l func(string) bool) { activatesOnDeck, launchActivation = a, l }(activatesOnDeck, launchActivation)
	var scripts []string
	activatesOnDeck = func(string) bool { return true }
	launchActivation = func(script string) bool { scripts = append(scripts, script); return true }
	for _, c := range []struct {
		version, tag string
		older        bool
	}{
		{"5.3.0.4", "v5.3.0.4-r5", true},
		{"5.3.0.5", "v5.3.0.5-r1", true},
		{"5.3.0.5", "v5.3.0.5-r2", false},
		{"5.3.0.6", "", false},
	} {
		custom := filepath.Join(t.TempDir(), "enginex")
		os.MkdirAll(filepath.Join(custom, "bundle"), 0o700)
		os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.5\n"), 0o644)
		os.WriteFile(filepath.Join(custom, "bundle", releaseTagName), []byte("v5.3.0.5-r2\n"), 0o644)
		s := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: t.TempDir(), VersionFile: filepath.Join(custom, "version.txt")}, nil)
		scripts = nil
		s.applyDataBundleFromReader(bytes.NewReader(rename234Bundle(t, c.version, c.tag)))
		s.Close()
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); strings.TrimSpace(string(b)) != c.version {
			t.Fatalf("%s %s: not installed (%q, %+v)", c.version, c.tag, b, s.updateCache)
		}
		if len(scripts) != 1 {
			t.Fatalf("%s %s: %d activations", c.version, c.tag, len(scripts))
		}
		leaves := strings.Contains(scripts[0], "exec sh /run/enginex/leave.sh") && strings.Contains(scripts[0], "systemctl reboot")
		activates := strings.Contains(scripts[0], "/bundle/boot-activate.sh")
		if c.older && (!leaves || activates) {
			t.Errorf("older %s %s: %q, want the leave step and a reboot, no boot-activate.sh", c.version, c.tag, scripts[0])
		}
		if !c.older && (leaves || !activates || !strings.Contains(scripts[0], "[e]nginex-suite (stream-sync|rb-bridge|enginex-deemixd)") ||
			!strings.Contains(scripts[0], "[s]c5000-suite (stream-sync|rb-bridge|sc5000-deemixd)")) {
			t.Errorf("newer %s %s: %q, want boot-activate.sh after ending both suite names", c.version, c.tag, scripts[0])
		}
	}
}
