package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type Deezer struct {
	BaseURL string
	UserID  string
	// ARL is the owner's Deezer login cookie, kept with the linked profile.
	ARL string
	// GatewayURL overrides https://www.deezer.com in tests.
	GatewayURL string
	HTTP       *http.Client
}

var (
	deezerUserRefs = []*regexp.Regexp{
		regexp.MustCompile(`/profile/(\d{1,20})`),
		regexp.MustCompile(`\buser-(\d{1,20})`),
		regexp.MustCompile(`[?&]host=(\d{1,20})`),
	}
	deezerPlaylistRef = regexp.MustCompile(`/playlist/(\d{1,20})`)
	deezerUserID      = regexp.MustCompile(`^\d{1,20}$`)
	deezerARL         = regexp.MustCompile(`^[0-9A-Za-z]{32,512}$`)
)

// Short links only ever lead here; nothing else is fetched on a phone's say-so.
var deezerLinkHosts = map[string]bool{"link.deezer.com": true, "deezer.page.link": true, "dzr.page.link": true, "www.deezer.com": true, "deezer.com": true}

// IsDeezerUserID reports whether s has the form of a Deezer user ID.
func IsDeezerUserID(s string) bool { return deezerUserID.MatchString(s) }

func (d *Deezer) deezerRef(ctx context.Context, s string) string {
	if decoded, err := url.QueryUnescape(s); err == nil {
		s = decoded
	}
	for _, re := range deezerUserRefs {
		if m := re.FindStringSubmatch(s); m != nil {
			return m[1]
		}
	}
	if m := deezerPlaylistRef.FindStringSubmatch(s); m != nil {
		var p struct {
			Creator struct {
				ID int64 `json:"id"`
			} `json:"creator"`
		}
		if d.get(ctx, "/playlist/"+m[1], nil, &p) == nil && p.Creator.ID != 0 {
			return strconv.FormatInt(p.Creator.ID, 10)
		}
	}
	return ""
}

// ResolveUser finds the user ID in an ID, a profile or playlist link, or a
// short link, whose redirects are followed only within Deezer's own hosts.
func (d *Deezer) ResolveUser(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if deezerUserID.MatchString(raw) {
		return raw, nil
	}
	if id := d.deezerRef(ctx, raw); id != "" {
		return id, nil
	}
	c := *client(d.HTTP)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	next, err := url.Parse(raw)
	for hop := 0; err == nil && hop < 6; hop++ {
		if next.Scheme != "https" || !deezerLinkHosts[next.Hostname()] {
			break
		}
		req, e := http.NewRequestWithContext(ctx, "GET", next.String(), nil)
		if e != nil {
			break
		}
		resp, e := c.Do(req)
		if e != nil {
			return "", errors.New("deezer: could not open the link")
		}
		resp.Body.Close()
		location, e := resp.Location()
		if e != nil {
			break
		}
		if id := d.deezerRef(ctx, location.String()); id != "" {
			return id, nil
		}
		next = location
	}
	return "", errors.New("deezer: no user ID in that link")
}

// IdentifyARL asks Deezer whose login cookie this is, and never echoes it.
func (d *Deezer) IdentifyARL(ctx context.Context, arl string) (string, error) {
	if !deezerARL.MatchString(arl) {
		return "", errors.New("deezer: malformed arl")
	}
	base := d.GatewayURL
	if base == "" {
		base = "https://www.deezer.com"
	}
	q := url.Values{"method": {"deezer.getUserData"}, "input": {"3"}, "api_version": {"1.0"}, "api_token": {"null"}}
	var r struct {
		Results struct {
			User struct {
				ID json.Number `json:"USER_ID"`
			} `json:"USER"`
		} `json:"results"`
	}
	if err := JSON(ctx, d.HTTP, "POST", endpoint(base, "/ajax/gw-light.php", q), strings.NewReader("{}"), http.Header{"Cookie": {"arl=" + arl}, "Content-Type": {"application/json"}}, &r); err != nil {
		return "", err
	}
	id := r.Results.User.ID.String()
	if !deezerUserID.MatchString(id) || strings.Trim(id, "0") == "" {
		return "", errors.New("deezer: arl is not logged in")
	}
	return id, nil
}

type dzTrack struct {
	Contributors []struct {
		Name string `json:"name"`
	} `json:"contributors"`
	ID       int64   `json:"id"`
	Title    string  `json:"title"`
	Duration int64   `json:"duration"`
	ISRC     string  `json:"isrc"`
	BPM      float64 `json:"bpm"`
	Preview  string  `json:"preview"`
	Artist   struct {
		Name string `json:"name"`
	} `json:"artist"`
	Album struct {
		Title    string `json:"title"`
		CoverBig string `json:"cover_big"`
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
	return Track{ID: strconv.FormatInt(t.ID, 10), Title: t.Title, Artist: strings.Join(artists, ", "), Artists: artists, Album: t.Album.Title, ISRC: t.ISRC, DurationMS: t.Duration * 1000, Provider: "deezer", BPM: t.BPM, Preview: t.Preview, CoverURL: t.Album.CoverBig}
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
	all, err := collectPages(ctx, deezerPages, 100, 1000, func(page int) (listPage[Playlist], error) {
		var r struct {
			Data []struct {
				ID    int64
				Title string
				Count int `json:"nb_tracks"`
			}
			Next  string
			Total int
		}
		err := d.get(ctx, "/user/"+url.PathEscape(d.UserID)+"/playlists", url.Values{"index": {strconv.Itoa(page * 100)}, "limit": {"100"}}, &r)
		out := listPage[Playlist]{more: r.Next != "", total: r.Total}
		for _, p := range r.Data {
			out.items = append(out.items, Playlist{strconv.FormatInt(p.ID, 10), p.Title, p.Count})
		}
		return out, err
	})
	return all, err
}
func (d *Deezer) Tracks(ctx context.Context, id string) ([]Track, error) {
	all, err := collectPages(ctx, deezerPages, 100, 1000, func(page int) (listPage[Track], error) {
		var r struct {
			Data  []dzTrack
			Next  string
			Total int
		}
		err := d.get(ctx, "/playlist/"+url.PathEscape(id)+"/tracks", url.Values{"index": {strconv.Itoa(page * 100)}, "limit": {"100"}}, &r)
		out := listPage[Track]{more: r.Next != "", total: r.Total}
		for _, t := range r.Data {
			out.items = append(out.items, t.track())
		}
		return out, err
	})
	return all, err
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
