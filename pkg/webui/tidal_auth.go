package webui

// Real TIDAL credentials live independently of Engine's shared gateway account.
// Engine may replace that account with local tokens when another source opens.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Cd1s/EngineX-OS/internal/migration"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type tidalAuth struct {
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	UserID       any    `json:"userId,omitempty"`
	CountryCode  string `json:"countryCode,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
}

func isSyntheticTidalToken(tok string) bool {
	switch strings.TrimSpace(tok) {
	case "", "enginex", "enginex-token", "enginex-refresh", migration.Token, migration.RefreshToken:
		return true
	}
	return false
}

func (g *tidalGateway) authPath() string {
	if g.s.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(g.s.cfg.StateDir, "tidal-auth.json")
}

// Caller holds authMu. Read on demand so a restart needs no credential migration.
func (g *tidalGateway) readAuth() tidalAuth {
	var a tidalAuth
	b, _ := os.ReadFile(g.authPath())
	_ = json.Unmarshal(b, &a)
	return a
}
func (g *tidalGateway) realAuth() tidalAuth {
	g.authMu.Lock()
	defer g.authMu.Unlock()
	return g.readAuth()
}
func (g *tidalGateway) hasRealAuth() bool {
	a := g.realAuth()
	return !isSyntheticTidalToken(a.RefreshToken) || !isSyntheticTidalToken(a.AccessToken)
}
func (g *tidalGateway) clearRealAuth() error {
	g.authMu.Lock()
	defer g.authMu.Unlock()
	err := os.Remove(g.authPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (g *tidalGateway) saveRealAuth(in tidalAuth) error {
	return g.updateRealAuth(in, false)
}

// Captured Engine requests may fill missing credentials, but cannot overwrite a
// rotated token. Make that decision under authMu together with the disk update.
func (g *tidalGateway) updateRealAuth(in tidalAuth, fillOnly bool) error {
	g.authMu.Lock()
	defer g.authMu.Unlock()
	a := g.readAuth()
	if fillOnly {
		if !isSyntheticTidalToken(a.AccessToken) {
			in.AccessToken = ""
		}
		if !isSyntheticTidalToken(a.RefreshToken) {
			in.RefreshToken = ""
		}
		if a.ClientID != "" {
			in.ClientID = ""
		}
		if isSyntheticTidalToken(in.AccessToken) && isSyntheticTidalToken(in.RefreshToken) && in.ClientID == "" {
			return nil
		}
	}
	if !isSyntheticTidalToken(in.AccessToken) {
		a.AccessToken = in.AccessToken
	}
	if !isSyntheticTidalToken(in.RefreshToken) {
		a.RefreshToken = in.RefreshToken
	}
	if in.UserID != nil {
		a.UserID = in.UserID
	}
	if in.CountryCode != "" {
		a.CountryCode = in.CountryCode
	}
	if in.ClientID != "" {
		a.ClientID = in.ClientID
	}
	if g.authPath() == "" {
		return errors.New("TIDAL state directory unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(g.authPath()), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(g.authPath()), ".tidal-auth-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), g.authPath()) // CreateTemp uses 0600, including replacements.
}

var (
	errTidalAuthInvalid = errors.New("TIDAL login expired")
	errTidalTransient   = errors.New("temporary TIDAL connection error")
)

const tidalRetryBackoff = 150 * time.Millisecond

func tidalNotLoggedIn(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	io.WriteString(w, `{"status":401,"subStatus":6001,"userMessage":"Not logged in"}`)
}

func tidalTemporarilyUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	io.WriteString(w, `{"status":503,"userMessage":"Temporary TIDAL connection error"}`)
}

// isTidalSessionLogout401 reports whether a TIDAL 401 response body will drive
// Engine's TidalAPI::handleErrors into StreamingLogoutEvent -> onLoggedOut ->
// unloadFromProvider (subStatus 6001, 6002, 11001, 11002, 11003, 11101, or
// absent/empty). Non-zero subStatuses outside that set (such as 4005 track
// restrictions) do not log out the session and should pass through untouched.
func isTidalSessionLogout401(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	var v struct {
		SubStatus int `json:"subStatus"`
	}
	if json.Unmarshal(body, &v) != nil || v.SubStatus == 0 {
		return true
	}
	switch v.SubStatus {
	case 6001, 6002, 11001, 11002, 11003, 11101:
		return true
	}
	return false
}

// isSafePreConnectError reports transport errors that definitely occurred before
// any bytes reached the upstream server, making a retry safe even on rotating
// refresh_token grants.
func isSafePreConnectError(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func isTransientUpstreamStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func isTransientOAuthError(code string) bool {
	return code == "temporarily_unavailable" || code == "server_error"
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Capture only the Engine streaming boundary, never console credentials. Keep
// the form body intact for ReverseProxy and reject unbounded token requests.
func (g *tidalGateway) captureAuth(r *http.Request, clean string) (url.Values, error) {
	form := url.Values{}
	if strings.HasSuffix(clean, "/oauth2/token") && r.Method == http.MethodPost {
		b, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		if err != nil || len(b) > 65536 {
			return nil, errors.New("invalid TIDAL token request")
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
		form, err = url.ParseQuery(string(b))
		if err != nil {
			return nil, errors.New("invalid TIDAL token request")
		}
	}
	a := tidalAuth{RefreshToken: form.Get("refresh_token"), ClientID: form.Get("client_id")}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		a.AccessToken = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if !isSyntheticTidalToken(a.AccessToken) || !isSyntheticTidalToken(a.RefreshToken) {
		if err := g.updateRealAuth(a, true); err != nil {
			return nil, errors.New("could not save TIDAL login")
		}
	}
	return form, nil
}

// Device pairing has no bearer token yet. Every other real-TIDAL request must
// carry real credentials or recover them from the independent owner store.
func (g *tidalGateway) prepareRealAuth(w http.ResponseWriter, r *http.Request, clean string, form url.Values) bool {
	deviceGrant := form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code"
	if strings.HasSuffix(clean, "/oauth2/device_authorization") || (strings.HasSuffix(clean, "/oauth2/token") && deviceGrant) {
		// A leftover local bearer is never part of a device pairing request.
		if isSyntheticTidalToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
			r.Header.Del("Authorization")
		}
		return true
	}
	if strings.HasSuffix(clean, "/oauth2/token") {
		if form.Get("grant_type") == "refresh_token" {
			a := g.realAuth()
			if isSyntheticTidalToken(a.RefreshToken) {
				tidalNotLoggedIn(w)
				return false
			}
			form.Set("refresh_token", a.RefreshToken)
			if form.Get("client_id") == "" && a.ClientID != "" {
				form.Set("client_id", a.ClientID)
			}
			body := form.Encode()
			r.Body = io.NopCloser(strings.NewReader(body))
			r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
			r.ContentLength = int64(len(body))
		}
		if form.Has("refresh_token") && isSyntheticTidalToken(form.Get("refresh_token")) {
			tidalNotLoggedIn(w)
			return false
		}
		// OAuth grants authenticate with their form/client credentials. A local
		// bearer must not cause a second refresh before a rotating-token grant.
		if isSyntheticTidalToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
			r.Header.Del("Authorization")
		}
		return true
	}
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") || !strings.HasSuffix(clean, "/oauth2/token") {
		bearer := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		a := g.realAuth()
		if isSyntheticTidalToken(bearer) {
			if isSyntheticTidalToken(a.AccessToken) {
				var err error
				a, err = g.refreshRealAuth(r.Context(), a.AccessToken)
				if err != nil {
					if errors.Is(err, errTidalAuthInvalid) {
						tidalNotLoggedIn(w)
					} else {
						tidalTemporarilyUnavailable(w)
					}
					return false
				}
			}
			r.Header.Set("Authorization", "Bearer "+a.AccessToken)
		} else if !isSyntheticTidalToken(a.AccessToken) && a.AccessToken != bearer {
			// Engine keeps its pre-rotation access token in memory after gateway-side
			// refresh; upgrade outgoing requests to the current rotated token so later
			// expiries refresh cleanly instead of passing a 401 back to Engine.
			r.Header.Set("Authorization", "Bearer "+a.AccessToken)
		}
	}
	return true
}

func (g *tidalGateway) tokenResponse(resp *http.Response, refresh, clientID string) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1048577))
	resp.Body.Close()
	if err != nil || len(b) > 1048576 {
		return errors.New("invalid TIDAL token response")
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	if resp.StatusCode == 200 {
		var v struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			User    struct {
				ID      any    `json:"userId"`
				Country string `json:"countryCode"`
			} `json:"user"`
		}
		if json.Unmarshal(b, &v) == nil && (!isSyntheticTidalToken(v.Access) || !isSyntheticTidalToken(v.Refresh)) {
			return g.saveRealAuth(tidalAuth{AccessToken: v.Access, RefreshToken: v.Refresh, UserID: v.User.ID, CountryCode: v.User.Country, ClientID: clientID})
		}
	} else if resp.StatusCode == 400 || resp.StatusCode == 401 {
		var v struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &v)
		if v.Error == "invalid_grant" && !isSyntheticTidalToken(refresh) && refresh == g.realAuth().RefreshToken {
			return g.clearRealAuth()
		}
	}
	return nil
}

func (g *tidalGateway) tidalTransport() http.RoundTripper {
	if g.s != nil && g.s.cfg.HTTP != nil && g.s.cfg.HTTP.Transport != nil {
		return g.s.cfg.HTTP.Transport
	}
	return http.DefaultTransport
}

func (g *tidalGateway) refreshRealAuth(ctx context.Context, failedAccessToken string) (tidalAuth, error) {
	g.refreshMu.Lock()
	defer g.refreshMu.Unlock()
	a := g.realAuth()
	if !isSyntheticTidalToken(a.AccessToken) && a.AccessToken != failedAccessToken {
		return a, nil // another request already rotated the failed credentials
	}
	if isSyntheticTidalToken(a.RefreshToken) {
		return tidalAuth{}, fmt.Errorf("no TIDAL refresh token: %w", errTidalAuthInvalid)
	}
	base := g.s.cfg.TidalAuthURL
	if base == "" {
		base = "https://auth.tidal.com"
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.RefreshToken}}
	if a.ClientID != "" {
		form.Set("client_id", a.ClientID)
	}
	encoded := form.Encode()
	endpoint := strings.TrimRight(base, "/") + "/v1/oauth2/token"
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: g.tidalTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && !sleepContext(ctx, tidalRetryBackoff*time.Duration(attempt)) {
			return tidalAuth{}, errors.New("could not refresh TIDAL login")
		}
		// Not cancelled with the caller: once TIDAL has the request it rotates the pair and the
		// old refresh token is dead, so the answer must be stored even if Engine gave up waiting.
		// The client's own timeout still bounds it.
		req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodPost, endpoint, strings.NewReader(encoded))
		if err != nil {
			return tidalAuth{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			if attempt+1 < maxAttempts && isSafePreConnectError(err) {
				continue
			}
			return tidalAuth{}, fmt.Errorf("could not refresh TIDAL login: %w", err)
		}
		tokErr := g.tokenResponse(resp, a.RefreshToken, a.ClientID)
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if tokErr != nil {
			return tidalAuth{}, tokErr
		}
		if resp.StatusCode == 200 {
			a = g.realAuth()
			if isSyntheticTidalToken(a.AccessToken) {
				return tidalAuth{}, errors.New("no TIDAL access token")
			}
			return a, nil
		}
		if resp.StatusCode == 400 || resp.StatusCode == 401 {
			var v struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(b, &v)
			if isTransientOAuthError(v.Error) {
				if attempt+1 < maxAttempts {
					continue
				}
				return tidalAuth{}, errors.New("TIDAL auth temporarily unavailable")
			}
			return tidalAuth{}, fmt.Errorf("TIDAL login expired: %w", errTidalAuthInvalid)
		}
		if isTransientUpstreamStatus(resp.StatusCode) && attempt+1 < maxAttempts {
			continue
		}
		return tidalAuth{}, fmt.Errorf("TIDAL refresh failed with HTTP %d", resp.StatusCode)
	}
	return tidalAuth{}, errors.New("could not refresh TIDAL login")
}

// Engine 5.3.0 writes a provider-name map whose values are encoded JSON strings.
// Also accept an object value from older account writers. Never modify this file:
// Engine owns it and may store an opaque Profile/SSO session there.
func (g *tidalGateway) streamingTidalAccount() map[string]json.RawMessage {
	p := g.s.cfg.StreamingAccountsPath
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(st, opened) {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil
	}
	latest, err := f.Stat()
	if err != nil || latest.Size() > 1<<20 {
		return nil
	}
	var accounts map[string]json.RawMessage
	if json.Unmarshal(b, &accounts) != nil {
		return nil
	}
	raw := accounts["TIDAL"]
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		raw = json.RawMessage(encoded)
	}
	var account map[string]json.RawMessage
	if json.Unmarshal(raw, &account) != nil {
		return nil
	}
	return account
}

func (g *tidalGateway) seedFromStreamingAccounts() {
	// Only seed a missing store; existing credentials are authoritative.
	if _, err := os.Lstat(g.authPath()); !errors.Is(err, os.ErrNotExist) {
		return
	}
	account := g.streamingTidalAccount()
	b, _ := json.Marshal(account)
	var a tidalAuth
	if json.Unmarshal(b, &a) != nil {
		return
	}
	if !isSyntheticTidalToken(a.AccessToken) || !isSyntheticTidalToken(a.RefreshToken) {
		if err := g.saveRealAuth(a); err != nil {
			// Do not include account contents or tokens in diagnostics.
			log.Print("stream-sync: could not seed TIDAL login from Engine account")
		}
	}
}

func (g *tidalGateway) hasSyntheticStreamingAccount() bool {
	account := g.streamingTidalAccount()
	found := false
	for key, raw := range account {
		var value string
		switch key {
		case "accessToken", "refreshToken":
			if json.Unmarshal(raw, &value) != nil || !isSyntheticTidalToken(value) {
				return false
			}
			found = found || strings.TrimSpace(value) != ""
		case "provider", "providerType":
			if json.Unmarshal(raw, &value) != nil || value != "TIDAL" {
				return false
			}
		default:
			// Extra Profile/SSO data is not evidence of a disposable placeholder.
			return false
		}
	}
	return found
}

func (g *tidalGateway) isSyntheticAccountOnly() bool {
	return !g.hasRealAuth() && g.hasSyntheticStreamingAccount()
}
