package webui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func TestColdFetchDiscoversFinishedProviderFileWithoutNetwork(t *testing.T) {
	var network atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		network.Add(1)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	var audio bytes.Buffer
	if err := testaudio.FLAC(&audio, testaudio.Song(1)); err != nil {
		t.Fatal(err)
	}
	oggAudio, err := os.ReadFile("testdata/spotify-native-44100.ogg")
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"deezer", "netease", "spotify", "beatport"} {
		t.Run(provider, func(t *testing.T) {
			dir := t.TempDir()
			name := provider + "-42.flac"
			payload := audio.Bytes()
			if provider == "deezer" {
				name = "42.flac"
			} else if provider == "spotify" {
				name = "spotify-42.ogg"
				payload = oggAudio
			}
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, payload, 0600); err != nil {
				t.Fatal(err)
			}
			j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{BaseURL: srv.URL, DownloadDir: dir}})
			defer j.Close()
			tr := providers.Track{ID: "42", Provider: provider, CoverURL: srv.URL + "/cover"}
			start := time.Now()
			got, err := j.Fetch(context.Background(), "s", tr, false)
			if err != nil || got != path || network.Load() != 0 {
				t.Fatalf("path=%q err=%v network=%d", got, err, network.Load())
			}
			t.Logf("finished %s cache -> playable path: %s, upstream requests=0", provider, time.Since(start).Round(time.Microsecond))
		})
	}
}

func TestColdFileDiscoveryKeepsProviderIsolationAndFinalNames(t *testing.T) {
	dir := t.TempDir()
	var audio bytes.Buffer
	if err := testaudio.FLAC(&audio, testaudio.Song(1)); err != nil {
		t.Fatal(err)
	}
	oggAudio, err := os.ReadFile("testdata/spotify-native-44100.ogg")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"42.flac", ".part-99.flac", ".deemix-99-test.part", "spotify-13.flac"} {
		if err := os.WriteFile(filepath.Join(dir, name), audio.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "spotify-14.ogg"), oggAudio, 0600); err != nil {
		t.Fatal(err)
	}
	j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dir}})
	defer j.Close()
	j.neteaseDirect = func() bool { return true }
	for _, tr := range []providers.Track{{ID: "42", Provider: "netease"}, {ID: "42", Provider: "spotify"}, {ID: "42", Provider: "beatport"}, {ID: "42", Provider: "soundcloud"}, {ID: "99", Provider: "deezer"}, {ID: "13", Provider: "deezer"}, {ID: "14", Provider: "deezer"}, {ID: "13", Provider: "spotify"}, {ID: "../42", Provider: "netease"}, {ID: "18446744073709551616", Provider: "deezer"}} {
		if path := j.File(tr); path != "" {
			t.Fatalf("%s:%s crossed cache boundary: %q", tr.Provider, tr.ID, path)
		}
	}
}

func TestColdBeatportDiscoveryPreservesLosslessUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beatport-42.mp4")
	if err := os.WriteFile(path, []byte("\x00\x00\x00\x18ftyp"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &deemixclient.Client{DownloadDir: dir}
	j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: c})
	defer j.Close()
	track := providers.Track{ID: "42", Provider: "beatport"}
	if got := j.File(track); got != "" {
		t.Fatalf("lossless request bypassed upgrade: %q", got)
	}
	c.PreferMP3 = func() bool { return true }
	if got := j.File(track); got != path {
		t.Fatalf("stream quality did not reuse AAC: %q", got)
	}
}

func TestColdTidalPassthroughLatency300ms(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"items":[],"totalNumberOfItems":0}`))
	}))
	defer api.Close()
	ui := New(Config{AccessToken: "fixture", UIDir: t.TempDir(), StateDir: t.TempDir(), TidalAPIURL: api.URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	res, err := http.Post(srv.URL+"/v1/sc5000/ui?scope=tidal", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for run := 0; run < 2; run++ {
		for _, path := range []string{"/v1/users/1/playlists?limit=10&offset=0", "/v1/playlists/fixture/items?limit=10&offset=0"} {
			start := time.Now()
			res, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatal(res.StatusCode)
			}
			t.Logf("unchanged run=%d %s cold=%s requests=1", run, path, time.Since(start).Round(time.Millisecond))
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("calls=%d", calls.Load())
	}
}
