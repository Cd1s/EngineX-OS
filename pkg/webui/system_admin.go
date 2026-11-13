package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// SystemRunner executes commands on the host or inside test mocks.
type SystemRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

var defaultSystemRunner SystemRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := guardHostCommand(name); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

var (
	sysRunnerMu sync.RWMutex
	sysRunner   = defaultSystemRunner
)

// SetSystemRunner allows tests or callers to mock system command execution.
func SetSystemRunner(r SystemRunner) {
	sysRunnerMu.Lock()
	defer sysRunnerMu.Unlock()
	if r == nil {
		sysRunner = defaultSystemRunner
	} else {
		sysRunner = r
	}
}

func getSystemRunner() SystemRunner {
	sysRunnerMu.RLock()
	defer sysRunnerMu.RUnlock()
	return sysRunner
}

// --------------------------------------------------------------------------
// SSH Management (#114)
// --------------------------------------------------------------------------

// sshPortDropInPath is a var so tests do not write the host's /etc.
var sshPortDropInPath = "/etc/systemd/system/sshd.service.d/51-port.conf"

// sshReservedPorts are EngineX's own listeners (console, relay, stems).
var sshReservedPorts = []int{8080, 6595, 13579}

type SSHConfig struct {
	Enabled          bool   `json:"enabled"`
	Port             int    `json:"port"`
	Password         string `json:"-"` // never persisted: chpasswd gets it once, HasPassword remembers that one was set
	HasPassword      bool   `json:"hasPassword"`
	HasAuthorizedKey bool   `json:"hasAuthorizedKey"`
}

type SSHStatusView struct {
	Enabled          bool `json:"enabled"`
	Port             int  `json:"port"`
	HasPassword      bool `json:"hasPassword"`
	HasAuthorizedKey bool `json:"hasAuthorizedKey"`
	Running          bool `json:"running"`
}

type sshPostRequest struct {
	Enabled       *bool   `json:"enabled"`
	Port          *int    `json:"port"`
	Password      *string `json:"password"`
	AuthorizedKey *string `json:"authorizedKey,omitempty"`
}

// sshRunDropInPath is the /run fallback for read-only roots (2nd-gen decks); a var for tests.
var sshRunDropInPath = "/run/systemd/system/sshd.service.d/51-port.conf"

// First-generation EngineX images run their own key-only sshd unit (az01_packer.py):
// Conflicts=sshd.service, PasswordAuthentication no, keys in /data/enginex. Starting
// the stock sshd there stopped it (BH-GC2-9). Second-generation decks keep the stock
// sshd.service and its read-only /etc.
const gen1SSHUnit = "sc5000-custom-sshd"

var gen1SSHDropInPath = "/etc/systemd/system/sc5000-custom-sshd.service.d/51-port.conf"

// gen1SSHRunDropInPath: first-generation 5.3.0 roots are mounted read-only too (no etc.mount),
// so the port drop-in goes to /run there; RestoreSSH writes it again on every boot.
var gen1SSHRunDropInPath = "/run/systemd/system/sc5000-custom-sshd.service.d/51-port.conf"

