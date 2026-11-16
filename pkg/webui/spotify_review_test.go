package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

type reviewCatalog294 struct{ calls int }

func (c *reviewCatalog294) ByISRC(context.Context, string) (providers.Track, error) {
	c.calls++
	return providers.Track{}, deemixclient.ErrNoMatch
}
func (c *reviewCatalog294) Search(context.Context, string) ([]providers.Track, error) {
	c.calls++
	return nil, deemixclient.ErrNoMatch
}

func TestSpotifyReviewSettingsCopy(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	original := s.loadUI()
	original.SpotifySource = "soundcloud"
	exported := copyUIExport(original)
	if _, ok := exported["spotifySource"]; !ok {
		t.Fatal("spotifySource is dropped from settings_copy export")
	}
	restored, _, err := copyUIMerge(uiSettings{SpotifySource: "auto"}, exported, "SC5000")
	if err != nil || restored.SpotifySource != "soundcloud" {
		t.Fatalf("copy lost source: %+v %v", restored, err)
	}
	if err := s.saveUI(restored); err != nil {
		t.Fatal(err)
	}
	if got := s.loadUI().SpotifySource; got != "soundcloud" {
		t.Fatalf("spotify_source did not survive disk copy: %s", got)
	}
	if _, _, err := copyUIMerge(original, map[string]json.RawMessage{"spotifySource": json.RawMessage(`"unknown"`)}, "SC5000"); err == nil {
		t.Fatal("accepted invalid copied source")
	}
}
func TestSpotifyReviewWarmCache(t *testing.T) {
	j, dir, tr := cacheTestJobs(t)
	deezer := tr
	tr.Provider, tr.ID = "spotify", "reviewwarm"
	if err := os.WriteFile(filepath.Join(dir, "spotify-reviewwarm.mp3"), testMP3(200), 0644); err != nil {
		t.Fatal(err)
	}
	j.p.Deemix.AllowLossy = true
	catalog := &reviewCatalog294{}
	j.p.Catalog = catalog
	j.spotifySource = func() string { return "deezer" }
	j.deezerReady = func() bool { return true }
	_, dl, err := j.acquire(context.Background(), "review", tr, false, nil)
	if err != nil || dl == nil {
		t.Fatalf("cache unavailable: %v", err)
	}
	if catalog.calls != 0 {
		t.Fatalf("warm Spotify cache was re-matched: %d catalog calls", catalog.calls)
	}
	dl, err = j.p.Deemix.DownloadAs(context.Background(), deezer, "spotify-reviewwarm", nil)
	if err != nil || dl == nil || filepath.Ext(dl.Path) != ".mp3" {
		t.Fatalf("DownloadAs upgraded warm Spotify cache: %+v %v", dl, err)
	}
}
func TestSpotifyReviewBeatportISRCPath(t *testing.T) {
	dir := t.TempDir()
	wrong, right := filepath.Join(dir, "beatport-11.flac"), filepath.Join(dir, "beatport-12.flac")
	writeTestFLAC(t, wrong)
	writeTestFLAC(t, right)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog/search/" {
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"tracks":[{"id":11,"name":"Song","artists":[{"name":"A"}],"length_ms":200000,"isrc":"USWRONG00001"},{"id":12,"name":"Song","artists":[{"name":"A"}],"length_ms":200100,"isrc":"USRIGHT00001"}]}`))
	}))
	defer srv.Close()
	b := &providers.Beatport{BaseURL: srv.URL}
	if err := b.SetToken(providers.BeatportToken{Access: "fixture"}); err != nil {
		t.Fatal(err)
	}
	j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dir}})
	defer j.Close()
	j.spotifySource = func() string { return "beatport" }
	j.beatport = func() *providers.Beatport { return b }
	_, dl, err := j.acquire(context.Background(), "review", providers.Track{ID: "reviewisrc", Provider: "spotify", Title: "Song", Artist: "A", DurationMS: 200000, ISRC: "USRIGHT00001"}, false, nil)
	if err != nil || dl == nil {
		t.Fatalf("fetch: %v", err)
	}
	got, _ := os.Stat(dl.Path)
	expected, _ := os.Stat(right)
	if !os.SameFile(got, expected) {
		t.Fatal("Spotify/Beatport acquisition linked the different-ISRC recording")
	}
}

func TestSpotifyReviewScopedSettingsAndSources(t *testing.T) {
	j, dir, tr := cacheTestJobs(t)
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), AccessToken: "fixture"}, j)
	defer s.Close()
	for _, source := range []string{"auto", "deezer", "netease", "beatport", "soundcloud"} {
		req := httptest.NewRequest("POST", "/api/ui?spotifySource="+source, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer fixture")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != 200 || j.spotifySource() != source {
			t.Fatalf("setting %s: %d", source, w.Code)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "settings.conf"))
		if !strings.Contains(string(data), "spotify_source="+source+"\n") {
			t.Fatal("not persisted")
		}
	}
	for _, bad := range []string{"", "tidal", "AUTO", "../bad"} {
		req := httptest.NewRequest("POST", "/api/ui?spotifySource="+bad, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer fixture")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("accepted %q", bad)
		}
	}
	tr.Provider, tr.ID = "spotify", "autoreview"
	// Auto succeeds on its first connected source before any later branch runs.
	j.spotifySource = func() string { return "auto" }
	j.deezerReady = func() bool { return true }
	j.netease = func(string, bool) *providers.Netease {
		t.Error("auto queried NetEase after a Deezer success")
		return nil
	}
	_, dl, err := j.acquire(context.Background(), "review", tr, false, nil)
	if err != nil || dl == nil || filepath.Base(dl.Path) != "spotify-autoreview.flac" {
		t.Fatalf("auto Deezer: %v", err)
	}
	j.deezerReady = nil
	j.netease = nil
	j.beatport = nil
	j.soundcloud = nil
	for _, source := range []string{"deezer", "netease", "beatport", "soundcloud"} {
		j.spotifySource = func() string { return source }
		tr.ID = "loggedout" + source
		_, _, err := j.acquire(context.Background(), "review", tr, false, nil)
		if err == nil || !strings.Contains(err.Error(), "not logged in") {
			t.Fatalf("logged out %s: %v", source, err)
		}
	}
}

func TestSpotifyReviewMatchedFormatsManifest(t *testing.T) {
	for _, format := range []struct{ ext, mime, codec, suffix string }{
		{"flac", "audio/flac", "flac", ".flac"},
		{"mp3", "audio/mpeg", "mp3", ".mp3"},
		{"mp4", "audio/mp4", "mp4a.40.2", ".mp4"},
		{"ogg", "audio/ogg", "mp3", ".mp3"},
	} {
		t.Run(format.ext, func(t *testing.T) {
			s, srv, track := spotifyLoadGateway(t, func(http.ResponseWriter, *http.Request) { t.Error("cached manifest requested upstream") })
			s.jobs.p.Deemix.AllowLossy = true
			cache := filepath.Join(s.jobs.p.Deemix.DownloadDir, "spotify-4uLU6hMCjMI75M1A2tKUQC."+format.ext)
			switch format.ext {
			case "flac":
				writeTestFLAC(t, cache)
			case "mp3":
				if err := os.WriteFile(cache, testMP3(200), 0600); err != nil {
					t.Fatal(err)
				}
			case "mp4":
				if err := os.WriteFile(cache, []byte("\x00\x00\x00\x18ftyp"), 0600); err != nil {
					t.Fatal(err)
				}
			case "ogg":
				data, err := os.ReadFile("testdata/spotify-native-44100.ogg")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cache, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			data := tidalGet(t, srv.URL, track+"playbackinfopostpaywall", http.StatusOK, nil)
			var reply struct{ Manifest string }
			if err := json.Unmarshal(data, &reply); err != nil {
				t.Fatal(err)
			}
			raw, err := base64.StdEncoding.DecodeString(reply.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				MimeType, Codecs string
				URLs             []string
			}
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.MimeType != format.mime || manifest.Codecs != format.codec || len(manifest.URLs) != 1 || !strings.HasSuffix(manifest.URLs[0], format.suffix) {
				t.Fatalf("wrong cached format manifest: %s", raw)
			}
			resp, err := http.Get(manifest.URLs[0])
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			expected, _ := os.ReadFile(cache)
			if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != format.mime || !bytes.Equal(body, expected) {
				t.Fatalf("stream disagrees: status=%d mime=%s err=%v", resp.StatusCode, resp.Header.Get("Content-Type"), err)
			}
			var legacy struct{ URL, Codec string }
			tidalGet(t, srv.URL, track+"streamurl", http.StatusOK, &legacy)
			if legacy.URL != manifest.URLs[0] || legacy.Codec != format.codec {
				t.Fatalf("streamurl disagrees: %+v", legacy)
			}
		})
	}
}

func TestSpotifyReviewSearchISRCBeforeMetadata(t *testing.T) {
	source := providers.Track{Provider: "spotify", ID: "source", Title: "Song", Artist: "A", ISRC: "USRIGHT00001", DurationMS: 200000}
	for _, mode := range []string{"exact", "metadata", "conflicting", "metadata-isrc-result"} {
		t.Run(mode, func(t *testing.T) {
			var queries []string
			got, err := spotifySearchRecording(context.Background(), func(_ context.Context, q string) ([]providers.Track, error) {
				queries = append(queries, q)
				if q == source.ISRC {
					if mode == "exact" {
						return []providers.Track{
							{ID: "wrong", Title: "Song", Artist: "A", ISRC: "USWRONG00001", DurationMS: 200000},
							{ID: "right", Title: "Song", Artist: "A", ISRC: source.ISRC, DurationMS: 200100},
						}, nil
					}
					if mode == "metadata-isrc-result" {
						return []providers.Track{{ID: "unidentified", Title: "Song", Artist: "A", DurationMS: 200000}}, nil
					}
					return nil, nil
				}
				if mode == "metadata-isrc-result" {
					return []providers.Track{{ID: "right", Title: "Song", Artist: "A", ISRC: source.ISRC, DurationMS: 200100}}, nil
				}
				candidate := providers.Track{ID: "fallback", Title: "Song", Artist: "A", DurationMS: 204000}
				if mode == "conflicting" {
					candidate.ISRC = "USWRONG00001"
				}
				return []providers.Track{candidate}, nil
			}, source)
			if len(queries) == 0 || queries[0] != source.ISRC {
				t.Fatalf("ISRC was not searched first: %v", queries)
			}
			switch mode {
			case "exact":
				if err != nil || got.ID != "right" || len(queries) != 1 {
					t.Fatalf("exact=%+v queries=%v err=%v", got, queries, err)
				}
			case "metadata":
				if err != nil || got.ID != "fallback" || len(queries) != 2 {
					t.Fatalf("fallback=%+v queries=%v err=%v", got, queries, err)
				}
			case "metadata-isrc-result":
				if err != nil || got.ID != "right" || len(queries) != 2 {
					t.Fatalf("metadata outranked ISRC: %+v %v %v", got, queries, err)
				}
			case "conflicting":
				if err == nil {
					t.Fatalf("accepted conflicting ISRC: %+v", got)
				}
			}
		})
	}
}

func TestSpotifyReviewWarmLossyFileAndRestart(t *testing.T) {
	for _, format := range []string{"mp3", "ogg"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			name := "spotify-warm." + format
			cache := filepath.Join(dir, name)
			data := testMP3(200)
			if format == "ogg" {
				var err error
				data, err = os.ReadFile("testdata/spotify-native-44100.ogg")
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(cache, data, 0600); err != nil {
				t.Fatal(err)
			}
			track := providers.Track{Provider: "spotify", ID: "warm", Title: "Song", Artist: "A", ISRC: "FIXTURE"}
			for range 2 {
				cat := &reviewCatalog294{}
				j := NewJobs(Pipeline{Catalog: cat, Deemix: &deemixclient.Client{DownloadDir: dir, AllowLossy: true}})
				j.deezerReady = func() bool { return true }
				j.spotifySource = func() string { return "deezer" }
				if got := j.File(track); got != cache {
					t.Errorf("File lost warm %s: %s", format, got)
				}
				_, dl, err := j.acquire(context.Background(), "s", track, false, nil)
				j.Close()
				if err != nil || dl == nil || dl.Path != cache || cat.calls != 0 {
					t.Fatalf("rematched warm %s: %+v err=%v calls=%d", format, dl, err, cat.calls)
				}
			}
		})
	}
}
