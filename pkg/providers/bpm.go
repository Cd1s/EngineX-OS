package providers

// Tempo and key for browsed tracks, from free public sources, kept in a file so a
// track is only looked up once. Lists never wait long for this: ReccoBeats' batch
// lookup gets a short deadline and everything else fills in behind the scenes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	DefaultReccoURL   = "https://api.reccobeats.com"
	defaultGetSongURL = "https://api.getsong.co"

	reccoBatch       = 40
	bpmListWait      = 500 * time.Millisecond
	bpmRateLimitWait = 5 * time.Minute
	bpmQueueLimit    = 2000
)

type BPMInfo struct {
	BPM float64 `json:"bpm"`
	Key string  `json:"key,omitempty"`
	// Measured marks a value analysed from the audio file on this deck. Catalog
	// values are only a preview for list rows and can never replace it.
	Measured bool `json:"measured,omitempty"`
}

// BPMConfig holds the optional keys of services that need one.
type BPMConfig struct {
	SoundCloudClientID string `json:"soundcloudClientId,omitempty"`
	SoundCloudSecret   string `json:"soundcloudClientSecret,omitempty"`
	SoundCloudToken    string `json:"soundcloudToken,omitempty"`
	GetSongBPMKey      string `json:"getSongBpmKey,omitempty"`
}

type BPMStore struct {
	Path, ConfigPath string
	HTTP             *http.Client
	ReccoURL         string
	SoundCloudURL    string
	GetSongURL       string
	DeezerURL        string
	// SoundCloudAuth is the token store shared with the SoundCloud source.
	SoundCloudAuth *SoundCloudAuth
	// Spotify returns the owner's login, if any, for finding a track's IDs.
	Spotify func() *Spotify
	// PreviewBPM estimates tempo and key from a preview clip, for tracks too new
	// for any catalog. It is the last resort and never replaces a measured value.
	PreviewBPM func(ctx context.Context, previewURL string) BPMInfo
	// PreviewAllowed overrides the accepted clip hosts in tests.
	PreviewAllowed func(*url.URL) bool

	mu       sync.Mutex
	loaded   bool
	known    map[string]BPMInfo
	tried    map[string]bool // lookups that found nothing, until the next restart
	queue    chan Track
	previews chan Track
	waitTill time.Time
	// Spotify's own rate limit, separate from ReccoBeats'.
	spotifyWaitTill time.Time
}

func bpmKey(t Track) string { return t.Provider + ":" + t.ID }

func (b *BPMStore) load() {
	if b.loaded {
		return
	}
	b.loaded = true
	b.known = map[string]BPMInfo{}
	b.tried = map[string]bool{}
	if data, err := os.ReadFile(b.Path); err == nil {
		json.Unmarshal(data, &b.known)
	}
}

// Put records a tempo for a track. A measured value replaces a looked-up one, and
// never the other way round.
func (b *BPMStore) Put(t Track, info BPMInfo) error {
	if info.BPM <= 0 {
		return nil
	}
	info.BPM = math.Round(info.BPM*10) / 10
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	if b.known[bpmKey(t)].Measured && !info.Measured {
		return nil
	}
	b.known[bpmKey(t)] = info
	return b.saveLocked()
}

func (b *BPMStore) saveLocked() error { return SaveJSON(b.Path, b.known) }

// Apply fills in what is already known.
func (b *BPMStore) Apply(tracks []Track) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	for i := range tracks {
		if v, ok := b.known[bpmKey(tracks[i])]; ok {
			tracks[i].BPM, tracks[i].Key = v.BPM, v.Key
		}
	}
}

// Enrich fills a list in place from the cache and one quick batch lookup. Lists must
// open at once, so everything else is looked up behind the scenes and shows the next
// time the list opens.
func (b *BPMStore) Enrich(ctx context.Context, tracks []Track) {
	if b == nil {
		return
	}
	b.Apply(tracks)
	ctx, cancel := context.WithTimeout(ctx, bpmListWait)
	defer cancel()
	b.batch(ctx, tracks)
	b.Apply(tracks)
	for _, t := range tracks {
		if t.BPM == 0 {
			b.enqueue(t)
		}
	}
}

// claim reserves a track's lookup, reporting false if it was already tried.
func (b *BPMStore) claim(t Track) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	if b.tried[bpmKey(t)] {
		return false
	}
	b.tried[bpmKey(t)] = true
	return true
}

