package providers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresIn    int64     `json:"expires_in,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	ExpiresAt    int64     `json:"expires_at,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
}
type Spotify struct {
	BaseURL, AccountsURL, ClientID, TokenPath string
	HTTP                                      *http.Client
	mu                                        sync.Mutex
	Token                                     Token
}

type spTrack struct {
	ID       string
	Name     string
	Type     string
	Duration int64 `json:"duration_ms"`
	Artists  []struct{ Name string }
	Album    struct{ Name string }
	External struct{ ISRC string } `json:"external_ids"`
}

func (t spTrack) track() Track {
	a := []string{}
	for _, v := range t.Artists {
		a = append(a, v.Name)
	}
	return Track{ID: t.ID, Title: t.Name, Artist: strings.Join(a, ", "), Artists: a, Album: t.Album.Name, ISRC: t.External.ISRC, DurationMS: t.Duration, Provider: "spotify"}
}
func RandomID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func (s *Spotify) base() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return "https://api.spotify.com/v1"
}
func (s *Spotify) accounts() string {
	if s.AccountsURL != "" {
		return s.AccountsURL
	}
	return "https://accounts.spotify.com"
}

// LoadToken accepts the common OAuth2 expiry string and numeric expires_at.
func (s *Spotify) LoadToken() error {
	b, err := os.ReadFile(s.TokenPath)
	if err != nil {
		return err
	}
	var t Token
	if err = json.Unmarshal(b, &t); err != nil {
		return errors.New("spotify: invalid token file")
	}
	if t.Expiry.IsZero() && t.ExpiresAt > 0 {
		t.Expiry = time.Unix(t.ExpiresAt, 0)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Token = t
	// Only the client that minted a token can refresh it.
	if t.ClientID != "" {
		s.ClientID = t.ClientID
	}
	return nil
}

// SetToken installs and saves a token obtained elsewhere, such as the cloud relay.
func (s *Spotify) SetToken(t Token) error {
	if t.AccessToken == "" {
		return errors.New("spotify: token missing access token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.Expiry.IsZero() && t.ExpiresIn > 0 {
		t.Expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	if t.ClientID != "" {
		s.ClientID = t.ClientID
	}
	t.ClientID = s.ClientID
	if err := SaveJSON(s.TokenPath, t); err != nil {
		return err
	}
	s.Token = t
	return nil
}
func (s *Spotify) tokenRequest(ctx context.Context, v url.Values) error {
	v.Set("client_id", s.ClientID)
	var t Token
	if err := JSON(ctx, s.HTTP, "POST", s.accounts()+"/api/token", strings.NewReader(v.Encode()), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, &t); err != nil {
		return err
	}
	if t.AccessToken == "" {
		return errors.New("spotify: token response missing access token")
	}
	if t.RefreshToken == "" {
		t.RefreshToken = s.Token.RefreshToken
	}
	if t.ExpiresIn <= 0 {
		t.ExpiresIn = 3600
	}
	t.Expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	t.ClientID = s.ClientID
	s.Token = t
	return SaveJSON(s.TokenPath, t)
}
func (s *Spotify) authorization(ctx context.Context) (http.Header, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Token.Expiry.IsZero() && time.Now().Add(30*time.Second).After(s.Token.Expiry) {
		if s.Token.RefreshToken == "" || s.ClientID == "" {
			return nil, errors.New("spotify: expired token; login required")
		}
		if err := s.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.Token.RefreshToken}}); err != nil {
			return nil, err
		}
	}
	if s.Token.AccessToken == "" {
		return nil, errors.New("spotify: login required")
	}
	return http.Header{"Authorization": {"Bearer " + s.Token.AccessToken}}, nil
}

// BeginPKCE returns an authorization URL and a private verifier. The caller
// binds state, verifier, redirect URI and expiry to the initiating session.
func (s *Spotify) BeginPKCE(state, redirect string) (authURL, verifier string) {
	verifier = RandomID()
	hash := sha256.Sum256([]byte(verifier))
	authURL = endpoint(s.accounts(), "/authorize", url.Values{"client_id": {s.ClientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {"playlist-read-private playlist-read-collaborative"}, "state": {state}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}})
	return
}
func (s *Spotify) CompletePKCE(ctx context.Context, code, verifier, redirect string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}})
}
func (s *Spotify) get(ctx context.Context, path string, out any) error {
	h, err := s.authorization(ctx)
	if err != nil {
		return err
	}
	return JSON(ctx, s.HTTP, "GET", s.base()+path, nil, h, out)
}
func (s *Spotify) Playlists(ctx context.Context) ([]Playlist, error) {
	var all []Playlist
	for offset := 0; offset < 100000; offset += 50 {
		var r struct {
			Items []struct {
				ID     string
				Name   string
				Tracks struct{ Total int }
			}
			Next *string
		}
		if err := s.get(ctx, "/me/playlists?limit=50&offset="+strconv.Itoa(offset), &r); err != nil {
			return nil, err
		}
		for _, p := range r.Items {
			all = append(all, Playlist{p.ID, p.Name, p.Tracks.Total})
		}
		if r.Next == nil || *r.Next == "" {
			return all, nil
		}
	}
	return nil, errors.New("spotify: pagination limit")
}
func (s *Spotify) Tracks(ctx context.Context, id string) ([]Track, error) {
	var all []Track
	for offset := 0; offset < 100000; offset += 100 {
		var r struct {
			Items []struct {
				IsLocal bool     `json:"is_local"`
				Track   *spTrack `json:"track"`
			}
			Next *string
		}
		if err := s.get(ctx, "/playlists/"+url.PathEscape(id)+"/tracks?limit=100&offset="+strconv.Itoa(offset), &r); err != nil {
			return nil, err
		}
		for _, item := range r.Items {
			t := item.Track
			if t == nil || item.IsLocal || t.ID == "" || (t.Type != "" && t.Type != "track") {
				continue
			}
			all = append(all, t.track())
		}
		if r.Next == nil || *r.Next == "" {
			return all, nil
		}
	}
	return nil, errors.New("spotify: pagination limit")
}
func (s *Spotify) Search(ctx context.Context, query string) ([]Track, error) {
	var r struct{ Tracks struct{ Items []*spTrack } }
	if err := s.get(ctx, "/search?"+url.Values{"q": {query}, "type": {"track"}, "limit": {"20"}}.Encode(), &r); err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(r.Tracks.Items))
	for _, t := range r.Tracks.Items {
		if t != nil && t.ID != "" {
			out = append(out, t.track())
		}
	}
	return out, nil
}
func (s *Spotify) SearchPlaylists(ctx context.Context, query string) ([]Playlist, error) {
	// Spotify returns null entries for playlists that became unavailable.
	var r struct {
		Playlists struct {
			Items []*struct {
				ID     string
				Name   string
				Tracks struct{ Total int }
			}
		}
	}
	if err := s.get(ctx, "/search?"+url.Values{"q": {query}, "type": {"playlist"}, "limit": {"20"}}.Encode(), &r); err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(r.Playlists.Items))
	for _, p := range r.Playlists.Items {
		if p != nil && p.ID != "" {
			out = append(out, Playlist{p.ID, p.Name, p.Tracks.Total})
		}
	}
	return out, nil
}
