package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
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
		"install.sh":  "#!/bin/sh\nset -eu\ncp \"$(dirname \"$0\")/version.txt\" \"$ENGINEX_CUSTOM/version.txt\"\n: > \"$ENGINEX_CUSTOM/in-place\"\n",
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

// #234: a bundle of an older release than the installed one (a downgrade by upload) never runs
// its install.sh over /data (review R1/R2): installOlder stages it and swaps it in
// (internal/leave), and this release's leave step takes its units out of /run and reboots. A
// refusal reaches the screen and changes nothing. A newer one is installed and activated in place
// as before, after the daemons under either suite name have ended.
func TestRename234OlderBundleLeavesAndRebootsInsteadOfActivating(t *testing.T) {
	defer func(a func(string) bool, l func(string) bool, i func(string) error) {
		activatesOnDeck, launchActivation, installOlder = a, l, i
	}(activatesOnDeck, launchActivation, installOlder)
	var scripts, staged []string
	var refuse error
	activatesOnDeck = func(string) bool { return true }
	launchActivation = func(script string) bool { scripts = append(scripts, script); return true }
	for _, c := range []struct {
		version, tag string
		older        bool
		refused      bool
	}{
		{"5.3.0.4", "v5.3.0.4-r5", true, false},
		{"5.3.0.2", "", true, true},
		{"5.3.0.5", "v5.3.0.5-r1", true, false},
		{"5.3.0.5", "v5.3.0.5-r2", false, false},
		{"5.3.0.6", "", false, false},
	} {
		custom := filepath.Join(t.TempDir(), "enginex")
		os.MkdirAll(filepath.Join(custom, "bundle"), 0o700)
		os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.5\n"), 0o644)
		os.WriteFile(filepath.Join(custom, "bundle", releaseTagName), []byte("v5.3.0.5-r2\n"), 0o644)
		installOlder = func(top string) error {
			b, _ := os.ReadFile(filepath.Join(top, "version.txt"))
			staged = append(staged, strings.TrimSpace(string(b)))
			if refuse != nil {
				return refuse
			}
			return os.WriteFile(filepath.Join(custom, "version.txt"), b, 0o644)
		}
		refuse = nil
		if c.refused {
			refuse = errors.New("this deck cannot swap directories atomically")
		}
		s := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: t.TempDir(), VersionFile: filepath.Join(custom, "version.txt")}, nil)
		scripts, staged = nil, nil
		s.applyDataBundleFromReader(bytes.NewReader(rename234Bundle(t, c.version, c.tag)))
		s.Close()
		_, err := os.Stat(filepath.Join(custom, "in-place"))
		inPlace := err == nil
		want := c.version
		if c.refused {
			want = "5.3.0.5"
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); strings.TrimSpace(string(b)) != want {
			t.Fatalf("%s %s: version %q, want %s (%+v)", c.version, c.tag, b, want, s.updateCache)
		}
		if c.older && (inPlace || len(staged) != 1 || staged[0] != c.version) {
			t.Fatalf("older %s %s: install.sh in place %v, staged %q", c.version, c.tag, inPlace, staged)
		}
		if !c.older && (!inPlace || len(staged) != 0) {
			t.Fatalf("newer %s %s: install.sh in place %v, staged %q", c.version, c.tag, inPlace, staged)
		}
		if c.refused {
			if len(scripts) != 0 || !strings.Contains(s.updateCache.Error, "cannot swap directories atomically") {
				t.Errorf("refused %s: activations %q, error %q", c.version, scripts, s.updateCache.Error)
			}
			continue
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

// Settings copy authorizes the same local screen after the gateway canonicalizes the old prefix.
func TestRename234SettingsCopyLocalScreen(t *testing.T) {
	for _, prefix := range []string{"/v1/enginex/", "/v1/sc5000/"} {
		t.Run(prefix, func(t *testing.T) {
			r := httptest.NewRequest("POST", prefix+"settings-copy", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			if !copyLocalScreen(r) {
				t.Errorf("local screen refused under %s", prefix)
			}
			r.Header.Set("Cf-Connecting-Ip", "192.0.2.1")
			if copyLocalScreen(r) {
				t.Errorf("relayed screen accepted under %s", prefix)
			}
			r.Header.Del("Cf-Connecting-Ip")
			r.RemoteAddr = "192.0.2.1:1234"
			if copyLocalScreen(r) {
				t.Errorf("remote screen accepted under %s", prefix)
			}
		})
	}
}

func TestRename234SyntheticTidalAccountsStayLocal(t *testing.T) {
	for _, token := range []string{"enginex-token", "enginex-refresh", "sc5000-token", "sc5000-refresh"} {
		t.Run(token, func(t *testing.T) {
			g := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
			if err := g.tidal.saveRealAuth(tidalAuth{AccessToken: token, RefreshToken: token}); err != nil {
				t.Fatal(err)
			}
			if g.tidal.hasRealAuth() {
				t.Errorf("synthetic account %q became real upstream credentials", token)
			}
			g.Close()
		})
	}
}