// attempt looks a claimed track up. A lookup cut short says nothing about the
// track, so it stays eligible for the next listing.
func (b *BPMStore) attempt(ctx context.Context, t Track) {
	info := b.lookup(ctx, t)
	if info.BPM > 0 {
		b.Put(t, info)
		return
	}
	if ctx.Err() != nil || b.rateLimited() {
		b.mu.Lock()
		delete(b.tried, bpmKey(t))
		b.mu.Unlock()
		return
	}
	b.enqueuePreview(t)
}

// enqueuePreview hands a track no catalog knows to the preview worker. There is one
// worker because stream-sync runs on a single background CPU, so more would only
// take turns, and it never holds up a list.
func (b *BPMStore) enqueuePreview(t Track) {
	if b.PreviewBPM == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.previews == nil {
		b.previews = make(chan Track, bpmQueueLimit)
		go b.previewWorker(b.previews)
	}
	select {
	case b.previews <- t:
	default:
		delete(b.tried, bpmKey(t)) // full: try again on a later listing
	}
}

func (b *BPMStore) previewWorker(previews chan Track) {
	for t := range previews {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if address := b.previewFor(ctx, t); address != "" {
			b.Put(t, b.PreviewBPM(ctx, address))
		}
		cancel()
	}
}

func isDeezerPreview(u *url.URL) bool {
	host := u.Hostname()
	return u.Scheme == "https" && (host == "dzcdn.net" || strings.HasSuffix(host, ".dzcdn.net"))
}

// previewFor finds the track's Deezer preview clip: by ID, ISRC or name, and last by
// the ISRC that the owner's Spotify knows for it.
func (b *BPMStore) previewFor(ctx context.Context, t Track) string {
	d := &Deezer{BaseURL: b.DeezerURL, HTTP: b.HTTP}
	var found Track
	if t.Provider == "deezer" {
		found, _ = d.ByID(ctx, t.ID)
	}
	if found.Preview == "" && t.ISRC != "" {
		found, _ = d.ByISRC(ctx, strings.ReplaceAll(t.ISRC, "-", ""))
	}
	if found.Preview == "" {
		if results, err := d.Search(ctx, bpmQuery(t)); err == nil {
			found, _ = bestMatch(t, results)
		}
	}
	// Deezer's search misses obscure names that its ISRC lookup still knows.
	if found.Preview == "" && b.Spotify != nil && t.ISRC == "" && t.Provider != "spotify" {
		if s := b.Spotify(); s != nil {
			if results, err := s.Search(ctx, bpmQuery(t)); err == nil {
				if c, ok := bestMatch(t, results); ok && c.ISRC != "" {
					found, _ = d.ByISRC(ctx, c.ISRC)
				}
			}
		}
	}
	u, err := url.Parse(found.Preview)
	if found.Preview == "" || err != nil {
		return ""
	}
	allowed := b.PreviewAllowed
	if allowed == nil {
		allowed = isDeezerPreview
	}
	if !allowed(u) {
		return ""
	}
	return found.Preview
}

func (b *BPMStore) enqueue(t Track) {
	if t.Provider == "soundcloud" || !b.claim(t) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.queue == nil {
		b.queue = make(chan Track, bpmQueueLimit)
		go b.worker(b.queue)
	}
	select {
	case b.queue <- t:
	default:
		delete(b.tried, bpmKey(t)) // full: try again on a later listing
	}
}

func (b *BPMStore) worker(queue chan Track) {
	for t := range queue {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		b.attempt(ctx, t)
		cancel()
	}
}

func (b *BPMStore) rateLimited() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().Before(b.waitTill)
}

// get decodes a JSON response. A 429 pauses ReccoBeats rather than retrying.
func (b *BPMStore) get(ctx context.Context, address string, headers http.Header, out any) error {
	err := JSON(ctx, b.HTTP, "GET", address, nil, headers, out)
	if err != nil && strings.Contains(err.Error(), "HTTP 429") {
		b.mu.Lock()
		b.waitTill = time.Now().Add(bpmRateLimitWait)
		b.mu.Unlock()
	}
	return err
}

func (b *BPMStore) reccoURL() string {
	if b.ReccoURL != "" {
		return strings.TrimRight(b.ReccoURL, "/")
	}
	return DefaultReccoURL
}

type reccoFeatures struct {
	Href  string  `json:"href"`
	ISRC  string  `json:"isrc"`
	Tempo float64 `json:"tempo"`
	Key   *int    `json:"key"`
	Mode  *int    `json:"mode"`
}

func (r reccoFeatures) info() BPMInfo {
	info := BPMInfo{BPM: r.Tempo}
	if r.Key != nil && r.Mode != nil {
		info.Key = Camelot(*r.Key, *r.Mode)
	}
	return info
}

