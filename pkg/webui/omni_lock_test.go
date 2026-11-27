package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
)

// leftOut is how long a bubble test stays after its measurement: a synctest bubble's
// clock stops when the test function returns, so the searches an answer left out must
// end (within their own budgets, the longest 90 s) before it does. It costs no real time.
const leftOut = 2 * time.Minute

// newOmniServer is a deck with OmniSource on and no drive (media does not exist), built
// outside any synctest bubble: its own background work is not on the bubble's clock.
func newOmniServer(t *testing.T, nas, settings string, hc *http.Client) *Server {
	t.Helper()
	uiDir := t.TempDir()
	if nas != "" {
		if err := os.WriteFile(filepath.Join(uiDir, "nas.json"), []byte(nas), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir(), AllowLossy: true}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: t.TempDir(), MediaRoot: filepath.Join(t.TempDir(), "none"), HTTP: hc}, jobs)
	t.Cleanup(s.Close)
	return s
}

// WebDAV round 13 (round-12 re-check, item 4): the answer waited for every extra after
// the cut, and the servers' search reads the settings under the settings lock before it
// can see the cut. On b8b93665, with the selected source done after 1 ms and the read
// of nas.json taking 2.2 s under that lock (slow storage, or a save running), omniSearch
// returned after 2.2 s. Now it takes what the extras have filed at the cut and returns:
// at the later of "selected source done" and the deadline, to the millisecond, on the
// bubble's clock.
func TestOmniSearchNeverWaitsForTheSettings(t *testing.T) {
	for _, c := range []struct {
		name           string
		selected, want time.Duration
	}{
		{"the selected source done after 1 ms", time.Millisecond, serverReadyWait},
		{"the selected source done after 1.5 s", 1500 * time.Millisecond, 1500 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOmniServer(t, "", "sources=\nscope=\nomni_source=1\nwebdav=1\n", nil)
			synctest.Test(t, func(t *testing.T) {
				// The first read of this deck's nas.json from here on takes 2.2 s, as it is
				// read: under the settings lock.
				var slow atomic.Bool
				slow.Store(true)
				nas := s.nasConfigPath()
				stateIO.Store(&stateFiles{read: func(path string) ([]byte, error) {
					if path == nas && slow.CompareAndSwap(true, false) {
						time.Sleep(2200 * time.Millisecond)
					}
					return os.ReadFile(path)
				}})
				defer stateIO.Store(nil)
				selected := make(chan struct{})
				time.AfterFunc(c.selected, func() { close(selected) })
				start := time.Now()
				s.tidal.omniSearch(context.Background(), "intro", selected)
				if took := time.Since(start); took != c.want {
					t.Errorf("the answer left after %v, want %v: it waited for the settings", took, c.want)
				}
				time.Sleep(leftOut)
				if slow.Load() {
					t.Error("the settings were not read by the search: the test shows nothing")
				}
			})
		})
	}
}

// The same with the settings lock held by someone else (a save that does not end): the
// extras cannot even start reading. This is on the real clock, because a goroutine that
// waits for a mutex stops a synctest bubble's clock, and it bounds no wall time by the
// host's speed: the answer must come while the lock is still held, and on b8b93665 it
// never did, on any host.
func TestOmniSearchAnswersWhileTheSettingsAreLocked(t *testing.T) {
	s := newOmniServer(t, "", "sources=\nscope=\nomni_source=1\nwebdav=1\n", nil)
	selected := make(chan struct{})
	close(selected)
	s.uiMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.tidal.omniSearch(context.Background(), "intro", selected)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("no answer while the settings were locked: the answer waits for the settings lock")
	}
	s.uiMu.Unlock()
	<-done
}

// memDAV is WebDAV servers in memory (no socket, so a synctest bubble's clock sees every
// wait): each host's /music holds intro.mp3 and answers after its delay, or not at all
// (stuck).
type memDAV map[string]time.Duration

