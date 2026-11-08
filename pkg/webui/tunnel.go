package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

var (
	tryCloudflareRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

	// cloudflaredDownloadBaseURL points to official GitHub releases; tests override it with httptest.
	cloudflaredDownloadBaseURL = "https://github.com/cloudflare/cloudflared/releases/latest/download"

	// cloudflaredGOARCH defaults to runtime.GOARCH; tests may override it.
	cloudflaredGOARCH = runtime.GOARCH

	// cloudflaredSearchPaths lists candidate locations checked before PATH.
	cloudflaredSearchPaths = []string{
		"/data/enginex/bin/cloudflared",
		"/data/sc5000-custom/bin/cloudflared",
		"/usr/local/bin/cloudflared",
	}

	// cloudflaredLookPath is exec.LookPath; tests may override it.
	cloudflaredLookPath = exec.LookPath

	// cloudflaredInstallPath overrides the default destination when non-empty.
	cloudflaredInstallPath string

	tunnelManagers         sync.Map // map[*Server]*TunnelManager
	tunnelManagerMu        sync.Mutex
	tunnelRoutesRegistered sync.Map // map[*Server]bool
)

// TunnelConfig is persisted to <StateDir>/tunnel-config.json.
type TunnelConfig struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token,omitempty"`
}

// TunnelStatus is returned by GET/POST /api/tunnel.
type TunnelStatus struct {
	Enabled    bool   `json:"enabled"`
	Status     string `json:"status"` // running|starting|installing|stopped|error
	PublicURL  string `json:"publicUrl"`
	AccessCode string `json:"accessCode"`
	HasToken   bool   `json:"hasToken,omitempty"`
	Error      string `json:"error"`
}

// TunnelManager manages the cloudflared Quick Tunnel or Named Tunnel process.
type TunnelManager struct {
	mu         sync.Mutex
	stateDir   string
	localURL   string
	httpClient *http.Client

	enabled   bool
	token     string
	status    string
	publicURL string
	lastErr   string

	cancel  context.CancelFunc
	gen     uint64
	readyCh chan struct{}
}

// NewTunnelManager creates a TunnelManager backed by stateDir.
func NewTunnelManager(stateDir, localURL string, httpClient *http.Client) *TunnelManager {
	if localURL == "" {
		localURL = "http://127.0.0.1:8080"
	}
	m := &TunnelManager{
		stateDir:   stateDir,
		localURL:   localURL,
		httpClient: httpClient,
		status:     "stopped",
	}
	cfg := m.loadConfig()
	m.enabled = cfg.Enabled
	m.token = cfg.Token
	return m
}

func (m *TunnelManager) configPath() string {
	if m.stateDir == "" {
		return ""
	}
	return filepath.Join(m.stateDir, "tunnel-config.json")
}

func (m *TunnelManager) loadConfig() TunnelConfig {
	var cfg TunnelConfig
	if p := m.configPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &cfg)
		}
	}
	return cfg
}

func (m *TunnelManager) saveConfigLocked() error {
	p := m.configPath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return providers.SaveJSON(p, TunnelConfig{
		Enabled: m.enabled,
		Token:   m.token,
	})
}

func cloudflaredAssetName(arch string) (string, error) {
	switch arch {
	case "arm":
		return "cloudflared-linux-arm", nil
	case "arm64":
		return "cloudflared-linux-arm64", nil
	case "amd64":
		return "cloudflared-linux-amd64", nil
	default:
		return "", fmt.Errorf("unsupported CPU architecture for cloudflared: %s", arch)
	}
}

// InstallPath returns the destination path where on-demand cloudflared is downloaded.
func (m *TunnelManager) InstallPath() string {
	if cloudflaredInstallPath != "" {
		return cloudflaredInstallPath
	}
	if m.stateDir == "" || strings.HasPrefix(m.stateDir, "/data/") {
		return "/data/sc5000-custom/bin/cloudflared"
	}
	return filepath.Join(m.stateDir, "bin", "cloudflared")
}

func isExecutableFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

func (m *TunnelManager) findBinary() (string, bool) {
	for _, p := range cloudflaredSearchPaths {
		if isExecutableFile(p) {
			return p, true
		}
	}
	if m.stateDir != "" {
		cand := filepath.Join(m.stateDir, "bin", "cloudflared")
		if isExecutableFile(cand) {
			return cand, true
		}
	}
	if cloudflaredInstallPath != "" && isExecutableFile(cloudflaredInstallPath) {
		return cloudflaredInstallPath, true
	}
	if p, err := cloudflaredLookPath("cloudflared"); err == nil && isExecutableFile(p) {
		return p, true
	}
	return "", false
}

