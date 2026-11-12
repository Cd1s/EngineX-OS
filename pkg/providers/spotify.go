package providers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Token struct {
	Scope        string    `json:"scope,omitempty"`
	Username     string    `json:"username,omitempty"`
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
	CredsPath, DeviceID                       string
	nativeMu                                  sync.Mutex
	native                                    spotifyNative
	nativeClosed                              bool
	nativeFactory                             func(context.Context) (spotifyNative, error)
	mediaOnce                                 sync.Once
	mediaHTTP                                 *http.Client
	StreamURL                                 string
	LinksPath, EmbedURL                       string // public playlist links (spotify_links.go)
	AudioKey                                  []byte
	HTTP                                      *http.Client
	refreshMu                                 sync.Mutex
	mu                                        sync.Mutex
	Token                                     Token
}

type spTrack struct {
	ID       string
	Name     string
	Type     string
	Duration int64 `json:"duration_ms"`
	Artists  []struct{ Name string }
	Album    struct {
		Name   string
		Images []struct{ URL string }
	}
	External struct{ ISRC string } `json:"external_ids"`
}

func (t spTrack) cover() string {
	if len(t.Album.Images) == 0 {
		return ""
	}
	return t.Album.Images[0].URL // Spotify lists the largest first
}

func (t spTrack) thumb() string {
	if len(t.Album.Images) == 0 {
		return ""
	}
	return t.Album.Images[len(t.Album.Images)-1].URL
}

func (t spTrack) track() Track {
	a := []string{}
	for _, v := range t.Artists {
		a = append(a, v.Name)
	}
	return Track{ID: t.ID, Title: t.Name, Artist: strings.Join(a, ", "), Artists: a, Album: t.Album.Name, ISRC: t.External.ISRC, CoverURL: t.cover(), ThumbURL: t.thumb(), DurationMS: t.Duration, Provider: "spotify"}
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

// SetClientID serializes app changes with OAuth token refresh.
func (s *Spotify) SetClientID(id string) { s.mu.Lock(); s.ClientID = id; s.mu.Unlock() }

// HasToken reports whether a login token is present, safe against concurrent refresh.
func (s *Spotify) HasToken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Token.AccessToken != ""
}

// SetToken installs and saves a token obtained elsewhere, such as the cloud relay.
func (s *Spotify) SetToken(t Token) error {
	s.nativeMu.Lock()
	defer s.nativeMu.Unlock()
	if s.nativeClosed {
		return errors.New("spotify: session closed")
	}
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
func tokenExpired(t Token) bool {
	return !t.Expiry.IsZero() && time.Now().Add(30*time.Second).After(t.Expiry)
}

func (s *Spotify) doTokenRequest(ctx context.Context, clientID string, prev Token, v url.Values) (Token, error) {
	v.Set("client_id", clientID)
	var t Token
	if err := JSON(ctx, s.HTTP, "POST", s.accounts()+"/api/token", strings.NewReader(v.Encode()), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, &t); err != nil {
		return Token{}, err
	}
	if t.AccessToken == "" {
		return Token{}, errors.New("spotify: token response missing access token")
	}
	if t.RefreshToken == "" {
		t.RefreshToken = prev.RefreshToken
	}
	if t.Scope == "" {
		t.Scope = prev.Scope
	}
	if t.Username == "" {
		t.Username = prev.Username
	}
	if t.ExpiresIn <= 0 {
		t.ExpiresIn = 3600
	}
	t.Expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	t.ClientID = clientID
	return t, nil
}

func (s *Spotify) nativeBearer(ctx context.Context) (http.Header, error) {
	var token string
	err := s.withNative(ctx, func(n spotifyNative) (err error) { token, err = n.Bearer(ctx); return err })
	if err != nil {
		return nil, err
	}
	return http.Header{"Authorization": {"Bearer " + token}}, nil
}

func oauthRefreshErr(err error) error {
	var st StatusError
	if errors.As(err, &st) && (st == 400 || st == 401) {
		return fmt.Errorf("spotify: expired token; login required: %w", StatusError(401))
	}
	return err
}

func (s *Spotify) authorization(ctx context.Context) (http.Header, error) {
	s.mu.Lock()
	tok, clientID := s.Token, s.ClientID
	s.mu.Unlock()
	if tok.AccessToken != "" && !tokenExpired(tok) {
		return http.Header{"Authorization": {"Bearer " + tok.AccessToken}}, nil
	}

	s.refreshMu.Lock()
	s.mu.Lock()
	tok, clientID = s.Token, s.ClientID
	s.mu.Unlock()
	if tok.AccessToken != "" && !tokenExpired(tok) {
		s.refreshMu.Unlock()
		return http.Header{"Authorization": {"Bearer " + tok.AccessToken}}, nil
	}
	if tok.RefreshToken != "" && clientID != "" {
		refreshed, err := s.doTokenRequest(ctx, clientID, tok, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}})
		if err == nil {
			saveErr := SaveJSON(s.TokenPath, refreshed)
			s.mu.Lock()
			if s.Token.AccessToken == tok.AccessToken {
				s.Token = refreshed
			} else {
				refreshed = s.Token
			}
			s.mu.Unlock()
			s.refreshMu.Unlock()
			if saveErr != nil {
				return nil, saveErr
			}
			return http.Header{"Authorization": {"Bearer " + refreshed.AccessToken}}, nil
		}
		s.refreshMu.Unlock()
		if s.HasStoredCredentials() {
			return s.nativeBearer(ctx)
		}
		return nil, oauthRefreshErr(err)
	}
	s.refreshMu.Unlock()

	if s.HasStoredCredentials() {
		return s.nativeBearer(ctx)
	}
	if tokenExpired(tok) {
		return nil, fmt.Errorf("spotify: expired token; login required: %w", StatusError(401))
	}
	return nil, fmt.Errorf("spotify: login required: %w", StatusError(401))
}