// writeGen1SSHPort writes the first-generation unit's port drop-in: /etc where it is
// writable, /run otherwise (review of 03cf399: EROFS was ignored, the port silently not set).
func writeGen1SSHPort(port int) error {
	// -p overrides the Port of EngineX's own sshd_config.
	dropIn := fmt.Sprintf("[Service]\nExecStart=\nExecStart=/usr/sbin/sshd -D -e -f /usr/share/sc5000-custom/sshd_config -p %d\n", port)
	if os.MkdirAll(filepath.Dir(gen1SSHDropInPath), 0o755) == nil && os.WriteFile(gen1SSHDropInPath, []byte(dropIn), 0o644) == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(gen1SSHRunDropInPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(gen1SSHRunDropInPath, []byte(dropIn), 0o644)
}

// RestoreSSH applies the console's SSH settings on a first-generation deck after a boot:
// its prepare script starts the key-only unit on port 22 whenever a key is stored and never
// reads them, and /run, where the port drop-in and the runtime mask live, is empty after a
// reboot. Second-generation decks do this in boot-activate.sh. Without saved settings
// nothing changes (#46, #116).
func (s *Server) RestoreSSH(ctx context.Context) {
	if !onDeck() {
		return
	}
	unit, _ := sshUnit()
	if unit != gen1SSHUnit {
		return
	}
	if _, err := os.Stat(s.sshConfigPath()); err != nil {
		return
	}
	cfg := s.loadSSHConfig()
	runner := getSystemRunner()
	if !cfg.Enabled {
		_, _ = runner(ctx, "systemctl", "stop", unit)
		_, _ = runner(ctx, "systemctl", "mask", "--runtime", unit)
		return
	}
	if cfg.Port != 22 {
		if err := writeGen1SSHPort(cfg.Port); err != nil {
			log.Printf("ssh: port %d not restored: %v", cfg.Port, err)
			return
		}
		_, _ = runner(ctx, "systemctl", "daemon-reload")
		_, _ = runner(ctx, "systemctl", "restart", unit)
	}
}

// sshUnit is the SSH unit this deck runs, and whether it takes passwords.
func sshUnit() (unit string, passwords bool) {
	if dataBundleBoards[imagePrefix(deviceCompatible())] {
		return "sshd", true
	}
	return gen1SSHUnit, false
}

// sshRunning: only "active" from systemctl is running; inactive, failed or no
// systemctl at all is not (an error used to fall back to "enabled", BH-GC2-9).
func sshRunning(ctx context.Context, runner SystemRunner) bool {
	if !onDeck() {
		return false
	}
	unit, _ := sshUnit()
	out, err := runner(ctx, "systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// sshPersistKeyDir is where authorized_keys survives reboot; falls back to StateDir when absent.
var sshPersistKeyDir = "/data/enginex"

func (s *Server) sshKeyStorePath() string {
	if st, err := os.Stat(sshPersistKeyDir); err == nil && st.IsDir() {
		return filepath.Join(sshPersistKeyDir, "authorized_keys")
	}
	return filepath.Join(s.cfg.StateDir, "authorized_keys")
}

func (s *Server) sshConfigPath() string {
	if s.cfg.StateDir != "" {
		return filepath.Join(s.cfg.StateDir, "ssh-config.json")
	}
	return filepath.Join(s.uiDir(), "ssh-config.json")
}

func (s *Server) loadSSHConfigLocked() SSHConfig {
	cfg := SSHConfig{
		Enabled: true,
		Port:    22,
	}
	if b, err := os.ReadFile(s.sshConfigPath()); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		cfg.Port = 22
	}
	return cfg
}

func (s *Server) loadSSHConfig() SSHConfig {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadSSHConfigLocked()
}

func (s *Server) saveSSHConfigLocked(cfg SSHConfig) error {
	return providers.SaveJSON(s.sshConfigPath(), cfg)
}

// handleUnlockGen2 reports (GET) or sets (POST) the flag that lets a second-generation deck
// take the bundle-carrying *-Update.img through its own updater (#119).
func (s *Server) handleUnlockGen2(w http.ResponseWriter, r *http.Request) {
	prefix := imagePrefix(deviceCompatible())
	gen := 1
	if dataBundleBoards[prefix] {
		gen = 2
	}
	if r.Method == http.MethodPost {
		if gen != 2 {
			fail(w, http.StatusBadRequest, errors.New("only second-generation decks need the .img unlock"))
			return
		}
		dir := s.customDir()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fail(w, http.StatusInternalServerError, errors.New("cannot write "+dir))
			return
		}
		if err := os.WriteFile(filepath.Join(dir, "gen2-unlocked"), []byte("1\n"), 0o644); err != nil {
			fail(w, http.StatusInternalServerError, errors.New("cannot write gen2-unlocked"))
			return
		}
		if err := os.Remove(filepath.Join(dir, "gen2-locked")); err != nil && !errors.Is(err, os.ErrNotExist) {
			fail(w, http.StatusInternalServerError, errors.New("cannot remove gen2-locked"))
			return
		}
		s.updateMu.Lock()
		if !s.updateCache.Updating {
			s.updateCache.checked = time.Time{}
		}
		s.updateMu.Unlock()
		respond(w, map[string]any{"ok": true, "unlocked": s.isGen2Unlocked(), "prefix": prefix})
		return
	}
	respond(w, map[string]any{"unlocked": s.isGen2Unlocked(), "generation": gen, "prefix": prefix})
}

// modelPrefix returns the board prefix for the current hardware (e.g. "SC5000", "SCLIVE4").
func (s *Server) modelPrefix() string {
	return imagePrefix(deviceCompatible())
}

// handleUninstallGen2 removes the EngineX data bundle from a second-generation deck and reverts to stock (#141).
func (s *Server) handleUninstallGen2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	prefix := s.modelPrefix()
	if !dataBundleBoards[prefix] {
		fail(w, http.StatusBadRequest, errors.New("only second-generation decks support uninstall-gen2"))
		return
	}
	customDir := s.customDir()
	uninstallScript := filepath.Join(customDir, "bundle", "uninstall.sh")
	if _, err := os.Stat(uninstallScript); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fail(w, http.StatusNotFound, errors.New("uninstall.sh not found"))
			return
		}
		fail(w, http.StatusInternalServerError, err)
		return
	}
	runner := getSystemRunner()
	if uninstallOnDeck(customDir) {
		// uninstall.sh stops enginex-stream-sync.service, this very process's
		// unit, and systemd then ends every process of that unit: the script
		// halfway (engine.service left pointing at the removed bundle) and the
		// reboot after it (BH-GC2-2). It runs in a unit of its own instead,
		// which reboots the deck when the script succeeds.
		if out, err := runner(r.Context(), "systemd-run", "--unit=enginex-uninstall-gen2", "--collect", "--no-block", "--quiet",
			"/bin/sh", "-c", `sh "$0" && { systemctl reboot || reboot; }`, uninstallScript); err != nil {
			fail(w, http.StatusInternalServerError, fmt.Errorf("uninstall could not start: %w: %s", err, out))
			return
		}
		respond(w, map[string]any{"ok": true, "prefix": prefix, "rebooting": true})
		return
	}
	// Off a deck (a developer machine, a test): run it here and never reboot.
	if out, err := runner(r.Context(), "sh", uninstallScript); err != nil {
		fail(w, http.StatusInternalServerError, fmt.Errorf("uninstall failed: %w: %s", err, out))
		return
	}
	respond(w, map[string]any{"ok": true, "prefix": prefix})
}

