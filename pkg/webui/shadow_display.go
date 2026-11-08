package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const shadowDisplayLimit = 12 << 20

// Static display data is deliberately absent from deckState and the UDP push.
// No path to audio is sent to the display-only QML.
type shadowDisplayAsset struct {
	Version int     `json:"version"`
	Key     string  `json:"key"`
	Title   string  `json:"title"`
	Artist  string  `json:"artist"`
	BPM     float64 `json:"bpm"`
	KeyName string  `json:"keyName"`
	enginedb.DisplayPerformance
	Artwork       []byte `json:"artwork,omitempty"`
	Detail        bool   `json:"detail"`
	DetailPending bool   `json:"detailPending"`
	DetailSource  string `json:"detailSource,omitempty"`
}

type shadowDisplayEntry struct {
	asset          *shadowDisplayAsset
	wave, overview []byte
	busy           bool
	at             time.Time
	retry          time.Time
	tileMu         sync.Mutex
	tiles          [][]byte // detailed images are generated once, on first visible use
}

type shadowDisplayCache struct {
	mu      sync.Mutex
	entries map[string]*shadowDisplayEntry
}

// claim limits both memory and concurrent requests. Caller holds c.mu.
func (c *shadowDisplayCache) claim(id string, now time.Time) *shadowDisplayEntry {
	if c.entries == nil {
		c.entries = map[string]*shadowDisplayEntry{}
	}
	if e := c.entries[id]; e != nil {
		return e
	}
	if len(c.entries) >= 4 {
		oldest := ""
		for k, e := range c.entries {
			if !e.busy && (oldest == "" || e.at.Before(c.entries[oldest].at)) {
				oldest = k
			}
		}
		if oldest == "" {
			return nil
		}
		delete(c.entries, oldest)
	}
	e := &shadowDisplayEntry{at: now}
	c.entries[id] = e
	return e
}

func displayPeerID(p peerDeck) string {
	id := p.Link
	if id == "" {
		id = p.ip
	}
	return fmt.Sprintf("peer:%s:%d:%s", id, p.Deck, p.Key)
}

func displayTrackKey(p peerDeck) string {
	if p.Key == "" {
		return ""
	}
	return fmt.Sprintf("%x", fnvSum(displayPeerID(p)))
}

// Preserve #160's role-overlap mapping, while retaining the source deck for
// asset requests. The map key is the receiver slot, p.Deck is the source slot.
func placeDisplayPeers(peers map[int]peerDeck, role string, now time.Time) map[int]peerDeck {
	placed, used := map[int]peerDeck{}, map[int]bool{}
	var slots []int
	for n := 1; n <= 4; n++ {
		if strings.Contains(role, strconv.Itoa(n)) {
			continue
		}
		slots = append(slots, n)
		if p, ok := peers[n]; ok {
			placed[n] = p
			used[n] = true
		}
	}
	for _, n := range slots {
		if _, ok := placed[n]; ok {
			continue
		}
		for src := 1; src <= 4; src++ {
			if p, ok := peers[src]; ok && !used[src] {
				placed[n] = p
				used[src] = true
				break
			}
		}
	}
	return placed
}

