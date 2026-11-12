package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
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
		case strings.HasSuffix(r.URL.Path, ".tar.gz") || strings.HasSuffix(r.URL.Path, "-Update.img"):
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
		{"unlocked by default when gen2-locked is absent", "", both, img},
		{"explicit gen2-locked falls back to bundle", "gen2-locked", both, bundle},
		{"unlocked", "gen2-unlocked", both, img},
		{"unlocked, release has no img", "gen2-unlocked", onlyBundle, bundle},
		{"default, release has no img", "", onlyBundle, bundle},
		{"unlocked but gen2-locked wins", "both", both, bundle},
	} {
		s, custom := bundleDeck(t, "inmusic,jp21", c.assets, nil)
		switch c.marker {
		case "gen2-locked":
			os.WriteFile(filepath.Join(custom, "gen2-locked"), []byte("1\n"), 0o644)
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
	t.Run("applies Gen-2 Update.img in fallback path", func(t *testing.T) {
		imgName := "SCLIVE4-5.3.0.3-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img"
		imgAssets := func(base string) string {
			return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, imgName, base+"/dl/"+imgName)
		}
		bundle := testBundle(t, "SCLIVE4", "5.3.0.3", false)
		sum := sha256.Sum256(bundle)
		var head [gen2ImgHeader]byte
		copy(head[:], fmt.Sprintf("%s%x\nSCLIVE4\n5.3.0.3\n", gen2ImgMagic, sum))
		validImg := append(head[:], bundle...)

		s, custom := bundleDeck(t, "inmusic,jp21", imgAssets, validImg)
		// Explicit gen2-locked: checkRelease finds no bundle asset, then POST unlock-gen2 invalidates cache and picks .img.
		os.WriteFile(filepath.Join(custom, "gen2-locked"), []byte("1\n"), 0o644)
		if v := getUpdate(s, ""); v.DownloadURL != "" {
			t.Fatalf("locked deck picked .img: %+v", v)
		}
		lockedPart := filepath.Join(custom, "locked.part")
		os.WriteFile(lockedPart, validImg, 0o644)
		if !s.tryApplyGen2Image(lockedPart) || s.updateCache.Error != "gen2 .img update is locked" {
			t.Fatalf("tryApplyGen2Image did not block gen2-locked: %+v", s.updateCache)
		}
		s.handleUnlockGen2(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/sc5000/unlock-gen2", nil))
		if v := getUpdate(s, ""); !strings.HasSuffix(v.DownloadURL, "/dl/"+imgName) {
			t.Fatalf("after unlock: %+v", v)
		}
		getUpdate(s, "?apply=1")
		v := waitUpdate(t, s)
		if v.Error != "" || v.UpdateAvailable || v.Current != "5.3.0.3" || v.Progress != 1 {
			t.Fatalf("after Gen-2 .img apply: %+v", v)
		}
		for path, want := range map[string]string{"version.txt": "5.3.0.3\n", "bundle/version.txt": "5.3.0.3\n", "ui/packs/a.txt": "5.3.0.3", "gen2-unlocked": "1\n"} {
			if b, err := os.ReadFile(filepath.Join(custom, path)); err != nil || string(b) != want {
				t.Errorf("%s = %q, %v", path, b, err)
			}
		}

		// Legacy 5.3.0.3 / early 5.3.0.4 install without gen2-unlocked or gen2-locked also picks and applies .img directly.
		sDefault, customDefault := bundleDeck(t, "inmusic,jp21", imgAssets, validImg)
		if v := getUpdate(sDefault, ""); !strings.HasSuffix(v.DownloadURL, "/dl/"+imgName) {
			t.Fatalf("default Gen-2 deck did not pick .img: %+v", v)
		}
		getUpdate(sDefault, "?apply=1")
		if v := waitUpdate(t, sDefault); v.Error != "" || v.Current != "5.3.0.3" {
			t.Fatalf("default Gen-2 .img apply failed: %+v", v)
		}
		if b, _ := os.ReadFile(filepath.Join(customDefault, "gen2-unlocked")); string(b) != "1\n" {
			t.Errorf("gen2-unlocked not written after apply: %q", b)
		}

		// Corrupted payload byte in Gen-2 .img must be rejected by SHA-256 check.
		badImg := append([]byte(nil), validImg...)
		badImg[len(badImg)-1] ^= 1
		sBad, customBad := bundleDeck(t, "inmusic,jp21", imgAssets, badImg)
		os.WriteFile(filepath.Join(customBad, "gen2-unlocked"), []byte("1\n"), 0o644)
		getUpdate(sBad, "")
		getUpdate(sBad, "?apply=1")
		vBad := waitUpdate(t, sBad)
		if vBad.Error != "downloaded image failed SHA-256 check" {
			t.Fatalf("corrupted Gen-2 .img accepted: %+v", vBad)
		}
		if b, _ := os.ReadFile(filepath.Join(customBad, "version.txt")); string(b) != "5.3.0.2\n" {
			t.Errorf("version.txt changed to %q", b)
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

func TestApplyOnlineUpdateGen2PreloadCompatibility(t *testing.T) {
	oldScript, oldPlanck := engineScriptPath, planckRemoteScreenPath
	oldURLFile, oldReasonFile := updateFileURLPath, engineQuitReasonPath
	oldAllow, oldQuit := allowUpdateFromURL, quitEngineForUpdate
	t.Cleanup(func() {
		engineScriptPath, planckRemoteScreenPath = oldScript, oldPlanck
		updateFileURLPath, engineQuitReasonPath = oldURLFile, oldReasonFile
		allowUpdateFromURL, quitEngineForUpdate = oldAllow, oldQuit
	})

	makeValidGen2Img := func(t *testing.T, prefix, version string) []byte {
		t.Helper()
		bundle := testBundle(t, prefix, version, false)
		sum := sha256.Sum256(bundle)
		var head [gen2ImgHeader]byte
		copy(head[:], fmt.Sprintf("%s%x\n%s\n%s\n", gen2ImgMagic, sum, prefix, version))
		return append(head[:], bundle...)
	}

	t.Run("Gen-2 without LD_PRELOAD on planck-remote-screen skips UpdateFromUrl and installs via tryApplyGen2Image", func(t *testing.T) {
		tmp := t.TempDir()
		engineScriptPath = filepath.Join(tmp, "engine")
		planckRemoteScreenPath = filepath.Join(tmp, "planck-remote-screen")
		updateFileURLPath = filepath.Join(tmp, "update-file-url")
		engineQuitReasonPath = filepath.Join(tmp, "engine-quit-reason")
		os.WriteFile(planckRemoteScreenPath, []byte("#!/bin/sh\n"), 0o755)
		// Earlier 5.3.0.4 script: Engine is wrapped with engine-ui-guard, planck-remote-screen is NOT wrapped with LD_PRELOAD.
		os.WriteFile(engineScriptPath, []byte(
			"#!/bin/sh\n\t/data/enginex/bundle/bin/engine-ui-guard launch /usr/Engine/Engine -d0\n\t/usr/bin/planck-remote-screen --update \"$UPDATE_FILE\"\n",
		), 0o755)
		allowUpdateFromURL = func(string) bool { return true }
		quitCalls := 0
		quitEngineForUpdate = func() error { quitCalls++; return nil }

		imgName := "SCLIVE4-5.3.0.3-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img"
		assets := func(base string) string {
			return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, imgName, base+"/dl/"+imgName)
		}
		s, custom := bundleDeck(t, "inmusic,jp21", assets, makeValidGen2Img(t, "SCLIVE4", "5.3.0.3"))
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		v := waitUpdate(t, s)
		if v.Error != "" || v.Current != "5.3.0.3" || v.Progress != 1 {
			t.Fatalf("fallback apply failed: %+v", v)
		}
		if quitCalls != 0 {
			t.Fatalf("quitEngineForUpdate called %d times on unhooked planck-remote-screen, want 0", quitCalls)
		}
		if _, err := os.Stat(engineQuitReasonPath); err == nil {
			t.Fatal("engine-quit-reason written despite unhooked planck-remote-screen")
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.3\n" {
			t.Errorf("version.txt = %q, want 5.3.0.3", b)
		}
	})

	t.Run("Gen-2 with LD_PRELOAD on planck-remote-screen takes UpdateFromUrl shortcut", func(t *testing.T) {
		tmp := t.TempDir()
		engineScriptPath = filepath.Join(tmp, "engine")
		planckRemoteScreenPath = filepath.Join(tmp, "planck-remote-screen")
		updateFileURLPath = filepath.Join(tmp, "update-file-url")
		engineQuitReasonPath = filepath.Join(tmp, "engine-quit-reason")
		os.WriteFile(planckRemoteScreenPath, []byte("#!/bin/sh\n"), 0o755)
		os.WriteFile(engineScriptPath, []byte(
			"#!/bin/sh\n\t/data/enginex/bundle/bin/engine-ui-guard launch /usr/Engine/Engine -d0\n\tenv LD_PRELOAD=\"/data/enginex/bundle/lib/libengine-ui-hook.so\" /usr/bin/planck-remote-screen --update \"$UPDATE_FILE\"\n",
		), 0o755)
		allowUpdateFromURL = func(string) bool { return true }
		quitCalls := 0
		quitEngineForUpdate = func() error { quitCalls++; return nil }

		imgName := "SCLIVE4-5.3.0.3-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img"
		assets := func(base string) string {
			return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, imgName, base+"/dl/"+imgName)
		}
		// Serve invalid bytes if downloaded: UpdateFromUrl shortcut must return before HTTP download.
		s, custom := bundleDeck(t, "inmusic,jp21", assets, []byte("not-downloaded"))
		u := getUpdate(s, "")
		getUpdate(s, "?apply=1")
		v := waitUpdate(t, s)
		if v.Error != "" || v.Progress != 1 || quitCalls != 1 {
			t.Fatalf("UpdateFromUrl shortcut failed: %+v (quitCalls=%d)", v, quitCalls)
		}
		if got, _ := os.ReadFile(updateFileURLPath); string(got) != u.DownloadURL+"\n" {
			t.Errorf("update-file-url = %q, want %q", got, u.DownloadURL+"\n")
		}
		if got, _ := os.ReadFile(engineQuitReasonPath); string(got) != "UpdateFromUrl\n" {
			t.Errorf("engine-quit-reason = %q, want UpdateFromUrl", got)
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.2\n" {
			t.Errorf("Go fallback unexpectedly modified version.txt: %q", b)
		}
	})

	t.Run("AZ04 without planck-remote-screen binary falls back to Go download and tryApplyGen2Image", func(t *testing.T) {
		tmp := t.TempDir()
		engineScriptPath = filepath.Join(tmp, "engine")
		planckRemoteScreenPath = filepath.Join(tmp, "missing-planck-remote-screen")
		updateFileURLPath = filepath.Join(tmp, "update-file-url")
		engineQuitReasonPath = filepath.Join(tmp, "engine-quit-reason")
		os.WriteFile(engineScriptPath, []byte(
			"#!/bin/sh\n\tenv LD_PRELOAD=\"/data/enginex/bundle/lib/libengine-ui-hook.so\" /usr/bin/planck-remote-screen --update \"$UPDATE_FILE\"\n",
		), 0o755)
		allowUpdateFromURL = func(string) bool { return true }
		quitCalls := 0
		quitEngineForUpdate = func() error { quitCalls++; return nil }

		imgName := "SYSTEMONE-5.3.0.3-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img"
		assets := func(base string) string {
			return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, imgName, base+"/dl/"+imgName)
		}
		s, custom := bundleDeck(t, "inmusic,rmz2", assets, makeValidGen2Img(t, "SYSTEMONE", "5.3.0.3"))
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		v := waitUpdate(t, s)
		if v.Error != "" || v.Current != "5.3.0.3" || quitCalls != 0 {
			t.Fatalf("AZ04 fallback failed: %+v (quitCalls=%d)", v, quitCalls)
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.3\n" {
			t.Errorf("version.txt = %q, want 5.3.0.3", b)
		}
	})

	t.Run("Gen-1 always uses UpdateFromUrl without requiring LD_PRELOAD on planck-remote-screen", func(t *testing.T) {
		tmp := t.TempDir()
		engineScriptPath = filepath.Join(tmp, "engine")
		planckRemoteScreenPath = filepath.Join(tmp, "missing-planck-remote-screen")
		updateFileURLPath = filepath.Join(tmp, "update-file-url")
		engineQuitReasonPath = filepath.Join(tmp, "engine-quit-reason")
		allowUpdateFromURL = func(string) bool { return true }
		quitCalls := 0
		quitEngineForUpdate = func() error { quitCalls++; return nil }

		imgName := "SC5000-5.3.0.3-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img"
		assets := func(base string) string {
			return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, imgName, base+"/dl/"+imgName)
		}
		s, _ := bundleDeck(t, "inmusic,jp07", assets, []byte("not-downloaded"))
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		v := waitUpdate(t, s)
		if v.Error != "" || v.Progress != 1 || quitCalls != 1 {
			t.Fatalf("Gen-1 UpdateFromUrl failed: %+v (quitCalls=%d)", v, quitCalls)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestUpdateRecordedReleasePicksAllModels verifies against the recorded real v5.3.0.4
// releases/latest payload that both 5.3.0.3's picker and the current checkRelease pick
// PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz for "inmusic,jp11s", the matching
// Data-Bundle.tar.gz for the other 7 second-gen boards (locked and unlocked), and the
// matching *-Update.img for all 8 first-gen boards, along with each asset's sha256 digest.
func TestUpdateRecordedReleasePicksAllModels(t *testing.T) {
	recorded, err := os.ReadFile("testdata/releases_latest_5.3.0.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var rel5303 struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(recorded, &rel5303); err != nil {
		t.Fatal(err)
	}
	// Exact 5.3.0.3 asset selection function for side-by-side regression proof.
	pick5303 := func(compatible string, unlocked bool) string {
		prefix := imagePrefix(strings.Split(compatible, "\x00"))
		find := func(suffix string) string {
			for _, a := range rel5303.Assets {
				if strings.HasPrefix(a.Name, prefix+"-") && strings.HasSuffix(a.Name, suffix) && strings.HasPrefix(a.URL, "https://") {
					return a.URL
				}
			}
			return ""
		}
		if dataBundleBoards[prefix] {
			var u string
			if unlocked {
				u = find("-Update.img")
			}
			if u == "" {
				u = find(bundleSuffix)
			}
			return u
		}
		return find("-Update.img")
	}

	wantDigests := map[string]string{}
	for _, a := range rel5303.Assets {
		wantDigests[a.Name] = a.Digest
	}

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/Cd1s/EngineX-OS/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(recorded)
	}))
	defer gh.Close()

	wantAssets := map[string]string{
		// All 8 second-generation boards (PRIME GO+ first, plus the other 7).
		"inmusic,jp11s": "PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
		"inmusic,jp21":  "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
		"inmusic,jp20":  "SCLIVE2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
		"inmusic,jc11s": "PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
		"inmusic,nh08s": "MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
		"inmusic,nh10":  "MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
		"inmusic,jp24":  "PRIME4G2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
		"inmusic,rmz2":  "SYSTEMONE-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
		// All 8 first-generation boards.
		"inmusic,jp07": "SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img",
		"inmusic,jp08": "SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img",
		"inmusic,jp11": "PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img",
		"inmusic,jp13": "SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img",
		"inmusic,jp14": "SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img",
		"inmusic,jc11": "PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img",
		"inmusic,jc16": "PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img",
		"inmusic,nh08": "MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img",
	}

	defer func(old string) { deviceCompatiblePath = old }(deviceCompatiblePath)
	for compat, wantName := range wantAssets {
		wantURL := "https://github.com/Cd1s/EngineX-OS/releases/download/v5.3.0.4/" + wantName
		wantDigest := wantDigests[wantName]
		if !strings.HasPrefix(wantDigest, "sha256:") {
			t.Fatalf("fixture %s missing sha256 digest: %q", wantName, wantDigest)
		}
		for _, mode := range []string{"default", "unlocked", "locked"} {
			if got5303 := pick5303(compat, mode == "unlocked"); got5303 != wantURL {
				t.Errorf("5.3.0.3 %s (%s): got %q, want %q", compat, mode, got5303, wantURL)
			}
			state := t.TempDir()
			custom := filepath.Join(state, "enginex")
			os.MkdirAll(custom, 0o700)
			os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.3\n"), 0o644)
			if mode == "unlocked" {
				os.WriteFile(filepath.Join(custom, "gen2-unlocked"), []byte("1\n"), 0o644)
			} else if mode == "locked" {
				os.WriteFile(filepath.Join(custom, "gen2-locked"), []byte("1\n"), 0o644)
			}
			cf := filepath.Join(state, "compatible")
			os.WriteFile(cf, []byte(compat+"\x00"), 0o644)
			deviceCompatiblePath = cf
			s := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, GitHubURL: gh.URL,
				VersionFile: filepath.Join(custom, "version.txt")}, nil)
			w := httptest.NewRecorder()
			s.update(w, httptest.NewRequest("GET", "/v1/sc5000/update", nil))
			var got struct {
				Current         string `json:"currentVersion"`
				Latest          string `json:"latestVersion"`
				UpdateAvailable bool   `json:"updateAvailable"`
				DownloadURL     string `json:"downloadUrl"`
				Digest          string `json:"digest"`
				Error           string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Current != "5.3.0.3" || got.Latest != "v5.3.0.4" || !got.UpdateAvailable || got.Error != "" ||
				got.DownloadURL != wantURL || got.Digest != wantDigest {
				t.Errorf("new %s (%s): got %+v, want url=%q digest=%q", compat, mode, got, wantURL, wantDigest)
			}
		}
	}
}

func TestUpdateDiagnosableDownloadErrorsAndJournalLog(t *testing.T) {
	const cdnHost = "release-assets.githubusercontent.com"
	tlsCDN := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tlsCDN.Close()

	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closedLn.Addr().String()
	closedLn.Close()

	cases := []struct {
		name     string
		wantHost string
		wantKind string
		rt       func(req *http.Request) (*http.Response, error)
	}{
		{
			name:     "DNS lookup on redirected CDN",
			wantHost: cdnHost,
			wantKind: "DNS lookup",
			rt: func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "github.com" {
					h := make(http.Header)
					h.Set("Location", "https://"+cdnHost+"/asset.tar.gz?jwt=secret-token&X-Amz-Signature=abcdef&rscd=attachment%3B")
					return &http.Response{StatusCode: http.StatusFound, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: cdnHost}}
			},
		},
		{
			name:     "TLS untrusted certificate",
			wantHost: "127.0.0.1",
			wantKind: "TLS",
			rt: func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "github.com" {
					h := make(http.Header)
					h.Set("Location", tlsCDN.URL+"/asset?jwt=secret")
					return &http.Response{StatusCode: http.StatusFound, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				return (&http.Transport{}).RoundTrip(req)
			},
		},
		{
			name:     "timeout on CDN",
			wantHost: cdnHost,
			wantKind: "timeout",
			rt: func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "github.com" {
					h := make(http.Header)
					h.Set("Location", "https://"+cdnHost+"/asset")
					return &http.Response{StatusCode: http.StatusFound, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				return nil, context.DeadlineExceeded
			},
		},
		{
			name:     "connection refused",
			wantHost: "127.0.0.1",
			wantKind: "connection refused",
			rt: func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "github.com" {
					h := make(http.Header)
					h.Set("Location", "http://"+closedAddr+"/asset")
					return &http.Response{StatusCode: http.StatusFound, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				return (&http.Transport{}).RoundTrip(req)
			},
		},
		{
			name:     "connection reset on CDN",
			wantHost: cdnHost,
			wantKind: "connection reset",
			rt: func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "github.com" {
					h := make(http.Header)
					h.Set("Location", "https://"+cdnHost+"/asset")
					return &http.Response{StatusCode: http.StatusFound, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				return nil, &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
			},
		},
		{
			name:     "HTTP 403 on CDN",
			wantHost: cdnHost,
			wantKind: "HTTP 403",
			rt: func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "github.com" {
					h := make(http.Header)
					h.Set("Location", "https://"+cdnHost+"/asset?sig=12345")
					return &http.Response{StatusCode: http.StatusFound, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: io.NopCloser(strings.NewReader("blocked")), Request: req}, nil
			},
		},
	}

	defer func(old string) { deviceCompatiblePath = old }(deviceCompatiblePath)
	defer func(old []time.Duration) { otaRetryBackoffs = old }(otaRetryBackoffs)
	otaRetryBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	origLog := log.Writer()
	defer log.SetOutput(origLog)

	for _, deck := range []struct {
		label, compat, asset string
	}{
		{"gen2 PRIMEGOPLUS", "inmusic,jp11s", "PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz"},
		{"gen1 SC5000 background", "inmusic,jp07", "SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img"},
	} {
		for _, tc := range cases {
			t.Run(deck.label+"/"+tc.name, func(t *testing.T) {
				var logBuf bytes.Buffer
				log.SetOutput(&logBuf)

				hc := &http.Client{
					Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						if req.URL.Path == "/repos/Cd1s/EngineX-OS/releases/latest" {
							body := fmt.Sprintf(`{"tag_name":"v5.3.0.4","html_url":"https://github.com/Cd1s/EngineX-OS/releases/tag/v5.3.0.4","assets":[{"name":%q,"browser_download_url":%q}]}`,
								deck.asset, "https://github.com/Cd1s/EngineX-OS/releases/download/v5.3.0.4/"+deck.asset)
							return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
						}
						return tc.rt(req)
					}),
				}
				state := t.TempDir()
				custom := filepath.Join(state, "enginex")
				os.MkdirAll(custom, 0o700)
				os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.3\n"), 0o644)
				cf := filepath.Join(state, "compatible")
				os.WriteFile(cf, []byte(deck.compat+"\x00"), 0o644)
				deviceCompatiblePath = cf

				s := New(Config{AccessToken: "K7M9-P4X2", UIDir: t.TempDir(), StateDir: state,
					GitHubURL: "https://api.github.com", HTTP: hc, VersionFile: filepath.Join(custom, "version.txt")}, nil)
				getUpdate(s, "")
				getUpdate(s, "?apply=1")
				v := waitUpdate(t, s)

				if v.Error == "" || v.Error == "update download failed" {
					t.Fatalf("undifferentiated error: %q", v.Error)
				}
				if !strings.Contains(v.Error, tc.wantHost) || !strings.Contains(v.Error, tc.wantKind) {
					t.Fatalf("got error %q, want host %q and kind %q", v.Error, tc.wantHost, tc.wantKind)
				}
				if len(v.Error) > 80 {
					t.Errorf("error message too long (%d chars): %q", len(v.Error), v.Error)
				}

				// Web console /api/update shows the exact same diagnosable error.
				cw := httptest.NewRecorder()
				cr := httptest.NewRequest("GET", "/api/update", nil)
				cr.Header.Set("Authorization", "Bearer K7M9-P4X2")
				s.ServeHTTP(cw, cr)
				var cv updateStatus
				if err := json.Unmarshal(cw.Body.Bytes(), &cv); err != nil || cw.Code != 200 || cv.Error != v.Error {
					t.Fatalf("/api/update status=%d got=%+v, want error=%q", cw.Code, cv, v.Error)
				}

				// Journal received the full error without signed query parameters, and survives redactLog.
				logged := logBuf.String()
				if logged == "" {
					t.Fatal("expected full error logged to journal, got empty log")
				}
				if strings.Contains(logged, "secret-token") || strings.Contains(logged, "X-Amz-Signature") {
					t.Errorf("log leaked signed redirect query parameters: %s", logged)
				}
				for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
					if strings.Contains(line, "ota") {
						if red := redactLog(line, "K7M9-P4X2"); strings.Contains(red, "REDACTED: credential-related") {
							t.Errorf("journal line wiped by redactLog: %q -> %q", line, red)
						}
					}
				}
			})
		}
	}
}

func TestUpdateDownloadRetriesTransientAndSkips4xx(t *testing.T) {
	if len(otaRetryBackoffs) != 3 {
		t.Fatalf("otaRetryBackoffs len = %d, want 3", len(otaRetryBackoffs))
	}
	for i, d := range otaRetryBackoffs {
		if d <= 0 || d > 5*time.Second {
			t.Fatalf("otaRetryBackoffs[%d] = %v, want short positive backoff", i, d)
		}
	}
	defer func(old []time.Duration) { otaRetryBackoffs = old }(otaRetryBackoffs)
	otaRetryBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func(old string) { deviceCompatiblePath = old }(deviceCompatiblePath)
	bundle := testBundle(t, "PRIMEGOPLUS", "5.3.0.4", false)
	sum := sha256.Sum256(bundle)
	name := "PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz"

	runCase := func(t *testing.T, handler func(attempt int, req *http.Request) (*http.Response, error)) (updateStatus, int32) {
		t.Helper()
		var attempts atomic.Int32
		hc := &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/repos/Cd1s/EngineX-OS/releases/latest" {
					body := fmt.Sprintf(`{"tag_name":"v5.3.0.4","html_url":"https://x/r","assets":[{"name":%q,"digest":"sha256:%x","browser_download_url":%q}]}`,
						name, sum, "https://release-assets.githubusercontent.com/"+name)
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				}
				n := int(attempts.Add(1))
				return handler(n, req)
			}),
		}
		state := t.TempDir()
		custom := filepath.Join(state, "enginex")
		os.MkdirAll(custom, 0o700)
		os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.3\n"), 0o644)
		cf := filepath.Join(state, "compatible")
		os.WriteFile(cf, []byte("inmusic,jp11s\x00"), 0o644)
		deviceCompatiblePath = cf
		s := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state,
			GitHubURL: "https://api.github.com", HTTP: hc, VersionFile: filepath.Join(custom, "version.txt")}, nil)
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		return waitUpdate(t, s), attempts.Load()
	}

	t.Run("retries 503 three times and succeeds on 4th attempt", func(t *testing.T) {
		v, n := runCase(t, func(attempt int, req *http.Request) (*http.Response, error) {
			if attempt <= 3 {
				return &http.Response{StatusCode: 503, Status: "503 Service Unavailable", Body: io.NopCloser(strings.NewReader("busy")), Request: req}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(bundle)), ContentLength: int64(len(bundle)), Request: req}, nil
		})
		if n != 4 || v.Error != "" || v.Current != "5.3.0.4" {
			t.Fatalf("attempts=%d status=%+v", n, v)
		}
	})

	t.Run("retries connection reset and timeout then fails after 3 retries", func(t *testing.T) {
		v, n := runCase(t, func(attempt int, req *http.Request) (*http.Response, error) {
			if attempt < 3 {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNRESET)}
			}
			return nil, context.DeadlineExceeded
		})
		if n != 4 || !strings.Contains(v.Error, "release-assets.githubusercontent.com") || !strings.Contains(v.Error, "timeout") {
			t.Fatalf("attempts=%d status=%+v", n, v)
		}
	})

	t.Run("does not retry HTTP 404 or HTTP 403", func(t *testing.T) {
		for _, code := range []int{403, 404} {
			v, n := runCase(t, func(attempt int, req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: code, Status: fmt.Sprintf("%d status", code), Body: io.NopCloser(strings.NewReader("nope")), Request: req}, nil
			})
			if n != 1 || !strings.Contains(v.Error, fmt.Sprintf("HTTP %d", code)) {
				t.Fatalf("code=%d: attempts=%d (want 1), status=%+v", code, n, v)
			}
		}
	})
}

