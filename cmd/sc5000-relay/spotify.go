package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	spotifyAccounts      = "https://accounts.spotify.com"
	spotifyLoopbackURI   = "http://127.0.0.1:8080/oauth/spotify/callback"
	spotifyScopes        = "playlist-read-private playlist-read-collaborative"
	spotifyProbeControl  = "https://redirect-probe.invalid/sc5000/callback"
	spotifyProbeCacheTTL = 30 * time.Minute
)

var clientIDRe = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

func validClientID(id string) bool { return clientIDRe.MatchString(id) }

func randomVerifier() string {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type probeResult struct {
	https bool
	at    time.Time
}

// spotifyAuth decides which redirect_uri a client ID can use and exchanges
// PKCE codes. Spotify only validates redirect_uri after login, so an
// anonymous probe is usually inconclusive; a successful HTTPS callback is
// remembered per client ID and wins from then on.
type spotifyAuth struct {
	httpsRedirect string
	learnedPath   string
	hc            *http.Client

	mu      sync.Mutex
	learned map[string]bool
	probes  map[string]probeResult
}

func newSpotifyAuth(learnedPath, httpsRedirect string) *spotifyAuth {
	a := &spotifyAuth{
		httpsRedirect: httpsRedirect,
		learnedPath:   learnedPath,
		hc: &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		learned: map[string]bool{},
		probes:  map[string]probeResult{},
	}
	if b, err := os.ReadFile(learnedPath); err == nil {
		_ = json.Unmarshal(b, &a.learned)
	}
	return a
}

func (a *spotifyAuth) authorizeURL(clientID, redirect, state, verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"scope":                 {spotifyScopes},
		"state":                 {state},
		"code_challenge_method": {"S256"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
	}
	return spotifyAccounts + "/authorize?" + q.Encode()
}

// rejects reports whether Spotify rejects redirect for clientID before login.
func (a *spotifyAuth) rejects(ctx context.Context, clientID, redirect string) (bool, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", a.authorizeURL(clientID, redirect, "probe", "probe-verifier-0000000000000000000000000000"), nil)
	resp, err := a.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	loc := resp.Header.Get("Location")
	text := strings.ToLower(string(body) + " " + loc)
	if strings.Contains(text, "invalid redirect uri") || strings.Contains(text, "invalid_redirect") ||
		strings.Contains(loc, "error=") {
		return true, nil
	}
	return resp.StatusCode >= 400, nil
}

// useHTTPS reports whether the automatic HTTPS callback should be offered as
// the primary flow for clientID.
func (a *spotifyAuth) useHTTPS(ctx context.Context, clientID string) bool {
	a.mu.Lock()
	if a.learned[clientID] {
		a.mu.Unlock()
		return true
	}
	if p, ok := a.probes[clientID]; ok && time.Since(p.at) < spotifyProbeCacheTTL {
		a.mu.Unlock()
		return p.https
	}
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ours, err1 := a.rejects(ctx, clientID, a.httpsRedirect)
	control, err2 := a.rejects(ctx, clientID, spotifyProbeControl)
	// Conclusive only when Spotify demonstrably rejects an unregistered URI
	// but not ours; otherwise fall back to the loopback paste flow.
	https := err1 == nil && err2 == nil && control && !ours
	if err1 != nil || err2 != nil {
		log.Printf("spotify probe: %v %v", err1, err2)
	}
	a.mu.Lock()
	a.probes[clientID] = probeResult{https: https, at: time.Now()}
	a.mu.Unlock()
	return https
}

func (a *spotifyAuth) learnHTTPS(clientID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.learned[clientID] {
		return
	}
	a.learned[clientID] = true
	b, _ := json.Marshal(a.learned)
	if err := writeFileAtomic(a.learnedPath, b, 0o600); err != nil {
		log.Printf("spotify: save learned redirects: %v", err)
	}
}

func (a *spotifyAuth) exchange(ctx context.Context, clientID, code, verifier, redirect string) (*spotifyToken, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", spotifyAccounts+"/api/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("spotify token: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode != 200 || out.AccessToken == "" {
		return nil, fmt.Errorf("spotify token: %s %s", out.Error, out.ErrorDescription)
	}
	if out.TokenType == "" {
		out.TokenType = "Bearer"
	}
	return &spotifyToken{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		TokenType:    out.TokenType,
		ExpiresIn:    out.ExpiresIn,
		Expiry:       time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).UTC(),
		ClientID:     clientID,
	}, nil
}

// parsePastedCode accepts the full loopback URL the browser failed to open,
// a bare query string, or a bare code. It returns the code and the redirect
// URI the code was issued for.
func parsePastedCode(in string) (code, redirect string, err error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", "", errors.New("请粘贴授权后的地址或 code")
	}
	if i := strings.IndexAny(in, " \t\r\n"); i > 0 && strings.Contains(in, "code=") {
		// Tolerate text copied around the URL.
		for _, f := range strings.Fields(in) {
			if strings.Contains(f, "code=") {
				in = f
				break
			}
		}
	}
	if strings.Contains(in, "code=") || strings.Contains(in, "error=") {
		raw := in
		if i := strings.Index(raw, "?"); i >= 0 {
			raw = raw[i+1:]
		}
		if i := strings.Index(raw, "#"); i >= 0 {
			raw = raw[:i]
		}
		q, perr := url.ParseQuery(raw)
		if perr != nil {
			return "", "", errors.New("无法解析粘贴的地址")
		}
		if e := q.Get("error"); e != "" {
			return "", "", fmt.Errorf("Spotify 返回错误：%s", e)
		}
		code = q.Get("code")
		redirect = spotifyLoopbackURI
		if u, perr := url.Parse(in); perr == nil && u.Scheme != "" && u.Host != "" {
			redirect = u.Scheme + "://" + u.Host + u.Path
		}
	} else {
		code = in
		redirect = spotifyLoopbackURI
	}
	if code == "" || strings.ContainsAny(code, " /") || len(code) > 2048 {
		return "", "", errors.New("没有找到有效的 code")
	}
	return code, redirect, nil
}
