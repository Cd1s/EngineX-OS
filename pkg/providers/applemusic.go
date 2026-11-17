package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// AppleMusic reads playlists from Apple Music's catalog (public playlist and
// album links, no account) and, with the owner's media-user-token, from their
// library (#65). Audio downloads are supplied by the enginex-amd helper.
type AppleMusic struct {
	BaseURL, WebURL string // api.music.apple.com and music.apple.com; tests override
	HTTP            *http.Client
	OwnerPath       string // where the owner's links and token are saved, mode 0600
	// HelperURL is the enginex-amd helper (LAN/VPS); empty disables downloads.
	HelperURL, HelperLoginURL, HelperToken string

	mu             sync.Mutex
	saveMu         sync.Mutex
	probeMu        sync.Mutex
	owner          AppleMusicOwner
	credGen        uint64
	helperSignedIn bool
	helperDevToken string // developer token synced from enginex-amd (fallback if web scrape fails)
	helperProbedAt time.Time
	web            string // developer token scraped from the web player
	webAt          time.Time
}

// AppleMusicLikedID names the owner's library & favourite songs view.
const AppleMusicLikedID = "liked"

const amLibraryAlbumPrefix = "libal:"

// AppleMusicAlbum is a saved album from the owner's library or pasted album links.
type AppleMusicAlbum struct {
	ID         string
	ListID     string
	Name       string
	ArtistName string
	TrackCount int
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
	a.saveMu.Lock()
	err := SaveJSON(a.OwnerPath, o)
	a.saveMu.Unlock()
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.owner = o
	a.credGen++
	a.mu.Unlock()
	return nil
}

// CredGen returns a monotonic counter incremented when owner credentials change.
func (a *AppleMusic) CredGen() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.credGen
}

// Forget clears the owner's account credentials and links in memory and removes
// the persisted owner file, leaving any configured download helper intact.
func (a *AppleMusic) Forget() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	a.owner = AppleMusicOwner{}
	a.credGen++
	a.helperSignedIn = false
	a.helperDevToken = ""
	a.helperProbedAt = time.Time{}
	a.web = ""
	a.webAt = time.Time{}
	path := a.OwnerPath
	a.mu.Unlock()
	if path == "" {
		return nil
	}
	a.saveMu.Lock()
	err := os.Remove(path)
	a.saveMu.Unlock()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Logout clears the owner's account credentials on sign-out.
func (a *AppleMusic) Logout() error {
	return a.Forget()
}

// Download asks the enginex-amd helper for a FLAC of t. The helper owns the
// Apple Music session; the deck only receives the finished file.
// AppleMusicAudio is the helper's finished file and quality.
type AppleMusicAudio struct {
	Path       string
	SampleRate int
	Channels   int
	Bits       int
	Lossless   bool
}

func (a *AppleMusic) Download(ctx context.Context, t Track) (*AppleMusicAudio, error) {
	return a.DownloadTo(ctx, t, "", "")
}

