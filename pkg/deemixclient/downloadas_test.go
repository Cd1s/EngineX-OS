package deemixclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// DownloadAs gives another service its own file name for a Deezer file already on disk.
func TestDownloadAsKeepsAnIndependentName(t *testing.T) {
	dir := t.TempDir()
	os.Rename(writeFLAC(t, dir), filepath.Join(dir, "123.flac"))
	c := &Client{DownloadDir: dir}
	dl, err := c.DownloadAs(context.Background(), providers.Track{ID: "123", Provider: "deezer"}, "spotify-abc", nil)
	if err != nil || filepath.Base(dl.Path) != "spotify-abc.flac" {
		t.Fatalf("%+v %v", dl, err)
	}
	if c.Cached("spotify-abc") == nil || c.Cached("123") == nil {
		t.Fatal("both names must stay cached")
	}
	// Removing one name (eviction) leaves the other.
	os.Remove(dl.Path)
	if c.Cached("123") == nil {
		t.Fatal("Deezer file lost")
	}
	if _, err := c.DownloadAs(context.Background(), providers.Track{ID: "123", Provider: "deezer"}, "../x", nil); err == nil {
		t.Fatal("path in name accepted")
	}
}
