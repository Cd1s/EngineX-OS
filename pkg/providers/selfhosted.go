package providers

import (
	"bytes"
	"cmp"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultMaxCache = 5000

type NASConfig struct {
	Protocol string `json:"protocol"` // "webdav", "subsonic", "plex", "smb"
	URL      string `json:"url"`      // e.g. "https://nas.example.com/music" or "http://192.168.1.10:4533"
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"` // Plex X-Plex-Token or Bearer token
	Enabled  bool   `json:"enabled"`
}

type SelfHostedProvider struct {
	cfg          NASConfig
	client       *http.Client
	mu           sync.RWMutex
	webdavCache  map[string]Track  // href/ID -> Track
	webdavKeys   []string          // FIFO eviction queue
	locatorMap   map[string]string // "<kind>\x00<publicID>" -> private same-origin path+query
	locatorOrder []string          // FIFO eviction queue
	maxCache     int               // max entries for caches (<=0 uses defaultMaxCache)
}

func NewSelfHostedProvider(cfg NASConfig, c *http.Client) *SelfHostedProvider {
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &SelfHostedProvider{
		cfg:         cfg,
		client:      c,
		webdavCache: make(map[string]Track),
		locatorMap:  make(map[string]string),
		maxCache:    defaultMaxCache,
	}
}

func (p *SelfHostedProvider) Config() NASConfig {
	return p.cfg
}

func (p *SelfHostedProvider) httpClient() *http.Client {
	if p != nil && p.client != nil {
		return p.client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	def := map[string]string{"http": "80", "https": "443"}[strings.ToLower(a.Scheme)]
	return cmp.Or(a.Port(), def) == cmp.Or(b.Port(), def)
}

// authHTTPClient copies p.httpClient() with one redirect policy for authenticated
// NAS requests: always drop Referer and refuse cross-origin (or disallowed) redirects.
func (p *SelfHostedProvider) authHTTPClient(baseU *url.URL, check ...func(*http.Request) bool) *http.Client {
	client := *p.httpClient()
	origCheck := client.CheckRedirect
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		r.Header.Del("Referer")
		if baseU == nil || r.URL.User != nil || !sameOrigin(r.URL, baseU) || (len(check) > 0 && !check[0](r)) {
			r.Header.Del("Authorization")
			r.Header.Del("X-Plex-Token")
			r.Header.Del("Cookie")
			return errors.New("redirect refused")
		}
		if origCheck != nil {
			return origCheck(r, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

func (p *SelfHostedProvider) lookupLocator(kind, id string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	prefix := kind + "\x00"
	if v := p.locatorMap[prefix+id]; v != "" {
		return v
	}
	basePath, hashed := splitSelfHostedHash(id)
	if hashed || !strings.HasPrefix(basePath, "/") {
		return ""
	}
	target, found := strings.TrimRight(basePath, "/"), ""
	for k, val := range p.locatorMap {
		if rest, ok := strings.CutPrefix(k, prefix); ok {
			if kBase, _ := splitSelfHostedHash(rest); strings.TrimRight(kBase, "/") == target {
				if found != "" && found != val {
					return ""
				}
				found = val
			}
		}
	}
	return found
}

func (p *SelfHostedProvider) withLocator(kind, id, fallback, missingErr string, refresh func() string, do func(string) (bool, error)) error {
	ref, refreshed := p.lookupLocator(kind, id), false
	if ref == "" {
		if _, hashed := splitSelfHostedHash(id); hashed || fallback == "" {
			ref, refreshed = refresh(), true
		} else {
			ref = fallback
		}
	}
	if ref == "" {
		return errors.New(missingErr)
	}
	retry, err := do(ref)
	if retry && !refreshed {
		p.mu.Lock()
		delete(p.locatorMap, kind+"\x00"+id)
		p.mu.Unlock()
		if next := refresh(); next != "" && next != ref {
			_, err = do(next)
		}
	}
	return err
}

func (p *SelfHostedProvider) getWithLocator(ctx context.Context, proto, kind, id, fallback, missingErr string, baseU *url.URL,
	buildURL func(string) (string, error), setAuth func(*http.Request), refresh func() string,
) (resp *http.Response, reqURL *url.URL, usedRef string, err error) {
	err = p.withLocator(kind, id, fallback, missingErr, refresh, func(ref string) (bool, error) {
		if resp != nil {
			resp.Body.Close()
			resp = nil
		}
		rawURL, rerr := buildURL(ref)
		if rerr != nil {
			return false, rerr
		}
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if rerr != nil {
			return false, fmt.Errorf("%s: invalid download URL", proto)
		}
		setAuth(req)
		r, rerr := p.authHTTPClient(baseU).Do(req)
		if rerr != nil {
			return false, safeSelfHostedError(proto, "download", req.URL, rerr)
		}
		resp, reqURL, usedRef = r, req.URL, ref
		return r.StatusCode == 401 || r.StatusCode == 403 || r.StatusCode == 404, nil
	})
	if err != nil && resp != nil {
		resp.Body.Close()
		resp = nil
	}
	return
}

func isRetryableLocatorError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), "HTTP 404"))
}

func (p *SelfHostedProvider) Playlists(ctx context.Context) ([]Playlist, error) {
	switch strings.ToLower(p.cfg.Protocol) {
	case "subsonic":
		return p.subsonicPlaylists(ctx)
	case "webdav":
		return p.webdavPlaylists(ctx)
	case "plex":
		return p.plexPlaylists(ctx)
	case "smb":
		return p.smbPlaylists(ctx)
	default:
		return nil, fmt.Errorf("selfhosted: unsupported protocol %q", p.cfg.Protocol)
	}
}

func (p *SelfHostedProvider) PlaylistTracks(ctx context.Context, playlistID string) ([]Track, error) {
	switch strings.ToLower(p.cfg.Protocol) {
	case "subsonic":
		return p.subsonicPlaylistTracks(ctx, playlistID)
	case "webdav":
		return p.webdavPlaylistTracks(ctx, playlistID)
	case "plex":
		return p.plexPlaylistTracks(ctx, playlistID)
	case "smb":
		return p.smbPlaylistTracks(ctx, playlistID)
	default:
		return nil, fmt.Errorf("selfhosted: unsupported protocol %q", p.cfg.Protocol)
	}
}

// Tracks implements the Provider interface.
func (p *SelfHostedProvider) Tracks(ctx context.Context, playlistID string) ([]Track, error) {
	return p.PlaylistTracks(ctx, playlistID)
}

func (p *SelfHostedProvider) Search(ctx context.Context, query string) ([]Track, error) {
	switch strings.ToLower(p.cfg.Protocol) {
	case "subsonic":
		return p.subsonicSearch(ctx, query)
	case "webdav":
		return p.webdavSearch(ctx, query)
	case "plex":
		return p.plexSearch(ctx, query)
	case "smb":
		return p.smbSearch(ctx, query)
	default:
		return nil, fmt.Errorf("selfhosted: unsupported protocol %q", p.cfg.Protocol)
	}
}

func (p *SelfHostedProvider) DownloadTrack(ctx context.Context, trackID, destDir string) (string, error) {
	switch strings.ToLower(p.cfg.Protocol) {
	case "subsonic":
		return p.subsonicDownloadTrack(ctx, trackID, destDir)
	case "webdav":
		return p.webdavDownloadTrack(ctx, trackID, destDir)
	case "plex":
		return p.plexDownloadTrack(ctx, trackID, destDir)
	case "smb":
		return p.smbDownloadTrack(ctx, trackID, destDir)
	default:
		return "", fmt.Errorf("selfhosted: unsupported protocol %q", p.cfg.Protocol)
	}
}

// ----------------------------------------------------------------------------
// Subsonic / Navidrome (OpenSubsonic REST API)
// ----------------------------------------------------------------------------

func (p *SelfHostedProvider) subsonicAuthParams() url.Values {
	q := make(url.Values)
	q.Set("v", "1.16.1")
	q.Set("c", "EngineX")
	q.Set("f", "json")
	if p.cfg.Username != "" {
		q.Set("u", p.cfg.Username)
	}
	if p.cfg.Password != "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		salt := hex.EncodeToString(b[:])
		hash := md5.Sum([]byte(p.cfg.Password + salt))
		q.Set("s", salt)
		q.Set("t", hex.EncodeToString(hash[:]))
	} else if p.cfg.Token != "" {
		q.Set("p", p.cfg.Token)
		if p.cfg.Username == "" {
			q.Set("u", "token")
		}
	}
	return q
}

