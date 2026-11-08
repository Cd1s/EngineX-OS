package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestUpdateChecksTheConfiguredGitHubRepo(t *testing.T) {
	var asked string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		if r.URL.Path != "/repos/me/fork/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"tag_name":"v2","html_url":"https://github.com/me/fork/releases/v2","assets":[
			{"name":"SC5000M-2-EngineX-OS-Update.img","browser_download_url":"https://example.invalid/m.img"},
			{"name":"SC5000-2-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img","browser_download_url":"https://example.invalid/sc5000.img"},
			{"name":"SHA256SUMS.txt","browser_download_url":"https://example.invalid/sums"}]}`))
	}))
	defer gh.Close()
	ui, state := t.TempDir(), t.TempDir()
	compatible := filepath.Join(state, "compatible")
	os.WriteFile(compatible, []byte("inmusic,jp07\x00rockchip,rk3288\x00"), 0o644)
	defer func(old string) { deviceCompatiblePath = old }(deviceCompatiblePath)
	deviceCompatiblePath = compatible
	version := filepath.Join(state, "version.txt")
	os.WriteFile(version, []byte("v1\n"), 0o644)
	s := New(Config{AccessToken: "c", UIDir: ui, StateDir: state, GitHubURL: gh.URL, VersionFile: version}, nil)
	get := func() updateStatus {
		w := httptest.NewRecorder()
		s.update(w, httptest.NewRequest("GET", "/v1/sc5000/update", nil))
		var v updateStatus
		json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	if v := get(); v.Repo != defaultGitHubRepo || v.Error == "" || v.UpdateAvailable {
		t.Fatalf("default repo without releases: %+v", v)
	}
	// A bad name never reaches the URL; a valid one survives a settings save.
	os.WriteFile(filepath.Join(ui, "settings.conf"), []byte("github_repo=../x\n"), 0o644)
	if s.loadUI().GitHubRepo != defaultGitHubRepo {
		t.Fatal("accepted an invalid repository name")
	}
	os.WriteFile(filepath.Join(ui, "settings.conf"), []byte("github_repo=me/fork\n"), 0o644)
	if err := s.saveUI(s.loadUI()); err != nil || s.loadUI().GitHubRepo != "me/fork" {
		t.Fatal("github_repo lost on save")
	}
	if v := get(); asked != "/repos/me/fork/releases/latest" || v.Current != "v1" || v.Latest != "v2" || !v.UpdateAvailable || v.Error != "" || v.DownloadURL != "https://example.invalid/sc5000.img" {
		t.Fatalf("update %+v (asked %s)", v, asked)
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v2", "v1", true},
		{"v1", "v1", false},
		{"v1", "v2", false}, // rollback / lower latest
		{"v5.0.4-custom.10", "v5.0.4-custom.9", true},
		{"v5.0.4-custom.1", "v5.0.4", true},
		{"v5.0.4", "dev", false}, // unknown current version
		{"", "v1", false},
	} {
		if got := newerVersion(c.latest, c.current); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v", c.latest, c.current, got)
		}
	}
}

func TestImagePrefix(t *testing.T) {
	for in, want := range map[string]string{
		"inmusic,jp07\x00rockchip,rk3288":  "SC5000",
		"inmusic,jc16":                     "PRIME2",
		"inmusic,jp21":                     "SCLIVE4",
		"inmusic,jp20":                     "SCLIVE2",
		"inmusic,jc11s\x00rockchip,rk3288": "PRIME4PLUS",
		"inmusic,jp11s":                    "PRIMEGOPLUS",
		"inmusic,nh08s":                    "MIXSTREAMPROPLUS",
		"inmusic,nh10":                     "MIXSTREAMPROGO",
		"inmusic,jp24\x00inmusic,az04":     "PRIME4G2",
		"inmusic,rmz2\x00inmusic,az04":     "SYSTEMONE",
		"rockchip,rk3288":                  "",
	} {
		if got := imagePrefix(strings.Split(in, "\x00")); got != want {
			t.Errorf("imagePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// bundleInstallScript is the real install.sh that build-data-bundle.py ships.
func bundleInstallScript(t *testing.T) []byte {
	t.Helper()
	src, err := os.ReadFile("../../tools/az01-packer/build-data-bundle.py")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\nINSTALL = r'''(.*?)'''`).FindSubmatch(src)
	if m == nil {
		t.Fatal("INSTALL not found in build-data-bundle.py")
	}
	// The packer expands @MIGRATE_IF_DEFAULT@ into the /data migration; the test deck is not
	// a real /data (SC5000_CUSTOM is set), so there is nothing to migrate.
	return bytes.ReplaceAll(m[1], []byte("@MIGRATE_IF_DEFAULT@"), []byte(":"))
}

func tarOf(t *testing.T, files map[string]string) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "packs/", Typeflag: tar.TypeDir, Mode: 0o755})
	for n, c := range files {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c))})
		tw.Write([]byte(c))
	}
	tw.Close()
	return b.Bytes()
}

