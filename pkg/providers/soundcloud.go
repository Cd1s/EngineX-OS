package providers

// SoundCloud through its official API with the owner's registered app: the Client ID
// and secret are exchanged for an access token, or a user's own token is used. Only
// what the API marks fully playable is fetched. Previews are refused, and nothing is
// decrypted.

import (
	"bufio"
	"context"
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

const (
	defaultSoundCloudAPI  = "https://api.soundcloud.com"
	soundCloudTokenPath   = "https://secure.soundcloud.com/oauth/token"
	soundCloudLikesID     = "likes"
	soundCloudListLimit   = 2000
	soundCloudTokenMargin = time.Minute
)

// ErrNoFullStream means the API only offers a preview of the track.
var ErrNoFullStream = errors.New("soundcloud: no full-length stream for this track")

// SoundCloudAuth exchanges an app's credentials for an access token. SoundCloud
// limits how often that may happen, so the token is shared and kept in a file.
type SoundCloudAuth struct {
	TokenURL string
	Path     string
	HTTP     *http.Client

	mu     sync.Mutex
	loaded bool
	saved  soundCloudToken
}

type soundCloudToken struct {
	ClientID string    `json:"client_id"`
	Access   string    `json:"access_token"`
	Refresh  string    `json:"refresh_token,omitempty"`
	Expiry   time.Time `json:"expiry"`

	// The owner's account login, separate from the app's token.
	UserAccess  string    `json:"user_access_token,omitempty"`
	UserRefresh string    `json:"user_refresh_token,omitempty"`
	UserExpiry  time.Time `json:"user_expiry,omitzero"`
}

// Token returns a valid access token for the app.
func (a *SoundCloudAuth) Token(ctx context.Context, id, secret string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	if a.saved.ClientID == id && a.saved.Access != "" && time.Now().Add(soundCloudTokenMargin).Before(a.saved.Expiry) {
		return a.saved.Access, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if a.saved.ClientID == id && a.saved.Refresh != "" {
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", a.saved.Refresh)
	}
	var r soundCloudTokenResponse
	err := a.post(ctx, form, id, secret, &r)
	if err != nil && form.Get("grant_type") == "refresh_token" {
		form.Set("grant_type", "client_credentials")
		form.Del("refresh_token")
		err = a.post(ctx, form, id, secret, &r)
	}
	if err != nil {
		return "", err
	}
	a.saved.ClientID, a.saved.Access, a.saved.Refresh, a.saved.Expiry = id, r.Access, r.Refresh, r.expiry()
	if err := SaveJSON(a.Path, a.saved); err != nil {
		return "", err
	}
	return r.Access, nil
}

func (a *SoundCloudAuth) load() {
	if a.loaded {
		return
	}
	a.loaded = true
	if b, err := os.ReadFile(a.Path); err == nil {
		json.Unmarshal(b, &a.saved)
	}
}

type soundCloudTokenResponse struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Expires int64  `json:"expires_in"`
}

func (r soundCloudTokenResponse) expiry() time.Time {
	if r.Expires <= 0 {
		r.Expires = 3600
	}
	return time.Now().Add(time.Duration(r.Expires) * time.Second)
}

// post sends a token request. The app's credentials go in a Basic header for
// client_credentials; a refresh_token grant answers 400 to that header and wants
// them in the form instead.
func (a *SoundCloudAuth) post(ctx context.Context, form url.Values, id, secret string, out *soundCloudTokenResponse) error {
	address := a.TokenURL
	if address == "" {
		address = soundCloudTokenPath
	}
	h := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json; charset=utf-8"}}
	if form.Get("grant_type") == "refresh_token" {
		form = cloneValues(form)
		form.Set("client_id", id)
		form.Set("client_secret", secret)
	} else {
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(id+":"+secret)))
	}
	if err := JSON(ctx, a.HTTP, "POST", address, strings.NewReader(form.Encode()), h, out); err != nil {
		return err
	}
	if out.Access == "" {
		return errors.New("soundcloud: token response missing access token")
	}
	return nil
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