// DownloadTo writes on the destination filesystem and publishes only a complete file.
func (a *AppleMusic) DownloadTo(ctx context.Context, t Track, dstDir, name string) (*AppleMusicAudio, error) {
	cfg := a.HelperConfig()
	if cfg.URL == "" {
		return nil, errors.New("applemusic: download helper is not configured")
	}
	if dstDir != "" && (name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\`)) {
		return nil, errors.New("applemusic: invalid cache name")
	}
	songID := t.CatalogID
	if songID == "" {
		songID = t.ID
	}
	if strings.HasPrefix(songID, "i.") {
		var catalog amPage
		if err := a.get(ctx, "/v1/me/library/songs/"+url.PathEscape(songID)+"/catalog", true, &catalog); err != nil {
			return nil, errors.New("applemusic: could not resolve library song catalog ID")
		}
		if len(catalog.Data) == 0 || catalog.Data[0].ID == "" {
			return nil, errors.New("applemusic: library song has no catalog recording")
		}
		songID = catalog.Data[0].ID
	}
	if songID == "" {
		return nil, errors.New("applemusic: song ID is missing")
	}
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	// Never forward the helper credential across redirects.
	copyClient := *hc
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	hc = &copyClient
	storefront := strings.ToLower(a.state().Storefront)
	if storefront == "" {
		storefront = "us"
	}
	payload, err := json.Marshal(map[string]string{"url": "https://music.apple.com/" + storefront + "/song/" + url.PathEscape(songID)})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(cfg.URL, "/")+"/v1/download", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("applemusic: download helper unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("applemusic: download helper HTTP %d", resp.StatusCode)
	}
	var out struct {
		File    string `json:"file"`
		Quality struct {
			SampleRate int  `json:"sampleRate"`
			Channels   int  `json:"channels"`
			Bits       int  `json:"bits"`
			Lossless   bool `json:"lossless"`
		} `json:"quality"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.File == "" {
		return nil, errors.New("applemusic: download helper returned no file")
	}
	base, _ := url.Parse(strings.TrimRight(cfg.URL, "/") + "/")
	file, parseErr := url.Parse(out.File)
	if parseErr != nil || base == nil {
		return nil, errors.New("applemusic: invalid helper file URL")
	}
	file = base.ResolveReference(file)
	if file.Scheme != base.Scheme || file.Host != base.Host || file.User != nil {
		return nil, errors.New("applemusic: helper file must use the configured helper origin")
	}
	fileURL := file.String()
	freq, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
	if err != nil {
		return nil, err
	}
	if cfg.Token != "" {
		freq.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	fresp, err := hc.Do(freq)
	if err != nil {
		return nil, errors.New("applemusic: helper file unreachable")
	}
	defer fresp.Body.Close()
	if fresp.StatusCode != 200 {
		return nil, fmt.Errorf("applemusic: helper file HTTP %d", fresp.StatusCode)
	}
	if dstDir != "" {
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.CreateTemp(dstDir, ".part-am-*.flac")
	if err != nil {
		return nil, err
	}
	tempName := f.Name()
	defer os.Remove(tempName)
	finalName := tempName
	if dstDir != "" {
		finalName = filepath.Join(dstDir, name+".flac")
	}
	var size int64
	if size, err = io.Copy(f, io.LimitReader(fresp.Body, (1<<30)+1)); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && size > 1<<30 {
		err = errors.New("applemusic: helper file exceeds size limit")
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		chmodErr := os.Chmod(tempName, 0o644)
		// FAT/exFAT cache volumes may not implement Unix permission changes.
		if !errors.Is(chmodErr, syscall.EPERM) && !errors.Is(chmodErr, syscall.EOPNOTSUPP) && !errors.Is(chmodErr, syscall.ENOSYS) {
			err = chmodErr
		}
	}
	if err == nil && dstDir != "" {
		err = os.Rename(tempName, finalName)
	}
	if err != nil {
		return nil, err
	}
	// Download without a destination transfers ownership of its temporary file.
	if dstDir == "" {
		keep := tempName + ".flac"
		if err := os.Rename(tempName, keep); err != nil {
			return nil, err
		}
		finalName = keep
	}
	return &AppleMusicAudio{Path: finalName, SampleRate: out.Quality.SampleRate, Channels: out.Quality.Channels, Bits: out.Quality.Bits, Lossless: out.Quality.Lossless}, nil
}

// Ready reports whether there is anything to show: a link, a library login, or an active helper session.
func (a *AppleMusic) Ready() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.owner.Links) > 0 || a.owner.UserToken != "" || a.helperSignedIn
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
// ships in its script bundle, refreshed a day before it expires. If a download
// helper is configured and the web player cannot be scraped, it falls back to
// the helper's developer token.
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
	helperToken := a.helperDevToken
	cfg := AppleMusicHelperConfig{a.HelperURL, a.HelperLoginURL, a.HelperToken}
	a.mu.Unlock()

	scrapeCtx := ctx
	var cancelScrape context.CancelFunc
	if helperToken != "" || cfg.URL != "" {
		limit := 2 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			if rem := time.Until(deadline) / 2; rem > 0 && rem < limit {
				limit = rem
			}
		}
		scrapeCtx, cancelScrape = context.WithTimeout(ctx, limit)
	}
	token, err := a.scrapeDeveloperToken(scrapeCtx)
	if cancelScrape != nil {
		cancelScrape()
	}
	if err == nil {
		until := time.Now().Add(12 * time.Hour)
		if exp := jwtExpiry(token); !exp.IsZero() {
			until = exp.Add(-24 * time.Hour)
		}
		a.mu.Lock()
		a.web, a.webAt = token, until
		a.mu.Unlock()
		return token, nil
	}
	if ctx.Err() == nil && cfg.URL != "" && helperToken == "" {
		syncCtx, cancelSync := context.WithTimeout(ctx, 2*time.Second)
		a.syncHelperCredentials(syncCtx, cfg)
		cancelSync()
		a.mu.Lock()
		helperToken = a.helperDevToken
		a.mu.Unlock()
	}
	if helperToken != "" {
		exp := jwtExpiry(helperToken)
		if exp.IsZero() || time.Now().Before(exp) {
			until := time.Now().Add(15 * time.Minute)
			if !exp.IsZero() && exp.Before(until) {
				until = exp
			}
			a.mu.Lock()
			a.web, a.webAt = helperToken, until
			a.mu.Unlock()
			return helperToken, nil
		}
	}
	return "", err
}

