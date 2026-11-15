package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AppleMusicHelperConfig is persisted separately from the browsing account.
type AppleMusicHelperConfig struct {
	URL      string `json:"url"`
	LoginURL string `json:"loginUrl"`
	Token    string `json:"token"`
}

func (a *AppleMusic) HelperConfig() AppleMusicHelperConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AppleMusicHelperConfig{a.HelperURL, a.HelperLoginURL, a.HelperToken}
}

func (a *AppleMusic) SetHelper(helperURL, loginURL, token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.HelperURL, a.HelperLoginURL, a.HelperToken = strings.TrimRight(helperURL, "/"), loginURL, token
}

type AppleMusicHelperStatus struct {
	Configured bool   `json:"configured"`
	Online     bool   `json:"online"`
	SignedIn   bool   `json:"signedIn"`
	LoginState string `json:"loginState"`
	Message    string `json:"message"`
	Error      string `json:"error,omitempty"`
}

// helperCall deliberately returns no upstream bodies or transport URLs in errors.
func (a *AppleMusic) helperCall(ctx context.Context, cfg AppleMusicHelperConfig, method, path string, payload any, out any) error {
	if cfg.URL == "" {
		return errors.New("applemusic: helper is not configured")
	}
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cfg.URL, "/")+path, body)
	if err != nil {
		return errors.New("applemusic: invalid helper URL")
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := *client(a.HTTP)
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New("applemusic: helper unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("applemusic: helper HTTP %d", resp.StatusCode)
	}
	if out != nil && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out) != nil {
		return errors.New("applemusic: invalid helper response")
	}
	return nil
}

func (a *AppleMusic) helperLogin(ctx context.Context, cfg AppleMusicHelperConfig, method, path string, payload any) (AppleMusicHelperStatus, error) {
	var wire struct {
		State      string `json:"state"`
		LoginState string `json:"loginState"`
		SignedIn   bool   `json:"signedIn"`
	}
	status := AppleMusicHelperStatus{Configured: cfg.URL != ""}
	err := a.helperCall(ctx, cfg, method, path, payload, &wire)
	if err != nil {
		status.Error = err.Error()
		return status, err
	}
	status.Online = true
	status.LoginState = wire.State
	if wire.LoginState != "" {
		status.LoginState = wire.LoginState
	}
	status.SignedIn = wire.SignedIn || status.LoginState == "ready"
	// Use an allowlisted message rather than exposing an upstream credential echo.
	switch status.LoginState {
	case "ready":
		status.Message = "Signed in"
	case "2fa":
		status.Message = "Verification code required"
	case "logging-in":
		status.Message = "Signing in"
	default:
		status.Message = "Sign in required"
	}
	return status, nil
}

func (a *AppleMusic) HelperStatus(ctx context.Context) AppleMusicHelperStatus {
	cfg := a.HelperConfig()
	status := AppleMusicHelperStatus{Configured: cfg.URL != ""}
	if !status.Configured {
		return status
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := a.helperCall(ctx, cfg, "GET", "/healthz", nil, nil); err != nil {
		status.Error = err.Error()
		return status
	}
	status, err := a.helperLogin(ctx, cfg, "GET", "/v1/login/status", nil)
	if err != nil {
		status.Online = true
	}
	return status
}

func (a *AppleMusic) HelperLoginBegin(ctx context.Context, username, password string) (AppleMusicHelperStatus, error) {
	if strings.TrimSpace(username) == "" || password == "" {
		return AppleMusicHelperStatus{}, errors.New("applemusic: username and password are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return a.helperLogin(ctx, a.HelperConfig(), "POST", "/v1/login", map[string]string{"username": username, "password": password})
}

func (a *AppleMusic) HelperLogin2FA(ctx context.Context, code string) (AppleMusicHelperStatus, error) {
	if len(code) != 6 || strings.Trim(code, "0123456789") != "" {
		return AppleMusicHelperStatus{}, errors.New("applemusic: enter a six-digit verification code")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return a.helperLogin(ctx, a.HelperConfig(), "POST", "/v1/2fa", map[string]string{"code": code})
}