// batch asks ReccoBeats about every track with a Spotify ID or ISRC.
func (b *BPMStore) batch(ctx context.Context, tracks []Track) {
	byID := map[string]Track{}
	ids := []string{}
	for _, t := range tracks {
		if t.BPM != 0 {
			continue
		}
		id := t.ISRC
		if t.Provider == "spotify" {
			id = t.ID
		}
		if id == "" {
			continue
		}
		id = strings.ToUpper(strings.ReplaceAll(id, "-", ""))
		if t.Provider == "spotify" {
			id = t.ID
		}
		if _, seen := byID[id]; !seen {
			byID[id] = t
			ids = append(ids, id)
		}
	}
	for start := 0; start < len(ids) && ctx.Err() == nil && !b.rateLimited(); start += reccoBatch {
		var r struct {
			Content []reccoFeatures `json:"content"`
		}
		q := url.Values{"ids": {strings.Join(ids[start:min(start+reccoBatch, len(ids))], ",")}}
		if b.get(ctx, b.reccoURL()+"/v1/audio-features?"+q.Encode(), nil, &r) != nil {
			return
		}
		for _, v := range r.Content {
			t, ok := byID[strings.ToUpper(v.ISRC)]
			if !ok {
				t, ok = byID[v.Href[strings.LastIndex(v.Href, "/")+1:]]
			}
			if ok {
				b.Put(t, v.info())
			}
		}
	}
}

// lookup tries the per-track sources. Most catalogs only know a track by its ISRC
// or Spotify ID, which a NetEase track lacks, so the first steps find those by name.
func (b *BPMStore) lookup(ctx context.Context, t Track) BPMInfo {
	if t.Provider == "deezer" {
		d := &Deezer{BaseURL: b.DeezerURL, HTTP: b.HTTP}
		if full, err := d.ByID(ctx, t.ID); err == nil && full.BPM > 0 {
			return BPMInfo{BPM: full.BPM}
		}
	}
	steps := []func() BPMInfo{
		func() BPMInfo { return b.viaSpotify(ctx, t) },
		func() BPMInfo { return b.viaDeezer(ctx, t) },
		func() BPMInfo { return b.reccoSearch(ctx, t) },
	}
	cfg := b.Config()
	if cfg.GetSongBPMKey != "" {
		steps = append(steps, func() BPMInfo { return b.getSong(ctx, t, cfg.GetSongBPMKey) })
	}
	if cfg.SoundCloudToken != "" || (cfg.SoundCloudClientID != "" && cfg.SoundCloudSecret != "") {
		steps = append(steps, func() BPMInfo { return b.soundCloud(ctx, t, cfg) })
	}
	for _, step := range steps {
		if info := step(); info.BPM > 0 {
			return info
		}
	}
	return BPMInfo{}
}

// bpmQuery is what a catalog with real search is asked for.
func bpmQuery(t Track) string { return primaryArtist(t) + " " + bpmMainTitle(t.Title) }

func isLatin(s string) bool {
	for _, r := range s {
		if r > unicode.MaxLatin1 {
			return false
		}
	}
	return true
}

// bestMatch picks the catalog's version of a track. A name written in Chinese is
// often spelled differently elsewhere (张震岳, A-Yue Chang), and the search already
// included it, so for those the title decides. Latin names must match.
func bestMatch(t Track, found []Track) (Track, bool) {
	for _, match := range []func(Track, string, []string) bool{sameSong, sameMainTitle} {
		for _, c := range found {
			if match(t, c.Title, c.Artists) {
				return c, true
			}
		}
	}
	if !isLatin(primaryArtist(t)) {
		for _, c := range found {
			if bpmWords(bpmMainTitle(c.Title)) == bpmWords(bpmMainTitle(t.Title)) && bpmWords(c.Title) != "" {
				return c, true
			}
		}
	}
	return Track{}, false
}

// features asks ReccoBeats about specific IDs and returns the first with a tempo.
func (b *BPMStore) features(ctx context.Context, ids ...string) BPMInfo {
	var known []string
	for _, id := range ids {
		if id != "" {
			known = append(known, id)
		}
	}
	if len(known) == 0 || b.rateLimited() {
		return BPMInfo{}
	}
	var r struct {
		Content []reccoFeatures `json:"content"`
	}
	if b.get(ctx, b.reccoURL()+"/v1/audio-features?"+url.Values{"ids": {strings.Join(known, ",")}}.Encode(), nil, &r) != nil {
		return BPMInfo{}
	}
	for _, v := range r.Content {
		if v.Tempo > 0 {
			return v.info()
		}
	}
	return BPMInfo{}
}

