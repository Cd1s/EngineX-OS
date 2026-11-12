package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Requests to another EngineX deck use its answer as it is: a redirect it sends is not followed.
func TestPeerRequestsDoNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer peer.Close()

	if resp, err := peerClient.Get(peer.URL + "/peer/decks"); err == nil {
		resp.Body.Close()
	}
	if got := fetchPerf(context.Background(), peer.URL+"/peer/decks/1/perf?key=x"); got != nil {
		t.Fatalf("fetchPerf used a redirect's answer: %q", got)
	}
	req, _ := http.NewRequest("GET", peer.URL+"/peer/decks/1/audio?key=x", nil)
	if resp, err := peerFileClient.Do(req); err == nil {
		resp.Body.Close()
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("%d peer requests went to the redirect target", n)
	}
}