func TestUpdateVerifiesReleaseAssetDigestBeforeInstall(t *testing.T) {
	name := "PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz"
	goodBundle := testBundle(t, "PRIMEGOPLUS", "5.3.0.4", false)
	goodSum := sha256.Sum256(goodBundle)

	t.Run("tampered byte in downloaded bundle is refused", func(t *testing.T) {
		tampered := append([]byte(nil), goodBundle...)
		tampered[len(tampered)-1] ^= 0x01
		assets := func(base string) string {
			return fmt.Sprintf(`{"name":%q,"digest":"sha256:%x","browser_download_url":%q}`, name, goodSum, base+"/dl/"+name)
		}
		s, custom := bundleDeck(t, "inmusic,jp11s", assets, tampered)
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		v := waitUpdate(t, s)
		if !strings.Contains(v.Error, "SHA-256") {
			t.Fatalf("expected SHA-256 rejection, got %+v", v)
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.2\n" {
			t.Errorf("version.txt modified to %q", b)
		}
	})

	t.Run("forged valid archive with wrong outer digest never runs install.sh", func(t *testing.T) {
		canaryDir := t.TempDir()
		canaryFile := filepath.Join(canaryDir, "ran-install")
		top := "PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle"
		script := []byte("#!/bin/sh\necho pwned > " + canaryFile + "\nexit 0\n")
		h := sha256.Sum256(script)
		sums := []byte(fmt.Sprintf("%x  install.sh\n", h))
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.WriteHeader(&tar.Header{Name: top + "/install.sh", Mode: 0o755, Size: int64(len(script))})
		tw.Write(script)
		tw.WriteHeader(&tar.Header{Name: top + "/SHA256SUMS", Mode: 0o644, Size: int64(len(sums))})
		tw.Write(sums)
		tw.Close()
		gz.Close()

		assets := func(base string) string {
			return fmt.Sprintf(`{"name":%q,"digest":"sha256:%x","browser_download_url":%q}`, name, goodSum, base+"/dl/"+name)
		}
		s, custom := bundleDeck(t, "inmusic,jp11s", assets, buf.Bytes())
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		v := waitUpdate(t, s)
		if !strings.Contains(v.Error, "SHA-256") {
			t.Fatalf("forged bundle accepted: %+v", v)
		}
		if _, err := os.Stat(canaryFile); err == nil {
			t.Fatal("install.sh executed despite release digest mismatch")
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.2\n" {
			t.Errorf("version.txt modified to %q", b)
		}
	})

	t.Run("matching digest installs cleanly", func(t *testing.T) {
		assets := func(base string) string {
			return fmt.Sprintf(`{"name":%q,"digest":"sha256:%x","browser_download_url":%q}`, name, goodSum, base+"/dl/"+name)
		}
		s, _ := bundleDeck(t, "inmusic,jp11s", assets, goodBundle)
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		v := waitUpdate(t, s)
		if v.Error != "" || v.Current != "5.3.0.4" || v.Progress != 1 {
			t.Fatalf("valid digest bundle failed: %+v", v)
		}
	})
}