func (s *Spotify) refreshAfter401(ctx context.Context, failedAuth string) (http.Header, error) {
	s.refreshMu.Lock()
	s.mu.Lock()
	tok, clientID := s.Token, s.ClientID
	s.mu.Unlock()
	if tok.AccessToken != "" && !tokenExpired(tok) && "Bearer "+tok.AccessToken != failedAuth {
		s.refreshMu.Unlock()
		return http.Header{"Authorization": {"Bearer " + tok.AccessToken}}, nil
	}
	if tok.RefreshToken != "" && clientID != "" {
		refreshed, err := s.doTokenRequest(ctx, clientID, tok, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}})
		if err == nil {
			saveErr := SaveJSON(s.TokenPath, refreshed)
			s.mu.Lock()
			if s.Token.AccessToken == tok.AccessToken {
				s.Token = refreshed
			} else {
				refreshed = s.Token
			}
			s.mu.Unlock()
			s.refreshMu.Unlock()
			if saveErr != nil {
				return nil, saveErr
			}
			return http.Header{"Authorization": {"Bearer " + refreshed.AccessToken}}, nil
		}
		s.refreshMu.Unlock()
		if s.HasStoredCredentials() {
			return s.nativeBearer(ctx)
		}
		return nil, oauthRefreshErr(err)
	}
	s.refreshMu.Unlock()
	if s.HasStoredCredentials() {
		h, err := s.nativeBearer(ctx)
		if err != nil {
			return nil, err
		}
		if h.Get("Authorization") == failedAuth {
			return nil, StatusError(401)
		}
		return h, nil
	}
	return nil, StatusError(401)
}

// BeginPKCE returns an authorization URL and a private verifier. The caller
// binds state, verifier, redirect URI and expiry to the initiating session.
func (s *Spotify) BeginPKCE(state, redirect string) (authURL, verifier string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	verifier = RandomID()
	hash := sha256.Sum256([]byte(verifier))
	authURL = endpoint(s.accounts(), "/authorize", url.Values{"client_id": {s.ClientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {spotifyOAuthScopes}, "state": {state}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}})
	return
}
func (s *Spotify) CompletePKCE(ctx context.Context, code, verifier, redirect string) error {
	s.nativeMu.Lock()
	closed := s.nativeClosed
	s.nativeMu.Unlock()
	if closed {
		return errors.New("spotify: session closed")
	}
	s.mu.Lock()
	prev, clientID := s.Token, s.ClientID
	s.mu.Unlock()
	refreshed, err := s.doTokenRequest(ctx, clientID, prev, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}})
	if err != nil {
		return err
	}
	s.nativeMu.Lock()
	if s.nativeClosed {
		s.nativeMu.Unlock()
		return errors.New("spotify: session closed")
	}
	s.mu.Lock()
	s.Token = refreshed
	saveErr := SaveJSON(s.TokenPath, refreshed)
	s.mu.Unlock()
	s.nativeMu.Unlock()
	if saveErr != nil {
		return saveErr
	}
	return s.ExchangeOAuthCredentials(ctx)
}
func (s *Spotify) get(ctx context.Context, path string, out any) error {
	h, err := s.authorization(ctx)
	if err != nil {
		return err
	}
	err = JSON(ctx, s.HTTP, "GET", s.base()+path, nil, h, out)
	var st StatusError
	if errors.As(err, &st) && st == 401 {
		if h2, rerr := s.refreshAfter401(ctx, h.Get("Authorization")); rerr == nil {
			return JSON(ctx, s.HTTP, "GET", s.base()+path, nil, h2, out)
		}
	}
	return err
}

func endpointMissing(err error) bool {
	msg := err.Error()
	return msg == "provider: HTTP 404" || msg == "provider: HTTP 405"
}

// SpotifyLikedID names the owner's Liked Songs, which Spotify keeps outside playlists.
const SpotifyLikedID = "liked"

