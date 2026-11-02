package providers

// SoundCloud through its API with the owner's own Client ID or OAuth token. Only
// streams the account may already play are fetched: previews and encrypted streams
// are refused, so tracks reserved for SoundCloud Go+ stay out of reach.

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	defaultSoundCloudAPI = "https://api-v2.soundcloud.com"
	soundCloudLikesID    = "likes"
	soundCloudListLimit  = 2000
)

// ErrNoFullStream means the track only offers a preview or an encrypted stream.
var ErrNoFullStream = errors.New("soundcloud: no full-length unencrypted stream for this track")

type SoundCloud struct {
	BaseURL  string
	HTTP     *http.Client
	ClientID string
	Token    string
	UserID   int64
	Username string
}

type scTranscoding struct {
	URL     string `json:"url"`
	Snipped bool   `json:"snipped"`
	Format  struct {
		Protocol string `json:"protocol"`
		MimeType string `json:"mime_type"`
	} `json:"format"`
}

type scTrack struct {
	ID       int64   `json:"id"`
	Title    string  `json:"title"`
	Duration int64   `json:"duration"`
	BPM      float64 `json:"bpm"`
	Policy   string  `json:"policy"`
	Auth     string  `json:"track_authorization"`
	User     struct {
		Username string `json:"username"`
	} `json:"user"`
	Publisher struct {
		ISRC   string `json:"isrc"`
		Artist string `json:"artist"`
		Album  string `json:"album_title"`
	} `json:"publisher_metadata"`
	Media struct {
		Transcodings []scTranscoding `json:"transcodings"`
	} `json:"media"`
}

func (t scTrack) track() Track {
	artist := t.Publisher.Artist
	if artist == "" {
		artist = t.User.Username
	}
	return Track{ID: strconv.FormatInt(t.ID, 10), Title: t.Title, Artist: artist, Artists: []string{artist}, Album: t.Publisher.Album, ISRC: t.Publisher.ISRC, DurationMS: t.Duration, Provider: "soundcloud", BPM: t.BPM}
}

func (s *SoundCloud) base() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return defaultSoundCloudAPI
}

func (s *SoundCloud) headers() http.Header {
	h := http.Header{"Accept": {"application/json"}}
	if s.Token != "" {
		h.Set("Authorization", "OAuth "+s.Token)
	}
	return h
}

func (s *SoundCloud) get(ctx context.Context, address string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	if s.ClientID != "" {
		q.Set("client_id", s.ClientID)
	}
	if strings.Contains(address, "?") {
		address += "&" + q.Encode()
	} else {
		address += "?" + q.Encode()
	}
	return JSON(ctx, s.HTTP, "GET", address, nil, s.headers(), out)
}

// Identify fills in the account from the token, if there is no profile yet.
func (s *SoundCloud) Identify(ctx context.Context) error {
	if s.UserID != 0 || s.Token == "" {
		return nil
	}
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := s.get(ctx, s.base()+"/me", nil, &me); err != nil {
		return err
	}
	s.UserID, s.Username = me.ID, me.Username
	return nil
}

