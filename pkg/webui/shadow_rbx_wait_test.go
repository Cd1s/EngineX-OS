package webui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
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

// A cached rekordbox Link audio file gets a new sidecar path for each edited
// analysis generation, without downloading the audio again.
func TestShadowPerfGenRefreshesWithoutDownload(t *testing.T) {
	var mu sync.Mutex
	audio, perf := 0, []byte(nil)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/peer/decks/1/audio":
			audio++
			_, _ = w.Write([]byte("AUDIO"))
		case "/peer/decks/1/perf":
			if perf == nil || r.URL.Query().Get("format") == "native" {
				http.Error(w, "not analysed", http.StatusNotFound)
				return
			}
			_, _ = w.Write(perf)
		}
	}))
	defer src.Close()
	s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	defer s.Close()
	u, _ := url.Parse(src.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())
	p := peerDeck{deckState: deckState{Deck: 1, Key: "k1", Ext: ".m4a", Name: "Song.m4a"}, ip: "127.0.0.1"}
	key := "peer:k1"
	s.shadow.mu.Lock()
	p.at = time.Now()
	s.shadow.peerDecks[1] = p
	s.shadow.mu.Unlock()
	first := ""
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, d := range s.shadowComposeLegacy("", time.Now()) {
			if d.Path != "" {
				first = d.Path
			}
		}
		if first != "" {
			break
		}
	}
	if first == "" {
		t.Fatal("no initial shadow copy")
	}
	if _, err := os.Stat(first + ".perf"); !os.IsNotExist(err) {
		t.Fatal("perf unexpectedly exists before the source analysis")
	}
	inPeerCache := regexp.MustCompile(`/shadow-v3/peer-[0-9a-f]{1,16}/[^/]+$`)
	for i, want := range [][]byte{[]byte("EXPF1"), []byte("EXPF2")} {
		mu.Lock()
		perf = want
		mu.Unlock()
		p.PerfGen = "g" + strconv.Itoa(i+1)
		s.shadow.mu.Lock()
		p.at = time.Now()
		s.shadow.peerDecks[1] = p
		s.shadow.mu.Unlock()
		old := first
		first = ""
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			for _, d := range s.shadowComposeLegacy("", time.Now()) {
				if d.Path != "" && d.Path != old {
					first = d.Path
				}
			}
			if first != "" {
				break
			}
		}
		if first == "" {
			t.Fatal("no refreshed shadow copy")
		}
		got, _ := os.ReadFile(first + ".perf")
		body, _ := os.ReadFile(first)
		if !bytes.Equal(got, want) || string(body) != "AUDIO" || !inPeerCache.MatchString(filepath.ToSlash(first)) {
			t.Fatalf("refreshed copy %s: perf %q audio %q", first, got, body)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatal("old shadow copy remains")
		}
		s.shadow.mu.Lock()
		gen := s.shadow.copyGens[key]
		s.shadow.mu.Unlock()
		mu.Lock()
		n := audio
		mu.Unlock()
		if gen != p.PerfGen || n != 1 {
			t.Fatalf("generation %q, audio downloads %d", gen, n)
		}
	}
}
