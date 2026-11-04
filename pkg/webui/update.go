package webui

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
)

// EngineX checks its own GitHub repository for releases; the UI pack hides
// Engine's Denon firmware check, whose updates would replace the whole mod.

const (
	defaultGitHubRepo = "Cd1s/sc5000-custom-os"
	versionFile       = "/data/sc5000-custom/version.txt"
	updateCacheFor    = 10 * time.Minute
)

var githubRepoRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

type updateStatus struct {
	Repo            string `json:"repo"`
	Current         string `json:"currentVersion"`
	Latest          string `json:"latestVersion,omitempty"`
	URL             string `json:"releaseUrl,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	Error           string `json:"error,omitempty"`
	checked         time.Time
}

func currentVersion(path string) string {
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b))
	}
	return "dev"
}

// update answers GET /v1/sc5000/update with the newest release of the repo.
func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	s.uiMu.Lock()
	repo := s.loadUI().GitHubRepo
	s.uiMu.Unlock()
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if v := s.updateCache; v.Repo != repo || time.Since(v.checked) > updateCacheFor {
		s.updateCache = s.checkRelease(r.Context(), repo)
	}
	respond(w, s.updateCache)
}

func (s *Server) checkRelease(ctx context.Context, repo string) updateStatus {
	path := s.cfg.VersionFile
	if path == "" {
		path = versionFile
	}
	v := updateStatus{Repo: repo, Current: currentVersion(path), checked: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	base := s.cfg.GitHubURL
	if base == "" {
		base = "https://api.github.com"
	}
	var rel struct {
		Tag string `json:"tag_name"`
		URL string `json:"html_url"`
	}
	h := http.Header{"Accept": {"application/vnd.github+json"}, "User-Agent": {"EngineX-OS"}}
	if err := providers.JSON(ctx, s.cfg.HTTP, "GET", base+"/repos/"+repo+"/releases/latest", nil, h, &rel); err != nil {
		// GitHub answers 404 both for no release yet and for a private repository.
		v.Error = "no release found (no releases yet, or the repository is private)"
		if !strings.Contains(err.Error(), "HTTP 404") {
			v.Error = "GitHub unreachable"
		}
		v.checked = time.Now().Add(time.Minute - updateCacheFor) // retry failures after a minute
		return v
	}
	v.Latest, v.URL = rel.Tag, rel.URL
	v.UpdateAvailable = rel.Tag != "" && rel.Tag != v.Current
	return v
}