// uninstallOnDeck: the deck's own /data/enginex (or the legacy path).
var uninstallOnDeck = isDeckDataDir

func (s *Server) handleSSH(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleSSHGet(w, r)
	case http.MethodPost:
		s.handleSSHPost(w, r)
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleSSHGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.loadSSHConfig()
	running := sshRunning(r.Context(), getSystemRunner())

	respond(w, SSHStatusView{
		Enabled:          cfg.Enabled,
		Port:             cfg.Port,
		HasPassword:      cfg.HasPassword,
		HasAuthorizedKey: cfg.HasAuthorizedKey,
		Running:          running,
	})
}

func (s *Server) handleSSHPost(w http.ResponseWriter, r *http.Request) {
	var in sshPostRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		return
	}

	if in.Port != nil {
		if *in.Port < 1 || *in.Port > 65535 {
			fail(w, http.StatusBadRequest, errors.New("invalid port: must be between 1 and 65535"))
			return
		}
	}

	if in.Port != nil && slices.Contains(sshReservedPorts, *in.Port) {
		fail(w, http.StatusBadRequest, errors.New("invalid port: reserved by EngineX services"))
		return
	}
	setPassword := in.Password != nil && *in.Password != "" && *in.Password != "***"
	if setPassword && strings.ContainsAny(*in.Password, "\r\n\x00") {
		fail(w, http.StatusBadRequest, errors.New("invalid password: line breaks and NUL are not allowed"))
		return
	}

	setKey := in.AuthorizedKey != nil && strings.TrimSpace(*in.AuthorizedKey) != ""
	if setKey && strings.ContainsRune(*in.AuthorizedKey, 0) {
		fail(w, http.StatusBadRequest, errors.New("invalid public key: NUL is not allowed"))
		return
	}

	s.uiMu.Lock()
	defer s.uiMu.Unlock()

	cfg := s.loadSSHConfigLocked()
	runner := getSystemRunner()
	// Off a deck (a developer machine, a build host running tests) the settings
	// are only saved: never a drop-in for that machine's sshd, never a command (r3).
	deck := onDeck()
	unit, passwords := sshUnit()
	if setPassword && deck && !passwords {
		fail(w, http.StatusBadRequest, errors.New("this deck's SSH takes public keys only: add a public key instead of a password"))
		return
	}

	if setKey {
		store := s.sshKeyStorePath()
		_ = os.MkdirAll(filepath.Dir(store), 0o700)
		if err := os.WriteFile(store, []byte(strings.TrimSpace(*in.AuthorizedKey)+"\n"), 0o600); err != nil {
			fail(w, http.StatusInternalServerError, errors.New("could not save public key"))
			return
		}
		// Same as boot-activate.sh: copy, else bind-mount over the read-only /root.
		if deck {
			_, _ = runner(r.Context(), "sh", "-c", `{ mkdir -p -m 700 /root/.ssh && cp "$1" /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys; } 2>/dev/null || mount --bind "$1" /root/.ssh/authorized_keys 2>/dev/null || :`, "sh", store)
		}
		cfg.HasAuthorizedKey = true
	}

	if in.Port != nil {
		cfg.Port = *in.Port
	}
	if in.Port != nil && deck && unit == gen1SSHUnit {
		if err := writeGen1SSHPort(cfg.Port); err != nil {
			fail(w, http.StatusInternalServerError, errors.New("could not set the ssh port"))
			return
		}
		_, _ = runner(r.Context(), "systemctl", "daemon-reload")
	} else if in.Port != nil && deck {
		// Apply port configuration drop-in if directory exists or via runner
		portDropIn := fmt.Sprintf("[Service]\nExecStart=\nExecStart=/usr/sbin/sshd -D -e -p %d\n", cfg.Port)
		_ = os.MkdirAll(filepath.Dir(sshPortDropInPath), 0o755)
		if err := os.WriteFile(sshPortDropInPath, []byte(portDropIn), 0o644); err != nil {
			// read-only /etc (2nd-gen decks): /run is honoured by systemd until reboot
			_ = os.MkdirAll(filepath.Dir(sshRunDropInPath), 0o755)
			_ = os.WriteFile(sshRunDropInPath, []byte(portDropIn), 0o644)
		}
		_, _ = runner(r.Context(), "systemctl", "daemon-reload") // the drop-in is ignored until reloaded
	}

	if setPassword && deck {
		// The password is a positional parameter, so the shell never expands it.
		if _, err := runner(r.Context(), "sh", "-c", `printf 'root:%s\n' "$1" | chpasswd`, "sh", *in.Password); err != nil {
			fail(w, http.StatusInternalServerError, errors.New("could not set root password"))
			return
		}
		cfg.HasPassword = true
	}

	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.Enabled != nil && deck {
		if cfg.Enabled {
			if unit == gen1SSHUnit { // its mask is a runtime one (read-only /etc), see below
				_, _ = runner(r.Context(), "systemctl", "unmask", "--runtime", unit)
			}
			_, _ = runner(r.Context(), "systemctl", "unmask", unit)
			_, _ = runner(r.Context(), "systemctl", "restart", unit)
		} else {
			_, _ = runner(r.Context(), "systemctl", "stop", unit)
			if unit == gen1SSHUnit { // the prepare script's target would start it again
				_, _ = runner(r.Context(), "systemctl", "mask", "--runtime", unit)
			}
		}
	} else if in.Port != nil && cfg.Enabled && deck {
		_, _ = runner(r.Context(), "systemctl", "restart", unit)
	} else if setKey && cfg.Enabled && deck {
		// A first key lets the key-only unit start (ConditionFileNotEmpty); it
		// was saved but SSH never came up until the next boot (BH-GC2-9).
		_, _ = runner(r.Context(), "systemctl", "start", unit)
	}

	if err := s.saveSSHConfigLocked(cfg); err != nil {
		fail(w, http.StatusInternalServerError, errors.New("could not save ssh configuration"))
		return
	}

	respond(w, SSHStatusView{
		Enabled:          cfg.Enabled,
		Port:             cfg.Port,
		HasPassword:      cfg.HasPassword,
		HasAuthorizedKey: cfg.HasAuthorizedKey,
		Running:          sshRunning(r.Context(), runner),
	})
}

