package deemixclient

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// DownloadAs gives another service its own file name for a Deezer file already on disk.
func TestDownloadAsKeepsAnIndependentName(t *testing.T) {
	dir := t.TempDir()
	os.Rename(writeFLAC(t, dir), filepath.Join(dir, "123.flac"))
	c := &Client{DownloadDir: dir}
	dl, err := c.DownloadAs(context.Background(), providers.Track{ID: "123", Provider: "deezer"}, "soundcloud-77", nil)
	if err != nil || filepath.Base(dl.Path) != "soundcloud-77.flac" {
		t.Fatalf("%+v %v", dl, err)
	}
	if c.Cached("soundcloud-77") == nil || c.Cached("123") == nil {
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

// Two callers giving one file the same second name at the same moment (a queued job and a deck load of the
// same track) both succeed: they share the temporary name, and one used to remove the other's link.
func TestLinkAsTwoCallersForOneName(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "123.flac")
	if err := os.Rename(writeFLAC(t, dir), file); err != nil {
		t.Fatal(err)
	}
	c := &Client{DownloadDir: dir}
	src := &Download{Path: file}
	const callers = 20
	var start, done sync.WaitGroup
	start.Add(1)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if _, err := c.LinkAs(src, "soundcloud-77"); err != nil {
				errs <- err
			}
		}()
	}
	start.Done()
	done.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("LinkAs: %v", err)
	}
	if c.Cached("soundcloud-77") == nil || c.Cached("123") == nil {
		t.Fatal("both names must stay cached")
	}
}