// testBundle builds a Data-Bundle.tar.gz with the real layout; badSums breaks SHA256SUMS.
func testBundle(t *testing.T, prefix, version string, badSums bool) []byte {
	files := map[string][]byte{
		"bin/sc5000-suite": []byte("\x7fELF"), "bin/stream-sync": []byte("#!/bin/sh\n"),
		"lib/libengine-ui-hook.so": []byte("\x7fELF"), "ui-packs.tar": tarOf(t, map[string]string{"packs/a.txt": version}),
		"version.txt": []byte(version + "\n"), "alsa-dir": []byte("system/var-lib/alsa/conf.d\n"),
		"boot-activate.sh": []byte("#!/bin/sh\n"), "99-sc5000-custom.conf": []byte("x\n"),
		"uninstall.sh": []byte("#!/bin/sh\n"), "install.sh": bundleInstallScript(t),
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var sums strings.Builder
	for _, n := range names {
		h := sha256.Sum256(files[n])
		if badSums {
			h[0] ^= 1
		}
		fmt.Fprintf(&sums, "%x  %s\n", h, n)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	top := prefix + "-" + version + "-EngineX-OS-Data-Bundle"
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: top + "/bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: top + "/lib/", Typeflag: tar.TypeDir, Mode: 0o755})
	for n, c := range files {
		mode := int64(0o644)
		if strings.HasSuffix(n, ".sh") || strings.HasPrefix(n, "bin/") {
			mode = 0o755
		}
		tw.WriteHeader(&tar.Header{Name: top + "/" + n, Mode: mode, Size: int64(len(c))})
		tw.Write(c)
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func bundleDeck(t *testing.T, compatible string, assets func(base string) string, bundle []byte) (*Server, string) {
	var base string
	gh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/Cd1s/EngineX-OS/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v5.3.0.3","html_url":"https://x/r","assets":[%s]}`, assets(base))
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			w.Write(bundle)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	base = gh.URL
	state := t.TempDir()
	custom := filepath.Join(state, "sc5000-custom")
	os.MkdirAll(custom, 0o700)
	os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.2\n"), 0o644)
	cf := filepath.Join(state, "compatible")
	os.WriteFile(cf, []byte(compatible+"\x00"), 0o644)
	t.Cleanup(func(old string) func() { return func() { deviceCompatiblePath = old } }(deviceCompatiblePath))
	deviceCompatiblePath = cf
	return New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, GitHubURL: gh.URL, HTTP: gh.Client(),
		VersionFile: filepath.Join(custom, "version.txt")}, nil), custom
}

func getUpdate(s *Server, q string) updateStatus {
	w := httptest.NewRecorder()
	s.update(w, httptest.NewRequest("GET", "/v1/sc5000/update"+q, nil))
	var v updateStatus
	json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func TestUpdatePicksTheDataBundleOnSecondGeneration(t *testing.T) {
	allAssets := func(base string) string {
		var a []string
		for _, n := range []string{"PRIME4-5.3.0.3-EngineX-OS-Denon-PRIME4-JC11-Update.img", "SCLIVE4-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz",
			"PRIME4PLUS-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz", "SYSTEMONE-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz"} {
			a = append(a, fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, n, base+"/dl/"+n))
		}
		return strings.Join(a, ",")
	}
	for compat, want := range map[string]string{
		"inmusic,jp21": "SCLIVE4-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz", "inmusic,jc11s": "PRIME4PLUS-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz",
		"inmusic,rmz2": "SYSTEMONE-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz", "inmusic,jc11": "PRIME4-5.3.0.3-EngineX-OS-Denon-PRIME4-JC11-Update.img",
	} {
		s, _ := bundleDeck(t, compat, allAssets, nil)
		if v := getUpdate(s, ""); !v.UpdateAvailable || !strings.HasSuffix(v.DownloadURL, "/dl/"+want) {
			t.Errorf("%s: %+v, want %s", compat, v, want)
		}
	}
}

func TestUpdatePicksGen2ImgWhenUnlocked(t *testing.T) {
	const img, bundle = "SCLIVE4-5.3.0.3-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img", "SCLIVE4-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz"
	both := func(base string) string {
		return fmt.Sprintf(`{"name":%q,"browser_download_url":%q},{"name":%q,"browser_download_url":%q}`, bundle, base+"/dl/"+bundle, img, base+"/dl/"+img)
	}
	onlyBundle := func(base string) string {
		return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, bundle, base+"/dl/"+bundle)
	}
	for _, c := range []struct {
		name, marker string
		assets       func(string) string
		want         string
	}{
		{"locked by default", "", both, bundle},
		{"unlocked", "gen2-unlocked", both, img},
		{"unlocked, release has no img", "gen2-unlocked", onlyBundle, bundle},
		{"unlocked but gen2-locked wins", "both", both, bundle},
	} {
		s, custom := bundleDeck(t, "inmusic,jp21", c.assets, nil)
		switch c.marker {
		case "gen2-unlocked":
			os.WriteFile(filepath.Join(custom, "gen2-unlocked"), []byte("1\n"), 0o644)
		case "both":
			os.WriteFile(filepath.Join(custom, "gen2-unlocked"), []byte("1\n"), 0o644)
			os.WriteFile(filepath.Join(custom, "gen2-locked"), []byte("1\n"), 0o644)
		}
		if v := getUpdate(s, ""); !strings.HasSuffix(v.DownloadURL, "/dl/"+c.want) {
			t.Errorf("%s: %+v, want %s", c.name, v, c.want)
		}
	}
}

func TestUnlockGen2Endpoint(t *testing.T) {
	call := func(s *Server, method string) map[string]any {
		w := httptest.NewRecorder()
		s.handleUnlockGen2(w, httptest.NewRequest(method, "/v1/sc5000/unlock-gen2", nil))
		var v map[string]any
		json.Unmarshal(w.Body.Bytes(), &v)
		v["status"] = w.Code
		return v
	}
	s, custom := bundleDeck(t, "inmusic,jp21", func(string) string { return "" }, nil)
	if v := call(s, "GET"); v["unlocked"] != false || v["generation"] != float64(2) || v["prefix"] != "SCLIVE4" {
		t.Fatalf("GET before: %v", v)
	}
	os.WriteFile(filepath.Join(custom, "gen2-locked"), []byte("1\n"), 0o644)
	if v := call(s, "POST"); v["ok"] != true || v["unlocked"] != true || v["prefix"] != "SCLIVE4" {
		t.Fatalf("POST: %v", v)
	}
	if _, err := os.Stat(filepath.Join(custom, "gen2-locked")); err == nil || !s.isGen2Unlocked() {
		t.Error("POST must write gen2-unlocked and remove gen2-locked")
	}
	g, _ := bundleDeck(t, "inmusic,jp07", func(string) string { return "" }, nil)
	if v := call(g, "POST"); v["status"] != 400 {
		t.Errorf("1st-gen POST: %v", v)
	}
}

func waitUpdate(t *testing.T, s *Server) updateStatus {
	for i := 0; i < 200; i++ {
		s.updateMu.Lock()
		v := s.updateCache
		s.updateMu.Unlock()
		if !v.Updating {
			return v
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("update did not finish")
	return updateStatus{}
}

func TestUpdateAppliesTheDataBundle(t *testing.T) {
	name := "SCLIVE4-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz"
	assets := func(base string) string {
		return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, name, base+"/dl/"+name)
	}
	t.Run("installs", func(t *testing.T) {
		s, custom := bundleDeck(t, "inmusic,jp21", assets, testBundle(t, "SCLIVE4", "5.3.0.3", false))
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		v := waitUpdate(t, s)
		if v.Error != "" || v.UpdateAvailable || v.Current != "5.3.0.3" || v.Progress != 1 {
			t.Fatalf("after apply: %+v", v)
		}
		for path, want := range map[string]string{"version.txt": "5.3.0.3\n", "bundle/version.txt": "5.3.0.3\n", "ui/packs/a.txt": "5.3.0.3"} {
			if b, err := os.ReadFile(filepath.Join(custom, path)); err != nil || string(b) != want {
				t.Errorf("%s = %q, %v", path, b, err)
			}
		}
		if m, _ := filepath.Glob(filepath.Join(custom, ".ota-bundle-*")); len(m) != 0 {
			t.Errorf("scratch left behind: %v", m)
		}
	})
	t.Run("rejects damaged SHA256SUMS", func(t *testing.T) {
		s, custom := bundleDeck(t, "inmusic,jp21", assets, testBundle(t, "SCLIVE4", "5.3.0.3", true))
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		v := waitUpdate(t, s)
		if !strings.HasPrefix(v.Error, "bundle install:") || !strings.Contains(v.Error, "SHA256SUMS mismatch") {
			t.Fatalf("damaged bundle accepted: %+v", v)
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.2\n" {
			t.Errorf("version.txt changed to %q", b)
		}
		if _, err := os.Stat(filepath.Join(custom, "bundle")); err == nil {
			t.Error("bundle installed despite the mismatch")
		}
	})
}

func TestUnpackBundleRefusesUnsafeArchives(t *testing.T) {
	pack := func(hs ...*tar.Header) string {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		for _, h := range hs {
			tw.WriteHeader(h)
			tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
		}
		tw.Close()
		gz.Close()
		p := filepath.Join(t.TempDir(), "b.tar.gz")
		os.WriteFile(p, b.Bytes(), 0o644)
		return p
	}
	file := func(n string) *tar.Header { return &tar.Header{Name: n, Mode: 0o644, Size: 1} }
	for name, a := range map[string]string{
		"symlink":    pack(file("t/install.sh"), file("t/SHA256SUMS"), &tar.Header{Name: "t/l", Typeflag: tar.TypeSymlink, Linkname: "/etc"}),
		"dotdot":     pack(file("t/../x")),
		"absolute":   pack(file("/t/install.sh")),
		"two tops":   pack(file("t/install.sh"), file("u/SHA256SUMS")),
		"no install": pack(file("t/SHA256SUMS")),
	} {
		if _, err := unpackBundle(a, filepath.Join(t.TempDir(), "x")); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := unpackBundle(pack(file("t/install.sh"), file("t/SHA256SUMS")), filepath.Join(t.TempDir(), "x")); err != nil {
		t.Errorf("valid archive refused: %v", err)
	}
}