// --------------------------------------------------------------------------
// Remote Mounts Management (#114)
// --------------------------------------------------------------------------

type RemoteMount struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"` // "sftp", "smb", "webdav"
	Host       string `json:"host"`
	Port       int    `json:"port"`
	RemotePath string `json:"remotePath"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	MountPoint string `json:"mountPoint"`
	Mounted    bool   `json:"mounted"`
	Enabled    bool   `json:"enabled"`
	Options    string `json:"options,omitempty"`
}

type RemoteMountView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	RemotePath  string `json:"remotePath"`
	Username    string `json:"username,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	MountPoint  string `json:"mountPoint"`
	Mounted     bool   `json:"mounted"`
	Enabled     bool   `json:"enabled"`
	Options     string `json:"options,omitempty"`
}

func (s *Server) mountsConfigPath() string {
	if s.cfg.StateDir != "" {
		return filepath.Join(s.cfg.StateDir, "mounts.json")
	}
	return filepath.Join(s.uiDir(), "mounts.json")
}

func (s *Server) loadMountsLocked() []RemoteMount {
	var list []RemoteMount
	if b, err := os.ReadFile(s.mountsConfigPath()); err == nil {
		_ = json.Unmarshal(b, &list)
	}
	return list
}

func (s *Server) loadMounts() []RemoteMount {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadMountsLocked()
}

func (s *Server) saveMountsLocked(list []RemoteMount) error {
	return providers.SaveJSON(s.mountsConfigPath(), list)
}

