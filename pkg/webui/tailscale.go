package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// Tailscale / Headscale client (#130).
//
// The deck kernels have no TUN device and signed modules, so the client runs in
// userspace-networking mode: no interface, no kernel routes. Other tailnet nodes
// reach the deck only through the TCP ports forwarded with "tailscale serve".
// The binary is a trimmed upstream build (tools/tailscale/build.sh, ~4 MB with UPX)
// downloaded on demand, so the firmware image does not grow.

const (
	tailscaleVersion = "1.104.5"
	tailscaleUnit    = "enginex-tailscaled"
	tailscaleSocket  = "/run/enginex-tailscaled.sock"
	tailscaleWebPort = 8080 // stream-sync console
	// legacyTailscaleUnit is the transient unit's name before 5.3.0.5 (#234). An in-place
	// update of a /data bundle keeps it running, and a second daemon on the same state dir
	// would fight it, so it is stopped before this one starts.
	legacyTailscaleUnit = "sc5000-tailscaled"
)

// legacyTailscaleSocket points at the daemon's socket under its name before 5.3.0.5 (#234),
// for a Tailscale Remote Mod installed earlier that still asks there; tests move it.
var legacyTailscaleSocket = "/run/sc5000-tailscaled.sock"

// tailscaleReleaseURL hosts tailscaled-<arch> and SHA256SUMS; tests point it at httptest.
var tailscaleReleaseURL = "https://github.com/Cd1s/EngineX-OS/releases/download/tailscale-v" + tailscaleVersion

// tailscaleGOARCH is runtime.GOARCH; tests change it.
var tailscaleGOARCH = runtime.GOARCH

var (
	tailscaleMu sync.Mutex
	// tailscaleServed: the forwards were applied since the daemon last left "Running".
	tailscaleServed atomic.Bool
	tailscaleHost   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	tailscaleKey    = regexp.MustCompile(`^[A-Za-z0-9_-]{8,256}$`)
)

type TailscaleForward struct {
	SSH   bool `json:"ssh"`
	WebUI bool `json:"webui"`
}

type TailscaleConfig struct {
	Enabled     bool             `json:"enabled"`
	LoginServer string           `json:"loginServer"` // empty means Tailscale's own control server
	Hostname    string           `json:"hostname"`
	Forward     TailscaleForward `json:"forward"`
}

type TailscaleStatusView struct {
	Installed   bool             `json:"installed"`
	Supported   bool             `json:"supported"`
	Version     string           `json:"version"`
	Enabled     bool             `json:"enabled"`
	Running     bool             `json:"running"`
	State       string           `json:"state"` // Stopped, NeedsLogin, Running, ...
	LoginServer string           `json:"loginServer"`
	Hostname    string           `json:"hostname"`
	AuthURL     string           `json:"authUrl,omitempty"`
	DeviceName  string           `json:"deviceName,omitempty"`
	IPs         []string         `json:"ips,omitempty"`
	Forward     TailscaleForward `json:"forward"`
}

type tailscalePost struct {
	Enabled     *bool             `json:"enabled"`
	LoginServer *string           `json:"loginServer"`
	Hostname    *string           `json:"hostname"`
	Forward     *TailscaleForward `json:"forward"`
	AuthKey     string            `json:"authKey"` // used once for "up", never stored
	Action      string            `json:"action"`  // install | login | logout
}

type tailscaleStatusJSON struct {
	Version      string
	BackendState string
	AuthURL      string
	Self         struct {
		HostName     string
		TailscaleIPs []string
	}
}

func (s *Server) tailscaleRoot() string {
	if s.cfg.StateDir != "" {
		return filepath.Join(s.cfg.StateDir, "tailscale")
	}
	return "/data/enginex/tailscale"
}

func (s *Server) tailscaleBin() string { return filepath.Join(s.tailscaleRoot(), "bin", "tailscaled") }

