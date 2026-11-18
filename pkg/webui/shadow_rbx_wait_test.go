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
	if b := fetchPerf(context.Background(), peer.URL); b != nil || time.Since(start) > time.Second {
		t.Fatalf("slow /perf: %q after %v", b, time.Since(start))
	}
}

// A rekordbox Link copy whose analysis comes after its audio (or changes with an
// edit in rekordbox) gets it from the source's PerfGen: the same audio under a new
// path (Engine keeps the analysis of a path it loaded) with the new sidecars, and no
// second download.
func TestShadowPerfGenRefreshesWithoutDownload(t *testing.T) {
	var mu sync.Mutex
	audio, perf := 0, []byte(nil)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/peer/decks/1/audio":
			audio++
			w.Write([]byte("AUDIO"))
		case "/peer/decks/1/perf":
			if perf == nil || r.URL.Query().Get("format") == "native" {
				http.Error(w, "not analysed", http.StatusNotFound)
				return
			}
			w.Write(perf)
		}
	}))
	defer src.Close()
	s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	defer s.Close()
	u, _ := url.Parse(src.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())
	p := peerDeck{deckState: deckState{Deck: 1, Key: "k1", Ext: ".m4a", Name: "Song.m4a"}, ip: "127.0.0.1"}
	key := "peer:k1"
	copyAt := func(old string) string {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			s.shadow.mu.Lock()
			f, busy := s.shadow.files[key], s.shadow.busy[key]
			s.shadow.mu.Unlock()
			if f != "" && f != old && !busy {
				return f
			}
		}
		t.Fatal("no copy")
		return ""
	}
	// The source's analysis is not there yet: the audio is copied without it.
	s.shadow.mu.Lock()
	s.shadowFetchPeer(key, p)
	s.shadow.mu.Unlock()
	first := copyAt("")
	if _, err := os.Stat(first + ".perf"); !os.IsNotExist(err) {
		t.Fatal("perf without analysis")
	}
	inPeerCache := regexp.MustCompile(`/shadow-v3/peer-[0-9a-f]{1,16}/[^/]+$`)
	for i, want := range [][]byte{[]byte("EXPF1"), []byte("EXPF2")} {
		mu.Lock()
		perf = want
		mu.Unlock()
		p.PerfGen = "g" + strconv.Itoa(i+1)
		s.shadow.mu.Lock()
		if s.shadow.copyGens[key] == p.PerfGen {
			t.Fatal("the copy claims the new analysis already")
		}
		s.shadowRefreshPeerPerf(key, p, s.shadow.files[key])
		s.shadow.mu.Unlock()
		old := first
		first = copyAt(old)
		got, _ := os.ReadFile(first + ".perf")
		body, _ := os.ReadFile(first)
		if !bytes.Equal(got, want) || string(body) != "AUDIO" || !inPeerCache.MatchString(filepath.ToSlash(first)) {
			t.Fatalf("refreshed copy %s: perf %q audio %q", first, got, body)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatal("the old copy stays")
		}
		s.shadow.mu.Lock()
		gen := s.shadow.copyGens[key]
		s.shadow.mu.Unlock()
		mu.Lock()
		n := audio
		mu.Unlock()
		if gen != p.PerfGen || n != 1 {
			t.Fatalf("gen %q, %d audio downloads", gen, n)
		}
	}
}
