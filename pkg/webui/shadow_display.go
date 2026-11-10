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
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
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
	styled         map[shadowWaveStyle]map[int][]byte
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

// localPeerTrack covers a peer that plays a track from this deck's drive over
// Engine's network: it has no file to analyse and sends no Key, but the same
// file and its analysis are here. Display data only, as for any peer track.
func (s *Server) localPeerTrack(p peerDeck, now time.Time) peerDeck {
	if p.Key != "" || p.Net == "" {
		return p
	}
	sh := s.shadow
	sh.mu.Lock()
	c, ok := sh.peerLocal[p.Net]
	sh.mu.Unlock()
	if !ok || (c.file == "" && now.Sub(c.at) > 2*time.Second) {
		c = peerLocalFile{at: now}
		if f := resolveNetPathFile(p.Net, sh.dataRoot, sh.mediaRoot, false); f != "" {
			if st, err := os.Stat(f); err == nil {
				c.file, c.key = f, fmt.Sprintf("local-%x", fnvSum(fmt.Sprintf("%s %d %d", f, st.Size(), st.ModTime().UnixNano())))
			}
		}
		sh.mu.Lock()
		if sh.peerLocal == nil || len(sh.peerLocal) >= 8 {
			sh.peerLocal = map[string]peerLocalFile{}
		}
		sh.peerLocal[p.Net] = c
		sh.mu.Unlock()
	}
	p.Key, p.local = c.key, c.file
	return p
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
	// Expiry matches a goodbye: free shadow slots on every model.
	fresh := make(map[int]peerDeck, len(peers))
	for n, p := range peers {
		if now.Sub(p.at) <= 15*time.Second {
			fresh[n] = p
		}
	}
	peers = fresh
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
func (s *Server) shadowComposeDisplay(role string, now time.Time, lines ...[]int) []deckState {
	pfx := imagePrefix(deviceCompatible())
	isConsole := pfx != "" && !playerBoards[pfx]
	var consoleLines []int
	if isConsole && len(lines) > 0 && len(lines[0]) > 0 {
		consoleLines = slices.Clone(lines[0])
		slices.Sort(consoleLines)
	}

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
	var candidates []int
	for deck := 1; deck <= 4; deck++ {
		if strings.Contains(role, strconv.Itoa(deck)) {
			continue
		}
		candidates = append(candidates, deck)
	}
	for _, deck := range candidates {
		p, ok := peers[deck]
		if !ok {
			free = append(free, deck)
			continue
		}
		p = s.localPeerTrack(p, now)
		d := p.deckState
		d.Deck, d.Key = deck, displayTrackKey(p)
		d.LineIn = false
		d.Stale = p.Stale || now.Sub(p.at) >= 2*time.Second
		if d.Playing {
			if !p.Stale {
				d.PositionMS, d.Rate = p.carried(min(2*time.Second, max(0, now.Sub(p.at))))
			}
		}
		d.Path, d.Ext, d.Net = "", "", ""
		d.DisplayOnly = true
		d.At = now.UnixMilli()
		if p.Key != "" {
			e := s.ensureShadowDisplay(p, now)
			d.Loading = e == nil
			d.DisplayDetail = e != nil && e.asset != nil && !e.asset.DetailPending
		}
		out = append(out, d)
	}
	if len(consoleLines) > 0 {
		free = nil
		for _, deck := range consoleLines {
			if _, occupied := peers[deck]; !occupied {
				free = append(free, deck)
			}
		}
	}
	out = append(out, s.shadowDisplayCDJs(free, len(consoleLines) > 0, now)...)
	slices.SortFunc(out, func(a, b deckState) int { return a.Deck - b.Deck })
	return out
}

// shadowDisplayCDJs puts the CDJs on the free decks for the display renderer
// (lineIn: the decks are the console's Line In channels). Only cached
// rekordbox analysis is used here, never shadowFetchCDJ/audio.
func (s *Server) shadowDisplayCDJs(free []int, lineIn bool, now time.Time) []deckState {
	sh := s.shadow
	devices := s.mergedDevices()
	var lineFree []int
	if lineIn {
		lineFree = free
	}
	var out []deckState
	for deck, dev := range cdjDecks(devices, free, now, lineFree) {
		d := deckState{Deck: deck, DisplayOnly: true, LineIn: lineIn, Title: dev.Title, Artist: dev.Artist, MetadataOnly: true, Playing: dev.Playing, PositionMS: float64(dev.PlayPositionMS), Pitch: dev.Pitch, Master: dev.Master, Sync: dev.Sync, BPM: dev.BPM, At: now.UnixMilli()}
		if dev.TrackID != 0 {
			host := net.ParseIP(dev.Address)
			if dev.SourcePlayer != dev.Number {
				host = sourceHost(prolink.ExportStatus{}, devices, dev.SourcePlayer)
			}
			version := ""
			if s.loadUI().ProlinkShare && s.prolinkExport != nil {
				version = s.prolinkExport.DisplayMediaCache().MediaIdentity(dev, host)
			}
			d.Key = fmt.Sprintf("cdj:%s:%d:%d:%d:%s", host, dev.SourcePlayer, dev.SourceSlot, dev.TrackID, version)
			d.MetadataOnly = s.ensureCDJDisplay(dev, host, d.Key, version, now) == nil
		}
		at := dev.LastSeen
		sh.mu.Lock()
		if p, ok := sh.precise[dev.Number]; ok && now.Sub(p.at) < time.Second {
			d.PositionMS, at = p.ms, p.at
		}
		sh.mu.Unlock()
		if d.Playing {
			d.PositionMS += float64(max(int64(0), now.Sub(at).Milliseconds())) * (1 + d.Pitch/100)
		}
		out = append(out, d)
	}
	return out
}

func (s *Server) ensureCDJDisplay(dev prolink.Device, host net.IP, key, version string, now time.Time) *shadowDisplayEntry {
	if version == "" || !s.loadUI().ProlinkShare || s.prolinkExport == nil {
		return nil
	}
	media := s.prolinkExport.DisplayMediaCache()
	if version != media.MediaIdentity(dev, host) {
		return nil
	}
	c := &s.shadow.display
	c.mu.Lock()
	slot := c.claim(key, now)
	if slot == nil {
		c.mu.Unlock()
		return nil
	}
	slot.at = now
	if slot.asset != nil {
		c.mu.Unlock()
		return slot
	}
	if slot.busy || now.Before(slot.retry) {
		c.mu.Unlock()
		return nil
	}
	slot.busy = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.entries[key] == slot {
			slot.busy = false
			slot.retry = now.Add(3 * time.Second)
		}
		c.mu.Unlock()
	}()
	a, title, artist := media.CachedAnalysisVersion(dev, host, version)
	if a == nil {
		return nil
	}
	wave, _, duration := prolink.ExtractANLZ(a)
	if len(wave) == 0 {
		return nil
	}
	const rate = 44100.0 // display timebase, no audio is opened
	detail := enginedb.DisplayWaveFromRekordbox(a, rate)
	if detail != nil {
		span := float64(len(detail.Bands)/3) * detail.SamplesPerBin / rate * 1000
		if span+100 < float64(duration) {
			detail = nil // truncated ANLZ detail must not stretch over the song
		} else {
			duration = uint32(span)
			w := downsampleDisplayWave(*detail, 65536)
			detail = &w
		}
	}
	if detail == nil && a.DurationMS > 0 {
		duration = a.DurationMS
	}
	if duration == 0 {
		duration = dev.DurationMS
	}
	if duration == 0 {
		return nil
	}
	asset := &shadowDisplayAsset{Version: 1, Key: key, Title: title, Artist: artist, BPM: dev.BPM,
		DisplayPerformance: enginedb.DisplayPerformance{SampleRate: rate, DurationMS: float64(duration), Waveform: detail,
			Overview: enginedb.DisplayWave{SamplesPerBin: rate * float64(duration) / 1000 / float64(len(wave))},
			Beats:    []enginedb.DisplayBeat{}, Cues: []enginedb.DisplayCue{}, Loops: []enginedb.DisplayLoop{}}}
	for _, b := range wave {
		asset.Overview.Bands = append(asset.Overview.Bands, b[:]...)
	}
	// ANLZ beat numbers are beats within the bar; the display expects a
	// monotonically increasing zero-based index and the complete track grid.
	for i, b := range a.Beats {
		if float64(b.TimeMS) > asset.DurationMS {
			continue
		} // PDB duration is rounded to whole seconds
		asset.Beats = append(asset.Beats, enginedb.DisplayBeat{MS: float64(b.TimeMS), Number: int64(i) + int64(a.Beats[0].Number) - 1})
	}
	filterCDJDisplayCues(asset, a.Cues, float64(dev.PlayPositionMS))
	if detail != nil {
		asset.DetailSource = "rekordbox"
	}
	if validateDisplayAsset(asset, key) != nil {
		return nil
	}
	overview, err := renderShadowWave(asset.Overview, 1024, 64)
	if err != nil {
		return nil
	}
	w, err := renderShadowWave(asset.Overview, 2048, 128)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[key] != slot || version != media.MediaIdentity(dev, host) || !s.loadUI().ProlinkShare {
		return nil
	}
	e := &shadowDisplayEntry{asset: asset, overview: overview, wave: w, at: now}
	if detail != nil {
		e.tiles = make([][]byte, (len(detail.Bands)/3+2047)/2048)
	}
	c.entries[key] = e
	return e
}

