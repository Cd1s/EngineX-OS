package webui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestShadowPostsRecoverAfterClockRollback(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	s.shadow.beacon = false
	t.Cleanup(s.Close)
	s.shadow.ownPostAt = time.Now().Add(time.Minute).UnixMilli()
	for position := 1; position <= 2; position++ {
		s.decksPost(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/sc5000/decks",
			strings.NewReader(fmt.Sprintf(`[{"deck":1,"positionMs":%d,"at":%d}]`, position, time.Now().UnixMilli()))))
		s.shadow.mu.Lock()
		own := append([]deckState(nil), s.shadow.own...)
		s.shadow.mu.Unlock()
		if len(own) != 1 || own[0].PositionMS != float64(position) {
			t.Fatalf("post %d after rollback was discarded: %+v", position, own)
		}
	}
}

func TestShadowFetchCleansExpiredCopiesBeforeENOSPC(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	dir := s.shadowDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"old", "used", "busy"} {
		path := filepath.Join(dir, key+".mp3")
		for _, name := range []string{path, path + ".perf"} {
			if err := os.WriteFile(name, []byte("copy"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		s.shadow.files[key], s.shadow.used[key] = path, time.Now().Add(-3*time.Minute)
	}
	s.shadow.used["used"], s.shadow.busy["busy"] = time.Now(), true
	done := make(chan bool, 1)
	s.shadow.mu.Lock()
	s.shadowFetch("new", func(context.Context, *shadowCopy) (string, error) {
		_, err := os.Stat(filepath.Join(dir, "old.mp3"))
		_, perfErr := os.Stat(filepath.Join(dir, "old.mp3.perf"))
		done <- os.IsNotExist(err) && os.IsNotExist(perfErr)
		return "", syscall.ENOSPC
	})
	s.shadow.mu.Unlock()
	if !<-done {
		t.Error("expired copies still occupy space when the next download starts")
	}
	for _, key := range []string{"used", "busy"} {
		if _, err := os.Stat(filepath.Join(dir, key+".mp3")); err != nil {
			t.Errorf("protected copy %s: %v", key, err)
		}
	}
}

func TestShadowCopyAudioLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		length    int64
		wantError bool
	}{
		{"known at limit", "12345678", 8, false},
		{"unknown at limit", "12345678", -1, false},
		{"known over limit", "123456789", 9, true},
		{"unknown over limit", "123456789", -1, true},
		{"unknown far over limit", strings.Repeat("x", 100), -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			resp := &http.Response{ContentLength: tc.length, Body: io.NopCloser(strings.NewReader(tc.body))}
			err := copyShadowAudio(&dst, resp, 8)
			if (err != nil) != tc.wantError {
				t.Fatalf("copy error: %v", err)
			}
			if dst.Len() > 9 || (!tc.wantError && dst.String() != tc.body) || (tc.length > 8 && dst.Len() != 0) {
				t.Fatalf("unexpected copy: %q", dst.String())
			}
		})
	}
}

func TestShadowPeerRejectsOversizedAudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1073741825")
		fmt.Fprint(w, "tiny")
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	s.shadow.peerPort, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	s.shadow.mu.Lock()
	s.shadowFetchPeer("oversize", peerDeck{deckState: deckState{Deck: 2, Key: "oversize", Ext: ".mp3"}, ip: u.Hostname()})
	s.shadow.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.shadow.mu.Lock()
		busy, published := s.shadow.busy["oversize"], s.shadow.files["oversize"] != ""
		s.shadow.mu.Unlock()
		if !busy {
			if published {
				t.Fatal("oversized audio was published as a successful copy")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("oversized audio was not rejected promptly")
		}
		time.Sleep(time.Millisecond)
	}
	if err := filepath.WalkDir(s.shadowDir(), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			t.Errorf("failed copy left a file: %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
