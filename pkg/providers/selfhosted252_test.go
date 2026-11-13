package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BH2-A8-23: Equal WebDAV basenames in different folders cannot overwrite each other.
func TestWebDAVEqualBasenamesDifferentFoldersNoOverwrite(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/music/Deep/track.mp3":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("DEEP-GROOVE-DATA"))
		case "/music/Tech/track.mp3":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("TECH-GROOVE-DATA"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cfg := NASConfig{
		Protocol: "webdav",
		URL:      ts.URL + "/music/",
		Enabled:  true,
	}
	p := NewSelfHostedProvider(cfg, ts.Client())
	ctx := context.Background()
	destDir := t.TempDir()

	path1, err := p.DownloadTrack(ctx, "/music/Deep/track.mp3", destDir)
	if err != nil {
		t.Fatalf("DownloadTrack 1 failed: %v", err)
	}
	if filepath.Base(path1) != "track.mp3" {
		t.Fatalf("unexpected basename for path1: %s", path1)
	}

	path2, err := p.DownloadTrack(ctx, "/music/Tech/track.mp3", destDir)
	if err != nil {
		t.Fatalf("DownloadTrack 2 failed: %v", err)
	}
	if filepath.Base(path2) != "track.mp3" {
		t.Fatalf("unexpected basename for path2: %s", path2)
	}

	if path1 == path2 {
		t.Fatalf("colliding download paths for different folders: %s", path1)
	}

	data1, err := os.ReadFile(path1)
	if err != nil {
		t.Fatalf("failed reading path1: %v", err)
	}
	if string(data1) != "DEEP-GROOVE-DATA" {
		t.Fatalf("path1 content corrupted or overwritten: %s", string(data1))
	}

	data2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatalf("failed reading path2: %v", err)
	}
	if string(data2) != "TECH-GROOVE-DATA" {
		t.Fatalf("path2 content mismatch: %s", string(data2))
	}
}

// BH2-A8-24: Cache growth is bounded and evicts oldest entries.
func TestSelfHostedCacheGrowthBounded(t *testing.T) {
	p := NewSelfHostedProvider(NASConfig{Protocol: "webdav", URL: "http://example.com/music"}, nil)
	p.maxCache = 3

	// 1. WebDAV cache bounding
	for i := 1; i <= 5; i++ {
		p.cacheWebDAVTrack(Track{
			ID:    fmt.Sprintf("tr-%d", i),
			Title: fmt.Sprintf("Title %d", i),
		})
	}

	p.mu.RLock()
	cacheLen := len(p.webdavCache)
	_, tr1Exists := p.webdavCache["tr-1"]
	_, tr2Exists := p.webdavCache["tr-2"]
	_, tr3Exists := p.webdavCache["tr-3"]
	_, tr4Exists := p.webdavCache["tr-4"]
	_, tr5Exists := p.webdavCache["tr-5"]
	p.mu.RUnlock()

	if cacheLen != 3 {
		t.Fatalf("expected webdavCache length 3, got %d", cacheLen)
	}
	if tr1Exists || tr2Exists {
		t.Fatalf("oldest entries tr-1 or tr-2 not evicted: tr1=%v, tr2=%v", tr1Exists, tr2Exists)
	}
	if !tr3Exists || !tr4Exists || !tr5Exists {
		t.Fatalf("expected tr-3, tr-4, tr-5 to remain in cache: tr3=%v, tr4=%v, tr5=%v", tr3Exists, tr4Exists, tr5Exists)
	}

	// 2. Plex part map bounding
	pPlex := NewSelfHostedProvider(NASConfig{Protocol: "plex", URL: "http://example.com:32400"}, nil)
	pPlex.maxCache = 3

	for i := 1; i <= 5; i++ {
		pPlex.setPlexPart(fmt.Sprintf("rk-%d", i), fmt.Sprintf("/part/%d", i))
	}

	pPlex.mu.RLock()
	plexLen := len(pPlex.plexPartMap)
	_, rk1Exists := pPlex.plexPartMap["rk-1"]
	_, rk2Exists := pPlex.plexPartMap["rk-2"]
	_, rk3Exists := pPlex.plexPartMap["rk-3"]
	_, rk4Exists := pPlex.plexPartMap["rk-4"]
	_, rk5Exists := pPlex.plexPartMap["rk-5"]
	pPlex.mu.RUnlock()

	if plexLen != 3 {
		t.Fatalf("expected plexPartMap length 3, got %d", plexLen)
	}
	if rk1Exists || rk2Exists {
		t.Fatalf("oldest plex parts rk-1 or rk-2 not evicted: rk1=%v, rk2=%v", rk1Exists, rk2Exists)
	}
	if !rk3Exists || !rk4Exists || !rk5Exists {
		t.Fatalf("expected rk-3, rk-4, rk-5 to remain in plexPartMap: rk3=%v, rk4=%v, rk5=%v", rk3Exists, rk4Exists, rk5Exists)
	}
}

