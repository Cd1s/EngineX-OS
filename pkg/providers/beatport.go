package providers

// Beatport through its v4 API with the owner's own account, as the open-source
// beatportdl (github.com/unspok3n/beatportdl) does. What a track's audio comes from
// depends on the subscription: Professional downloads FLAC or 256k AAC, Advanced
// 128k AAC, and Essential only streams 128k AAC over HLS (see beatport_hls.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBeatportAPI = "https://api.beatport.com/v4"
	// BeatportClientID is the public client of Beatport's own API documentation page.
	BeatportClientID    = "ryZ8LuyQVPqbK2mBX2Hwt4qSMtnWuTYSqBPO92yQ"
	beatportTokenMargin = 300 * time.Second
	beatportChartPrefix = "chart:"
	beatportListLimit   = 2000
	// Beatport's edge turns away Go's default client name.
	beatportUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36"
)

var errBeatportNoDownload = errors.New("beatport: no download for this subscription")

// BeatportToken is an account login. Expiry is zero when unknown (a pasted token).
type BeatportToken struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token,omitempty"`
	Expiry  time.Time `json:"expiry,omitzero"`
}

// ParseBeatportToken reads a pasted token: the JSON Beatport's token endpoint
// returns, or the bare access token.
func ParseBeatportToken(text string) (BeatportToken, error) {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "Bearer "))
	if !strings.HasPrefix(text, "{") {
		if text == "" || strings.ContainsAny(text, " \t\r\n\"") {
			return BeatportToken{}, errors.New("beatport: not a token")
		}
		return BeatportToken{Access: text}, nil
	}
	var v struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
	}
	if json.Unmarshal([]byte(text), &v) != nil || v.Access == "" {
		return BeatportToken{}, errors.New("beatport: not a token")
	}
	t := BeatportToken{Access: v.Access, Refresh: v.Refresh}
	if v.Expires > 0 {
		t.Expiry = time.Now().Add(time.Duration(v.Expires) * time.Second)
	}
	return t, nil
}

// Beatport is one account. Path keeps its login (0600); empty keeps it in memory.
// The owner's instance is shared, because every refresh replaces the refresh token.
type Beatport struct {
	BaseURL string
	HTTP    *http.Client
	Path    string

	mu     sync.Mutex
	loaded bool
	tok    BeatportToken
}

func (b *Beatport) base() string {
	if b.BaseURL != "" {
		return strings.TrimRight(b.BaseURL, "/")
	}
	return defaultBeatportAPI
}

// load must be called with b.mu held.
func (b *Beatport) load() {
	if b.loaded {
		return
	}
	b.loaded = true
	if data, err := os.ReadFile(b.Path); b.Path != "" && err == nil {
		json.Unmarshal(data, &b.tok)
	}
}

// Ready reports whether there is a login.
func (b *Beatport) Ready() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	return b.tok.Access != ""
}

// Token returns the current login, which the relay hands on to the deck.
func (b *Beatport) Token() BeatportToken {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	return b.tok
}