// tailscaleCLI is the same box binary under the name "tailscale" (it picks its role from argv[0]).
func (s *Server) tailscaleCLI() string { return filepath.Join(s.tailscaleRoot(), "bin", "tailscale") }

func tailscaleArch() (string, bool) {
	switch tailscaleGOARCH {
	case "arm":
		return "armhf", true
	case "arm64":
		return "arm64", true
	}
	return "", false
}

func (s *Server) loadTailscaleConfig() TailscaleConfig {
	var cfg TailscaleConfig
	if b, err := os.ReadFile(filepath.Join(s.tailscaleRoot(), "config.json")); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	return cfg
}

func (s *Server) saveTailscaleConfig(cfg TailscaleConfig) error {
	if err := os.MkdirAll(s.tailscaleRoot(), 0o700); err != nil {
		return err
	}
	return providers.SaveJSON(filepath.Join(s.tailscaleRoot(), "config.json"), cfg)
}

func (s *Server) tailscaleInstalled() bool {
	st, err := os.Stat(s.tailscaleBin())
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

func (s *Server) tailscaleCmd(ctx context.Context, args ...string) ([]byte, error) {
	args = append([]string{"--socket=" + tailscaleSocket}, args...)
	return getSystemRunner()(ctx, s.tailscaleCLI(), args...)
}

func (s *Server) tailscaleDaemonActive(ctx context.Context) bool {
	out, err := getSystemRunner()(ctx, "systemctl", "is-active", tailscaleUnit)
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

func (s *Server) tailscaleStart(ctx context.Context) error {
	if s.tailscaleDaemonActive(ctx) {
		return nil
	}
	run := getSystemRunner()
	_, _ = run(ctx, "systemctl", "stop", legacyTailscaleUnit)
	_, _ = run(ctx, "systemctl", "reset-failed", tailscaleUnit)
	// A transient unit works on both the image and the read-only-root /data bundle decks.
	out, err := run(ctx, "systemd-run", "--unit="+tailscaleUnit, "--slice=enginex-background.slice",
		"--property=Nice=19", "--property=MemoryMax=128M", "--property=Restart=on-failure",
		"--setenv=GOMEMLIMIT=64MiB", s.tailscaleBin(),
		"--tun=userspace-networking", "--statedir="+filepath.Join(s.tailscaleRoot(), "state"),
		"--socket="+tailscaleSocket, "--port=0")
	if err != nil {
		return fmt.Errorf("start tailscaled: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if isDeckStateDir(s.cfg.StateDir) { // never the build host's /run in tests
		linkLegacyTailscaleSocket()
	}
	return nil
}

// linkLegacyTailscaleSocket points the old socket name at the new one, unless something
// that is not a symlink (an old daemon's own socket) is there.
func linkLegacyTailscaleSocket() {
	if fi, err := os.Lstat(legacyTailscaleSocket); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return
		}
		os.Remove(legacyTailscaleSocket)
	}
	_ = os.Symlink(filepath.Base(tailscaleSocket), legacyTailscaleSocket)
}

func (s *Server) tailscaleStop(ctx context.Context) {
	tailscaleServed.Store(false)
	_, _ = getSystemRunner()(ctx, "systemctl", "stop", tailscaleUnit)
	_, _ = getSystemRunner()(ctx, "systemctl", "stop", legacyTailscaleUnit)
}

// tailscaleStatus asks the running daemon; a failed query means "not running".
func (s *Server) tailscaleStatus(ctx context.Context) (tailscaleStatusJSON, bool) {
	var st tailscaleStatusJSON
	out, err := s.tailscaleCmd(ctx, "status", "--json")
	if err != nil || json.Unmarshal(out, &st) != nil {
		return st, false
	}
	return st, true
}

func (s *Server) tailscaleView(ctx context.Context) TailscaleStatusView {
	cfg := s.loadTailscaleConfig()
	_, supported := tailscaleArch()
	v := TailscaleStatusView{
		Supported: supported, Installed: s.tailscaleInstalled(), Enabled: cfg.Enabled,
		LoginServer: cfg.LoginServer, Hostname: cfg.Hostname, Forward: cfg.Forward, State: "Stopped",
	}
	if v.Installed {
		v.Version = tailscaleVersion
		if st, ok := s.tailscaleStatus(ctx); ok {
			v.Running, v.State, v.AuthURL = true, st.BackendState, st.AuthURL
			v.DeviceName, v.IPs = st.Self.HostName, st.Self.TailscaleIPs
		}
	}
	return v
}

// tailscaleAutoServe applies the forwards once the browser login completes, so
// they work without a reboot. The status poll drives it; a POST in flight serves itself.
func (s *Server) tailscaleAutoServe(ctx context.Context, v TailscaleStatusView) {
	if v.State != "Running" {
		tailscaleServed.Store(false)
		return
	}
	cfg := s.loadTailscaleConfig()
	if !cfg.Enabled || !(cfg.Forward.SSH || cfg.Forward.WebUI) || tailscaleServed.Load() || !tailscaleMu.TryLock() {
		return
	}
	defer tailscaleMu.Unlock()
	if !tailscaleServed.Load() {
		s.tailscaleServe(ctx, cfg)
	}
}

func validLoginServer(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || strings.ContainsAny(raw, " \t\r\n\"'") {
		return errors.New("invalid login server: use https://host[:port]")
	}
	return nil
}

// tailscaleServe replaces the forwards with exactly the ticked services.
func (s *Server) tailscaleServe(ctx context.Context, cfg TailscaleConfig) {
	tailscaleServed.Store(true)
	_, _ = s.tailscaleCmd(ctx, "serve", "reset")
	ports := []int{}
	if cfg.Forward.SSH {
		ports = append(ports, s.loadSSHConfig().Port)
	}
	if cfg.Forward.WebUI {
		ports = append(ports, tailscaleWebPort)
	}
	for _, p := range ports {
		n, target := strconv.Itoa(p), "127.0.0.1:"+strconv.Itoa(p)
		if p == tailscaleWebPort {
			target = ForwardAddr // the console without the deck-local gateway
		}
		_, _ = s.tailscaleCmd(ctx, "serve", "--bg", "--tcp="+n, "tcp://"+target)
	}
}

func (s *Server) tailscaleInstall(ctx context.Context) error {
	arch, ok := tailscaleArch()
	if !ok {
		return errors.New("unsupported CPU architecture")
	}
	client := s.cfg.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	get := func(name string, limit int64) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, tailscaleReleaseURL+"/"+name, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil || int64(len(b)) > limit {
			return nil, fmt.Errorf("download %s failed or too large", name)
		}
		return b, nil
	}
	sums, err := get("SHA256SUMS", 64<<10)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == "tailscaled-"+arch {
			want = strings.ToLower(f[0])
		}
	}
	if len(want) != 64 {
		return errors.New("SHA256SUMS has no entry for this CPU")
	}
	bin, err := get("tailscaled-"+arch, 32<<20)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(bin); hex.EncodeToString(sum[:]) != want {
		return errors.New("checksum mismatch, download discarded")
	}
	dir := filepath.Join(s.tailscaleRoot(), "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".tailscaled.new")
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.tailscaleBin()); err != nil {
		return err
	}
	_ = os.Remove(s.tailscaleCLI())
	return os.Symlink("tailscaled", s.tailscaleCLI())
}

