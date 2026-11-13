package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	relayTryCloudflareRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

	// relayCloudflaredDownloadBaseURL points to official cloudflared releases; tests override it.
	relayCloudflaredDownloadBaseURL = "https://github.com/cloudflare/cloudflared/releases/latest/download"

	// relayCloudflaredGOARCH defaults to runtime.GOARCH; tests may override it.
	relayCloudflaredGOARCH = runtime.GOARCH

	// relayCloudflaredSearchPaths lists candidate locations checked before PATH.
	relayCloudflaredSearchPaths = []string{
		"/data/enginex/bin/cloudflared",
		"/data/sc5000-custom/bin/cloudflared",
		"/usr/local/bin/cloudflared",
	}

	// relayCloudflaredLookPath defaults to exec.LookPath; tests may override it.
	relayCloudflaredLookPath = exec.LookPath
)

func (s *server) setPublicURL(raw string) {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	s.publicMu.Lock()
	s.public = u
	if strings.HasSuffix(u, ".trycloudflare.com") {
		s.tunnelURL = u
	}
	if s.spotify != nil {
		s.spotify.mu.Lock()
		if u != "" {
			s.spotify.httpsRedirect = u + "/oauth/spotify/callback"
		} else {
			s.spotify.httpsRedirect = ""
		}
		s.spotify.mu.Unlock()
	}
	s.publicMu.Unlock()
}

func (s *server) publicURL() string {
	s.publicMu.RLock()
	defer s.publicMu.RUnlock()
	return s.public
}

func (s *server) spotifyHTTPSRedirect() string {
	s.publicMu.RLock()
	defer s.publicMu.RUnlock()
	if s.spotify == nil {
		return ""
	}
	s.spotify.mu.Lock()
	defer s.spotify.mu.Unlock()
	return s.spotify.httpsRedirect
}

func (s *server) quickTunnelURL() string {
	s.publicMu.RLock()
	defer s.publicMu.RUnlock()
	if s.tunnelURL != "" {
		return s.tunnelURL
	}
	if strings.HasSuffix(s.public, ".trycloudflare.com") {
		return s.public
	}
	return ""
}

func relayListenLocalURL(listenAddr string) string {
	listenAddr = strings.TrimSpace(listenAddr)
	if strings.HasPrefix(listenAddr, "http://") || strings.HasPrefix(listenAddr, "https://") {
		return listenAddr
	}
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" || port == "0" {
		port = "6595"
	}
	return "http://127.0.0.1:" + port
}

func relayIsExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

func findOrInstallCloudflared(ctx context.Context, stateDir string) (string, error) {
	if stateDir != "" {
		cand := filepath.Join(stateDir, "bin", "cloudflared")
		if relayIsExecutable(cand) {
			return cand, nil
		}
	}
	for _, p := range relayCloudflaredSearchPaths {
		if relayIsExecutable(p) {
			return p, nil
		}
	}
	if p, err := relayCloudflaredLookPath("cloudflared"); err == nil && relayIsExecutable(p) {
		return p, nil
	}

	var asset string
	switch relayCloudflaredGOARCH {
	case "arm":
		asset = "cloudflared-linux-arm"
	case "arm64":
		asset = "cloudflared-linux-arm64"
	case "amd64":
		asset = "cloudflared-linux-amd64"
	default:
		return "", fmt.Errorf("unsupported CPU architecture for cloudflared: %s", relayCloudflaredGOARCH)
	}

	if stateDir == "" {
		stateDir = os.TempDir()
	}
	dst := filepath.Join(stateDir, "bin", "cloudflared")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}

	dlURL := strings.TrimRight(relayCloudflaredDownloadBaseURL, "/") + "/" + asset
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return "", err
	}
	hc := &http.Client{Timeout: 10 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", asset, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", asset, resp.StatusCode)
	}

	tmp := filepath.Join(filepath.Dir(dst), fmt.Sprintf(".cloudflared.%d.tmp", time.Now().UnixNano()))
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

// startQuickTunnel launches cloudflared Quick Tunnel for listenAddr, downloading the binary
// to <stateDir>/bin/cloudflared if needed, and returns the assigned https://*.trycloudflare.com URL.
func startQuickTunnel(ctx context.Context, listenAddr, stateDir string, onURL ...func(string)) (string, error) {
	bin, err := findOrInstallCloudflared(ctx, stateDir)
	if err != nil {
		return "", err
	}
	localURL := relayListenLocalURL(listenAddr)
	cmd := exec.CommandContext(ctx, bin, "tunnel", "--no-autoupdate", "--url", localURL)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return "", err
	}

	go func() {
		_ = cmd.Wait()
		_ = pw.Close()
	}()

	type result struct {
		url string
		err error
	}
	resCh := make(chan result, 1)
	var once sync.Once
	sendRes := func(u string, e error) {
		once.Do(func() {
			resCh <- result{url: u, err: e}
		})
	}

	go func() {
		defer pr.Close()
		var lastLine string
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				lastLine = line
			}
			if u := relayTryCloudflareRe.FindString(line); u != "" {
				for _, fn := range onURL {
					if fn != nil {
						fn(u)
					}
				}
				sendRes(u, nil)
			}
		}
		if lastLine != "" {
			sendRes("", errors.New(lastLine))
		} else {
			sendRes("", errors.New("cloudflared exited without producing a trycloudflare.com URL"))
		}
	}()

	select {
	case r := <-resCh:
		return r.url, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
