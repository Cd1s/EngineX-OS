package providers

import (
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
	"strings"
	"sync"
	"time"
)

// AppleMusic reads playlists from Apple Music's catalog (public playlist and
// album links, no account) and, with the owner's media-user-token, from their
// library (#65). Apple Music audio is FairPlay-protected and is never fetched:
// its tracks play through the deck's ISRC / artist + title matching, as
// Spotify's do.
type AppleMusic struct {
	BaseURL, WebURL string // api.music.apple.com and music.apple.com; tests override
	HTTP            *http.Client
	OwnerPath       string // where the owner's links and token are saved, mode 0600

	mu    sync.Mutex
	owner AppleMusicOwner
	web   string // developer token scraped from the web player
	webAt time.Time
}

// AppleMusicOwner is what the phone page saves.
type AppleMusicOwner struct {
	Links      []string `json:"links,omitempty"`
	UserToken  string   `json:"mediaUserToken,omitempty"`
	Storefront string   `json:"storefront,omitempty"`
	DevToken   string   `json:"developerToken,omitempty"` // optional override of the web player's token
}

// AppleMusicLink is one parsed music.apple.com playlist or album address.
type AppleMusicLink struct{ Storefront, Kind, ID string }

// URL is the short form saved for the owner.
func (l AppleMusicLink) URL() string {
	sf := l.Storefront
	if sf == "" {
		sf = "us"
	}
	return "https://music.apple.com/" + sf + "/" + l.Kind + "/" + l.ID
}

var (
	amLinkRE    = regexp.MustCompile(`^https?://(?:geo\.|beta\.)?music\.apple\.com/(?:([a-z]{2})/)?(playlist|album)/(?:[^/?#]+/)?((?:pl\.[A-Za-z0-9._-]+)|[0-9]+)`)
	amStore     = regexp.MustCompile(`^[a-z]{2}$`)
	amScriptTag = regexp.MustCompile(`(?i)<(?:script|link)\b[^>]*\b(?:src|href)=["']([^"']+\.js(?:\?[^"'\s>]*)?)["']`)
	amAssetPath = regexp.MustCompile(`/assets/[^"'\s<>]+\.js(?:\?[^"'\s<>]*)?`)
	amJWT       = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}`)
)

const maxAppleMusicScripts = 8

// ParseAppleMusicLinks finds every playlist or album link in pasted text, as
// Share → Copy Link gives them. Unknown text is ignored.
func ParseAppleMusicLinks(text string) []AppleMusicLink {
	var out []AppleMusicLink
	index := map[string]int{}
	for _, f := range strings.Fields(text) {
		f = strings.Trim(f, `<>"'()[],;`)
		m := amLinkRE.FindStringSubmatch(f)
		if m == nil {
			continue
		}
		sf, kind, id := m[1], m[2], m[3]
		key := kind + ":" + id
		if idx, seen := index[key]; seen {
			if out[idx].Storefront == "" && sf != "" {
				out[idx].Storefront = sf
			}
			continue
		}
		index[key] = len(out)
		out = append(out, AppleMusicLink{Storefront: sf, Kind: kind, ID: id})
	}
	return out
}

func (a *AppleMusic) base() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return "https://api.music.apple.com"
}

func (a *AppleMusic) webURL() string {
	if a.WebURL != "" {
		return strings.TrimRight(a.WebURL, "/")
	}
	return "https://music.apple.com"
}

// Load reads the saved owner login; a missing file is no login.
func (a *AppleMusic) Load() error {
	b, err := os.ReadFile(a.OwnerPath)
	if err != nil {
		return err
	}
	var o AppleMusicOwner
	if json.Unmarshal(b, &o) != nil {
		return errors.New("applemusic: invalid owner file")
	}
	a.mu.Lock()
	a.owner = o
	a.mu.Unlock()
	return nil
}

// SetOwner checks and saves a login from the phone page. Links that do not
// open are refused, so a typo never becomes an empty playlist on the deck.
func (a *AppleMusic) SetOwner(ctx context.Context, o AppleMusicOwner) error {
	o.Storefront = strings.ToLower(strings.TrimSpace(o.Storefront))
	if o.Storefront == "" {
		o.Storefront = "us"
	}
	if !amStore.MatchString(o.Storefront) {
		return errors.New("applemusic: storefront must be two letters, like us or cn")
	}
	o.UserToken, o.DevToken = strings.TrimSpace(o.UserToken), strings.TrimSpace(o.DevToken)
	if len(o.Links) == 0 && o.UserToken == "" {
		return errors.New("applemusic: paste a playlist link or a media-user-token")
	}
	check := &AppleMusic{BaseURL: a.BaseURL, WebURL: a.WebURL, HTTP: a.HTTP, owner: o}
	for _, l := range o.Links {
		if _, err := check.linkPlaylist(ctx, l); err != nil {
			return fmt.Errorf("applemusic: %s does not open: %w", l, err)
		}
	}
	if o.UserToken != "" {
		if _, err := check.libraryPlaylists(ctx, 1); err != nil {
			return fmt.Errorf("applemusic: the media-user-token was refused: %w", err)
		}
	}
	if err := SaveJSON(a.OwnerPath, o); err != nil {
		return err
	}
	a.mu.Lock()
	a.owner = o
	a.mu.Unlock()
	return nil
}

