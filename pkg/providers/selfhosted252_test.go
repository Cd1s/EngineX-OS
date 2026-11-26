package providers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/webui"
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

	cfg := providers.NASConfig{
		Protocol: "webdav",
		URL:      ts.URL + "/music/",
		Enabled:  true,
	}
	p := providers.NewSelfHostedProvider(cfg, ts.Client())
	ctx := context.Background()
	destDir := t.TempDir()

	path1, err := p.DownloadTrack(ctx, "/music/Deep/track.mp3", destDir)
	if err != nil {
		t.Fatalf("DownloadTrack 1 failed: %v", err)
	}
	if filepath.Ext(path1) != ".mp3" {
		t.Fatalf("unexpected basename for path1: %s", path1)
	}

	path2, err := p.DownloadTrack(ctx, "/music/Tech/track.mp3", destDir)
	if err != nil {
		t.Fatalf("DownloadTrack 2 failed: %v", err)
	}
	if filepath.Ext(path2) != ".mp3" {
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
	const limit = 5000
	var dav strings.Builder
	dav.WriteString(`<multistatus xmlns="DAV:">`)
	for i := 1; i <= limit+2; i++ {
		fmt.Fprintf(&dav, `<response><href>/music/%d.mp3</href><propstat><prop><displayname>Track %d</displayname></prop></propstat></response>`, i, i)
	}
	dav.WriteString(`</multistatus>`)
	var plex strings.Builder
	plex.WriteString(`{"MediaContainer":{"Metadata":[`)
	for i := 1; i <= limit/2+2; i++ {
		if i > 1 {
			plex.WriteByte(',')
		}
		fmt.Fprintf(&plex, `{"ratingKey":"rk-%d","Media":[{"Part":[{"key":"/part/%d.mp3"}]}]}`, i, i)
	}
	plex.WriteString(`]}}`)
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PROPFIND":
			fmt.Fprint(w, dav.String())
		case r.URL.Path == "/playlists/all/items":
			fmt.Fprint(w, plex.String())
		case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
			lookups.Add(1)
			http.NotFound(w, r)
		default:
			fmt.Fprint(w, "media")
		}
	}))
	defer srv.Close()
	p := providers.NewSelfHostedProvider(providers.NASConfig{Protocol: "webdav", URL: srv.URL + "/music/"}, srv.Client())
	if _, err := p.PlaylistTracks(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	tracks, err := p.Search(context.Background(), "")
	if err != nil || len(tracks) != limit {
		t.Fatalf("bounded WebDAV search: count=%d error=%v", len(tracks), err)
	}
	ids := make(map[string]bool)
	for _, tr := range tracks {
		ids[tr.ID] = true
	}
	if ids["/music/1.mp3"] || ids["/music/2.mp3"] || !ids["/music/3.mp3"] || !ids["/music/5002.mp3"] {
		t.Fatal("WebDAV FIFO eviction changed")
	}
	p = providers.NewSelfHostedProvider(providers.NASConfig{Protocol: "plex", URL: srv.URL}, srv.Client())
	if _, err := p.PlaylistTracks(context.Background(), "all"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownloadTrack(context.Background(), "rk-1", t.TempDir()); err == nil || lookups.Load() != 1 {
		t.Fatalf("old Plex key was not evicted: error=%v lookups=%d", err, lookups.Load())
	}
	for _, id := range []string{"rk-3", "rk-2502"} {
		if _, err := p.DownloadTrack(context.Background(), id, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	if lookups.Load() != 1 {
		t.Fatal("retained Plex keys unexpectedly required metadata lookup")
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
		wd := providers.NewSelfHostedProvider(providers.NASConfig{Protocol: "webdav", URL: badBase}, nil)
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
		sb := providers.NewSelfHostedProvider(providers.NASConfig{Protocol: "subsonic", URL: badBase}, nil)
		if _, err := sb.Playlists(ctx); err == nil {
			t.Errorf("subsonic.Playlists expected error for base %q", badBase)
		}
		if _, err := sb.DownloadTrack(ctx, "123", destDir); err == nil {
			t.Errorf("subsonic.DownloadTrack expected error for base %q", badBase)
		}

		// Plex
		px := providers.NewSelfHostedProvider(providers.NASConfig{Protocol: "plex", URL: badBase}, nil)
		if _, err := px.Playlists(ctx); err == nil {
			t.Errorf("plex.Playlists expected error for base %q", badBase)
		}
		if _, err := px.DownloadTrack(ctx, "456", destDir); err == nil {
			t.Errorf("plex.DownloadTrack expected error for base %q", badBase)
		}
	}

	// Malformed ref / track ID on valid base
	validWD := providers.NewSelfHostedProvider(providers.NASConfig{Protocol: "webdav", URL: "http://nas.local/music/"}, nil)
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

	validPlex := providers.NewSelfHostedProvider(providers.NASConfig{Protocol: "plex", URL: "http://nas.local:32400"}, nil)
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

	cfg := providers.NASConfig{
		Protocol: "plex",
		URL:      ts.URL,
		Enabled:  true,
	}
	p := providers.NewSelfHostedProvider(cfg, ts.Client())
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

func selfHosted252Provider(t *testing.T, basePrefix string) *providers.SelfHostedProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(bytes.Repeat([]byte(r.URL.Path+"\n"), 64))
	}))
	t.Cleanup(srv.Close)
	return providers.NewSelfHostedProvider(providers.NASConfig{Protocol: "webdav", URL: basePrefix + srv.URL + "/music/", Enabled: true}, srv.Client())
}