// shadowComposeDisplay has no file resolver, download, EXPF or Engine control
// path. Even a locally accessible peer file must not load into Engine.
func (s *Server) shadowComposeDisplay(role string, now time.Time) []deckState {
	sh := s.shadow
	sh.mu.Lock()
	peers := make(map[int]peerDeck, len(sh.peerDecks))
	for n, p := range sh.peerDecks {
		peers[n] = p
	}
	sh.mu.Unlock()
	peers = placeDisplayPeers(peers, role, now)
	out := []deckState{}
	free := []int{}
	for deck := 1; deck <= 4; deck++ {
		if strings.Contains(role, strconv.Itoa(deck)) {
			continue
		}
		p, ok := peers[deck]
		if !ok {
			free = append(free, deck)
			continue
		}
		d := p.deckState
		d.Deck, d.Key = deck, displayTrackKey(p)
		d.Stale = p.Stale || now.Sub(p.at) >= 2*time.Second
		if d.Playing {
			if !p.Stale {
				d.PositionMS += float64(min(int64(2000), max(int64(0), now.Sub(p.at).Milliseconds()))) * (1 + d.Pitch/100)
			}
		}
		d.Path, d.Ext, d.Net = "", "", ""
		d.At = now.UnixMilli()
		if p.Key != "" {
			d.Loading = s.ensureShadowDisplay(p, now) == nil
		}
		out = append(out, d)
	}
	// CDJ compatibility is metadata-only in this mode. In particular this must
	// not call shadowFetchCDJ, whose prerequisite is a full audio download.
	for deck, dev := range cdjDecks(s.mergedDevices(), free, now) {
		d := deckState{Deck: deck, Playing: dev.Playing, PositionMS: float64(dev.PlayPositionMS), Pitch: dev.Pitch, Master: dev.Master, Sync: dev.Sync, BPM: dev.BPM, At: now.UnixMilli()}
		if d.Playing {
			d.PositionMS += float64(now.Sub(dev.LastSeen).Milliseconds()) * (1 + d.Pitch/100)
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b deckState) int { return a.Deck - b.Deck })
	return out
}

func (s *Server) ensureShadowDisplay(p peerDeck, now time.Time) *shadowDisplayEntry {
	c := &s.shadow.display
	id := displayPeerID(p)
	c.mu.Lock()
	e := c.claim(id, now)
	if e == nil {
		c.mu.Unlock()
		return nil
	}
	e.at = now
	if e.asset != nil && (!e.asset.DetailPending || now.Before(e.retry) || e.busy) {
		c.mu.Unlock()
		return e // immutable after publication
	}
	if e.busy || now.Before(e.retry) {
		c.mu.Unlock()
		return nil
	}
	e.busy = true
	c.mu.Unlock()
	go s.fetchShadowDisplay(p, id, e)
	if e.asset != nil {
		return e
	}
	return nil
}

func (s *Server) fetchShadowDisplay(p peerDeck, id string, slot *shadowDisplayEntry) {
	sh := s.shadow
	sh.mu.Lock()
	port := sh.peerPort
	sh.mu.Unlock()
	if port == 0 {
		port = 8080
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	u := fmt.Sprintf("http://%s/peer/decks/%d/display?key=%s", net.JoinHostPort(p.ip, strconv.Itoa(port)), p.Deck, url.QueryEscape(p.Key))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	// A peer must not redirect this read-only request to arbitrary URLs.
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	var a *shadowDisplayAsset
	var wave, overview []byte
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("display peer: HTTP %d", resp.StatusCode)
		} else {
			var b []byte
			b, err = io.ReadAll(io.LimitReader(resp.Body, shadowDisplayLimit+1))
			if err == nil && len(b) > shadowDisplayLimit {
				err = fmt.Errorf("display peer: response too large")
			}
			if err == nil {
				a = &shadowDisplayAsset{}
				err = json.Unmarshal(b, a)
			}
			if err == nil {
				err = validateDisplayAsset(a, p.Key)
			}
			if err == nil && a.Waveform != nil {
				w := downsampleDisplayWave(*a.Waveform, 65536)
				a.Waveform = &w
			}
			if err == nil {
				overview, err = renderShadowWave(a.Overview, 1024, 64)
			}
			if err == nil {
				w := a.Overview
				if a.Waveform != nil {
					w = *a.Waveform
				}
				wave, err = renderShadowWave(w, 2048, 128)
			}
		}
	}
	c := &sh.display
	c.mu.Lock()
	// Replacing the whole entry makes values returned to HTTP readers immutable.
	if c.entries[id] == slot {
		e := &shadowDisplayEntry{at: time.Now(), retry: time.Now().Add(2 * time.Second)}
		if err == nil {
			e.asset, e.wave, e.overview = a, wave, overview
			if a.Waveform != nil {
				e.tiles = make([][]byte, (len(a.Waveform.Bands)/3+2047)/2048)
			}
		} else if slot.asset != nil {
			e.asset, e.wave, e.overview = slot.asset, slot.wave, slot.overview
		}
		c.entries[id] = e
	}
	c.mu.Unlock()
	sh.dbg.event("display-fetch", map[string]any{"deck": p.Deck, "ok": err == nil})
	sh.mu.Lock()
	sh.bump()
	sh.mu.Unlock()
}