func (b *BPMStore) viaSpotify(ctx context.Context, t Track) BPMInfo {
	if b.Spotify == nil || t.Provider == "spotify" {
		return BPMInfo{}
	}
	b.mu.Lock()
	waiting := time.Now().Before(b.spotifyWaitTill)
	b.mu.Unlock()
	s := b.Spotify()
	if s == nil || waiting {
		return BPMInfo{}
	}
	found, err := s.Search(ctx, bpmQuery(t))
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 429") {
			b.mu.Lock()
			b.spotifyWaitTill = time.Now().Add(bpmRateLimitWait)
			b.mu.Unlock()
		}
		return BPMInfo{}
	}
	c, ok := bestMatch(t, found)
	if !ok {
		return BPMInfo{}
	}
	return b.features(ctx, c.ID, c.ISRC)
}

func (b *BPMStore) viaDeezer(ctx context.Context, t Track) BPMInfo {
	if t.Provider == "deezer" {
		return BPMInfo{}
	}
	d := &Deezer{BaseURL: b.DeezerURL, HTTP: b.HTTP}
	found, err := d.Search(ctx, bpmQuery(t))
	if err != nil {
		return BPMInfo{}
	}
	c, ok := bestMatch(t, found)
	if !ok {
		return BPMInfo{}
	}
	full, err := d.ByID(ctx, c.ID)
	if err != nil {
		return BPMInfo{}
	}
	if info := b.features(ctx, full.ISRC); info.BPM > 0 {
		return info // ReccoBeats also knows the key
	}
	return BPMInfo{BPM: full.BPM}
}

var (
	bpmEdition = regexp.MustCompile(`(?i)\(original( mix)?\)|\boriginal mix\b|\[original( mix)?\]`)
	// Everything after the song's own name: "(Club Mix)", "[Live]", ", av mix", " - Edit".
	bpmSuffix = regexp.MustCompile(`\s*[(\[].*$|\s*,.*$|\s+-\s+.*$`)
)

