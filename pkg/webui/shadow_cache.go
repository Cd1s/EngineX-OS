package webui

// Reserve each receiver entry's entire lazy image budget up front. Tile/theme
// requests can then run without acquiring the live-state or cache mutex.
const (
	shadowCacheEntries = 16
	shadowCacheBytes   = 32 << 20
	shadowTileBytes    = 1 << 20
)

func shadowAssetBytes(a *shadowDisplayAsset) int {
	if a == nil {
		return 0
	}
	n := 1024 + len(a.Key) + len(a.Title) + len(a.Artist) + len(a.KeyName) + len(a.DetailSource)
	n += cap(a.Artwork) + cap(a.Overview.Bands) + cap(a.Beats)*16 + cap(a.Cues)*64 + cap(a.Loops)*64
	if a.Waveform != nil {
		n += cap(a.Waveform.Bands)
	}
	for _, cue := range a.Cues {
		n += len(cue.N) + len(cue.C)
	}
	for _, loop := range a.Loops {
		n += len(loop.N) + len(loop.C)
	}
	return n
}

// Caller holds c.mu. Published entries stay immutable and remain safe for
// in-flight readers after eviction. Never evict a fetch which owns a slot.
func (c *shadowDisplayCache) trim(keep string) bool {
	for {
		total, oldest := 0, ""
		for key, e := range c.entries {
			total += shadowAssetBytes(e.asset) + cap(e.wave) + cap(e.overview) + shadowTileBytes + cap(e.tiles)*24 + 512 + len(key)
			if key != keep && !e.busy && (oldest == "" || e.at.Before(c.entries[oldest].at)) {
				oldest = key
			}
		}
		if len(c.entries) <= shadowCacheEntries && total <= shadowCacheBytes {
			return true
		}
		if oldest == "" {
			return false
		}
		delete(c.entries, oldest)
	}
}

func (c *shadowSourceCache) trim(keep string) bool {
	for {
		total, oldest := 0, ""
		for key, e := range c.entries {
			total += shadowAssetBytes(e.asset) + 512 + len(key) + len(e.path) + len(e.req)
			if key != keep && !e.queued && !e.working && (oldest == "" || e.at.Before(c.entries[oldest].at)) {
				oldest = key
			}
		}
		if len(c.entries) <= shadowCacheEntries && total <= shadowCacheBytes {
			return true
		}
		if oldest == "" {
			return false
		}
		delete(c.entries, oldest)
	}
}

// Caller holds tileMu. PNGs that exceed the per-entry budget are served once
// without retention; this covers plain tiles and every theme variant together.
func (e *shadowDisplayEntry) retainImage(b []byte) bool {
	if e.imageBytes+cap(b) > shadowTileBytes {
		return false
	}
	e.imageBytes += cap(b)
	return true
}