// BH2-A8-25: Malformed URL input returns an error.
func TestSelfHostedMalformedURLInput(t *testing.T) {
	ctx := context.Background()
	destDir := t.TempDir()

	malformedBases := []string{
		"",
		"   ",
		"ftp://nas.local/music",
		"http://",
		"://invalid-url",
		"http://[::1/music",
	}

	for _, badBase := range malformedBases {
		// WebDAV
		wd := NewSelfHostedProvider(NASConfig{Protocol: "webdav", URL: badBase}, nil)
		if _, err := wd.Playlists(ctx); err == nil {
			t.Errorf("webdav.Playlists expected error for base %q", badBase)
		}
		if _, err := wd.PlaylistTracks(ctx, "root"); err == nil {
			t.Errorf("webdav.PlaylistTracks expected error for base %q", badBase)
		}
		if _, err := wd.Search(ctx, "test"); err == nil {
			t.Errorf("webdav.Search expected error for base %q", badBase)
		}
		if _, err := wd.DownloadTrack(ctx, "track.mp3", destDir); err == nil {
			t.Errorf("webdav.DownloadTrack expected error for base %q", badBase)
		}

		// Subsonic
		sb := NewSelfHostedProvider(NASConfig{Protocol: "subsonic", URL: badBase}, nil)
		if _, err := sb.Playlists(ctx); err == nil {
			t.Errorf("subsonic.Playlists expected error for base %q", badBase)
		}
		if _, err := sb.DownloadTrack(ctx, "123", destDir); err == nil {
			t.Errorf("subsonic.DownloadTrack expected error for base %q", badBase)
		}

		// Plex
		px := NewSelfHostedProvider(NASConfig{Protocol: "plex", URL: badBase}, nil)
		if _, err := px.Playlists(ctx); err == nil {
			t.Errorf("plex.Playlists expected error for base %q", badBase)
		}
		if _, err := px.DownloadTrack(ctx, "456", destDir); err == nil {
			t.Errorf("plex.DownloadTrack expected error for base %q", badBase)
		}
	}

	// Malformed ref / track ID on valid base
	validWD := NewSelfHostedProvider(NASConfig{Protocol: "webdav", URL: "http://nas.local/music/"}, nil)
	badRefs := []string{
		"://bad",
		"/music/%zz/song.mp3",
		"http://evil.com/music/song.mp3", // SSRF / host mismatch
		"ftp://nas.local/music/song.mp3", // scheme mismatch
	}
	for _, badRef := range badRefs {
		if _, err := validWD.DownloadTrack(ctx, badRef, destDir); err == nil {
			t.Errorf("webdav.DownloadTrack expected error for bad ref %q", badRef)
		}
	}

	validPlex := NewSelfHostedProvider(NASConfig{Protocol: "plex", URL: "http://nas.local:32400"}, nil)
	badPlexIDs := []string{
		"://bad",
		"http://evil.com/part/1",
	}
	for _, badID := range badPlexIDs {
		if _, err := validPlex.DownloadTrack(ctx, badID, destDir); err == nil {
			t.Errorf("plex.DownloadTrack expected error for bad ID %q", badID)
		}
	}
}

// BH2-A8-26: Extensionless Plex media uses response metadata.
func TestPlexExtensionlessMediaUsesResponseMetadata(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/parts/101":
			// Extensionless part with Content-Disposition filename
			w.Header().Set("Content-Disposition", `attachment; filename="studio_mix.wav"`)
			w.Write([]byte("WAV-STREAM"))
		case "/library/parts/102":
			// Extensionless part with Content-Type audio/flac
			w.Header().Set("Content-Type", "audio/flac")
			w.Write([]byte("FLAC-STREAM"))
		case "/library/parts/103":
			// Extensionless part with Content-Type audio/mpeg
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write([]byte("MP3-STREAM"))
		case "/library/parts/104":
			// Extensionless part with generic Content-Type (fallback to .mp3)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("OCTET-STREAM"))
		case "/library/parts/105/audio.aiff":
			// Part key that already has an extension
			w.Header().Set("Content-Type", "audio/aiff")
			w.Write([]byte("AIFF-STREAM"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cfg := NASConfig{
		Protocol: "plex",
		URL:      ts.URL,
		Enabled:  true,
	}
	p := NewSelfHostedProvider(cfg, ts.Client())
	ctx := context.Background()
	destDir := t.TempDir()

	// 1. Content-Disposition filename takes precedence
	p1, err := p.DownloadTrack(ctx, "/library/parts/101", destDir)
	if err != nil {
		t.Fatalf("DownloadTrack 101 failed: %v", err)
	}
	if !strings.HasSuffix(p1, ".wav") {
		t.Fatalf("expected .wav extension from Content-Disposition, got %s", p1)
	}

	// 2. Content-Type audio/flac provides .flac
	p2, err := p.DownloadTrack(ctx, "/library/parts/102", destDir)
	if err != nil {
		t.Fatalf("DownloadTrack 102 failed: %v", err)
	}
	if !strings.HasSuffix(p2, ".flac") {
		t.Fatalf("expected .flac extension from Content-Type, got %s", p2)
	}

	// 3. Content-Type audio/mpeg provides .mp3
	p3, err := p.DownloadTrack(ctx, "/library/parts/103", destDir)
	if err != nil {
		t.Fatalf("DownloadTrack 103 failed: %v", err)
	}
	if !strings.HasSuffix(p3, ".mp3") {
		t.Fatalf("expected .mp3 extension from Content-Type, got %s", p3)
	}

	// 4. Fallback when unknown Content-Type is .mp3
	p4, err := p.DownloadTrack(ctx, "/library/parts/104", destDir)
	if err != nil {
		t.Fatalf("DownloadTrack 104 failed: %v", err)
	}
	if !strings.HasSuffix(p4, ".mp3") {
		t.Fatalf("expected default .mp3 extension, got %s", p4)
	}

	// 5. Part with existing extension preserves it
	p5, err := p.DownloadTrack(ctx, "/library/parts/105/audio.aiff", destDir)
	if err != nil {
		t.Fatalf("DownloadTrack 105 failed: %v", err)
	}
	if !strings.HasSuffix(p5, ".aiff") {
		t.Fatalf("expected preserved .aiff extension, got %s", p5)
	}
}
