package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"

	"github.com/Cd1s/EngineX-OS/internal/migration"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #234: the deck's state dir is recognised under the new path and through the
// real pre-5.3.0.5 data directory an older unit passes during an upgrade;
// a test's own dir is not the deck's.
func TestRename234DeckStateDirUnderBothNames(t *testing.T) {
	for dir, want := range map[string]bool{
		"/data/enginex/state":        true,
		migration.DataDir + "/state": true,
		"/data/enginex":              false,
		t.TempDir():                  false,
		"":                           false,
	} {
		if got := isDeckStateDir(dir); got != want {
			t.Errorf("isDeckStateDir(%q) = %v, want %v", dir, got, want)
		}
	}
}

// #234: shadow telemetry turns on with the new hand flag and, for two releases, the old one.
func TestRename234ShadowDebugFlagUnderBothNames(t *testing.T) {
	if shadowDebugFlags != [...]string{"/tmp/enginex-debug", migration.DebugFlag} {
		t.Errorf("deck flag files %q", shadowDebugFlags)
	}
	dir := t.TempDir()
	flags := []string{filepath.Join(dir, "enginex-debug"), filepath.Join(dir, migration.ProjectName+"-debug")}
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

// Only owned socket aliases are removed; real sockets and unrelated links survive.
func TestRename234RemovesLegacySocketAlias(t *testing.T) {
	old := legacyTailscaleSocket
	defer func() { legacyTailscaleSocket = old }()
	legacyTailscaleSocket = filepath.Join(t.TempDir(), "old.sock")
	for _, target := range []string{"enginex-tailscaled.sock", "/run/enginex-tailscaled.sock", "foreign.sock"} {
		if err := os.Symlink(target, legacyTailscaleSocket); err != nil {
			t.Fatal(err)
		}
		removeLegacyTailscaleSocket()
		_, err := os.Lstat(legacyTailscaleSocket)
		if target == "foreign.sock" {
			if err != nil {
				t.Fatal("unrelated link removed")
			}
			os.Remove(legacyTailscaleSocket)
		} else if !os.IsNotExist(err) {
			t.Fatal("owned alias remains")
		}
	}
	if err := os.WriteFile(legacyTailscaleSocket, []byte("old daemon"), 0600); err != nil {
		t.Fatal(err)
	}
	removeLegacyTailscaleSocket()
	if b, err := os.ReadFile(legacyTailscaleSocket); err != nil || string(b) != "old daemon" {
		t.Fatal("real socket placeholder changed")
	}
}

// Every local API route is registered under the current namespace only.
func TestRename234EveryRouteUsesCurrentPrefix(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	registered := regexp.MustCompile(`(?:HandleFunc|safeRegisterRoute)\([^"]*"([A-Z]+) (/v1/enginex/[^"]+)"`)
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	defer s.Close()
	count := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range registered.FindAllSubmatch(b, -1) {
			count++
			route := regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(string(m[2]), "x")
			if _, pattern := s.tidal.mux.Handler(httptest.NewRequest(string(m[1]), route, nil)); pattern == "" {
				t.Errorf("missing current route %s", route)
			}
		}
	}
	if count < 40 {
		t.Fatalf("found only %d current routes", count)
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

// Uploading a downgrade is refused before its installer or activation can run.
func TestRename234OlderBundleIsRefused(t *testing.T) {
	original := launchActivation
	defer func() { launchActivation = original }()
	launched := false
	launchActivation = func(string) bool { launched = true; return true }
	for _, version := range []string{"5.3.0.2", "5.3.0.4"} {
		t.Run(version, func(t *testing.T) {
			custom := filepath.Join(t.TempDir(), "enginex")
			os.MkdirAll(filepath.Join(custom, "bundle"), 0700)
			os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.5\n"), 0600)
			s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), VersionFile: filepath.Join(custom, "version.txt")}, nil)
			defer s.Close()
			s.applyDataBundleFromReader(bytes.NewReader(rename234Bundle(t, version, "v"+version)))
			if !strings.Contains(s.updateCache.Error, "downgrade") {
				t.Fatalf("refusal: %s", s.updateCache.Error)
			}
			if launched {
				t.Fatal("downgrade activated")
			}
			if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.5\n" {
				t.Fatal("live version changed")
			}
			if _, err := os.Stat(filepath.Join(custom, "in-place")); !os.IsNotExist(err) {
				t.Fatal("old installer ran")
			}
		})
	}
}

// Settings copy authorizes only the current local screen namespace.
func TestRename234SettingsCopyLocalScreen(t *testing.T) {
	for _, prefix := range []string{"/v1/enginex/"} {
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
	for _, token := range []string{"enginex-token", "enginex-refresh", migration.Token, migration.RefreshToken} {
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

// Engine persists these wire values; current output stays synthetic on upgrade.
func TestRename234IssuedTidalTokensStaySynthetic(t *testing.T) {
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	defer s.Close()
	r := httptest.NewRequest("POST", "/au/oauth2/token", strings.NewReader("grant_type=refresh_token&refresh_token=enginex-refresh"))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("token endpoint: %d %s", w.Code, w.Body.String())
	}
	var tokens map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &tokens); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"access_token", "refresh_token"} {
		if tokens[key] != "enginex" {
			t.Errorf("%s = %q, want current synthetic output", key, tokens[key])
		}
	}
}

func TestRename234StemsListWritesCurrentIDAndReadsSavedLegacyID(t *testing.T) {
	if got := stemsListID("deezer"); got != "enginex-stems-deezer" {
		t.Fatalf("current list ID: %q", got)
	}
	for _, id := range []string{"enginex-stems-deezer", migration.StemsListPrefix + "deezer"} {
		if !isStemsListID(id, "deezer") || isStemsListID(id, "spotify") {
			t.Errorf("list scope mismatch for %q", id)
		}
	}
}
