package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// SpotifyBuiltinClientID is go-librespot's public desktop identity.
var SpotifyBuiltinClientID = librespot.ClientIdHex

// Match go-librespot's desktop scopes, including streaming for AP exchange.
const spotifyOAuthScopes = "app-remote-control playlist-modify playlist-modify-private playlist-modify-public playlist-read playlist-read-collaborative playlist-read-private streaming ugc-image-upload user-follow-modify user-follow-read user-library-modify user-library-read user-modify user-modify-playback-state user-modify-private user-personalized user-read-birthdate user-read-currently-playing user-read-email user-read-play-history user-read-playback-position user-read-playback-state user-read-private user-read-recently-played user-top-read"

// SpotifyDeviceAuth keeps the private device code out of JSON/UI responses.
type SpotifyDeviceAuth struct {
	VerificationURL string
	UserCode        string
	Expires         time.Time
	mu              sync.Mutex
	code            string
	interval        time.Duration
	next            time.Time
	done            bool
	approved        *Token
	terminal        error
}

var ErrSpotifyDeviceAuthExpired = errors.New("spotify: device authorization expired or denied")

func (s *Spotify) DeviceAuthorizationEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return (s.AccountsURL == "" || s.AccountsURL == "https://accounts.spotify.com") && (s.BaseURL == "" || s.BaseURL == "https://api.spotify.com/v1")
}
func (s *Spotify) deviceRequest(ctx context.Context, path string, v url.Values, out any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", s.accounts()+path, strings.NewReader(v.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client(s.HTTP).Do(req)
	if err != nil {
		return "", errors.New("spotify: device authorization unavailable")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil {
			return e.Error, errors.New("spotify: device authorization refused")
		}
		return "", errors.New("spotify: device authorization refused")
	}
	if json.Unmarshal(b, out) != nil {
		return "", errors.New("spotify: invalid device authorization response")
	}
	return "", nil
}
func (s *Spotify) BeginDeviceAuth(ctx context.Context) (*SpotifyDeviceAuth, error) {
	clientID := SpotifyBuiltinClientID
	var v struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	_, err := s.deviceRequest(ctx, "/oauth2/device/authorize", url.Values{"client_id": {clientID}, "scope": {spotifyOAuthScopes}}, &v)
	if err != nil {
		return nil, err
	}
	u := v.VerificationURIComplete
	if u == "" {
		u = v.VerificationURI
	}
	parsed, e := url.Parse(u)
	if v.DeviceCode == "" || v.ExpiresIn <= 0 || e != nil || parsed.Scheme != "https" || (parsed.Host != "accounts.spotify.com" && parsed.Host != "spotify.com" && parsed.Host != "www.spotify.com") {
		return nil, errors.New("spotify: invalid device authorization response")
	}
	if v.Interval < 5 {
		v.Interval = 5
	}
	now := time.Now()
	return &SpotifyDeviceAuth{VerificationURL: u, UserCode: v.UserCode, Expires: now.Add(time.Duration(v.ExpiresIn) * time.Second), code: v.DeviceCode, interval: time.Duration(v.Interval) * time.Second, next: now.Add(time.Duration(v.Interval) * time.Second)}, nil
}

// PollDeviceAuthorization makes at most one request, honoring interval/slow_down.
func (s *Spotify) PollDeviceAuthorization(ctx context.Context, d *SpotifyDeviceAuth) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done {
		return true, nil
	}
	if d.terminal != nil {
		return false, d.terminal
	}
	if time.Now().After(d.Expires) {
		return false, ErrSpotifyDeviceAuthExpired
	}
	if time.Now().Before(d.next) {
		return false, nil
	}
	if d.approved != nil {
		if err := s.AcceptToken(ctx, *d.approved); err != nil {
			if errors.Is(err, ErrSpotifyAuthentication) {
				d.terminal = err
			}
			return false, err
		}
		d.done = true
		d.approved = nil
		return true, nil
	}
	clientID := SpotifyBuiltinClientID
	var t Token
	d.next = time.Now().Add(d.interval)
	code, err := s.deviceRequest(ctx, "/api/token", url.Values{"client_id": {clientID}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {d.code}}, &t)
	switch code {
	case "authorization_pending":
		return false, nil
	case "slow_down":
		d.interval += 5 * time.Second
		d.next = time.Now().Add(d.interval)
		return false, nil
	case "invalid_client", "unauthorized_client", "invalid_scope":
		d.terminal = &spotifyAuthFailure{message: "spotify: device authorization refused; restart Spotify login", permanent: true}
		return false, d.terminal
	case "expired_token", "access_denied", "invalid_grant":
		d.code = ""
		d.Expires = time.Now()
		return false, ErrSpotifyDeviceAuthExpired
	}
	if err != nil {
		return false, err
	}
	if t.AccessToken == "" {
		d.terminal = &spotifyAuthFailure{message: "spotify: device response missing access token", permanent: true}
		return false, d.terminal
	}
	if t.Scope == "" {
		t.Scope = spotifyOAuthScopes
	}
	t.ClientID = SpotifyBuiltinClientID
	t.Username = "" // Device grants let the AP derive the canonical username.
	d.approved = &t
	d.code = ""
	if err = s.AcceptToken(ctx, t); err != nil {
		if errors.Is(err, ErrSpotifyAuthentication) {
			d.terminal = err
		}
		return false, err
	}
	d.done = true
	d.approved = nil
	d.code = ""
	return true, nil
}
