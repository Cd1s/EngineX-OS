package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// slowSearch is a streaming login whose search answers only once released.
type slowSearch struct {
	browseTestProvider
	hold chan struct{}
}

func (p *slowSearch) Search(ctx context.Context, query string) ([]providers.Track, error) {
	select {
	case <-p.hold:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []providers.Track{{ID: "sp-slow", Title: query + " slow", Provider: "spotify"}}, nil
}

// blackHoleSMB accepts connections and never answers: an SMB negotiation with it stays
// open until the client gives up or closes its end. open counts the connections the
// client has not closed.
type blackHoleSMB struct {
	l     net.Listener
	mu    sync.Mutex
	conns []net.Conn
	open  atomic.Int32
}

func newBlackHoleSMB(t *testing.T) *blackHoleSMB {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &blackHoleSMB{l: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.conns = append(b.conns, c)
			b.mu.Unlock()
			b.open.Add(1)
			go func() {
				io.Copy(io.Discard, c) // until the client closes its end
				b.open.Add(-1)
			}()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, c := range b.conns {
			c.Close()
		}
	})
	return b
}

func (b *blackHoleSMB) url() string { return "smb://" + b.l.Addr().String() + "/music" }

// waitUntil polls ok until it holds or d has passed.
func waitUntil(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// WebDAV round 11b (PM correction 20:50 to item 5): the selected source is what the DJ
// searched, so OmniSource's deadline never cuts it: the first answer waits for it, as
// without OmniSource. OmniSource's extras add no time: the answer goes out at the later
// of "selected source done" and the deadline, and an extra still running then is left
// out. On 12cb4faa (round 4) a selected source slower than the deadline was cut: a
// login answering after 1.5 s was missing from the first answer.
//
// WebDAV round 5 (round-11 re-check, item 5): no bound here depends on the host's
// speed. What each case checks is what the code controls: which hits are in the answer,
// that it does not come before the selected source is done, and that it does not wait
// for a stuck extra (late). The exact times are TestOmniSourceTimingRule's, on a clock
// that does not depend on the host.
func TestServersOmniSourceWaitsForTheSelectedSourceOnly(t *testing.T) {
	// late: an answer that waits for a stuck extra comes at that extra's own budget
	// (15 s), on any host. One that comes before late did not wait for it.
	const late = 10 * time.Second
	timed := func(t *testing.T, r *webdavRig, query string) ([]string, time.Duration) {
		t.Helper()
		start := time.Now()
		got := webdavTitles(r.search(t, query))
		return got, time.Since(start)
	}
	held := func(t *testing.T) (chan struct{}, func()) {
		t.Helper()
		hold := make(chan struct{})
		release := sync.OnceFunc(func() { close(hold) })
		t.Cleanup(release)
		return hold, release
	}
	t.Run("selected login answering after 1.5 s", func(t *testing.T) {
		fast := newFakeDAV(t, testMP3(120), nil)
		raw, _ := json.Marshal(nasFile{WebDAV: []savedServer{{Name: "Fast", URL: fast.srv.URL + "/music/", Enabled: true}}})
		r := newWebDAVRig(t, string(raw), "sources=spotify\nscope=spotify\nomni_source=1\nwebdav=1\n")
		slow := &slowSearch{hold: make(chan struct{})}
		release := sync.OnceFunc(func() { close(slow.hold) })
		t.Cleanup(release)
		r.s.mu.Lock()
		r.s.sessions["sp-owner"] = &Session{ID: "sp-owner", Provider: "spotify", Ready: true, p: slow}
		r.s.mu.Unlock()
		armed := time.Now() // the login answers 1.5 s after this, never sooner
		time.AfterFunc(1500*time.Millisecond, release)
		got, _ := timed(t, r, "intro")
		if !slices.Equal(got, []string{"intro slow@", "intro@Fast"}) {
			t.Errorf("first answer with the selected login answering after 1.5 s: %v (the selected source cut by OmniSource's deadline?)", got)
		}
		if took := time.Since(armed); took < 1500*time.Millisecond || took >= late {
			t.Errorf("the answer came %v after the login was set to answer in 1.5 s, want with it", took)
		}
	})
	t.Run("slow extra server adds no time", func(t *testing.T) {
		hold, _ := held(t)
		stuck := newFakeDAV(t, testMP3(90), hold) // never answers within the search
		selHold, selRelease := held(t)
		sel, fast := newFakeDAV(t, testMP3(150), selHold), newFakeDAV(t, testMP3(120), nil)
		r := newWebDAVRig(t, "", "sources=\nscope=\nomni_source=1\nwebdav=1\n")
		r.change(t, serverChange{Action: "add", Name: "Fast", URL: fast.srv.URL + "/music/"}, 200)
		r.change(t, serverChange{Action: "add", Name: "Stuck", URL: stuck.srv.URL + "/music/"}, 200)
		views := r.change(t, serverChange{Action: "add", Name: "Selected", URL: sel.srv.URL + "/music/"}, 200)
		if code, _ := uiPost(t, r.gw.URL, "scope="+scopeOf(views, "Selected")); code != http.StatusOK {
			t.Fatalf("selecting the server: HTTP %d", code)
		}
		armed := time.Now() // the selected server answers 1.5 s after this, never sooner
		time.AfterFunc(1500*time.Millisecond, selRelease)
		got, _ := timed(t, r, "intro")
		if !slices.Equal(got, []string{"intro@Fast", "intro@Selected"}) {
			t.Errorf("first answer: %v, want the selected server's and the fast extra's hits, not the stuck one's", got)
		}
		if took := time.Since(armed); took < 1500*time.Millisecond || took >= late {
			t.Errorf("the answer came %v after the selected server was set to answer in 1.5 s, want with it: the stuck extra adds no time", took)
		}
	})
	t.Run("selected server out of its budget, fast extra", func(t *testing.T) {
		// The re-check's fixture: the slow server itself selected. Its search ends at
		// its own budget; the fast extra's hit comes with that answer, not after it plus
		// another wait for the stuck extra.
		selHold, _ := held(t)
		stuckHold, _ := held(t)
		sel, fast, stuck := newFakeDAV(t, testMP3(90), selHold), newFakeDAV(t, testMP3(120), nil), newFakeDAV(t, testMP3(60), stuckHold)
		r := newWebDAVRig(t, "", "sources=\nscope=\nomni_source=1\nwebdav=1\n")
		r.change(t, serverChange{Action: "add", Name: "Fast", URL: fast.srv.URL + "/music/"}, 200)
		r.change(t, serverChange{Action: "add", Name: "Stuck", URL: stuck.srv.URL + "/music/"}, 200)
		views := r.change(t, serverChange{Action: "add", Name: "Slow", URL: sel.srv.URL + "/music/"}, 200)
		if code, _ := uiPost(t, r.gw.URL, "scope="+scopeOf(views, "Slow")); code != http.StatusOK {
			t.Fatalf("selecting the slow server: HTTP %d", code)
		}
		got, took := timed(t, r, "intro")
		if !slices.Equal(got, []string{"intro@Fast"}) {
			t.Errorf("first answer: %v, want the fast extra's hit", got)
		}
		if took < searchTimeout-300*time.Millisecond || took >= late {
			t.Errorf("the answer came %v after the start, want with the selected server's own budget (%v), not with the stuck extra's", took, searchTimeout)
		}
	})
	t.Run("fast selected server, slow extra", func(t *testing.T) {
		// The deadline still bounds the extras.
		stuckHold, _ := held(t)
		sel, stuck := newFakeDAV(t, testMP3(150), nil), newFakeDAV(t, testMP3(60), stuckHold)
		r := newWebDAVRig(t, "", "sources=\nscope=\nomni_source=1\nwebdav=1\n")
		r.change(t, serverChange{Action: "add", Name: "Stuck", URL: stuck.srv.URL + "/music/"}, 200)
		views := r.change(t, serverChange{Action: "add", Name: "Selected", URL: sel.srv.URL + "/music/"}, 200)
		if code, _ := uiPost(t, r.gw.URL, "scope="+scopeOf(views, "Selected")); code != http.StatusOK {
			t.Fatalf("selecting the server: HTTP %d", code)
		}
		got, took := timed(t, r, "intro")
		if !slices.Equal(got, []string{"intro@Selected"}) {
			t.Errorf("first answer: %v, want the selected server's hit only", got)
		}
		if took < serverReadyWait || took >= late {
			t.Errorf("the answer came %v after the start, want at OmniSource's deadline (%v), not with the stuck extra's budget", took, serverReadyWait)
		}
	})
}

// With OmniSource off the selected source is searched alone, as before: its answer is
// waited for (within its budget), however slow.
func TestServersSelectedSourceAloneIsWaitedFor(t *testing.T) {
	hold := make(chan struct{})
	slow := newFakeDAV(t, testMP3(90), hold)
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	r := newWebDAVRig(t, "", "sources=\nscope=\nomni_source=0\nwebdav=1\n")
	views := r.change(t, serverChange{Action: "add", Name: "Slow", URL: slow.srv.URL + "/music/"}, 200)
	if code, _ := uiPost(t, r.gw.URL, "scope="+scopeOf(views, "Slow")); code != http.StatusOK {
		t.Fatalf("selecting the slow server: HTTP %d", code)
	}
	time.AfterFunc(1500*time.Millisecond, release)
	start := time.Now()
	got := webdavTitles(r.search(t, "intro"))
	if elapsed := time.Since(start); !slices.Equal(got, []string{"intro@Slow"}) || elapsed < time.Second {
		t.Errorf("the selected slow server alone: %v after %v, want its row once it answered", got, elapsed)
	}
}

// NAS switched off while the selected legacy NAS slot was searched: on 7a1424de that
// search was not tied to the slot's connection, so its SMB connection stayed open (to
// its 3 s connect budget) and the search ran on. Now switching NAS off closes it at
// once and the search answers, with OmniSource off and on.
func TestNASOffEndsTheSelectedSlotSearch(t *testing.T) {
	for _, omni := range []string{"0", "1"} {
		t.Run("omni_source="+omni, func(t *testing.T) {
			b := newBlackHoleSMB(t)
			nas := fmt.Sprintf(`{"protocol":"smb","url":%q,"username":"dj","password":"pw","enabled":true}`, b.url())
			r := newWebDAVRig(t, nas, "scope=nas\nomni_source="+omni+"\n")
			done := make(chan int, 1)
			started := time.Now()
			go func() {
				resp, err := http.Get(r.gw.URL + "/v1/search?query=intro&limit=50&offset=0")
				if err != nil {
					done <- -1
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				done <- resp.StatusCode
			}()
			if !waitUntil(3*time.Second, func() bool { return b.open.Load() > 0 }) {
				t.Fatal("the NAS slot's search never connected")
			}
			if code, _ := uiPost(t, r.gw.URL, "nas=0"); code != http.StatusOK {
				t.Fatalf("nas=0: HTTP %d", code)
			}
			off := time.Now()
			if !waitUntil(500*time.Millisecond, func() bool { return b.open.Load() == 0 }) {
				t.Errorf("the NAS slot's SMB connection was still open %v after NAS was switched off", time.Since(off).Round(time.Millisecond))
			}
			// The answer comes at once (OmniSource off), or at OmniSource's deadline if that
			// is later (round 11b: the extras wait until then; the slot's search is over).
			due := off
			if omni == "1" && started.Add(serverReadyWait).After(off) {
				due = started.Add(serverReadyWait)
			}
			select {
			case code := <-done:
				if late := time.Since(due); code != http.StatusOK || late > 500*time.Millisecond {
					t.Errorf("the search answered HTTP %d %v after NAS was switched off", code, time.Since(off).Round(time.Millisecond))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the search never answered")
			}
		})
	}
}

// The same for every request of an SMB server, not only a search: switching NAS off
// closes the connection of a browse of the NAS slot and of an SMB server's own entry
// (on 7a1424de neither was tied to its server's connection).
func TestNASOffEndsSMBBrowses(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, c := range []struct{ name, nas, scope string }{
		{"NAS slot", `{"protocol":"smb","url":%q,"username":"dj","password":"pw","enabled":true}`, "nas"},
		{"server entry", `{"smb":[{"id":"` + id + `","name":"Box","url":%q,"username":"dj","password":"pw","enabled":true}]}`, "nas:" + id},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBlackHoleSMB(t)
			r := newWebDAVRig(t, fmt.Sprintf(c.nas, b.url()), "scope="+c.scope+"\nomni_source=0\n")
			done := make(chan int, 1)
			go func() {
				resp, err := http.Get(r.gw.URL + "/v1/users/1/playlists?limit=50&offset=0")
				if err != nil {
					done <- -1
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				done <- resp.StatusCode
			}()
			if !waitUntil(3*time.Second, func() bool { return b.open.Load() > 0 }) {
				t.Fatal("the browse never connected")
			}
			if code, _ := uiPost(t, r.gw.URL, "nas=0"); code != http.StatusOK {
				t.Fatalf("nas=0: HTTP %d", code)
			}
			off := time.Now()
			if !waitUntil(500*time.Millisecond, func() bool { return b.open.Load() == 0 }) {
				t.Errorf("the browse's SMB connection was still open %v after NAS was switched off", time.Since(off).Round(time.Millisecond))
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the browse never answered")
			}
		})
	}
}
