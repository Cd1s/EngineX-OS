package deemixclient

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
)

// TestSaveAbortsOnStalledBody verifies S24: a CDN that sends 200 OK headers and
// then stalls mid-body must abort after mediaStallTimeout instead of hanging
// until the 30-minute download timeout.
func TestSaveAbortsOnStalledBody(t *testing.T) {
	prev := mediaStallTimeout
	mediaStallTimeout = 100 * time.Millisecond
	defer func() { mediaStallTimeout = prev }()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fLaC"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	dir := t.TempDir()
	c := &Client{DownloadDir: dir, AllowLossy: true}
	start := time.Now()
	_, err := c.Save(context.Background(), "stalled", srv.URL+"/track.flac")
	if err == nil {
		t.Fatal("expected Save to fail when CDN stalls mid-body")
	}
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Fatalf("Save hung past mediaStallTimeout: %s", elapsed)
	}
	leftover, _ := filepath.Glob(filepath.Join(dir, ".part-*"))
	if len(leftover) != 0 {
		t.Fatalf("stalled Save left temporary files: %v", leftover)
	}
}

// TestSaveSplitAbortsOnStalledBody verifies S24 for both 200 OK (no Range) and
// 206 Partial Content ranged transfers stalling mid-body.
func TestSaveSplitAbortsOnStalledBody(t *testing.T) {
	prev := mediaStallTimeout
	mediaStallTimeout = 100 * time.Millisecond
	defer func() { mediaStallTimeout = prev }()

	for _, status := range []int{http.StatusOK, http.StatusPartialContent} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusPartialContent {
					w.Header().Set("Content-Range", "bytes 0-4095/8192")
				}
				w.Header().Set("Content-Length", "4096")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("fLaC"))
				w.(http.Flusher).Flush()
				<-release
			}))
			defer srv.Close()
			defer close(release)

			c := &Client{DownloadDir: t.TempDir(), AllowLossy: true}
			start := time.Now()
			_, err := c.SaveSplit(context.Background(), "split-stall", srv.URL+"/track.flac")
			if err == nil {
				t.Fatal("expected SaveSplit to fail on stalled body")
			}
			if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
				t.Fatalf("SaveSplit hung past mediaStallTimeout: %s", elapsed)
			}
		})
	}
}

// TestSavePartsRetriesStalledSegment verifies S24: a stalled HLS/range segment
// times out on attempt 1 and recovers cleanly on retry without wedging.
func TestSavePartsRetriesStalledSegment(t *testing.T) {
	prev := mediaStallTimeout
	mediaStallTimeout = 100 * time.Millisecond
	defer func() { mediaStallTimeout = prev }()

	release := make(chan struct{})
	var attempts atomic.Int32
	payload := append([]byte("\x00\x00\x00\x18ftypisom\x00\x00\x00\x00isommp41"), bytes.Repeat([]byte{0x42}, 64)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:8])
			w.(http.Flusher).Flush()
			<-release
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	defer close(release)

	c := &Client{DownloadDir: t.TempDir(), AllowLossy: true}
	start := time.Now()
	dl, err := c.SaveParts(context.Background(), "hls-retry", "mp4", []string{srv.URL + "/seg0.m4s"})
	if err != nil {
		t.Fatalf("SaveParts did not recover after stalled attempt 1: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Fatalf("SaveParts retry took too long: %s", elapsed)
	}
	if dl == nil || dl.Path == "" {
		t.Fatal("expected finished download path")
	}
}

// TestEmbedCoverAbortsOnStalledBody verifies S24: stalled artwork bodies abort
// promptly after mediaStallTimeout without modifying or corrupting the audio file.
func TestEmbedCoverAbortsOnStalledBody(t *testing.T) {
	prev := mediaStallTimeout
	mediaStallTimeout = 100 * time.Millisecond
	defer func() { mediaStallTimeout = prev }()

	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xe0})
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	dir := t.TempDir()
	var audio bytes.Buffer
	if err := testaudio.FLAC(&audio, testaudio.Song(1)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "track.flac")
	if err := os.WriteFile(path, audio.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Client{DownloadDir: dir, Media: srv.Client()}
	start := time.Now()
	err := c.EmbedCover(context.Background(), path, srv.URL+"/cover.jpg")
	if err == nil {
		t.Fatal("expected EmbedCover to fail on stalled body")
	}
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Fatalf("EmbedCover hung past mediaStallTimeout: %s", elapsed)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, audio.Bytes()) {
		t.Fatal("stalled EmbedCover altered the audio file")
	}
}

// TestSaveAllowsSteadySlowTransferPastStallTimeout verifies S24: a legitimate
// slow transfer whose total duration exceeds mediaStallTimeout still completes
// cleanly as long as bytes keep arriving faster than the idle stall window.
func TestSaveAllowsSteadySlowTransferPastStallTimeout(t *testing.T) {
	prev := mediaStallTimeout
	mediaStallTimeout = 120 * time.Millisecond
	defer func() { mediaStallTimeout = prev }()

	var audio bytes.Buffer
	if err := testaudio.FLAC(&audio, testaudio.Song(1)); err != nil {
		t.Fatal(err)
	}
	raw := audio.Bytes()
	const chunks = 6
	step := max(1, len(raw)/chunks)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for off := 0; off < len(raw); off += step {
			end := min(len(raw), off+step)
			if _, err := w.Write(raw[off:end]); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			if end < len(raw) {
				time.Sleep(40 * time.Millisecond) // total ~240ms > 120ms stall timeout
			}
		}
	}))
	defer srv.Close()

	c := &Client{DownloadDir: t.TempDir(), AllowLossy: true}
	dl, err := c.Save(context.Background(), "steady-slow", srv.URL+"/slow.flac")
	if err != nil || dl == nil || dl.Path == "" {
		t.Fatalf("steady slow download should succeed: dl=%+v err=%v", dl, err)
	}
}