func toMountView(m RemoteMount) RemoteMountView {
	return RemoteMountView{
		ID:          m.ID,
		Name:        m.Name,
		Protocol:    m.Protocol,
		Host:        m.Host,
		Port:        m.Port,
		RemotePath:  m.RemotePath,
		Username:    m.Username,
		HasPassword: m.Password != "",
		MountPoint:  m.MountPoint,
		Mounted:     m.Mounted,
		Enabled:     m.Enabled,
		Options:     m.Options,
	}
}

func (s *Server) mountRemote(ctx context.Context, m *RemoteMount) error {
	runner := getSystemRunner()
	_ = os.MkdirAll(m.MountPoint, 0o755)

	switch strings.ToLower(m.Protocol) {
	case "sftp", "sshfs":
		portStr := "22"
		if m.Port > 0 {
			portStr = strconv.Itoa(m.Port)
		}
		target := fmt.Sprintf("%s:%s", m.Host, m.RemotePath)
		if m.Username != "" {
			target = fmt.Sprintf("%s@%s:%s", m.Username, m.Host, m.RemotePath)
		}
		args := []string{"-p", portStr, target, m.MountPoint, "-o", "reconnect,ServerAliveInterval=15"}
		if _, err := runner(ctx, "sshfs", args...); err != nil {
			return err
		}
	case "smb", "cifs":
		share := fmt.Sprintf("//%s/%s", m.Host, strings.TrimPrefix(m.RemotePath, "/"))
		opts := []string{"ro"}
		if m.Port > 0 {
			opts = append(opts, fmt.Sprintf("port=%d", m.Port))
		}
		if m.Username != "" {
			opts = append(opts, fmt.Sprintf("username=%s", m.Username))
		}
		if m.Password != "" {
			opts = append(opts, fmt.Sprintf("password=%s", m.Password))
		} else {
			opts = append(opts, "guest")
		}
		args := []string{"-t", "cifs", share, m.MountPoint, "-o", strings.Join(opts, ",")}
		if _, err := runner(ctx, "mount", args...); err != nil {
			return err
		}
	case "webdav":
		scheme := "http"
		if m.Port == 443 || strings.HasPrefix(m.Host, "https://") {
			scheme = "https"
		}
		cleanHost := strings.TrimPrefix(strings.TrimPrefix(m.Host, "http://"), "https://")
		urlStr := fmt.Sprintf("%s://%s", scheme, cleanHost)
		if m.Port > 0 && m.Port != 80 && m.Port != 443 && !strings.Contains(cleanHost, ":") {
			urlStr = fmt.Sprintf("%s://%s:%d", scheme, cleanHost, m.Port)
		}
		urlStr = fmt.Sprintf("%s/%s", strings.TrimSuffix(urlStr, "/"), strings.TrimPrefix(m.RemotePath, "/"))
		args := []string{"-t", "davfs", urlStr, m.MountPoint}
		if m.Username != "" && m.Password != "" {
			args = append(args, "-o", fmt.Sprintf("username=%s,password=%s", m.Username, m.Password))
		}
		if _, err := runner(ctx, "mount", args...); err != nil {
			return err
		}
	}
	m.Mounted = true
	return nil
}

func (s *Server) unmountRemote(ctx context.Context, m *RemoteMount) error {
	runner := getSystemRunner()
	_, err := runner(ctx, "umount", m.MountPoint)
	if err == nil {
		m.Mounted = false
	}
	return err
}

func (s *Server) syncMountToNAS(m RemoteMount) {
	if strings.ToLower(m.Protocol) == "webdav" {
		scheme := "http"
		if m.Port == 443 || strings.HasPrefix(m.Host, "https://") {
			scheme = "https"
		}
		cleanHost := strings.TrimPrefix(strings.TrimPrefix(m.Host, "http://"), "https://")
		urlStr := fmt.Sprintf("%s://%s", scheme, cleanHost)
		if m.Port > 0 && m.Port != 80 && m.Port != 443 && !strings.Contains(cleanHost, ":") {
			urlStr = fmt.Sprintf("%s://%s:%d", scheme, cleanHost, m.Port)
		}
		urlStr = fmt.Sprintf("%s/%s", strings.TrimSuffix(urlStr, "/"), strings.TrimPrefix(m.RemotePath, "/"))
		nasCfg := providers.NASConfig{
			Protocol: "webdav",
			URL:      urlStr,
			Username: m.Username,
			Password: m.Password,
			Enabled:  m.Enabled,
		}
		_ = providers.SaveJSON(s.nasConfigPath(), nasCfg)
	}
}

