package deemixclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrimNASHashesProtectsOpenAndDownloadingFiles(t *testing.T) {
	for _, ext := range []string{".mp3", ".flac", ".wav", ".aiff", ".aif", ".m4a", ".aac", ".ogg", ".mp4", ".opus", ".mp3 ", "."} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			old := filepath.Join(dir, "nas-"+strings.Repeat("1", 64)+ext)
			active := filepath.Join(dir, "nas-"+strings.Repeat("2", 64)+ext)
			kept := filepath.Join(dir, "nas-"+strings.Repeat("3", 64)+ext)
			foreign := filepath.Join(dir, "personal"+ext)
			for _, p := range []string{old, active, kept, foreign} {
				if err := os.WriteFile(p, make([]byte, 100), 0600); err != nil {
					t.Fatal(err)
				}
			}
			f, err := os.Open(active)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			download, err := os.CreateTemp(dir, ".tmp-download-*")
			if err != nil {
				t.Fatal(err)
			}
			defer download.Close()
			if _, err := download.Write(make([]byte, 100)); err != nil {
				t.Fatal(err)
			}
			c := &Client{DownloadDir: dir, MaxCache: 1}
			c.Trim(kept)
			if _, err := os.Stat(old); !os.IsNotExist(err) {
				t.Fatalf("NAS cache not evicted: %v", err)
			}
			for _, p := range []string{active, kept, foreign, download.Name()} {
				if _, err := os.Stat(p); err != nil {
					t.Fatalf("protected file deleted: %s: %v", p, err)
				}
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			c.Trim(kept)
			if _, err := os.Stat(active); !os.IsNotExist(err) {
				t.Fatalf("closed NAS file not evicted: %v", err)
			}
		})
	}
}
