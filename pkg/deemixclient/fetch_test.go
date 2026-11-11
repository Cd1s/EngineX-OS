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
