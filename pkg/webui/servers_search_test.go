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

// WebDAV round 4 (round-10 re-check, item 5): with OmniSource on, the selected source
// was searched first and with no cutoff. With the slow WebDAV server itself selected,
// the fast server's hit came back after 4 s (the slow one's search budget). Now the
// selected source runs beside the others under OmniSource's deadline, whichever source
// it is (a server, a streaming login): ready rows never wait for it, and its search
// goes on and is cached for the next one.
func TestServersSelectedSourceNeverHoldsReadyResults(t *testing.T) {
	t.Run("slow server selected", func(t *testing.T) {
		hold := make(chan struct{})
		fast, slow := newFakeDAV(t, testMP3(120), nil), newFakeDAV(t, testMP3(90), hold)
		t.Cleanup(func() { close(hold) })
		r := newWebDAVRig(t, "", "sources=\nscope=\nomni_source=1\nwebdav=1\n")
		r.change(t, serverChange{Action: "add", Name: "Fast", URL: fast.srv.URL + "/music/"}, 200)
		views := r.change(t, serverChange{Action: "add", Name: "Slow", URL: slow.srv.URL + "/music/"}, 200)
		if code, _ := uiPost(t, r.gw.URL, "scope="+scopeOf(views, "Slow")); code != http.StatusOK {
			t.Fatalf("selecting the slow server: HTTP %d", code)
		}
		start := time.Now()
		got := webdavTitles(r.search(t, "intro"))
		if elapsed := time.Since(start); elapsed > searchTimeout/2 {
			t.Errorf("the fast server's hit waited %v for the selected slow server", elapsed)
		}
		if !slices.Equal(got, []string{"intro@Fast"}) {
			t.Errorf("OmniSource search with the slow server selected found %v", got)
		}
	})
	t.Run("slow streaming login selected", func(t *testing.T) {
		fast := newFakeDAV(t, testMP3(120), nil)
		raw, _ := json.Marshal(nasFile{WebDAV: []savedServer{{Name: "Fast", URL: fast.srv.URL + "/music/", Enabled: true}}})
		r := newWebDAVRig(t, string(raw), "sources=spotify\nscope=spotify\nomni_source=1\nwebdav=1\n")
		slow := &slowSearch{hold: make(chan struct{})}
		release := sync.OnceFunc(func() { close(slow.hold) })
		t.Cleanup(release)
		r.s.mu.Lock()
		r.s.sessions["sp-owner"] = &Session{ID: "sp-owner", Provider: "spotify", Ready: true, p: slow}
		r.s.mu.Unlock()
		start := time.Now()
		got := webdavTitles(r.search(t, "intro"))
		if elapsed := time.Since(start); elapsed > searchTimeout/2 {
			t.Errorf("the fast server's hit waited %v for the selected streaming login", elapsed)
		}
		if !slices.Equal(got, []string{"intro@Fast"}) {
			t.Errorf("OmniSource search with the slow login selected found %v", got)
		}
		// Its search goes on: once it answers, the next search has its row (from the cache).
		release()
		if !waitUntil(3*time.Second, func() bool { return slices.Contains(webdavTitles(r.search(t, "intro")), "intro slow@") }) {
			t.Error("the selected login's late answer is not in the next search")
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
			select {
			case code := <-done:
				if took := time.Since(off); code != http.StatusOK || took > time.Second {
					t.Errorf("the search answered HTTP %d %v after NAS was switched off", code, took.Round(time.Millisecond))
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