type subsonicResponse struct {
	SubsonicResponse struct {
		Status    string         `json:"status"`
		Version   string         `json:"version"`
		Error     *subsonicError `json:"error,omitempty"`
		Playlists *struct {
			Playlist []subsonicPlaylist `json:"playlist"`
		} `json:"playlists,omitempty"`
		Playlist *struct {
			ID        string         `json:"id"`
			Name      string         `json:"name"`
			SongCount int            `json:"songCount"`
			Entry     []subsonicSong `json:"entry"`
		} `json:"playlist,omitempty"`
		SearchResult3 *struct {
			Song []subsonicSong `json:"song"`
		} `json:"searchResult3,omitempty"`
	} `json:"subsonic-response"`
}

type subsonicError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type subsonicPlaylist struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	SongCount int    `json:"songCount"`
}

type subsonicSong struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Artist      string  `json:"artist"`
	Album       string  `json:"album"`
	Duration    float64 `json:"duration"` // in seconds
	BPM         float64 `json:"bpm"`
	CoverArt    string  `json:"coverArt"`
	ContentType string  `json:"contentType"`
	Suffix      string  `json:"suffix"`
	Path        string  `json:"path"`
}

func validateBaseURL(proto, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s: empty URL", proto)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid URL", proto)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s: invalid URL scheme %q", proto, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%s: missing URL host", proto)
	}
	return u, nil
}

type selfHostedError struct {
	msg   string
	cause error // only standard context sentinels; never raw transport/redirect errors
}

func (e *selfHostedError) Error() string { return e.msg }
func (e *selfHostedError) Unwrap() error { return e.cause }

// cleanBaseURL returns scheme://host/path with userinfo, query, fragment, and
// trailing slash stripped.
func cleanBaseURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.TrimRight(safeURLRoute(u), "/")
}

func isSubsonicCoverPath(p string) bool {
	return strings.HasSuffix(p, "/rest/getCoverArt.view") || strings.HasSuffix(p, "/rest/getCoverArt")
}

func hasRawPathTraversal(u *url.URL) bool {
	if u == nil || strings.Contains(u.Path, "\\") {
		return true
	}
	if u.Path != "" {
		clean := path.Clean(u.Path)
		if strings.HasSuffix(u.Path, "/") && clean != "/" {
			clean += "/"
		}
		if clean != u.Path || clean == ".." || strings.HasPrefix(clean, "../") {
			return true
		}
	}
	low := strings.ToLower(u.RawPath)
	return strings.Contains(low, "%2f") || strings.Contains(low, "%5c") || strings.Contains(low, "%2e")
}

// isPlexCoverPath reports whether rel (relative to the Plex base path) is one
// of Plex's artwork endpoints rather than arbitrary private metadata/prefs/media.
func isPlexCoverPath(rel string) bool {
	if !strings.HasPrefix(rel, "/") || strings.HasSuffix(rel, "/") || path.Clean(rel) != rel ||
		strings.ContainsAny(rel, "\\?#") || strings.IndexFunc(rel, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false
	}
	p := strings.Split(rel[1:], "/")
	n := len(p)
	return ((n == 4 || n == 5) && p[0] == "library" && ((p[1] == "metadata" && (p[3] == "thumb" || p[3] == "art" || (p[3] == "composite" && n == 5))) || (p[1] == "sections" && (p[3] == "thumb" || (p[3] == "composite" && n == 5))))) ||
		(n == 4 && p[0] == "playlists" && p[2] == "composite")
}

// IsValidCoverImage reports whether data is a supported raster image body.
func IsValidCoverImage(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	switch http.DetectContentType(data) {
	case "image/jpeg", "image/png", "image/gif":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		return err == nil && cfg.Width > 0 && cfg.Height > 0
	case "image/webp":
		return string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	case "image/bmp":
		return len(data) >= 26 && data[0] == 'B' && data[1] == 'M'
	}
	return false
}

const selfHostedHashSep = ";id="

func splitSelfHostedHash(id string) (string, bool) {
	if i := strings.LastIndex(id, selfHostedHashSep); i >= 0 {
		if h := id[i+len(selfHostedHashSep):]; len(h) == 16 && h == strings.ToLower(h) {
			if _, err := hex.DecodeString(h); err == nil {
				return id[:i], true
			}
		}
	}
	return id, false
}

func isEphemeralAuthQueryKey(k string) bool {
	lk := strings.ToLower(k)
	switch lk {
	case "sig", "sign", "signature", "hmac", "token", "tok", "auth", "apikey", "api_key", "jwt",
		"secret", "pass", "password", "pwd", "credential", "nonce", "ts", "timestamp",
		"exp", "expires", "expire", "expiration", "ttl", "policy", "key-pair-id",
		"awsaccesskeyid", "googleaccessid":
		return true
	}
	return strings.HasPrefix(lk, "x-amz-") || strings.HasPrefix(lk, "x-goog-") ||
		strings.HasSuffix(lk, "token") || strings.HasSuffix(lk, "sig") ||
		strings.HasSuffix(lk, "signature") || strings.HasSuffix(lk, "_key") || strings.HasSuffix(lk, "-key")
}

func filterQuery(rawQuery string, drop func(string) bool) string {
	if rawQuery == "" {
		return ""
	}
	q, _ := url.ParseQuery(rawQuery)
	for k := range q {
		if drop(k) {
			q.Del(k)
		}
	}
	return q.Encode()
}

func canonicalSelfHostedQuery(q string) string { return filterQuery(q, isEphemeralAuthQueryKey) }

func nasCacheResource(u *url.URL) string {
	if cq := canonicalSelfHostedQuery(u.RawQuery); cq != "" {
		return u.Path + "?" + cq
	}
	return u.Path
}