// SetUser stores the owner's account login. Empty values clear it.
func (a *SoundCloudAuth) SetUser(access, refresh string, expiry time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	a.saved.UserAccess, a.saved.UserRefresh, a.saved.UserExpiry = access, refresh, expiry
	return SaveJSON(a.Path, a.saved)
}

// HasUser reports whether the owner logged in with an account.
func (a *SoundCloudAuth) HasUser() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	return a.saved.UserAccess != ""
}

// UserToken returns the owner's token, renewing it when it is about to expire. The
// lock also keeps two callers from spending the same rotating refresh token.
func (a *SoundCloudAuth) UserToken(ctx context.Context, id, secret string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	if time.Now().Add(soundCloudTokenMargin).Before(a.saved.UserExpiry) {
		return a.saved.UserAccess, nil
	}
	if a.saved.UserRefresh == "" {
		return "", errors.New("soundcloud: login expired")
	}
	var r soundCloudTokenResponse
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.saved.UserRefresh}}
	if err := a.post(ctx, form, id, secret, &r); err != nil {
		return "", err
	}
	a.saved.UserAccess, a.saved.UserExpiry = r.Access, r.expiry()
	if r.Refresh != "" {
		a.saved.UserRefresh = r.Refresh
	}
	if err := SaveJSON(a.Path, a.saved); err != nil {
		return "", err
	}
	return r.Access, nil
}

type SoundCloud struct {
	BaseURL  string
	HTTP     *http.Client
	ClientID string
	Secret   string
	Token    string // a user's own token, which also reaches private content
	Auth     *SoundCloudAuth
	UserID   int64
	Username string
}

// Usable reports whether there is anything to reach the API with.
func (s *SoundCloud) Usable() bool {
	return s.Token != "" || (s.ClientID != "" && s.Secret != "")
}

type scTrack struct {
	ID       int64   `json:"id"`
	Title    string  `json:"title"`
	Duration int64   `json:"duration"`
	BPM      float64 `json:"bpm"`
	Key      string  `json:"key_signature"`
	ISRC     string  `json:"isrc"`
	Access   string  `json:"access"`
	Artwork  string  `json:"artwork_url"`
	User     struct {
		Username string `json:"username"`
		Avatar   string `json:"avatar_url"`
	} `json:"user"`
}

// UnmarshalJSON also accepts a like, which wraps the track as {"track": {...}}.
func (t *scTrack) UnmarshalJSON(b []byte) error {
	type plain scTrack
	var like struct {
		Track *plain `json:"track"`
	}
	if json.Unmarshal(b, &like) == nil && like.Track != nil && like.Track.ID != 0 {
		*t = scTrack(*like.Track)
		return nil
	}
	return json.Unmarshal(b, (*plain)(t))
}

func (t scTrack) cover() string {
	art := t.Artwork
	if art == "" {
		art = t.User.Avatar
	}
	return strings.Replace(art, "-large.", "-t500x500.", 1)
}

func (t scTrack) track() Track {
	return Track{ID: strconv.FormatInt(t.ID, 10), Title: t.Title, Artist: t.User.Username, Artists: []string{t.User.Username}, ISRC: t.ISRC, DurationMS: t.Duration, Provider: "soundcloud", BPM: t.BPM, Key: camelotFromName(t.Key), CoverURL: t.cover()}
}

func (s *SoundCloud) base() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return defaultSoundCloudAPI
}

// headers authorises a request. An account login comes first because only it
// renews itself; a pasted token is used as is; the app's own token is the fallback.
func (s *SoundCloud) headers(ctx context.Context) (http.Header, error) {
	h := http.Header{"Accept": {"application/json; charset=utf-8"}}
	appKeys := s.Auth != nil && s.ClientID != "" && s.Secret != ""
	var token string
	var err error
	switch {
	case appKeys && s.Auth.HasUser():
		token, err = s.Auth.UserToken(ctx, s.ClientID, s.Secret)
	case s.Token != "":
		token = s.Token
	case appKeys:
		token, err = s.Auth.Token(ctx, s.ClientID, s.Secret)
	default:
		return nil, errors.New("soundcloud: not connected")
	}
	if err != nil {
		return nil, err
	}
	h.Set("Authorization", "OAuth "+token)
	return h, nil
}