const stuck = time.Duration(-1)

func (m memDAV) RoundTrip(r *http.Request) (*http.Response, error) {
	delay, ok := m[r.URL.Host]
	if !ok {
		return nil, errors.New("no such server")
	}
	if r.Body != nil {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	wait := make(<-chan time.Time)
	if delay != stuck {
		wait = time.After(delay)
	}
	select {
	case <-wait:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	rec := httptest.NewRecorder()
	if r.Method == "PROPFIND" && strings.TrimSuffix(r.URL.Path, "/") == "/music" {
		rec.Header().Set("Content-Type", "application/xml")
		rec.WriteHeader(http.StatusMultiStatus)
		io.WriteString(rec, davXML("/music/", "/music/intro.mp3"))
	} else {
		rec.WriteHeader(http.StatusNotFound)
	}
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// WebDAV round 13 (round-12 re-check, item 4, the tests): the whole search reply, through
// the handler, with servers that are still busy at the cut, on a clock that does not
// depend on the host. A reply that waited for a busy extra would leave at that server's
// own search budget (4 s) and not at OmniSource's deadline (1 s); here each time is
// exact. The selected server keeps its own budget.
func TestSearchReplyTimesWithBusyServers(t *testing.T) {
	for _, c := range []struct {
		name    string
		servers memDAV
		want    time.Duration
		titles  []string
	}{
		{"selected at once, one extra stuck", memDAV{"selected.example": 0, "fast.example": 0, "stuck.example": stuck},
			serverReadyWait, []string{"intro@Fast", "intro@Selected"}},
		{"selected after 1.5 s, one extra stuck", memDAV{"selected.example": 1500 * time.Millisecond, "fast.example": 0, "stuck.example": stuck},
			1500 * time.Millisecond, []string{"intro@Fast", "intro@Selected"}},
		{"selected stuck to its own budget, one extra stuck", memDAV{"selected.example": stuck, "fast.example": 0, "stuck.example": stuck},
			searchTimeout, []string{"intro@Fast"}},
		{"selected at once, the only extra after 300 ms", memDAV{"selected.example": 0, "fast.example": 300 * time.Millisecond},
			300 * time.Millisecond, []string{"intro@Fast", "intro@Selected"}},
		{"selected at once, the only extra after 2.1 s", memDAV{"selected.example": 0, "fast.example": 2100 * time.Millisecond},
			serverReadyWait, []string{"intro@Selected"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var f nasFile
			for _, host := range slices.Sorted(func(yield func(string) bool) {
				for host := range c.servers {
					if !yield(host) {
						return
					}
				}
			}) {
				name := strings.ToUpper(host[:1]) + strings.TrimSuffix(host[1:], ".example")
				f.WebDAV = append(f.WebDAV, savedServer{Name: name, URL: "http://" + host + "/music/", Enabled: true})
			}
			raw, _ := json.Marshal(f)
			scope := scopeOf(serverViews(kindWebDAV, f), "Selected")
			s := newOmniServer(t, string(raw), "sources=\nscope="+scope+"\nomni_source=1\nwebdav=1\n", &http.Client{Transport: c.servers})
			synctest.Test(t, func(t *testing.T) {
				rec := httptest.NewRecorder()
				start := time.Now()
				s.tidal.search(rec, httptest.NewRequest("GET", "/v1/search?query=intro&limit=50&offset=0", nil))
				took := time.Since(start)
				var res struct{ Tracks struct{ Items []gwItem } }
				if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
					t.Fatalf("reply: %v (%s)", err, rec.Body.String())
				}
				if got := webdavTitles(res.Tracks.Items); !slices.Equal(got, c.titles) {
					t.Errorf("reply: %v, want %v", got, c.titles)
				}
				if took != c.want {
					t.Errorf("the reply left after %v, want %v", took, c.want)
				}
				time.Sleep(leftOut)
			})
		})
	}
}
