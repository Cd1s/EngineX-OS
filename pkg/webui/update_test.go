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
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/releasesig"
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
	script := bytes.ReplaceAll(m[1], []byte("@MIGRATE_IF_DEFAULT@"), []byte(":"))
	// @SWAP_UNDO@ is the real rollback of an interrupted install (BH-G2-4).
	undo := regexp.MustCompile(`(?s)\nSWAP_UNDO = r"""(.*?)"""`).FindSubmatch(src)
	if undo == nil {
		t.Fatal("SWAP_UNDO not found in build-data-bundle.py")
	}
	return bytes.ReplaceAll(script, []byte("@SWAP_UNDO@"), undo[1])
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
	return bundleDeckWith(t, compatible, assets, bundle, releaseFault{})
}

// bundleDeckWith is a deck whose GitHub serves a release signed with the test release key
// (SHA256SUMS.txt and .sig beside the update file, #239), unless fault says otherwise.
func bundleDeckWith(t *testing.T, compatible string, assets func(base string) string, bundle []byte, fault releaseFault) (*Server, string) {
	var base string
	gh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/Cd1s/EngineX-OS/releases/latest":
			fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://x/r","assets":[%s]}`, fault.apiTag(), assets(base))
		case strings.HasSuffix(r.URL.Path, ".tar.gz") || strings.HasSuffix(r.URL.Path, "-Update.img"):
			if fault.serve != nil {
				fault.serve(w, r, fault.served(bundle))
				return
			}
			w.Write(fault.served(bundle))
		case strings.HasSuffix(r.URL.Path, "/"+releasesig.SumsAssetName) && !fault.unsigned:
			sums, _ := fault.signed(assetNames(assets(base)), bundle)
			w.Write(sums)
		case strings.HasSuffix(r.URL.Path, "/"+releasesig.SigAssetName) && !fault.unsigned:
			_, sig := fault.signed(assetNames(assets(base)), bundle)
			w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	base = gh.URL
	state := t.TempDir()
	custom := filepath.Join(state, "sc5000-custom")
	os.MkdirAll(custom, 0o700)
	os.WriteFile(filepath.Join(custom, "version.txt"), []byte(fault.installedVersion()+"\n"), 0o644)
	if fault.installTag != "" { // a bundle install carries the release tag it came from
		os.MkdirAll(filepath.Join(custom, "bundle"), 0o700)
		os.WriteFile(filepath.Join(custom, "bundle", releaseTagName), []byte(fault.installTag+"\n"), 0o644)
	}
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

// #239: the deck no longer hands the update URL to Denon's updater (UpdateFromUrl): that updater
// downloads the file on its own and checks only the image's own integrity fields, so the release
// signature could not be tied to what is flashed. Every board downloads the file itself, checks
// it against the signed SHA256SUMS.txt and installs it: the second generation unpacks the bundle,
// the first runs az01-update on the staged image.
func TestApplyOnlineUpdateInstallsTheVerifiedFileItself(t *testing.T) {
	makeValidGen2Img := func(t *testing.T, prefix, version string) []byte {
		t.Helper()
		bundle := testBundle(t, prefix, version, false)
		sum := sha256.Sum256(bundle)
		var head [gen2ImgHeader]byte
		copy(head[:], fmt.Sprintf("%s%x\n%s\n%s\n", gen2ImgMagic, sum, prefix, version))
		return append(head[:], bundle...)
	}
	for _, c := range []struct{ name, compatible, imgName, prefix string }{
		{"armhf second generation", "inmusic,jp21", "SCLIVE4-5.3.0.3-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img", "SCLIVE4"},
		{"AZ04 second generation", "inmusic,rmz2", "SYSTEMONE-5.3.0.3-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img", "SYSTEMONE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			assets := func(base string) string {
				return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, c.imgName, base+"/dl/"+c.imgName)
			}
			s, custom := bundleDeck(t, c.compatible, assets, makeValidGen2Img(t, c.prefix, "5.3.0.3"))
			getUpdate(s, "")
			getUpdate(s, "?apply=1")
			v := waitUpdate(t, s)
			if v.Error != "" || v.Current != "5.3.0.3" || v.Progress != 1 {
				t.Fatalf("apply failed: %+v", v)
			}
			if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.3\n" {
				t.Errorf("version.txt = %q, want 5.3.0.3", b)
			}
		})
	}

	t.Run("first generation downloads, verifies and runs az01-update --file", func(t *testing.T) {
		img := az01Image()
		imgName := "SC5000-5.3.0.3-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img"
		assets := func(base string) string {
			return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, imgName, base+"/dl/"+imgName)
		}
		s, custom := bundleDeck(t, "inmusic,jp07", assets, img)
		calls := fakeAz01Update(t)
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		if v := waitUpdate(t, s); v.Error != noRestart {
			t.Fatalf("Gen-1 apply failed: %+v", v)
		}
		staged := filepath.Join(custom, "az01-update.img")
		if got := readCalls(t, calls); got != "--file "+staged+"\n" {
			t.Fatalf("az01-update was run as %q, want --file %s", got, staged)
		}
		if !hasDigest(staged, fmt.Sprintf("%x", sha256.Sum256(img))) {
			t.Error("the staged image is not the signed one")
		}
	})
}

