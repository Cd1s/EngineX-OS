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
	trimmed := strings.TrimRight(helperURL, "/")
	if trimmed == "" || a.HelperURL != trimmed || a.HelperToken != token {
		a.ownerForgot = false
		a.forgottenToken = ""
		a.helperOnline = false
		a.helperSignedIn = false
		a.helperDevToken = ""
		a.helperProbedAt = time.Time{}
	}
	a.HelperURL, a.HelperLoginURL, a.HelperToken = trimmed, loginURL, token
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
	a.mu.Lock()
	a.helperOnline = true
	if method == "POST" || !status.SignedIn {
		a.ownerForgot = false
		a.forgottenToken = ""
	}
	forgot := a.ownerForgot
	forgotTok := a.forgottenToken
	a.helperSignedIn = status.SignedIn && !forgot
	if !status.SignedIn || forgot {
		a.helperDevToken = ""
	}
	a.mu.Unlock()
	if status.SignedIn {
		if !forgot || forgotTok != "" {
			a.syncHelperCredentials(ctx, cfg)
		}
		a.mu.Lock()
		stillForgot := a.ownerForgot
		if !stillForgot {
			a.helperSignedIn = true
		}
		a.mu.Unlock()
		if stillForgot {
			status.SignedIn = false
			status.LoginState = "signed-out"
		}
	}
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

// syncHelperCredentials pulls the signed-in account's media-user-token,
// developer token, and storefront from enginex-amd when supported. Older
// helpers without /v1/login/credentials are ignored without error.
func (a *AppleMusic) syncHelperCredentials(ctx context.Context, cfg AppleMusicHelperConfig) {
	a.mu.Lock()
	if a.ownerForgot && a.forgottenToken == "" {
		a.mu.Unlock()
		return
	}
	startGen := a.credGen
	a.mu.Unlock()

	var creds struct {
		SignedIn       bool   `json:"signedIn"`
		MediaUserToken string `json:"mediaUserToken"`
		DeveloperToken string `json:"developerToken"`
		Storefront     string `json:"storefront"`
	}
	if err := a.helperCall(ctx, cfg, "GET", "/v1/login/credentials", nil, &creds); err != nil || !creds.SignedIn {
		return
	}
	userToken := strings.TrimSpace(creds.MediaUserToken)
	devToken := strings.TrimSpace(creds.DeveloperToken)
	storefront := strings.ToLower(strings.TrimSpace(creds.Storefront))
	if !amStore.MatchString(storefront) {
		storefront = ""
	}
	var save bool
	var savedGen uint64
	var owner AppleMusicOwner
	var ownerPath string

	a.mu.Lock()
	if a.credGen != startGen {
		a.mu.Unlock()
		return
	}
	if a.ownerForgot {
		if userToken == "" || userToken == a.forgottenToken {
			a.mu.Unlock()
			return
		}
		a.ownerForgot = false
		a.forgottenToken = ""
	}
	if devToken != "" && devToken != a.helperDevToken {
		if a.web != "" && a.web == a.helperDevToken {
			a.web = ""
			a.webAt = time.Time{}
		}
		a.helperDevToken = devToken
	}
	if userToken != "" && userToken != a.owner.UserToken {
		a.owner.UserToken = userToken
		save = true
	}
	if storefront != "" && storefront != a.owner.Storefront {
		a.owner.Storefront = storefront
		save = true
	}
	if save {
		a.credGen++
		savedGen = a.credGen
		owner = a.owner
		owner.Links = append([]string(nil), a.owner.Links...)
		ownerPath = a.OwnerPath
	}
	a.mu.Unlock()

	if save && ownerPath != "" {
		a.saveMu.Lock()
		a.mu.Lock()
		valid := !a.ownerForgot && a.credGen == savedGen
		a.mu.Unlock()
		if valid {
			_ = SaveJSON(ownerPath, owner)
		}
		a.saveMu.Unlock()
	}
}

// ProbeHelper checks helper status at most once per cooldown when unready so an
// offline helper does not stall every gateway browse or search request.
func (a *AppleMusic) ProbeHelper(ctx context.Context, cooldown time.Duration) AppleMusicHelperStatus {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	a.mu.Lock()
	configured := a.HelperURL != ""
	last := a.helperProbedAt
	online := a.helperOnline
	signedIn := a.helperSignedIn
	effective := cooldown
	if online && effective > 2*time.Second {
		effective = 2 * time.Second
	}
	if configured && (last.IsZero() || time.Since(last) >= effective) {
		a.helperProbedAt = time.Now()
		a.mu.Unlock()
		return a.HelperStatus(ctx)
	}
	a.mu.Unlock()
	return AppleMusicHelperStatus{Configured: configured, Online: online, SignedIn: signedIn}
}

func (a *AppleMusic) HelperStatus(ctx context.Context) AppleMusicHelperStatus {
	cfg := a.HelperConfig()
	status := AppleMusicHelperStatus{Configured: cfg.URL != ""}
	if !status.Configured {
		a.mu.Lock()
		a.helperOnline = false
		a.helperSignedIn = false
		a.helperDevToken = ""
		a.mu.Unlock()
		return status
	}
	a.mu.Lock()
	a.helperProbedAt = time.Now()
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := a.helperCall(ctx, cfg, "GET", "/healthz", nil, nil); err != nil {
		a.mu.Lock()
		a.helperOnline = false
		a.helperSignedIn = false
		a.mu.Unlock()
		status.Error = err.Error()
		return status
	}
	status, err := a.helperLogin(ctx, cfg, "GET", "/v1/login/status", nil)
	if err != nil {
		a.mu.Lock()
		a.helperOnline = true
		a.mu.Unlock()
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
