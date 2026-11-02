package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Netease speaks to music.163.com directly, or to a NeteaseCloudMusicApi-compatible
// service when BaseURL is set.
type Netease struct {
	BaseURL    string
	WebURL     string // overrides https://music.163.com in tests
	HTTP       *http.Client
	CookiePath string
	mu         sync.Mutex
	Cookie     string
	UserID     int64
}
type QRLogin struct {
	Key  string `json:"key"`
	URL  string `json:"url"`
	Code int    `json:"code"`
}

type neSong struct {
	ID   int64
	Name string
	DT   int64
	AR   []struct{ Name string }
	AL   neAlbum
	// Older search responses use these names instead of dt, ar and al.
	Duration int64 `json:"duration"`
	Artists  []struct{ Name string }
	Album    neAlbum
}

type neAlbum struct {
	Name   string
	PicURL string `json:"picUrl"`
}

func (s neSong) track() Track {
	if s.DT == 0 {
		s.DT = s.Duration
	}
	if len(s.AR) == 0 {
		s.AR = s.Artists
	}
	if s.AL.Name == "" {
		s.AL = s.Album
	}
	artists := []string{}
	for _, a := range s.AR {
		artists = append(artists, a.Name)
	}
	return Track{ID: strconv.FormatInt(s.ID, 10), Title: s.Name, Artist: strings.Join(artists, ", "), Artists: artists, Album: s.AL.Name, DurationMS: s.DT, Provider: "netease", CoverURL: neteaseCover(s.AL.PicURL)}
}
func neteaseCover(pic string) string {
	if pic == "" {
		return ""
	}
	return pic + "?param=500y500"
}

func remarshal(a, b any) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, b)
}
func (n *Netease) get(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = make(url.Values)
	}
	n.mu.Lock()
	cookie := n.Cookie
	n.mu.Unlock()
	// Send cookies in POST bodies rather than URLs (reverse proxy access logs).
	q.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	if cookie != "" {
		q.Set("cookie", cookie)
	}
	h := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	var r map[string]any
	var err error
	if n.BaseURL == "" {
		r, err = n.web(ctx, path, q)
	} else {
		err = JSON(ctx, n.HTTP, "POST", endpoint(n.BaseURL, path, nil), strings.NewReader(q.Encode()), h, &r)
	}
	if err != nil {
		return err
	}
	if code, ok := r["code"].(float64); ok && code != 200 && code != 800 && code != 801 && code != 802 && code != 803 {
		return fmt.Errorf("netease: API rejected request (code %v)", code)
	}
	return remarshal(r, out)
}
func (n *Netease) BeginQR(ctx context.Context) (QRLogin, error) {
	var k struct {
		Data struct {
			Unikey string `json:"unikey"`
		}
	}
	if err := n.get(ctx, "/login/qr/key", nil, &k); err != nil {
		return QRLogin{}, err
	}
	if k.Data.Unikey == "" {
		return QRLogin{}, errors.New("netease: missing QR key")
	}
	var q struct {
		Data struct {
			URL string `json:"qrurl"`
		}
	}
	if err := n.get(ctx, "/login/qr/create", url.Values{"key": {k.Data.Unikey}}, &q); err != nil {
		return QRLogin{}, err
	}
	return QRLogin{Key: k.Data.Unikey, URL: q.Data.URL, Code: 801}, nil
}
func (n *Netease) PollQR(ctx context.Context, key string) (int, error) {
	var r struct {
		Code   int
		Cookie string
	}
	if err := n.get(ctx, "/login/qr/check", url.Values{"key": {key}}, &r); err != nil {
		return 0, err
	}
	if r.Code == 803 {
		if r.Cookie == "" {
			return 0, errors.New("netease: login returned no cookie")
		}
		n.mu.Lock()
		n.Cookie = r.Cookie
		n.mu.Unlock()
		var status struct {
			Data struct {
				Profile struct {
					UserID int64 `json:"userId"`
				}
			}
		}
		if err := n.get(ctx, "/login/status", nil, &status); err != nil {
			return 0, err
		}
		n.mu.Lock()
		n.UserID = status.Data.Profile.UserID
		err := SaveJSON(n.CookiePath, struct {
			Cookie string
			UserID int64
		}{n.Cookie, n.UserID})
		n.mu.Unlock()
		if err != nil {
			return 0, err
		}
	}
	return r.Code, nil
}
func (n *Netease) Playlists(ctx context.Context) ([]Playlist, error) {
	n.mu.Lock()
	uid := n.UserID
	n.mu.Unlock()
	if uid == 0 {
		return nil, errors.New("netease: login required")
	}
	var all []Playlist
	// Saved albums and radios appear after the playlists. If they cannot be read,
	// the playlists are still worth showing.
	var albums []Album
	var radios []Playlist
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		albums, _ = n.FavoriteAlbums(ctx)
	}()
	go func() {
		defer wg.Done()
		radios, _ = n.favoriteRadios(ctx)
	}()
	defer wg.Wait()
	withSaved := func() []Playlist {
		wg.Wait()
		for _, a := range albums {
			all = append(all, Playlist{albumPrefix + strconv.FormatInt(a.ID, 10), "【专辑】" + a.Name, a.Size})
		}
		return append(all, radios...)
	}
	for offset := 0; offset < 100000; offset += 100 {
		var r struct {
			Playlist []struct {
				ID         int64
				Name       string
				TrackCount int
			}
			More bool
		}
		if err := n.get(ctx, "/user/playlist", url.Values{"uid": {strconv.FormatInt(uid, 10)}, "limit": {"100"}, "offset": {strconv.Itoa(offset)}}, &r); err != nil {
			return nil, err
		}
		for _, p := range r.Playlist {
			all = append(all, Playlist{strconv.FormatInt(p.ID, 10), p.Name, p.TrackCount})
		}
		if !r.More {
			return withSaved(), nil
		}
	}
	return nil, errors.New("netease: pagination limit")
}

