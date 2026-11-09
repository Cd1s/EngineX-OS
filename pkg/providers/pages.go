package providers

import (
	"context"
	"errors"
	"sync"
)

var errPageLimit = errors.New("provider: pagination limit")

// Two concurrent page/detail requests per platform, shared across accounts and
// simultaneous cache misses. Existing independent metadata probes are unchanged.
// Cursor-only APIs remain sequential; never guess their next cursor.
var (
	deezerPages       = make(chan struct{}, 2)
	spotifyPages      = make(chan struct{}, 2)
	neteasePages      = make(chan struct{}, 2)
	soundcloudPages   = make(chan struct{}, 2)
	beatportPageSlots = make(chan struct{}, 2)
)

type listPage[T any] struct {
	items []T
	more  bool
	total int
}

type pageResult[T any] struct {
	value T
	err   error
}

// pageBatch uses a fixed-size wave, rather than a goroutine per page. Results
// and errors are consumed in upstream order, independent of completion order.
// At most one speculative request can accompany a failing page. Do not retry
// 429s (or any other errors) here, or publish the later pages past a failure.
func pageBatch[T any](ctx context.Context, slots chan struct{}, start, end int, fetch func(int) (T, error)) []pageResult[T] {
	out := make([]pageResult[T], end-start)
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				out[i].err = ctx.Err()
				return
			}
			if err := ctx.Err(); err != nil {
				out[i].err = err
				return
			}
			out[i].value, out[i].err = fetch(start + i)
		}(i)
	}
	wg.Wait()
	return out
}

// collectPages keeps the existing full-list contract (global sorting happens
// in the gateway). Only a total supplied by the upstream enables parallelism.
// Missing totals retain the sequential next-page behavior. maxPages preserves
// each provider's existing cap, including when an upstream supplies a bad total.
func collectPages[T any](ctx context.Context, slots chan struct{}, size, maxPages int, fetch func(int) (listPage[T], error)) ([]T, error) {
	var out []T
	knownEnd := 0
	for start := 0; start < maxPages; {
		end := start + 1
		if start < knownEnd {
			end = min(start+2, knownEnd)
		}
		results := pageBatch(ctx, slots, start, min(end, maxPages), fetch)
		for _, r := range results {
			if r.err != nil {
				return out, r.err
			}
			out = append(out, r.value.items...)
			if !r.value.more {
				return out, nil
			}
		}
		if start == 0 && results[0].value.total > size {
			knownEnd = min(maxPages, (results[0].value.total-1)/size+1)
		}
		start += len(results)
		// A stale total must not truncate a list that still has a next page.
		// Continue sequentially beyond the known range, following the old cap.
	}
	return out, errPageLimit
}

// collectBatches is for APIs that return the complete ID list first (NetEase
// and SoundCloud web playlists). The caller decides whether a failed batch
// discards everything or preserves the successfully fetched prefix.
func collectBatches[T any](ctx context.Context, slots chan struct{}, count int, fetch func(int) ([]T, error)) ([]T, error) {
	var out []T
	for start := 0; start < count; start += 2 {
		for _, r := range pageBatch(ctx, slots, start, min(start+2, count), fetch) {
			if r.err != nil {
				return out, r.err
			}
			out = append(out, r.value...)
		}
	}
	return out, nil
}
