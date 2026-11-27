package webui

import (
	"context"
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

// omniRows is what lets the answer leave at the cut without waiting for any extra: rows
// filed before take are in the answer, in their group's own order; rows filed after it
// are dropped; and rows of a source that is no longer live when the answer is put
// together (a server switched off after it answered) are left out.
func TestOmniRowsTakeEndsTheCollection(t *testing.T) {
	ref := func(title string) []trackRef { return []trackRef{{file: title}} }
	titles := func(rows [][]trackRef) (out []string) {
		for _, refs := range rows {
			for _, ref := range refs {
				out = append(out, ref.file)
			}
		}
		return out
	}
	on := true
	var found omniRows
	found.put(omniServers, 2, 3, ref("server three"), func() bool { return true })
	found.put(omniServers, 0, 3, ref("server one"), func() bool { return on })
	found.put(omniDrives, 0, 1, ref("drive"), nil)
	on = false // server one is switched off after it answered
	got := found.take()
	found.put(omniServers, 1, 3, ref("late server"), nil)
	found.put(omniDjay, 0, 1, ref("late djay"), nil)
	if d, s := titles(got[omniDrives]), titles(got[omniServers]); len(d) != 1 || d[0] != "drive" || got[omniDjay] != nil || len(s) != 1 || s[0] != "server three" {
		t.Errorf("rows taken: drives %v, djay %v, servers %v; want the drive's and the third server's", d, got[omniDjay], s)
	}
	if again := found.take(); again[omniDjay] != nil || len(titles(again[omniServers])) != 1 {
		t.Errorf("rows filed after the answer left were kept: %v", again)
	}
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
