package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/deezer"
)

const (
	deezerGW  = "https://www.deezer.com/ajax/gw-light.php"
	deezerAPI = "https://api.deezer.com"
	userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
)

var (
	arlRe     = regexp.MustCompile(`^[0-9a-fA-F]{120,300}$`)
	digitsRe  = regexp.MustCompile(`^\d{1,20}$`)
	profileRe = []*regexp.Regexp{
		regexp.MustCompile(`/profile/(\d+)`),
		regexp.MustCompile(`user-(\d+)`),
		regexp.MustCompile(`host=(\d+)`),
	}
	playlistRe = regexp.MustCompile(`/playlist/(\d+)`)
)

func validARL(a string) bool { return arlRe.MatchString(a) }

func maskSecret(s string) string {
	if len(s) <= 12 {
		return strings.Repeat("•", len(s))
	}
	return s[:6] + "…" + s[len(s)-4:] + fmt.Sprintf("（%d 位）", len(s))
}

type deezerClient struct {
	hc *http.Client
}

func newDeezerClient() *deezerClient {
	return &deezerClient{hc: &http.Client{Timeout: 20 * time.Second}}
}

func (d *deezerClient) getJSON(ctx context.Context, u string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", userAgent)
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var e struct {
		Error *struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != nil {
		return fmt.Errorf("deezer api: %s", e.Error.Message)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("deezer api: HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(b, out)
}

type dzTrack struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Duration int    `json:"duration"`
	ISRC     string `json:"isrc"`
	Readable bool   `json:"readable"`
	Artist   struct {
		Name string `json:"name"`
	} `json:"artist"`
}

func (d *deezerClient) trackByID(ctx context.Context, id string) (*dzTrack, error) {
	var t dzTrack
	if err := d.getJSON(ctx, deezerAPI+"/track/"+url.PathEscape(id), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (d *deezerClient) trackByISRC(ctx context.Context, isrc string) (*dzTrack, error) {
	return d.trackByID(ctx, "isrc:"+isrc)
}

func (d *deezerClient) searchTrack(ctx context.Context, artist, title string, duration int) (*dzTrack, error) {
	q := fmt.Sprintf(`track:"%s"`, title)
	if artist != "" {
		q = fmt.Sprintf(`artist:"%s" %s`, artist, q)
	}
	var r struct {
		Data []dzTrack `json:"data"`
	}
	if err := d.getJSON(ctx, deezerAPI+"/search/track?limit=10&q="+url.QueryEscape(q), &r); err != nil {
		return nil, err
	}
	var best *dzTrack
	for i := range r.Data {
		t := &r.Data[i]
		if duration > 0 && abs(t.Duration-duration) > 30 {
			continue
		}
		if best == nil || (duration > 0 && abs(t.Duration-duration) < abs(best.Duration-duration)) {
			best = t
		}
	}
	if best == nil {
		return nil, errors.New("deezer search: no match")
	}
	return best, nil
}

// userPlaylistsOK checks the public playlist API answers for userID.
func (d *deezerClient) userPlaylistsOK(ctx context.Context, userID string) error {
	var r struct {
		Data []json.RawMessage `json:"data"`
	}
	return d.getJSON(ctx, deezerAPI+"/user/"+userID+"/playlists?limit=1", &r)
}

func isDeezerHost(h string) bool {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h == "deezer.com" || strings.HasSuffix(h, ".deezer.com") ||
		h == "deezer.page.link" || h == "dzr.page.link"
}

// resolveUser turns a pasted profile/playlist/short link or numeric ID into
// a Deezer user ID. Only Deezer hosts are fetched (no open redirect/SSRF).
func (d *deezerClient) resolveUser(ctx context.Context, in string) (string, error) {
	in = strings.TrimSpace(in)
	if digitsRe.MatchString(in) {
		return in, nil
	}
	if f := strings.Fields(in); len(f) > 1 {
		// "Check out my profile https://link.deezer.com/s/..." share text.
		for _, w := range f {
			if strings.Contains(w, "deezer") {
				in = w
				break
			}
		}
	}
	if !strings.HasPrefix(in, "http://") && !strings.HasPrefix(in, "https://") &&
		(strings.HasPrefix(in, "link.deezer.com") || strings.HasPrefix(in, "www.deezer.com") || strings.HasPrefix(in, "deezer.com")) {
		in = "https://" + in
	}
	seen := []string{in}
	if strings.HasPrefix(in, "http://") || strings.HasPrefix(in, "https://") {
		u, err := url.Parse(in)
		if err != nil || !isDeezerHost(u.Host) {
			return "", errors.New("只支持 deezer.com 链接或数字用户 ID")
		}
		if !matchesAny(in) {
			chain, err := d.followRedirects(ctx, in)
			if err != nil && len(chain) == 0 {
				return "", fmt.Errorf("无法打开链接：%v", err)
			}
			seen = append(seen, chain...)
		}
	}
	for _, s := range seen {
		for _, v := range decodedForms(s) {
			for _, re := range profileRe {
				if m := re.FindStringSubmatch(v); m != nil && m[1] != "0" {
					return m[1], nil
				}
			}
		}
	}
	for _, s := range seen {
		for _, v := range decodedForms(s) {
			if m := playlistRe.FindStringSubmatch(v); m != nil {
				var p struct {
					Creator struct {
						ID int64 `json:"id"`
					} `json:"creator"`
				}
				if err := d.getJSON(ctx, deezerAPI+"/playlist/"+m[1], &p); err != nil {
					return "", fmt.Errorf("读取歌单失败：%v", err)
				}
				if p.Creator.ID > 0 {
					return strconv.FormatInt(p.Creator.ID, 10), nil
				}
			}
		}
	}
	return "", errors.New("没有在链接里找到 Deezer 用户 ID")
}

func matchesAny(s string) bool {
	for _, v := range decodedForms(s) {
		for _, re := range append(profileRe, playlistRe) {
			if re.MatchString(v) {
				return true
			}
		}
	}
	return false
}

// decodedForms returns s plus up to two levels of URL-decoding, since short
// links nest the destination in percent-encoded query parameters.
func decodedForms(s string) []string {
	out := []string{s}
	for i := 0; i < 2; i++ {
		u, err := url.QueryUnescape(s)
		if err != nil || u == s {
			break
		}
		out = append(out, u)
		s = u
	}
	return out
}

// followRedirects returns every URL visited, stopping at the first
// non-Deezer host or once a URL carries a recognisable ID.
func (d *deezerClient) followRedirects(ctx context.Context, start string) ([]string, error) {
	var chain []string
	hc := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			chain = append(chain, req.URL.String())
			if len(via) >= 10 || !isDeezerHost(req.URL.Host) || matchesAny(req.URL.String()) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", start, nil)
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return chain, err
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "" {
		if u, err := resp.Request.URL.Parse(loc); err == nil {
			chain = append(chain, u.String())
		}
	}
	chain = append(chain, resp.Request.URL.String())
	return chain, nil
}

func (d *deezerClient) login(ctx context.Context, arl string) (*deezer.Session, error) {
	return deezer.Login(ctx, arl)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// arlUser validates a browser cookie with deezer.getUserData. It deliberately
// does not require public playlists: account login must work for private users.
func (d *deezerClient) arlUser(ctx context.Context, arl string) (string, string, error) {
	if !validARL(arl) {
		return "", "", errors.New("invalid ARL")
	}
	q := url.Values{"method": {"deezer.getUserData"}, "input": {"3"}, "api_version": {"1.0"}, "api_token": {"null"}}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.deezer.com/ajax/gw-light.php?"+q.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.AddCookie(&http.Cookie{Name: "arl", Value: arl})
	resp, err := d.hc.Do(req)
	if err != nil {
		return "", "", errors.New("Deezer identity unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New("Deezer identity rejected")
	}
	var result struct {
		Results struct {
			User struct {
				ID   json.Number `json:"USER_ID"`
				Name string      `json:"BLOG_NAME"`
			} `json:"USER"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); err != nil {
		return "", "", errors.New("invalid Deezer identity")
	}
	id, err := result.Results.User.ID.Int64()
	if err != nil || id <= 0 {
		return "", "", errors.New("Deezer account is not logged in")
	}
	return strconv.FormatInt(id, 10), result.Results.User.Name, nil
}