// Ready reports whether there is anything to show: a link or a library login.
func (a *AppleMusic) Ready() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.owner.Links) > 0 || a.owner.UserToken != ""
}

// Owner is the saved login, for the phone page to amend.
func (a *AppleMusic) Owner() AppleMusicOwner {
	a.mu.Lock()
	defer a.mu.Unlock()
	o := a.owner
	o.Links = append([]string(nil), o.Links...)
	return o
}

func (a *AppleMusic) state() AppleMusicOwner {
	a.mu.Lock()
	defer a.mu.Unlock()
	o := a.owner
	if o.Storefront == "" {
		o.Storefront = "us"
	}
	return o
}

// DeveloperToken is the token the phone page hands to MusicKit JS.
func (a *AppleMusic) DeveloperToken(ctx context.Context) (string, error) {
	return a.developerToken(ctx)
}

// developerToken is the owner's own token, or the one Apple's web player
// ships in its script bundle, refreshed a day before it expires.
func (a *AppleMusic) developerToken(ctx context.Context) (string, error) {
	if t := a.state().DevToken; t != "" {
		return t, nil
	}
	a.mu.Lock()
	if a.web != "" && time.Now().Before(a.webAt) {
		t := a.web
		a.mu.Unlock()
		return t, nil
	}
	a.mu.Unlock()
	page, err := a.fetch(ctx, a.webURL()+"/us/browse", 4<<20)
	if err != nil {
		return "", fmt.Errorf("applemusic: web player: %w", err)
	}
	token := findDeveloperToken(page)
	if token == "" {
		if unescaped, uerr := url.QueryUnescape(string(page)); uerr == nil {
			token = findDeveloperToken([]byte(unescaped))
		}
	}
	if token == "" {
		scripts := a.webPlayerScripts(page)
		if len(scripts) == 0 {
			return "", errors.New("applemusic: web player script not found")
		}
		var lastErr error
		fetchedAny := false
		for _, scriptURL := range scripts {
			js, ferr := a.fetch(ctx, scriptURL, 32<<20)
			if ferr != nil {
				lastErr = ferr
				continue
			}
			fetchedAny = true
			if token = findDeveloperToken(js); token != "" {
				break
			}
		}
		if token == "" {
			if !fetchedAny && lastErr != nil {
				return "", fmt.Errorf("applemusic: web player script: %w", lastErr)
			}
			return "", errors.New("applemusic: no developer token in the web player")
		}
	}
	until := time.Now().Add(12 * time.Hour)
	if exp := jwtExpiry(token); !exp.IsZero() {
		until = exp.Add(-24 * time.Hour)
	}
	a.mu.Lock()
	a.web, a.webAt = token, until
	a.mu.Unlock()
	return token, nil
}

func (a *AppleMusic) webPlayerScripts(page []byte) []string {
	baseURL, err := url.Parse(a.webURL())
	if err != nil || baseURL.Host == "" {
		return nil
	}
	allowHost := func(host string) bool {
		if strings.EqualFold(host, baseURL.Host) {
			return true
		}
		if a.WebURL != "" {
			return false
		}
		h := strings.ToLower(strings.Split(host, ":")[0])
		return h == "music.apple.com" || strings.HasSuffix(h, ".music.apple.com")
	}

	var priority, others []string
	seen := map[string]bool{}
	add := func(raw string) {
		u, uerr := baseURL.Parse(strings.TrimSpace(raw))
		if uerr != nil || (u.Scheme != "http" && u.Scheme != "https") || !allowHost(u.Host) {
			return
		}
		resolved := u.String()
		if seen[resolved] {
			return
		}
		seen[resolved] = true
		if strings.Contains(u.Path, "/assets/index") {
			priority = append(priority, resolved)
		} else {
			others = append(others, resolved)
		}
	}
	for _, m := range amScriptTag.FindAllSubmatch(page, -1) {
		add(string(m[1]))
	}
	for _, m := range amAssetPath.FindAll(page, -1) {
		add(string(m))
	}
	all := append(priority, others...)
	if len(all) > maxAppleMusicScripts {
		all = all[:maxAppleMusicScripts]
	}
	return all
}

type amJWTHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type amJWTClaims struct {
	Iss string `json:"iss"`
	Exp int64  `json:"exp"`
}

func parseJWT(token string) (header amJWTHeader, claims amJWTClaims, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return header, claims, false
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	cb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	if err1 != nil || err2 != nil {
		return header, claims, false
	}
	if json.Unmarshal(hb, &header) != nil || json.Unmarshal(cb, &claims) != nil {
		return header, claims, false
	}
	return header, claims, true
}

// findDeveloperToken extracts the web player's MusicKit developer token from
// Apple Music's JavaScript bundle. It prefers a non-expired token issued for
// AMPWebPlay (or with kid: WebPlayKid), which has permission to access the
// catalog and library API from web origins, falling back to any non-expired JWT.
func findDeveloperToken(js []byte) string {
	matches := amJWT.FindAll(js, -1)
	if len(matches) == 0 {
		return ""
	}
	now := time.Now()
	var webPlayToken string
	var firstValidToken string

	for _, m := range matches {
		tok := string(m)
		header, claims, ok := parseJWT(tok)
		if !ok {
			continue
		}
		if claims.Exp <= now.Unix() {
			continue
		}
		if (claims.Iss == "AMPWebPlay" || header.Kid == "WebPlayKid") && webPlayToken == "" {
			webPlayToken = tok
		}
		if firstValidToken == "" {
			firstValidToken = tok
		}
	}
	if webPlayToken != "" {
		return webPlayToken
	}
	return firstValidToken
}