func (s *Server) handleMounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleMountsGet(w, r)
	case http.MethodPost:
		s.handleMountsPost(w, r)
	case http.MethodDelete:
		s.handleMountsDelete(w, r)
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleMountsGet(w http.ResponseWriter, r *http.Request) {
	list := s.loadMounts()
	views := make([]RemoteMountView, 0, len(list))
	for _, m := range list {
		// What is mounted now, not the flag saved when it was: nothing remounts a share after a
		// reboot, and a mount can drop (BH-GC2-14).
		m.Mounted = mountedAt(m.MountPoint)
		views = append(views, toMountView(m))
	}
	respond(w, views)
}

// mountedAt reports whether path is a mount point now. A var for the tests.
var mountedAt = func(path string) bool {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false
	}
	escaped := strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(filepath.Clean(path))
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[1] == escaped {
			return true
		}
	}
	return false
}

func (s *Server) handleMountsPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Protocol   string `json:"protocol"`
		Host       string `json:"host"`
		Port       int    `json:"port"`
		RemotePath string `json:"remotePath"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		MountPoint string `json:"mountPoint"`
		Enabled    *bool  `json:"enabled"`
		Options    string `json:"options"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		return
	}

	proto := strings.ToLower(strings.TrimSpace(in.Protocol))
	if proto == "sshfs" {
		proto = "sftp"
	}
	if proto == "cifs" {
		proto = "smb"
	}
	if proto != "sftp" && proto != "smb" && proto != "webdav" {
		fail(w, http.StatusBadRequest, errors.New("invalid protocol: must be sftp, smb, or webdav"))
		return
	}

	host := strings.TrimSpace(in.Host)
	if host == "" {
		fail(w, http.StatusBadRequest, errors.New("host cannot be empty"))
		return
	}

	port := in.Port
	if port < 0 || port > 65535 {
		fail(w, http.StatusBadRequest, errors.New("invalid port: must be between 1 and 65535"))
		return
	}
	if port == 0 {
		switch proto {
		case "sftp":
			port = 22
		case "smb":
			port = 445
		case "webdav":
			if strings.HasPrefix(host, "https://") {
				port = 443
			} else {
				port = 80
			}
		}
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = fmt.Sprintf("%s-%s", proto, strings.ReplaceAll(host, ".", "-"))
	}

	mountPoint := strings.TrimSpace(in.MountPoint)
	if mountPoint == "" {
		mountPoint = filepath.Join(s.mediaRoot(), name)
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	s.uiMu.Lock()
	defer s.uiMu.Unlock()

	list := s.loadMountsLocked()
	id := in.ID
	if id == "" {
		id = providers.RandomID()
	}

	// Check if already exists
	var existingIdx = -1
	for i, m := range list {
		if m.ID == id || m.Name == name {
			existingIdx = i
			id = m.ID
			break
		}
	}

	pass := in.Password
	if (pass == "" || pass == "***") && existingIdx >= 0 {
		pass = list[existingIdx].Password
	}

	m := RemoteMount{
		ID:         id,
		Name:       name,
		Protocol:   proto,
		Host:       host,
		Port:       port,
		RemotePath: strings.TrimSpace(in.RemotePath),
		Username:   strings.TrimSpace(in.Username),
		Password:   pass,
		MountPoint: mountPoint,
		Enabled:    enabled,
		Options:    strings.TrimSpace(in.Options),
	}

	// BH-GC2-14: a share switched off is unmounted, a changed one is mounted afresh, and a mount
	// that fails is reported (the settings are kept) instead of answering 200 with mounted: true.
	if existingIdx >= 0 && mountedAt(list[existingIdx].MountPoint) {
		old := list[existingIdx]
		if err := s.unmountRemote(r.Context(), &old); err != nil && !enabled {
			log.Printf("webui: unmount %s: %v", old.MountPoint, err)
		}
	}
	var mountErr error
	if enabled {
		mountErr = s.mountRemote(r.Context(), &m)
	}

	if existingIdx >= 0 {
		list[existingIdx] = m
	} else {
		list = append(list, m)
	}

	if err := s.saveMountsLocked(list); err != nil {
		fail(w, http.StatusInternalServerError, errors.New("could not save mounts configuration"))
		return
	}

	// Synchronize to OmniSource NAS list
	s.syncMountToNAS(m)

	if mountErr != nil {
		fail(w, http.StatusBadGateway, fmt.Errorf("saved, but mounting %s failed: %w", m.Name, mountErr))
		return
	}
	respond(w, toMountView(m))
}

