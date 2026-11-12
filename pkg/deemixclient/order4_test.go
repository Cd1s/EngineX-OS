package deemixclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func TestDownloadSurfacesQueueErrorDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(QueueStatus{
			ID:    "job-arl",
			State: "error",
			Error: "deezer: ARL invalid or expired",
		})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, PollInterval: time.Millisecond}
	_, err := c.Download(context.Background(), providers.Track{ID: "42", Provider: "deezer"}, nil)
	if err == nil || !strings.Contains(err.Error(), "ARL invalid or expired") {
		t.Fatalf("expected queue error detail to be preserved, got: %v", err)
	}

	leakSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(QueueStatus{
			ID:    "job-url",
			State: "error",
			Error: "cdn failure on https://cdns-proxy.dzcdn.net/mobile/1/secret-token",
		})
	}))
	defer leakSrv.Close()
	_, err = (&Client{BaseURL: leakSrv.URL, PollInterval: time.Millisecond}).Download(context.Background(), providers.Track{ID: "42", Provider: "deezer"}, nil)
	if err == nil || strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "cdn failure") {
		t.Fatalf("expected URL redaction with preserved error summary, got: %v", err)
	}

	dirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"result":false,"error":"ARL cookie rejected"}`))
	}))
	defer dirSrv.Close()
	_, err = (&Client{Mode: "directory", BaseURL: dirSrv.URL, DownloadDir: t.TempDir(), PollInterval: time.Millisecond}).Download(
		context.Background(), providers.Track{ID: "42", Provider: "deezer"}, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "ARL cookie rejected") {
		t.Fatalf("expected directory submission error detail, got: %v", err)
	}
}

func TestSaveAndSavePartsRemoveStaleFormatExtensionsAndTolerateFATChmod(t *testing.T) {
	frames := mp3Frames(14, 4)
	mp4Init := []byte("\x00\x00\x00\x18ftypiso5rest")
	jpeg := append([]byte{0xff, 0xd8, 0xff}, bytes.Repeat([]byte("art"), 50)...)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mp3":
			w.Write(frames)
		case "/mp4":
			w.Write(mp4Init)
		case "/art.jpg":
			w.Write(jpeg)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := &Client{DownloadDir: dir, AllowLossy: true, Media: srv.Client()}

	origChmod := chmodFile
	t.Cleanup(func() { chmodFile = origChmod })

	// Simulate FAT32/exFAT SD card mounts returning EPERM or EOPNOTSUPP on chmod.
	chmodErr := error(syscall.EPERM)
	chmodFile = func(string, os.FileMode) error { return chmodErr }

	// Pre-existing .mp3 must be removed when SaveParts writes a newer .mp4 for the same ID.
	staleMP3 := filepath.Join(dir, "soundcloud-99.mp3")
	if err := os.WriteFile(staleMP3, frames, 0o644); err != nil {
		t.Fatal(err)
	}
	dl, err := c.SaveParts(context.Background(), "soundcloud-99", "mp4", []string{srv.URL + "/mp4"})
	if err != nil {
		t.Fatalf("SaveParts with FAT EPERM chmod: %v", err)
	}
	if _, err := os.Stat(staleMP3); !os.IsNotExist(err) {
		t.Fatalf("SaveParts left stale .mp3 behind: %v", err)
	}
	if got := c.Cached("soundcloud-99"); got == nil || got.Path != dl.Path {
		t.Fatalf("Cached returned %+v, want %q", got, dl.Path)
	}

	chmodErr = syscall.EOPNOTSUPP
	staleMP4 := filepath.Join(dir, "netease-99.mp4")
	staleOGG := filepath.Join(dir, "netease-99.ogg")
	if err := os.WriteFile(staleMP4, mp4Init, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleOGG, []byte("OggS-stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved, err := c.Save(context.Background(), "netease-99", srv.URL+"/mp3")
	if err != nil {
		t.Fatalf("Save with FAT EOPNOTSUPP chmod: %v", err)
	}
	for _, old := range []string{staleMP4, staleOGG} {
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatalf("Save left stale %s behind: %v", old, err)
		}
	}
	if got := c.Cached("netease-99"); got == nil || got.Path != saved.Path {
		t.Fatalf("Cached returned %+v, want %q", got, saved.Path)
	}

	if _, err := c.SaveSplit(context.Background(), "soundcloud-split", srv.URL+"/mp3"); err != nil {
		t.Fatalf("SaveSplit with FAT EOPNOTSUPP chmod: %v", err)
	}
	if err := c.EmbedCover(context.Background(), saved.Path, srv.URL+"/art.jpg"); err != nil {
		t.Fatalf("EmbedCover with FAT EOPNOTSUPP chmod: %v", err)
	}

	// Non-permission I/O errors from chmod must still be propagated.
	chmodErr = syscall.EIO
	if _, err := c.Save(context.Background(), "netease-ioerr", srv.URL+"/mp3"); err == nil {
		t.Fatal("expected EIO from chmod to fail Save")
	}
}

func TestMatchAcceptsISRCEditionSuffixesAndOriginalMix(t *testing.T) {
	// Beatport "(Original Mix)" vs Deezer plain title with identical ISRC and duration.
	bpOrig := providers.Track{Title: "Strobe (Original Mix)", Artist: "deadmau5", ISRC: "CAUL30900158", DurationMS: 637000, Provider: "beatport"}
	dzPlain := providers.Track{ID: "100", Title: "Strobe", Artist: "deadmau5", ISRC: "CAUL30900158", DurationMS: 637500, Provider: "deezer"}
	if got, err := Match(bpOrig, []providers.Track{dzPlain}); err != nil || got.ID != "100" {
		t.Fatalf("ISRC Original Mix rejected: got=%+v err=%v", got, err)
	}

	// Non-ISRC fallback for "(Original Mix)" vs plain title.
	bpNoISRC := bpOrig
	bpNoISRC.ISRC = ""
	dzNoISRC := dzPlain
	dzNoISRC.ISRC = ""
	if got, err := Match(bpNoISRC, []providers.Track{dzNoISRC}); err != nil || got.ID != "100" {
		t.Fatalf("fallback Original Mix rejected: got=%+v err=%v", got, err)
	}

	// Remaster suffix on Spotify vs plain title on Deezer with identical ISRC.
	spRemaster := providers.Track{Title: "Heroes (2017 Remaster)", Artist: "David Bowie", ISRC: "GBARL1700786", DurationMS: 371000, Provider: "spotify"}
	dzHeroes := providers.Track{ID: "101", Title: "Heroes", Artist: "David Bowie", ISRC: "GBARL1700786", DurationMS: 371200, Provider: "deezer"}
	if got, err := Match(spRemaster, []providers.Track{dzHeroes}); err != nil || got.ID != "101" {
		t.Fatalf("ISRC Remaster rejected: got=%+v err=%v", got, err)
	}

	// Extended Mix on Beatport vs Deezer track whose title omits title_version, same ISRC & duration.
	bpExt := providers.Track{Title: "Opus (Extended Mix)", Artist: "Eric Prydz", ISRC: "GB5KW1501234", DurationMS: 543000, Provider: "beatport"}
	dzOpus := providers.Track{ID: "102", Title: "Opus", Artist: "Eric Prydz", ISRC: "GB5KW1501234", DurationMS: 543000, Provider: "deezer"}
	if got, err := Match(bpExt, []providers.Track{dzOpus}); err != nil || got.ID != "102" {
		t.Fatalf("ISRC Extended Mix vs plain Deezer title rejected: got=%+v err=%v", got, err)
	}

	// Conflicting explicit versions or alternate vocal/live recordings must still be rejected even if ISRC collides.
	for _, badTitle := range []string{"Opus (Radio Edit)", "Opus (Original Mix)", "Opus (Instrumental)", "Opus (Live)", "Opus (Four Tet Remix)"} {
		bad := dzOpus
		bad.Title = badTitle
		if got, err := Match(bpExt, []providers.Track{bad}); err == nil {
			t.Fatalf("matched conflicting edition %q: %+v", badTitle, got)
		}
	}
}