// SanitizeSelfHostedID removes URL authority, userinfo, query, and fragment
// credentials from URL-shaped Plex/WebDAV/NAS identifiers, hashing remaining
// non-token query selectors so distinct resources stay unique.
func SanitizeSelfHostedID(provider, rawID string) string {
	s, proto := strings.TrimSpace(rawID), strings.ToLower(strings.TrimSpace(provider))
	if s == "" {
		return ""
	}
	if proto != "plex" && proto != "webdav" && proto != "nas" {
		return rawID
	}
	if !strings.Contains(s, "://") && !strings.HasPrefix(s, "//") &&
		!((strings.HasPrefix(s, "/") || proto != "nas") && strings.ContainsAny(s, "?#")) {
		return s
	}
	cleanPath, _, _ := strings.Cut(s, "#")
	cleanPath, rawQuery, _ := strings.Cut(cleanPath, "?")
	if u, err := url.Parse(s); err == nil {
		cleanPath, rawQuery = u.EscapedPath(), u.RawQuery
		if cleanPath != "" && (u.Scheme != "" || u.Host != "") && !strings.HasPrefix(cleanPath, "/") {
			cleanPath = "/" + cleanPath
		}
	} else if _, rest, ok := strings.Cut(cleanPath, "//"); ok && (strings.Contains(cleanPath, "://") || strings.HasPrefix(cleanPath, "//")) {
		cleanPath = ""
		if _, after, ok := strings.Cut(rest, "/"); ok {
			cleanPath = "/" + after
		}
	}
	if cleanPath == "" {
		return ""
	}
	if cq := canonicalSelfHostedQuery(rawQuery); cq != "" {
		sum := sha256.Sum256([]byte(cleanPath + "?" + cq))
		return cleanPath + selfHostedHashSep + hex.EncodeToString(sum[:8])
	}
	return cleanPath
}

// SanitizeSelfHostedTrack scrubs embedded credentials from cover/thumbnail
// URLs and normalizes URL-shaped Plex/WebDAV track identifiers in place.
func SanitizeSelfHostedTrack(t *Track) bool {
	if t == nil {
		return false
	}
	changed := false
	scrub := func(dst *string, clean string) {
		if clean != *dst {
			*dst, changed = clean, true
		}
	}
	scrub(&t.CoverURL, SanitizeCoverURL(t.CoverURL))
	scrub(&t.ThumbURL, SanitizeCoverURL(t.ThumbURL))
	if proto := strings.ToLower(strings.TrimSpace(t.Provider)); proto == "plex" || proto == "webdav" {
		scrub(&t.ID, SanitizeSelfHostedID(proto, t.ID))
		if proto == "webdav" && (strings.Contains(t.Album, "://") || strings.HasPrefix(t.Album, "//") || strings.ContainsAny(t.Album, "?#")) {
			scrub(&t.Album, SanitizeSelfHostedID("webdav", t.Album))
		}
	}
	return changed
}

