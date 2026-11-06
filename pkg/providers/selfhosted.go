package providers

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type NASConfig struct {
	Protocol string `json:"protocol"` // "webdav", "subsonic", "plex"
	URL      string `json:"url"`      // e.g. "https://nas.example.com/music" or "http://192.168.1.10:4533"
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"` // Plex X-Plex-Token or Bearer token
	Enabled  bool   `json:"enabled"`
}

type SelfHostedProvider struct {
	cfg         NASConfig
	client      *http.Client
	mu          sync.RWMutex
	webdavCache map[string]Track  // href/ID -> Track
	plexPartMap map[string]string // ratingKey -> Part.key or Part.key -> Part.key
}

func NewSelfHostedProvider(cfg NASConfig, c *http.Client) *SelfHostedProvider {
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &SelfHostedProvider{
		cfg:         cfg,
		client:      c,
		webdavCache: make(map[string]Track),
		plexPartMap: make(map[string]string),
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

func (p *SelfHostedProvider) Playlists(ctx context.Context) ([]Playlist, error) {
	switch strings.ToLower(p.cfg.Protocol) {
	case "subsonic":
		return p.subsonicPlaylists(ctx)
	case "webdav":
		return p.webdavPlaylists(ctx)
	case "plex":
		return p.plexPlaylists(ctx)
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

func (p *SelfHostedProvider) subsonicGet(ctx context.Context, action string, extra url.Values, out *subsonicResponse) error {
	params := p.subsonicAuthParams()
	for k, vals := range extra {
		for _, v := range vals {
			params.Add(k, v)
		}
	}
	u := strings.TrimRight(p.cfg.URL, "/") + "/rest/" + action + ".view?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("subsonic: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("subsonic: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("subsonic: invalid JSON: %w", err)
	}
	if out.SubsonicResponse.Status == "failed" {
		if out.SubsonicResponse.Error != nil {
			return fmt.Errorf("subsonic error: %s (code %d)", out.SubsonicResponse.Error.Message, out.SubsonicResponse.Error.Code)
		}
		return errors.New("subsonic error: unknown failure")
	}
	return nil
}

func (p *SelfHostedProvider) subsonicEntryToTrack(e subsonicSong) Track {
	var coverURL string
	if e.CoverArt != "" {
		if strings.HasPrefix(e.CoverArt, "http://") || strings.HasPrefix(e.CoverArt, "https://") {
			coverURL = e.CoverArt
		} else {
			artParams := p.subsonicAuthParams()
			artParams.Set("id", e.CoverArt)
			coverURL = strings.TrimRight(p.cfg.URL, "/") + "/rest/getCoverArt.view?" + artParams.Encode()
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

func (p *SelfHostedProvider) subsonicDownloadTrack(ctx context.Context, trackID, destDir string) (string, error) {
	// First try /rest/download.view?id=<id>
	params := p.subsonicAuthParams()
	params.Set("id", trackID)
	downloadURL := strings.TrimRight(p.cfg.URL, "/") + "/rest/download.view?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.httpClient().Do(req)
	if err != nil || (resp != nil && resp.StatusCode != http.StatusOK) {
		if resp != nil {
			resp.Body.Close()
		}
		// Fallback to /rest/stream.view?id=<id>&format=raw
		streamParams := p.subsonicAuthParams()
		streamParams.Set("id", trackID)
		streamParams.Set("format", "raw")
		streamURL := strings.TrimRight(p.cfg.URL, "/") + "/rest/stream.view?" + streamParams.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
		if err != nil {
			return "", err
		}
		resp, err = p.httpClient().Do(req)
		if err != nil {
			return "", fmt.Errorf("subsonic: stream failed: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("subsonic: stream HTTP %d", resp.StatusCode)
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

	filename := fmt.Sprintf("subsonic-%s%s", sanitizeFilename(trackID), ext)
	return saveStreamToFile(destDir, filename, resp.Body)
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

func (p *SelfHostedProvider) webdavPropfind(ctx context.Context, targetURL string) ([]davResponse, error) {
	propfindXML := `<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:displayname/>
    <D:resourcetype/>
    <D:getcontentlength/>
  </D:prop>
</D:propfind>`
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", targetURL, bytes.NewBufferString(propfindXML))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=\"utf-8\"")
	p.setWebDAVAuth(req)

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("webdav: propfind %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("webdav: propfind HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}

	var multi davMultistatus
	if err := xml.Unmarshal(body, &multi); err != nil {
		return nil, fmt.Errorf("webdav: invalid XML: %w", err)
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
	responses, err := p.webdavPropfind(ctx, p.cfg.URL)
	if err != nil {
		return nil, err
	}

	parsedBase, _ := url.Parse(p.cfg.URL)
	baseCleanPath := "/" + strings.Trim(parsedBase.Path, "/") + "/"
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
			folderName := strings.Trim(parsedHref.Path, "/")
			if idx := strings.LastIndex(folderName, "/"); idx >= 0 {
				folderName = folderName[idx+1:]
			}
			folderName, _ = url.PathUnescape(folderName)
			if prop.DisplayName != "" {
				folderName = prop.DisplayName
			}
			dirPlaylists = append(dirPlaylists, Playlist{
				ID:    resp.Href,
				Title: folderName,
				Count: 0,
			})
		} else {
			ext := strings.ToLower(filepath.Ext(parsedHref.Path))
			if webdavAudioExts[ext] {
				rootTrackCount++
				t := p.davResponseToTrack(resp, "root")
				p.cacheWebDAVTrack(t)
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

func (p *SelfHostedProvider) davResponseToTrack(resp davResponse, playlistID string) Track {
	prop := getDavProp(resp)
	parsedHref, _ := url.Parse(resp.Href)
	unescapedPath, _ := url.PathUnescape(parsedHref.Path)
	filename := path.Base(unescapedPath)
	ext := filepath.Ext(filename)
	title := strings.TrimSuffix(filename, ext)
	if prop.DisplayName != "" {
		title = strings.TrimSuffix(prop.DisplayName, ext)
	}

	return Track{
		ID:         resp.Href,
		Title:      title,
		Artist:     "",
		Album:      playlistID,
		DurationMS: 0,
		Provider:   "webdav",
	}
}

func (p *SelfHostedProvider) cacheWebDAVTrack(t Track) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.webdavCache == nil {
		p.webdavCache = make(map[string]Track)
	}
	p.webdavCache[t.ID] = t
}

func (p *SelfHostedProvider) webdavPlaylistTracks(ctx context.Context, playlistID string) ([]Track, error) {
	targetURL := p.cfg.URL
	if playlistID != "" && playlistID != "root" {
		u, err := url.Parse(p.cfg.URL)
		if err != nil {
			return nil, err
		}
		subU, err := u.Parse(playlistID)
		if err != nil {
			return nil, err
		}
		targetURL = subU.String()
	}

	responses, err := p.webdavPropfind(ctx, targetURL)
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
			t := p.davResponseToTrack(resp, playlistID)
			p.cacheWebDAVTrack(t)
			tracks = append(tracks, t)
		}
	}
	return tracks, nil
}

func (p *SelfHostedProvider) webdavSearch(ctx context.Context, query string) ([]Track, error) {
	p.mu.RLock()
	cachedCount := len(p.webdavCache)
	p.mu.RUnlock()

	// If cache is empty, prime it with root tracks
	if cachedCount == 0 {
		_, _ = p.webdavPlaylistTracks(ctx, "root")
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	q := strings.ToLower(query)
	var out []Track
	for _, t := range p.webdavCache {
		if strings.Contains(strings.ToLower(t.Title), q) || strings.Contains(strings.ToLower(t.ID), q) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (p *SelfHostedProvider) webdavDownloadTrack(ctx context.Context, trackID, destDir string) (string, error) {
	u, err := url.Parse(p.cfg.URL)
	if err != nil {
		return "", err
	}
	targetU, err := u.Parse(trackID)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetU.String(), nil)
	if err != nil {
		return "", err
	}
	p.setWebDAVAuth(req)

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("webdav: download %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("webdav: download HTTP %d", resp.StatusCode)
	}

	unescapedPath, _ := url.PathUnescape(targetU.Path)
	filename := path.Base(unescapedPath)
	if filename == "" || filename == "." || filename == "/" {
		filename = "webdav-track.mp3"
	}
	return saveStreamToFile(destDir, filename, resp.Body)
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

func (p *SelfHostedProvider) setPlexPart(ratingKeyOrPart, partKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.plexPartMap == nil {
		p.plexPartMap = make(map[string]string)
	}
	p.plexPartMap[ratingKeyOrPart] = partKey
}

func (p *SelfHostedProvider) getPlexPart(key string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.plexPartMap[key]
}

func (p *SelfHostedProvider) plexGet(ctx context.Context, pathAndQuery string, out *plexContainer) error {
	sep := "?"
	if strings.Contains(pathAndQuery, "?") {
		sep = "&"
	}
	u := strings.TrimRight(p.cfg.URL, "/") + pathAndQuery
	if tok := p.plexToken(); tok != "" {
		u += sep + "X-Plex-Token=" + url.QueryEscape(tok)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	p.setPlexHeaders(req)

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("plex: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("plex: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("plex: invalid JSON: %w", err)
	}
	return nil
}

func (p *SelfHostedProvider) plexPlaylists(ctx context.Context) ([]Playlist, error) {
	var c plexContainer
	if err := p.plexGet(ctx, "/playlists?playlistType=audio", &c); err != nil {
		return nil, err
	}
	var out []Playlist
	for _, m := range c.MediaContainer.Metadata {
		id := m.RatingKey
		if id == "" {
			id = m.Key
		}
		out = append(out, Playlist{
			ID:    id,
			Title: m.Title,
			Count: m.LeafCount,
		})
	}
	return out, nil
}

func (p *SelfHostedProvider) plexMetadataToTrack(m plexMetadata) Track {
	var partKey string
	if len(m.Media) > 0 && len(m.Media[0].Part) > 0 {
		partKey = m.Media[0].Part[0].Key
	}

	trackID := partKey
	if trackID == "" {
		trackID = m.RatingKey
	}
	if m.RatingKey != "" && partKey != "" {
		p.setPlexPart(m.RatingKey, partKey)
	}
	if partKey != "" {
		p.setPlexPart(partKey, partKey)
	}

	artist := m.GrandparentTitle
	if artist == "" {
		artist = m.ParentTitle
	}

	var coverURL string
	if m.Thumb != "" {
		if strings.HasPrefix(m.Thumb, "http://") || strings.HasPrefix(m.Thumb, "https://") {
			coverURL = m.Thumb
		} else {
			coverURL = strings.TrimRight(p.cfg.URL, "/") + m.Thumb
			if tok := p.plexToken(); tok != "" {
				if strings.Contains(coverURL, "?") {
					coverURL += "&X-Plex-Token=" + url.QueryEscape(tok)
				} else {
					coverURL += "?X-Plex-Token=" + url.QueryEscape(tok)
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
		Preview:    partKey,
		Provider:   "plex",
	}
}

func (p *SelfHostedProvider) plexPlaylistTracks(ctx context.Context, playlistID string) ([]Track, error) {
	path := "/playlists/" + url.PathEscape(playlistID) + "/items"
	var c plexContainer
	if err := p.plexGet(ctx, path, &c); err != nil {
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

func (p *SelfHostedProvider) plexDownloadTrack(ctx context.Context, trackID, destDir string) (string, error) {
	partKey := trackID
	if !strings.HasPrefix(partKey, "/") {
		if pk := p.getPlexPart(trackID); pk != "" {
			partKey = pk
		} else {
			// Lookup via /library/metadata/<trackID>
			var c plexContainer
			if err := p.plexGet(ctx, "/library/metadata/"+url.PathEscape(trackID), &c); err == nil && len(c.MediaContainer.Metadata) > 0 {
				m := c.MediaContainer.Metadata[0]
				if len(m.Media) > 0 && len(m.Media[0].Part) > 0 {
					partKey = m.Media[0].Part[0].Key
					p.setPlexPart(trackID, partKey)
				}
			}
		}
	}

	if !strings.HasPrefix(partKey, "/") {
		return "", fmt.Errorf("plex: could not resolve part key for track %q", trackID)
	}

	downloadURL := strings.TrimRight(p.cfg.URL, "/") + partKey
	if tok := p.plexToken(); tok != "" {
		if strings.Contains(downloadURL, "?") {
			downloadURL += "&X-Plex-Token=" + url.QueryEscape(tok)
		} else {
			downloadURL += "?X-Plex-Token=" + url.QueryEscape(tok)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	p.setPlexHeaders(req)

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("plex: download %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("plex: download HTTP %d", resp.StatusCode)
	}

	filename := path.Base(partKey)
	if filename == "" || filename == "." || filename == "/" {
		filename = fmt.Sprintf("plex-%s.mp3", sanitizeFilename(trackID))
	}
	return saveStreamToFile(destDir, filename, resp.Body)
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

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

func sanitizeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
	return strings.Trim(s, "_")
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