// Keep valid Hot Cues A-H first; fill remaining marker slots with the nearest
// Memory Cues. I=9 is reserved for memory, separate from Engine's main CUE (0).
func filterCDJDisplayCues(asset *shadowDisplayAsset, cues []rekordbox.Cue, positionMS float64) {
	var hot, memory []rekordbox.Cue
	for _, cue := range cues {
		if cue.HotCue > 8 || float64(cue.TimeMS) > asset.DurationMS || len(cue.Name) > 255 {
			continue
		}
		if cue.Loop && (cue.EndMS <= cue.TimeMS || float64(cue.EndMS) > asset.DurationMS) {
			continue
		}
		if cue.HotCue == 0 {
			memory = append(memory, cue)
		} else {
			hot = append(hot, cue)
		}
	}
	slices.SortStableFunc(hot, func(a, b rekordbox.Cue) int { return int(a.HotCue) - int(b.HotCue) })
	slices.SortStableFunc(memory, func(a, b rekordbox.Cue) int {
		da, db := math.Abs(float64(a.TimeMS)-positionMS), math.Abs(float64(b.TimeMS)-positionMS)
		if da < db {
			return -1
		}
		if da > db {
			return 1
		}
		return 0
	})
	seen := map[uint32]bool{}
	for _, cue := range append(hot, memory...) {
		col := fmt.Sprintf("#%02x%02x%02x", cue.Color[1], cue.Color[2], cue.Color[3])
		if cue.Color == [4]byte{} {
			col = "#ffffff"
		}
		if cue.Loop && len(asset.Loops) < 8 {
			asset.Loops = append(asset.Loops, enginedb.DisplayLoop{StartMS: float64(cue.TimeMS), EndMS: float64(cue.EndMS), N: cue.Name, C: col})
		}
		if len(asset.Cues) >= 9 || cue.HotCue > 0 && seen[cue.HotCue] {
			continue
		}
		i := int(cue.HotCue)
		if i == 0 {
			i = 9
		} else {
			seen[cue.HotCue] = true
		}
		asset.Cues = append(asset.Cues, enginedb.DisplayCue{I: i, MS: float64(cue.TimeMS), N: cue.Name, C: col})
	}
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
	var a *shadowDisplayAsset
	var wave, overview []byte
	var err error
	if p.local != "" {
		var own *shadowDisplayAsset
		if own, err = s.sourceDisplayAsset(ctx, p.local, p.Key); err == nil {
			copyAsset := *own // the source cache's asset stays immutable
			a = &copyAsset
		}
	} else {
		u := fmt.Sprintf("http://%s/peer/decks/%d/display?key=%s", net.JoinHostPort(p.ip, strconv.Itoa(port)), p.Deck, url.QueryEscape(p.Key))
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		// A peer must not redirect this read-only request to arbitrary URLs.
		client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		var resp *http.Response
		if resp, err = client.Do(req); err == nil {
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
			}
		}
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
	c := &sh.display
	c.mu.Lock()
	changed := false
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
		changed = displayReadinessChanged(slot, e)
	}
	c.mu.Unlock()
	sh.dbg.event("display-fetch", map[string]any{"deck": p.Deck, "ok": err == nil, "changed": changed})
	// A retry while the source still analyses (every 2 s) changes nothing the
	// display shows: do not wake the long polls (each wake is a JSON parse on
	// Engine's GUI thread). Readiness also reaches the QML through the
	// X-Shadow-State digest (loading).
	if changed {
		sh.mu.Lock()
		sh.bump()
		sh.mu.Unlock()
	}
}

