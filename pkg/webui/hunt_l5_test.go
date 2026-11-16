package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

type huntL5Provider struct {
	provider   string
	mu         sync.Mutex
	err        error
	queries    []string
	playlists  []providers.Playlist
	tracks     []providers.Track
	searchHits []providers.Track
	delay      time.Duration
}

func (p *huntL5Provider) Playlists(ctx context.Context) ([]providers.Playlist, error) {
	p.mu.Lock()
	err, d, out := p.err, p.delay, append([]providers.Playlist(nil), p.playlists...)
	p.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return out, err
}

func (p *huntL5Provider) Tracks(ctx context.Context, _ string) ([]providers.Track, error) {
	p.mu.Lock()
	err, d, out := p.err, p.delay, append([]providers.Track(nil), p.tracks...)
	p.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return out, err
}

func (p *huntL5Provider) Search(ctx context.Context, q string) ([]providers.Track, error) {
	p.mu.Lock()
	err, d, out := p.err, p.delay, append([]providers.Track(nil), p.searchHits...)
	p.queries = append(p.queries, q)
	p.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return out, err
}

func newHuntL5Server(t *testing.T, scope string, p *huntL5Provider) (*Server, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("scope="+scope+"\nsources="+scope+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) {
		return nil, errors.New("unconfigured")
	}}, nil)
	t.Cleanup(s.Close)
	now := time.Unix(1_700_000_000, 0)
	s.tidal.browse.now = func() time.Time { return now }
	s.sessions["s1"] = &Session{ID: "s1", Provider: scope, Ready: true, p: p, expires: time.Now().Add(time.Hour)}
	return s, &now
}

func huntGet(s *Server, target string, ctx context.Context) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	s.tidal.ServeHTTP(rec, req)
	return rec
}

