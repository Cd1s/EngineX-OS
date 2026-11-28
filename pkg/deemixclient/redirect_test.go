package deemixclient

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// #335 round 11 (round-10 re-check, item 2): a signed NAS cover embedded in a download
// (marked providers.WithCredential) follows redirects only within its own origin, and no
// redirect carries a Referer. On 5ea09903 the cover fetch followed a redirect to another
// HTTPS origin and sent the signed URL, credentials included, as its Referer. A cover not
// marked may still be redirected elsewhere, without a Referer.
func TestEmbedCoverRedirectsKeepCredentialsOnTheOrigin(t *testing.T) {
	jpeg := append([]byte{0xff, 0xd8, 0xff}, bytes.Repeat([]byte("art"), 100)...)
	var mu sync.Mutex
	var refs []string
	others := 0
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		others++
		refs = append(refs, r.Referer())
		mu.Unlock()
		w.Write(jpeg)
	}))
	defer elsewhere.Close()
	nas := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/getCoverArt.view":
			http.Redirect(w, r, elsewhere.URL+"/a.jpg", http.StatusFound)
		case "/same/getCoverArt.view":
			http.Redirect(w, r, "/same/a.jpg", http.StatusFound)
		case "/same/a.jpg":
			mu.Lock()
			refs = append(refs, r.Referer())
			mu.Unlock()
			w.Write(jpeg)
		}
	}))
	defer nas.Close()
	dir := t.TempDir()
	c := &Client{DownloadDir: dir, AllowLossy: true, Media: nas.Client()} // trusts both test servers
	mp3 := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, mp3Frames(14, 3), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	signed := providers.WithCredential(context.Background())
	if err := c.EmbedCover(signed, mp3("a.mp3"), nas.URL+"/rest/getCoverArt.view?id=cv&u=dj&t=r11token&s=salt"); err == nil {
		t.Error("a signed cover redirected to another origin was embedded")
	}
	mu.Lock()
	if others != 0 {
		t.Errorf("a signed cover's redirect reached another origin %d time(s), Referer %q", others, refs)
	}
	mu.Unlock()
	if err := c.EmbedCover(signed, mp3("b.mp3"), nas.URL+"/same/getCoverArt.view?u=dj&t=r11token&s=salt"); err != nil {
		t.Fatalf("a signed cover redirected within its origin: %v", err)
	}
	if err := c.EmbedCover(context.Background(), mp3("c.mp3"), nas.URL+"/rest/getCoverArt.view"); err != nil {
		t.Fatalf("a cover without credentials redirected to a CDN: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, ref := range refs {
		if ref != "" {
			t.Fatalf("a redirect carried a Referer: %q", ref)
		}
	}
}