// displayReadinessChanged: e is the first usable asset, or its detail arrived.
func displayReadinessChanged(old, e *shadowDisplayEntry) bool {
	if old == nil || (old.asset == nil) != (e.asset == nil) {
		return true
	}
	if e.asset == nil {
		return false
	}
	return old.asset.DetailPending != e.asset.DetailPending || old.asset.Detail != e.asset.Detail || (old.asset.Waveform == nil) != (e.asset.Waveform == nil)
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
		if w != nil {
			span := float64(len(w.Bands)/3) * w.SamplesPerBin / a.SampleRate * 1000
			if !finite(span) || math.Abs(span-a.DurationMS) > 100 {
				return fmt.Errorf("display: waveform span differs from track")
			}
		}
	}
	for i, b := range a.Beats {
		if !finite(b.MS) || b.MS < 0 || b.MS > a.DurationMS || b.Number < -1000000 || b.Number > 1000000 || i > 0 && (b.MS <= a.Beats[i-1].MS || b.Number <= a.Beats[i-1].Number) {
			return fmt.Errorf("display: invalid beat")
		}
	}
	for _, c := range a.Cues {
		if !finite(c.MS) || c.MS < 0 || c.MS > a.DurationMS || c.I < 0 || c.I > 9 || len(c.N) > 255 || !displayHexColor(c.C) {
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
		// int64: x*n passes 2^31 on the 32-bit players once a track has more bins than the limit.
		for i, end := int(int64(x)*int64(n)/int64(limit)), int(int64(x+1)*int64(n)/int64(limit)); i < end; i++ {
			for c := 0; c < 3; c++ {
				b[x*3+c] = max(b[x*3+c], w.Bands[i*3+c])
			}
		}
	}
	return enginedb.DisplayWave{Bands: b, SamplesPerBin: w.SamplesPerBin * float64(n) / float64(limit)}
}