// Album is an album the account has saved.
type Album struct {
	ID      int64
	Name    string
	Size    int
	Artists []struct{ Name string }
}

const (
	albumPrefix      = "album:"
	radioPrefix      = "dj:"
	maxRadioPrograms = 500
)

// FavoriteAlbums lists the albums the account has saved.
func (n *Netease) FavoriteAlbums(ctx context.Context) ([]Album, error) {
	var all []Album
	for offset := 0; offset < 10000; offset += 100 {
		var r struct {
			Data    []Album
			HasMore bool
		}
		if err := n.get(ctx, "/album/sublist", url.Values{"limit": {"100"}, "offset": {strconv.Itoa(offset)}}, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Data...)
		if !r.HasMore {
			break
		}
	}
	return all, nil
}

func (n *Netease) favoriteRadios(ctx context.Context) ([]Playlist, error) {
	var all []Playlist
	for offset := 0; offset < 10000; offset += 100 {
		var r struct {
			DJRadios []struct {
				ID           int64
				Name         string
				ProgramCount int
			}
			HasMore bool
		}
		if err := n.get(ctx, "/dj/sublist", url.Values{"limit": {"100"}, "offset": {strconv.Itoa(offset)}}, &r); err != nil {
			return nil, err
		}
		for _, v := range r.DJRadios {
			all = append(all, Playlist{radioPrefix + strconv.FormatInt(v.ID, 10), "【电台】" + v.Name, v.ProgramCount})
		}
		if !r.HasMore {
			break
		}
	}
	return all, nil
}

func (n *Netease) albumTracks(ctx context.Context, id string) ([]Track, error) {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return nil, errors.New("netease: invalid album ID")
	}
	var r struct{ Songs []neSong }
	if err := n.get(ctx, "/album", url.Values{"id": {id}}, &r); err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(r.Songs))
	for _, s := range r.Songs {
		out = append(out, s.track())
	}
	return out, nil
}

