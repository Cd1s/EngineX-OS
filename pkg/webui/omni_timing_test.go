package webui

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
)

// The timing tests below run in a synctest bubble: time is the bubble's own clock, which
// moves only when every goroutine waits, so each duration is exact on any host.

// WebDAV round 5 (round-11 re-check, item 5): OmniSource's timing rule, to the
// millisecond. The selected source is never cut and nothing is waited for after it; the
// extras are waited for until they have all answered or the deadline has passed.
func TestOmniSourceTimingRule(t *testing.T) {
	const never, none = time.Duration(-1), time.Duration(-2)
	for _, c := range []struct {
		name               string
		selected, answered time.Duration // when each is done (none: no selected search)
		want               time.Duration
	}{
		{"everything answered early", 20 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond},
		{"a finished selected source, an extra taking 2.1 s", time.Millisecond, 2100 * time.Millisecond, serverReadyWait},
		{"a finished selected source, an extra that never answers", time.Millisecond, never, serverReadyWait},
		{"the selected source at its own budget, an extra that never answers", searchTimeout, never, searchTimeout},
		{"the selected source at 1.5 s, the extras at 2.1 s", 1500 * time.Millisecond, 2100 * time.Millisecond, 1500 * time.Millisecond},
		{"the selected source at 1.5 s, the extras at 1.2 s", 1500 * time.Millisecond, 1200 * time.Millisecond, 1500 * time.Millisecond},
		{"no selected search, the extras at 300 ms", none, 300 * time.Millisecond, 300 * time.Millisecond},
		{"no selected search, an extra that never answers", none, never, serverReadyWait},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				after := func(d time.Duration) <-chan struct{} {
					if d == none {
						return nil
					}
					done := make(chan struct{})
					if d != never {
						timer := time.AfterFunc(d, func() { close(done) })
						t.Cleanup(func() { timer.Stop() })
					}
					return done
				}
				start := time.Now()
				omniWait(serverReadyWait, after(c.selected), after(c.answered))
				if got := time.Since(start); got != c.want {
					t.Errorf("the answer goes out after %v, want %v", got, c.want)
				}
			})
		})
	}
}

// eachUntil is what puts the drives' listing, each drive and djay under that deadline:
// at the cut it returns what has answered, leaves the rest out and ends their context.
// Without a cut it is each: every input is waited for, within the 15 s budget.
func TestEachUntilLeavesOutWhatHasNotAnswered(t *testing.T) {
	const stuck = time.Duration(-1)
	work := func(ended *time.Duration, start time.Time, left chan struct{}) func(context.Context, time.Duration) ([]string, error) {
		return func(ctx context.Context, d time.Duration) ([]string, error) {
			if d == stuck {
				<-ctx.Done()
				*ended = time.Since(start)
				close(left)
				return nil, ctx.Err()
			}
			select {
			case <-time.After(d):
				return []string{d.String()}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	in := []time.Duration{2100 * time.Millisecond, 10 * time.Millisecond, stuck}
	t.Run("cut after 1 s", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cut := make(chan struct{})
			time.AfterFunc(time.Second, func() { close(cut) })
			var ended time.Duration
			left := make(chan struct{})
			start := time.Now()
			out := eachUntil(context.Background(), in, work(&ended, start, left), cut)
			if took := time.Since(start); took != time.Second {
				t.Errorf("returned after %v, want at the cut (1 s)", took)
			}
			if len(out) != 3 || out[0] != nil || !slices.Equal(out[1], []string{"10ms"}) || out[2] != nil {
				t.Errorf("results %v, want only the input that answered before the cut", out)
			}
			<-left // the work left out sees its context end
			if ended != time.Second {
				t.Errorf("the work left out had its context ended after %v, want at the cut (1 s)", ended)
			}
		})
	})
	t.Run("no cut", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var ended time.Duration
			start := time.Now()
			out := each(context.Background(), in, work(&ended, start, make(chan struct{})))
			if took := time.Since(start); took != 15*time.Second {
				t.Errorf("returned after %v, want once every input was done (the stuck one at its 15 s budget)", took)
			}
			if len(out) != 3 || !slices.Equal(out[0], []string{"2.1s"}) || !slices.Equal(out[1], []string{"10ms"}) || out[2] != nil {
				t.Errorf("results %v, want both inputs that answered", out)
			}
		})
	})
}

// omniSearch adds no wait of its own: with no drive, djay library or server to search,
// it returns at the moment the selected source's search is done, however long that took.
func TestOmniSearchReturnsWithTheSelectedSource(t *testing.T) {
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir(), AllowLossy: true}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: t.TempDir()}, jobs)
	t.Cleanup(s.Close)
	for _, after := range []time.Duration{time.Millisecond, 1500 * time.Millisecond, searchTimeout} {
		t.Run(after.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				selected := make(chan struct{})
				time.AfterFunc(after, func() { close(selected) })
				start := time.Now()
				items := s.tidal.omniSearch(context.Background(), "intro", selected)
				if took := time.Since(start); took != after || len(items) != 0 {
					t.Errorf("omniSearch returned %d rows after %v, want none, the moment the selected source was done (%v)", len(items), took, after)
				}
			})
		})
	}
}
