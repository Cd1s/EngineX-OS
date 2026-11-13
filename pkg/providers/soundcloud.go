package providers

// SoundCloud through its official API with the owner's registered app: the Client ID
// and secret are exchanged for an access token, or a user's own token is used.
// Registering an app needs Artist Pro, so a deck without either reads public
// profiles through the web API soundcloud.com itself uses (see web). Previews are listed
// (Track.PreviewOnly) but never downloaded as if whole, and nothing is decrypted.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultSoundCloudAPI  = "https://api.soundcloud.com"
	defaultSoundCloudWeb  = "https://api-v2.soundcloud.com"
	defaultSoundCloudSite = "https://soundcloud.com"
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
		var st StatusError
		if errors.As(err, &st) && (st == 400 || st == 401) {
			return "", fmt.Errorf("soundcloud: invalid credentials: %w", StatusError(401))
		}
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
		a.dropUser()
		return "", fmt.Errorf("soundcloud: login expired: %w", StatusError(401))
	}
	var r soundCloudTokenResponse
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.saved.UserRefresh}}
	if err := a.post(ctx, form, id, secret, &r); err != nil {
		// A refresh token is single-use: once refused it never works again, and
		// keeping it would make HasUser() true and every request fail with it.
		var st StatusError
		if errors.As(err, &st) && (st == 400 || st == 401) {
			a.dropUser()
			return "", fmt.Errorf("soundcloud: login expired: %w", StatusError(401))
		}
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

// dropUser forgets a login that can no longer be renewed. Called with a.mu held.
func (a *SoundCloudAuth) dropUser() {
	a.saved.UserAccess, a.saved.UserRefresh, a.saved.UserExpiry = "", "", time.Time{}
	SaveJSON(a.Path, a.saved) // best effort: the in-memory state is already cleared
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
	WebURL   string // overrides the web API in tests
	SiteURL  string // overrides soundcloud.com in tests

	// goPlus caches HasGoPlus under scGoPlusMu (a package lock keeps the struct copyable).
	goPlus   bool
	goPlusAt time.Time
}

var scGoPlusMu sync.Mutex

const scGoPlusTTL = 10 * time.Minute

// HasGoPlus reports whether the logged-in account has a Go/Go+ subscription,
// which is what makes preview-only (SNIP) tracks play in full. An app token or
// public profile never does. A failed /me lookup counts as no and is retried.
func (s *SoundCloud) HasGoPlus(ctx context.Context) bool {
	if s.Token == "" && !s.Auth.HasUser() {
		return false
	}
	scGoPlusMu.Lock()
	if !s.goPlusAt.IsZero() && time.Since(s.goPlusAt) < scGoPlusTTL {
		ok := s.goPlus
		scGoPlusMu.Unlock()
		return ok
	}
	scGoPlusMu.Unlock()
	var me struct {
		Sub  *scSubscription  `json:"consumer_subscription"`
		Subs []scSubscription `json:"consumer_subscriptions"`
	}
	if err := s.get(ctx, s.base()+"/me", &me); err != nil {
		return false
	}
	ok := me.Sub.goPlus()
	for i := range me.Subs {
		ok = ok || me.Subs[i].goPlus()
	}
	scGoPlusMu.Lock()
	s.goPlus, s.goPlusAt = ok, time.Now()
	scGoPlusMu.Unlock()
	return ok
}

type scSubscription struct {
	Product struct {
		ID string `json:"id"`
	} `json:"product"`
}

func (c *scSubscription) goPlus() bool {
	if c == nil {
		return false
	}
	id := strings.ToLower(c.Product.ID)
	return strings.Contains(id, "high_tier") || strings.Contains(id, "high-tier") || strings.HasPrefix(id, "go")
}

// web reports whether the deck has no app of its own. Public profiles, likes and
// playlists are then read as soundcloud.com reads them, with the client ID its own
// pages carry; an oauth_token from a browser login adds the account's private ones.
func (s *SoundCloud) web() bool {
	return s.ClientID == "" || s.Secret == ""
}

var (
	scWebMu      sync.Mutex
	scWebIDs     = map[string]string{} // by site, shared by every instance
	scScriptRe   = regexp.MustCompile(`<script[^>]+src="(https?://[^"]+\.js)"`)
	scClientIDRe = regexp.MustCompile(`client_id\s*[:=]\s*"([A-Za-z0-9]{32})"`)
)

func (s *SoundCloud) site() string {
	if s.SiteURL != "" {
		return strings.TrimRight(s.SiteURL, "/")
	}
	return defaultSoundCloudSite
}

// webClientID finds the public client ID in soundcloud.com's scripts. SoundCloud
// changes it now and then, so a refused one is looked up again.
func (s *SoundCloud) webClientID(ctx context.Context, renew bool) (string, error) {
	site := s.site()
	scWebMu.Lock()
	if id := scWebIDs[site]; id != "" && !renew {
		scWebMu.Unlock()
		return id, nil
	}
	scWebMu.Unlock()
	page, err := s.text(ctx, site+"/")
	if err != nil {
		return "", err
	}
	scripts := scScriptRe.FindAllStringSubmatch(page, -1)
	// Usually in one of the last bundles; walk back to the first.
	for i := len(scripts) - 1; i >= 0; i-- {
		js, err := s.text(ctx, scripts[i][1])
		if err != nil {
			continue
		}
		if m := scClientIDRe.FindStringSubmatch(js); m != nil {
			scWebMu.Lock()
			scWebIDs[site] = m[1]
			scWebMu.Unlock()
			return m[1], nil
		}
	}
	return "", errors.New("soundcloud: public client ID not found")
}

func (s *SoundCloud) text(ctx context.Context, address string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client(s.HTTP).Do(req)
	if err != nil {
		return "", errors.New("soundcloud: site unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", StatusError(resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return string(b), err
}

// webGet adds the public client ID, which stays on the web API's host.
func (s *SoundCloud) webGet(ctx context.Context, address string, out any) error {
	u, err := url.Parse(address)
	if base, _ := url.Parse(s.base()); err != nil || base == nil || u.Host != base.Host {
		return errors.New("soundcloud: invalid address")
	}
	h := http.Header{"Accept": {"application/json; charset=utf-8"}}
	if s.Token != "" {
		h.Set("Authorization", "OAuth "+s.Token)
	}
	for renew := false; ; renew = true {
		id, err := s.webClientID(ctx, renew)
		if err != nil {
			return err
		}
		q := u.Query()
		q.Set("client_id", id)
		u.RawQuery = q.Encode()
		err = JSON(ctx, s.HTTP, "GET", u.String(), nil, h, out)
		var status StatusError
		if renew || !errors.As(err, &status) || (status != 401 && status != 403) {
			return err
		}
	}
}

type scTrack struct {
	ID       int64   `json:"id"`
	Title    string  `json:"title"`
	Duration int64   `json:"duration"`
	BPM      float64 `json:"bpm"`
	Key      string  `json:"key_signature"`
	ISRC     string  `json:"isrc"`
	Access   string  `json:"access"`
	Policy   string  `json:"policy"` // web API: SNIP is a preview, BLOCK nothing
	Artwork  string  `json:"artwork_url"`
	// Set when the uploader lets listeners download their original file.
	Downloadable bool   `json:"downloadable"`
	DownloadURL  string `json:"download_url"`
	User         struct {
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
	return Track{ID: strconv.FormatInt(t.ID, 10), Title: t.Title, Artist: t.User.Username, Artists: []string{t.User.Username}, ISRC: t.ISRC, DurationMS: t.Duration, Provider: "soundcloud", BPM: t.BPM, Key: camelotFromName(t.Key), CoverURL: t.cover(), PreviewOnly: t.Policy == "SNIP" || t.Access == "preview"}
}

func (s *SoundCloud) base() string {
	if s.web() {
		if s.WebURL != "" {
			return strings.TrimRight(s.WebURL, "/")
		}
		return defaultSoundCloudWeb
	}
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
	if err != nil && appKeys && !s.Auth.HasUser() && (s.Token != "" || s.UserID != 0) {
		// The account login expired for good: carry on as without one when a pasted token or public UserID exists.
		if token, err = s.Token, nil; token == "" {
			token, err = s.Auth.Token(ctx, s.ClientID, s.Secret)
		}
	}
	if err != nil {
		return nil, err
	}
	h.Set("Authorization", "OAuth "+token)
	return h, nil
}

func (a *SoundCloudAuth) invalidateUser(stale string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.load()
	if stale != "" && a.saved.UserAccess == stale {
		a.saved.UserExpiry = time.Time{}
	}
}

func (s *SoundCloud) get(ctx context.Context, address string, out any) error {
	if s.web() {
		return s.webGet(ctx, address, out)
	}
	hadUser := s.Auth != nil && s.Auth.HasUser()
	h, err := s.headers(ctx)
	if err != nil {
		return err
	}
	err = JSON(ctx, s.HTTP, "GET", address, nil, h, out)
	var st StatusError
	if hadUser && errors.As(err, &st) && st == 401 && s.ClientID != "" && s.Secret != "" {
		s.Auth.invalidateUser(strings.TrimPrefix(h.Get("Authorization"), "OAuth "))
		if h2, rerr := s.headers(ctx); rerr == nil {
			return JSON(ctx, s.HTTP, "GET", address, nil, h2, out)
		}
	}
	return err
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
		if s.get(ctx, s.list(s.path("/users", "/search/users"), url.Values{"q": {name}, "limit": {"1"}}), &found) == nil && len(found.Collection) > 0 {
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

// path picks the official API's path or the web API's.
func (s *SoundCloud) path(official, web string) string {
	if s.web() {
		return web
	}
	return official
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

func (s *SoundCloud) playlistPages(ctx context.Context, address, prefix string, out []Playlist) ([]Playlist, error) {
	for address != "" && len(out) < soundCloudListLimit {
		var r struct {
			Collection []scPlaylist `json:"collection"`
			Next       string       `json:"next_href"`
		}
		if err := s.get(ctx, address, &r); err != nil {
			return out, err
		}
		for _, p := range r.Collection {
			out = append(out, Playlist{strconv.FormatInt(p.ID, 10), prefix + p.Title, p.TrackCount})
		}
		address = s.nextPage(r.Next)
	}
	return out, nil
}

// library is where the account's own lists live. A user's token also reaches private
// ones through /me; otherwise it is the public profile.
func (s *SoundCloud) library() (string, error) {
	if s.Token != "" || (!s.web() && s.Auth.HasUser()) {
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
	out, err = s.playlistPages(ctx, s.list(root+"/playlists", q()), "", out)
	var st StatusError
	if len(out) == 1 && errors.As(err, &st) && st == 404 && s.web() && root == "/me" && s.UserID != 0 {
		root = "/users/" + strconv.FormatInt(s.UserID, 10)
		out, err = s.playlistPages(ctx, s.list(root+"/playlists", q()), "", out)
	}
	if err != nil {
		return out, err
	}
	likesPath := s.path("/likes/playlists", "/playlist_likes")
	beforeLikes := len(out)
	var likeErr error
	out, likeErr = s.playlistPages(ctx, s.list(root+likesPath, q()), "【收藏】", out)
	if len(out) == beforeLikes && errors.As(likeErr, &st) && st == 404 && s.web() && root == "/me" && s.UserID != 0 {
		out, likeErr = s.playlistPages(ctx, s.list("/users/"+strconv.FormatInt(s.UserID, 10)+likesPath, q()), "【收藏】", out)
	}
	if likeErr != nil {
		var lst StatusError
		if len(out) == beforeLikes && errors.As(likeErr, &lst) && lst == 404 {
			return out, nil
		}
		return out, likeErr
	}
	return out, nil
}

func (s *SoundCloud) trackPages(ctx context.Context, address string) ([]Track, error) {
	var out []Track
	for address != "" && len(out) < soundCloudListLimit {
		var r struct {
			Collection []scTrack `json:"collection"`
			Next       string    `json:"next_href"`
		}
		if err := s.get(ctx, address, &r); err != nil {
			return out, err
		}
		for _, t := range r.Collection {
			if t.ID != 0 && t.Access != "blocked" && t.Policy != "BLOCK" {
				out = append(out, t.track())
			}
		}
		address = s.nextPage(r.Next)
	}
	return out, nil
}

func (s *SoundCloud) Tracks(ctx context.Context, id string) ([]Track, error) {
	q := url.Values{"limit": {"100"}, "access": {"playable,preview"}}
	if id == soundCloudLikesID {
		root, err := s.library()
		if err != nil {
			return nil, err
		}
		address := func() string { return s.list(root+s.path("/likes/tracks", "/track_likes"), q) }
		out, err := s.trackPages(ctx, address())
		var st StatusError
		if len(out) == 0 && errors.As(err, &st) && st == 400 {
			q.Del("access") // some accounts' likes refuse the filter
			out, err = s.trackPages(ctx, address())
		}
		if len(out) == 0 && errors.As(err, &st) && st == 404 && s.web() && root == "/me" && s.UserID != 0 {
			out, err = s.trackPages(ctx, s.list("/users/"+strconv.FormatInt(s.UserID, 10)+"/track_likes", q))
		}
		if len(out) == 0 && errors.As(err, &st) && (st == 400 || st == 401 || st == 403) && !s.web() && s.UserID != 0 {
			// The login or app token is refused: a public profile's likes still read.
			w := &SoundCloud{
				BaseURL:  s.BaseURL,
				HTTP:     s.HTTP,
				UserID:   s.UserID,
				Username: s.Username,
				WebURL:   s.WebURL,
				SiteURL:  s.SiteURL,
			}
			return w.Tracks(ctx, id)
		}
		return out, err
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return nil, errors.New("soundcloud: invalid playlist ID")
	}
	if s.web() {
		return s.webPlaylistTracks(ctx, id)
	}
	return s.trackPages(ctx, s.list("/playlists/"+id+"/tracks", q))
}

// webPlaylistTracks reads a playlist from the web API, which fills in only its first
// few tracks; the rest come by ID, 50 at a time.
func (s *SoundCloud) webPlaylistTracks(ctx context.Context, id string) ([]Track, error) {
	var list struct {
		Tracks []scTrack `json:"tracks"`
	}
	if err := s.get(ctx, s.base()+"/playlists/"+id, &list); err != nil {
		return nil, err
	}
	full := map[int64]scTrack{}
	var missing []string
	for _, t := range list.Tracks {
		if t.Title != "" {
			full[t.ID] = t
		} else if t.ID != 0 {
			missing = append(missing, strconv.FormatInt(t.ID, 10))
		}
	}
	// A successful batch may omit unavailable IDs; only a failed batch ends
	// the prefix, including any pre-expanded tracks after that boundary.
	complete := make([]bool, (len(missing)+49)/50)
	got, err := collectBatches(ctx, soundcloudPages, len(complete), func(batch int) ([]scTrack, error) {
		start := batch * 50
		var tracks []scTrack
		err := s.get(ctx, s.base()+"/tracks?"+url.Values{"ids": {strings.Join(missing[start:min(start+50, len(missing))], ",")}}.Encode(), &tracks)
		complete[batch] = err == nil
		return tracks, err
	})
	stopID := ""
	for batch, complete := range complete {
		if !complete {
			stopID = missing[batch*50]
			break
		}
	}
	for _, t := range got {
		full[t.ID] = t
	}
	var out []Track
	for _, ref := range list.Tracks {
		if err != nil && strconv.FormatInt(ref.ID, 10) == stopID {
			break
		}
		if t, ok := full[ref.ID]; ok && t.Policy != "BLOCK" && len(out) < soundCloudListLimit {
			out = append(out, t.track())
		}
	}
	return out, err
}

func (s *SoundCloud) Search(ctx context.Context, query string) ([]Track, error) {
	// Search is a single page, unlike a library/playlist. Following next_href
	// here can fetch up to 2,000 hits and consume the gateway's whole deadline.
	var r struct {
		Collection []scTrack `json:"collection"`
	}
	if err := s.get(ctx, s.list(s.path("/tracks", "/search/tracks"), url.Values{"q": {query}, "limit": {"30"}, "access": {"playable,preview"}}), &r); err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(r.Collection))
	for _, t := range r.Collection {
		if t.ID != 0 && t.Access != "blocked" && t.Policy != "BLOCK" {
			out = append(out, t.track())
		}
	}
	return out, nil
}

func (s *SoundCloud) SearchPlaylists(ctx context.Context, query string) ([]Playlist, error) {
	var r struct {
		Collection []scPlaylist `json:"collection"`
	}
	if err := s.get(ctx, s.list(s.path("/playlists", "/search/playlists"), url.Values{"q": {query}, "limit": {"20"}, "show_tracks": {"false"}}), &r); err != nil {
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
	// Original is the uploader's own file (often WAV or FLAC), offered only when
	// lossless was asked for and the uploader allows downloads.
	Original string
}

// Media finds where to download a track's whole audio from. The preview address is
// never used, however it is offered. Lossless also looks for the original upload,
// which is far larger: a stream of an hour-long set is 60 MB, the WAV 600 MB.
func (s *SoundCloud) Media(ctx context.Context, id string, lossless bool) (SoundCloudMedia, error) {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return SoundCloudMedia{}, errors.New("soundcloud: invalid track ID")
	}
	if s.web() {
		return s.webMedia(ctx, id, lossless)
	}
	var t scTrack
	if err := s.get(ctx, s.base()+"/tracks/"+id, &t); err != nil {
		return SoundCloudMedia{}, err
	}
	if t.Access != "" && t.Access != "playable" {
		return SoundCloudMedia{}, ErrNoFullStream
	}
	var m SoundCloudMedia
	if lossless && t.Downloadable && t.DownloadURL != "" {
		// The address is built here, not taken from the reply, so the login is only
		// ever sent to the API. A refused download (a limit, a changed setting)
		// still leaves the stream.
		address := s.base() + "/tracks/" + id + "/download"
		if location, err := s.signedAddress(ctx, address); err == nil && location != address {
			m.Original = location
		}
	}
	var streams struct {
		HTTPMP3   string `json:"http_mp3_128_url"`
		HLSMP3    string `json:"hls_mp3_128_url"`
		HLSAAC    string `json:"hls_aac_160_url"`
		HLSAAC256 string `json:"hls_aac_256_url"`
	}
	if err := s.get(ctx, s.base()+"/tracks/"+id+"/streams", &streams); err != nil {
		if m.Original != "" {
			return m, nil
		}
		return SoundCloudMedia{}, err
	}
	// Best first: 256 and 160 kbps AAC, then the 128 kbps MP3 (plain, then HLS).
	var lastErr error
	for _, c := range []struct{ address, ext string }{
		{streams.HLSAAC256, "mp4"}, {streams.HLSAAC, "mp4"}, {streams.HTTPMP3, "mp3"}, {streams.HLSMP3, "mp3"},
	} {
		if c.address == "" {
			continue
		}
		location, err := s.signedAddress(ctx, c.address)
		var urls []string
		if err == nil {
			urls = []string{location}
			// The plain MP3 address answers with a signed redirect, which needs no credentials.
			if c.address != streams.HTTPMP3 {
				urls, err = s.segments(ctx, location)
			}
		}
		if err != nil {
			lastErr = err
			continue
		}
		m.Ext, m.URLs = c.ext, urls
		return m, nil
	}
	if m.Original != "" {
		return m, nil
	}
	if lastErr != nil {
		return SoundCloudMedia{}, lastErr
	}
	return SoundCloudMedia{}, ErrNoFullStream
}

// webMedia picks the best full-length stream from the web API: 256 kbps AAC (hq), 160 kbps
// AAC, then 128 kbps MP3 (plain, then HLS). Encrypted streams are refused. Lossless also
// asks for the uploader's original when the track allows downloads.
func (s *SoundCloud) webMedia(ctx context.Context, id string, lossless bool) (SoundCloudMedia, error) {
	var t struct {
		Policy           string `json:"policy"`
		Auth             string `json:"track_authorization"`
		Downloadable     bool   `json:"downloadable"`
		HasDownloadsLeft bool   `json:"has_downloads_left"`
		Media            struct {
			Transcodings []struct {
				URL     string `json:"url"`
				Snipped bool   `json:"snipped"`
				Quality string `json:"quality"`
				Preset  string `json:"preset"`
				Format  struct {
					Protocol string `json:"protocol"`
					MimeType string `json:"mime_type"`
				} `json:"format"`
			} `json:"transcodings"`
		} `json:"media"`
	}
	if err := s.get(ctx, s.base()+"/tracks/"+id, &t); err != nil {
		return SoundCloudMedia{}, err
	}
	if t.Policy == "SNIP" || t.Policy == "BLOCK" {
		return SoundCloudMedia{}, ErrNoFullStream
	}
	var m SoundCloudMedia
	if lossless && t.Downloadable && t.HasDownloadsLeft {
		var dl struct {
			RedirectURI string `json:"redirectUri"`
		}
		if s.get(ctx, s.base()+"/tracks/"+id+"/download", &dl) == nil {
			m.Original = dl.RedirectURI
		}
	}
	for _, want := range []struct {
		protocol, mime, ext string
		hq                  bool
	}{
		{"hls", "audio/mp4", "mp4", true}, {"hls", "audio/mp4", "mp4", false},
		{"progressive", "audio/mpeg", "mp3", false}, {"hls", "audio/mpeg", "mp3", false},
	} {
		for _, c := range t.Media.Transcodings {
			if c.Snipped || c.Format.Protocol != want.protocol || !strings.HasPrefix(c.Format.MimeType, want.mime) {
				continue
			}
			if want.hq && c.Quality != "hq" && !strings.Contains(c.Preset, "256") {
				continue
			}
			var stream struct {
				URL string `json:"url"`
			}
			address := c.URL
			if t.Auth != "" {
				sep := "?"
				if strings.Contains(address, "?") {
					sep = "&"
				}
				address += sep + url.Values{"track_authorization": {t.Auth}}.Encode()
			}
			if s.get(ctx, address, &stream) != nil || stream.URL == "" {
				continue
			}
			if want.protocol == "progressive" {
				return SoundCloudMedia{Ext: want.ext, URLs: []string{stream.URL}, Original: m.Original}, nil
			}
			if urls, err := s.segments(ctx, stream.URL); err == nil {
				return SoundCloudMedia{Ext: want.ext, URLs: urls, Original: m.Original}, nil
			}
		}
	}
	if m.Original != "" {
		return m, nil
	}
	return SoundCloudMedia{}, ErrNoFullStream
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