func (s *SoundCloud) get(ctx context.Context, address string, out any) error {
	h, err := s.headers(ctx)
	if err != nil {
		return err
	}
	return JSON(ctx, s.HTTP, "GET", address, nil, h, out)
}

func (s *SoundCloud) list(path string, q url.Values) string {
	q.Set("linked_partitioning", "true")
	return s.base() + path + "?" + q.Encode()
}

// Identify fills in the account behind a user's token. An app token has no user.
func (s *SoundCloud) Identify(ctx context.Context) error {
	if s.UserID != 0 || (s.Token == "" && !s.Auth.HasUser()) {
		return nil
	}
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := s.get(ctx, s.base()+"/me", &me); err != nil {
		return err
	}
	s.UserID, s.Username = me.ID, me.Username
	return nil
}

// ResolveUser finds a profile from a link, a share link or a username. An app token
// is enough, since profiles are public.
func (s *SoundCloud) ResolveUser(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("soundcloud: profile required")
	}
	name := ""
	if !strings.Contains(raw, "://") {
		name = strings.TrimPrefix(raw, "@")
		raw = "https://soundcloud.com/" + url.PathEscape(name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("soundcloud: not a SoundCloud profile link")
	}
	if u.Hostname() == "on.soundcloud.com" {
		if raw, err = s.expandShareLink(ctx, raw); err != nil {
			return err
		}
		if u, err = url.Parse(raw); err != nil {
			return errors.New("soundcloud: not a SoundCloud profile link")
		}
	}
	if h := u.Hostname(); h != "soundcloud.com" && h != "www.soundcloud.com" && h != "m.soundcloud.com" {
		return errors.New("soundcloud: not a SoundCloud profile link")
	}
	var v struct {
		Kind     string `json:"kind"`
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	err = s.get(ctx, s.base()+"/resolve?"+url.Values{"url": {raw}}.Encode(), &v)
	if err == nil && v.Kind == "user" && v.ID != 0 {
		s.UserID, s.Username = v.ID, v.Username
		return nil
	}
	// A display name is not a profile address, so look it up by name.
	if name != "" {
		var found struct {
			Collection []struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"collection"`
		}
		if s.get(ctx, s.list("/users", url.Values{"q": {name}, "limit": {"1"}}), &found) == nil && len(found.Collection) > 0 {
			s.UserID, s.Username = found.Collection[0].ID, found.Collection[0].Username
			return nil
		}
	}
	if err != nil {
		return err
	}
	return errors.New("soundcloud: that link is not a profile")
}

func (s *SoundCloud) expandShareLink(ctx context.Context, raw string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return "", errors.New("soundcloud: invalid share link")
	}
	c := *client(s.HTTP)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", errors.New("soundcloud: could not open the share link")
	}
	defer resp.Body.Close()
	location, err := resp.Location()
	if err != nil {
		return "", errors.New("soundcloud: invalid share link")
	}
	location.RawQuery = ""
	return location.String(), nil
}

// nextPage keeps paging on the API, so a response cannot redirect the credentials.
func (s *SoundCloud) nextPage(next string) string {
	u, err := url.Parse(next)
	base, baseErr := url.Parse(s.base())
	if next == "" || err != nil || baseErr != nil || u.Host != base.Host || u.Scheme != base.Scheme {
		return ""
	}
	return u.String()
}

type scPlaylist struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	TrackCount int    `json:"track_count"`
}

// UnmarshalJSON also accepts a like, which wraps the playlist as {"playlist": {...}}.
func (p *scPlaylist) UnmarshalJSON(b []byte) error {
	type plain scPlaylist
	var like struct {
		Playlist *plain `json:"playlist"`
	}
	if json.Unmarshal(b, &like) == nil && like.Playlist != nil && like.Playlist.ID != 0 {
		*p = scPlaylist(*like.Playlist)
		return nil
	}
	return json.Unmarshal(b, (*plain)(p))
}

func (s *SoundCloud) playlistPages(ctx context.Context, address, prefix string, out []Playlist) []Playlist {
	for address != "" && len(out) < soundCloudListLimit {
		var r struct {
			Collection []scPlaylist `json:"collection"`
			Next       string       `json:"next_href"`
		}
		if s.get(ctx, address, &r) != nil {
			return out // what was already read is still worth showing
		}
		for _, p := range r.Collection {
			out = append(out, Playlist{strconv.FormatInt(p.ID, 10), prefix + p.Title, p.TrackCount})
		}
		address = s.nextPage(r.Next)
	}
	return out
}

// library is where the account's own lists live. A user's token also reaches private
// ones through /me; otherwise it is the public profile.
func (s *SoundCloud) library() (string, error) {
	if s.Token != "" || s.Auth.HasUser() {
		return "/me", nil
	}
	if s.UserID != 0 {
		return "/users/" + strconv.FormatInt(s.UserID, 10), nil
	}
	return "", errors.New("soundcloud: profile required")
}

// Playlists lists Likes, the account's own playlists, then the ones it liked.
func (s *SoundCloud) Playlists(ctx context.Context) ([]Playlist, error) {
	root, err := s.library()
	if err != nil {
		return nil, nil // search still works with only the app's credentials
	}
	q := func() url.Values { return url.Values{"limit": {"100"}, "show_tracks": {"false"}} }
	out := []Playlist{{ID: soundCloudLikesID, Title: "Liked Tracks & Sets（我点赞的音乐）"}}
	out = s.playlistPages(ctx, s.list(root+"/playlists", q()), "", out)
	return s.playlistPages(ctx, s.list(root+"/likes/playlists", q()), "【收藏】", out), nil
}

func (s *SoundCloud) trackPages(ctx context.Context, address string) ([]Track, error) {
	var out []Track
	for address != "" && len(out) < soundCloudListLimit {
		var r struct {
			Collection []scTrack `json:"collection"`
			Next       string    `json:"next_href"`
		}
		if err := s.get(ctx, address, &r); err != nil {
			if len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		for _, t := range r.Collection {
			if t.ID != 0 && t.Access != "blocked" {
				out = append(out, t.track())
			}
		}
		address = s.nextPage(r.Next)
	}
	return out, nil
}

func (s *SoundCloud) Tracks(ctx context.Context, id string) ([]Track, error) {
	q := url.Values{"limit": {"100"}, "access": {"playable"}}
	if id == soundCloudLikesID {
		root, err := s.library()
		if err != nil {
			return nil, err
		}
		q.Set("limit", "200")
		return s.trackPages(ctx, s.list(root+"/likes/tracks", q))
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return nil, errors.New("soundcloud: invalid playlist ID")
	}
	return s.trackPages(ctx, s.list("/playlists/"+id+"/tracks", q))
}

func (s *SoundCloud) Search(ctx context.Context, query string) ([]Track, error) {
	return s.trackPages(ctx, s.list("/tracks", url.Values{"q": {query}, "limit": {"30"}, "access": {"playable"}}))
}

func (s *SoundCloud) SearchPlaylists(ctx context.Context, query string) ([]Playlist, error) {
	var r struct {
		Collection []scPlaylist `json:"collection"`
	}
	if err := s.get(ctx, s.list("/playlists", url.Values{"q": {query}, "limit": {"20"}, "show_tracks": {"false"}}), &r); err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(r.Collection))
	for _, p := range r.Collection {
		out = append(out, Playlist{strconv.FormatInt(p.ID, 10), p.Title, p.TrackCount})
	}
	return out, nil
}

// SoundCloudMedia is a track's audio: one address, or a list of segments to join.
type SoundCloudMedia struct {
	Ext  string // mp3, or mp4 for AAC
	URLs []string
}

// Media finds where to download a track's whole audio from. The preview address is
// never used, however it is offered.
func (s *SoundCloud) Media(ctx context.Context, id string) (SoundCloudMedia, error) {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return SoundCloudMedia{}, errors.New("soundcloud: invalid track ID")
	}
	var t scTrack
	if err := s.get(ctx, s.base()+"/tracks/"+id, &t); err != nil {
		return SoundCloudMedia{}, err
	}
	if t.Access != "" && t.Access != "playable" {
		return SoundCloudMedia{}, ErrNoFullStream
	}
	var streams struct {
		HTTPMP3 string `json:"http_mp3_128_url"`
		HLSMP3  string `json:"hls_mp3_128_url"`
		HLSAAC  string `json:"hls_aac_160_url"`
	}
	if err := s.get(ctx, s.base()+"/tracks/"+id+"/streams", &streams); err != nil {
		return SoundCloudMedia{}, err
	}
	if streams.HTTPMP3 != "" {
		// That address answers with a signed redirect, which needs no credentials.
		location, err := s.signedAddress(ctx, streams.HTTPMP3)
		if err != nil {
			return SoundCloudMedia{}, err
		}
		return SoundCloudMedia{Ext: "mp3", URLs: []string{location}}, nil
	}
	ext, playlist := "mp3", streams.HLSMP3
	if playlist == "" {
		ext, playlist = "mp4", streams.HLSAAC
	}
	if playlist == "" {
		return SoundCloudMedia{}, ErrNoFullStream
	}
	location, err := s.signedAddress(ctx, playlist)
	if err != nil {
		return SoundCloudMedia{}, err
	}
	urls, err := s.segments(ctx, location)
	if err != nil {
		return SoundCloudMedia{}, err
	}
	return SoundCloudMedia{Ext: ext, URLs: urls}, nil
}

// signedAddress follows the API's redirect by hand, keeping credentials off the CDN.
func (s *SoundCloud) signedAddress(ctx context.Context, address string) (string, error) {
	h, err := s.headers(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return "", errors.New("soundcloud: invalid stream address")
	}
	req.Header = h
	c := *client(s.HTTP)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", errors.New("soundcloud: stream transfer failed")
	}
	defer resp.Body.Close()
	if location, err := resp.Location(); err == nil {
		return location.String(), nil
	}
	if resp.StatusCode == http.StatusOK {
		return address, nil
	}
	return "", errors.New("soundcloud: stream refused: HTTP " + strconv.Itoa(resp.StatusCode))
}

// segments reads an HLS playlist. An encrypted one is refused, not decrypted.
func (s *SoundCloud) segments(ctx context.Context, playlist string) ([]string, error) {
	base, err := url.Parse(playlist)
	if err != nil {
		return nil, errors.New("soundcloud: invalid playlist address")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", playlist, nil)
	if err != nil {
		return nil, errors.New("soundcloud: invalid playlist address")
	}
	resp, err := client(s.HTTP).Do(req)
	if err != nil {
		return nil, errors.New("soundcloud: playlist transfer failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("soundcloud: playlist refused")
	}
	var urls []string
	add := func(ref string) error {
		u, err := base.Parse(ref)
		if err != nil {
			return errors.New("soundcloud: invalid segment address")
		}
		urls = append(urls, u.String())
		return nil
	}
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-KEY") && !strings.Contains(line, "METHOD=NONE"):
			return nil, ErrNoFullStream
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			_, rest, _ := strings.Cut(line, `URI="`)
			ref, _, ok := strings.Cut(rest, `"`)
			if !ok {
				return nil, errors.New("soundcloud: invalid playlist")
			}
			if err := add(ref); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "#"):
		default:
			if err := add(line); err != nil {
				return nil, err
			}
		}
	}
	if err := lines.Err(); err != nil {
		return nil, errors.New("soundcloud: playlist transfer failed")
	}
	if len(urls) == 0 {
		return nil, errors.New("soundcloud: empty playlist")
	}
	return urls, nil
}