func jwtExpiry(token string) time.Time {
	_, claims, ok := parseJWT(token)
	if !ok || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func (a *AppleMusic) fetch(ctx context.Context, u string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15")
	resp, err := client(a.HTTP).Do(req)
	if err != nil {
		return nil, errors.New("HTTP transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, StatusError(resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

// get calls the API. A 401 drops a scraped token once, since Apple rotates it.
func (a *AppleMusic) get(ctx context.Context, path string, user bool, out any) error {
	for try := 0; ; try++ {
		token, err := a.developerToken(ctx)
		if err != nil {
			return err
		}
		h := http.Header{"Authorization": {"Bearer " + token}, "Origin": {"https://music.apple.com"}}
		if user {
			h.Set("Media-User-Token", a.state().UserToken)
		}
		err = JSON(ctx, a.HTTP, "GET", a.base()+path, nil, h, out)
		var status StatusError
		if try == 0 && errors.As(err, &status) && status == 401 && a.state().DevToken == "" {
			a.mu.Lock()
			a.web = ""
			a.mu.Unlock()
			continue
		}
		return err
	}
}

type amResource struct {
	ID         string
	Type       string
	Attributes struct {
		Name, ArtistName, AlbumName, ISRC string
		DurationInMillis                  int64
		TrackCount                        int
		Artwork                           struct{ URL string }
		PlayParams                        struct{ CatalogID string }
	}
	Relationships struct {
		Catalog struct{ Data []amResource }
		Tracks  struct {
			Data []amResource
			Next string
		}
	}
}

type amPage struct {
	Data []amResource
	Next string
}

func artwork(u string, size int) string {
	s := fmt.Sprint(size)
	return strings.NewReplacer("{w}", s, "{h}", s, "{f}", "jpg").Replace(u)
}

func (r amResource) track() (Track, bool) {
	at := r.Attributes
	// A library song names its catalog twin, which carries the ISRC.
	if len(r.Relationships.Catalog.Data) > 0 {
		c := r.Relationships.Catalog.Data[0].Attributes
		if at.ISRC == "" {
			at.ISRC = c.ISRC
		}
		if at.Artwork.URL == "" {
			at.Artwork = c.Artwork
		}
	}
	if (r.Type != "songs" && r.Type != "library-songs") || at.Name == "" {
		return Track{}, false // music videos and unavailable rows
	}
	t := Track{ID: r.ID, Title: at.Name, Artist: at.ArtistName, Album: at.AlbumName, ISRC: at.ISRC, DurationMS: at.DurationInMillis, Provider: "applemusic"}
	if at.ArtistName != "" {
		t.Artists = []string{at.ArtistName}
	}
	if at.Artwork.URL != "" {
		t.CoverURL, t.ThumbURL = artwork(at.Artwork.URL, 600), artwork(at.Artwork.URL, 120)
	}
	return t, true
}

// pages follows "next" links until max items or the end.
func (a *AppleMusic) pages(ctx context.Context, path string, user bool, max int) ([]amResource, error) {
	var all []amResource
	for path != "" && len(all) < max {
		var p amPage
		if err := a.get(ctx, path, user, &p); err != nil {
			return all, err
		}
		all = append(all, p.Data...)
		path = p.Next
		if path != "" && !strings.Contains(path, "limit=") {
			path += map[bool]string{true: "&", false: "?"}[strings.Contains(path, "?")] + "limit=100"
		}
	}
	return all, nil
}

// Playlist IDs: "pl:<storefront>:<id>", "al:<storefront>:<id>", "lib:<id>".
func linkID(l AppleMusicLink) string {
	return map[string]string{"playlist": "pl", "album": "al"}[l.Kind] + ":" + l.Storefront + ":" + l.ID
}

func (a *AppleMusic) linkPlaylist(ctx context.Context, raw string) (Playlist, error) {
	links := ParseAppleMusicLinks(raw)
	if len(links) != 1 {
		return Playlist{}, errors.New("not an Apple Music playlist or album link")
	}
	l := links[0]
	if l.Storefront == "" {
		l.Storefront = a.state().Storefront
	}
	var p amPage
	if err := a.get(ctx, "/v1/catalog/"+l.Storefront+"/"+l.Kind+"s/"+url.PathEscape(l.ID), false, &p); err != nil {
		return Playlist{}, err
	}
	if len(p.Data) == 0 {
		return Playlist{}, StatusError(404)
	}
	r := p.Data[0]
	count := r.Attributes.TrackCount
	if count == 0 {
		count = len(r.Relationships.Tracks.Data)
	}
	return Playlist{ID: linkID(l), Title: r.Attributes.Name, Count: count}, nil
}

func (a *AppleMusic) libraryPlaylists(ctx context.Context, max int) ([]Playlist, error) {
	rs, err := a.pages(ctx, "/v1/me/library/playlists?limit=100", true, max)
	var out []Playlist
	for _, r := range rs {
		out = append(out, Playlist{ID: "lib:" + r.ID, Title: r.Attributes.Name})
	}
	return out, err
}

// Playlists lists the saved links first, then the library. One broken link
// does not hide the others.
func (a *AppleMusic) Playlists(ctx context.Context) ([]Playlist, error) {
	o := a.state()
	var out []Playlist
	var firstErr error
	for _, l := range o.Links {
		p, err := a.linkPlaylist(ctx, l)
		if err != nil {
			firstErr = keepFirst(firstErr, err)
			continue
		}
		out = append(out, p)
	}
	if o.UserToken != "" {
		lib, err := a.libraryPlaylists(ctx, 1000)
		out = append(out, lib...)
		if err != nil {
			firstErr = keepFirst(firstErr, err)
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func keepFirst(old, err error) error {
	if old != nil {
		return old
	}
	return err
}

func (a *AppleMusic) Tracks(ctx context.Context, id string) ([]Track, error) {
	var path string
	user := false
	switch kind, rest, _ := strings.Cut(id, ":"); kind {
	case "pl", "al":
		sf, cid, ok := strings.Cut(rest, ":")
		if !ok || !amStore.MatchString(sf) {
			return nil, errors.New("applemusic: invalid playlist id")
		}
		path = "/v1/catalog/" + sf + "/" + map[string]string{"pl": "playlists", "al": "albums"}[kind] + "/" + url.PathEscape(cid) + "/tracks?limit=100"
	case "lib":
		path, user = "/v1/me/library/playlists/"+url.PathEscape(rest)+"/tracks?limit=100&include=catalog", true
	default:
		return nil, errors.New("applemusic: invalid playlist id")
	}
	rs, err := a.pages(ctx, path, user, 20000)
	var out []Track
	for _, r := range rs {
		if t, ok := r.track(); ok {
			out = append(out, t)
		}
	}
	if len(out) == 0 && err != nil {
		return nil, err
	}
	return out, nil
}

// Search looks up songs in the owner's storefront; no account is needed.
func (a *AppleMusic) Search(ctx context.Context, query string) ([]Track, error) {
	var r struct {
		Results struct{ Songs amPage }
	}
	q := url.Values{"term": {query}, "types": {"songs"}, "limit": {"20"}}
	if err := a.get(ctx, "/v1/catalog/"+a.state().Storefront+"/search?"+q.Encode(), false, &r); err != nil {
		return nil, err
	}
	var out []Track
	for _, s := range r.Results.Songs.Data {
		if t, ok := s.track(); ok {
			out = append(out, t)
		}
	}
	return out, nil
}
