package webui

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

const (
	searchTimeout = 4 * time.Second
	searchTTL     = time.Minute
)

type searchCache struct {
	*browseCache
	workerMu sync.Mutex
	workers  map[string]int
	active   int
}

func newSearchCache() *searchCache {
	// Search is volatile: keep the existing provider/session/operation/query key
	// scheme and memory budgets, but never persist or serve day-old search hits.
	c := newBrowseCache("")
	c.ttl, c.stale = searchTTL, searchTTL
	return &searchCache{browseCache: c, workers: make(map[string]int)}
}

// A timed-out worker retains its slot until it really exits. New queries cannot
// accumulate an unlimited number of workers behind one stuck provider/lock.
// Each kind/session has two slots, leaving capacity for other sessions; the
// global budget also bounds workers belonging to sessions that have expired.
func (c *searchCache) call(ctx context.Context, provider, session, kind string, load func(context.Context) (browseData, error)) (browseData, error) {
	key := browseKey(provider, session, kind, "")
	c.workerMu.Lock()
	if c.active >= 32 || c.workers[key] >= 2 {
		c.workerMu.Unlock()
		return browseData{}, errors.New("search provider busy")
	}
	c.workers[key]++
	c.active++
	c.workerMu.Unlock()
	return searchCall(ctx, provider, kind, func(ctx context.Context) (browseData, error) {
		defer func() {
			c.workerMu.Lock()
			c.workers[key]--
			c.active--
			if c.workers[key] == 0 {
				delete(c.workers, key)
			}
			c.workerMu.Unlock()
		}()
		return load(ctx)
	})
}

// searchCall bounds each independent search, including providers waiting on a
// lock or otherwise ignoring cancellation. Workers only write to their buffered
// channel, so a late result cannot race with the response or enter the cache.
func searchCall(ctx context.Context, provider, kind string, load func(context.Context) (browseData, error)) (data browseData, err error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	started := time.Now()
	defer func() {
		status := "ok"
		if err != nil {
			status = "failed"
			if errors.Is(err, context.DeadlineExceeded) {
				status = "timeout"
			} else if errors.Is(err, context.Canceled) {
				status = "cancelled"
			}
		}
		// No query, account identifier, URLs or provider errors (which may carry
		// credentials) belong in these per-platform timing diagnostics.
		log.Printf("gateway search: provider=%s kind=%s elapsed=%s status=%s", provider, kind, time.Since(started).Round(time.Millisecond), status)
	}()
	type result struct {
		data browseData
		err  error
	}
	done := make(chan result, 1)
	go func() {
		r := result{}
		defer func() {
			if recover() != nil {
				r = result{err: errors.New("search provider failed")}
			}
			done <- r
		}()
		r.data, r.err = load(ctx)
	}()
	select {
	case r := <-done:
		return r.data, r.err
	case <-ctx.Done():
		return browseData{}, ctx.Err()
	}
}

func (g *tidalGateway) searchTracks(ctx context.Context, provider, session string, p trackSearcher, query string) ([]providers.Track, error) {
	data, err := g.searches.get(ctx, browseKey(provider, session, "search-tracks", query), func(ctx context.Context) (browseData, error) {
		return g.searches.call(ctx, provider, session, "tracks", func(ctx context.Context) (browseData, error) {
			tracks, err := p.Search(ctx, query)
			out := make([]browseTrack, len(tracks))
			for i, t := range tracks {
				out[i] = browseTrack{t, t.ThumbURL, t.Preview}
			}
			return browseData{Tracks: out}, err
		})
	})
	tracks := make([]providers.Track, len(data.Tracks))
	for i, t := range data.Tracks {
		tracks[i] = t.Track
		tracks[i].ThumbURL, tracks[i].Preview = t.Thumb, t.PreviewURL
	}
	return tracks, err
}

func (g *tidalGateway) searchLists(ctx context.Context, provider, session string, p playlistSearcher, query string) ([]providers.Playlist, error) {
	data, err := g.searches.get(ctx, browseKey(provider, session, "search-playlists", query), func(ctx context.Context) (browseData, error) {
		return g.searches.call(ctx, provider, session, "playlists", func(ctx context.Context) (browseData, error) {
			lists, err := p.SearchPlaylists(ctx, query)
			return browseData{Lists: lists}, err
		})
	})
	return data.Lists, err
}
