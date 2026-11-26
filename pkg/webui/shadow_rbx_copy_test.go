package webui

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A copy is published only once the source's analysis has arrived: while the source
// is still reading it, there is no copy (no download either), and the next try
// publishes the audio with it.
func TestShadowCopyWaitsForTheSourcesAnalysis(t *testing.T) {
	oldWait, oldTries := perfRetryWait, perfRetries
	perfRetryWait, perfRetries = time.Millisecond, 2
	defer func() { perfRetryWait, perfRetries = oldWait, oldTries }()
	var mu sync.Mutex
	audio, pending := 0, true
	valid := validTestEXPF(128)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/peer/decks/1/audio":
			audio++
			w.Write([]byte("AUDIO"))
		case pending:
			http.Error(w, "analysing", http.StatusServiceUnavailable)
		case r.URL.Query().Get("format") == "native":
			http.Error(w, "not analysed", http.StatusNotFound)
		default:
			w.Write(valid)
		}
	}))
	defer src.Close()
	s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.copyRetry = time.Millisecond
	u, _ := url.Parse(src.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())
	p := peerDeck{deckState: deckState{Deck: 1, Key: "k1", Ext: ".m4a", Name: "Song.m4a"}, ip: "127.0.0.1"}
	key := "peer:k1"
	try := func() string {
		s.shadow.mu.Lock()
		s.shadowFetchPeer(key, p)
		s.shadow.mu.Unlock()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			s.shadow.mu.Lock()
			f, busy := s.shadow.files[key], s.shadow.busy[key]
			s.shadow.mu.Unlock()
			if !busy {
				return f
			}
		}
		t.Fatal("copy never finished")
		return ""
	}
	if f := try(); f != "" {
		t.Fatalf("copy published without the source's analysis: %s", f)
	}
	s.shadow.mu.Lock()
	shown, err := s.shadow.copyShown(key)
	s.shadow.mu.Unlock()
	mu.Lock()
	n := audio
	pending = false
	mu.Unlock()
	if n != 0 || err != "" || shown != 0 {
		t.Fatalf("while the source analyses: %d downloads, shown %v %q", n, shown, err)
	}
	time.Sleep(5 * time.Millisecond) // past the retry backoff
	f := try()
	got, _ := os.ReadFile(f + ".perf")
	mu.Lock()
	n = audio
	mu.Unlock()
	if f == "" || !bytes.Equal(got, valid) || n != 1 {
		t.Fatalf("copy %q perf %d bytes after %d downloads", f, len(got), n)
	}
}

// The log says how each copy got its analysis, and refuses to publish when
// the source has no playable analysis (404).
func TestShadowCopyLogsHowItWasAnalysed(t *testing.T) {
	var mu sync.Mutex
	var out bytes.Buffer // written by the copies' goroutines
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return out.Write(p)
	}))
	defer log.SetOutput(os.Stderr)
	valid := validTestEXPF(128)
	analysed := false
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/peer/decks/1/audio":
			w.Write([]byte("AUDIO"))
		case analysed:
			w.Write(valid)
		default:
			http.Error(w, "not analysed", http.StatusNotFound)
		}
	}))
	defer src.Close()
	s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	defer s.Close()
	u, _ := url.Parse(src.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())
	copyOf := func(key string) (string, string) {
		s.shadow.mu.Lock()
		s.shadowFetchPeer(key, peerDeck{deckState: deckState{Deck: 1, Key: key, Ext: ".m4a", Name: "Song.m4a"}, ip: "127.0.0.1"})
		s.shadow.mu.Unlock()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			s.shadow.mu.Lock()
			f, busy := s.shadow.files[key], s.shadow.busy[key]
			var cerr string
			if cp := s.shadow.copies[key]; cp != nil {
				cerr = cp.err
			}
			s.shadow.mu.Unlock()
			if !busy {
				return f, cerr
			}
		}
		t.Fatal("copy never finished")
		return "", ""
	}
	if f, cerr := copyOf("bare"); f != "" || cerr != "no analysis" {
		t.Fatalf("unanalysed copy: file=%q err=%q, want empty file and \"no analysis\"", f, cerr)
	}
	mu.Lock()
	analysed = true
	mu.Unlock()
	if f, cerr := copyOf("replayed"); f == "" || cerr != "" {
		t.Fatalf("analysed copy: file=%q err=%q", f, cerr)
	}
	mu.Lock()
	got := out.String()
	mu.Unlock()
	for _, want := range []string{
		"shadow: the source gave no analysis (http 404)",
		`shadow: copy replayed "Song.m4a" analysis replay=true fill=false (not a player)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
