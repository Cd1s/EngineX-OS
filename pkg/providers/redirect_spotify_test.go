package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

// #335 round 12 (round-11 re-check, item 2, the body): a provider's login and token
// requests carry their credential in the body (Spotify's refresh token, a client
// secret, a password) and no credential header. Go sends a body again on a 307 or 308,
// to any origin. On 900f8055 Spotify's token refresh followed such a redirect and the
// other origin was sent the refresh token. Now a redirect that repeats the body stays
// on the request's origin, for every request of the shared client.
func TestRedirectsNeverRepeatABodyOnAnotherOrigin(t *testing.T) {
	const refresh = "spotify-refresh-r12"
	var mu sync.Mutex
	var sent []string // what the other origin was sent: "<method> <body>"
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = append(sent, r.Method+" "+string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"from-elsewhere"}`))
	}))
	defer elsewhere.Close()
	accounts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.Split(r.URL.Path, "/")[1] {
		case "307":
			http.Redirect(w, r, elsewhere.URL+"/api/token", http.StatusTemporaryRedirect)
		case "308":
			http.Redirect(w, r, elsewhere.URL+"/api/token", http.StatusPermanentRedirect)
		case "302":
			http.Redirect(w, r, elsewhere.URL+"/api/token", http.StatusFound)
		case "same":
			http.Redirect(w, r, "/final/api/token", http.StatusTemporaryRedirect)
		case "final":
			if b, _ := io.ReadAll(r.Body); !strings.Contains(string(b), refresh) {
				http.Error(w, "the body did not follow the redirect", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"fresh"}`))
		}
	}))
	defer accounts.Close()
	token := func(path string) (Token, error) {
		s := &Spotify{AccountsURL: accounts.URL + path}
		return s.doTokenRequest(context.Background(), "client", Token{}, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	}
	leaked := func(t *testing.T, before int) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		for _, v := range sent[before:] {
			if strings.Contains(v, refresh) {
				t.Errorf("another origin was sent the refresh token: %q", v)
			}
		}
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(sent)
	}
	for _, code := range []string{"307", "308"} {
		t.Run(code, func(t *testing.T) {
			before := count()
			tok, err := token("/" + code)
			leaked(t, before)
			if !errors.Is(err, ErrRedirectOffOrigin) || tok.AccessToken != "" {
				t.Errorf("a token request redirected to another origin: token %q, error %v, want it refused", tok.AccessToken, err)
			}
		})
	}
	t.Run("within the origin", func(t *testing.T) {
		if tok, err := token("/same"); err != nil || tok.AccessToken != "fresh" {
			t.Errorf("a redirect within the origin: token %q, error %v, want the body sent again there", tok.AccessToken, err)
		}
	})
	t.Run("302 sends no body", func(t *testing.T) {
		// Go turns it into a GET without the body: nothing of ours leaves, as before.
		before := count()
		if _, err := token("/302"); err != nil {
			t.Errorf("a 302 without a credential in its headers: %v, want it followed as before", err)
		}
		leaked(t, before)
	})
}