func (s *Server) tailscaleLogin(ctx context.Context, cfg TailscaleConfig, authKey string) error {
	args := []string{"up", "--accept-dns=false", "--accept-routes=false", "--timeout=5s"}
	if cfg.LoginServer != "" {
		args = append(args, "--login-server="+cfg.LoginServer)
	}
	if cfg.Hostname != "" {
		args = append(args, "--hostname="+cfg.Hostname)
	}
	if authKey != "" {
		// A 0600 file keeps the key out of the process list; it is removed right after.
		f, err := os.CreateTemp(s.tailscaleRoot(), ".authkey-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			return err
		}
		_, werr := f.WriteString(authKey)
		if cerr := f.Close(); werr != nil || cerr != nil {
			return errors.New("could not write auth key")
		}
		args = append(args, "--auth-key=file:"+f.Name())
	}
	// "up" waits for the login; the timeout ends the wait, the daemon keeps the pending login URL.
	_, _ = s.tailscaleCmd(ctx, args...)
	return nil
}

func (s *Server) handleTailscale(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		v := s.tailscaleView(r.Context())
		s.tailscaleAutoServe(r.Context(), v)
		respond(w, v)
	case http.MethodPost:
		s.handleTailscalePost(w, r)
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) handleTailscalePost(w http.ResponseWriter, r *http.Request) {
	var in tailscalePost
	if !input(w, r, &in) {
		return
	}
	if in.LoginServer != nil {
		if err := validLoginServer(*in.LoginServer); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	}
	if in.Hostname != nil && *in.Hostname != "" && !tailscaleHost.MatchString(*in.Hostname) {
		fail(w, http.StatusBadRequest, errors.New("invalid device name: letters, digits and '-' only"))
		return
	}
	if in.AuthKey != "" && !tailscaleKey.MatchString(in.AuthKey) {
		fail(w, http.StatusBadRequest, errors.New("invalid auth key"))
		return
	}
	switch in.Action {
	case "", "install", "login", "logout":
	default:
		fail(w, http.StatusBadRequest, errors.New("unknown action"))
		return
	}

	tailscaleMu.Lock()
	defer tailscaleMu.Unlock()
	ctx := r.Context()
	cfg := s.loadTailscaleConfig()
	if in.LoginServer != nil {
		cfg.LoginServer = strings.TrimRight(*in.LoginServer, "/")
	}
	if in.Hostname != nil {
		cfg.Hostname = *in.Hostname
	}
	if in.Forward != nil {
		cfg.Forward = *in.Forward
	}

	if in.Action == "install" {
		if err := s.tailscaleInstall(ctx); err != nil {
			fail(w, http.StatusBadGateway, err)
			return
		}
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if cfg.Enabled && !s.tailscaleInstalled() {
		fail(w, http.StatusConflict, errors.New("Tailscale is not installed yet: use action \"install\" first"))
		return
	}
	if err := s.saveTailscaleConfig(cfg); err != nil {
		fail(w, http.StatusInternalServerError, errors.New("could not save Tailscale configuration"))
		return
	}

	switch {
	case !cfg.Enabled:
		s.tailscaleStop(ctx)
	default:
		if err := s.tailscaleStart(ctx); err != nil {
			fail(w, http.StatusBadGateway, err)
			return
		}
		switch in.Action {
		case "login":
			if err := s.tailscaleLogin(ctx, cfg, in.AuthKey); err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
		case "logout":
			_, _ = s.tailscaleCmd(ctx, "logout")
		}
		if v := s.tailscaleView(ctx); v.State == "Running" {
			s.tailscaleServe(ctx, cfg)
		}
	}
	respond(w, s.tailscaleView(ctx))
}

// tailscaleResume restarts the daemon after a reboot when the user left it on.
func (s *Server) tailscaleResume() {
	cfg := s.loadTailscaleConfig()
	if !cfg.Enabled || !s.tailscaleInstalled() {
		return
	}
	tailscaleMu.Lock()
	defer tailscaleMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.tailscaleStart(ctx); err != nil {
		return
	}
	for i := 0; i < 20; i++ { // the daemon needs a moment to reach "Running"
		if v := s.tailscaleView(ctx); v.State == "Running" {
			s.tailscaleServe(ctx, cfg)
			return
		}
		time.Sleep(time.Second)
	}
}
