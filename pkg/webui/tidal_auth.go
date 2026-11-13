package webui

// Real TIDAL credentials live independently of Engine's shared gateway account.
// Engine may replace that account with local tokens when another source opens.
import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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
	case "", "enginex", "sc5000-token", "sc5000-refresh":
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
	g.authMu.Lock()
	defer g.authMu.Unlock()
	a := g.readAuth()
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

func tidalNotLoggedIn(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	io.WriteString(w, `{"status":401,"subStatus":6001,"userMessage":"Not logged in"}`)
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
		if err := g.saveRealAuth(a); err != nil {
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
		if form.Get("grant_type") == "refresh_token" && isSyntheticTidalToken(form.Get("refresh_token")) {
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
		if isSyntheticTidalToken(strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))) {
			a := g.realAuth()
			if isSyntheticTidalToken(a.AccessToken) {
				var err error
				a, err = g.refreshRealAuth(r, a)
				if err != nil {
					tidalNotLoggedIn(w)
					return false
				}
			}
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

func (g *tidalGateway) refreshRealAuth(r *http.Request, a tidalAuth) (tidalAuth, error) {
	if isSyntheticTidalToken(a.RefreshToken) {
		return tidalAuth{}, errors.New("no TIDAL refresh token")
	}
	base := g.s.cfg.TidalAuthURL
	if base == "" {
		base = "https://auth.tidal.com"
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.RefreshToken}}
	if a.ClientID != "" {
		form.Set("client_id", a.ClientID)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(base, "/")+"/v1/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tidalAuth{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return tidalAuth{}, errors.New("could not refresh TIDAL login")
	}
	defer resp.Body.Close()
	if err := g.tokenResponse(resp, a.RefreshToken, a.ClientID); err != nil {
		return tidalAuth{}, err
	}
	if resp.StatusCode != 200 {
		return tidalAuth{}, errors.New("TIDAL login expired")
	}
	a = g.realAuth()
	if isSyntheticTidalToken(a.AccessToken) {
		return tidalAuth{}, errors.New("no TIDAL access token")
	}
	return a, nil
}
