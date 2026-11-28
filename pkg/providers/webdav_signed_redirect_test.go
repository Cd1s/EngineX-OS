package providers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// Cloudreve with OneDrive or S3 storage answers a track GET with a signed URL on another
// origin. The download must follow it, and the WebDAV login must not travel with it.
func TestWebDAVDownloadFollowsSignedRedirectWithoutTheLogin(t *testing.T) {
	audio := []byte("ID3 the track's bytes")
	var hits int
	var auth, cookie, referer string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		auth, cookie, referer = r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("Referer")
		w.Write(audio)
	}))
	defer store.Close()
	dav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "dj" || p != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, store.URL+"/personal/download.aspx?tempauth=signed", http.StatusFound)
	}))
	defer dav.Close()

	p := NewSelfHostedProvider(NASConfig{Protocol: "webdav", URL: dav.URL + "/dav/", Username: "dj", Password: "secret", Enabled: true}, nil)
	path, err := p.DownloadTrack(context.Background(), "/dav/Dom%20Carlo%20-%20Terrace.mp3", t.TempDir())
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, audio) {
		t.Fatalf("saved %q (%v), want the track's bytes", got, err)
	}
	if hits != 1 || auth != "" || cookie != "" || referer != "" {
		t.Fatalf("signed URL got hits=%d Authorization=%q Cookie=%q Referer=%q; want one request and none of them", hits, auth, cookie, referer)
	}
}

// A redirect to an address that is not http(s) is refused, not fetched.
func TestWebDAVDownloadRefusesAnUnusableRedirect(t *testing.T) {
	dav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "file:///etc/passwd")
		w.WriteHeader(http.StatusFound)
	}))
	defer dav.Close()
	p := NewSelfHostedProvider(NASConfig{Protocol: "webdav", URL: dav.URL + "/dav/", Username: "dj", Password: "secret", Enabled: true}, nil)
	if _, err := p.DownloadTrack(context.Background(), "/dav/a.mp3", t.TempDir()); err == nil {
		t.Fatal("a redirect to file:// was followed")
	}
}
