package webui

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
)

// newOmniRig is a deck with OmniSource on, its drives under media and one WebDAV server
// (answering at once) selected: the source the DJ searches.
func newOmniRig(t *testing.T, media string) *webdavRig {
	t.Helper()
	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=\nomni_source=1\nwebdav=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dlDir, AllowLossy: true}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir, MediaRoot: media}, jobs)
	t.Cleanup(s.Close)
	gw := httptest.NewServer(s)
	t.Cleanup(gw.Close)
	r := &webdavRig{s: s, gw: gw, uiDir: uiDir}
	sel := newFakeDAV(t, testMP3(150), nil)
	views := r.change(t, serverChange{Action: "add", Name: "Selected", URL: sel.srv.URL + "/music/"}, 200)
	if code, _ := uiPost(t, r.gw.URL, "scope="+scopeOf(views, "Selected")); code != http.StatusOK {
		t.Fatalf("selecting the server: HTTP %d", code)
	}
	return r
}

// localBudget is how long a drive's or djay's search may run by itself (each's budget).
// An answer that comes well before it did not wait for that search: the bound is not on
// the host's speed, a correct answer comes at OmniSource's deadline (1 s).
const localBudget = 15 * time.Second

// WebDAV round 5 (round-11 re-check, item 5): the drives' and djay's searches are
// OmniSource extras like the servers'. On 16563658 they ran before the deadline was
// looked at: with the selected server answering at once, a drive whose library search
// did not end held the answer until that search's own 15 s budget, and a djay library
// that could not be read held it for as long as it took.
func TestOmniSourceLocalSearchesAddNoTime(t *testing.T) {
	answer := func(t *testing.T, r *webdavRig, release func()) {
		t.Helper()
		type result struct {
			titles []string
			took   time.Duration
		}
		done := make(chan result, 1)
		start := time.Now()
		go func() {
			var res struct{ Tracks struct{ Items []gwItem } }
			if resp, err := http.Get(r.gw.URL + "/v1/search?query=intro&limit=50&offset=0"); err == nil {
				json.NewDecoder(resp.Body).Decode(&res)
				resp.Body.Close()
			}
			done <- result{webdavTitles(res.Tracks.Items), time.Since(start)}
		}()
		var got result
		select {
		case got = <-done:
		case <-time.After(localBudget - 5*time.Second):
			t.Errorf("no answer %v after the search began: it waits for a local search", localBudget-5*time.Second)
			release()
			got = <-done
		}
		if !slices.Equal(got.titles, []string{"intro@Selected"}) {
			t.Errorf("answer %v, want the selected server's hit", got.titles)
		}
		if got.took < serverReadyWait {
			t.Errorf("the answer came after %v: the local searches did not get OmniSource's deadline (%v)", got.took, serverReadyWait)
		}
	}
	t.Run("a drive whose library search does not end", func(t *testing.T) {
		media := t.TempDir()
		dir := filepath.Join(media, "USB1", "Engine Library", "Database2")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// A library whose Track rows never end: the search reads on until it is told to stop.
		db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`CREATE VIEW Track AS WITH RECURSIVE n(id) AS (SELECT 1 UNION ALL SELECT id+1 FROM n WHERE id < 4000000000)
			SELECT id, '' AS path, 'track ' || id AS title, 'x' AS artist, 'x' AS album, 0 AS length, 0 AS bpmAnalyzed, 0 AS bpm FROM n`)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		r := newOmniRig(t, media)
		if n := len(r.s.drives()); n != 1 {
			t.Fatalf("%d drives found, want the one with the endless library", n)
		}
		answer(t, r, func() {})
	})
	t.Run("a djay library that is not read", func(t *testing.T) {
		media := t.TempDir()
		dir := filepath.Join(media, "USB2", "djay")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// A playlist file that is a pipe: opening it waits until release opens its other end.
		pipe := filepath.Join(dir, "held.m3u")
		if err := syscall.Mkfifo(pipe, 0o600); err != nil {
			t.Fatal(err)
		}
		release := sync.OnceFunc(func() {
			if f, err := os.OpenFile(pipe, os.O_RDWR, 0); err == nil {
				f.Close()
			}
		})
		t.Cleanup(release)
		r := newOmniRig(t, media)
		answer(t, r, release)
		release()
	})
}