// SanitizeCoverURL removes embedded credentials (userinfo, Plex X-Plex-Token,
// and Subsonic u/p/t/s auth query parameters) from a cover URL while keeping
// the credential-free cover location intact.
func SanitizeCoverURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		if u.User != nil {
			u.User = nil
			return u.String()
		}
		return raw
	}
	changed := u.User != nil
	u.User = nil
	if u.RawQuery != "" {
		q, qerr := url.ParseQuery(u.RawQuery)
		changed = changed || qerr != nil
		hasU, hasSecret := false, false
		for k := range q {
			switch strings.ToLower(k) {
			case "x-plex-token":
				q.Del(k)
				changed = true
			case "u":
				hasU = true
			case "p", "t", "s":
				hasSecret = true
			}
		}
		if (hasU && hasSecret) || (isSubsonicCoverPath(u.Path) && (hasU || hasSecret)) {
			for k := range q {
				switch strings.ToLower(k) {
				case "u", "p", "t", "s", "v", "c", "f":
					q.Del(k)
					changed = true
				}
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	if !changed {
		return raw
	}
	return u.String()
}

// NASConfigMatchesCoverURL reports whether rawURL is a supported artwork URL
// belonging to cfg's configured Subsonic or Plex server.
func NASConfigMatchesCoverURL(cfg NASConfig, rawURL string) bool {
	proto := strings.ToLower(strings.TrimSpace(cfg.Protocol))
	baseU, err := validateBaseURL(proto, cfg.URL)
	if !cfg.Enabled || (proto != "subsonic" && proto != "plex") || err != nil {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Fragment != "" || !sameOrigin(u, baseU) || hasRawPathTraversal(u) {
		return false
	}
	basePath := strings.TrimRight(baseU.Path, "/")
	rel, ok := strings.CutPrefix(u.Path, basePath)
	if basePath != "" && (!ok || (rel != "" && !strings.HasPrefix(rel, "/"))) {
		return false
	}
	if proto == "subsonic" {
		if rel != "/rest/getCoverArt.view" && rel != "/rest/getCoverArt" {
			return false
		}
		q, qerr := url.ParseQuery(u.RawQuery)
		if qerr != nil || strings.TrimSpace(q.Get("id")) == "" {
			return false
		}
		for k := range q {
			if k != "id" && k != "size" {
				return false
			}
		}
		return true
	}
	return u.RawQuery == "" && !u.ForceQuery && isPlexCoverPath(rel)
}

// MatchesCoverURL reports whether rawURL belongs to this provider's configured
// Subsonic or Plex server and should be fetched with its configured credentials.
func (p *SelfHostedProvider) MatchesCoverURL(rawURL string) bool {
	return p != nil && NASConfigMatchesCoverURL(p.cfg, rawURL)
}

// SameOrigin reports whether rawURL shares the origin of this NAS base URL.
func (p *SelfHostedProvider) SameOrigin(rawURL string) bool {
	if p == nil {
		return false
	}
	baseU, err := validateBaseURL(strings.ToLower(strings.TrimSpace(p.cfg.Protocol)), p.cfg.URL)
	u, uerr := url.Parse(strings.TrimSpace(rawURL))
	return err == nil && uerr == nil && sameOrigin(u, baseU)
}

func (p *SelfHostedProvider) sanitizeTrackCover(raw string) string {
	if c := SanitizeCoverURL(raw); c != "" && (!p.SameOrigin(c) || p.MatchesCoverURL(c)) {
		return c
	}
	return ""
}

// FetchCover fetches cover image bytes for a credential-free NAS cover URL,
// attaching credentials from p.cfg only inside the outgoing HTTP request.
func (p *SelfHostedProvider) FetchCover(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	u, uerr := url.Parse(strings.TrimSpace(rawURL))
	if !p.MatchesCoverURL(rawURL) || uerr != nil {
		return nil, errors.New("cover: invalid address")
	}
	proto := strings.ToLower(strings.TrimSpace(p.cfg.Protocol))
	baseU, err := validateBaseURL(proto, p.cfg.URL)
	if err != nil {
		return nil, err
	}
	reqU := *u
	reqU.User, reqU.RawQuery, reqU.Fragment = nil, "", ""
	if proto == "subsonic" {
		inQ, _ := url.ParseQuery(u.RawQuery)
		q := p.subsonicAuthParams()
		q.Set("id", inQ.Get("id"))
		if sz := strings.TrimSpace(inQ.Get("size")); sz != "" {
			q.Set("size", sz)
		}
		reqU.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqU.String(), nil)
	if err != nil {
		return nil, errors.New("cover: invalid address")
	}
	if tok := p.plexToken(); proto == "plex" && tok != "" {
		req.Header.Set("X-Plex-Token", tok)
	}

	resp, err := p.authHTTPClient(baseU, func(r *http.Request) bool {
		target := r.URL.String()
		if proto == "subsonic" {
			target = SanitizeCoverURL(target)
		}
		return p.MatchesCoverURL(target)
	}).Do(req)
	if err != nil {
		return nil, safeSelfHostedError(proto, "cover", req.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cover: HTTP %d", resp.StatusCode)
	}
	if ct := strings.TrimSpace(resp.Header.Get("Content-Type")); ct != "" {
		if mt, _, _ := mime.ParseMediaType(ct); (!strings.HasPrefix(mt, "image/") || mt == "image/svg+xml") && mt != "application/octet-stream" && mt != "binary/octet-stream" {
			return nil, errors.New("cover: not an image")
		}
	}
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, safeSelfHostedError(proto, "cover-read", req.URL, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("cover: too large")
	}
	if !IsValidCoverImage(data) {
		return nil, errors.New("cover: not an image")
	}
	return data, nil
}

// safeURLRoute returns scheme://host/path with userinfo, query, and fragment stripped.
func safeURLRoute(u *url.URL) string {
	if u == nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	return (&url.URL{Scheme: scheme, Host: u.Host, Path: u.Path}).String()
}

func errAs[T any](err error) bool { var target T; return errors.As(err, &target) }

// classifySelfHostedError maps a transport, redirect, or I/O error to a fixed
// diagnostic category without copying any bytes from the underlying error text.
func classifySelfHostedError(err error) (string, error) {
	var netErr net.Error
	var ue *url.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "request canceled", context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out", context.DeadlineExceeded
	case errors.As(err, &netErr) && netErr.Timeout():
		return "request timed out", nil
	case errAs[*net.DNSError](err):
		return "host lookup failed", nil
	case errAs[*tls.CertificateVerificationError](err) || errAs[tls.RecordHeaderError](err) ||
		errAs[x509.UnknownAuthorityError](err) || errAs[x509.HostnameError](err) || errAs[x509.CertificateInvalidError](err):
		return "TLS handshake failed", nil
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused", nil
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE):
		return "connection reset", nil
	case errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH):
		return "host unreachable", nil
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed", nil
	case errors.As(err, &ue) && ue.Err != nil:
		inner := ue.Err.Error()
		switch {
		case strings.Contains(inner, "Location"), strings.Contains(inner, "redirect"):
			return "redirect failed", nil
		case strings.Contains(inner, "not allowed"):
			return "target address not allowed", nil
		case strings.Contains(inner, "no addresses"):
			return "host lookup failed", nil
		case strings.Contains(inner, "invalid url"):
			return "invalid redirect URL", nil
		}
	}
	if errAs[*net.OpError](err) {
		return "connection failed", nil
	}
	if errAs[*os.PathError](err) || errAs[*os.LinkError](err) {
		return "storage write failed", nil
	}
	return "request failed", nil
}

// safeSelfHostedError builds an outward diagnostic from a fixed protocol,
// operation, sanitized initial request route, and fixed error category so
// transport/redirect inner errors never leak Subsonic/Plex/WebDAV credentials.
func safeSelfHostedError(proto, op string, reqURL *url.URL, err error) error {
	category, sentinel := classifySelfHostedError(err)
	if route := safeURLRoute(reqURL); route != "" {
		return &selfHostedError{msg: fmt.Sprintf("%s: %s %s: %s", proto, op, route, category), cause: sentinel}
	}
	return &selfHostedError{msg: fmt.Sprintf("%s: %s: %s", proto, op, category), cause: sentinel}
}

func (p *SelfHostedProvider) subsonicGet(ctx context.Context, action string, extra url.Values, out *subsonicResponse) error {
	baseU, err := validateBaseURL("subsonic", p.cfg.URL)
	if err != nil {
		return err
	}
	params := p.subsonicAuthParams()
	for k, vals := range extra {
		for _, v := range vals {
			params.Add(k, v)
		}
	}
	u := cleanBaseURL(p.cfg.URL) + "/rest/" + action + ".view?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return errors.New("subsonic: invalid request URL")
	}
	resp, err := p.authHTTPClient(baseU).Do(req)
	if err != nil {
		return safeSelfHostedError("subsonic", "request", req.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("subsonic: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return safeSelfHostedError("subsonic", "read", req.URL, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("subsonic: invalid JSON response")
	}
	if out.SubsonicResponse.Status == "failed" {
		if out.SubsonicResponse.Error != nil {
			return fmt.Errorf("subsonic error (code %d)", out.SubsonicResponse.Error.Code)
		}
		return errors.New("subsonic error: unknown failure")
	}
	return nil
}

func (p *SelfHostedProvider) subsonicEntryToTrack(e subsonicSong) Track {
	var coverURL string
	if e.CoverArt != "" {
		if strings.HasPrefix(e.CoverArt, "http://") || strings.HasPrefix(e.CoverArt, "https://") {
			coverURL = p.sanitizeTrackCover(e.CoverArt)
		} else if base := cleanBaseURL(p.cfg.URL); base != "" {
			q := url.Values{}
			q.Set("id", e.CoverArt)
			coverURL = base + "/rest/getCoverArt.view?" + q.Encode()
		}
	}
	return Track{
		ID:         e.ID,
		Title:      e.Title,
		Artist:     e.Artist,
		Album:      e.Album,
		DurationMS: int64(e.Duration * 1000),
		BPM:        e.BPM,
		CoverURL:   coverURL,
		Provider:   "subsonic",
	}
}

func (p *SelfHostedProvider) subsonicPlaylists(ctx context.Context) ([]Playlist, error) {
	var resp subsonicResponse
	if err := p.subsonicGet(ctx, "getPlaylists", nil, &resp); err != nil {
		return nil, err
	}
	var out []Playlist
	if resp.SubsonicResponse.Playlists != nil {
		for _, pl := range resp.SubsonicResponse.Playlists.Playlist {
			name := pl.Name
			if name == "" {
				name = pl.Title
			}
			out = append(out, Playlist{
				ID:    pl.ID,
				Title: name,
				Count: pl.SongCount,
			})
		}
	}
	return out, nil
}

func (p *SelfHostedProvider) subsonicPlaylistTracks(ctx context.Context, playlistID string) ([]Track, error) {
	extra := make(url.Values)
	extra.Set("id", playlistID)
	var resp subsonicResponse
	if err := p.subsonicGet(ctx, "getPlaylist", extra, &resp); err != nil {
		return nil, err
	}
	var out []Track
	if resp.SubsonicResponse.Playlist != nil {
		for _, s := range resp.SubsonicResponse.Playlist.Entry {
			out = append(out, p.subsonicEntryToTrack(s))
		}
	}
	return out, nil
}

func (p *SelfHostedProvider) subsonicSearch(ctx context.Context, query string) ([]Track, error) {
	extra := make(url.Values)
	extra.Set("query", query)
	extra.Set("songCount", "50")
	var resp subsonicResponse
	if err := p.subsonicGet(ctx, "search3", extra, &resp); err != nil {
		return nil, err
	}
	var out []Track
	if resp.SubsonicResponse.SearchResult3 != nil {
		for _, s := range resp.SubsonicResponse.SearchResult3.Song {
			out = append(out, p.subsonicEntryToTrack(s))
		}
	}
	return out, nil
}

func checkSubsonicMediaError(resp *http.Response) error {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if semi := strings.IndexByte(ct, ';'); semi >= 0 {
		ct = ct[:semi]
	}
	ct = strings.TrimSpace(ct)
	isJSON := ct == "application/json" || ct == "text/json" || strings.HasSuffix(ct, "+json")
	isXML := ct == "application/xml" || ct == "text/xml" || strings.HasSuffix(ct, "+xml")
	if !isJSON && !isXML {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if isJSON {
		var sr subsonicResponse
		if json.Unmarshal(body, &sr) == nil && sr.SubsonicResponse.Error != nil {
			return fmt.Errorf("subsonic error (code %d)", sr.SubsonicResponse.Error.Code)
		}
	}
	if isXML {
		var xr struct {
			Status string `xml:"status,attr"`
			Error  *struct {
				Code    int    `xml:"code,attr"`
				Message string `xml:"message,attr"`
			} `xml:"error"`
		}
		if xml.Unmarshal(body, &xr) == nil && xr.Error != nil {
			return fmt.Errorf("subsonic error (code %d)", xr.Error.Code)
		}
	}
	if isJSON {
		return errors.New("subsonic: unexpected JSON response instead of audio")
	}
	return errors.New("subsonic: unexpected XML response instead of audio")
}

func (p *SelfHostedProvider) subsonicDownloadTrack(ctx context.Context, trackID, destDir string) (string, error) {
	baseU, err := validateBaseURL("subsonic", p.cfg.URL)
	if err != nil {
		return "", err
	}
	baseStr := cleanBaseURL(p.cfg.URL)
	client := p.authHTTPClient(baseU)
	// First try /rest/download.view?id=<id>
	params := p.subsonicAuthParams()
	params.Set("id", trackID)
	downloadURL := baseStr + "/rest/download.view?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", errors.New("subsonic: invalid download URL")
	}
	resp, err := client.Do(req)
	var dlErr error
	if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
		dlErr = checkSubsonicMediaError(resp)
	}
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || dlErr != nil {
		if resp != nil {
			resp.Body.Close()
		}
		// Fallback to /rest/stream.view?id=<id>&format=raw
		streamParams := p.subsonicAuthParams()
		streamParams.Set("id", trackID)
		streamParams.Set("format", "raw")
		streamURL := baseStr + "/rest/stream.view?" + streamParams.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
		if err != nil {
			return "", errors.New("subsonic: invalid stream URL")
		}
		resp, err = client.Do(req)
		if err != nil {
			return "", safeSelfHostedError("subsonic", "stream", req.URL, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("subsonic: stream HTTP %d", resp.StatusCode)
		}
		if err := checkSubsonicMediaError(resp); err != nil {
			resp.Body.Close()
			return "", err
		}
	}
	defer resp.Body.Close()

	ext := parseExtFromHeader(resp.Header.Get("Content-Disposition"))
	if ext == "" {
		ext = extFromContentType(resp.Header.Get("Content-Type"))
	}
	if ext == "" {
		ext = ".mp3"
	}

	// The same flat hashed name as WebDAV, Plex and SMB: the cache budget evicts only those,
	// and two IDs or two servers never share a file.
	filename := nasCacheFilename(baseU, strings.TrimRight(baseU.Path, "/")+"\x00subsonic\x00"+trackID, ext)
	outPath, err := saveStreamToFile(destDir, filename, resp.Body)
	if err != nil {
		return "", safeSelfHostedError("subsonic", "download", req.URL, err)
	}
	return outPath, nil
}

// ----------------------------------------------------------------------------
// WebDAV
// ----------------------------------------------------------------------------

var webdavAudioExts = map[string]bool{
	".mp3":  true,
	".flac": true,
	".wav":  true,
	".aiff": true,
	".m4a":  true,
	".aac":  true,
	".ogg":  true,
}

type davMultistatus struct {
	XMLName   xml.Name      `xml:"multistatus"`
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string        `xml:"href"`
	Propstat []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Prop   davProp `xml:"prop"`
	Status string  `xml:"status"`
}

type davProp struct {
	DisplayName   string          `xml:"displayname"`
	ContentLength int64           `xml:"getcontentlength"`
	ResourceType  davResourceType `xml:"resourcetype"`
}

type davResourceType struct {
	Collection *struct{} `xml:"collection"`
}

func (p *SelfHostedProvider) setWebDAVAuth(req *http.Request) {
	if p.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	} else if p.cfg.Username != "" {
		req.SetBasicAuth(p.cfg.Username, p.cfg.Password)
	}
}

func (p *SelfHostedProvider) webdavCleanRef(rawHref string) (string, bool) {
	refU, err := resolveWebDAVURL(p.cfg.URL, rawHref)
	if err != nil {
		return "", false
	}
	priv := cmp.Or(refU.EscapedPath(), "/")
	if refU.RawQuery != "" {
		priv += "?" + refU.RawQuery
	}
	cleanID := SanitizeSelfHostedID("webdav", priv)
	if cleanID != priv {
		p.setLocator("dav", cleanID, priv)
	}
	return cleanID, true
}

func (p *SelfHostedProvider) refreshWebDAVRef(ctx context.Context, baseU *url.URL, id string, isPlaylist bool) string {
	if isPlaylist {
		_, _ = p.webdavPlaylists(ctx)
	} else if basePath, _ := splitSelfHostedHash(id); strings.Trim(path.Dir(basePath), "/") == strings.Trim(baseU.EscapedPath(), "/") {
		_, _ = p.webdavPlaylistTracks(ctx, "root")
	} else {
		parent := strings.Trim(path.Dir(basePath), "/")
		pls, _ := p.webdavPlaylists(ctx)
		matched := false
		for _, pl := range pls {
			if plBase, _ := splitSelfHostedHash(pl.ID); pl.ID != "root" && strings.Trim(plBase, "/") == parent {
				_, _ = p.webdavPlaylistTracks(ctx, pl.ID)
				matched = true
			}
		}
		if !matched {
			_, _ = p.webdavPlaylistTracks(ctx, "/"+strings.TrimLeft(parent+"/", "/"))
		}
	}
	return p.lookupLocator("dav", id)
}

func (p *SelfHostedProvider) webdavPropfind(ctx context.Context, targetURL string) ([]davResponse, error) {
	reqU, err := resolveWebDAVURL(p.cfg.URL, targetURL)
	if err != nil {
		return nil, err
	}
	propfindXML := `<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:displayname/>
    <D:resourcetype/>
    <D:getcontentlength/>
  </D:prop>
</D:propfind>`
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", reqU.String(), bytes.NewBufferString(propfindXML))
	if err != nil {
		return nil, errors.New("webdav: invalid propfind URL")
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=\"utf-8\"")
	p.setWebDAVAuth(req)

	resp, err := p.authHTTPClient(reqU).Do(req)
	if err != nil {
		return nil, safeSelfHostedError("webdav", "propfind", req.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("webdav: propfind HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, safeSelfHostedError("webdav", "read", req.URL, err)
	}

	var multi davMultistatus
	if err := xml.Unmarshal(body, &multi); err != nil {
		return nil, errors.New("webdav: invalid XML response")
	}
	return multi.Responses, nil
}

func isDavCollection(resp davResponse) bool {
	for _, ps := range resp.Propstat {
		if ps.Prop.ResourceType.Collection != nil {
			return true
		}
	}
	cleanHref := strings.TrimRight(resp.Href, "/") + "/"
	if strings.HasSuffix(resp.Href, "/") && !strings.Contains(path.Base(cleanHref), ".") {
		return true
	}
	return false
}

func getDavProp(resp davResponse) davProp {
	for _, ps := range resp.Propstat {
		if ps.Prop.DisplayName != "" || ps.Prop.ContentLength > 0 || ps.Prop.ResourceType.Collection != nil {
			return ps.Prop
		}
	}
	if len(resp.Propstat) > 0 {
		return resp.Propstat[0].Prop
	}
	return davProp{}
}

func (p *SelfHostedProvider) webdavPlaylists(ctx context.Context) ([]Playlist, error) {
	baseU, err := validateBaseURL("webdav", p.cfg.URL)
	if err != nil {
		return nil, err
	}
	responses, err := p.webdavPropfind(ctx, safeURLRoute(baseU))
	if err != nil {
		return nil, err
	}

	baseCleanPath := "/" + strings.Trim(baseU.Path, "/") + "/"
	if baseCleanPath == "//" {
		baseCleanPath = "/"
	}

	var dirPlaylists []Playlist
	rootTrackCount := 0

	for _, resp := range responses {
		parsedHref, err := url.Parse(resp.Href)
		if err != nil {
			continue
		}
		hrefPath := "/" + strings.Trim(parsedHref.Path, "/") + "/"
		if hrefPath == "//" {
			hrefPath = "/"
		}
		// Skip self
		if hrefPath == baseCleanPath {
			continue
		}

		prop := getDavProp(resp)
		if isDavCollection(resp) {
			cleanID, ok := p.webdavCleanRef(resp.Href)
			if !ok {
				continue
			}
			folderName := strings.Trim(parsedHref.Path, "/")
			if idx := strings.LastIndex(folderName, "/"); idx >= 0 {
				folderName = folderName[idx+1:]
			}
			folderName, _ = url.PathUnescape(folderName)
			if prop.DisplayName != "" {
				folderName = prop.DisplayName
			}
			dirPlaylists = append(dirPlaylists, Playlist{
				ID:    cleanID,
				Title: folderName,
				Count: 0,
			})
		} else {
			ext := strings.ToLower(filepath.Ext(parsedHref.Path))
			if webdavAudioExts[ext] {
				if t, ok := p.davResponseToTrack(resp, "root"); ok {
					rootTrackCount++
					p.cacheWebDAVTrack(t)
				}
			}
		}
	}

	rootPlaylist := Playlist{
		ID:    "root",
		Title: "Root",
		Count: rootTrackCount,
	}
	return append([]Playlist{rootPlaylist}, dirPlaylists...), nil
}

func (p *SelfHostedProvider) davResponseToTrack(resp davResponse, playlistID string) (Track, bool) {
	cleanID, ok := p.webdavCleanRef(resp.Href)
	if !ok {
		return Track{}, false
	}
	prop := getDavProp(resp)
	basePath, _ := splitSelfHostedHash(cleanID)
	unescapedPath, _ := url.PathUnescape(basePath)
	filename := path.Base(unescapedPath)
	ext := filepath.Ext(filename)
	title := strings.TrimSuffix(filename, ext)
	if prop.DisplayName != "" {
		title = strings.TrimSuffix(prop.DisplayName, ext)
	}

	return Track{
		ID:         cleanID,
		Title:      title,
		Artist:     "",
		Album:      SanitizeSelfHostedID("webdav", playlistID),
		DurationMS: 0,
		Provider:   "webdav",
	}, true
}

func (p *SelfHostedProvider) cacheWebDAVTrack(t Track) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.webdavCache == nil {
		p.webdavCache = make(map[string]Track)
	}
	max := p.maxCache
	if max <= 0 {
		max = defaultMaxCache
	}
	if _, exists := p.webdavCache[t.ID]; !exists {
		for len(p.webdavCache) >= max && len(p.webdavKeys) > 0 {
			evictKey := p.webdavKeys[0]
			p.webdavKeys = p.webdavKeys[1:]
			delete(p.webdavCache, evictKey)
		}
		p.webdavKeys = append(p.webdavKeys, t.ID)
	}
	p.webdavCache[t.ID] = t
}

func resolveWebDAVURL(baseRaw, ref string) (*url.URL, error) {
	u, err := validateBaseURL("webdav", baseRaw)
	if err != nil {
		return nil, err
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment = nil, "", false, ""
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	if u.RawPath != "" && !strings.HasSuffix(u.RawPath, "/") {
		u.RawPath += "/"
	}
	refU, err := u.Parse(ref)
	if err != nil {
		return nil, errors.New("webdav: invalid target URL")
	}
	if !sameOrigin(refU, u) {
		return nil, errors.New("webdav: target host does not match base URL")
	}
	refU.User, refU.Fragment = nil, ""
	return refU, nil
}

func (p *SelfHostedProvider) webdavPlaylistTracks(ctx context.Context, playlistID string) ([]Track, error) {
	baseU, err := validateBaseURL("webdav", p.cfg.URL)
	if err != nil {
		return nil, err
	}
	targetURL, cleanListID := safeURLRoute(baseU), "root"
	var responses []davResponse
	if playlistID != "" && playlistID != "root" {
		cleanListID = SanitizeSelfHostedID("webdav", playlistID)
		err = p.withLocator("dav", cleanListID, playlistID, "webdav: could not resolve playlist locator",
			func() string { return p.refreshWebDAVRef(ctx, baseU, cleanListID, true) },
			func(ref string) (bool, error) {
				subU, rerr := resolveWebDAVURL(p.cfg.URL, ref)
				if rerr != nil {
					return false, rerr
				}
				targetURL = subU.String()
				responses, rerr = p.webdavPropfind(ctx, targetURL)
				return isRetryableLocatorError(rerr), rerr
			})
	} else {
		responses, err = p.webdavPropfind(ctx, targetURL)
	}
	if err != nil {
		return nil, err
	}

	parsedTarget, _ := url.Parse(targetURL)
	targetCleanPath := "/" + strings.Trim(parsedTarget.Path, "/") + "/"
	if targetCleanPath == "//" {
		targetCleanPath = "/"
	}

	var tracks []Track
	for _, resp := range responses {
		parsedHref, err := url.Parse(resp.Href)
		if err != nil {
			continue
		}
		hrefPath := "/" + strings.Trim(parsedHref.Path, "/") + "/"
		if hrefPath == "//" {
			hrefPath = "/"
		}
		// Skip self directory
		if hrefPath == targetCleanPath {
			continue
		}
		if isDavCollection(resp) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(parsedHref.Path))
		if webdavAudioExts[ext] {
			if t, ok := p.davResponseToTrack(resp, cleanListID); ok {
				p.cacheWebDAVTrack(t)
				tracks = append(tracks, t)
			}
		}
	}
	return tracks, nil
}

func (p *SelfHostedProvider) webdavSearch(ctx context.Context, query string) ([]Track, error) {
	if _, err := validateBaseURL("webdav", p.cfg.URL); err != nil {
		return nil, err
	}
	p.mu.RLock()
	cachedCount := len(p.webdavCache)
	p.mu.RUnlock()

	// If cache is empty, prime it with root tracks
	if cachedCount == 0 {
		_, _ = p.webdavPlaylistTracks(ctx, "root")
	}

	return p.filenameSearch(query), nil
}

func (p *SelfHostedProvider) filenameSearch(query string) []Track {
	p.mu.RLock()
	defer p.mu.RUnlock()
	q := strings.ToLower(query)
	var out []Track
	for _, t := range p.webdavCache {
		if strings.Contains(strings.ToLower(t.Title), q) || strings.Contains(strings.ToLower(t.ID), q) {
			out = append(out, t)
		}
	}
	return out
}

func (p *SelfHostedProvider) webdavDownloadTrack(ctx context.Context, trackID, destDir string) (string, error) {
	baseU, err := validateBaseURL("webdav", p.cfg.URL)
	if err != nil {
		return "", err
	}
	cleanTrackID := SanitizeSelfHostedID("webdav", trackID)
	resp, targetU, _, err := p.getWithLocator(ctx, "webdav", "dav", cleanTrackID, trackID, "webdav: could not resolve track locator", baseU,
		func(ref string) (string, error) {
			u, e := resolveWebDAVURL(p.cfg.URL, ref)
			if e != nil {
				return "", e
			}
			return u.String(), nil
		}, p.setWebDAVAuth, func() string { return p.refreshWebDAVRef(ctx, baseU, cleanTrackID, false) })
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("webdav: download HTTP %d", resp.StatusCode)
	}

	ext := filepath.Ext(targetU.Path)
	if ext == "" {
		ext = parseExtFromHeader(resp.Header.Get("Content-Disposition"))
		if ext == "" {
			ext = extFromContentType(resp.Header.Get("Content-Type"))
		}
		if ext == "" {
			ext = ".mp3"
		}
	}
	filename := nasCacheFilename(targetU, nasCacheResource(targetU), ext)
	outPath, err := saveStreamToFile(destDir, filename, resp.Body)
	if err != nil {
		return "", safeSelfHostedError("webdav", "download", targetU, err)
	}
	return outPath, nil
}

// ----------------------------------------------------------------------------
// Plex Media Server
// ----------------------------------------------------------------------------

type plexContainer struct {
	MediaContainer struct {
		Size     int            `json:"size"`
		Metadata []plexMetadata `json:"Metadata"`
		Hub      []struct {
			Type     string         `json:"type"`
			Metadata []plexMetadata `json:"Metadata"`
		} `json:"Hub,omitempty"`
	} `json:"MediaContainer"`
}

type plexMetadata struct {
	RatingKey        string      `json:"ratingKey"`
	Key              string      `json:"key"`
	Title            string      `json:"title"`
	Type             string      `json:"type"`
	PlaylistType     string      `json:"playlistType"`
	LeafCount        int         `json:"leafCount"`
	Duration         int64       `json:"duration"` // ms
	GrandparentTitle string      `json:"grandparentTitle"`
	ParentTitle      string      `json:"parentTitle"`
	Thumb            string      `json:"thumb"`
	Media            []plexMedia `json:"Media"`
}

type plexMedia struct {
	ID   int        `json:"id"`
	Part []plexPart `json:"Part"`
}

type plexPart struct {
	ID   int    `json:"id"`
	Key  string `json:"key"`
	File string `json:"file"`
}

func (p *SelfHostedProvider) plexToken() string {
	if p.cfg.Token != "" {
		return p.cfg.Token
	}
	return p.cfg.Password
}

func (p *SelfHostedProvider) setPlexHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	if tok := p.plexToken(); tok != "" {
		req.Header.Set("X-Plex-Token", tok)
	}
}

