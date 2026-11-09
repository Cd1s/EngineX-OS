package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSoundCloudSearchDoesNotWalkLibraryPages(t *testing.T) {
	var pages atomic.Int32
	var base string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/tracks" {
			pages.Add(1)
			w.WriteHeader(500)
			return
		}
		if r.URL.Query().Get("limit") != "30" || r.URL.Query().Get("q") != "Daft Punk" {
			t.Errorf("search query changed: %s", r.URL.RawQuery)
		}
		fmt.Fprintf(w, `{"collection":[{"id":1,"title":"first"},{"id":2,"access":"blocked"},{"id":3,"policy":"BLOCK"},{"id":4,"title":"last"}],"next_href":%q}`, base+"/more")
	}))
	defer api.Close()
	base = api.URL
	// Use a test site that supplies a public app identifier, without any login.
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			fmt.Fprintf(w, `<script src="http://%s/app.js"></script>`, r.Host)
		} else {
			fmt.Fprint(w, `client_id:"0123456789abcdef0123456789abcdef"`)
		}
	}))
	defer site.Close()
	p := &SoundCloud{WebURL: api.URL, SiteURL: site.URL}
	tracks, err := p.Search(context.Background(), "Daft Punk")
	if err != nil || len(tracks) != 2 || tracks[0].Title != "first" || tracks[1].Title != "last" || pages.Load() != 0 {
		t.Fatalf("search followed next_href or changed order/filter: %+v err=%v extra_pages=%d", tracks, err, pages.Load())
	}
}