func TestSelfHosted252DistinctFolders(t *testing.T) {
	p := selfHosted252Provider(t, "")
	for _, ids := range [][2]string{
		{"/music/Deep/track.mp3", "/Deep/track.mp3"},
		{"/music/a%252Fb/track.mp3", "/music/a/b/track.mp3"},
	} {
		dir := t.TempDir()
		first, err := p.DownloadTrack(context.Background(), ids[0], dir)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(first)
		second, err := p.DownloadTrack(context.Background(), ids[1], dir)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(first)
		if first == second || !bytes.Equal(before, after) {
			t.Errorf("distinct URLs %q and %q collide and overwrite at %s", ids[0], ids[1], first)
		}
	}
}

func TestSelfHosted252PercentFilename(t *testing.T) {
	p := selfHosted252Provider(t, "")
	if _, err := p.DownloadTrack(context.Background(), "/music/Deep/100%25.mp3", t.TempDir()); err != nil {
		t.Fatalf("valid percent-encoded filename rejected: %v", err)
	}
}

func TestSelfHosted252WhitespaceBase(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("URL handling panicked instead of returning a result: %v", r)
		}
	}()
	p := selfHosted252Provider(t, " ")
	if _, err := p.DownloadTrack(context.Background(), "/music/Deep/track.mp3", t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestSelfHosted252SymlinkBoundary(t *testing.T) {
	p := selfHosted252Provider(t, "")
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "Deep")); err != nil {
		t.Fatal(err)
	}
	got, err := p.DownloadTrack(context.Background(), "/music/Deep/track.mp3", dir)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := filepath.EvalSymlinks(got)
	if err != nil || filepath.Dir(actual) != dir {
		t.Fatalf("download escaped cache: %s, error=%v", actual, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "track.mp3")); !os.IsNotExist(err) {
		t.Fatalf("outside path was written: %v", err)
	}
}

