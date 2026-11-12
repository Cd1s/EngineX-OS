package deemixclient

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCachedOgg(t *testing.T) {
	for _, tc := range []struct {
		name string
		data func(*testing.T) []byte
		want Quality
	}{
		{"inspected", func(t *testing.T) []byte {
			b := oggIdentificationPage(t)
			binary.LittleEndian.PutUint32(b[48:], 160000)
			return b
		}, Quality{Label: "[160k]", Format: "ogg", SampleRate: 16000, Bitrate: 160000}},
		{"offline_fixture", func(*testing.T) []byte { return []byte("synthetic offline audio") }, Quality{Label: "[320k]", Format: "ogg", SampleRate: 44100}},
		{"incomplete_vorbis", func(*testing.T) []byte { return []byte("OggS") }, Quality{Label: "[320k]", Format: "ogg", SampleRate: 44100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			name := "spotify-4uLU6hMCjMI75M1A2tKUQC"
			path := filepath.Join(dir, name+".ogg")
			if err := os.WriteFile(path, tc.data(t), 0600); err != nil {
				t.Fatal(err)
			}
			// Native Ogg cache lookup does not require Deemix's MP3 fallback flag.
			dl := (&Client{DownloadDir: dir}).Cached(name)
			if dl == nil || dl.Path != path || dl.Quality != tc.want {
				t.Fatalf("cache=%+v, want %+v", dl, tc.want)
			}
		})
	}
	for _, name := range []string{"missing", "empty", "directory"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name+".ogg")
			if name == "empty" {
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if dl := (&Client{DownloadDir: dir}).Cached(name); dl != nil {
				t.Fatalf("invalid cache entry returned: %+v", dl)
			}
		})
	}
}

func TestEvictableSpotifyOgg(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"spotify-4uLU6hMCjMI75M1A2tKUQC.ogg", true},
		{"spotify-ABCxyz012.ogg", true},
		{"123.mp3", true}, {"netease-123.flac", true}, {"soundcloud-123.mp4", true}, {"beatport-123.flac", true},
		{"my-song.ogg", false}, {"123.ogg", false}, {"spotify-.ogg", false}, {"spotify-abc-def.ogg", false},
		{"spotify-abc.ogg.part", false}, {"prefix-spotify-abc.ogg", false}, {"spotify-abc.mp3", true}, {"spotify-abc.flac", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evictable.MatchString(tc.name); got != tc.want {
				t.Fatalf("match=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestTrimEvictsSpotifyOggAndProtectsCurrent(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, 100), 0600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}
	old := write("spotify-4uLU6hMCjMI75M1A2tKUQC.ogg", 3*time.Hour)
	keep := write("spotify-new123.ogg", 4*time.Hour)
	foreign := write("personal.ogg", 5*time.Hour)
	newer := write("netease-123.flac", time.Hour)
	(&Client{DownloadDir: dir, MaxCache: 300}).Trim(keep)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old Spotify cache not evicted: %v", err)
	}
	for _, path := range []string{keep, foreign, newer} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected/newer file removed: %s, %v", path, err)
		}
	}
}

func TestSpotifyCacheIgnoresLegacyCrossServiceFiles(t *testing.T) {
	dir := t.TempDir()
	client := &Client{DownloadDir: dir, AllowLossy: true}
	for _, ext := range []string{".flac", ".mp3", ".mp4"} {
		if err := os.WriteFile(filepath.Join(dir, "spotify-abc"+ext), []byte("old-download"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := client.Cached("spotify-abc"); got != nil {
		t.Fatal("legacy Spotify cross-service cache was reused")
	}
	if err := os.WriteFile(filepath.Join(dir, "spotify-abc.ogg"), []byte("OggS-synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	got := client.Cached("spotify-abc")
	if got == nil || got.Quality.Format != "ogg" || filepath.Ext(got.Path) != ".ogg" {
		t.Fatal("native Ogg cache did not win")
	}
	part := filepath.Join(dir, ".part-spotify-interrupted")
	if err := os.WriteFile(part, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	client.RemoveStaleParts()
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("Spotify interrupted part was not removed")
	}
}

func TestCacheLRUModTimeMultiKeepAndActiveDeckProtectionOnMinFree(t *testing.T) {
	dir := t.TempDir()
	c := &Client{DownloadDir: dir, MaxCache: 250}

	writeAge := func(name string, age time.Duration) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("OggS-synthetic-track-payload-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// 1. LRU: touching trackA via Cached() must refresh ModTime so Trim evicts trackB instead of trackA.
	a := writeAge("spotify-trackA.ogg", 3*time.Hour)
	b := writeAge("spotify-trackB.ogg", 2*time.Hour)
	if dl := c.Cached("spotify-trackA"); dl == nil {
		t.Fatal("Cached(spotify-trackA) missed")
	}
	stA, _ := os.Stat(a)
	stB, _ := os.Stat(b)
	if !stA.ModTime().After(stB.ModTime()) {
		t.Fatalf("Cached() did not refresh ModTime (FIFO instead of LRU): a=%v b=%v", stA.ModTime(), stB.ModTime())
	}
	cur := writeAge("spotify-trackC.ogg", 0)
	c.Trim(cur)
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatal("Trim did not evict least-recently-used trackB")
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatal("Trim evicted recently accessed trackA (FIFO bug)")
	}

	// 2. MinFree on /data must not wipe active deck tracks under MaxCache while evicting older non-active files.
	dir2 := t.TempDir()
	c2 := &Client{DownloadDir: dir2, MaxCache: 1 << 30, MinFree: 1 << 62}
	stale := filepath.Join(dir2, "spotify-stale.ogg")
	if err := os.WriteFile(stale, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-5 * time.Hour)
	_ = os.Chtimes(stale, oldTime, oldTime)

	deck1 := filepath.Join(dir2, "spotify-deck1.ogg")
	deck2 := filepath.Join(dir2, "spotify-deck2.ogg")
	_ = os.WriteFile(deck1, []byte("OggS-deck1"), 0o600)
	_ = os.WriteFile(deck2, []byte("OggS-deck2"), 0o600)
	// Deck 1 loads deck1 via Cached; Deck 2 then saves/trims deck2.
	if c2.Cached("spotify-deck1") == nil {
		t.Fatal("Cached(spotify-deck1) missed")
	}
	c2.Trim(deck2)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale unpinned file not evicted under MinFree")
	}
	if _, err := os.Stat(deck1); err != nil {
		t.Fatal("active Deck 1 track wiped by Deck 2 Trim under MinFree")
	}
	if _, err := os.Stat(deck2); err != nil {
		t.Fatal("current Deck 2 track wiped by Trim")
	}
}
