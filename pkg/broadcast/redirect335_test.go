package broadcast

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// #335 round 11 (item 2's class: every client that follows redirects with a
// credential): the metadata update sends the server's password. On 5ea09903 it followed
// a redirect to another port of the same host with the password (Go keeps
// Authorization there). Now it follows a redirect only within the configured server's
// origin.
func TestUpdateMetadataKeepsThePasswordOnTheServer(t *testing.T) {
	var got atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	var followed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/metadata":
			if r.URL.Query().Get("song") == "Same" {
				http.Redirect(w, r, "/admin/metadata2?"+r.URL.RawQuery, http.StatusFound)
				return
			}
			http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusFound)
		case "/admin/metadata2":
			if _, pass, ok := r.BasicAuth(); ok && pass == "bc-pass-r11" {
				followed.Store(true)
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	b := New(newTestGuard())
	defer b.Close()
	b.cfg = Config{Protocol: "icecast", ServerURL: server.URL, Mountpoint: "/live.mp3", Username: "source", Password: "bc-pass-r11"}
	err := b.UpdateMetadata("Artist", "Title")
	if v, _ := got.Load().(string); v != "" {
		t.Fatalf("the metadata update took the password to another origin: %q", v)
	}
	if err == nil {
		t.Fatal("the refused redirect reported no error")
	}
	// A redirect within the server's origin is followed, with the password.
	if err := b.UpdateMetadata("", "Same"); err != nil || !followed.Load() {
		t.Fatalf("a redirect within the server: %v, followed with the password: %v", err, followed.Load())
	}
}