// radioTracks lists a radio's programs. Each plays through its main song, so a
// program is an ordinary track named after the program and its host.
func (n *Netease) radioTracks(ctx context.Context, id string) ([]Track, error) {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return nil, errors.New("netease: invalid radio ID")
	}
	var all []Track
	// Some radios have thousands of programs, and paging through them all trips
	// NetEase's rate limit (code 406). The newest ones are what anyone browses.
	for offset := 0; offset < maxRadioPrograms; offset += 100 {
		var r struct {
			Programs []struct {
				Name     string
				Duration int64
				DJ       struct{ Nickname string }
				MainSong neSong
			}
			More bool
		}
		if err := n.get(ctx, "/dj/program", url.Values{"id": {id}, "limit": {"100"}, "offset": {strconv.Itoa(offset)}}, &r); err != nil {
			if len(all) > 0 {
				return all, nil
			}
			return nil, err
		}
		for _, p := range r.Programs {
			if p.MainSong.ID == 0 {
				continue
			}
			t := p.MainSong.track()
			if p.Name != "" {
				t.Title = p.Name
			}
			if t.DurationMS == 0 {
				t.DurationMS = p.Duration
			}
			if p.DJ.Nickname != "" {
				t.Artist, t.Artists = p.DJ.Nickname, []string{p.DJ.Nickname}
			}
			all = append(all, t)
		}
		if !r.More {
			break
		}
	}
	return all, nil
}

func (n *Netease) Tracks(ctx context.Context, id string) ([]Track, error) {
	if album, ok := strings.CutPrefix(id, albumPrefix); ok {
		return n.albumTracks(ctx, album)
	}
	if radio, ok := strings.CutPrefix(id, radioPrefix); ok {
		return n.radioTracks(ctx, radio)
	}
	// playlist/detail trackIds is complete even when tracks is truncated.
	var p struct {
		Playlist struct {
			TrackIDs []struct{ ID int64 } `json:"trackIds"`
		}
	}
	if err := n.get(ctx, "/playlist/detail", url.Values{"id": {id}}, &p); err != nil {
		return nil, err
	}
	var all []Track
	for start := 0; start < len(p.Playlist.TrackIDs); start += 200 {
		ids := []string{}
		for _, v := range p.Playlist.TrackIDs[start:min(start+200, len(p.Playlist.TrackIDs))] {
			ids = append(ids, strconv.FormatInt(v.ID, 10))
		}
		var r struct{ Songs []neSong }
		if err := n.get(ctx, "/song/detail", url.Values{"ids": {strings.Join(ids, ",")}}, &r); err != nil {
			return nil, err
		}
		for _, s := range r.Songs {
			all = append(all, s.track())
		}
	}
	return all, nil
}
func (n *Netease) Search(ctx context.Context, query string) ([]Track, error) {
	var r struct{ Result struct{ Songs []neSong } }
	if err := n.get(ctx, "/cloudsearch", url.Values{"keywords": {query}, "type": {"1"}, "limit": {"30"}}, &r); err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(r.Result.Songs))
	for _, s := range r.Result.Songs {
		out = append(out, s.track())
	}
	return out, nil
}

// ErrNoFullAudio means the account only gets a preview of this track, or nothing.
var ErrNoFullAudio = errors.New("netease: no full-length audio for this account")

// AudioURL returns the address NetEase gives this account for a track at the best
// quality on offer, starting at 320k MP3 unless lossless is wanted. Previews never
// count: a DJ must not load 30 seconds of a song.
func (n *Netease) AudioURL(ctx context.Context, id string, lossless bool) (string, error) {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", errors.New("netease: invalid track ID")
	}
	levels := []string{"exhigh", "standard"}
	if lossless {
		levels = append([]string{"lossless"}, levels...)
	}
	for _, level := range levels {
		var r struct {
			Data []struct {
				URL   string          `json:"url"`
				Trial json.RawMessage `json:"freeTrialInfo"`
			} `json:"data"`
		}
		if err := n.get(ctx, "/song/url/v1", url.Values{"id": {id}, "level": {level}}, &r); err != nil {
			return "", err
		}
		for _, v := range r.Data {
			if v.URL != "" && (len(v.Trial) == 0 || string(v.Trial) == "null") {
				return v.URL, nil
			}
		}
	}
	return "", ErrNoFullAudio
}

func (n *Netease) SearchPlaylists(ctx context.Context, query string) ([]Playlist, error) {
	var r struct {
		Result struct {
			Playlists []struct {
				ID         int64
				Name       string
				TrackCount int
			}
		}
	}
	if err := n.get(ctx, "/cloudsearch", url.Values{"keywords": {query}, "type": {"1000"}, "limit": {"25"}}, &r); err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(r.Result.Playlists))
	for _, p := range r.Result.Playlists {
		out = append(out, Playlist{strconv.FormatInt(p.ID, 10), p.Name, p.TrackCount})
	}
	return out, nil
}