// A same-version re-release keeps version.txt and is tagged v<version>-rN. Decks on the
// first build of that version, and on older versions, are offered it; a deck that carries
// its tag is offered only what comes after. The first generation reads the tag from the
// image, the second from the installed bundle (data bundle or bundle-carrying *-Update.img).
func TestSameVersionReRelease(t *testing.T) {
	var tag string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://x/r","assets":[
			{"name":"SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img","browser_download_url":"https://example.invalid/gen1.img"},
			{"name":"SCLIVE4-5.3.0.4-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img","browser_download_url":"https://example.invalid/gen2.img"},
			{"name":"SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz","browser_download_url":"https://example.invalid/gen2.tar.gz"}]}`, tag)
	}))
	defer gh.Close()
	defer func(c, i string) { deviceCompatiblePath, imageReleaseTagFile = c, i }(deviceCompatiblePath, imageReleaseTagFile)
	const gen1, gen2 = "inmusic,jp07", "inmusic,jp21"
	for _, c := range []struct {
		name, compatible, version, imageTag, bundleTag string
		locked                                         bool
		release                                        string
		want                                           bool
		asset                                          string
	}{
		{"gen1 5.3.0.3", gen1, "5.3.0.3", "", "", false, "v5.3.0.4-r2", true, "gen1.img"},
		{"gen1 first 5.3.0.4 build", gen1, "5.3.0.4", "", "", false, "v5.3.0.4-r2", true, "gen1.img"},
		{"gen1 r2 is not offered r2", gen1, "5.3.0.4", "v5.3.0.4-r2", "", false, "v5.3.0.4-r2", false, ""},
		{"gen1 r2 is not offered the first build", gen1, "5.3.0.4", "v5.3.0.4-r2", "", false, "v5.3.0.4", false, ""},
		{"gen1 r2 is offered r3", gen1, "5.3.0.4", "v5.3.0.4-r2", "", false, "v5.3.0.4-r3", true, "gen1.img"},
		{"gen1 r2 is offered 5.3.0.5", gen1, "5.3.0.4", "v5.3.0.4-r2", "", false, "v5.3.0.5", true, "gen1.img"},
		{"gen1 plain tag of a new version", gen1, "5.3.0.5", "v5.3.0.5", "", false, "v5.3.0.5", false, ""},
		{"gen2 5.3.0.3", gen2, "5.3.0.3", "", "", false, "v5.3.0.4-r2", true, "gen2.img"},
		{"gen2 first 5.3.0.4 build", gen2, "5.3.0.4", "", "", false, "v5.3.0.4-r2", true, "gen2.img"},
		{"gen2 r2 is not offered r2", gen2, "5.3.0.4", "", "v5.3.0.4-r2", false, "v5.3.0.4-r2", false, ""},
		{"gen2 r2 is offered r3", gen2, "5.3.0.4", "", "v5.3.0.4-r2", false, "v5.3.0.4-r3", true, "gen2.img"},
		{"gen2 r2 is offered 5.3.0.5", gen2, "5.3.0.4", "", "v5.3.0.4-r2", false, "v5.3.0.5", true, "gen2.img"},
		{"gen2 locked r2 is not offered r2", gen2, "5.3.0.4", "", "v5.3.0.4-r2", true, "v5.3.0.4-r2", false, ""},
		{"gen2 locked r2 takes the r3 bundle", gen2, "5.3.0.4", "", "v5.3.0.4-r2", true, "v5.3.0.4-r3", true, "gen2.tar.gz"},
		{"a tag left by another version is ignored", gen2, "5.3.0.3", "", "v5.3.0.4-r2", false, "v5.3.0.4-r2", true, "gen2.img"},
	} {
		dir := t.TempDir()
		custom := filepath.Join(dir, "enginex")
		os.MkdirAll(filepath.Join(custom, "bundle"), 0o700)
		os.WriteFile(filepath.Join(custom, "version.txt"), []byte(c.version+"\n"), 0o644)
		deviceCompatiblePath = filepath.Join(dir, "compatible")
		os.WriteFile(deviceCompatiblePath, []byte(c.compatible+"\x00"), 0o644)
		imageReleaseTagFile = filepath.Join(dir, "image-release-tag")
		if c.imageTag != "" {
			os.WriteFile(imageReleaseTagFile, []byte(c.imageTag+"\n"), 0o644)
		}
		if c.bundleTag != "" {
			os.WriteFile(filepath.Join(custom, "bundle", releaseTagName), []byte(c.bundleTag+"\n"), 0o644)
		}
		if c.locked {
			os.WriteFile(filepath.Join(custom, "gen2-locked"), nil, 0o644)
		}
		tag = c.release
		s := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: dir, GitHubURL: gh.URL, VersionFile: filepath.Join(custom, "version.txt")}, nil)
		v := getUpdate(s, "")
		if v.Error != "" || v.Current != c.version || v.Latest != c.release || v.UpdateAvailable != c.want ||
			(c.want && v.DownloadURL != "https://example.invalid/"+c.asset) {
			t.Errorf("%s: %+v", c.name, v)
		}
	}
}

// #218: end-to-end 2nd-gen PRIME 4 G2 (AZ04, arm64) re-release flow with real deck path
// resolution (Config.VersionFile == ""). Covers plain 5.3.0.4 -> v5.3.0.4-r3 apply via both
// *-Update.img and *-Data-Bundle.tar.gz, restart into a fresh Server, v5.3.0.5 still offered,
// and missing/unmigrated /data/sc5000-custom alongside /data/enginex.
func TestGen2EndToEndReReleaseApplyAndCustomDirResolution(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	buildRealGen2 := func(version, relTag string) (imgBytes, tarBytes []byte, imgName, tarName string) {
		t.Helper()
		dir := t.TempDir()
		elf := make([]byte, 64)
		copy(elf, "\x7fELF\x02\x01\x01")
		elf[16], elf[18] = 2, 183 // ET_EXEC, EM_AARCH64
		suite := filepath.Join(dir, "sc5000-suite")
		hook := filepath.Join(dir, "libengine-ui-hook.so")
		os.WriteFile(suite, append(elf, []byte("suite-"+relTag)...), 0o755)
		os.WriteFile(hook, append(elf, []byte("hook-"+relTag)...), 0o755)
		ui := filepath.Join(dir, "packs.tar")
		os.WriteFile(ui, tarOf(t, map[string]string{"packs/en/Main.qml": "Item {}\n"}), 0o644)
		outDir := filepath.Join(dir, "out")
		script := filepath.Join("..", "..", "tools", "az01-packer", "build-data-bundle.py")
		args := []string{"-I", script, version, "--prefix", "PRIME4G2", "--suite", suite, "--hook", hook, "--ui-packs", ui, "--out", outDir}
		if relTag != "" {
			args = append(args, "--release-tag", relTag)
		}
		if out, err := exec.Command("python3", args...).CombinedOutput(); err != nil {
			t.Fatalf("build-data-bundle: %v\n%s", err, out)
		}
		imgs, _ := filepath.Glob(filepath.Join(outDir, "PRIME4G2-*-Update.img"))
		tars, _ := filepath.Glob(filepath.Join(outDir, "PRIME4G2-*"+bundleSuffix))
		if len(imgs) != 1 || len(tars) != 1 {
			t.Fatalf("unexpected outputs: imgs=%v tars=%v", imgs, tars)
		}
		ib, _ := os.ReadFile(imgs[0])
		tb, _ := os.ReadFile(tars[0])
		return ib, tb, filepath.Base(imgs[0]), filepath.Base(tars[0])
	}

	_, plainTar, _, _ := buildRealGen2("5.3.0.4", "v5.3.0.4")
	r3Img, r3Tar, r3ImgName, r3TarName := buildRealGen2("5.3.0.4", "v5.3.0.4-r3")

	defer func(vf, ivf, bcd, lbcd, dcp, irtf string) {
		versionFile, imageVersionFile = vf, ivf
		bundleCustomDir, legacyBundleCustomDir = bcd, lbcd
		deviceCompatiblePath, imageReleaseTagFile = dcp, irtf
	}(versionFile, imageVersionFile, bundleCustomDir, legacyBundleCustomDir, deviceCompatiblePath, imageReleaseTagFile)

	for _, viaImg := range []bool{true, false} {
		mode := "update-img"
		if !viaImg {
			mode = "data-bundle"
		}
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			bundleCustomDir = filepath.Join(root, "enginex")
			legacyBundleCustomDir = filepath.Join(root, "sc5000-custom")
			versionFile = filepath.Join(legacyBundleCustomDir, "version.txt")
			imageVersionFile = filepath.Join(root, "no-etc-version")
			imageReleaseTagFile = filepath.Join(root, "no-etc-release-tag")
			deviceCompatiblePath = filepath.Join(root, "compatible")
			os.WriteFile(deviceCompatiblePath, []byte("inmusic,jp24\x00inmusic,az04\x00"), 0o644)

			// Seed initial plain 5.3.0.4 install under /data/enginex + /data/sc5000-custom -> enginex.
			unpackDir := filepath.Join(root, "seed")
			top, err := unpackBundleReader(bytes.NewReader(plainTar), unpackDir)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", filepath.Join(top, "install.sh"))
			cmd.Env = append(os.Environ(), "SC5000_CUSTOM="+bundleCustomDir, "SC5000_ALSA_OVERLAY="+filepath.Join(root, "alsa"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("seed install: %v\n%s", err, out)
			}
			if err := os.Symlink("enginex", legacyBundleCustomDir); err != nil {
				t.Fatal(err)
			}
			if !viaImg {
				os.WriteFile(filepath.Join(bundleCustomDir, "gen2-locked"), []byte("1\n"), 0o644)
			}

			curTag := "v5.3.0.4-r3"
			var gh *httptest.Server
			gh = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/Cd1s/EngineX-OS/releases/latest":
					fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://x/r","assets":[
						{"name":%q,"browser_download_url":%q},
						{"name":%q,"browser_download_url":%q}]}`,
						curTag, r3ImgName, gh.URL+"/dl/"+r3ImgName, r3TarName, gh.URL+"/dl/"+r3TarName)
				case "/dl/" + r3ImgName:
					w.Write(r3Img)
				case "/dl/" + r3TarName:
					w.Write(r3Tar)
				case "/dl/" + releasesig.SumsAssetName, "/dl/" + releasesig.SigAssetName:
					// The release is signed (#239): both of its files are listed.
					sums := []byte(fmt.Sprintf("%x  %s\n%x  %s\n%s  %s\n", sha256.Sum256(r3Img), r3ImgName, sha256.Sum256(r3Tar), r3TarName, releasesig.TagDigest(curTag), releasesig.TagEntryName))
					if strings.HasSuffix(r.URL.Path, ".sig") {
						sums, _ = releasesig.Sign(testReleasePriv, sums)
					}
					w.Write(sums)
				default:
					http.NotFound(w, r)
				}
			}))
			defer gh.Close()

			newSrv := func() *Server {
				return New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: filepath.Join(root, "state"), GitHubURL: gh.URL, HTTP: gh.Client()}, nil)
			}

			// 1. Plain 5.3.0.4 install IS offered v5.3.0.4-r3.
			s1 := newSrv()
			u1 := getUpdate(s1, "")
			if !u1.UpdateAvailable || u1.Current != "5.3.0.4" || u1.Latest != "v5.3.0.4-r3" {
				t.Fatalf("plain 5.3.0.4 before r3: %+v", u1)
			}
			getUpdate(s1, "?apply=1")
			if done := waitUpdate(t, s1); done.Error != "" || done.Progress != 1 {
				t.Fatalf("apply r3 (%s): %+v", mode, done)
			}
			if got, _ := os.ReadFile(filepath.Join(bundleCustomDir, "bundle", releaseTagName)); string(got) != "v5.3.0.4-r3\n" {
				t.Fatalf("bundle/release-tag after apply = %q, want v5.3.0.4-r3", got)
			}

			// 2. Restart into a fresh Server: r3 is NOT offered r3 again, and IS offered v5.3.0.5.
			s2 := newSrv()
			if u2 := getUpdate(s2, ""); u2.UpdateAvailable || u2.Current != "5.3.0.4" || u2.Latest != "v5.3.0.4-r3" {
				t.Fatalf("after restart on r3: %+v", u2)
			}
			curTag = "v5.3.0.5"
			s3 := newSrv()
			if u3 := getUpdate(s3, ""); !u3.UpdateAvailable || u3.Latest != "v5.3.0.5" {
				t.Fatalf("r3 offered 5.3.0.5: %+v", u3)
			}
		})
	}

	t.Run("missing-or-unmigrated-sc5000-custom-still-reads-enginex-release-tag", func(t *testing.T) {
		root := t.TempDir()
		bundleCustomDir = filepath.Join(root, "enginex")
		legacyBundleCustomDir = filepath.Join(root, "sc5000-custom")
		versionFile = filepath.Join(legacyBundleCustomDir, "version.txt")
		imageVersionFile = filepath.Join(root, "no-etc-version")
		imageReleaseTagFile = filepath.Join(root, "no-etc-release-tag")
		deviceCompatiblePath = filepath.Join(root, "compatible")
		os.WriteFile(deviceCompatiblePath, []byte("inmusic,jp24\x00inmusic,az04\x00"), 0o644)

		top, err := unpackBundleReader(bytes.NewReader(r3Tar), filepath.Join(root, "u"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", filepath.Join(top, "install.sh"))
		cmd.Env = append(os.Environ(), "SC5000_CUSTOM="+bundleCustomDir, "SC5000_ALSA_OVERLAY="+filepath.Join(root, "alsa"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("install r3: %v\n%s", err, out)
		}

		curTag := "v5.3.0.4-r3"
		gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://x/r","assets":[{"name":%q,"browser_download_url":"https://example.invalid/r3.img"}]}`, curTag, r3ImgName)
		}))
		defer gh.Close()
		newSrv := func() *Server {
			return New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: filepath.Join(root, "state"), GitHubURL: gh.URL}, nil)
		}

		// Case A: /data/sc5000-custom symlink missing on 2nd-gen deck (no /etc/sc5000/version.txt).
		if u := getUpdate(newSrv(), ""); u.UpdateAvailable || u.Current != "5.3.0.4" {
			t.Errorf("missing sc5000-custom symlink: got %+v, want Current=5.3.0.4 UpdateAvailable=false", u)
		}

		// Case B: leftover real /data/sc5000-custom directory (old 5.3.0.4 without release-tag) alongside /data/enginex (r3).
		os.MkdirAll(filepath.Join(legacyBundleCustomDir, "bundle"), 0o700)
		os.WriteFile(filepath.Join(legacyBundleCustomDir, "version.txt"), []byte("5.3.0.4\n"), 0o644)
		if u := getUpdate(newSrv(), ""); u.UpdateAvailable || u.Current != "5.3.0.4" {
			t.Errorf("coexisting legacy sc5000-custom dir: got %+v, want Current=5.3.0.4 UpdateAvailable=false", u)
		}
		curTag = "v5.3.0.5"
		if u := getUpdate(newSrv(), ""); !u.UpdateAvailable || u.Latest != "v5.3.0.5" {
			t.Errorf("coexisting dirs with 5.3.0.5: got %+v, want UpdateAvailable=true", u)
		}
	})
}
