package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
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

	bundleSuffix          = "-EngineX-OS-Data-Bundle.tar.gz"
	bundleDirSuffix       = "-EngineX-OS-Data-Bundle"
	bundleCustomDir       = "/data/enginex"
	legacyBundleCustomDir = "/data/sc5000-custom"
	bundleMaxDown         = 64 << 20  // compressed download
	bundleMaxUnpack       = 128 << 20 // all extracted files together
	gen2ImgPrefix         = "ENGINEX_DATA_BUNDLE"
	gen2ImgMagic          = "ENGINEX_DATA_BUNDLE_IMG_V1\n"
	gen2ImgHeader         = 128
)

func isDeckDataDir(dir string) bool {
	return dir == bundleCustomDir || dir == legacyBundleCustomDir
}

var (
	engineScriptPath       = "/usr/Engine/Scripts/engine"
	planckRemoteScreenPath = "/usr/bin/planck-remote-screen"
	updateFileURLPath      = "/tmp/update-file-url"
	engineQuitReasonPath   = "/tmp/engine-quit-reason"
	allowUpdateFromURL     = isDeckDataDir
	quitEngineForUpdate    = func() error {
		return exec.Command("pkill", "-TERM", "-f", "^/usr/Engine/Engine( |$)").Run()
	}

	errBundleTooLarge = errors.New("update download too large")
	bundleVersionRE   = regexp.MustCompile(`^\d+(\.\d+){2,4}$`)
	readmeUpdateImgRE = regexp.MustCompile(`(?m)^Update\.img:\s*([A-Z0-9]+)-`)
	// Short backoff schedule for up to 3 transient download retries (4 total attempts).
	otaRetryBackoffs = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
)

func hasPreloadedPlanckUpdater(scriptPath, updaterPath string) bool {
	if _, err := os.Stat(updaterPath); err != nil {
		return false
	}
	b, err := os.ReadFile(scriptPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "LD_PRELOAD=") && strings.Contains(trimmed, "planck-remote-screen") {
			return true
		}
	}
	return false
}

func canUseUpdateFromURL(customDir string) bool {
	if !allowUpdateFromURL(customDir) {
		return false
	}
	if dataBundleBoards[imagePrefix(deviceCompatible())] {
		return hasPreloadedPlanckUpdater(engineScriptPath, planckRemoteScreenPath)
	}
	return true
}

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
	Digest      string  `json:"digest,omitempty"`
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