func (a *AppleMusic) scrapeDeveloperToken(ctx context.Context) (string, error) {
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
			if a.web != "" && a.web == a.helperDevToken {
				a.helperDevToken = ""
			}
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
	Meta struct{ Total int }
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
	if r.Type == "library-songs" {
		t.CatalogID = at.PlayParams.CatalogID
		if t.CatalogID == "" && len(r.Relationships.Catalog.Data) > 0 {
			t.CatalogID = r.Relationships.Catalog.Data[0].ID
		}
	}
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

// Playlist IDs: "pl:<storefront>:<id>", "al:<storefront>:<id>", "lib:<id>", "libal:<id>", "liked".
func linkID(l AppleMusicLink) string {
	return map[string]string{"playlist": "pl", "album": "al"}[l.Kind] + ":" + l.Storefront + ":" + l.ID
}

func (a *AppleMusic) linkResource(ctx context.Context, raw string) (AppleMusicLink, amResource, int, error) {
	links := ParseAppleMusicLinks(raw)
	if len(links) != 1 {
		return AppleMusicLink{}, amResource{}, 0, errors.New("not an Apple Music playlist or album link")
	}
	l := links[0]
	if l.Storefront == "" {
		l.Storefront = a.state().Storefront
	}
	var p amPage
	if err := a.get(ctx, "/v1/catalog/"+l.Storefront+"/"+l.Kind+"s/"+url.PathEscape(l.ID), false, &p); err != nil {
		return AppleMusicLink{}, amResource{}, 0, err
	}
	if len(p.Data) == 0 {
		return AppleMusicLink{}, amResource{}, 0, StatusError(404)
	}
	r := p.Data[0]
	count := r.Attributes.TrackCount
	if count == 0 {
		count = len(r.Relationships.Tracks.Data)
	}
	return l, r, count, nil
}

func (a *AppleMusic) linkPlaylist(ctx context.Context, raw string) (Playlist, error) {
	l, r, count, err := a.linkResource(ctx, raw)
	if err != nil {
		return Playlist{}, err
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

func (a *AppleMusic) librarySongsCount(ctx context.Context) (int, error) {
	var p amPage
	if err := a.get(ctx, "/v1/me/library/songs?limit=1&include=catalog", true, &p); err != nil {
		return 0, err
	}
	if len(p.Data) == 0 && p.Meta.Total == 0 {
		return 0, nil
	}
	if p.Meta.Total > 0 {
		return p.Meta.Total, nil
	}
	return len(p.Data), nil
}

func (a *AppleMusic) libraryAlbums(ctx context.Context, max int) ([]AppleMusicAlbum, error) {
	rs, err := a.pages(ctx, "/v1/me/library/albums?limit=100", true, max)
	var out []AppleMusicAlbum
	for _, r := range rs {
		out = append(out, AppleMusicAlbum{
			ID:         r.ID,
			ListID:     amLibraryAlbumPrefix + r.ID,
			Name:       r.Attributes.Name,
			ArtistName: r.Attributes.ArtistName,
			TrackCount: r.Attributes.TrackCount,
		})
	}
	return out, err
}

// FavoriteAlbums lists saved library albums and any pasted album links.
func (a *AppleMusic) FavoriteAlbums(ctx context.Context) ([]AppleMusicAlbum, error) {
	o := a.state()
	var out []AppleMusicAlbum
	var firstErr error
	for _, raw := range o.Links {
		links := ParseAppleMusicLinks(raw)
		if len(links) != 1 || links[0].Kind != "album" {
			continue
		}
		l, r, count, err := a.linkResource(ctx, raw)
		if err != nil {
			var st StatusError
			if errors.As(err, &st) && (st == 401 || st == 403) {
				return nil, err
			}
			firstErr = keepFirst(firstErr, err)
			continue
		}
		out = append(out, AppleMusicAlbum{
			ID:         l.ID,
			ListID:     linkID(l),
			Name:       r.Attributes.Name,
			ArtistName: r.Attributes.ArtistName,
			TrackCount: count,
		})
	}
	if o.UserToken != "" {
		albums, err := a.libraryAlbums(ctx, 1000)
		if err != nil {
			var st StatusError
			if errors.As(err, &st) && st == 404 {
				return out, nil
			}
			if len(albums) == 0 && len(out) == 0 {
				return nil, err
			}
			out = append(out, albums...)
			return out, err
		}
		out = append(out, albums...)
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
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
			var st StatusError
			if errors.As(err, &st) && (st == 401 || st == 403) {
				return nil, err
			}
			firstErr = keepFirst(firstErr, err)
			continue
		}
		out = append(out, p)
	}
	if o.UserToken != "" {
		// Warm the developer token once so concurrent library endpoints share it.
		if _, err := a.developerToken(ctx); err != nil {
			return nil, err
		}
		var lib []Playlist
		var libErr error
		var songsCount int
		var songsErr error
		var albums []AppleMusicAlbum
		var albumsErr error
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			lib, libErr = a.libraryPlaylists(ctx, 1000)
		}()
		go func() {
			defer wg.Done()
			songsCount, songsErr = a.librarySongsCount(ctx)
		}()
		go func() {
			defer wg.Done()
			albums, albumsErr = a.libraryAlbums(ctx, 1000)
		}()
		wg.Wait()
		if libErr != nil {
			return nil, libErr
		}
		if songsErr == nil && songsCount > 0 {
			out = append(out, Playlist{ID: AppleMusicLikedID, Title: "资料库歌曲 (Library Songs)", Count: songsCount})
		}
		out = append(out, lib...)
		if albumsErr == nil {
			for _, al := range albums {
				out = append(out, Playlist{ID: al.ListID, Title: "【专辑】" + al.Name, Count: al.TrackCount})
			}
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
	if id == AppleMusicLikedID {
		if a.state().UserToken == "" {
			return nil, nil
		}
		path, user = "/v1/me/library/songs?limit=100&include=catalog", true
	} else {
		switch kind, rest, _ := strings.Cut(id, ":"); kind {
		case "pl", "al":
			sf, cid, ok := strings.Cut(rest, ":")
			if !ok || !amStore.MatchString(sf) {
				return nil, errors.New("applemusic: invalid playlist id")
			}
			path = "/v1/catalog/" + sf + "/" + map[string]string{"pl": "playlists", "al": "albums"}[kind] + "/" + url.PathEscape(cid) + "/tracks?limit=100"
		case "lib":
			path, user = "/v1/me/library/playlists/"+url.PathEscape(rest)+"/tracks?limit=100&include=catalog", true
		case "libal":
			if rest == "" {
				return nil, errors.New("applemusic: invalid playlist id")
			}
			path, user = "/v1/me/library/albums/"+url.PathEscape(rest)+"/tracks?limit=100&include=catalog", true
		default:
			return nil, errors.New("applemusic: invalid playlist id")
		}
	}
	rs, err := a.pages(ctx, path, user, 20000)
	if err != nil {
		return nil, err
	}
	var out []Track
	for _, r := range rs {
		if t, ok := r.track(); ok {
			out = append(out, t)
		}
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

// SearchPlaylists looks up catalog playlists in the owner's storefront.
func (a *AppleMusic) SearchPlaylists(ctx context.Context, query string) ([]Playlist, error) {
	var r struct {
		Results struct{ Playlists amPage }
	}
	sf := a.state().Storefront
	q := url.Values{"term": {query}, "types": {"playlists"}, "limit": {"20"}}
	if err := a.get(ctx, "/v1/catalog/"+sf+"/search?"+q.Encode(), false, &r); err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(r.Results.Playlists.Data))
	for _, p := range r.Results.Playlists.Data {
		if p.ID == "" || p.Attributes.Name == "" {
			continue
		}
		out = append(out, Playlist{ID: "pl:" + sf + ":" + p.ID, Title: p.Attributes.Name, Count: p.Attributes.TrackCount})
	}
	return out, nil
}