func TestWebConsoleManualBundleUpload(t *testing.T) {
	defer func(old string) { deviceCompatiblePath = old }(deviceCompatiblePath)

	newDeck := func(t *testing.T, compat string) (*Server, string) {
		t.Helper()
		state := t.TempDir()
		custom := filepath.Join(state, "enginex")
		os.MkdirAll(custom, 0o700)
		os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.3\n"), 0o644)
		cf := filepath.Join(state, "compatible")
		os.WriteFile(cf, []byte(compat+"\x00"), 0o644)
		deviceCompatiblePath = cf
		return New(Config{AccessToken: "K7M9-P4X2", UIDir: t.TempDir(), StateDir: state,
			VersionFile: filepath.Join(custom, "version.txt")}, nil), custom
	}

	goodName := "PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz"
	goodBundle := testBundle(t, "PRIMEGOPLUS", "5.3.0.4", false)

	t.Run("requires console access code", func(t *testing.T) {
		s, _ := newDeck(t, "inmusic,jp11s")
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/update/upload?name="+goodName, bytes.NewReader(goodBundle))
		req.Header.Set("Content-Type", "application/gzip")
		s.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated upload got %d, want 401", w.Code)
		}
	})

	t.Run("refuses first-gen deck", func(t *testing.T) {
		s, _ := newDeck(t, "inmusic,jp07")
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/update/upload?name=SC5000-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz", bytes.NewReader(goodBundle))
		req.Header.Set("Authorization", "Bearer K7M9-P4X2")
		req.Header.Set("Content-Type", "application/gzip")
		s.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("first-gen deck upload got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("refuses wrong file-name prefix and renamed wrong-model bundle", func(t *testing.T) {
		s, custom := newDeck(t, "inmusic,jp11s")
		for _, badName := range []string{
			"SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
			"PRIMEGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
			"PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz",
			"random.tar.gz",
		} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/api/update/upload?name="+badName, bytes.NewReader(goodBundle))
			req.Header.Set("Authorization", "Bearer K7M9-P4X2")
			req.Header.Set("Content-Type", "application/gzip")
			s.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: got %d, want 400 (%s)", badName, w.Code, w.Body.String())
			}
		}

		scliveBundle := testBundle(t, "SCLIVE4", "5.3.0.4", false)
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/update/upload?name="+goodName, bytes.NewReader(scliveBundle))
		req.Header.Set("Authorization", "Bearer K7M9-P4X2")
		req.Header.Set("Content-Type", "application/gzip")
		s.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("renamed wrong-model bundle got %d: %s", w.Code, w.Body.String())
		}

		var markerBuf bytes.Buffer
		mgz := gzip.NewWriter(&markerBuf)
		mtw := tar.NewWriter(mgz)
		mtop := "PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle"
		mscript := []byte("#!/bin/sh\nexit 0\n")
		mmodel := []byte("PRIME4G2\n")
		msums := []byte(fmt.Sprintf("%x  install.sh\n%x  model.txt\n", sha256.Sum256(mscript), sha256.Sum256(mmodel)))
		mtw.WriteHeader(&tar.Header{Name: mtop + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		mtw.WriteHeader(&tar.Header{Name: mtop + "/install.sh", Mode: 0o755, Size: int64(len(mscript))})
		mtw.Write(mscript)
		mtw.WriteHeader(&tar.Header{Name: mtop + "/model.txt", Mode: 0o644, Size: int64(len(mmodel))})
		mtw.Write(mmodel)
		mtw.WriteHeader(&tar.Header{Name: mtop + "/SHA256SUMS", Mode: 0o644, Size: int64(len(msums))})
		mtw.Write(msums)
		mtw.Close()
		mgz.Close()
		w2 := httptest.NewRecorder()
		req2 := httptest.NewRequest("POST", "/api/update/upload?name="+goodName, bytes.NewReader(markerBuf.Bytes()))
		req2.Header.Set("Authorization", "Bearer K7M9-P4X2")
		req2.Header.Set("Content-Type", "application/gzip")
		s.ServeHTTP(w2, req2)
		if w2.Code != http.StatusBadRequest {
			t.Fatalf("mismatched model.txt bundle got %d: %s", w2.Code, w2.Body.String())
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.3\n" {
			t.Errorf("version.txt changed to %q", b)
		}
	})

	t.Run("caps upload at bundleMaxDown", func(t *testing.T) {
		s, custom := newDeck(t, "inmusic,jp11s")
		w0 := httptest.NewRecorder()
		req0 := httptest.NewRequest("POST", "/api/update/upload?name="+goodName, strings.NewReader("x"))
		req0.ContentLength = bundleMaxDown + 1
		req0.Header.Set("Authorization", "Bearer K7M9-P4X2")
		req0.Header.Set("Content-Type", "application/gzip")
		s.ServeHTTP(w0, req0)
		if w0.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("Content-Length > bundleMaxDown got %d, want 413 (%s)", w0.Code, w0.Body.String())
		}

		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			gz, _ := gzip.NewWriterLevel(pw, gzip.NoCompression)
			tw := tar.NewWriter(gz)
			top := "PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle"
			_ = tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
			bigSize := int64(bundleMaxDown + (1 << 20))
			_ = tw.WriteHeader(&tar.Header{Name: top + "/bin/huge", Mode: 0o644, Size: bigSize})
			_, _ = io.Copy(tw, io.LimitReader(zeroReader{}, bigSize))
			_ = tw.Close()
			_ = gz.Close()
		}()
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/update/upload?name="+goodName, pr)
		req.ContentLength = -1
		req.Header.Set("Authorization", "Bearer K7M9-P4X2")
		req.Header.Set("Content-Type", "application/gzip")
		s.ServeHTTP(w, req)
		_ = pr.Close()
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized streamed upload got %d, want 413 (%s)", w.Code, w.Body.String())
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.3\n" {
			t.Errorf("version.txt changed to %q", b)
		}
	})

	t.Run("streams valid bundle via multipart and reports progress", func(t *testing.T) {
		s, custom := newDeck(t, "inmusic,jp11s")
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, err := mw.CreateFormFile("file", goodName)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(goodBundle)
		mw.Close()

		var sawUpdating atomic.Bool
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			data := body.Bytes()
			half := len(data) / 2
			pw.Write(data[:half])
			for i := 0; i < 40; i++ {
				cw := httptest.NewRecorder()
				cr := httptest.NewRequest("GET", "/api/update", nil)
				cr.Header.Set("Authorization", "Bearer K7M9-P4X2")
				s.ServeHTTP(cw, cr)
				var st updateStatus
				if json.Unmarshal(cw.Body.Bytes(), &st) == nil && st.Updating && st.Progress > 0.05 {
					sawUpdating.Store(true)
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			pw.Write(data[half:])
		}()

		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/update/upload", pr)
		req.ContentLength = int64(body.Len())
		req.Header.Set("Authorization", "Bearer K7M9-P4X2")
		req.Header.Set("Content-Type", mw.FormDataContentType())
		s.ServeHTTP(w, req)
		_ = pr.Close()
		if w.Code != http.StatusOK {
			t.Fatalf("valid multipart upload failed: %d %s", w.Code, w.Body.String())
		}
		if !sawUpdating.Load() {
			t.Error("expected live progress > 0.05 via /api/update while streaming upload")
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.4\n" {
			t.Fatalf("version.txt = %q, want 5.3.0.4", b)
		}
	})

	t.Run("streams valid bundle via raw gzip and exposes console markup", func(t *testing.T) {
		s, custom := newDeck(t, "inmusic,jp11s")
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/update/upload?name="+goodName, bytes.NewReader(goodBundle))
		req.ContentLength = int64(len(goodBundle))
		req.Header.Set("Authorization", "Bearer K7M9-P4X2")
		req.Header.Set("Content-Type", "application/gzip")
		s.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("raw gzip upload failed: %d %s", w.Code, w.Body.String())
		}
		if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.4\n" {
			t.Fatalf("version.txt = %q, want 5.3.0.4", b)
		}
		hb, err := assets.ReadFile("index.html")
		if err != nil {
			t.Fatal(err)
		}
		page := string(hb)
		for _, snippet := range []string{`id="updCard"`, `id="updErr"`, `id="updDrop"`, `/api/update/upload?name=`} {
			if !strings.Contains(page, snippet) {
				t.Errorf("index.html missing %s", snippet)
			}
		}

		// Real TCP socket check: a large wrong-model upload receives clean HTTP 400 JSON without TCP RST.
		srv := httptest.NewServer(s)
		defer srv.Close()
		largePayload := bytes.Repeat([]byte("A"), 4<<20)
		tcpReq, err := http.NewRequest("POST", srv.URL+"/api/update/upload?name=SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz", bytes.NewReader(largePayload))
		if err != nil {
			t.Fatal(err)
		}
		tcpReq.Header.Set("Authorization", "Bearer K7M9-P4X2")
		tcpReq.Header.Set("Content-Type", "application/gzip")
		resp, err := srv.Client().Do(tcpReq)
		if err != nil {
			t.Fatalf("real TCP wrong-model upload reset instead of HTTP 400: %v", err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(respBody), "SCLIVE4") {
			t.Fatalf("real TCP wrong-model upload: status=%d body=%s", resp.StatusCode, respBody)
		}
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
