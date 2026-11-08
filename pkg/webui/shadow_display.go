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
	Artwork []byte `json:"artwork,omitempty"`
	Detail  bool   `json:"detail"`
}

type shadowDisplayEntry struct {
	asset          *shadowDisplayAsset
	wave, overview []byte
	busy           bool
	at             time.Time
	retry          time.Time
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
	if len(c.entries) >= 8 {
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
	out := []deckState{}
	free := []int{}
	for deck := 1; deck <= 4; deck++ {
		if strings.Contains(role, strconv.Itoa(deck)) {
			continue
		}
		p, ok := peers[deck]
		if !ok || now.Sub(p.at) >= 2*time.Second {
			free = append(free, deck)
			continue
		}
		d := p.deckState
		if d.Playing {
			d.PositionMS += float64(now.Sub(p.at).Milliseconds()) * (1 + d.Pitch/100)
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
	if e.asset != nil {
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
		e := &shadowDisplayEntry{at: time.Now(), retry: time.Now().Add(time.Second)}
		if err == nil {
			e.asset, e.wave, e.overview = a, wave, overview
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
	if len(c) != 7 || c[0] != '#' {
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
	a, err := libraryDisplay(ctx, path, s.shadow.dataRoot, s.shadow.mediaRoot)
	if err != nil {
		http.Error(w, "display data not ready", http.StatusNotFound)
		return
	}
	// Recheck after the read: a track switch must not label old data as new.
	if s.ownDeckFile(r) != path {
		http.Error(w, "track changed", http.StatusConflict)
		return
	}
	a.Version, a.Key = 1, r.URL.Query().Get("key")
	if validateDisplayAsset(a, a.Key) != nil {
		http.Error(w, "unsupported display data", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, a)
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
	err = db.QueryRowContext(ctx, `SELECT t.id, COALESCE(t.title,''), COALESCE(t.artist,''), COALESCE(t.bpmAnalyzed,t.bpm,0), t.key,
		substr(p.trackData,1,4097), substr(p.overviewWaveFormData,1,8388609), substr(p.beatData,1,8388609), substr(p.quickCues,1,65537), substr(p.loops,1,65537)
		FROM Track t JOIN PerformanceData p ON p.trackId=t.id WHERE t.path=? AND t.isAnalyzed=1`, rel).
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
	// Do not assume this optional column exists: measured 5.x schemas omit it.
	var detail []byte
	if db.QueryRowContext(ctx, `SELECT substr(highResolutionWaveFormData,1,8388609) FROM PerformanceData WHERE trackId=?`, id).Scan(&detail) == nil && len(detail) > 0 {
		if d, e := enginedb.ParseDisplayWave(detail); e == nil {
			a.Waveform = &d
			a.Detail = true
		}
	}
	var hash, cover []byte
	if db.QueryRowContext(ctx, `SELECT a.hash, substr(a.albumArt,1,1048577) FROM AlbumArt a JOIN Track t ON t.albumArtId=a.id WHERE t.id=?`, id).Scan(&hash, &cover) == nil {
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
	p, ok := s.shadow.peerDecks[deck]
	s.shadow.mu.Unlock()
	if !ok || p.Key != key || time.Since(p.at) >= 2*time.Second {
		return nil, deckState{}
	}
	return s.ensureShadowDisplay(p, time.Now()), p.deckState
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
			if c.I >= 1 && c.I <= 8 && ms >= 0 && ms <= a.DurationMS && !math.IsNaN(ms) && !math.IsInf(ms, 0) && len(c.N) <= 255 && displayHexColor(c.C) {
				cues = append(cues, enginedb.DisplayCue{I: c.I, MS: ms, N: c.N, C: c.C})
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
	w.Header().Set("Cache-Control", "no-store")
	respond(w, map[string]any{"key": a.Key, "title": a.Title, "artist": a.Artist, "bpm": a.BPM, "keyName": a.KeyName,
		"sampleRate": a.SampleRate, "durationMs": a.DurationMS, "beats": a.Beats, "cues": cues, "loops": a.Loops, "detail": a.Detail,
		"waveformUrl": "http://127.0.0.1:8080/v1/sc5000/shadow/waveform.png" + q,
		"overviewUrl": "http://127.0.0.1:8080/v1/sc5000/shadow/waveform.png" + q + "&overview=1", "artworkUrl": art})
}

func (s *Server) shadowWaveformGet(w http.ResponseWriter, r *http.Request) {
	e, _ := s.currentShadowDisplay(r)
	if e == nil {
		http.Error(w, "display data not ready", http.StatusNotFound)
		return
	}
	b := e.wave
	if r.URL.Query().Get("overview") == "1" {
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