func (s *Spotify) Playlists(ctx context.Context) ([]Playlist, error) {
	linked := s.linkedPlaylists()
	if !s.HasToken() && !s.HasStoredCredentials() {
		return linked, nil
	}
	all, err := s.apiPlaylists(ctx)
	if s.nativeLibraryFallback(err) {
		all, err = s.nativePlaylists(ctx)
	}
	if err != nil {
		return nil, err
	}
	return append(linked, all...), nil
}

func (s *Spotify) apiPlaylists(ctx context.Context) ([]Playlist, error) {
	// An older login may lack the library scope; its playlists still work.
	likedCtx, cancelLiked := context.WithCancel(ctx)
	defer cancelLiked()
	liked := make(chan Playlist, 1)
	go func() {
		var r struct {
			Total int `json:"total"`
		}
		if s.get(likedCtx, "/me/tracks?limit=1&offset=0", &r) == nil {
			liked <- Playlist{SpotifyLikedID, "已點讚的歌曲 (Liked Songs)", r.Total}
		}
		close(liked)
	}()
	all, err := collectPages(ctx, spotifyPages, 50, 2000, func(page int) (listPage[Playlist], error) {
		var r struct {
			Items []struct {
				ID     string
				Name   string
				Tracks struct{ Total int }
				Items  struct{ Total int }
			}
			Next  *string
			Total int
		}
		err := s.get(ctx, "/me/playlists?limit=50&offset="+strconv.Itoa(page*50), &r)
		out := listPage[Playlist]{more: r.Next != nil && *r.Next != "", total: r.Total}
		for _, p := range r.Items {
			total := p.Items.Total
			if total == 0 {
				total = p.Tracks.Total
			}
			out.items = append(out.items, Playlist{p.ID, p.Name, total})
		}
		return out, err
	})
	if err != nil {
		cancelLiked()
		<-liked
		return nil, err
	}
	if p, ok := <-liked; ok {
		all = append([]Playlist{p}, all...)
	}
	return all, nil
}

// spPageItem accepts both the current /items schema and the legacy /tracks
// schema. Local tracks, episodes and unavailable rows keep their old filtering.
type spPageItem struct {
	IsLocal bool `json:"is_local"`
	Track   *spTrack
	Item    *spTrack
}

func (s *Spotify) trackPages(ctx context.Context, path string, size int, fallback bool) ([]Track, error) {
	all, err := collectPages(ctx, spotifyPages, size, 100000/size, func(page int) (listPage[Track], error) {
		var r struct {
			Items []spPageItem
			Next  *string
			Total int
		}
		query := "?limit=" + strconv.Itoa(size) + "&offset=" + strconv.Itoa(page*size)
		err := s.get(ctx, path+query, &r)
		// Only an endpoint that is gone falls back; never retry a 429 or 5xx.
		if err != nil && page == 0 && fallback && endpointMissing(err) {
			path = strings.TrimSuffix(path, "/items") + "/tracks"
			err = s.get(ctx, path+query, &r)
		}
		out := listPage[Track]{more: r.Next != nil && *r.Next != "", total: r.Total}
		for _, item := range r.Items {
			t := item.Item
			if t == nil {
				t = item.Track
			}
			if t == nil || t.ID == "" || (fallback && (item.IsLocal || (t.Type != "" && t.Type != "track"))) {
				continue
			}
			out.items = append(out.items, t.track())
		}
		return out, err
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}
func (s *Spotify) likedTracks(ctx context.Context) ([]Track, error) {
	return s.trackPages(ctx, "/me/tracks", 50, false)
}
func (s *Spotify) Tracks(ctx context.Context, id string) ([]Track, error) {
	if t, ok := s.linkedTracks(id); ok {
		return t, nil
	}
	var tracks []Track
	var err error
	if id == SpotifyLikedID {
		tracks, err = s.likedTracks(ctx)
	} else {
		tracks, err = s.trackPages(ctx, "/playlists/"+url.PathEscape(id)+"/items", 100, true)
	}
	if s.nativeLibraryFallback(err) {
		return s.nativeTracks(ctx, id)
	}
	return tracks, err

}

func (s *Spotify) Search(ctx context.Context, query string) ([]Track, error) {
	var r struct{ Tracks struct{ Items []*spTrack } }
	if err := s.get(ctx, "/search?"+url.Values{"q": {query}, "type": {"track"}, "limit": {"10"}}.Encode(), &r); err != nil {
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
				Tracks struct{ Total int } `json:"tracks"`
				Items  struct{ Total int } `json:"items"`
			}
		}
	}
	if err := s.get(ctx, "/search?"+url.Values{"q": {query}, "type": {"playlist"}, "limit": {"10"}}.Encode(), &r); err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(r.Playlists.Items))
	for _, p := range r.Playlists.Items {
		if p != nil && p.ID != "" {
			total := p.Items.Total
			if total == 0 {
				total = p.Tracks.Total
			}
			out = append(out, Playlist{p.ID, p.Name, total})
		}
	}
	return out, nil
}
