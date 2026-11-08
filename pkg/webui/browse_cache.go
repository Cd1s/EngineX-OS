package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

const (
	browseTTL         = 2 * time.Minute
	browseStale       = 24 * time.Hour
	browseRetry       = 5 * time.Second
	browseEntryBytes  = 8 << 20
	browseMemoryBytes = 32 << 20
	browseDiskBytes   = 64 << 20
	browseEntries     = 128
)

// Only metadata crosses this boundary. Providers, cookies, tokens, audio URLs,
// and download state are never serialized. URL fields stay in memory only.
// This also preserves ThumbURL and Preview, whose Track JSON tags omit them.
type browseTrack struct {
	providers.Track
	Thumb      string `json:"thumb,omitempty"`
	PreviewURL string `json:"previewUrl,omitempty"`
}
type browseData struct {
	Lists  []providers.Playlist `json:"lists,omitempty"`
	Tracks []browseTrack        `json:"tracks,omitempty"`
}

var errBrowseTooLarge = errors.New("playlist too large")

type browseEntry struct {
	data            []byte
	at, used, retry time.Time
	disk            bool
	err             error
}
type browseFlight struct {
	done chan struct{}
	data []byte
	err  error
}
type browseCache struct {
	diskMu  sync.Mutex
	mu      sync.Mutex
	dir     string
	entries map[string]*browseEntry
	flights map[string]*browseFlight
	bytes   int
	now     func() time.Time
}

func newBrowseCache(state string) *browseCache {
	dir := ""
	if state != "" {
		dir = filepath.Join(state, "cache", "browse-v1")
		if strings.HasPrefix(filepath.Clean(state), "/data/") {
			dir = "/data/enginex/cache/browse-v1"
		}
	}
	c := &browseCache{dir: dir, entries: map[string]*browseEntry{}, flights: map[string]*browseFlight{}, now: time.Now}
	c.pruneDisk()
	return c
}

func browseKey(provider, session, operation, id string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + session + "\x00" + operation + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

// get returns a private copy: enrichment, sorting and OmniSource's title prefix
// must never mutate the cached value. Expired successes return immediately while
// one independent, bounded refresh runs; cold callers share the same flight.
func (c *browseCache) get(ctx context.Context, key string, load func(context.Context) (browseData, error)) (browseData, error) {
	c.mu.Lock()
	now := c.now()
	e := c.entries[key]
	if e == nil {
		e = c.readDisk(key, now)
		if e != nil {
			c.insert(key, e)
		}
	}
	if e != nil {
		e.used = now
		if e.data != nil && now.Sub(e.at) < browseStale {
			if (e.disk || now.Sub(e.at) >= browseTTL) && !now.Before(e.retry) {
				c.start(key, load)
			}
			data := e.data
			c.mu.Unlock()
			return decodeBrowse(data)
		}
		if e.err != nil && now.Before(e.retry) {
			err := e.err
			c.mu.Unlock()
			return browseData{}, err
		}
	}
	f := c.start(key, load)
	c.mu.Unlock()
	if f == nil {
		return browseData{}, errors.New("browse cache busy")
	}
	select {
	case <-ctx.Done():
		return browseData{}, ctx.Err()
	case <-f.done:
		if f.err != nil {
			return browseData{}, f.err
		}
		return decodeBrowse(f.data)
	}
}

// start is called under mu. A cancelled browser request cannot cancel another
// browser's shared fetch, or the refresh of a stale page.
func (c *browseCache) start(key string, load func(context.Context) (browseData, error)) *browseFlight {
	if f := c.flights[key]; f != nil {
		return f
	}
	if len(c.flights) >= 32 {
		return nil
	}
	f := &browseFlight{done: make(chan struct{})}
	c.flights[key] = f
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		var data []byte
		var err error
		func() {
			defer func() {
				if recover() != nil {
					err = errors.New("browse provider failed")
				}
			}()
			var v browseData
			v, err = load(ctx)
			if err == nil {
				data, err = json.Marshal(v)
			}
		}()
		c.mu.Lock()
		now := c.now()
		persist := err == nil && len(data) <= browseEntryBytes
		oversized := err == nil && !persist
		var status providers.StatusError
		revoked := errors.As(err, &status) && (status == 401 || status == 403)
		if persist {
			c.insert(key, &browseEntry{data: data, at: now, used: now})
		} else if oversized {
			// Never keep returning an older small snapshot when a successful
			// refresh grew beyond the per-entry budget. Return it uncached.
			if e := c.entries[key]; e != nil {
				c.bytes -= len(e.data)
				delete(c.entries, key)
			}
		} else if err != nil {
			if revoked {
				c.insert(key, &browseEntry{err: err, used: now, retry: now.Add(browseRetry)})
			} else if e := c.entries[key]; e != nil {
				e.err, e.retry = err, now.Add(browseRetry)
			} else {
				c.insert(key, &browseEntry{err: err, used: now, retry: now.Add(browseRetry)})
			}
		}
		c.mu.Unlock()
		// Slow flash writes cannot hold up unrelated warm reads. Serialize disk
		// maintenance independently; atomic rename makes concurrent reads safe.
		if persist || revoked || oversized {
			c.diskMu.Lock()
			if revoked || oversized {
				if c.dir != "" {
					_ = os.Remove(filepath.Join(c.dir, key+".json"))
				}
			} else {
				c.writeDisk(key, data, now)
			}
			c.diskMu.Unlock()
		}
		c.mu.Lock()
		f.data, f.err = data, err
		delete(c.flights, key)
		close(f.done)
		c.mu.Unlock()
	}()
	return f
}