func (p *SelfHostedProvider) setLocator(kind, publicID, privateRef string) {
	if publicID == "" || privateRef == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.locatorMap == nil {
		p.locatorMap = make(map[string]string)
	}
	max := p.maxCache
	if max <= 0 {
		max = defaultMaxCache
	}
	key := kind + "\x00" + publicID
	if _, exists := p.locatorMap[key]; !exists {
		for len(p.locatorMap) >= max && len(p.locatorOrder) > 0 {
			delete(p.locatorMap, p.locatorOrder[0])
			p.locatorOrder = p.locatorOrder[1:]
		}
		p.locatorOrder = append(p.locatorOrder, key)
	}
	p.locatorMap[key] = privateRef
}

func plexPrivatePart(baseU *url.URL, rawPartKey string) (string, string, bool) {
	s := strings.TrimSpace(rawPartKey)
	pu, err := url.Parse(s)
	if s == "" || err != nil || pu.EscapedPath() == "" || hasRawPathTraversal(pu) ||
		((pu.Scheme != "" || pu.Host != "") && (baseU == nil || !sameOrigin(pu, baseU))) {
		return "", "", false
	}
	cleanPath := pu.EscapedPath()
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}
	if (pu.Scheme != "" || pu.Host != "") && baseU != nil {
		if basePath := strings.TrimRight(baseU.Path, "/"); basePath != "" && strings.HasPrefix(cleanPath, basePath+"/") {
			cleanPath = strings.TrimPrefix(cleanPath, basePath)
		}
	}
	priv := cleanPath
	if enc := filterQuery(pu.RawQuery, func(k string) bool { return strings.EqualFold(k, "x-plex-token") }); enc != "" {
		priv += "?" + enc
	}
	return SanitizeSelfHostedID("plex", priv), priv, true
}

