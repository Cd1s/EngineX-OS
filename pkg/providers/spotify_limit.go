package providers

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Spotify's Web API answers 429 with a Retry-After. A short one is a burst that
// went over the rolling window: wait it out and go on, the listing succeeds. A
// long or repeated one, or a 429 that names no wait, is a limit that lasts: stop
// asking until it is over, so that the deck re-opening the source does not keep
// the limit alive (#227).
const (
	spotifyRetryWait  = 5 * time.Second  // the longest wait sat out inside one request
	spotifyRetries    = 2                // waits per request
	spotifyBlockFloor = 30 * time.Second // first pause after the retries ran out, doubled each time
	spotifyBlockCap   = 15 * time.Minute // then ask again, whatever Retry-After said
)

type spotifyLimit struct {
	mu      sync.Mutex
	until   time.Time // no Web API request before this
	strikes int       // requests in a row that ran out of retries

	// The clock and the wait; nil outside tests.
	clock func() time.Time
	sleep func(context.Context, time.Duration) error
}

func (l *spotifyLimit) now() time.Time {
	if l.clock != nil {
		return l.clock()
	}
	return time.Now()
}

func (l *spotifyLimit) pause(ctx context.Context, d time.Duration) error {
	if l.sleep != nil {
		return l.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// wait holds a request back while a short limit runs, and refuses it during a
// long one without asking Spotify.
func (l *spotifyLimit) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		d := l.until.Sub(l.now())
		l.mu.Unlock()
		if d <= 0 {
			return nil
		}
		if d > spotifyRetryWait {
			return &RateLimited{RetryAfter: d}
		}
		if err := l.pause(ctx, d); err != nil {
			return err
		}
	}
}

// block notes a 429 and returns how long no request will be made. gaveUp: this
// request will not try again.
func (l *spotifyLimit) block(wait time.Duration, gaveUp bool) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if gaveUp {
		wait = max(wait, spotifyBlockFloor<<min(l.strikes, 5))
		l.strikes++
	}
	now := l.now()
	if until := now.Add(min(wait, spotifyBlockCap)); until.After(l.until) {
		l.until = until
	}
	return l.until.Sub(now)
}

func (l *spotifyLimit) ok() {
	l.mu.Lock()
	l.strikes = 0
	l.mu.Unlock()
}

// get is one Web API request, retried after a short Retry-After. Parallel pages
// share the wait instead of each running into the limit.
func (s *Spotify) get(ctx context.Context, path string, out any) error {
	for attempt := 0; ; attempt++ {
		if err := s.limit.wait(ctx); err != nil {
			return err
		}
		err := s.getOnce(ctx, path, out)
		var limited *RateLimited
		if !errors.As(err, &limited) {
			if err == nil {
				s.limit.ok()
			}
			return err
		}
		wait := limited.RetryAfter
		// A login the protocol session can serve (nativeLibraryFallback) does not
		// wait: its listing comes from there at once.
		if wait <= 0 || wait > spotifyRetryWait || attempt >= spotifyRetries || s.HasStoredCredentials() {
			return &RateLimited{RetryAfter: s.limit.block(wait, true)}
		}
		s.limit.block(wait, false)
	}
}
