package webui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func cdjDisplayHost(dev prolink.Device, devices []prolink.Device) net.IP {
	if dev.SourceSlot != 2 && dev.SourceSlot != 3 || dev.TrackID == 0 {
		return nil
	}
	// rekordbox Link, CD, streaming and non-rekordbox tracks have no exported ANLZ.
	if dev.TrackType != 1 {
		return nil
	}
	for _, src := range devices {
		if src.Number == dev.SourcePlayer && !strings.HasPrefix(src.Name, "EngineX") && time.Since(src.LastSeen) < 5*time.Second {
			return net.ParseIP(src.Address)
		}
	}
	return nil
}

func cdjDisplayAsset(t rekordbox.Track, a *rekordbox.Analysis, key string) (*shadowDisplayAsset, error) {
	p, err := enginedb.EncodePerformance(t, a)
	if err != nil {
		return nil, err
	}
	if !p.Complete {
		return nil, fmt.Errorf("display: incomplete CDJ analysis")
	}
	d, err := enginedb.ParseDisplayPerformance(p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops)
	if err != nil {
		return nil, err
	}
	asset := &shadowDisplayAsset{Version: 1, Key: key, Title: t.Title, Artist: t.Artist, BPM: t.BPM, KeyName: t.Key, DisplayPerformance: d}
	asset.Waveform = enginedb.DisplayWaveFromRekordbox(a, d.SampleRate)
	if asset.Waveform != nil {
		// PDB duration is whole seconds; detailed ANLZ retains the exact 150 Hz
		// timeline. Use that span for drawing without stretching the detail or
		// changing the source beat/cue positions. Reject larger inconsistencies.
		span := float64(len(asset.Waveform.Bands)/3) * asset.Waveform.SamplesPerBin / d.SampleRate * 1000
		if math.Abs(span-d.DurationMS) < 1000 {
			asset.DurationMS = span
			asset.Overview.SamplesPerBin = span * d.SampleRate / 1000 / float64(len(asset.Overview.Bands)/3)
		}
		w := downsampleDisplayWave(*asset.Waveform, 65536)
		asset.Waveform = &w
		asset.Detail, asset.DetailSource = true, "rekordbox"
	}
	if err = validateDisplayAsset(asset, key); err != nil {
		return nil, err
	}
	return asset, nil
}

func (s *Server) ensureCDJDisplay(dev prolink.Device, host net.IP, key string, now time.Time) *shadowDisplayEntry {
	c := &s.shadow.display
	c.mu.Lock()
	e := c.claim(key, now)
	if e == nil {
		c.mu.Unlock()
		return nil
	}
	e.at = now
	if !e.busy && !now.Before(e.retry) {
		e.busy = true
		go s.fetchCDJDisplay(dev, host, key, e)
	}
	var ready *shadowDisplayEntry
	if e.asset != nil {
		ready = e
	}
	c.mu.Unlock()
	return ready
}

func (s *Server) fetchCDJDisplay(dev prolink.Device, host net.IP, key string, slot *shadowDisplayEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var reader prolink.AnalysisReader = &prolink.NFSClient{IP: host}
	if s.shadow.cdjReader != nil {
		reader = s.shadow.cdjReader(host)
	}
	dir := filepath.Join(filepath.Dir(s.shadowDir()), "shadow-display-cdj")
	t, analysis, err := s.shadow.cdjAnalysis.Read(ctx, reader, host.String(), dev.SourceSlot, dev.TrackID, dir)
	var asset *shadowDisplayAsset
	var wave, overview []byte
	var revision string
	if err == nil {
		asset, err = cdjDisplayAsset(t, analysis, key)
	}
	if err == nil {
		var b []byte
		b, err = json.Marshal(asset)
		if err == nil {
			hash := sha256.Sum256(b)
			revision = fmt.Sprintf("%x", hash[:16])
		}
	}
	if err == nil {
		overview, err = renderShadowWave(asset.Overview, 1024, 64)
	}
	if err == nil {
		w := asset.Overview
		if asset.Waveform != nil {
			w = *asset.Waveform
		}
		wave, err = renderShadowWave(w, 2048, 128)
	}
	c := &s.shadow.display
	c.mu.Lock()
	if c.entries[key] == slot {
		e := &shadowDisplayEntry{at: time.Now(), retry: time.Now().Add(5 * time.Second)}
		if err == nil {
			e.asset, e.wave, e.overview = asset, wave, overview
			e.revision = revision
			e.retry = time.Now().Add(30 * time.Second) // re-stat PDB even when TrackID is unchanged
			if asset.Waveform != nil {
				e.tiles = make([][]byte, (len(asset.Waveform.Bands)/3+2047)/2048)
			}
		}
		// Failed revalidation removes old assets (e.g. an exchanged USB drive).
		c.entries[key] = e
	}
	c.mu.Unlock()
	s.shadow.dbg.event("cdj-display-fetch", map[string]any{"deck": dev.Number, "ok": err == nil})
}
