package webui

import (
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
