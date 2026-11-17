package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testsmb"
)

func TestSMBNASBrowseSearchAndDownload(t *testing.T) {
	s := testsmb.Start(t)
	os.MkdirAll(filepath.Join(s.Root, "Sets"), 0700)
	os.WriteFile(filepath.Join(s.Root, "Björk.mp3"), []byte("ID3 fixture"), 0600)
	os.WriteFile(filepath.Join(s.Root, "Sets", "Été.flac"), []byte("fLaC fixture"), 0600)
	p := NewSelfHostedProvider(NASConfig{Protocol: "smb", URL: s.URL, Username: s.User, Password: s.Password, Enabled: true}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lists, err := p.Playlists(ctx)
	if err != nil || len(lists) != 2 || lists[0].Count != 1 || lists[1].Title != "Sets" {
		t.Fatalf("playlists: %+v %v", lists, err)
	}
	tracks, err := p.Tracks(ctx, "Sets")
	if err != nil || len(tracks) != 1 || tracks[0].Title != "Été" {
		t.Fatalf("tracks: %+v %v", tracks, err)
	}
	hits, err := p.Search(ctx, "bjÖrk")
	if err != nil || len(hits) != 1 {
		t.Fatalf("search: %+v %v", hits, err)
	}
	file, err := p.DownloadTrack(ctx, tracks[0].ID, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "fLaC fixture" {
		t.Fatalf("download: %q %v", b, err)
	}
	if _, err := p.DownloadTrack(ctx, "../outside.mp3", t.TempDir()); err == nil {
		t.Fatal("accepted path outside configured folder")
	}
	bad := NewSelfHostedProvider(NASConfig{Protocol: "smb", URL: s.URL, Username: s.User, Password: "wrong"}, nil)
	started := time.Now()
	if _, err := bad.Playlists(ctx); err == nil || time.Since(started) > 4*time.Second {
		t.Fatalf("wrong password: %v after %s", err, time.Since(started))
	}
}
