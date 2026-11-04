package webui

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// EngineX checks its own GitHub repository for releases; the UI pack hides
// Engine's Denon firmware check, whose updates would replace the whole mod.

const (
	defaultGitHubRepo = "Cd1s/EngineX-OS"
	versionFile       = "/data/sc5000-custom/version.txt"
	// imageVersionFile is written by az01_packer.py patch-rootfs --set-version.
	imageVersionFile = "/etc/sc5000/version.txt"
	stagedUpdateFile = "/data/az01-update.img"
	updateCacheFor   = 10 * time.Minute
)

var githubRepoRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

type updateStatus struct {
	Repo            string  `json:"repo"`
	Current         string  `json:"currentVersion"`
	Latest          string  `json:"latestVersion,omitempty"`
	URL             string  `json:"releaseUrl,omitempty"`
	UpdateAvailable bool    `json:"updateAvailable"`
	// DownloadURL is this model's upgrade image in the release, for Engine's
	// own online updater (Settings → Update).
	DownloadURL string  `json:"downloadUrl,omitempty"`
	Updating    bool    `json:"updating,omitempty"`
	Progress    float64 `json:"progress,omitempty"`
	Error       string  `json:"error,omitempty"`
	checked     time.Time
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
	if !s.updateCache.Updating {
		if v := s.updateCache; v.Repo != repo || time.Since(v.checked) > updateCacheFor {
			// Detached: a deck page closing mid-check must not cache "unreachable" (#13).
			s.updateCache = s.checkRelease(context.WithoutCancel(r.Context()), repo)
		}
		if strings.HasPrefix(r.URL.Path, "/v1/sc5000/update") && r.URL.Query().Get("apply") == "1" &&
			s.updateCache.UpdateAvailable && s.updateCache.DownloadURL != "" {
			s.updateCache.Updating = true
			s.updateCache.Progress = 0.05
			s.updateCache.Error = ""
			go s.applyOnlineUpdate(s.updateCache.DownloadURL)
		}
	}
	respond(w, s.updateCache)
}

func hasValidAZ01Image(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() < 50*1024*1024 {
		return false
	}
	var head [4]byte
	if _, err := io.ReadFull(f, head[:]); err != nil || string(head[:]) != "AZ01" {
		return false
	}
	var tail [16]byte
	if _, err := f.ReadAt(tail[:], st.Size()-16); err != nil {
		return false
	}
	return bytes.Equal(tail[:4], []byte("EOF\x00"))
}

func (s *Server) setUpdateProgress(p float64, errMsg string, done bool) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.updateCache.Progress = p
	if errMsg != "" {
		s.updateCache.Error = errMsg
	}
	if done {
		s.updateCache.Updating = false
	}
}

func hasMatchingStagedImage(path, downloadURL string) bool {
	if b, err := os.ReadFile(path + ".url"); err != nil || strings.TrimSpace(string(b)) != downloadURL {
		return false
	}
	return hasValidAZ01Image(path)
}

