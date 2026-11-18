package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The receiver asks a source that says the analysis is coming (503) again, a few
// times, instead of loading the copy bare; a track without analysis (404) is not
// waited for.
func TestShadowFetchPerfWaitsForADelayedAnalysis(t *testing.T) {
	oldWait, oldTries := perfRetryWait, perfRetries
	perfRetryWait, perfRetries = 10*time.Millisecond, 5
	defer func() { perfRetryWait, perfRetries = oldWait, oldTries }()
	var calls int
	status := http.StatusServiceUnavailable
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, "analysing", status)
			return
		}
		w.Write([]byte("EXPF"))
	}))
	defer peer.Close()
	if b := fetchPerf(context.Background(), peer.URL); string(b) != "EXPF" || calls != 3 {
		t.Fatalf("delayed analysis: %q after %d requests", b, calls)
	}
	calls, status = 0, http.StatusNotFound
	if b := fetchPerf(context.Background(), peer.URL); b != nil || calls != 1 {
		t.Fatalf("no analysis: %q after %d requests", b, calls)
	}
}

// The analysis file is read only as a regular file of its own directory.
func TestShadowRekordboxAnalysisNoLink(t *testing.T) {
	r := newRbxShadowSource(t)
	id := r.row(7)
	r.publish(t, id, 500)
	other := filepath.Join(t.TempDir(), "other.bin")
	os.Rename(rbxAnalysisPath(id), other)
	os.Symlink(other, rbxAnalysisPath(id))
	if _, err := rbxReadAnalysisFile(rbxAnalysisPath(id)); err == nil {
		t.Fatal("followed a link")
	}
	if _, err := rbxReadAnalysisFile(other); err == nil {
		t.Fatal("read outside the analysis directory")
	}
}