// Peak-preserving downsampling. The default is retained for older clients.
// Local UI clients supply their stock palette; rendering never runs on a frame tick.
func renderShadowWave(w enginedb.DisplayWave, width, height int) ([]byte, error) {
	return renderShadowWaveStyle(w, width, height, defaultShadowWaveStyle())
}

type shadowWaveStyle struct {
	Colors                                         [3]color.NRGBA
	RGB, Half                                      bool // Half: from the bottom edge (stock overview, Praline)
	Dim                                            float64
	Saturation, BrightAmp, BrightGain, BrightFloor float64
}

func defaultShadowWaveStyle() shadowWaveStyle {
	return shadowWaveStyle{Dim: 1, Colors: [3]color.NRGBA{{0x4a, 0x78, 0xff, 255}, {0xf0, 0xb4, 0, 255}, {255, 255, 255, 255}}}
}

// Values are the local WaveFormItem's properties after EngineX/theme overrides.
func parseShadowWaveStyle(q url.Values) (shadowWaveStyle, error) {
	style := defaultShadowWaveStyle()
	style.RGB = q.Get("rgb") == "1"
	style.Half = q.Get("half") == "1"
	if raw := q.Get("dim"); raw != "" {
		dim, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(dim) || math.IsInf(dim, 0) || dim < 0 || dim > 1 {
			return style, fmt.Errorf("invalid dim")
		}
		style.Dim = dim
	}
	for i, name := range []string{"low", "mid", "high"} {
		b, err := hex.DecodeString(q.Get(name))
		if err != nil || (len(b) != 3 && len(b) != 4) {
			return style, fmt.Errorf("invalid %s color", name)
		}
		if len(b) == 4 {
			style.Colors[i] = color.NRGBA{b[1], b[2], b[3], b[0]} // QColor's #AARRGGBB
		} else {
			style.Colors[i] = color.NRGBA{b[0], b[1], b[2], 255}
		}
	}
	for _, field := range []struct {
		name  string
		value *float64
	}{
		{"saturation", &style.Saturation}, {"brightAmp", &style.BrightAmp}, {"brightGain", &style.BrightGain}, {"brightFloor", &style.BrightFloor},
	} {
		v, err := strconv.ParseFloat(q.Get(field.name), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 16 {
			return style, fmt.Errorf("invalid %s", field.name)
		}
		*field.value = v
	}
	return style, nil
}

func renderShadowWaveStyle(w enginedb.DisplayWave, width, height int, style shadowWaveStyle) ([]byte, error) {
	if width < 1 || width > 2048 || height < 2 || height > 128 || len(w.Bands) == 0 || len(w.Bands)%3 != 0 {
		return nil, fmt.Errorf("display: invalid render size/bands")
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	palette := style.Colors
	n := len(w.Bands) / 3
	full := height / 2
	if style.Half {
		full = height
	}
	column := func(x, h int, c color.NRGBA) {
		lo, hi := height/2-h, height/2+h
		if style.Half {
			lo, hi = height-h, height
		}
		for y := lo; y < hi; y++ {
			img.SetNRGBA(x, y, c)
		}
	}
	for x := 0; x < width; x++ {
		lo, hi := x*n/width, max(x*n/width+1, (x+1)*n/width)
		var peak [3]int
		for i := lo; i < hi && i < n; i++ {
			for band := range peak {
				peak[band] = max(peak[band], int(w.Bands[i*3+band]))
			}
		}
		if style.RGB {
			// WaveFormFBORGB.ps: mix L/M/H, normalize, saturate, then apply
			// amplitude brightness. Preserve mixed red/orange/green/cyan columns.
			amp := float64(peak[0]+peak[1]+peak[2]) / 255
			var c [3]float64
			for band, p := range peak {
				c[0] += float64(p) / 255 * float64(palette[band].R) / 255
				c[1] += float64(p) / 255 * float64(palette[band].G) / 255
				c[2] += float64(p) / 255 * float64(palette[band].B) / 255
			}
			sum := c[0] + c[1] + c[2] + 1e-5
			for j := range c {
				c[j] /= sum
			}
			gray := c[0]*0.299 + c[1]*0.587 + c[2]*0.114
			brightness := math.Pow(amp, style.BrightAmp)*style.BrightGain + style.BrightFloor
			for j := range c {
				c[j] = min(1, max(0, (gray+(c[j]-gray)*style.Saturation)*brightness))
			}
			pixel := color.NRGBA{uint8(math.Round(c[0] * 255 * style.Dim)), uint8(math.Round(c[1] * 255 * style.Dim)), uint8(math.Round(c[2] * 255 * style.Dim)), 255}
			// Both native RGB shaders apply 0.9 to the amplitude. Overview's
			// waveformColor() applies it before calcDistances(), not in calcDist().
			column(x, min(full, int(math.Ceil(amp*0.9*float64(full)))), pixel)
			continue
		}
		if style.Half {
			// Stock Praline overview at yScale 1, fitted on a 3-band screenshot
			// (median 0.3 %, p95 1.3 % of the view height): low at the bottom, mid
			// on it, high on top; each layer's share of the height depends on its
			// own band only. Edge rows are blended by coverage.
			share := [3]float64{
				0.300 * float64(peak[0]) / 255,
				0.0030 + 0.1837*math.Pow(min(1, float64(peak[1])/113.5), 1.72),
				0.0129 + 0.1220*math.Pow(min(1, float64(peak[2])/51), 2.85),
			}
			from := 0.0
			for band, s := range share {
				to := min(float64(height), from+s*float64(height))
				for r := int(from); float64(r) < to; r++ {
					c, p := palette[band], img.NRGBAAt(x, height-1-r)
					cover := (min(to, float64(r+1)) - max(from, float64(r))) * float64(c.A) / 255
					if cover <= 0 {
						continue
					}
					a := float64(p.A) / 255
					mix := func(old, new uint8) uint8 {
						return uint8(math.Round((float64(old)*a + float64(new)*cover) / (a + cover)))
					}
					img.SetNRGBA(x, height-1-r, color.NRGBA{mix(p.R, c.R), mix(p.G, c.G), mix(p.B, c.B), uint8(math.Round(min(1, a+cover) * 255))})
				}
				from = to
			}
			continue
		}
		// Layer the cumulative band amplitudes: lows outside, highs at center.
		for band := range peak {
			amp := 0
			for j := band; j < 3; j++ {
				amp += peak[j]
			}
			column(x, min(full, (amp*full+254)/255), palette[band])
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

func bridgeDisplayWave(ctx context.Context, file, rel string, id int64, rate, durationMS float64) *enginedb.DisplayWave {
	// rb-bridge's side database is separate from Engine's library and read-only.
	// Its track IDs belong only to m.db; stm.db can have unrelated matching IDs.
	if filepath.Base(file) != "m.db" {
		return nil
	}
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
	if db.QueryRowContext(ctx, `SELECT substr(a.sourceJSON,1,12582913) FROM BridgeAnalysis a JOIN BridgeState s ON s.trackId=a.trackId WHERE a.trackId=? AND s.path=?`, id, rel).Scan(&b) != nil || len(b) > shadowDisplayLimit {
		return nil
	}
	var source struct{ Analysis *rekordbox.Analysis }
	if json.Unmarshal(b, &source) != nil {
		return nil
	}
	w := enginedb.DisplayWaveFromRekordbox(source.Analysis, rate)
	if w == nil || math.Abs(float64(len(w.Bands)/3)*w.SamplesPerBin/rate*1000-durationMS) > 100 {
		return nil
	}
	return w
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
	a.Waveform = bridgeDisplayWave(ctx, file, rel, id, a.SampleRate, a.DurationMS)
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
	s.shadow.mu.Lock()
	lines := slices.Clone(s.shadow.lines)
	s.shadow.mu.Unlock()
	if pfx := imagePrefix(deviceCompatible()); pfx == "" || playerBoards[pfx] {
		lines = nil
	}
	if role == "" || v.ShadowMode == "engine" || (strings.Contains(role, strconv.Itoa(deck)) && !slices.Contains(lines, deck)) {
		return nil, deckState{}
	}
	s.shadow.mu.Lock()
	peers := make(map[int]peerDeck, len(s.shadow.peerDecks))
	for n, p := range s.shadow.peerDecks {
		peers[n] = p
	}
	s.shadow.mu.Unlock()
	p, ok := placeDisplayPeers(peers, role, time.Now())[deck]
	if !ok || v.ShadowMode != "cdj" { // by source: Engine draws the peers, only CDJ decks have assets
		// Compose and HTTP use the same placement, including peer priority and
		// exact current track keys. An old CDJ URL cannot read a different song.
		for _, d := range s.shadowCompose(role, time.Now(), lines) {
			if d.Deck == deck && d.Key == key && d.DisplayOnly && !d.MetadataOnly {
				c := &s.shadow.display
				c.mu.Lock()
				e := c.entries[key]
				c.mu.Unlock()
				return e, d
			}
		}
		return nil, deckState{}
	}
	p = s.localPeerTrack(p, time.Now())
	if displayTrackKey(p) != key {
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
	waveDuration := a.DurationMS
	if a.Waveform != nil {
		width = len(a.Waveform.Bands) / 3
		waveDuration = float64(width) * a.Waveform.SamplesPerBin / a.SampleRate * 1000
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
		"waveformTiles": tiles, "waveformWidth": width, "waveformDurationMs": waveDuration, "tileWidth": 2048,
		"overviewUrl": "http://127.0.0.1:8080/v1/sc5000/shadow/waveform.png" + q + "&overview=1", "artworkUrl": art})
}

func (s *Server) shadowWaveformGet(w http.ResponseWriter, r *http.Request) {
	e, _ := s.currentShadowDisplay(r)
	if e == nil {
		http.Error(w, "display data not ready", http.StatusNotFound)
		return
	}
	if r.URL.Query().Has("low") {
		style, err := parseShadowWaveStyle(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		index, width, height := -1, 2048, 128
		wave := e.asset.Overview
		if e.asset.Waveform != nil {
			wave = *e.asset.Waveform
		}
		if r.URL.Query().Get("overview") == "1" {
			index, width, height, wave = -2, 1024, 64, e.asset.Overview
		}
		if r.URL.Query().Has("tile") {
			index, err = strconv.Atoi(r.URL.Query().Get("tile"))
			if err != nil || index < 0 || index >= len(e.tiles) || e.asset.Waveform == nil {
				http.NotFound(w, r)
				return
			}
			bands := e.asset.Waveform.Bands
			lo, hi := index*2048*3, min(len(bands), (index+1)*2048*3)
			wave, width, height = enginedb.DisplayWave{Bands: bands[lo:hi]}, (hi-lo)/3, 128
		}
		e.tileMu.Lock()
		if e.styled == nil {
			e.styled = make(map[shadowWaveStyle]map[int][]byte)
		}
		images := e.styled[style]
		if images == nil {
			// Bound theme/brightness variants even for arbitrary local requests.
			if len(e.styled) >= 8 {
				clear(e.styled)
			}
			images = make(map[int][]byte)
			e.styled[style] = images
		}
		b := images[index]
		if b == nil {
			b, err = renderShadowWaveStyle(wave, width, height, style)
			if err == nil {
				images[index] = b
			}
		}
		e.tileMu.Unlock()
		if err != nil {
			http.Error(w, "render failed", 500)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
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
