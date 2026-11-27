package webui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

// #335 round 13 (round-12 re-check, the request inventory): the preview clip was fetched
// with Go's default client, so a redirect to another host was sent the preview URL, its
// signature included, as the Referer. Now it follows the redirect without one.
func TestPreviewClipRedirectCarriesNoReferer(t *testing.T) {
	var clip bytes.Buffer
	if err := testaudio.FLAC(&clip, testaudio.Song(30)); err != nil {
		t.Fatal(err)
	}
	var referer atomic.Value
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referer.Store(r.Referer())
		w.Write(clip.Bytes())
	}))
	defer cdn.Close()
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/clip", http.StatusFound)
	}))
	defer service.Close()
	analyze := PreviewAnalyzer(resourceguard.New(resourceguard.Config{Disabled: true}))
	if got := analyze(context.Background(), service.URL+"/preview?hdnea=signed-r13"); got.BPM <= 0 {
		t.Fatalf("the preview behind a redirect was not measured: %+v", got)
	}
	if got, _ := referer.Load().(string); got != "" {
		t.Errorf("the redirect's target was sent the preview URL as the Referer: %q", got)
	}
}