// update answers GET /v1/sc5000/update and GET /api/update with the newest release of the repo.
func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	s.uiMu.Lock()
	repo := s.loadUI().GitHubRepo
	s.uiMu.Unlock()
	forceCheck := r.URL.Query().Get("check") == "1"
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if !s.updateCache.Updating {
		if v := s.updateCache; forceCheck || v.Repo != repo || time.Since(v.checked) > updateCacheFor {
			// Detached: a deck page closing mid-check must not cache "unreachable" (#13).
			s.updateCache = s.checkRelease(context.WithoutCancel(r.Context()), repo)
		}
		if strings.HasPrefix(r.URL.Path, "/v1/sc5000/update") && r.URL.Query().Get("apply") == "1" &&
			s.updateCache.UpdateAvailable && s.updateCache.DownloadURL != "" {
			s.updateCache.Updating = true
			s.updateCache.Progress = 0.05
			s.updateCache.Error = ""
			go s.applyOnlineUpdateWithDigest(s.updateCache.DownloadURL, s.updateCache.Digest)
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
	s.applyOnlineUpdateWithDigest(downloadURL, "")
}

func (s *Server) applyOnlineUpdateWithDigest(downloadURL, digest string) {
	if strings.HasSuffix(downloadURL, bundleSuffix) {
		s.applyDataBundleUpdate(downloadURL, digest)
		return
	}
	customDir := s.customDir()
	// Trigger stock Denon full-screen graphical updater (/usr/bin/planck-remote-screen --update)
	// via /usr/Engine/Scripts/engine's native UpdateFromUrl quit handler. On Gen-2 boards, only
	// hand off to UpdateFromUrl when /usr/Engine/Scripts/engine already wraps planck-remote-screen
	// with LD_PRELOAD (and planck-remote-screen exists); otherwise download and apply in Go (#215).
	if canUseUpdateFromURL(customDir) {
		if err := os.WriteFile(updateFileURLPath, []byte(downloadURL+"\n"), 0o644); err == nil {
			if err := os.WriteFile(engineQuitReasonPath, []byte("UpdateFromUrl\n"), 0o644); err == nil {
				s.setUpdateProgress(0.10, "", false)
				time.Sleep(150 * time.Millisecond)
				// Engine renames itself "EMain", so match the command line, not the name.
				if quitEngineForUpdate() == nil {
					s.setUpdateProgress(1.0, "", true)
					return
				}
				_ = os.Remove(engineQuitReasonPath)
				_ = os.Remove(updateFileURLPath)
			}
		}
	}
	staged := stagedUpdateFile
	if !isDeckDataDir(customDir) {
		_ = os.MkdirAll(customDir, 0o700)
		staged = filepath.Join(customDir, "az01-update.img")
	}
	if !hasMatchingStagedImage(staged, downloadURL) {
		part := staged + ".part"
		_ = os.Remove(part)
		_ = os.Remove(staged + ".url")
		msg := s.downloadToFile(downloadURL, part, 0o644, 20*time.Minute, 0, true, func(written, total int64) {
			p := 0.05 + 0.88*(float64(written)/float64(total))
			if p > 0.93 {
				p = 0.93
			}
			s.setUpdateProgress(p, "", false)
		})
		if msg != "" {
			_ = os.Remove(part)
			s.setUpdateProgress(0, msg, true)
			return
		}
		if err := verifyFileSHA256(part, digest); err != nil {
			_ = os.Remove(part)
			log.Printf("stream-sync: ota image %s failed SHA-256 verification: %v", sanitizeLogURL(downloadURL), err)
			s.setUpdateProgress(0, "downloaded image failed SHA-256 check", true)
			return
		}
		if s.tryApplyGen2Image(part) {
			return
		}
		if !hasValidAZ01Image(part) || os.Rename(part, staged) != nil {
			_ = os.Remove(part)
			s.setUpdateProgress(0, "downloaded image failed AZ01 check", true)
			return
		}
		_ = os.WriteFile(staged+".url", []byte(downloadURL+"\n"), 0o644)
	}
	s.setUpdateProgress(0.97, "", false)
	cmd := exec.Command("/usr/sbin/az01-update", "--file", staged)
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

// tryApplyGen2Image handles a 2nd-generation *-Update.img (128-byte ENGINEX_DATA_BUNDLE_IMG_V1
// header + Data-Bundle.tar.gz) when the background download fallback ran instead of Engine's
// Quit-to-Updater path. Returns false only if part is not a 2nd-gen bundle image.
func (s *Server) tryApplyGen2Image(part string) bool {
	f, err := os.Open(part)
	if err != nil {
		return false
	}
	defer f.Close()
	var head [gen2ImgHeader]byte
	n, _ := io.ReadFull(f, head[:])
	if !bytes.HasPrefix(head[:n], []byte(gen2ImgPrefix)) && !dataBundleBoards[imagePrefix(deviceCompatible())] {
		return false
	}
	_ = os.Remove(part)
	if s.isGen2Locked() {
		s.setUpdateProgress(0, "gen2 .img update is locked", true)
		return true
	}
	st, err := f.Stat()
	if err != nil || n != gen2ImgHeader || string(head[:len(gen2ImgMagic)]) != gen2ImgMagic ||
		head[len(gen2ImgMagic)+64] != '\n' || st.Size() <= gen2ImgHeader || st.Size()-gen2ImgHeader > bundleMaxDown {
		s.setUpdateProgress(0, "downloaded image failed SHA-256 check", true)
		return true
	}
	wantHex := strings.ToLower(string(head[len(gen2ImgMagic) : len(gen2ImgMagic)+64]))
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil || hex.EncodeToString(h.Sum(nil)) != wantHex {
		s.setUpdateProgress(0, "downloaded image failed SHA-256 check", true)
		return true
	}
	if _, err := f.Seek(gen2ImgHeader, io.SeekStart); err != nil {
		s.setUpdateProgress(0, "downloaded image failed SHA-256 check", true)
		return true
	}
	s.applyDataBundleFromReader(f)
	return true
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
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	endpoint := base + "/repos/" + repo + "/releases/latest"
	h := http.Header{"Accept": {"application/vnd.github+json"}, "User-Agent": {"EngineX-OS"}}
	if err := providers.JSON(ctx, s.cfg.HTTP, "GET", endpoint, nil, h, &rel); err != nil {
		// GitHub answers 404 both for no release yet and for a private repository.
		v.Error = "no release found (no releases yet, or the repository is private)"
		if !strings.Contains(err.Error(), "HTTP 404") {
			log.Printf("stream-sync: ota release check %s failed: %v", sanitizeLogURL(endpoint), unwrapURLError(err))
			v.Error = "GitHub unreachable"
		}
		v.checked = time.Now().Add(time.Minute - updateCacheFor) // retry failures after a minute
		return v
	}
	v.Latest, v.URL = rel.Tag, rel.URL
	v.UpdateAvailable = newerVersion(rel.Tag, v.Current)
	if prefix := imagePrefix(deviceCompatible()); prefix != "" {
		// Engine's own scan expects <PREFIX>-<version>-...Update.img. The second generation
		// takes the /data bundle (#86); once unlocked (#119) it prefers the bundle-carrying
		// *-Update.img, which the updater hooks accept, and falls back to the bundle.
		find := func(suffix string) (string, string) {
			for _, a := range rel.Assets {
				if strings.HasPrefix(a.Name, prefix+"-") && strings.HasSuffix(a.Name, suffix) && strings.HasPrefix(a.URL, "https://") {
					return a.URL, a.Digest
				}
			}
			return "", ""
		}
		// Phased Gen-2 *-Update.img migration (#215):
		//   - 5.3.0.4 (current): Releases publish both *-Update.img and *-Data-Bundle.tar.gz (28 artifacts);
		//     checkRelease prefers -Update.img when not gen2-locked and falls back to -Data-Bundle.tar.gz.
		//   - 5.3.0.5 (bridge): Releases still include *-Data-Bundle.tar.gz one final time so 5.3.0.2/5.3.0.3/early-5.3.0.4
		//     decks can OTA upgrade to 5.3.0.5, while 5.3.0.5's own checkRelease switches to -Update.img ONLY.
		//   - 5.3.0.6+ (final): Retire *-Data-Bundle.tar.gz from build-release.sh and GitHub Releases; all 16 models
		//     use *-Update.img exclusively for USB, OTA, and EngineX-Installer.
		if dataBundleBoards[prefix] {
			if !s.isGen2Locked() {
				v.DownloadURL, v.Digest = find("-Update.img")
			}
			if v.DownloadURL == "" {
				v.DownloadURL, v.Digest = find(bundleSuffix)
			}
		} else {
			v.DownloadURL, v.Digest = find("-Update.img")
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

// playerBoards are the standalone players: no channel faders, no built-in mixer (#52).
var playerBoards = map[string]bool{"SC5000": true, "SC5000M": true, "SC6000": true, "SC6000M": true}

// fourDeckBoards support 4-deck layout and shadow decks: the 4 standalone 2-layer
// players (#49) plus the 4 native 4-deck all-in-one consoles (#160).
var fourDeckBoards = map[string]bool{
	"SC5000": true, "SC5000M": true, "SC6000": true, "SC6000M": true,
	"PRIME4": true, "PRIME4PLUS": true, "SCLIVE4": true, "PRIME4G2": true,
}

// mixstreamBoards are the compact 4-pad Numark Mixstream consoles (hasNHFx, no AutoLoop knob; #166).
var mixstreamBoards = map[string]bool{"MIXSTREAMPRO": true, "MIXSTREAMPROPLUS": true, "MIXSTREAMPROGO": true}

// noTrackSkipKeyBoards lack physical Previous/Next Track keys (#146).
var noTrackSkipKeyBoards = map[string]bool{
	"PRIMEGO": true, "PRIMEGOPLUS": true,
	"MIXSTREAMPRO": true, "MIXSTREAMPROPLUS": true, "MIXSTREAMPROGO": true,
}

// az04Boards run the 64-bit AZ04 Engine (Stems stays stock).
var az04Boards = map[string]bool{"PRIME4G2": true, "SYSTEMONE": true}

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

// customDir is where the deck keeps EngineX's /data install (/data/enginex, or the dir of the test VersionFile).
func (s *Server) customDir() string {
	if s.cfg.VersionFile != "" {
		return filepath.Dir(s.cfg.VersionFile)
	}
	if target, err := filepath.EvalSymlinks(legacyBundleCustomDir); err == nil && target != "" {
		return target
	}
	return bundleCustomDir
}

// isGen2Unlocked reports whether the *-Update.img upgrade path is open (#119, #215):
// true whenever gen2-locked is absent and either customDir is the deck's /data runtime dir
// or gen2-unlocked exists, keeping GET /v1/sc5000/unlock-gen2 consistent with checkRelease.
func (s *Server) isGen2Unlocked() bool {
	if s.isGen2Locked() {
		return false
	}
	dir := s.customDir()
	if isDeckDataDir(dir) {
		return true
	}
	_, err := os.Stat(filepath.Join(dir, "gen2-unlocked"))
	return err == nil
}

// isGen2Locked reports whether the *-Update.img path was explicitly disabled via gen2-locked (#215).
func (s *Server) isGen2Locked() bool {
	_, err := os.Stat(filepath.Join(s.customDir(), "gen2-locked"))
	return err == nil
}

// verifyFileSHA256 verifies path against a GitHub release asset digest ("sha256:...").
// An empty digest preserves existing behaviour for releases/mocks without digests.
func verifyFileSHA256(path, digest string) error {
	d := strings.ToLower(strings.TrimSpace(digest))
	if d == "" {
		return nil
	}
	wantHex, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(wantHex) != 64 {
		return fmt.Errorf("invalid digest %q", digest)
	}
	if _, err := hex.DecodeString(wantHex); err != nil {
		return fmt.Errorf("invalid digest hex: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	gotHex := hex.EncodeToString(h.Sum(nil))
	if gotHex != wantHex {
		return fmt.Errorf("got sha256:%s, want sha256:%s", gotHex, wantHex)
	}
	return nil
}

// applyDataBundleUpdate downloads a second-generation /data bundle, verifies any
// release digest before install.sh runs, unpacks into a scratch directory next to
// the install and lets its own install.sh verify SHA256SUMS and swap the parts in (#86).
func (s *Server) applyDataBundleUpdate(downloadURL, digest string) {
	fail := func(msg string) { s.setUpdateProgress(0, msg, true) }
	customDir := s.customDir()
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
	if err := verifyFileSHA256(archive, digest); err != nil {
		log.Printf("stream-sync: ota bundle %s failed SHA-256 verification: %v", sanitizeLogURL(downloadURL), err)
		fail("downloaded bundle failed SHA-256 check")
		return
	}
	f, err := os.Open(archive)
	if err != nil {
		fail("invalid data bundle archive")
		return
	}
	defer f.Close()
	s.installDataBundleReader(f, work)
}

// applyDataBundleFromReader unpacks a Data-Bundle.tar.gz stream into a scratch directory
// under customDir and runs its install.sh (#119).
func (s *Server) applyDataBundleFromReader(r io.Reader) {
	fail := func(msg string) { s.setUpdateProgress(0, msg, true) }
	customDir := s.customDir()
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
	s.installDataBundleReader(r, work)
}

func checkBundleModel(top, deckPrefix string) error {
	if deckPrefix == "" || !dataBundleBoards[deckPrefix] {
		return nil
	}
	topBase := filepath.Base(top)
	stem, ok := strings.CutSuffix(topBase, bundleDirSuffix)
	topPrefix, topVer, cutOK := strings.Cut(stem, "-")
	if !ok || !cutOK || !bundleVersionRE.MatchString(topVer) {
		return errors.New("invalid data bundle archive")
	}
	if topPrefix != deckPrefix {
		return fmt.Errorf("bundle is for %s, this deck is %s", topPrefix, deckPrefix)
	}
	for _, marker := range []string{"model.txt", "prefix.txt", "MODEL"} {
		b, err := os.ReadFile(filepath.Join(top, marker))
		if err != nil {
			continue
		}
		if m := strings.TrimSpace(string(b)); m != "" && m != deckPrefix {
			return fmt.Errorf("bundle is for %s, this deck is %s", m, deckPrefix)
		}
	}
	if b, err := os.ReadFile(filepath.Join(top, "README-INSTALL.txt")); err == nil {
		if m := readmeUpdateImgRE.FindSubmatch(b); m != nil && string(m[1]) != deckPrefix {
			return fmt.Errorf("bundle is for %s, this deck is %s", string(m[1]), deckPrefix)
		}
	}
	if b, err := os.ReadFile(filepath.Join(top, "bin", "sc5000-suite")); err == nil && len(b) >= 20 && string(b[:4]) == "\x7fELF" {
		mach := uint16(b[18]) | uint16(b[19])<<8
		wantMach := uint16(40) // EM_ARM
		if az04Boards[deckPrefix] {
			wantMach = 183 // EM_AARCH64
		}
		if mach != wantMach {
			return fmt.Errorf("bundle CPU architecture does not match %s", deckPrefix)
		}
	}
	return nil
}

func (s *Server) installDataBundleReader(r io.Reader, work string) {
	fail := func(msg string) { s.setUpdateProgress(0, msg, true) }
	customDir := s.customDir()
	top, err := unpackBundleReader(r, filepath.Join(work, "x"))
	if err != nil {
		fail(err.Error())
		return
	}
	if err := checkBundleModel(top, imagePrefix(deviceCompatible())); err != nil {
		fail(err.Error())
		return
	}
	s.setUpdateProgress(0.90, "", false)
	cmd := exec.Command("sh", filepath.Join(top, "install.sh"))
	cmd.Env = append(os.Environ(), "SC5000_CUSTOM="+customDir)
	if !isDeckDataDir(customDir) { // sandbox / test: no /data to write the ALSA file to
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
	os.RemoveAll(work) // before reporting done: a client polling for Done must not see the scratch dir
	s.updateMu.Lock()
	s.updateCache.Current = s.installedVersion()
	s.updateCache.UpdateAvailable = newerVersion(s.updateCache.Latest, s.updateCache.Current)
	s.updateCache.Error = ""
	s.updateMu.Unlock()
	if isDeckDataDir(customDir) {
		// Run boot-activate in a transient systemd service if available so killing
		// stream-sync does not terminate the activation script along with stream-sync's cgroup.
		script := `sleep 2; pkill -f '[s]c5000-suite (stream-sync|rb-bridge|sc5000-deemixd)'; sleep 1; exec sh ` + customDir + `/bundle/boot-activate.sh`
		launched := false
		if _, err := exec.LookPath("systemd-run"); err == nil {
			// systemd-run submits the transient unit to PID 1 and returns immediately.
			if out, err := exec.Command("systemd-run", "--description=EngineX OTA Activation", "sh", "-c", script).CombinedOutput(); err == nil {
				launched = true
			} else {
				log.Printf("stream-sync: systemd-run ota activation failed: %v (%s)", err, strings.TrimSpace(string(out)))
			}
		}
		if launched {
			s.setUpdateProgress(1.0, "", true)
		} else {
			// systemd-run unavailable/failed: do not kill stream-sync in a shared cgroup.
			// Keep stream-sync running so the screen displays the prompt to reboot.
			s.setUpdateProgress(1.0, "Update installed. Please reboot your deck.", true)
		}
	} else {
		s.setUpdateProgress(1.0, "", true)
	}
}

func hostOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func failingURL(fallback string, err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		return ue.URL
	}
	return fallback
}

// sanitizeLogURL strips credentials, signed query strings and fragments before
// writing to the journal so temporary CDN signatures are never logged and
// /api/logs redaction keeps the diagnostic line intact.
func sanitizeLogURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "[invalid-url]"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

func isTLSError(err error) bool {
	var (
		recErr     tls.RecordHeaderError
		certVerErr *tls.CertificateVerificationError
		alertErr   tls.AlertError
		unkAuth    x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		certInv    x509.CertificateInvalidError
		sysRoots   x509.SystemRootsError
	)
	return errors.As(err, &recErr) ||
		errors.As(err, &certVerErr) ||
		errors.As(err, &alertErr) ||
		errors.As(err, &unkAuth) ||
		errors.As(err, &hostErr) ||
		errors.As(err, &certInv) ||
		errors.As(err, &sysRoots)
}

func classifyDownloadError(err error) (kind string, retryable bool) {
	if err == nil {
		return "", false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS lookup", true
	}
	if isTLSError(err) {
		return "TLS", false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused", true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return "connection reset", true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout", true
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "no such host") || strings.Contains(s, "server misbehaving"):
		return "DNS lookup", true
	case strings.Contains(s, "tls:") || strings.Contains(s, "x509:") || strings.Contains(s, "certificate") || strings.Contains(s, "handshake"):
		return "TLS", false
	case strings.Contains(s, "connection refused"):
		return "connection refused", true
	case strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe"):
		return "connection reset", true
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded"):
		return "timeout", true
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
		return "connection reset", true
	case errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || strings.Contains(s, "network is unreachable") || strings.Contains(s, "no route to host"):
		return "network unreachable", true
	default:
		return "connection error", true
	}
}

func formatDownloadFailure(host, kind string) string {
	if host == "" {
		return "download failed (" + kind + ")"
	}
	return "download failed: " + host + " (" + kind + ")"
}

func (s *Server) downloadToFile(rawURL, dest string, mode os.FileMode, attemptTimeout time.Duration, maxBytes int64, syncFile bool, onProgress func(written, total int64)) string {
	maxAttempts := 1 + len(otaRetryBackoffs)
	buf := make([]byte, 256*1024)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(otaRetryBackoffs[attempt-2])
		}
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
		if err != nil {
			cancel()
			host := hostOfURL(rawURL)
			log.Printf("stream-sync: ota download %s failed [invalid URL]: %v", sanitizeLogURL(rawURL), unwrapURLError(err))
			return formatDownloadFailure(host, "invalid URL")
		}
		req.Header.Set("User-Agent", "EngineX-OS")
		hc := s.cfg.HTTP
		if hc == nil {
			hc = &http.Client{Timeout: attemptTimeout}
		}
		resp, err := hc.Do(req)
		if err != nil {
			cancel()
			failURL := failingURL(rawURL, err)
			host := hostOfURL(failURL)
			kind, retryable := classifyDownloadError(err)
			log.Printf("stream-sync: ota download %s (attempt %d/%d) failed [%s]: %v", sanitizeLogURL(failURL), attempt, maxAttempts, kind, unwrapURLError(err))
			if retryable && attempt < maxAttempts {
				continue
			}
			return formatDownloadFailure(host, kind)
		}
		if resp.StatusCode != http.StatusOK {
			failURL := rawURL
			if resp.Request != nil && resp.Request.URL != nil {
				failURL = resp.Request.URL.String()
			}
			host := hostOfURL(failURL)
			kind := fmt.Sprintf("HTTP %d", resp.StatusCode)
			statusText := resp.Status
			resp.Body.Close()
			cancel()
			retryable := resp.StatusCode >= 500 && resp.StatusCode <= 599
			log.Printf("stream-sync: ota download %s (attempt %d/%d) failed [%s]: HTTP %s", sanitizeLogURL(failURL), attempt, maxAttempts, kind, statusText)
			if retryable && attempt < maxAttempts {
				continue
			}
			return formatDownloadFailure(host, kind)
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			resp.Body.Close()
			cancel()
			log.Printf("stream-sync: ota download cannot open %s: %v", dest, err)
			if dest == stagedUpdateFile+".part" {
				return "cannot write /data/az01-update.img"
			}
			return "disk write failed"
		}
		var written int64
		var readErr error
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				written += int64(n)
				if maxBytes > 0 && written > maxBytes {
					out.Close()
					resp.Body.Close()
					cancel()
					_ = os.Remove(dest)
					log.Printf("stream-sync: ota download %s exceeded limit (%d bytes)", sanitizeLogURL(rawURL), maxBytes)
					return "update download too large"
				}
				if _, werr := out.Write(buf[:n]); werr != nil {
					out.Close()
					resp.Body.Close()
					cancel()
					_ = os.Remove(dest)
					log.Printf("stream-sync: ota download write %s failed: %v", dest, werr)
					return "disk write failed"
				}
				if onProgress != nil && resp.ContentLength > 0 {
					onProgress(written, resp.ContentLength)
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				readErr = rerr
				break
			}
		}
		if readErr != nil {
			out.Close()
			resp.Body.Close()
			cancel()
			_ = os.Remove(dest)
			failURL := rawURL
			if resp.Request != nil && resp.Request.URL != nil {
				failURL = resp.Request.URL.String()
			}
			host := hostOfURL(failURL)
			kind, retryable := classifyDownloadError(readErr)
			log.Printf("stream-sync: ota download %s (attempt %d/%d) interrupted [%s]: %v", sanitizeLogURL(failURL), attempt, maxAttempts, kind, unwrapURLError(readErr))
			if retryable && attempt < maxAttempts {
				continue
			}
			return formatDownloadFailure(host, kind)
		}
		if syncFile {
			_ = out.Sync()
		}
		cerr := out.Close()
		resp.Body.Close()
		cancel()
		if cerr != nil {
			_ = os.Remove(dest)
			log.Printf("stream-sync: ota download close %s failed: %v", dest, cerr)
			return "disk write failed"
		}
		return ""
	}
	return "update download failed"
}

// downloadBundle fetches url into dest, at most bundleMaxDown bytes; it returns an error message or "".
func (s *Server) downloadBundle(rawURL, dest string) string {
	return s.downloadToFile(rawURL, dest, 0o600, 10*time.Minute, bundleMaxDown, false, func(written, total int64) {
		p := 0.05 + 0.80*float64(written)/float64(total)
		if p > 0.85 {
			p = 0.85
		}
		s.setUpdateProgress(p, "", false)
	})
}

func parseBundleFileName(name string) (prefix, version string, err error) {
	base := filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), `\`, "/"))
	stem, ok := strings.CutSuffix(base, bundleSuffix)
	if !ok {
		return "", "", errors.New("expected <PREFIX>-<VERSION>-EngineX-OS-Data-Bundle.tar.gz")
	}
	prefix, version, ok = strings.Cut(stem, "-")
	if !ok || prefix == "" || !bundleVersionRE.MatchString(version) {
		return "", "", errors.New("expected <PREFIX>-<VERSION>-EngineX-OS-Data-Bundle.tar.gz")
	}
	return prefix, version, nil
}

type bundleUploadReader struct {
	r          io.Reader
	max, total int64
	read       int64
	tooLarge   bool
	onProgress func(float64)
}

func (u *bundleUploadReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	if n > 0 {
		u.read += int64(n)
		if u.max > 0 && u.read > u.max {
			u.tooLarge = true
			return 0, errBundleTooLarge
		}
		if u.onProgress != nil && u.total > 0 {
			prog := 0.05 + 0.80*float64(u.read)/float64(u.total)
			if prog > 0.85 {
				prog = 0.85
			}
			u.onProgress(prog)
		}
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		u.tooLarge = true
		return 0, errBundleTooLarge
	}
	return n, err
}

// handleBundleUpload accepts an authenticated web console upload of a second-generation
// <PREFIX>-<VERSION>-EngineX-OS-Data-Bundle.tar.gz and streams it directly into
// applyDataBundleFromReader with live progress reporting.
func (s *Server) handleBundleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	deckPrefix := imagePrefix(deviceCompatible())
	reject := func(status int, err error) {
		if status != http.StatusRequestEntityTooLarge && r.Body != nil && r.ContentLength > 0 && r.ContentLength <= bundleMaxDown+(1<<20) {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, bundleMaxDown+(1<<20)))
		}
		fail(w, status, err)
	}
	if !dataBundleBoards[deckPrefix] {
		reject(http.StatusBadRequest, errors.New("only second-generation decks accept a Data-Bundle"))
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(10 * time.Minute))
	_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Minute))

	ct := r.Header.Get("Content-Type")
	mediaType := ""
	if ct != "" {
		var err error
		mediaType, _, err = mime.ParseMediaType(ct)
		if err != nil {
			reject(http.StatusBadRequest, errors.New("invalid Content-Type"))
			return
		}
	}

	var (
		fileName  string
		stream    io.Reader
		totalSize = r.ContentLength
	)
	switch mediaType {
	case "multipart/form-data":
		if r.ContentLength > bundleMaxDown+(1<<20) {
			fail(w, http.StatusRequestEntityTooLarge, errBundleTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, bundleMaxDown+(1<<20))
		mr, err := r.MultipartReader()
		if err != nil {
			reject(http.StatusBadRequest, errors.New("invalid multipart upload"))
			return
		}
		for {
			part, perr := mr.NextPart()
			if errors.Is(perr, io.EOF) {
				break
			}
			if perr != nil {
				var maxErr *http.MaxBytesError
				if errors.As(perr, &maxErr) {
					fail(w, http.StatusRequestEntityTooLarge, errBundleTooLarge)
					return
				}
				reject(http.StatusBadRequest, errors.New("invalid multipart upload"))
				return
			}
			if part.FormName() == "file" {
				fileName = part.FileName()
				stream = part
				defer part.Close()
				break
			}
			part.Close()
		}
		if stream == nil {
			reject(http.StatusBadRequest, errors.New("missing file field"))
			return
		}
	case "application/gzip", "application/x-gzip", "application/octet-stream", "":
		if r.ContentLength > bundleMaxDown {
			fail(w, http.StatusRequestEntityTooLarge, errBundleTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, bundleMaxDown+1)
		fileName = r.URL.Query().Get("name")
		if fileName == "" {
			fileName = r.URL.Query().Get("filename")
		}
		if fileName == "" {
			fileName = r.Header.Get("X-File-Name")
		}
		stream = r.Body
	default:
		reject(http.StatusUnsupportedMediaType, errors.New("unsupported content type"))
		return
	}

	filePrefix, _, err := parseBundleFileName(fileName)
	if err != nil {
		reject(http.StatusBadRequest, err)
		return
	}
	if filePrefix != deckPrefix {
		reject(http.StatusBadRequest, fmt.Errorf("bundle is for %s, this deck is %s", filePrefix, deckPrefix))
		return
	}

	s.updateMu.Lock()
	if s.updateCache.Updating {
		s.updateMu.Unlock()
		reject(http.StatusConflict, errors.New("update already in progress"))
		return
	}
	if s.updateCache.Repo == "" {
		s.uiMu.Lock()
		s.updateCache.Repo = s.loadUI().GitHubRepo
		s.uiMu.Unlock()
	}
	if s.updateCache.Current == "" {
		s.updateCache.Current = s.installedVersion()
	}
	if s.updateCache.checked.IsZero() {
		s.updateCache.checked = time.Now()
	}
	s.updateCache.Updating = true
	s.updateCache.Progress = 0.05
	s.updateCache.Error = ""
	s.updateMu.Unlock()

	ur := &bundleUploadReader{
		r:     stream,
		max:   bundleMaxDown,
		total: totalSize,
		onProgress: func(p float64) {
			s.setUpdateProgress(p, "", false)
		},
	}
	s.applyDataBundleFromReader(ur)
	if ur.tooLarge {
		s.setUpdateProgress(0, errBundleTooLarge.Error(), true)
		fail(w, http.StatusRequestEntityTooLarge, errBundleTooLarge)
		return
	}
	s.updateMu.Lock()
	status := s.updateCache
	s.updateMu.Unlock()
	if status.Progress < 1.0 && status.Error != "" {
		if status.Error == errBundleTooLarge.Error() {
			fail(w, http.StatusRequestEntityTooLarge, errBundleTooLarge)
			return
		}
		reject(http.StatusBadRequest, errors.New(status.Error))
		return
	}
	respond(w, status)
}

// unpackBundle extracts a bundle into dir, refusing anything but plain files
// and directories under one top directory, and returns that directory.
func unpackBundle(archive, dir string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", errors.New("invalid data bundle archive")
	}
	defer f.Close()
	return unpackBundleReader(f, dir)
}

func unpackBundleReader(r io.Reader, dir string) (string, error) {
	bad := errors.New("invalid data bundle archive")
	gz, err := gzip.NewReader(r)
	if err != nil {
		if errors.Is(err, errBundleTooLarge) {
			return "", errBundleTooLarge
		}
		return "", bad
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var top string
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, errBundleTooLarge) {
				return "", errBundleTooLarge
			}
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
				if errors.Is(cerr, errBundleTooLarge) {
					return "", errBundleTooLarge
				}
				return "", bad
			}
		default:
			return "", bad
		}
	}
	if cerr := gz.Close(); cerr != nil {
		if errors.Is(cerr, errBundleTooLarge) {
			return "", errBundleTooLarge
		}
		return "", bad
	}
	if _, derr := io.Copy(io.Discard, r); derr != nil {
		if errors.Is(derr, errBundleTooLarge) {
			return "", errBundleTooLarge
		}
		return "", bad
	}
	root := filepath.Join(dir, top)
	for _, need := range []string{"install.sh", "SHA256SUMS"} {
		if st, err := os.Stat(filepath.Join(root, need)); top == "" || err != nil || !st.Mode().IsRegular() {
			return "", bad
		}
	}
	return root, nil
}
