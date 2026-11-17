package providers

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testsmb"
)

func TestSMBNASUsesTheHTTPDialPolicy(t *testing.T) {
	blocked := errors.New("NAS address policy rejected target")
	calls := 0
	httpClient := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second {
			t.Error("SMB connect has no bounded deadline")
		}
		return nil, blocked
	}}}
	p := NewSelfHostedProvider(NASConfig{Protocol: "smb", URL: "smb://share.invalid/music"}, httpClient)
	if _, err := p.Playlists(context.Background()); !errors.Is(err, blocked) {
		t.Fatalf("browse ignored the NAS dial policy: %v", err)
	}
	if _, err := p.DownloadTrack(context.Background(), "a.mp3", t.TempDir()); !errors.Is(err, blocked) {
		t.Fatalf("download ignored the NAS dial policy: %v", err)
	}
	if calls != 2 {
		t.Fatalf("NAS dial policy called %d times", calls)
	}
}

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

func TestSMBNASCacheSeparatesBaseFolders(t *testing.T) {
	s := testsmb.Start(t)
	dest := t.TempDir()
	files := map[string]string{}
	for _, folder := range []string{"A", "B"} {
		os.MkdirAll(filepath.Join(s.Root, folder), 0700)
		os.WriteFile(filepath.Join(s.Root, folder, "track.mp3"), []byte(folder), 0600)
		p := NewSelfHostedProvider(NASConfig{Protocol: "smb", URL: s.URL + "/" + folder, Username: s.User, Password: s.Password}, nil)
		file, err := p.DownloadTrack(context.Background(), "track.mp3", dest)
		if err != nil {
			t.Fatal(err)
		}
		files[folder] = file
	}
	if files["A"] == files["B"] {
		t.Fatal("different remote folders share a cache file")
	}
	for folder, file := range files {
		b, err := os.ReadFile(file)
		if err != nil || string(b) != folder {
			t.Fatalf("cache for %s: %q %v", folder, b, err)
		}
	}
}