func (m *TunnelManager) installBinary(ctx context.Context) (string, error) {
	asset, err := cloudflaredAssetName(cloudflaredGOARCH)
	if err != nil {
		return "", err
	}
	hc := m.httpClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	dlURL := strings.TrimRight(cloudflaredDownloadBaseURL, "/") + "/" + asset
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", asset, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", asset, resp.StatusCode)
	}
	dst := m.InstallPath()
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if m.stateDir != "" && dst != filepath.Join(m.stateDir, "bin", "cloudflared") {
			dst = filepath.Join(m.stateDir, "bin", "cloudflared")
			dir = filepath.Dir(dst)
			if err2 := os.MkdirAll(dir, 0o755); err2 != nil {
				return "", err2
			}
		} else {
			return "", err
		}
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".cloudflared.%d.tmp", time.Now().UnixNano()))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(f, io.LimitReader(resp.Body, 128<<20))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return "", closeErr
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

func validTunnelToken(tok string) error {
	if tok == "" {
		return nil
	}
	if strings.HasPrefix(tok, "-") || strings.ContainsAny(tok, " \t\r\n\"'`") {
		return errors.New("invalid cloudflare tunnel token")
	}
	return nil
}

// Configure updates the tunnel state, starting or stopping cloudflared as needed.
func (m *TunnelManager) Configure(enabled bool, token *string) error {
	m.mu.Lock()
	nextTok := m.token
	if token != nil {
		nextTok = strings.TrimSpace(*token)
	}
	if err := validTunnelToken(nextTok); err != nil {
		m.mu.Unlock()
		return err
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.gen++
	myGen := m.gen
	m.enabled = enabled
	m.token = nextTok
	m.publicURL = ""
	m.lastErr = ""

	if !enabled {
		m.status = "stopped"
		err := m.saveConfigLocked()
		m.mu.Unlock()
		return err
	}

	if _, ok := m.findBinary(); ok {
		m.status = "starting"
	} else {
		m.status = "installing"
	}
	if err := m.saveConfigLocked(); err != nil {
		m.status = "error"
		m.lastErr = err.Error()
		m.mu.Unlock()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	ready := make(chan struct{})
	m.readyCh = ready
	m.mu.Unlock()

	go m.runLoop(ctx, myGen, nextTok, ready)

	select {
	case <-ready:
	case <-time.After(300 * time.Millisecond):
	}
	return nil
}

// Stop terminates any running cloudflared process without erasing the saved config file.
func (m *TunnelManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.gen++
	m.status = "stopped"
	m.publicURL = ""
}

func (m *TunnelManager) runLoop(ctx context.Context, myGen uint64, token string, ready chan struct{}) {
	var once sync.Once
	markReady := func() { once.Do(func() { close(ready) }) }
	defer markReady()

	bin, ok := m.findBinary()
	if !ok {
		m.mu.Lock()
		if m.gen == myGen {
			m.status = "installing"
		}
		m.mu.Unlock()
		var err error
		bin, err = m.installBinary(ctx)
		if err != nil {
			m.mu.Lock()
			if m.gen == myGen {
				m.status = "error"
				m.lastErr = err.Error()
			}
			m.mu.Unlock()
			return
		}
	}

	m.mu.Lock()
	if m.gen != myGen || ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	m.status = "starting"
	localURL := m.localURL
	if localURL == "" {
		localURL = "http://127.0.0.1:8080"
	}
	m.mu.Unlock()

	var args []string
	if token != "" {
		args = []string{"tunnel", "--no-autoupdate", "run", "--token", token}
	} else {
		args = []string{"tunnel", "--no-autoupdate", "--url", localURL}
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		m.mu.Lock()
		if m.gen == myGen {
			m.status = "error"
			m.lastErr = err.Error()
		}
		m.mu.Unlock()
		return
	}

	waitCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = pw.Close()
		waitCh <- err
	}()

	var lastLine string
	scanner := bufio.NewScanner(pr)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lastLine = line
		}
		if u := tryCloudflareRe.FindString(line); u != "" {
			m.mu.Lock()
			if m.gen == myGen {
				m.publicURL = u
				m.status = "running"
				m.lastErr = ""
			}
			m.mu.Unlock()
			markReady()
		} else if token != "" && (strings.Contains(line, "Registered tunnel connection") ||
			(strings.Contains(line, "Connection") && strings.Contains(line, "registered")) ||
			strings.Contains(line, "Updated to new configuration")) {
			m.mu.Lock()
			if m.gen == myGen {
				m.status = "running"
				m.lastErr = ""
			}
			m.mu.Unlock()
			markReady()
		}
	}
	_ = pr.Close()
	err := <-waitCh

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != myGen {
		return
	}
	if ctx.Err() != nil || !m.enabled {
		m.status = "stopped"
		m.publicURL = ""
		return
	}
	if err == nil && m.status == "running" {
		return
	}
	m.status = "error"
	m.publicURL = ""
	if lastLine != "" {
		m.lastErr = lastLine
	} else if err != nil {
		m.lastErr = err.Error()
	} else {
		m.lastErr = "cloudflared exited unexpectedly"
	}
}

