package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Deezer struct {
	BaseURL string
	UserID  string
	HTTP    *http.Client
}
type dzTrack struct {
	Contributors []struct {
		Name string `json:"name"`
	} `json:"contributors"`
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Duration int64  `json:"duration"`
	ISRC     string `json:"isrc"`
	Artist   struct {
		Name string `json:"name"`
	} `json:"artist"`
	Album struct {
		Title string `json:"title"`
	} `json:"album"`
}

func (t dzTrack) track() Track {
	artists := []string{}
	seen := map[string]bool{}
	for _, a := range append([]struct {
		Name string `json:"name"`
	}{t.Artist}, t.Contributors...) {
		if a.Name != "" && !seen[a.Name] {
			artists = append(artists, a.Name)
			seen[a.Name] = true
		}
	}
	return Track{ID: strconv.FormatInt(t.ID, 10), Title: t.Title, Artist: strings.Join(artists, ", "), Artists: artists, Album: t.Album.Title, ISRC: t.ISRC, DurationMS: t.Duration * 1000, Provider: "deezer"}
}
func (d *Deezer) base() string {
	if d.BaseURL != "" {
		return d.BaseURL
	}
	return "https://api.deezer.com"
}
func (d *Deezer) get(ctx context.Context, path string, q url.Values, out any) error {
	var raw map[string]any
	if err := JSON(ctx, d.HTTP, "GET", endpoint(d.base(), path, q), nil, nil, &raw); err != nil {
		return err
	}
	if raw["error"] != nil {
		return errors.New("deezer: API rejected request")
	}
	return remarshal(raw, out)
}
func (d *Deezer) Playlists(ctx context.Context) ([]Playlist, error) {
	if d.UserID == "" {
		return nil, errors.New("deezer: set a public user ID or open a playlist ID")
	}
	var all []Playlist
	for index := 0; index < 100000; index += 100 {
		var r struct {
			Data []struct {
				ID    int64  `json:"id"`
				Title string `json:"title"`
				Count int    `json:"nb_tracks"`
			}
			Next string `json:"next"`
		}
		if err := d.get(ctx, "/user/"+url.PathEscape(d.UserID)+"/playlists", url.Values{"index": {strconv.Itoa(index)}, "limit": {"100"}}, &r); err != nil {
			return nil, err
		}
		for _, p := range r.Data {
			all = append(all, Playlist{strconv.FormatInt(p.ID, 10), p.Title, p.Count})
		}
		if r.Next == "" {
			return all, nil
		}
	}
	return nil, errors.New("deezer: pagination limit")
}
func (d *Deezer) Tracks(ctx context.Context, id string) ([]Track, error) {
	var all []Track
	for index := 0; index < 100000; index += 100 {
		var r struct {
			Data []dzTrack
			Next string `json:"next"`
		}
		if err := d.get(ctx, "/playlist/"+url.PathEscape(id)+"/tracks", url.Values{"index": {strconv.Itoa(index)}, "limit": {"100"}}, &r); err != nil {
			return nil, err
		}
		for _, t := range r.Data {
			all = append(all, t.track())
		}
		if r.Next == "" {
			return all, nil
		}
	}
	return nil, errors.New("deezer: pagination limit")
}
func (d *Deezer) Search(ctx context.Context, query string) ([]Track, error) {
	var r struct{ Data []dzTrack }
	if err := d.get(ctx, "/search", url.Values{"q": {query}, "limit": {"100"}}, &r); err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(r.Data))
	for _, t := range r.Data {
		out = append(out, t.track())
	}
	return out, nil
}
func (d *Deezer) ChartPlaylists(ctx context.Context) ([]Playlist, error) {
	var r struct {
		Data []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
			Count int    `json:"nb_tracks"`
		}
	}
	if err := d.get(ctx, "/chart/0/playlists", url.Values{"limit": {"10"}}, &r); err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(r.Data))
	for _, v := range r.Data {
		out = append(out, Playlist{strconv.FormatInt(v.ID, 10), v.Title, v.Count})
	}
	return out, nil
}
func (d *Deezer) SearchPlaylists(ctx context.Context, query string) ([]Playlist, error) {
	var r struct {
		Data []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
			Count int    `json:"nb_tracks"`
		}
	}
	if err := d.get(ctx, "/search/playlist", url.Values{"q": {query}, "limit": {"25"}}, &r); err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(r.Data))
	for _, v := range r.Data {
		out = append(out, Playlist{strconv.FormatInt(v.ID, 10), v.Title, v.Count})
	}
	return out, nil
}
func (d *Deezer) ByISRC(ctx context.Context, isrc string) (Track, error) {
	var t dzTrack
	err := d.get(ctx, "/track/isrc:"+url.PathEscape(isrc), nil, &t)
	if err != nil {
		return Track{}, err
	}
	if t.ID == 0 {
		return Track{}, fmt.Errorf("deezer: ISRC unavailable")
	}
	return t.track(), nil
}

func (d *Deezer) ByID(ctx context.Context, id string) (Track, error) {
	var t dzTrack
	if err := d.get(ctx, "/track/"+url.PathEscape(id), nil, &t); err != nil {
		return Track{}, err
	}
	if t.ID == 0 {
		return Track{}, errors.New("deezer: missing track")
	}
	return t.track(), nil
}