func decodeBrowse(data []byte) (browseData, error) {
	var v browseData
	err := json.Unmarshal(data, &v)
	return v, err
}

func (c *browseCache) insert(key string, e *browseEntry) {
	if old := c.entries[key]; old != nil {
		c.bytes -= len(old.data)
	}
	c.entries[key] = e
	c.bytes += len(e.data)
	for c.bytes > browseMemoryBytes || len(c.entries) > browseEntries {
		oldest := ""
		for k, v := range c.entries {
			if oldest == "" || v.used.Before(c.entries[oldest].used) {
				oldest = k
			}
		}
		c.bytes -= len(c.entries[oldest].data)
		delete(c.entries, oldest)
	}
}

type browseDiskEntry struct {
	At   time.Time  `json:"at"`
	Data browseData `json:"data"`
}

func (c *browseCache) readDisk(key string, now time.Time) *browseEntry {
	if c.dir == "" {
		return nil
	}
	path := filepath.Join(c.dir, key+".json")
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > browseEntryBytes {
		return nil
	}
	b, err := os.ReadFile(path)
	var v browseDiskEntry
	if err != nil || json.Unmarshal(b, &v) != nil || now.Sub(v.At) >= browseStale || v.At.After(now) {
		return nil
	}
	data, err := json.Marshal(v.Data)
	if err != nil {
		return nil
	}
	return &browseEntry{data: data, at: v.At, used: now, disk: true}
}

func (c *browseCache) writeDisk(key string, data []byte, now time.Time) {
	if c.dir == "" {
		return
	}
	v, err := decodeBrowse(data)
	if err != nil {
		return
	}
	// Even a signed artwork/preview URL can carry credentials. Persist no URL;
	// public metadata survives eviction, and a refresh restores artwork in memory.
	for i := range v.Tracks {
		v.Tracks[i].CoverURL, v.Tracks[i].Thumb, v.Tracks[i].PreviewURL = "", "", ""
	}
	b, err := json.Marshal(browseDiskEntry{now, v})
	if err != nil || len(b) > browseEntryBytes || os.MkdirAll(c.dir, 0o700) != nil {
		return
	}
	f, err := os.CreateTemp(c.dir, ".write-")
	if err != nil {
		return
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return
	}
	if os.Rename(name, filepath.Join(c.dir, key+".json")) != nil {
		return
	}
	c.pruneDisk()
}

func (c *browseCache) pruneDisk() {
	if c.dir == "" {
		return
	}
	files, _ := os.ReadDir(c.dir)
	type file struct {
		name string
		size int64
		at   time.Time
	}
	var all []file
	var size int64
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".write-") {
			_ = os.Remove(filepath.Join(c.dir, f.Name()))
			continue
		}
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		if st, err := f.Info(); err == nil {
			all = append(all, file{f.Name(), st.Size(), st.ModTime()})
			size += st.Size()
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for len(all) > browseEntries || size > browseDiskBytes {
		if os.Remove(filepath.Join(c.dir, all[0].name)) != nil {
			break
		}
		size -= all[0].size
		all = all[1:]
	}
}

func (g *tidalGateway) browseLists(ctx context.Context, v *Session) ([]providers.Playlist, error) {
	data, err := g.browse.get(ctx, browseKey(v.Provider, v.ID, "lists", ""), func(ctx context.Context) (browseData, error) {
		lists, err := v.p.Playlists(ctx)
		return browseData{Lists: lists}, err
	})
	return data.Lists, err
}

func (g *tidalGateway) browseTracks(ctx context.Context, provider, session string, p providers.Provider, id string) ([]providers.Track, error) {
	data, err := g.browse.get(ctx, browseKey(provider, session, "tracks", id), func(ctx context.Context) (browseData, error) {
		tracks, err := p.Tracks(ctx, id)
		if err != nil {
			return browseData{}, err
		}
		if len(tracks) > maxPlaylistTracks {
			return browseData{}, errBrowseTooLarge
		}
		out := make([]browseTrack, len(tracks))
		for i, t := range tracks {
			out[i] = browseTrack{t, t.ThumbURL, t.Preview}
		}
		return browseData{Tracks: out}, nil
	})
	tracks := make([]providers.Track, len(data.Tracks))
	for i, t := range data.Tracks {
		tracks[i] = t.Track
		tracks[i].ThumbURL, tracks[i].Preview = t.Thumb, t.PreviewURL
	}
	return tracks, err
}
