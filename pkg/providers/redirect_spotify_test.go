package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #335 round 12 (round-11 re-check, item 2): Spotify's API and stream requests carry a
// bearer token and used the shared provider client with Go's redirect policy, which
// keeps Authorization on another port of the same host. On 900f8055 both requests
// followed a redirect to another port with the token. Now the shared client follows a
// credentialed request's redirect only within its first URL's origin, without a
// Referer, for every provider.
func TestSpotifyRedirectsKeepTheTokenOnTheOrigin(t *testing.T) {
	const token = "spotify-token-r12"
	other := &seen{}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.add(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer elsewhere.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/same/"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"items":[{"id":"pl1","name":"Crate","tracks":{"total":1}}],"total":1}`))
		case strings.HasPrefix(r.URL.Path, "/v1/same"):
			http.Redirect(w, r, "/same"+strings.TrimPrefix(r.URL.Path, "/v1/same")+"?"+r.URL.RawQuery, http.StatusFound)
		default:
			http.Redirect(w, r, elsewhere.URL+r.URL.Path+"?"+r.URL.RawQuery, http.StatusFound)
		}
	}))
	defer api.Close()
	ctx := context.Background()
	leaked := func(t *testing.T, step string, before int) {
		t.Helper()
		for _, r := range other.all()[before:] {
			t.Errorf("%s: a redirect took the request to another origin: Authorization %q, Referer %q", step, r.Header.Get("Authorization"), r.Referer())
		}
	}
	t.Run("api", func(t *testing.T) {
		before := len(other.all())
		s := &Spotify{BaseURL: api.URL + "/v1", Token: Token{AccessToken: token}}
		var out map[string]any
		err := s.get(ctx, "/me/playlists?limit=50", &out)
		leaked(t, "API request", before)
		if err == nil {
			t.Error("the refused redirect reported no error")
		}
		// A redirect within the API's origin is followed.
		s = &Spotify{BaseURL: api.URL + "/v1/same", Token: Token{AccessToken: token}}
		if err := s.get(ctx, "/me/playlists?limit=50", &out); err != nil {
			t.Errorf("a redirect within the API's origin: %v", err)
		}
	})
	t.Run("stream", func(t *testing.T) {
		before := len(other.all())
		s := &Spotify{BaseURL: api.URL, StreamURL: api.URL + "/audio/4uLU6hMCjMI75M1A2tKUQC", Token: Token{AccessToken: token}}
		_, err := s.DownloadAudio(ctx, "4uLU6hMCjMI75M1A2tKUQC", t.TempDir())
		leaked(t, "stream request", before)
		if err == nil {
			t.Error("the refused redirect reported no error")
		}
	})
	t.Run("CDN media", func(t *testing.T) {
		before := len(other.all())
		s := &Spotify{}
		_, err := s.fetchSpotifyCDN(ctx, api.URL+"/cdn/audio?token=cdn-r12")
		leaked(t, "CDN request", before)
		if err == nil {
			t.Error("the refused redirect reported no error")
		}
	})
}

// Every request the shared client sends with a credential keeps it on its origin; one
// without a credential may follow a redirect anywhere, as before.
func TestProviderClientRedirectRule(t *testing.T) {
	other := &seen{}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.add(r)
		w.Write([]byte(`{}`))
	}))
	defer elsewhere.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/x", http.StatusFound)
	}))
	defer origin.Close()
	for _, c := range []struct {
		name    string
		header  http.Header
		query   string
		follows bool
	}{
		{"Authorization header", http.Header{"Authorization": {"Bearer a"}}, "", false},
		{"cookie", http.Header{"Cookie": {"arl=b"}}, "", false},
		{"token header", http.Header{"X-Plex-Token": {"c"}}, "", false},
		{"token in the query", nil, "?access_token=d", false},
		{"API key in the query", nil, "?apiKey=e", false},
		{"no credential", nil, "?q=house", true},
	} {
		before := len(other.all())
		var out map[string]any
		err := JSON(context.Background(), nil, "GET", origin.URL+"/a"+c.query, nil, c.header, &out)
		reached := len(other.all()) > before
		if reached != c.follows || (err == nil) != c.follows {
			t.Errorf("%s: reached the other origin %v (err %v), want %v", c.name, reached, err, c.follows)
		}
		for _, r := range other.all()[before:] {
			if r.Referer() != "" {
				t.Errorf("%s: the redirect carried a Referer %q", c.name, r.Referer())
			}
		}
	}
}