// Status returns a snapshot of the tunnel status.
func (m *TunnelManager) Status(accessCode string) TunnelStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status
	if st == "" {
		st = "stopped"
	}
	return TunnelStatus{
		Enabled:    m.enabled,
		Status:     st,
		PublicURL:  m.publicURL,
		AccessCode: accessCode,
		HasToken:   m.token != "",
		Error:      m.lastErr,
	}
}

// PublicURL returns the active https://*.trycloudflare.com URL when running, or "".
func (m *TunnelManager) PublicURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enabled && m.status == "running" && m.publicURL != "" {
		return m.publicURL
	}
	return ""
}

func safeRegisterRoute(mux *http.ServeMux, pattern string, handler http.HandlerFunc) {
	if mux == nil {
		return
	}
	defer func() { _ = recover() }()
	mux.HandleFunc(pattern, handler)
}

// RegisterTunnelRoutes registers /api/tunnel and /v1/sc5000/tunnel on s.
func (s *Server) RegisterTunnelRoutes() {
	if s == nil {
		return
	}
	if _, loaded := tunnelRoutesRegistered.LoadOrStore(s, true); loaded {
		return
	}
	safeRegisterRoute(s.mux, "GET /api/tunnel", s.handleTunnel)
	safeRegisterRoute(s.mux, "POST /api/tunnel", s.handleTunnel)
	safeRegisterRoute(s.mux, "GET /v1/sc5000/tunnel", s.handleTunnel)
	safeRegisterRoute(s.mux, "POST /v1/sc5000/tunnel", s.handleTunnel)
	if s.tidal != nil && s.tidal.mux != nil {
		safeRegisterRoute(s.tidal.mux, "GET /v1/sc5000/tunnel", s.handleTunnel)
		safeRegisterRoute(s.tidal.mux, "POST /v1/sc5000/tunnel", s.handleTunnel)
	}
}

// TunnelManager returns the lazily initialized TunnelManager for this Server.
func (s *Server) TunnelManager() *TunnelManager {
	if v, ok := tunnelManagers.Load(s); ok {
		return v.(*TunnelManager)
	}
	tunnelManagerMu.Lock()
	defer tunnelManagerMu.Unlock()
	if v, ok := tunnelManagers.Load(s); ok {
		return v.(*TunnelManager)
	}
	mgr := NewTunnelManager(s.cfg.StateDir, "http://127.0.0.1:8080", s.cfg.HTTP)
	tunnelManagers.Store(s, mgr)
	return mgr
}

func (s *Server) tunnelManager() *TunnelManager {
	return s.TunnelManager()
}

// tunnelResume restarts the tunnel after a reboot if it was left enabled.
func (s *Server) tunnelResume() {
	mgr := s.TunnelManager()
	cfg := mgr.loadConfig()
	if cfg.Enabled {
		_ = mgr.Configure(true, &cfg.Token)
	}
}

// PublicTunnelURL returns the active https://*.trycloudflare.com public URL when running.
func (s *Server) PublicTunnelURL() string {
	return s.TunnelManager().PublicURL()
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.tunnelGet(w, r)
	case http.MethodPost:
		s.tunnelPost(w, r)
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) tunnelGet(w http.ResponseWriter, r *http.Request) {
	mgr := s.TunnelManager()
	respond(w, mgr.Status(s.cfg.AccessToken))
}

func (s *Server) tunnelPost(w http.ResponseWriter, r *http.Request) {
	mgr := s.TunnelManager()
	var req struct {
		Enabled *bool   `json:"enabled"`
		Token   *string `json:"token"`
	}
	if !input(w, r, &req) {
		return
	}
	enabled := mgr.Status(s.cfg.AccessToken).Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if err := mgr.Configure(enabled, req.Token); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	respond(w, mgr.Status(s.cfg.AccessToken))
}
