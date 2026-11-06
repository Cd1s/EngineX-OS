package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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

	bundleSuffix    = "-EngineX-OS-Data-Bundle.tar.gz"
	bundleCustomDir = "/data/sc5000-custom"
	bundleMaxDown   = 64 << 20  // compressed download
	bundleMaxUnpack = 128 << 20 // all extracted files together
)

var githubRepoRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

type updateStatus struct {
	Repo            string `json:"repo"`
	Current         string `json:"currentVersion"`
	Latest          string `json:"latestVersion,omitempty"`
	URL             string `json:"releaseUrl,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
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
	if strings.HasSuffix(downloadURL, bundleSuffix) {
		s.applyDataBundleUpdate(downloadURL)
		return
	}
	uiDir := s.uiDir()
	_ = os.Remove(filepath.Join(uiDir, "safe-mode"))
	_ = os.Remove(filepath.Join(uiDir, "boot-count"))
	// Trigger stock Denon full-screen graphical updater (/usr/bin/planck-remote-screen --update)
	// via /usr/Engine/Scripts/engine's native UpdateFromUrl quit handler.
	if err := os.WriteFile("/tmp/update-file-url", []byte(downloadURL+"\n"), 0o644); err == nil {
		if err := os.WriteFile("/tmp/engine-quit-reason", []byte("UpdateFromUrl\n"), 0o644); err == nil {
			s.setUpdateProgress(0.10, "", false)
			time.Sleep(150 * time.Millisecond)
			// Engine renames itself "EMain", so match the command line, not the name.
			if exec.Command("pkill", "-TERM", "-f", "^/usr/Engine/Engine( |$)").Run() == nil {
				s.setUpdateProgress(1.0, "", true)
				return
			}
			_ = os.Remove("/tmp/engine-quit-reason")
			_ = os.Remove("/tmp/update-file-url")
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

func (s *Server) installedVersion() string {
	path := s.cfg.VersionFile
	if path == "" {
		path = versionFile
	}
	current := currentVersion(path)
	if s.cfg.VersionFile == "" {
		if imgVer := currentVersion(imageVersionFile); current == "dev" || newerVersion(imgVer, current) {
			current = imgVer // the flashed image's release (#13)
		}
	}
	return current
}

func (s *Server) checkRelease(ctx context.Context, repo string) updateStatus {
	current := s.installedVersion()
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
		// Engine's own scan expects <PREFIX>-<version>-...Update.img; the second
		// generation takes the /data bundle instead (#86).
		suffix := "-Update.img"
		if dataBundleBoards[prefix] {
			suffix = bundleSuffix
		}
		for _, a := range rel.Assets {
			if strings.HasPrefix(a.Name, prefix+"-") && strings.HasSuffix(a.Name, suffix) && strings.HasPrefix(a.URL, "https://") {
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
	// The second generation (SC LIVE 4/2, PRIME 4+, PRIME GO+, Mixstream Pro+ / Pro Go;
	// #61, #68, #69) runs signed AZ0x firmware with SC LIVE 4's Engine: no .img, only
	// the /data bundle, so it is never offered. Its own prefix keeps a PRIME 4+ off
	// the first PRIME 4 image and a Mixstream Pro+ off the Mixstream Pro one.
	"inmusic,jp21": "SCLIVE4", "inmusic,jp20": "SCLIVE2",
	"inmusic,jc11s": "PRIME4PLUS", "inmusic,jp11s": "PRIMEGOPLUS",
	"inmusic,nh08s": "MIXSTREAMPROPLUS", "inmusic,nh10": "MIXSTREAMPROGO",
	// PRIME 4 G2 / SYSTEM ONE: the 64-bit AZ04 firmware, also only a /data bundle (#76).
	"inmusic,jp24": "PRIME4G2", "inmusic,rmz2": "SYSTEMONE",
}

// dataBundleBoards update from a Data-Bundle.tar.gz, not an .img (#86).
var dataBundleBoards = map[string]bool{"SCLIVE4": true, "SCLIVE2": true, "PRIME4PLUS": true, "PRIMEGOPLUS": true,
	"MIXSTREAMPROPLUS": true, "MIXSTREAMPROGO": true, "PRIME4G2": true, "SYSTEMONE": true}

// playerBoards are the standalone players: no channel faders, so no Upfader Echo (#52).
var playerBoards = map[string]bool{"SC5000": true, "SC5000M": true, "SC6000": true, "SC6000M": true}

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

// applyDataBundleUpdate downloads a second-generation /data bundle, unpacks it
// into a scratch directory next to the install and lets its own install.sh
// verify SHA256SUMS and swap the parts in (#86).
func (s *Server) applyDataBundleUpdate(downloadURL string) {
	fail := func(msg string) { s.setUpdateProgress(0, msg, true) }
	customDir := bundleCustomDir
	if s.cfg.VersionFile != "" {
		customDir = filepath.Dir(s.cfg.VersionFile)
	}
	if err := os.MkdirAll(customDir, 0o700); err != nil {
		fail("cannot write " + customDir)
		return
	}
	work, err := os.MkdirTemp(customDir, ".ota-bundle-*")
	if err != nil {
		fail("cannot write " + customDir)
		return
	}
	defer os.RemoveAll(work)
	archive := filepath.Join(work, "bundle.tar.gz")
	if msg := s.downloadBundle(downloadURL, archive); msg != "" {
		fail(msg)
		return
	}
	top, err := unpackBundle(archive, filepath.Join(work, "x"))
	if err != nil {
		fail(err.Error())
		return
	}
	s.setUpdateProgress(0.90, "", false)
	cmd := exec.Command("sh", filepath.Join(top, "install.sh"))
	cmd.Env = append(os.Environ(), "SC5000_CUSTOM="+customDir)
	if customDir != bundleCustomDir { // sandbox / test: no /data to write the ALSA file to
		cmd.Env = append(cmd.Env, "SC5000_ALSA_OVERLAY="+filepath.Join(customDir, "alsa-conf.d"))
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		fail("bundle install: " + msg)
		return
	}
	s.updateMu.Lock()
	s.updateCache.Current = s.installedVersion()
	s.updateCache.UpdateAvailable = false
	s.updateMu.Unlock()
	s.setUpdateProgress(1.0, "", true)
	if customDir == bundleCustomDir {
		// A detached shell outlives stream-sync, which it kills so the new suite starts.
		// shortcut: if systemd kills the whole cgroup with the service, the reboot step is
		// lost and the user reboots by hand; confirm on a real deck.
		script := `sleep 2; pkill -f '[s]c5000-suite (stream-sync|rb-bridge|sc5000-deemixd)'; sleep 1; exec sh ` + bundleCustomDir + `/bundle/boot-activate.sh`
		c := exec.Command("sh", "-c", script)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		_ = c.Start()
	}
}

// downloadBundle fetches url into dest, at most bundleMaxDown bytes; it returns an error message or "".
func (s *Server) downloadBundle(url, dest string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "update download failed"
	}
	req.Header.Set("User-Agent", "EngineX-OS")
	hc := s.cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "update download failed"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "update download failed"
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "disk write failed"
	}
	defer out.Close()
	var written int64
	buf := make([]byte, 128*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > bundleMaxDown {
				return "update download too large"
			}
			if _, werr := out.Write(buf[:n]); werr != nil {
				return "disk write failed"
			}
			if total := resp.ContentLength; total > 0 {
				s.setUpdateProgress(0.05+0.80*float64(written)/float64(total), "", false)
			}
		}
		if rerr == io.EOF {
			return ""
		}
		if rerr != nil {
			return "download interrupted"
		}
	}
}

// unpackBundle extracts a bundle into dir, refusing anything but plain files
// and directories under one top directory, and returns that directory.
func unpackBundle(archive, dir string) (string, error) {
	bad := errors.New("invalid data bundle archive")
	f, err := os.Open(archive)
	if err != nil {
		return "", bad
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", bad
	}
	tr := tar.NewReader(gz)
	var top string
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", bad
		}
		name := strings.TrimSuffix(h.Name, "/")
		parts := strings.Split(name, "/")
		if name == "" || strings.HasPrefix(h.Name, "/") {
			return "", bad
		}
		for _, p := range parts {
			if p == ".." || p == "" || p == "." {
				return "", bad
			}
		}
		if top == "" {
			top = parts[0]
		} else if parts[0] != top {
			return "", bad
		}
		dest := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return "", bad
			}
		case tar.TypeReg:
			total += h.Size
			if h.Size < 0 || total > bundleMaxUnpack {
				return "", bad
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return "", bad
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return "", bad
			}
			_, cerr := io.Copy(out, io.LimitReader(tr, h.Size))
			if out.Close() != nil || cerr != nil {
				return "", bad
			}
		default:
			return "", bad
		}
	}
	root := filepath.Join(dir, top)
	for _, need := range []string{"install.sh", "SHA256SUMS"} {
		if st, err := os.Stat(filepath.Join(root, need)); top == "" || err != nil || !st.Mode().IsRegular() {
			return "", bad
		}
	}
	return root, nil
}
