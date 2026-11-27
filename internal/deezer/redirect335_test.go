package deezer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/redirect"
)

// sent records what one fixture server was sent.
type sent struct {
	mu   sync.Mutex
	reqs []string // "<method> <path> cookie=<Cookie> referer=<Referer> body=<body>"
}

func (s *sent) handler(answer func(w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.RequestURI()+" cookie="+r.Header.Get("Cookie")+" referer="+r.Referer()+" body="+string(b))
		s.mu.Unlock()
		answer(w, r)
	})
}

func (s *sent) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

// dialTo is a transport that connects every request to the fixture server named for
// its host, whatever address that host has: www.deezer.com and media.deezer.com are
// then the fixtures, and the session's own cookie jar and redirect policy run as on a
// deck. Requests are plain HTTP (the fixtures speak no TLS).
func dialTo(t *testing.T, hosts map[string]*httptest.Server) http.RoundTripper {
	t.Helper()
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		srv := hosts[host]
		if srv == nil {
			t.Errorf("a request went to %s, which is no fixture", host)
			return nil, errors.New("no such fixture")
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme = "http" // the fixtures are plain HTTP; the origin rule saw https
		return tr.RoundTrip(r)
	})
}

// #335 round 13 (round-12 re-check, item 2): the Deezer downloader's account client had
// a cookie jar with the ARL for every deezer.com host and Go's redirect policy. On
// d5f60ddf a redirect of the account request to another deezer.com host was followed
// with the ARL cookie, and a 307 of the media request sent the licence and track
// tokens to another host. Now the account client follows a redirect only within the
// origin it asked; the CDN client, which has no jar, may still follow one.
func TestAccountRequestsStayOnTheirOrigin(t *testing.T) {
	arl := strings.Repeat("a", 192)
	elsewhere := &sent{}
	other := httptest.NewServer(elsewhere.handler(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":{"checkForm":"csrf","USER":{"USER_ID":1,"OPTIONS":{"license_token":"license"}}},"data":[]}`))
	}))
	defer other.Close()
	leaked := func(t *testing.T, what string, secrets ...string) {
		t.Helper()
		for _, r := range elsewhere.all() {
			for _, secret := range secrets {
				if strings.Contains(r, secret) {
					t.Errorf("%s: another origin was sent %q: %s", what, secret, r)
				}
			}
		}
	}
	t.Run("the account request", func(t *testing.T) {
		www := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://evil.deezer.com"+r.URL.RequestURI(), http.StatusFound)
		}))
		defer www.Close()
		transport := http.DefaultTransport
		defer func() { http.DefaultTransport = transport }()
		http.DefaultTransport = dialTo(t, map[string]*httptest.Server{"www.deezer.com": www, "evil.deezer.com": other})
		_, err := Login(context.Background(), arl)
		leaked(t, "login", arl)
		if !errors.Is(err, redirect.ErrOffOrigin) {
			t.Errorf("a login redirected to another origin: error %v, want it refused", err)
		}
	})
	t.Run("the media request", func(t *testing.T) {
		www := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"results":{"checkForm":"csrf","USER":{"USER_ID":6850245583,"OPTIONS":{"license_token":"license-r13","web_hq":true,"web_lossless":true}}}}`))
		}))
		defer www.Close()
		media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://evil.deezer.com/v1/get_url", http.StatusTemporaryRedirect)
		}))
		defer media.Close()
		transport := http.DefaultTransport
		defer func() { http.DefaultTransport = transport }()
		http.DefaultTransport = dialTo(t, map[string]*httptest.Server{"www.deezer.com": www, "media.deezer.com": media, "evil.deezer.com": other})
		s, err := Login(context.Background(), arl)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = s.mediaURL(context.Background(), "track-token-r13", DownloadOptions{Bitrate: 9, AllowLossy: true})
		leaked(t, "media URL", arl, "license-r13", "track-token-r13")
		if !errors.Is(err, redirect.ErrOffOrigin) {
			t.Errorf("a media request redirected to another origin: error %v, want it refused", err)
		}
	})
	t.Run("the CDN follows a redirect without a Referer", func(t *testing.T) {
		audio := append([]byte("fLaC"), make([]byte, 4096)...)
		edge := &sent{}
		edgeSrv := httptest.NewServer(edge.handler(func(w http.ResponseWriter, r *http.Request) { w.Write(audio) }))
		defer edgeSrv.Close()
		cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://edge.example/audio", http.StatusFound)
		}))
		defer cdn.Close()
		s := &Session{hc: &http.Client{Transport: dialTo(t, map[string]*httptest.Server{"cdn.deezer.com": cdn, "edge.example": edgeSrv})}}
		if err := s.fetchDecrypt(context.Background(), "https://cdn.deezer.com/audio?hdnea=signed-r13", "3135556", filepath.Join(t.TempDir(), "track")); err != nil {
			t.Fatalf("the CDN's redirect to another host: %v, want it followed", err)
		}
		got := edge.all()
		if len(got) != 1 || strings.Contains(got[0], "signed-r13") || !strings.Contains(got[0], "cookie= referer= ") {
			t.Errorf("the CDN's redirect target was sent %v, want one request with no cookie and no Referer", got)
		}
	})
}