func bpmWords(s string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(bpmEdition.ReplaceAllString(s, " ")) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// bpmMainTitle is the song's name without any version.
func bpmMainTitle(s string) string {
	if main := strings.TrimSpace(bpmSuffix.ReplaceAllString(s, "")); main != "" {
		return main
	}
	return strings.TrimSpace(s)
}

func primaryArtist(t Track) string {
	if len(t.Artists) > 0 {
		return t.Artists[0]
	}
	return strings.TrimSpace(strings.Split(t.Artist, ",")[0])
}

func sameArtist(t Track, artists []string) bool {
	want := bpmWords(primaryArtist(t))
	for _, a := range artists {
		if want != "" && bpmWords(a) == want {
			return true
		}
	}
	return false
}

// sameSong accepts a result only for the exact title by the same main artist.
func sameSong(t Track, title string, artists []string) bool {
	return bpmWords(title) == bpmWords(t.Title) && bpmWords(t.Title) != "" && sameArtist(t, artists)
}

// sameMainTitle is sameSong that ignores versions, for catalogs that name them
// differently. A list's tempo is only a preview, so a close version will do.
func sameMainTitle(t Track, title string, artists []string) bool {
	return bpmWords(bpmMainTitle(title)) == bpmWords(bpmMainTitle(t.Title)) && bpmWords(bpmMainTitle(t.Title)) != "" && sameArtist(t, artists)
}

// reccoSearch looks a song up by name: searchText matches track titles only, so
// adding the artist or a version finds nothing. The artist is checked afterwards.
func (b *BPMStore) reccoSearch(ctx context.Context, t Track) BPMInfo {
	if b.rateLimited() {
		return BPMInfo{}
	}
	type result struct {
		ID      string `json:"id"`
		Title   string `json:"trackTitle"`
		Artists []struct {
			Name string `json:"name"`
		} `json:"artists"`
	}
	var r struct {
		Content []result `json:"content"`
	}
	q := url.Values{"searchText": {bpmMainTitle(t.Title)}}
	if b.get(ctx, b.reccoURL()+"/v1/track/search?"+q.Encode(), nil, &r) != nil {
		return BPMInfo{}
	}
	names := func(c result) []string {
		out := make([]string, len(c.Artists))
		for i, a := range c.Artists {
			out[i] = a.Name
		}
		return out
	}
	// The exact version first, then any version of the song.
	for _, match := range []func(Track, string, []string) bool{sameSong, sameMainTitle} {
		for _, c := range r.Content {
			if !match(t, c.Title, names(c)) {
				continue
			}
			var f reccoFeatures
			if b.get(ctx, b.reccoURL()+"/v1/track/"+url.PathEscape(c.ID)+"/audio-features", nil, &f) == nil && f.Tempo > 0 {
				return f.info()
			}
		}
	}
	return BPMInfo{}
}

func (b *BPMStore) getSong(ctx context.Context, t Track, key string) BPMInfo {
	base := b.GetSongURL
	if base == "" {
		base = defaultGetSongURL
	}
	var r struct {
		Search []struct {
			Title  string `json:"title"`
			Tempo  string `json:"tempo"`
			Artist struct {
				Name string `json:"name"`
			} `json:"artist"`
		} `json:"search"`
	}
	q := url.Values{"type": {"both"}, "lookup": {"song:" + t.Title + " artist:" + primaryArtist(t)}}
	if b.get(ctx, strings.TrimRight(base, "/")+"/search/?"+q.Encode(), http.Header{"X-API-KEY": {key}}, &r) != nil {
		return BPMInfo{}
	}
	for _, v := range r.Search {
		if tempo, err := strconv.ParseFloat(v.Tempo, 64); err == nil && sameSong(t, v.Title, []string{v.Artist.Name}) {
			return BPMInfo{BPM: tempo}
		}
	}
	return BPMInfo{}
}

// soundCloudTitle drops the "Artist - " many uploads start their title with.
func soundCloudTitle(s string) string {
	if i := strings.LastIndex(s, " - "); i >= 0 {
		return s[i+3:]
	}
	return s
}

// soundCloud reads tempo, or an ISRC for ReccoBeats, through the official API.
func (b *BPMStore) soundCloud(ctx context.Context, t Track, cfg BPMConfig) BPMInfo {
	s := &SoundCloud{BaseURL: b.SoundCloudURL, HTTP: b.HTTP, ClientID: cfg.SoundCloudClientID, Secret: cfg.SoundCloudSecret, Token: cfg.SoundCloudToken, Auth: b.SoundCloudAuth}
	found, err := s.Search(ctx, bpmQuery(t))
	if err != nil {
		return BPMInfo{}
	}
	for _, v := range found {
		if !sameMainTitle(t, soundCloudTitle(v.Title), v.Artists) {
			continue
		}
		if v.BPM > 0 {
			return BPMInfo{BPM: v.BPM, Key: v.Key}
		}
		if info := b.features(ctx, v.ISRC); info.BPM > 0 {
			return info
		}
	}
	return BPMInfo{}
}

// Config reads the optional service keys.
func (b *BPMStore) Config() BPMConfig {
	var v BPMConfig
	b.mu.Lock()
	defer b.mu.Unlock()
	if data, err := os.ReadFile(b.ConfigPath); err == nil {
		json.Unmarshal(data, &v)
	}
	return v
}

// SaveConfig writes the optional service keys.
func (b *BPMStore) SaveConfig(v BPMConfig) error {
	if b.ConfigPath == "" {
		return errors.New("bpm: no configuration file")
	}
	return SaveJSON(b.ConfigPath, v)
}

// Indexed by pitch class, C first.
var (
	camelotMajor = [12]int{8, 3, 10, 5, 12, 7, 2, 9, 4, 11, 6, 1}
	camelotMinor = [12]int{5, 12, 7, 2, 9, 4, 11, 6, 1, 8, 3, 10}
)

// Camelot converts a pitch class and mode (1 major, 0 minor) to e.g. "8A".
func Camelot(pitch, mode int) string {
	if pitch < 0 || pitch > 11 {
		return ""
	}
	if mode == 1 {
		return fmt.Sprintf("%dB", camelotMajor[pitch])
	}
	return fmt.Sprintf("%dA", camelotMinor[pitch])
}

// camelotFromName reads names such as "Am", "C#", "Bb minor" and SoundCloud's "F:min".
func camelotFromName(name string) string {
	m := regexp.MustCompile(`^([A-Ga-g])([#b]?)[\s:]*(m|min|minor|maj|major)?$`).FindStringSubmatch(strings.TrimSpace(name))
	if m == nil {
		return ""
	}
	pitch := map[string]int{"C": 0, "D": 2, "E": 4, "F": 5, "G": 7, "A": 9, "B": 11}[strings.ToUpper(m[1])]
	switch m[2] {
	case "#":
		pitch++
	case "b":
		pitch--
	}
	mode := 1
	if strings.HasPrefix(m[3], "m") && m[3] != "maj" && m[3] != "major" {
		mode = 0
	}
	return Camelot((pitch+12)%12, mode)
}