func TestHuntL5TokenExpiry401ThenSuccess(t *testing.T) {
	p := &huntL5Provider{
		provider:  "spotify",
		err:       providers.StatusError(401),
		playlists: []providers.Playlist{{ID: "pl1", Title: "Set"}},
		tracks:    []providers.Track{{ID: "t1", Title: "Track 1", Provider: "spotify"}},
	}
	s, now := newHuntL5Server(t, "spotify", p)

	rec := huntGet(s, "/v1/users/1/playlists", nil)
	if rec.Code != 200 {
		t.Fatalf("playlists status = %d, want 200 with auth hint", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "401") {
		t.Fatalf("playlists body missing 401 hint: %s", rec.Body.String())
	}

	p.mu.Lock()
	p.err = nil
	p.mu.Unlock()
	*now = now.Add(browseRetry + time.Second)

	rec = huntGet(s, "/v1/users/1/playlists", nil)
	if rec.Code != 200 {
		t.Fatalf("recovered playlists status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"Set"`) {
		t.Fatalf("recovered playlists missing Set: %s", rec.Body.String())
	}
}

func TestHuntL5NetworkDropAndReturnTimeouts(t *testing.T) {
	p := &huntL5Provider{
		provider:   "spotify",
		err:        context.DeadlineExceeded,
		playlists:  []providers.Playlist{{ID: "pl1", Title: "Recovered"}},
		searchHits: []providers.Track{{ID: "s1", Title: "Back Online", Provider: "spotify"}},
	}
	s, now := newHuntL5Server(t, "spotify", p)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p.mu.Lock()
	p.delay = 200 * time.Millisecond
	p.mu.Unlock()
	rec := huntGet(s, "/v1/search?query=drop", ctx)
	if rec.Code != 200 {
		t.Fatalf("search during timeout status = %d, want 200 (empty results)", rec.Code)
	}

	p.mu.Lock()
	p.delay = 0
	p.mu.Unlock()
	rec = huntGet(s, "/v1/users/1/playlists", nil)
	if !strings.Contains(rec.Body.String(), "List incomplete") {
		t.Fatalf("expected incomplete notice during drop, got: %s", rec.Body.String())
	}

	p.mu.Lock()
	p.err = nil
	p.mu.Unlock()
	*now = now.Add(browseRetry + time.Second)

	rec = huntGet(s, "/v1/users/1/playlists", nil)
	if !strings.Contains(rec.Body.String(), `"Recovered"`) || strings.Contains(rec.Body.String(), "List incomplete") {
		t.Fatalf("browse did not recover cleanly: %s", rec.Body.String())
	}
	rec = huntGet(s, "/v1/search?query=drop", nil)
	if !strings.Contains(rec.Body.String(), `"Back Online"`) {
		t.Fatalf("search did not recover: %s", rec.Body.String())
	}
}

func TestHuntL5SearchNonASCII(t *testing.T) {
	p := &huntL5Provider{
		provider:   "netease",
		searchHits: []providers.Track{{ID: "1", Title: "晴天", Artist: "周杰伦", Provider: "netease"}},
	}
	s, _ := newHuntL5Server(t, "netease", p)

	for _, q := range []string{"周杰伦 晴天", "Café del Mar", "Mix 🎵"} {
		rec := huntGet(s, "/v1/search?query="+url.QueryEscape(q), nil)
		if rec.Code != 200 {
			t.Fatalf("search(%q) status = %d, want 200: %s", q, rec.Code, rec.Body.String())
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queries) != 3 || p.queries[0] != "周杰伦 晴天" || p.queries[1] != "Café del Mar" || p.queries[2] != "Mix 🎵" {
		t.Fatalf("provider received queries = %#v", p.queries)
	}
}

func TestHuntL5VeryLongTitle(t *testing.T) {
	longTitle := strings.Repeat("音", 150) + " — " + strings.Repeat("a", 120)
	longArtist := strings.Repeat("歌", 60)
	name := stemsFileName(longArtist, longTitle, ".flac")
	if len(name) > 255-len(".flac") {
		t.Fatalf("stemsFileName length = %d, exceeds 250 bytes", len(name))
	}
	if !utf8.ValidString(name) {
		t.Fatalf("stemsFileName produced invalid UTF-8: %q", name)
	}

	cacheDir := t.TempDir()
	cache := filepath.Join(cacheDir, "999.flac")
	if err := os.WriteFile(cache, []byte("not-real-flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := stemsImportFile(cache, providers.Track{ID: "999", Title: longTitle, Artist: longArtist}, 0)
	if len(filepath.Base(out)) > 255 {
		t.Fatalf("imported filename length = %d > 255", len(filepath.Base(out)))
	}

	p := &huntL5Provider{
		provider:  "deezer",
		playlists: []providers.Playlist{{ID: "pl1", Title: longTitle}},
		tracks:    []providers.Track{{ID: "t1", Title: longTitle, Artist: longArtist, Provider: "deezer"}},
	}
	s, _ := newHuntL5Server(t, "deezer", p)
	rec := huntGet(s, "/v1/users/1/playlists", nil)
	if rec.Code != 200 {
		t.Fatalf("playlists with long title status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON for long title: %v", err)
	}
}

func TestHuntL5ConcurrentLoadsWhilePlayingAndQuickLoads(t *testing.T) {
	dlDir := t.TempDir()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	j := NewJobs(Pipeline{
		Catalog: &providers.Deezer{},
		Deemix:  &deemixclient.Client{DownloadDir: dlDir},
	})
	defer j.Close()
	for _, id := range []string{"101", "102", "103"} {
		f := filepath.Join(dlDir, id+".flac")
		if err := os.WriteFile(f, []byte("audio-"+id), 0o644); err != nil {
			t.Fatal(err)
		}
		tr := providers.Track{ID: id, Title: "T" + id, Provider: "deezer"}
		j.putFile(trackKey(tr), fetched{tr, &deemixclient.Download{Path: f}})
	}

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := []string{"101", "102", "103"}[i%3]
			p, err := j.Fetch(context.Background(), "s1", providers.Track{ID: id, Provider: "deezer"}, false)
			if err != nil {
				errs <- err
				return
			}
			if _, err := os.Stat(p); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Fetch failed: %v", err)
	}
}
