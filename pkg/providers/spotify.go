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
	if t.Scope == "" {
		t.Scope = s.Token.Scope
	}
	if t.Username == "" {
		t.Username = s.Token.Username
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
	expired := !s.Token.Expiry.IsZero() && time.Now().Add(30*time.Second).After(s.Token.Expiry)
	useNative := s.HasStoredCredentials() && (s.Token.AccessToken == "" || expired || s.Token.RefreshToken == "")
	if useNative {
		s.mu.Unlock()
		var token string
		err := s.withNative(ctx, func(n spotifyNative) (err error) { token, err = n.Bearer(ctx); return err })
		if err != nil {
			return nil, err
		}
		return http.Header{"Authorization": {"Bearer " + token}}, nil
	}
	defer s.mu.Unlock()
	if expired {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	verifier = RandomID()
	hash := sha256.Sum256([]byte(verifier))
	authURL = endpoint(s.accounts(), "/authorize", url.Values{"client_id": {s.ClientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {spotifyOAuthScopes}, "state": {state}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}})
	return
}
func (s *Spotify) CompletePKCE(ctx context.Context, code, verifier, redirect string) error {
	s.nativeMu.Lock()
	if s.nativeClosed {
		s.nativeMu.Unlock()
		return errors.New("spotify: session closed")
	}
	s.mu.Lock()
	err := s.tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}})
	s.mu.Unlock()
	s.nativeMu.Unlock()
	if err != nil {
		return err
	}
	return s.ExchangeOAuthCredentials(ctx)
}
func (s *Spotify) get(ctx context.Context, path string, out any) error {
	h, err := s.authorization(ctx)
	if err != nil {
		return err
	}
	return JSON(ctx, s.HTTP, "GET", s.base()+path, nil, h, out)
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
		native, nerr := s.nativePlaylists(ctx)
		if nerr == nil {
			all, err = s.mergePlaylists(all, native), nil
		} else {
			err = nerr
		}
	}
	if err != nil {
		if len(linked) > 0 {
			return linked, nil
		}
		return nil, err
	}
	return append(linked, all...), nil
}

func (s *Spotify) mergePlaylists(web, native []Playlist) []Playlist {
	if len(web) == 0 {
		return native
	}
	if len(native) == 0 {
		return web
	}
	seen := make(map[string]bool, len(web)+len(native))
	out := make([]Playlist, 0, len(web)+len(native))
	for _, p := range web {
		if !seen[p.ID] {
			seen[p.ID] = true
			out = append(out, p)
		}
	}
	for _, p := range native {
		if !seen[p.ID] {
			seen[p.ID] = true
			out = append(out, p)
		}
	}
	return out
}

func (s *Spotify) apiPlaylists(ctx context.Context) ([]Playlist, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// An older login may lack the library scope; its playlists still work.
	type likedResult struct {
		item Playlist
		err  error
	}
	liked := make(chan likedResult, 1)
	go func() {
		var r struct {
			Total int `json:"total"`
		}
		err := s.get(ctx, "/me/tracks?limit=1&offset=0", &r)
		if err == nil {
			liked <- likedResult{item: Playlist{SpotifyLikedID, "已點讚的歌曲 (Liked Songs)", r.Total}}
		} else {
			liked <- likedResult{err: err}
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
		cancel()
		return all, err
	}
	lr := <-liked
	if lr.err == nil {
		all = append([]Playlist{lr.item}, all...)
	} else if s.HasStoredCredentials() {
		all = append([]Playlist{{SpotifyLikedID, "已點讚的歌曲 (Liked Songs)", 0}}, all...)
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