func validateDisplayAsset(a *shadowDisplayAsset, key string) error {
	finite := func(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
	if a.Version != 1 || a.Key != key || !finite(a.SampleRate) || a.SampleRate < 8000 || a.SampleRate > 384000 || !finite(a.DurationMS) || a.DurationMS <= 0 || a.DurationMS > 86400000 || !finite(a.BPM) || a.BPM < 0 || a.BPM > 1000 {
		return fmt.Errorf("display: invalid asset header")
	}
	if len(a.Title) > 4096 || len(a.Artist) > 4096 || len(a.KeyName) > 64 || len(a.Beats) > 65536 || len(a.Cues) > 9 || len(a.Loops) > 8 {
		return fmt.Errorf("display: oversized metadata")
	}
	for _, w := range []*enginedb.DisplayWave{&a.Overview, a.Waveform} {
		if w != nil && (len(w.Bands) == 0 || len(w.Bands)%3 != 0 || len(w.Bands) > 6<<20 || !finite(w.SamplesPerBin) || w.SamplesPerBin <= 0) {
			return fmt.Errorf("display: invalid bands")
		}
	}
	for i, b := range a.Beats {
		if !finite(b.MS) || b.MS < 0 || b.MS > a.DurationMS || b.Number < -1000000 || b.Number > 1000000 || i > 0 && (b.MS <= a.Beats[i-1].MS || b.Number <= a.Beats[i-1].Number) {
			return fmt.Errorf("display: invalid beat")
		}
	}
	for _, c := range a.Cues {
		if !finite(c.MS) || c.MS < 0 || c.MS > a.DurationMS || c.I < 0 || c.I > 8 || len(c.N) > 255 || !displayHexColor(c.C) {
			return fmt.Errorf("display: invalid cue")
		}
	}
	for _, l := range a.Loops {
		if !finite(l.StartMS) || !finite(l.EndMS) || l.StartMS < 0 || l.EndMS <= l.StartMS || l.EndMS > a.DurationMS || len(l.N) > 255 || !displayHexColor(l.C) {
			return fmt.Errorf("display: invalid loop")
		}
	}
	if len(a.Artwork) != 0 && !validDisplayArtwork(a.Artwork) {
		return fmt.Errorf("display: invalid artwork")
	}
	a.Detail = a.Waveform != nil
	return nil
}

func displayHexColor(c string) bool {
	if (len(c) != 7 && len(c) != 9) || c[0] != '#' {
		return false
	}
	_, err := hex.DecodeString(c[1:])
	return err == nil
}

func validDisplayArtwork(b []byte) bool {
	if len(b) > 1<<20 {
		return false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	return err == nil && cfg.Width > 0 && cfg.Height > 0 && cfg.Width <= 2048 && cfg.Height <= 2048
}

func downsampleDisplayWave(w enginedb.DisplayWave, limit int) enginedb.DisplayWave {
	n := len(w.Bands) / 3
	if n <= limit {
		return w
	}
	b := make([]byte, limit*3)
	for x := 0; x < limit; x++ {
		for i := x * n / limit; i < (x+1)*n/limit; i++ {
			for c := 0; c < 3; c++ {
				b[x*3+c] = max(b[x*3+c], w.Bands[i*3+c])
			}
		}
	}
	return enginedb.DisplayWave{Bands: b, SamplesPerBin: w.SamplesPerBin * float64(n) / float64(limit)}
}

// Peak-preserving downsampling and a fixed, three-band palette. This runs once
// at fetch completion, never in a position poll or QML animation frame.
func renderShadowWave(w enginedb.DisplayWave, width, height int) ([]byte, error) {
	if width < 1 || width > 2048 || height < 2 || height > 128 || len(w.Bands) == 0 || len(w.Bands)%3 != 0 {
		return nil, fmt.Errorf("display: invalid render size/bands")
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	palette := [3]color.NRGBA{{0x4a, 0x78, 0xff, 255}, {0xf0, 0xb4, 0, 255}, {255, 255, 255, 255}}
	n := len(w.Bands) / 3
	for x := 0; x < width; x++ {
		lo, hi := x*n/width, max(x*n/width+1, (x+1)*n/width)
		var peak [3]int
		for i := lo; i < hi && i < n; i++ {
			for band := range peak {
				peak[band] = max(peak[band], int(w.Bands[i*3+band]))
			}
		}
		// Layer the cumulative band amplitudes: lows outside, highs at center.
		for band := range peak {
			amp := 0
			for j := band; j < 3; j++ {
				amp += peak[j]
			}
			h := min(height/2, (amp*(height/2)+254)/255)
			for y := height/2 - h; y < height/2+h; y++ {
				img.SetNRGBA(x, y, palette[band])
			}
		}
	}
	var b bytes.Buffer
	err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&b, img)
	return b.Bytes(), err
}

func (s *Server) peerDisplayGet(w http.ResponseWriter, r *http.Request) {
	path := s.ownDeckFile(r)
	if path == "" {
		http.Error(w, "not loaded", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 750*time.Millisecond)
	defer cancel()
	a, err := s.sourceDisplayAsset(ctx, path, r.URL.Query().Get("key"))
	if err != nil {
		http.Error(w, "display data not ready", http.StatusNotFound)
		return
	}
	// Recheck after the read: a track switch must not label old data as new.
	if s.ownDeckFile(r) != path {
		http.Error(w, "track changed", http.StatusConflict)
		return
	}
	copyAsset := *a
	a = &copyAsset
	a.Version, a.Key = 1, r.URL.Query().Get("key")
	if validateDisplayAsset(a, a.Key) != nil {
		http.Error(w, "unsupported display data", http.StatusNotFound)
		return
	}
	b, err := json.Marshal(a)
	if err != nil || len(b) > shadowDisplayLimit {
		http.Error(w, "display data too large", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func libraryDisplay(ctx context.Context, f, dataRoot, mediaRoot string) (*shadowDisplayAsset, error) {
	lib := engineLibrary(f, dataRoot, mediaRoot)
	rel, err := filepath.Rel(lib, f)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"m.db", "stm.db"} {
		var a *shadowDisplayAsset
		a, err = dbDisplay(ctx, filepath.Join(lib, "Database2", name), filepath.ToSlash(rel))
		if err == nil {
			return a, nil
		}
	}
	return nil, err
}

func bridgeDisplayWave(ctx context.Context, file string, id int64, rate float64) *enginedb.DisplayWave {
	// rb-bridge's side database is separate from Engine's library and read-only.
	file = filepath.Join(filepath.Dir(file), ".rb-bridge-state.db")
	if _, err := os.Stat(file); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: file}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(100)")
	if err != nil {
		return nil
	}
	defer db.Close()
	var b []byte
	if db.QueryRowContext(ctx, `SELECT substr(sourceJSON,1,12582913) FROM BridgeAnalysis WHERE trackId=?`, id).Scan(&b) != nil || len(b) > shadowDisplayLimit {
		return nil
	}
	var source struct{ Analysis *rekordbox.Analysis }
	if json.Unmarshal(b, &source) != nil {
		return nil
	}
	return enginedb.DisplayWaveFromRekordbox(source.Analysis, rate)
}

func dbDisplay(ctx context.Context, file, rel string) (*shadowDisplayAsset, error) {
	if _, err := os.Stat(file); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: file}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(100)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	a := &shadowDisplayAsset{}
	var id int64
	var key sql.NullInt64
	var track, overview, beats, cues, loops []byte
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT t.id, substr(COALESCE(t.title,''),1,4097), substr(COALESCE(t.artist,''),1,4097), COALESCE(t.bpmAnalyzed,t.bpm,0), t.key,
		substr(p.trackData,1,4097), substr(p.overviewWaveFormData,1,8388609), substr(p.beatData,1,8388609), substr(p.quickCues,1,65537), substr(p.loops,1,65537)
		FROM Track t JOIN PerformanceData p ON p.trackId=t.id WHERE t.path=? AND length(p.trackData)>4 AND length(p.overviewWaveFormData)>4 AND length(p.beatData)>4`, rel).
		Scan(&id, &a.Title, &a.Artist, &a.BPM, &key, &track, &overview, &beats, &cues, &loops)
	if err != nil {
		return nil, err
	}
	if a.DisplayPerformance, err = enginedb.ParseDisplayPerformance(track, overview, beats, cues, loops); err != nil {
		return nil, err
	}
	// Engine's Track.key, not trackData's differing analysis enum.
	if key.Valid && key.Int64 >= 0 && key.Int64 < 24 {
		a.KeyName = []string{"8B", "8A", "9B", "9A", "10B", "10A", "11B", "11A", "12B", "12A", "1B", "1A", "2B", "2A", "3B", "3A", "4B", "4A", "5B", "5A", "6B", "6A", "7B", "7A"}[key.Int64]
	}
	// Native Engine 5.3 stores detail only in process memory. A bridge's saved
	// Rekordbox analysis is the only persisted detailed waveform consumed here.
	a.Waveform = bridgeDisplayWave(ctx, file, id, a.SampleRate)
	a.Detail = a.Waveform != nil
	if a.Detail {
		a.DetailSource = "rekordbox"
	}
	var hash, cover []byte
	if tx.QueryRowContext(ctx, `SELECT a.hash, substr(a.albumArt,1,1048577) FROM AlbumArt a JOIN Track t ON t.albumArtId=a.id WHERE t.id=?`, id).Scan(&hash, &cover) == nil {
		if validDisplayArtwork(cover) {
			a.Artwork = cover
		} else if len(hash) == 20 {
			art := filepath.Join(filepath.Dir(filepath.Dir(file)), "Artwork", base64.RawURLEncoding.EncodeToString(hash)+".jpg")
			if f, e := os.Open(art); e == nil {
				b, re := io.ReadAll(io.LimitReader(f, (1<<20)+1))
				f.Close()
				if re == nil && validDisplayArtwork(b) {
					a.Artwork = b
				}
			}
		}
	}
	return a, nil
}

func (s *Server) currentShadowDisplay(r *http.Request) (*shadowDisplayEntry, deckState) {
	deck, _ := strconv.Atoi(r.URL.Query().Get("deck"))
	key := r.URL.Query().Get("key")
	if deck < 1 || deck > 4 || key == "" {
		return nil, deckState{}
	}
	s.uiMu.Lock()
	v := s.loadUI()
	s.uiMu.Unlock()
	role := s.activeRole(v)
	if role == "" || v.ShadowLegacyEngine || strings.Contains(role, strconv.Itoa(deck)) {
		return nil, deckState{}
	}
	s.shadow.mu.Lock()
	peers := make(map[int]peerDeck, len(s.shadow.peerDecks))
	for n, p := range s.shadow.peerDecks {
		peers[n] = p
	}
	s.shadow.mu.Unlock()
	p, ok := placeDisplayPeers(peers, role, time.Now())[deck]
	if !ok || displayTrackKey(p) != key {
		return nil, deckState{}
	}
	d := p.deckState
	d.Deck, d.Key = deck, key
	return s.ensureShadowDisplay(p, time.Now()), d
}

func (s *Server) shadowDisplayGet(w http.ResponseWriter, r *http.Request) {
	e, d := s.currentShadowDisplay(r)
	if e == nil {
		http.Error(w, "display data not ready", http.StatusNotFound)
		return
	}
	a := e.asset
	q := "?deck=" + strconv.Itoa(d.Deck) + "&key=" + url.QueryEscape(d.Key)
	cues := a.Cues
	if d.Cues != nil { // live unsaved hot-cue edits belong to the source too
		cues = []enginedb.DisplayCue{}
		for _, c := range d.Cues {
			ms := c.P * 1000 / a.SampleRate
			if c.I >= 1 && c.I <= 8 && ms >= 0 && ms <= a.DurationMS && !math.IsNaN(ms) && !math.IsInf(ms, 0) && len(c.N) <= 255 {
				col := c.C
				if !displayHexColor(col) {
					col = "#ffffff"
				}
				cues = append(cues, enginedb.DisplayCue{I: c.I, MS: ms, N: c.N, C: col})
			}
		}
	}
	if d.CuePos != nil {
		ms := *d.CuePos * 1000 / a.SampleRate
		if ms >= 0 && ms <= a.DurationMS {
			cues = append(slices.Clone(cues), enginedb.DisplayCue{I: 0, MS: ms, N: "CUE", C: "#ffffff"})
		}
	}
	art := ""
	if len(a.Artwork) != 0 {
		art = "http://127.0.0.1:8080/v1/sc5000/shadow/artwork" + q
	}
	var tiles []string
	width := 2048
	if a.Waveform != nil {
		width = len(a.Waveform.Bands) / 3
		for i := range e.tiles {
			tiles = append(tiles, "http://127.0.0.1:8080/v1/sc5000/shadow/waveform.png"+q+"&tile="+strconv.Itoa(i))
		}
	}
	title, artist := a.Title, a.Artist
	if title == "" {
		title = d.Title
	}
	if artist == "" {
		artist = d.Artist
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, map[string]any{"key": d.Key, "title": title, "artist": artist, "bpm": a.BPM, "keyName": a.KeyName,
		"sampleRate": a.SampleRate, "durationMs": a.DurationMS, "beats": a.Beats, "cues": cues, "loops": a.Loops, "detail": a.Detail, "detailPending": a.DetailPending, "detailSource": a.DetailSource,
		"waveformUrl":   "http://127.0.0.1:8080/v1/sc5000/shadow/waveform.png" + q,
		"waveformTiles": tiles, "waveformWidth": width, "tileWidth": 2048,
		"overviewUrl": "http://127.0.0.1:8080/v1/sc5000/shadow/waveform.png" + q + "&overview=1", "artworkUrl": art})
}

func (s *Server) shadowWaveformGet(w http.ResponseWriter, r *http.Request) {
	e, _ := s.currentShadowDisplay(r)
	if e == nil {
		http.Error(w, "display data not ready", http.StatusNotFound)
		return
	}
	b := e.wave
	if r.URL.Query().Has("tile") {
		n, err := strconv.Atoi(r.URL.Query().Get("tile"))
		if err != nil || n < 0 || n >= len(e.tiles) || e.asset.Waveform == nil {
			http.NotFound(w, r)
			return
		}
		e.tileMu.Lock()
		if e.tiles[n] == nil {
			bands := e.asset.Waveform.Bands
			lo, hi := n*2048*3, min(len(bands), (n+1)*2048*3)
			e.tiles[n], err = renderShadowWave(enginedb.DisplayWave{Bands: bands[lo:hi]}, (hi-lo)/3, 128)
		}
		b = e.tiles[n]
		e.tileMu.Unlock()
		if err != nil {
			http.Error(w, "render failed", 500)
			return
		}
	} else if r.URL.Query().Get("overview") == "1" {
		b = e.overview
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func (s *Server) shadowArtworkGet(w http.ResponseWriter, r *http.Request) {
	e, _ := s.currentShadowDisplay(r)
	if e == nil || len(e.asset.Artwork) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(e.asset.Artwork))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(e.asset.Artwork)
}
