package deemixclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Bug hunt 3: a cache name is built from a track ID that came from outside. A name with a
// path in it put the download outside the cache directory.
func TestSaveRefusesANameWithAPathInIt(t *testing.T) {
	audio := mp3Frames(14, 3)
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Write(audio)
	}))
	defer srv.Close()
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{DownloadDir: dir, AllowLossy: true}
	ctx := context.Background()
	for _, name := range []string{"netease-1/../../escaped", "../escaped", "sub/escaped", ".hidden", ""} {
		if _, err := c.Save(ctx, name, srv.URL+"/audio"); err == nil {
			t.Errorf("Save took the name %q", name)
		}
		if _, err := c.SaveSplit(ctx, name, srv.URL+"/audio"); err == nil {
			t.Errorf("SaveSplit took the name %q", name)
		}
		if _, err := c.SaveParts(ctx, name, "mp3", []string{srv.URL + "/audio"}); err == nil {
			t.Errorf("SaveParts took the name %q", name)
		}
		if dl := c.Cached(name); dl != nil {
			t.Errorf("Cached answered for the name %q: %s", name, dl.Path)
		}
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("%d requests were made for names that are refused", n)
	}
	// Nothing was written: not beside the cache directory, not in it.
	for _, d := range []string{root, dir} {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if e.Name() != "cache" {
				t.Errorf("left behind: %s", filepath.Join(d, e.Name()))
			}
		}
	}
	// A plain name still works.
	dl, err := c.Save(ctx, "netease-1", srv.URL+"/audio")
	if err != nil || filepath.Base(dl.Path) != "netease-1.mp3" {
		t.Fatalf("a plain name: %+v %v", dl, err)
	}
	if got := c.Cached("netease-1"); got == nil || got.Path != dl.Path {
		t.Fatalf("a plain name is not found again: %+v", got)
	}
}
