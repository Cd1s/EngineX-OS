package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// seen records what one fixture server was sent.
type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (s *seen) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r.Clone(context.Background()))
}

func (s *seen) all() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.reqs...)
}

// #335 round 11 (round-10 re-check, item 2): every request of a NAS provider carries the
// NAS's credentials (Subsonic in the query, Plex's X-Plex-Token header, WebDAV's
// Authorization). On 5ea09903 its client followed a redirect to any origin: another
// server got the Subsonic auth in the Referer, the Plex token header, and (same host,
// another port) the WebDAV Authorization. Now a redirect is followed only within the
// configured origin, and no redirect carries a Referer.
func TestSelfHostedRedirectsStayOnTheOrigin(t *testing.T) {
	const pass, token = "nas-pass-r11-335", "plex-token-r11-335"
	other := &seen{}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.add(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer elsewhere.Close()
	home := &seen{}
	nas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		home.add(r)
		switch r.URL.Path {
		case "/rest/getPlaylists.view", "/playlists", "/music/a.mp3":
			http.Redirect(w, r, elsewhere.URL+r.URL.Path+"?"+r.URL.RawQuery, http.StatusFound)
		case "/same/rest/getPlaylists.view":
			http.Redirect(w, r, "/same/rest/getPlaylists2.view?"+r.URL.RawQuery, http.StatusFound)
		case "/same/rest/getPlaylists2.view":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"subsonic-response":{"status":"ok","playlists":{"playlist":[{"id":"pl-1","name":"Crate","songCount":1}]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer nas.Close()
	ctx := context.Background()
	for _, c := range []struct {
		name string
		cfg  NASConfig
		do   func(p *SelfHostedProvider) error
	}{
		{"subsonic", NASConfig{Protocol: "subsonic", URL: nas.URL, Username: "dj", Password: pass}, func(p *SelfHostedProvider) error { _, err := p.Playlists(ctx); return err }},
		{"plex", NASConfig{Protocol: "plex", URL: nas.URL, Token: token}, func(p *SelfHostedProvider) error { _, err := p.Playlists(ctx); return err }},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := len(other.all())
			err := c.do(NewSelfHostedProvider(c.cfg, nil))
			if got := other.all()[before:]; len(got) != 0 {
				r := got[0]
				t.Fatalf("a redirect took the request to another origin: query %q, Referer %q, X-Plex-Token %v, Authorization %v",
					r.URL.RawQuery, r.Referer(), r.Header.Get("X-Plex-Token") != "", r.Header.Get("Authorization") != "")
			}
			if err == nil {
				t.Fatal("the refused redirect reported no error")
			}
		})
	}
	// WebDAV downloads are the exception: a server that keeps its files on OneDrive or S3
	// (Cloudreve) answers every track GET with a signed URL on another origin. The download
	// follows it in a fresh request that carries none of the login.
	t.Run("webdav download", func(t *testing.T) {
		before := len(other.all())
		p := NewSelfHostedProvider(NASConfig{Protocol: "webdav", URL: nas.URL + "/music/", Username: "dj", Password: pass}, nil)
		if _, err := p.DownloadTrack(ctx, "/music/a.mp3", t.TempDir()); err != nil {
			t.Fatalf("download through the signed redirect: %v", err)
		}
		got := other.all()[before:]
		if len(got) != 1 {
			t.Fatalf("the other origin got %d requests, want the one download", len(got))
		}
		if r := got[0]; r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Referer() != "" || strings.Contains(r.URL.String(), pass) {
			t.Fatalf("the login reached the other origin: query %q, Referer %q, Authorization %v", r.URL.RawQuery, r.Referer(), r.Header.Get("Authorization") != "")
		}
	})
	// A redirect within the origin is followed, and carries no Referer.
	p := NewSelfHostedProvider(NASConfig{Protocol: "subsonic", URL: nas.URL + "/same", Username: "dj", Password: pass}, nil)
	lists, err := p.Playlists(ctx)
	if err != nil || len(lists) != 1 {
		t.Fatalf("a redirect within the NAS's origin: %v %v", lists, err)
	}
	for _, r := range home.all() {
		if r.URL.Path == "/same/rest/getPlaylists2.view" && r.Referer() != "" {
			t.Fatalf("the redirect within the origin carried a Referer: %q", r.Referer())
		}
	}
}

// CheckRedirect itself: no Referer on any redirect; a marked request (or every request
// when always is set) stays on the origin of its first URL (scheme, host and effective
// port); others may leave it.
func TestCheckRedirectPolicy(t *testing.T) {
	req := func(ctx context.Context, raw string) *http.Request {
		r, _ := http.NewRequestWithContext(ctx, "GET", raw, nil)
		r.Header.Set("Referer", "https://nas.example/cover?t=secret")
		return r
	}
	ctx, marked := context.Background(), WithCredential(context.Background())
	first := req(ctx, "https://nas.example/cover?t=secret")
	for _, c := range []struct {
		always bool
		ctx    context.Context
		to     string
		ok     bool
	}{
		{false, ctx, "https://cdn.example/a.jpg", true},
		{false, marked, "https://cdn.example/a.jpg", false},
		{false, marked, "http://nas.example/cover", false},
		{false, marked, "https://nas.example:8443/cover", false},
		{false, marked, "https://nas.example:443/other", true},
		{true, ctx, "https://nas.example.evil/cover", false},
		{true, ctx, "https://sub.nas.example/cover", false},
		{true, ctx, "https://NAS.example/other", true},
	} {
		next := req(c.ctx, c.to)
		err := CheckRedirect(c.always, nil)(next, []*http.Request{first})
		if (err == nil) != c.ok {
			t.Errorf("always=%v marked=%v redirect to %s: err %v, want followed=%v", c.always, HasCredential(c.ctx), c.to, err, c.ok)
		}
		if next.Header.Get("Referer") != "" {
			t.Errorf("redirect to %s kept its Referer", c.to)
		}
	}
}