func (p *SelfHostedProvider) plexGet(ctx context.Context, pathAndQuery string, out *plexContainer) error {
	baseU, err := validateBaseURL("plex", p.cfg.URL)
	if err != nil {
		return err
	}
	u := cleanBaseURL(p.cfg.URL) + pathAndQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return errors.New("plex: invalid request URL")
	}
	p.setPlexHeaders(req)

	resp, err := p.authHTTPClient(baseU).Do(req)
	if err != nil {
		return safeSelfHostedError("plex", "request", req.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("plex: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return safeSelfHostedError("plex", "read", req.URL, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("plex: invalid JSON response")
	}
	return nil
}

func (p *SelfHostedProvider) plexPlaylists(ctx context.Context) ([]Playlist, error) {
	var c plexContainer
	if err := p.plexGet(ctx, "/playlists?playlistType=audio", &c); err != nil {
		return nil, err
	}
	baseU, _ := validateBaseURL("plex", p.cfg.URL)
	var out []Playlist
	for _, m := range c.MediaContainer.Metadata {
		id := SanitizeSelfHostedID("plex", m.RatingKey)
		if rk := strings.TrimSpace(m.RatingKey); strings.Contains(rk, "://") || strings.HasPrefix(rk, "//") || strings.HasPrefix(rk, "/") {
			clean, priv, ok := plexPrivatePart(baseU, rk)
			if !ok {
				continue
			}
			id = clean
			p.setLocator("plexpl", id, priv)
		}
		if clean, priv, ok := plexPrivatePart(baseU, m.Key); ok {
			id = cmp.Or(id, clean)
			p.setLocator("plexpl", id, priv)
		}
		if id != "" {
			out = append(out, Playlist{ID: id, Title: m.Title, Count: m.LeafCount})
		}
	}
	return out, nil
}

func (p *SelfHostedProvider) plexMetadataToTrack(m plexMetadata) Track {
	baseU, _ := validateBaseURL("plex", p.cfg.URL)
	var rawPartKey string
	if len(m.Media) > 0 && len(m.Media[0].Part) > 0 {
		rawPartKey = strings.TrimSpace(m.Media[0].Part[0].Key)
	}

	cleanRating := SanitizeSelfHostedID("plex", m.RatingKey)
	trackID := cleanRating
	if rawPartKey != "" {
		if cleanPart, privPart, ok := plexPrivatePart(baseU, rawPartKey); ok {
			p.setLocator("plex", cleanRating, privPart)
			p.setLocator("plex", cleanPart, privPart)
			if rawPartKey == cleanPart || cleanRating == "" {
				trackID = cleanPart
			}
		}
	}

	artist := m.GrandparentTitle
	if artist == "" {
		artist = m.ParentTitle
	}

	var coverURL string
	if m.Thumb != "" {
		if strings.HasPrefix(m.Thumb, "http://") || strings.HasPrefix(m.Thumb, "https://") {
			coverURL = p.sanitizeTrackCover(m.Thumb)
		} else if base := cleanBaseURL(p.cfg.URL); base != "" {
			if tu, err := url.Parse(m.Thumb); err == nil && tu.Scheme == "" && tu.Host == "" {
				if candidate := SanitizeCoverURL(base + "/" + strings.TrimPrefix(m.Thumb, "/")); p.MatchesCoverURL(candidate) {
					coverURL = candidate
				}
			}
		}
	}

	return Track{
		ID:         trackID,
		Title:      m.Title,
		Artist:     artist,
		Album:      m.ParentTitle,
		DurationMS: m.Duration,
		CoverURL:   coverURL,
		Provider:   "plex",
	}
}

func (p *SelfHostedProvider) plexPlaylistTracks(ctx context.Context, playlistID string) ([]Track, error) {
	baseU, err := validateBaseURL("plex", p.cfg.URL)
	if err != nil {
		return nil, err
	}
	fallback := "/playlists/" + url.PathEscape(playlistID) + "/items"
	if strings.HasPrefix(playlistID, "/") || strings.HasPrefix(playlistID, "//") || strings.Contains(playlistID, "://") {
		_, fallback, _ = plexPrivatePart(baseU, playlistID)
	}
	var c plexContainer
	err = p.withLocator("plexpl", playlistID, fallback, "plex: invalid playlist ID",
		func() string {
			_, _ = p.plexPlaylists(ctx)
			return p.lookupLocator("plexpl", playlistID)
		},
		func(reqPath string) (bool, error) {
			rerr := p.plexGet(ctx, reqPath, &c)
			return isRetryableLocatorError(rerr), rerr
		})
	if err != nil {
		return nil, err
	}
	var out []Track
	for _, m := range c.MediaContainer.Metadata {
		out = append(out, p.plexMetadataToTrack(m))
	}
	return out, nil
}

func (p *SelfHostedProvider) plexSearch(ctx context.Context, query string) ([]Track, error) {
	// First try /hubs/search?query=<q>&limit=50
	path := "/hubs/search?query=" + url.QueryEscape(query) + "&limit=50"
	var c plexContainer
	err := p.plexGet(ctx, path, &c)
	var out []Track
	if err == nil {
		for _, hub := range c.MediaContainer.Hub {
			if hub.Type == "track" || hub.Type == "" {
				for _, m := range hub.Metadata {
					if m.Type == "track" || m.Type == "" {
						out = append(out, p.plexMetadataToTrack(m))
					}
				}
			}
		}
	}

	// Fallback to /library/all?type=10&title=<q> if no tracks found
	if len(out) == 0 {
		path2 := "/library/all?type=10&title=" + url.QueryEscape(query)
		var c2 plexContainer
		if err2 := p.plexGet(ctx, path2, &c2); err2 == nil {
			for _, m := range c2.MediaContainer.Metadata {
				out = append(out, p.plexMetadataToTrack(m))
			}
		}
	}
	return out, nil
}

func (p *SelfHostedProvider) refreshPlexPart(ctx context.Context, trackID string) string {
	var c plexContainer
	if !strings.HasPrefix(trackID, "/") {
		if p.plexGet(ctx, "/library/metadata/"+url.PathEscape(trackID), &c) == nil && len(c.MediaContainer.Metadata) > 0 {
			p.plexMetadataToTrack(c.MediaContainer.Metadata[0])
		}
		return p.lookupLocator("plex", trackID)
	}
	if pls, err := p.plexPlaylists(ctx); err == nil {
		for i := 0; i < len(pls) && i < 20; i++ {
			_, _ = p.plexPlaylistTracks(ctx, pls[i].ID)
			if pk := p.lookupLocator("plex", trackID); pk != "" {
				return pk
			}
		}
	}
	return p.lookupLocator("plex", trackID)
}

func (p *SelfHostedProvider) plexDownloadTrack(ctx context.Context, trackID, destDir string) (string, error) {
	baseU, err := validateBaseURL("plex", p.cfg.URL)
	if err != nil {
		return "", err
	}
	if strings.Contains(trackID, "://") || strings.HasPrefix(trackID, "//") {
		u, err := url.Parse(trackID)
		if err != nil || u.Host != "" {
			return "", errors.New("plex: invalid track ID")
		}
	}
	fallback := ""
	if strings.HasPrefix(trackID, "/") {
		_, fallback, _ = plexPrivatePart(baseU, trackID)
	}
	resp, reqURL, _, err := p.getWithLocator(ctx, "plex", "plex", trackID, fallback, "plex: could not resolve track part key", baseU,
		func(pk string) (string, error) {
			if !strings.HasPrefix(pk, "/") {
				return "", errors.New("plex: could not resolve track part key")
			}
			return cleanBaseURL(p.cfg.URL) + pk, nil
		}, p.setPlexHeaders, func() string { return p.refreshPlexPart(ctx, trackID) })
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("plex: download HTTP %d", resp.StatusCode)
	}

	ext := filepath.Ext(reqURL.Path)
	if ext == "" {
		ext = parseExtFromHeader(resp.Header.Get("Content-Disposition"))
		if ext == "" {
			ext = extFromContentType(resp.Header.Get("Content-Type"))
		}
		if ext == "" {
			ext = ".mp3"
		}
	}
	filename := nasCacheFilename(baseU, nasCacheResource(reqURL), ext)
	outPath, err := saveStreamToFile(destDir, filename, resp.Body)
	if err != nil {
		return "", safeSelfHostedError("plex", "download", reqURL, err)
	}
	return outPath, nil
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

// Keep NAS downloads flat for the source index and the shared cache quota.
func nasCacheFilename(server *url.URL, resource, ext string) string {
	host := strings.ToLower(server.Host)
	if server.Scheme == "http" && server.Port() == "80" {
		host = strings.TrimSuffix(host, ":80")
	} else if server.Scheme == "https" && server.Port() == "443" {
		host = strings.TrimSuffix(host, ":443")
	}
	sum := sha256.Sum256([]byte(server.Scheme + "://" + host + "\x00" + resource))
	return fmt.Sprintf("nas-%x%s", sum, strings.ToLower(ext))
}

func parseExtFromHeader(cd string) string {
	if cd == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(cd)
	if err == nil {
		if fn := params["filename"]; fn != "" {
			return filepath.Ext(fn)
		}
	}
	return ""
}

func extFromContentType(ct string) string {
	if ct == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	}
	switch mediaType {
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/flac", "audio/x-flac":
		return ".flac"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/aac":
		return ".aac"
	case "audio/ogg", "application/ogg":
		return ".ogg"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/aiff", "audio/x-aiff":
		return ".aiff"
	default:
		return ""
	}
}

func saveStreamToFile(destDir, filename string, r io.Reader) (string, error) {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", err
	}
	targetPath := filepath.Join(destDir, filename)
	tmp, err := os.CreateTemp(destDir, ".tmp-download-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	if err := os.Rename(tmpName, targetPath); err != nil {
		return "", err
	}
	tmpName = ""
	return targetPath, nil
}
