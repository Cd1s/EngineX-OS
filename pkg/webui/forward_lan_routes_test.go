package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The routes that other units on the LAN call without a login (a linked player reads this deck's
// state and the loaded track's audio; Spotify's own app signs the deck in) are not served through
// the remote console listener: its callers come from outside the deck.
func TestRemoteConsoleForwardHasNoLANRoutes(t *testing.T) {
	s, _ := newTestServer(t)
	forward := httptest.NewServer(s.ForwardHandler()) // a loopback listener, as the tunnel reaches it
	defer forward.Close()
	lan := httptest.NewServer(s)
	defer lan.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, url string, headers ...string) int {
		t.Helper()
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range headers {
			req.Header.Set(h, "x")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	for _, p := range []string{
		"/peer/decks", "//peer/decks", "/x/../peer/decks", "/%70eer/decks", "/peer",
		"/peer/decks/1/audio?key=k", "/peer/decks/1/perf?key=k", "/peer/decks/1/display?key=k",
		"/spotify-connect", "/spotify-connect?action=getInfo", "//spotify-connect",
	} {
		if got := do("GET", forward.URL+p); got != http.StatusNotFound {
			t.Errorf("forwarded GET %s: %d, want 404", p, got)
		}
	}
	for _, p := range []string{"/peer/settings-copy/request", "/spotify-connect"} {
		if got := do("POST", forward.URL+p); got != http.StatusNotFound {
			t.Errorf("forwarded POST %s: %d, want 404", p, got)
		}
	}

	// A linked player on the LAN is served as before.
	if got := do("GET", lan.URL+"/peer/decks"); got != http.StatusOK {
		t.Errorf("LAN GET /peer/decks: %d, want 200", got)
	}
	if got := do("GET", lan.URL+"/spotify-connect"); got != http.StatusOK {
		t.Errorf("LAN GET /spotify-connect: %d, want 200", got)
	}
	// A tunnel set up to reach the LAN listener itself: the headers its edge adds mark the caller
	// as from outside, as they already do for the deck-local gateway.
	for _, h := range []string{"Cf-Connecting-Ip", "Cf-Ray", "Cdn-Loop", "Tailscale-User-Login"} {
		for _, p := range []string{"/peer/decks", "/peer/decks/1/audio?key=k", "/spotify-connect"} {
			if got := do("GET", lan.URL+p, h); got != http.StatusNotFound {
				t.Errorf("LAN listener, GET %s carrying %s: %d, want 404", p, h, got)
			}
		}
	}
	// The console itself is still there, behind its login.
	if got := do("GET", forward.URL+"/"); got != http.StatusOK {
		t.Errorf("forwarded GET /: %d, want the login page (200)", got)
	}
	if got := do("GET", forward.URL+"/api/ui"); got != http.StatusUnauthorized {
		t.Errorf("forwarded GET /api/ui without a login: %d, want 401", got)
	}
}
