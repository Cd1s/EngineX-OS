package deemixclient

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbedCoverOggIsUnmodifiedNoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"spotify-abc.ogg", oggIdentificationPage(t)},
		{"spotify-abc.OGG", []byte("offline fixture")},
		{"misnamed.mp3", oggIdentificationPage(t)},
		{"misnamed.flac", oggIdentificationPage(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, tc.data, 0400); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			// An invalid address and canceled context also prove no fetch is attempted.
			if err := (&Client{}).EmbedCover(ctx, path, ":invalid-cover-url"); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.data) || !os.SameFile(before, after) || before.ModTime() != after.ModTime() || before.Mode() != after.Mode() {
				t.Fatal("Ogg file changed while adding artwork")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary files left behind: %v, %v", entries, err)
			}
		})
	}
}