// ResolveUser finds a profile from its link or username.
func (s *SoundCloud) ResolveUser(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("soundcloud: profile required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://soundcloud.com/" + strings.TrimPrefix(raw, "@")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Hostname() != "soundcloud.com" && u.Hostname() != "www.soundcloud.com" && u.Hostname() != "m.soundcloud.com") {
		return errors.New("soundcloud: not a SoundCloud profile link")
	}
	var v struct {
		Kind     string `json:"kind"`
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := s.get(ctx, s.base()+"/resolve", url.Values{"url": {raw}}, &v); err != nil {
		return err
	}
	if v.Kind != "user" || v.ID == 0 {
		return errors.New("soundcloud: that link is not a profile")
	}
	s.UserID, s.Username = v.ID, v.Username
	return nil
}

// sameHost keeps paging on the API, so a response cannot redirect the credentials.
func (s *SoundCloud) nextPage(next string) string {
	u, err := url.Parse(next)
	base, baseErr := url.Parse(s.base())
	if next == "" || err != nil || baseErr != nil || u.Host != base.Host || u.Scheme != base.Scheme {
		return ""
	}
	q := u.Query()
	q.Del("client_id")
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *SoundCloud) Playlists(ctx context.Context) ([]Playlist, error) {
	if err := s.Identify(ctx); err != nil {
		return nil, err
	}
	if s.UserID == 0 {
		return nil, nil // search still works with only a Client ID
	}
	out := []Playlist{{ID: soundCloudLikesID, Title: "Likes"}}
	address := s.base() + "/users/" + strconv.FormatInt(s.UserID, 10) + "/playlists?limit=100&linked_partitioning=1"
	for address != "" && len(out) < soundCloudListLimit {
		var r struct {
			Collection []struct {
				ID         int64  `json:"id"`
				Title      string `json:"title"`
				TrackCount int    `json:"track_count"`
			} `json:"collection"`
			Next string `json:"next_href"`
		}
		if err := s.get(ctx, address, nil, &r); err != nil {
			return out, nil // the profile's likes are still worth showing
		}
		for _, p := range r.Collection {
			out = append(out, Playlist{strconv.FormatInt(p.ID, 10), p.Title, p.TrackCount})
		}
		address = s.nextPage(r.Next)
	}
	return out, nil
}

func (s *SoundCloud) Tracks(ctx context.Context, id string) ([]Track, error) {
	if id == soundCloudLikesID {
		return s.likes(ctx)
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return nil, errors.New("soundcloud: invalid playlist ID")
	}
	var p struct {
		Tracks []scTrack `json:"tracks"`
	}
	if err := s.get(ctx, s.base()+"/playlists/"+id, nil, &p); err != nil {
		return nil, err
	}
	// Only the first few tracks come complete; the rest need their own lookup.
	var missing []string
	for _, t := range p.Tracks {
		if t.Title == "" {
			missing = append(missing, strconv.FormatInt(t.ID, 10))
		}
	}
	full := map[int64]scTrack{}
	for start := 0; start < len(missing); start += 50 {
		var batch []scTrack
		q := url.Values{"ids": {strings.Join(missing[start:min(start+50, len(missing))], ",")}}
		if err := s.get(ctx, s.base()+"/tracks", q, &batch); err != nil {
			return nil, err
		}
		for _, t := range batch {
			full[t.ID] = t
		}
	}
	out := make([]Track, 0, len(p.Tracks))
	for _, t := range p.Tracks {
		if t.Title == "" {
			t = full[t.ID]
		}
		if t.ID != 0 {
			out = append(out, t.track())
		}
	}
	return out, nil
}

func (s *SoundCloud) likes(ctx context.Context) ([]Track, error) {
	if err := s.Identify(ctx); err != nil {
		return nil, err
	}
	if s.UserID == 0 {
		return nil, errors.New("soundcloud: profile required")
	}
	var out []Track
	address := s.base() + "/users/" + strconv.FormatInt(s.UserID, 10) + "/track_likes?limit=100&linked_partitioning=1"
	for address != "" && len(out) < soundCloudListLimit {
		var r struct {
			Collection []struct {
				Track scTrack `json:"track"`
			} `json:"collection"`
			Next string `json:"next_href"`
		}
		if err := s.get(ctx, address, nil, &r); err != nil {
			if len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		for _, c := range r.Collection {
			if c.Track.ID != 0 {
				out = append(out, c.Track.track())
			}
		}
		address = s.nextPage(r.Next)
	}
	return out, nil
}

func (s *SoundCloud) Search(ctx context.Context, query string) ([]Track, error) {
	var r struct {
		Collection []scTrack `json:"collection"`
	}
	if err := s.get(ctx, s.base()+"/search/tracks", url.Values{"q": {query}, "limit": {"30"}}, &r); err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(r.Collection))
	for _, t := range r.Collection {
		out = append(out, t.track())
	}
	return out, nil
}

func (s *SoundCloud) SearchPlaylists(ctx context.Context, query string) ([]Playlist, error) {
	var r struct {
		Collection []struct {
			ID         int64  `json:"id"`
			Title      string `json:"title"`
			TrackCount int    `json:"track_count"`
		} `json:"collection"`
	}
	if err := s.get(ctx, s.base()+"/search/playlists", url.Values{"q": {query}, "limit": {"20"}}, &r); err != nil {
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

func usableTranscoding(t scTrack) (scTranscoding, string, bool) {
	var best scTranscoding
	bestRank, bestExt := 0, ""
	for _, c := range t.Media.Transcodings {
		if c.Snipped || c.URL == "" {
			continue
		}
		mime := strings.ToLower(c.Format.MimeType)
		rank, ext := 0, ""
		switch {
		case c.Format.Protocol == "progressive" && strings.HasPrefix(mime, "audio/mpeg"):
			rank, ext = 3, "mp3"
		case c.Format.Protocol == "hls" && strings.HasPrefix(mime, "audio/mpeg"):
			rank, ext = 2, "mp3"
		case c.Format.Protocol == "hls" && strings.HasPrefix(mime, "audio/mp4"):
			rank, ext = 1, "mp4"
		}
		if rank > bestRank {
			best, bestRank, bestExt = c, rank, ext
		}
	}
	return best, bestExt, bestRank > 0
}

// Media finds where to download a track's whole audio from.
func (s *SoundCloud) Media(ctx context.Context, id string) (SoundCloudMedia, error) {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return SoundCloudMedia{}, errors.New("soundcloud: invalid track ID")
	}
	var t scTrack
	if err := s.get(ctx, s.base()+"/tracks/"+id, nil, &t); err != nil {
		return SoundCloudMedia{}, err
	}
	if t.Policy == "SNIP" || t.Policy == "BLOCK" {
		return SoundCloudMedia{}, ErrNoFullStream
	}
	c, ext, ok := usableTranscoding(t)
	if !ok {
		return SoundCloudMedia{}, ErrNoFullStream
	}
	var stream struct {
		URL string `json:"url"`
	}
	q := url.Values{}
	if t.Auth != "" {
		q.Set("track_authorization", t.Auth)
	}
	if err := s.get(ctx, c.URL, q, &stream); err != nil {
		return SoundCloudMedia{}, err
	}
	if stream.URL == "" {
		return SoundCloudMedia{}, ErrNoFullStream
	}
	if c.Format.Protocol == "progressive" {
		return SoundCloudMedia{Ext: ext, URLs: []string{stream.URL}}, nil
	}
	urls, err := s.segments(ctx, stream.URL)
	if err != nil {
		return SoundCloudMedia{}, err
	}
	return SoundCloudMedia{Ext: ext, URLs: urls}, nil
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
