package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"inmusic,jp07\x00rockchip,rk3288": "SC5000",
		"inmusic,jc16":                    "PRIME2",
		"inmusic,jp21":                    "SCLIVE4",
		"rockchip,rk3288":                 "",
	} {
		if got := imagePrefix(strings.Split(in, "\x00")); got != want {
			t.Errorf("imagePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