func TestSelfHosted252DiskBudget(t *testing.T) {
	p := selfHosted252Provider(t, "")
	dir := t.TempDir()
	first, err := p.DownloadTrack(context.Background(), "/music/A/101.mp3", dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.DownloadTrack(context.Background(), "/music/B/102.mp3", dir)
	if err != nil {
		t.Fatal(err)
	}
	c := &deemixclient.Client{DownloadDir: dir, MaxCache: 1}
	c.Trim(second)
	if _, err := os.Stat(first); err == nil {
		t.Errorf("completed unprotected NAS file remains outside Trim's budget: %s", first)
	}
}

// Exercise the source index through Jobs so the test covers a real restart.
func TestSelfHosted252RestartCache(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "new_flat"
		if legacy {
			name = "legacy_flat"
		}
		t.Run(name, func(t *testing.T) {
			dir, ui := t.TempDir(), t.TempDir()
			tr := providers.Track{Provider: "webdav", ID: "/music/Deep/synths.mp3"}
			client := &deemixclient.Client{DownloadDir: dir, AllowLossy: true}
			var saved string
			if legacy {
				saved = filepath.Join(dir, "synths.mp3")
				if err := os.WriteFile(saved, selfHosted252MP3(), 0600); err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(map[string]string{"webdav:" + tr.ID: "synths.mp3"})
				if err := os.WriteFile(filepath.Join(dir, ".sources.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "audio/mpeg")
					w.Write(selfHosted252MP3())
				}))
				defer srv.Close()
				data, _ := json.Marshal(providers.NASConfig{Protocol: "webdav", URL: srv.URL + "/music/", Enabled: true})
				if err := os.WriteFile(filepath.Join(ui, "nas.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				jobs := webui.NewJobs(webui.Pipeline{Catalog: selfHosted252Catalog{}, Deemix: client})
				defer jobs.Close()
				server := webui.New(webui.Config{UIDir: ui, StateDir: t.TempDir(), HTTP: srv.Client()}, jobs)
				defer server.Close()
				var err error
				saved, err = jobs.Fetch(context.Background(), "", tr, false)
				if err != nil {
					t.Fatal(err)
				}
			}
			restarted := webui.NewJobs(webui.Pipeline{Deemix: &deemixclient.Client{DownloadDir: dir, AllowLossy: true}})
			defer restarted.Close()
			if got := restarted.File(tr); got != saved {
				t.Fatalf("cache lost after restart: got %q, want %q", got, saved)
			}
		})
	}
}

type selfHosted252Catalog struct{}

func (selfHosted252Catalog) ByISRC(context.Context, string) (providers.Track, error) {
	return providers.Track{}, errors.New("unexpected catalog lookup")
}
func (selfHosted252Catalog) Search(context.Context, string) ([]providers.Track, error) {
	return nil, errors.New("unexpected catalog lookup")
}

func selfHosted252MP3() []byte {
	frame := make([]byte, 144*128000/44100)
	copy(frame, []byte{0xff, 0xfb, 0x90, 0})
	return bytes.Repeat(frame, 120)
}

func TestPlexCacheNamesUseFullPartKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/flac")
		fmt.Fprint(w, r.URL.Path)
	}))
	defer srv.Close()
	cfg := providers.NASConfig{Protocol: "plex", URL: srv.URL}
	p := providers.NewSelfHostedProvider(cfg, srv.Client())
	dir := t.TempDir()
	first, err := p.DownloadTrack(context.Background(), "/library/parts/1/file", dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.DownloadTrack(context.Background(), "/library/parts/2/file", dir)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || filepath.Dir(first) != dir || filepath.Dir(second) != dir || filepath.Ext(first) != ".flac" || filepath.Ext(second) != ".flac" {
		t.Fatalf("Plex parts did not get distinct flat FLAC names: %s, %s", first, second)
	}
	data, err := os.ReadFile(first)
	if err != nil || string(data) != "/library/parts/1/file" {
		t.Fatalf("Plex file overwritten: %q, error=%v", data, err)
	}
	again, err := providers.NewSelfHostedProvider(cfg, srv.Client()).DownloadTrack(context.Background(), "/library/parts/1/file", dir)
	if err != nil || again != first {
		t.Fatalf("unstable Plex cache name: %s, error=%v", again, err)
	}
}
