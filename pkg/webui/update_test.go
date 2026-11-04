package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		w.Write([]byte(`{"tag_name":"v2","html_url":"https://github.com/me/fork/releases/v2"}`))
	}))
	defer gh.Close()
	ui, state := t.TempDir(), t.TempDir()
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
	if v := get(); asked != "/repos/me/fork/releases/latest" || v.Current != "v1" || v.Latest != "v2" || !v.UpdateAvailable || v.Error != "" {
		t.Fatalf("update %+v (asked %s)", v, asked)
	}
}
