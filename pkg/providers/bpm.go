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
	DefaultReccoURL      = "https://api.reccobeats.com"
	defaultSoundCloudURL = "https://api.soundcloud.com"
	defaultGetSongURL    = "https://api.getsong.co"

	reccoBatch       = 40
	bpmListWait      = 4 * time.Second
	bpmListLookups   = 25
	bpmListWorkers   = 6
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

	mu       sync.Mutex
	loaded   bool
	known    map[string]BPMInfo
	tried    map[string]bool // lookups that found nothing, until the next restart
	queue    chan Track
	waitTill time.Time
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

// Enrich fills an opened list in place. It waits briefly for the batch lookup and
// for the first tracks without an ISRC or Spotify ID, such as NetEase's, so the
// rows on screen show a tempo at once. The rest fill in behind the scenes, and a
// later listing shows them.
func (b *BPMStore) Enrich(ctx context.Context, tracks []Track) {
	if b == nil {
		return
	}
	b.Apply(tracks)
	ctx, cancel := context.WithTimeout(ctx, bpmListWait)
	defer cancel()
	b.batch(ctx, tracks)
	b.Apply(tracks)
	var first []Track
	for _, t := range tracks {
		if t.BPM == 0 && len(first) < bpmListLookups && b.claim(t) {
			first = append(first, t)
		}
	}
	work := make(chan Track)
	var wg sync.WaitGroup
	for range min(bpmListWorkers, len(first)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				b.attempt(ctx, t)
			}
		}()
	}
	for _, t := range first {
		work <- t
	}
	close(work)
	wg.Wait()
	b.Apply(tracks)
	for _, t := range tracks {
		if t.BPM == 0 {
			b.enqueue(t)
		}
	}
}

// EnrichPreview is Enrich for search results. They change with every keystroke, so
// nothing waits on per-track lookups.
func (b *BPMStore) EnrichPreview(ctx context.Context, tracks []Track) {
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
	}
}

func (b *BPMStore) enqueue(t Track) {
	if !b.claim(t) {
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

// lookup tries the per-track sources, cheapest first.
func (b *BPMStore) lookup(ctx context.Context, t Track) BPMInfo {
	if t.Provider == "deezer" {
		d := &Deezer{BaseURL: b.DeezerURL, HTTP: b.HTTP}
		if full, err := d.ByID(ctx, t.ID); err == nil && full.BPM > 0 {
			return BPMInfo{BPM: full.BPM}
		}
	}
	if info := b.reccoSearch(ctx, t); info.BPM > 0 {
		return info
	}
	cfg := b.Config()
	if cfg.GetSongBPMKey != "" {
		if info := b.getSong(ctx, t, cfg.GetSongBPMKey); info.BPM > 0 {
			return info
		}
	}
	if cfg.SoundCloudClientID != "" || cfg.SoundCloudToken != "" {
		return b.soundCloud(ctx, t, cfg)
	}
	return BPMInfo{}
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

func (b *BPMStore) soundCloud(ctx context.Context, t Track, cfg BPMConfig) BPMInfo {
	base := b.SoundCloudURL
	if base == "" {
		base = defaultSoundCloudURL
	}
	q := url.Values{"q": {primaryArtist(t) + " " + t.Title}, "limit": {"10"}}
	var h http.Header
	if cfg.SoundCloudToken != "" {
		h = http.Header{"Authorization": {"OAuth " + cfg.SoundCloudToken}}
	} else {
		q.Set("client_id", cfg.SoundCloudClientID)
	}
	var r []struct {
		Title string  `json:"title"`
		BPM   float64 `json:"bpm"`
		Key   string  `json:"key_signature"`
		User  struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if b.get(ctx, strings.TrimRight(base, "/")+"/tracks?"+q.Encode(), h, &r) != nil {
		return BPMInfo{}
	}
	for _, v := range r {
		if v.BPM > 0 && sameSong(t, v.Title, []string{v.User.Username}) {
			return BPMInfo{BPM: v.BPM, Key: camelotFromName(v.Key)}
		}
	}
	return BPMInfo{}
}

// Config reads the optional service keys.
func (b *BPMStore) Config() BPMConfig {
	var v BPMConfig
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

// camelotFromName reads names such as "Am", "C#" and "Bb minor".
func camelotFromName(name string) string {
	m := regexp.MustCompile(`^([A-Ga-g])([#b]?)\s*(m|min|minor|maj|major)?$`).FindStringSubmatch(strings.TrimSpace(name))
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