// SetToken replaces the login and saves it.
func (b *Beatport) SetToken(t BeatportToken) error {
	if t.Access == "" {
		return errors.New("beatport: access token required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loaded, b.tok = true, t
	return SaveJSON(b.Path, t)
}

func (b *Beatport) header(token string) http.Header {
	h := http.Header{"Accept": {"application/json"}, "User-Agent": {beatportUserAgent}}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

// Login signs in as Beatport's website does, then exchanges the session for an API
// token. The password is not kept; when the refresh token runs out, the owner logs
// in again.
func (b *Beatport) Login(ctx context.Context, username, password string) error {
	if username == "" || password == "" {
		return errors.New("beatport: username and password required")
	}
	form := url.Values{"client_id": {BeatportClientID}, "grant_type": {"password"}, "username": {username}, "password": {password}}
	if code := b.authorizationCode(ctx, username, password); code != "" {
		form = url.Values{"client_id": {BeatportClientID}, "grant_type": {"authorization_code"}, "code": {code}}
	}
	t, err := b.requestToken(ctx, form)
	if err != nil {
		return err
	}
	return b.SetToken(t)
}

// authorizationCode returns "" when any step fails; the password grant is the fallback.
func (b *Beatport) authorizationCode(ctx context.Context, username, password string) string {
	c := *client(b.HTTP)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	do := func(req *http.Request) *http.Response {
		for k, v := range b.header("") {
			req.Header[k] = v
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		return resp
	}
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := http.NewRequestWithContext(ctx, "POST", b.base()+"/auth/login/", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp := do(req)
	if resp == nil {
		return ""
	}
	session := ""
	for _, c := range resp.Cookies() {
		if c.Name == "sessionid" {
			session = c.Value
		}
	}
	if session == "" {
		return ""
	}
	req, err = http.NewRequestWithContext(ctx, "GET", endpoint(b.base(), "/auth/o/authorize/", url.Values{"client_id": {BeatportClientID}, "response_type": {"code"}}), nil)
	if err != nil {
		return ""
	}
	req.AddCookie(&http.Cookie{Name: "sessionid", Value: session})
	if resp = do(req); resp == nil {
		return ""
	}
	location, err := resp.Location()
	if err != nil {
		return ""
	}
	return location.Query().Get("code")
}

func (b *Beatport) requestToken(ctx context.Context, form url.Values) (BeatportToken, error) {
	var r struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
	}
	h := b.header("")
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := JSON(ctx, b.HTTP, "POST", b.base()+"/auth/o/token/", strings.NewReader(form.Encode()), h, &r); err != nil {
		return BeatportToken{}, err
	}
	if r.Access == "" {
		return BeatportToken{}, errors.New("beatport: token response missing access token")
	}
	t := BeatportToken{Access: r.Access, Refresh: r.Refresh}
	if r.Expires > 0 {
		t.Expiry = time.Now().Add(time.Duration(r.Expires) * time.Second)
	}
	return t, nil
}

// access returns a usable access token, renewing it 300 s before it expires. stale is
// one the API just refused; it is renewed unless another caller already did.
func (b *Beatport) access(ctx context.Context, stale string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	if b.tok.Access == "" {
		return "", fmt.Errorf("beatport: not logged in: %w", StatusError(401))
	}
	refused := stale != "" && stale == b.tok.Access
	expiring := !b.tok.Expiry.IsZero() && time.Now().Add(beatportTokenMargin).After(b.tok.Expiry)
	if !refused && !expiring {
		return b.tok.Access, nil
	}
	if b.tok.Refresh == "" {
		if refused || (!b.tok.Expiry.IsZero() && time.Now().After(b.tok.Expiry)) {
			return "", fmt.Errorf("beatport: login expired: %w", StatusError(401))
		}
		return b.tok.Access, nil
	}
	t, err := b.requestToken(ctx, url.Values{"client_id": {BeatportClientID}, "grant_type": {"refresh_token"}, "refresh_token": {b.tok.Refresh}})
	if err != nil {
		if !refused && time.Now().Before(b.tok.Expiry) {
			return b.tok.Access, nil // still valid; the next request tries again
		}
		var st StatusError
		if errors.As(err, &st) && (st == 400 || st == 401) {
			return "", fmt.Errorf("beatport: login expired: %w", StatusError(401))
		}
		return "", err
	}
	if t.Refresh == "" {
		t.Refresh = b.tok.Refresh
	}
	b.tok = t
	if err := SaveJSON(b.Path, t); err != nil {
		return "", err
	}
	return t.Access, nil
}

// get makes an API call, renewing the login once if the API refuses it.
func (b *Beatport) get(ctx context.Context, path string, q url.Values, out any) error {
	token, err := b.access(ctx, "")
	if err != nil {
		return err
	}
	err = JSON(ctx, b.HTTP, "GET", endpoint(b.base(), path, q), nil, b.header(token), out)
	var status StatusError
	if errors.As(err, &status) && status == http.StatusUnauthorized {
		if token, err = b.access(ctx, token); err != nil {
			return err
		}
		err = JSON(ctx, b.HTTP, "GET", endpoint(b.base(), path, q), nil, b.header(token), out)
	}
	return err
}

// beatportPages reads a paginated listing until it ends or reaches limit items.
func beatportPages[T any](ctx context.Context, b *Beatport, path string, q url.Values, perPage, limit int, add func(T)) error {
	items, err := collectPages(ctx, beatportPageSlots, perPage, (limit+perPage-1)/perPage, func(page int) (listPage[T], error) {
		// Every request owns its query: pagination may now run concurrently.
		query := make(url.Values, len(q)+2)
		for k, v := range q {
			query[k] = append([]string(nil), v...)
		}
		query.Set("per_page", strconv.Itoa(perPage))
		query.Set("page", strconv.Itoa(page+1))
		var r struct {
			Next    *string
			Results []T
			Count   int
		}
		err := b.get(ctx, path, query, &r)
		return listPage[T]{items: r.Results, more: r.Next != nil && *r.Next != "" && len(r.Results) > 0, total: r.Count}, err
	})
	if err != nil && !errors.Is(err, errPageLimit) {
		return err
	}
	for _, v := range items {
		add(v)
	}
	return nil
}

type bpImage struct {
	URI     string `json:"uri"`
	Dynamic string `json:"dynamic_uri"`
}

func (i bpImage) sized(size string) string {
	if i.Dynamic != "" {
		return strings.ReplaceAll(i.Dynamic, "{w}x{h}", size)
	}
	return i.URI
}

type bpTrack struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	MixName  string  `json:"mix_name"`
	BPM      float64 `json:"bpm"`
	ISRC     string  `json:"isrc"`
	LengthMS int64   `json:"length_ms"`
	Artists  []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Key *struct {
		Number int    `json:"camelot_number"`
		Letter string `json:"camelot_letter"`
	} `json:"key"`
	Release struct {
		Name  string  `json:"name"`
		Image bpImage `json:"image"`
	} `json:"release"`
}

// UnmarshalJSON also accepts a playlist entry, which wraps the track as {"track": {...}}.
func (t *bpTrack) UnmarshalJSON(b []byte) error {
	type plain bpTrack
	var item struct {
		Track *plain `json:"track"`
	}
	if json.Unmarshal(b, &item) == nil && item.Track != nil && item.Track.ID != 0 {
		*t = bpTrack(*item.Track)
		return nil
	}
	return json.Unmarshal(b, (*plain)(t))
}

func (t bpTrack) track() Track {
	title := t.Name
	if t.MixName != "" && !strings.Contains(strings.ToLower(t.Name), strings.ToLower(t.MixName)) {
		title += " (" + t.MixName + ")"
	}
	names := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		names = append(names, a.Name)
	}
	out := Track{ID: strconv.FormatInt(t.ID, 10), Title: title, Artist: strings.Join(names, ", "), Artists: names, Album: t.Release.Name, ISRC: t.ISRC, DurationMS: t.LengthMS, Provider: "beatport", BPM: t.BPM, CoverURL: t.Release.Image.sized("500x500"), ThumbURL: t.Release.Image.sized("120x120")}
	if k := t.Key; k != nil && k.Number >= 1 && k.Number <= 12 && (k.Letter == "A" || k.Letter == "B") {
		out.Key = strconv.Itoa(k.Number) + k.Letter
	}
	return out
}

type bpList struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"track_count"`
}

// Playlists lists the account's own playlists, then Beatport's newest charts, so a
// new account still has something to browse.
func (b *Beatport) Playlists(ctx context.Context) ([]Playlist, error) {
	var out []Playlist
	mine := beatportPages(ctx, b, "/my/playlists/", url.Values{}, 50, beatportListLimit, func(p bpList) {
		out = append(out, Playlist{strconv.FormatInt(p.ID, 10), p.Name, p.Count})
	})
	if mine != nil {
		return nil, mine
	}
	charts := beatportPages(ctx, b, "/catalog/charts/", url.Values{"order_by": {"-publish_date"}}, 30, 30, func(p bpList) {
		out = append(out, Playlist{beatportChartPrefix + strconv.FormatInt(p.ID, 10), "【榜单】" + p.Name, p.Count})
	})
	if charts != nil && len(out) == 0 {
		return nil, charts
	}
	return out, nil
}

func validBeatportID(id string) bool {
	_, err := strconv.ParseUint(id, 10, 64)
	return err == nil
}

func (b *Beatport) trackPages(ctx context.Context, path string) ([]Track, error) {
	var out []Track
	err := beatportPages(ctx, b, path, url.Values{}, 100, beatportListLimit, func(t bpTrack) {
		if t.ID != 0 {
			out = append(out, t.track())
		}
	})
	return out, err
}

func (b *Beatport) Tracks(ctx context.Context, id string) ([]Track, error) {
	if chart, ok := strings.CutPrefix(id, beatportChartPrefix); ok {
		if !validBeatportID(chart) {
			return nil, errors.New("beatport: invalid chart ID")
		}
		return b.trackPages(ctx, "/catalog/charts/"+chart+"/tracks/")
	}
	if !validBeatportID(id) {
		return nil, errors.New("beatport: invalid playlist ID")
	}
	out, err := b.trackPages(ctx, "/my/playlists/"+id+"/tracks/")
	var status StatusError
	if errors.As(err, &status) && status == http.StatusNotFound {
		// A playlist found by search belongs to Beatport's editors, not the account.
		return b.trackPages(ctx, "/catalog/playlists/"+id+"/tracks/")
	}
	return out, err
}

func (b *Beatport) Search(ctx context.Context, query string) ([]Track, error) {
	var r struct {
		Tracks []bpTrack `json:"tracks"`
	}
	if err := b.get(ctx, "/catalog/search/", url.Values{"q": {query}, "is_available_for_streaming": {"true"}, "per_page": {"50"}}, &r); err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(r.Tracks))
	for _, t := range r.Tracks {
		if t.ID != 0 {
			out = append(out, t.track())
		}
	}
	return out, nil
}

// Download finds the signed address of a track's file at the best quality the
// subscription allows: FLAC, then 256k AAC (Professional), then 128k AAC (Advanced).
// ext is flac or mp4. Essential gets none of them and uses SaveStream instead.
func (b *Beatport) Download(ctx context.Context, id string, lossless bool) (location, ext string, err error) {
	if !validBeatportID(id) {
		return "", "", errors.New("beatport: invalid track ID")
	}
	qualities := []string{"high", "medium"}
	if lossless {
		qualities = []string{"lossless", "high", "medium"}
	}
	for _, q := range qualities {
		var r struct {
			Location string `json:"location"`
			Quality  string `json:"stream_quality"`
		}
		err := b.get(ctx, "/catalog/tracks/"+id+"/download/", url.Values{"quality": {q}}, &r)
		var status StatusError
		if errors.As(err, &status) && (status == http.StatusForbidden || status == http.StatusNotFound) {
			continue // not on this subscription
		}
		if err != nil {
			return "", "", err
		}
		switch {
		case !strings.HasPrefix(r.Location, "https://"):
		case strings.HasSuffix(r.Quality, ".flac"):
			return r.Location, "flac", nil
		case strings.HasSuffix(r.Quality, ".mp4"):
			return r.Location, "mp4", nil
		}
	}
	return "", "", errBeatportNoDownload
}