func (s *Server) handleMountsDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	if id == "" && r.Body != nil {
		var in struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
		_ = json.NewDecoder(r.Body).Decode(&in)
		id = in.ID
		if id == "" {
			id = in.Name
		}
	}
	if id == "" {
		fail(w, http.StatusBadRequest, errors.New("mount id is required"))
		return
	}

	s.uiMu.Lock()
	defer s.uiMu.Unlock()

	list := s.loadMountsLocked()
	found := -1
	for i, m := range list {
		if m.ID == id || m.Name == id {
			found = i
			break
		}
	}
	if found < 0 {
		fail(w, http.StatusNotFound, errors.New("mount not found"))
		return
	}

	target := list[found]
	_ = s.unmountRemote(r.Context(), &target)
	list = append(list[:found], list[found+1:]...)

	if err := s.saveMountsLocked(list); err != nil {
		fail(w, http.StatusInternalServerError, errors.New("could not save mounts configuration"))
		return
	}

	// Drop the OmniSource NAS credentials that belonged to the deleted WebDAV mount.
	if strings.ToLower(target.Protocol) == "webdav" {
		var left *RemoteMount
		for i := range list {
			if strings.ToLower(list[i].Protocol) == "webdav" {
				left = &list[i]
			}
		}
		if left != nil {
			s.syncMountToNAS(*left)
		} else {
			_ = os.Remove(s.nasConfigPath())
		}
	}

	respond(w, map[string]any{"ok": true, "deleted": target.ID})
}

// --------------------------------------------------------------------------
// Automix State Machine (#114)
// --------------------------------------------------------------------------

type AutomixTrack struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Artist      string  `json:"artist,omitempty"`
	Album       string  `json:"album,omitempty"`
	DurationSec float64 `json:"durationSec"`
	Path        string  `json:"path,omitempty"`
	BPM         float64 `json:"bpm,omitempty"`
}

type AutomixStatus struct {
	Enabled      bool           `json:"enabled"`
	FadeSeconds  int            `json:"fadeSeconds"`
	State        string         `json:"state"` // "idle", "playing", "fading", "stopped"
	CurrentDeck  int            `json:"currentDeck"`
	CurrentIndex int            `json:"currentIndex"`
	CurrentTrack *AutomixTrack  `json:"currentTrack,omitempty"`
	NextTrack    *AutomixTrack  `json:"nextTrack,omitempty"`
	Crossfade    float64        `json:"crossfade"` // 0.0 (Deck 1) .. 1.0 (Deck 2)
	PositionSec  float64        `json:"positionSec"`
	Tracks       []AutomixTrack `json:"tracks"`
}

type AutomixManager struct {
	mu           sync.Mutex
	filePath     string
	enabled      bool
	fadeSeconds  int
	state        string
	currentDeck  int
	currentIndex int
	positionSec  float64
	crossfade    float64
	tracks       []AutomixTrack
}

func newAutomixManager(path string) *AutomixManager {
	mgr := &AutomixManager{
		filePath:    path,
		enabled:     false,
		fadeSeconds: 10,
		state:       "idle",
		currentDeck: 1,
		crossfade:   0.0,
		tracks:      []AutomixTrack{},
	}
	if b, err := os.ReadFile(path); err == nil {
		var saved struct {
			Enabled     bool           `json:"enabled"`
			FadeSeconds int            `json:"fadeSeconds"`
			Tracks      []AutomixTrack `json:"tracks"`
		}
		if err := json.Unmarshal(b, &saved); err == nil {
			mgr.enabled = saved.Enabled
			if saved.FadeSeconds >= 4 && saved.FadeSeconds <= 30 {
				mgr.fadeSeconds = saved.FadeSeconds
			}
			mgr.tracks = saved.Tracks
			if len(mgr.tracks) > 0 && mgr.enabled {
				mgr.state = "playing"
			}
		}
	}
	return mgr
}

func (m *AutomixManager) saveLocked() {
	if m.filePath == "" {
		return
	}
	saved := struct {
		Enabled     bool           `json:"enabled"`
		FadeSeconds int            `json:"fadeSeconds"`
		Tracks      []AutomixTrack `json:"tracks"`
	}{
		Enabled:     m.enabled,
		FadeSeconds: m.fadeSeconds,
		Tracks:      m.tracks,
	}
	_ = providers.SaveJSON(m.filePath, saved)
}

func (m *AutomixManager) Status() AutomixStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

