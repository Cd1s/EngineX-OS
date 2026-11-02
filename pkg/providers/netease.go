package providers

import (
	"context"
	"encoding/json"
	"errors"
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
	AL   struct{ Name string }
}

func (s neSong) track() Track {
	artists := []string{}
	for _, a := range s.AR {
		artists = append(artists, a.Name)
	}
	return Track{ID: strconv.FormatInt(s.ID, 10), Title: s.Name, Artist: strings.Join(artists, ", "), Artists: artists, Album: s.AL.Name, DurationMS: s.DT, Provider: "netease"}
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
		return errors.New("netease: API rejected request")
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
			return all, nil
		}
	}
	return nil, errors.New("netease: pagination limit")
}
func (n *Netease) Tracks(ctx context.Context, id string) ([]Track, error) {
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