func (s *Server) applyOnlineUpdate(downloadURL string) {
	uiDir := s.uiDir()
	_ = os.Remove(filepath.Join(uiDir, "safe-mode"))
	_ = os.Remove(filepath.Join(uiDir, "boot-count"))
	// Trigger stock Denon full-screen graphical updater (/usr/bin/planck-remote-screen --update)
	// via /usr/Engine/Scripts/engine's native UpdateFromUrl quit handler.
	if err := os.WriteFile("/tmp/update-file-url", []byte(downloadURL+"\n"), 0o644); err == nil {
		if err := os.WriteFile("/tmp/engine-quit-reason", []byte("UpdateFromUrl\n"), 0o644); err == nil {
			s.setUpdateProgress(0.10, "", false)
			time.Sleep(150 * time.Millisecond)
			if exec.Command("pkill", "-TERM", "-x", "Engine").Run() == nil {
				return
			}
		}
	}
	if !hasMatchingStagedImage(stagedUpdateFile, downloadURL) {
		part := stagedUpdateFile + ".part"
		_ = os.Remove(part)
		_ = os.Remove(stagedUpdateFile + ".url")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
		if err != nil {
			cancel()
			s.setUpdateProgress(0, "update download failed", true)
			return
		}
		req.Header.Set("User-Agent", "EngineX-OS")
		hc := s.cfg.HTTP
		if hc == nil {
			hc = &http.Client{Timeout: 20 * time.Minute}
		}
		resp, err := hc.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			cancel()
			s.setUpdateProgress(0, "update download failed", true)
			return
		}
		out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			resp.Body.Close()
			cancel()
			s.setUpdateProgress(0, "cannot write /data/az01-update.img", true)
			return
		}
		total := resp.ContentLength
		var written int64
		buf := make([]byte, 256*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					out.Close()
					resp.Body.Close()
					cancel()
					_ = os.Remove(part)
					s.setUpdateProgress(0, "disk write failed", true)
					return
				}
				written += int64(n)
				if total > 0 {
					p := 0.05 + 0.88*(float64(written)/float64(total))
					if p > 0.93 {
						p = 0.93
					}
					s.setUpdateProgress(p, "", false)
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				out.Close()
				resp.Body.Close()
				cancel()
				_ = os.Remove(part)
				s.setUpdateProgress(0, "download interrupted", true)
				return
			}
		}
		_ = out.Sync()
		out.Close()
		resp.Body.Close()
		cancel()
		if !hasValidAZ01Image(part) || os.Rename(part, stagedUpdateFile) != nil {
			_ = os.Remove(part)
			s.setUpdateProgress(0, "downloaded image failed AZ01 check", true)
			return
		}
		_ = os.WriteFile(stagedUpdateFile+".url", []byte(downloadURL+"\n"), 0o644)
	}
	s.setUpdateProgress(0.97, "", false)
	_ = os.Remove(filepath.Join(uiDir, "safe-mode"))
	_ = os.Remove(filepath.Join(uiDir, "boot-count"))
	cmd := exec.Command("/usr/sbin/az01-update", "--file", stagedUpdateFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		s.setUpdateProgress(0, "az01-update: "+msg, true)
		return
	}
	s.setUpdateProgress(1.0, "", false)
}

func (s *Server) checkRelease(ctx context.Context, repo string) updateStatus {
	path := s.cfg.VersionFile
	if path == "" {
		path = versionFile
	}
	current := currentVersion(path)
	if current == "dev" && s.cfg.VersionFile == "" {
		current = currentVersion(imageVersionFile) // the flashed image's release (#13)
	}
	v := updateStatus{Repo: repo, Current: current, checked: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	base := s.cfg.GitHubURL
	if base == "" {
		base = "https://api.github.com"
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
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
	v.UpdateAvailable = newerVersion(rel.Tag, v.Current)
	if prefix := imagePrefix(deviceCompatible()); prefix != "" {
		for _, a := range rel.Assets {
			// Engine's own scan expects <PREFIX>-<version>-...Update.img.
			if strings.HasPrefix(a.Name, prefix+"-") && strings.HasSuffix(a.Name, "-Update.img") && strings.HasPrefix(a.URL, "https://") {
				v.DownloadURL = a.URL
				break
			}
		}
	}
	return v
}

// deviceCompatiblePath lists the board's device-tree compatible strings.
var deviceCompatiblePath = "/sys/firmware/devicetree/base/compatible"

func deviceCompatible() []string {
	b, err := os.ReadFile(deviceCompatiblePath)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}

// imagePrefixes name each model's upgrade image as Denon does (SC5000-…-Update.img).
var imagePrefixes = map[string]string{
	"inmusic,jp07": "SC5000", "inmusic,jp08": "SC5000M", "inmusic,jp11": "PRIMEGO",
	"inmusic,jp13": "SC6000", "inmusic,jp14": "SC6000M",
	"inmusic,jc11": "PRIME4", "inmusic,jc16": "PRIME2", "inmusic,nh08": "MIXSTREAMPRO",
}

func imagePrefix(compatible []string) string {
	for _, c := range compatible {
		if p := imagePrefixes[c]; p != "" {
			return p
		}
	}
	return ""
}

var versionNumberRE = regexp.MustCompile(`\d+`)

// newerVersion compares the numbers in two tags in order, so v5.0.4-custom.10 is
// newer than v5.0.4-custom.9 and an older tag is never offered. A current
// version without numbers ("dev", no version.txt) is unknown: never claim an update.
func newerVersion(latest, current string) bool {
	l, c := versionNumberRE.FindAllString(latest, -1), versionNumberRE.FindAllString(current, -1)
	if len(l) == 0 || len(c) == 0 {
		return false
	}
	for i := 0; i < len(l) && i < len(c); i++ {
		a, b := strings.TrimLeft(l[i], "0"), strings.TrimLeft(c[i], "0")
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		if a != b {
			return a > b
		}
	}
	return len(l) > len(c)
}
