package webui

import (
	"os"
	"path/filepath"
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