func (m *AutomixManager) statusLocked() AutomixStatus {
	var cur, next *AutomixTrack
	if len(m.tracks) > 0 {
		idx := m.currentIndex % len(m.tracks)
		cur = &m.tracks[idx]
		if len(m.tracks) > 1 {
			nextIdx := (idx + 1) % len(m.tracks)
			next = &m.tracks[nextIdx]
		}
	}
	return AutomixStatus{
		Enabled:      m.enabled,
		FadeSeconds:  m.fadeSeconds,
		State:        m.state,
		CurrentDeck:  m.currentDeck,
		CurrentIndex: m.currentIndex,
		CurrentTrack: cur,
		NextTrack:    next,
		Crossfade:    m.crossfade,
		PositionSec:  m.positionSec,
		Tracks:       m.tracks,
	}
}

func (m *AutomixManager) SetEnabled(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled = enabled
	if !enabled {
		m.state = "stopped"
	} else if len(m.tracks) > 0 {
		m.state = "playing"
	} else {
		m.state = "idle"
	}
	m.saveLocked()
}

func (m *AutomixManager) SetFadeSeconds(sec int) error {
	if sec < 4 || sec > 30 {
		return errors.New("fadeSeconds must be between 4 and 30")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fadeSeconds = sec
	m.saveLocked()
	return nil
}

func (m *AutomixManager) SetTracks(tracks []AutomixTrack) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tracks = tracks
	if m.currentIndex >= len(tracks) {
		m.currentIndex = 0
	}
	if len(tracks) == 0 {
		m.state = "idle"
	} else if m.enabled && m.state == "idle" {
		m.state = "playing"
	}
	m.saveLocked()
}

// Next transitions to the next track smoothly, switching decks.
func (m *AutomixManager) Next() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tracks) == 0 {
		return
	}
	m.currentIndex = (m.currentIndex + 1) % len(m.tracks)
	if m.currentDeck == 1 {
		m.currentDeck = 2
		m.crossfade = 1.0
	} else {
		m.currentDeck = 1
		m.crossfade = 0.0
	}
	m.positionSec = 0
	if m.enabled {
		m.state = "playing"
	}
}

// Tick advances the Automix playback clock by dt seconds and handles automated crossfades.
func (m *AutomixManager) Tick(dt float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled || len(m.tracks) == 0 || m.state == "stopped" {
		return
	}
	m.positionSec += dt
	cur := m.tracks[m.currentIndex%len(m.tracks)]
	if cur.DurationSec <= 0 {
		return
	}
	remaining := cur.DurationSec - m.positionSec
	fadeSec := float64(m.fadeSeconds)
	if remaining <= fadeSec && remaining > 0 {
		m.state = "fading"
		progress := (fadeSec - remaining) / fadeSec
		if m.currentDeck == 1 {
			m.crossfade = progress // 0.0 -> 1.0 (towards Deck 2)
		} else {
			m.crossfade = 1.0 - progress // 1.0 -> 0.0 (towards Deck 1)
		}
	} else if remaining <= 0 {
		// Completed song: transition to next track
		m.currentIndex = (m.currentIndex + 1) % len(m.tracks)
		if m.currentDeck == 1 {
			m.currentDeck = 2
			m.crossfade = 1.0
		} else {
			m.currentDeck = 1
			m.crossfade = 0.0
		}
		m.positionSec = 0
		m.state = "playing"
	}
}

func (s *Server) automixConfigPath() string {
	if s.cfg.StateDir != "" {
		return filepath.Join(s.cfg.StateDir, "automix.json")
	}
	return filepath.Join(s.uiDir(), "automix.json")
}

func (s *Server) handleAutomix(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAutomixGet(w, r)
	case http.MethodPost:
		s.handleAutomixPost(w, r)
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleAutomixGet(w http.ResponseWriter, r *http.Request) {
	if s.automix == nil {
		s.automix = newAutomixManager(s.automixConfigPath())
	}
	respond(w, s.automix.Status())
}

func (s *Server) handleAutomixPost(w http.ResponseWriter, r *http.Request) {
	if s.automix == nil {
		s.automix = newAutomixManager(s.automixConfigPath())
	}

	var in struct {
		Enabled     *bool          `json:"enabled"`
		FadeSeconds *int           `json:"fadeSeconds"`
		Tracks      []AutomixTrack `json:"tracks"`
		Action      string         `json:"action"` // "next", "stop", "play"
		Next        *bool          `json:"next"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		return
	}

	if in.FadeSeconds != nil {
		if err := s.automix.SetFadeSeconds(*in.FadeSeconds); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	}

	if in.Tracks != nil {
		s.automix.SetTracks(in.Tracks)
	}

	if in.Enabled != nil {
		s.automix.SetEnabled(*in.Enabled)
	}

	if strings.EqualFold(in.Action, "next") || (in.Next != nil && *in.Next) {
		s.automix.Next()
	}

	respond(w, s.automix.Status())
}
