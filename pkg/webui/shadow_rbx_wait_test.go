package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
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
	var always bool
	status := http.StatusServiceUnavailable
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 || always {
			http.Error(w, "analysing", status)
			return
		}
		w.Write([]byte("EXPF"))
	}))
	defer peer.Close()
	if b, pending := fetchPerfPending(context.Background(), peer.URL); string(b) != "EXPF" || pending || calls != 3 {
		t.Fatalf("delayed analysis: %q pending %v after %d requests", b, pending, calls)
	}
	calls, status = 0, http.StatusNotFound
	if b, pending := fetchPerfPending(context.Background(), peer.URL); b != nil || pending || calls != 1 {
		t.Fatalf("no analysis: %q pending %v after %d requests", b, pending, calls)
	}
	calls, status, always = 0, http.StatusServiceUnavailable, true // 503 past the retries: still pending
	if b, pending := fetchPerfPending(context.Background(), peer.URL); b != nil || !pending || calls != perfRetries {
		t.Fatalf("still analysing: %q pending %v after %d requests", b, pending, calls)
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

// Each /perf request has its own deadline inside the bounded retry: a source that
// does not answer does not hold the copy for the whole copy timeout.
func TestShadowFetchPerfTimesOut(t *testing.T) {
	oldTimeout := perfRequestTimeout
	perfRequestTimeout = 50 * time.Millisecond
	defer func() { perfRequestTimeout = oldTimeout }()
	release := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer peer.Close()
	defer close(release)
	start := time.Now()
	if b, pending := fetchPerfPending(context.Background(), peer.URL); b != nil || pending || time.Since(start) > time.Second {
		t.Fatalf("slow /perf: %q pending %v after %v", b, pending, time.Since(start))
	}
}

// Once the source said 503, a timeout (or a broken connection) is no answer: the
// copy stays unpublished, it does not fall back to Engine's analysis.
func TestShadowFetchPerf503ThenTimeoutStaysPending(t *testing.T) {
	oldWait, oldTries, oldTimeout := perfRetryWait, perfRetries, perfRequestTimeout
	perfRetryWait, perfRetries, perfRequestTimeout = time.Millisecond, 3, 50*time.Millisecond
	defer func() { perfRetryWait, perfRetries, perfRequestTimeout = oldWait, oldTries, oldTimeout }()
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "analysing", http.StatusServiceUnavailable)
			return
		}
		<-r.Context().Done() // never answers in time
	}))
	defer peer.Close()
	if b, pending := fetchPerfPending(context.Background(), peer.URL); b != nil || !pending || int(calls.Load()) != perfRetries {
		t.Fatalf("503 then timeouts: %q pending %v after %d requests", b, pending, calls.Load())
	}
}
