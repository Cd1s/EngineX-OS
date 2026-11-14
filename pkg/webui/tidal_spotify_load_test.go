package webui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func spotifyLoadGateway(t *testing.T, audio http.HandlerFunc) (*Server, *httptest.Server, string) {
	t.Helper()
	up := httptest.NewServer(audio)
	t.Cleanup(up.Close)
	j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}})
	t.Cleanup(j.Close)
	s := New(Config{AccessToken: "fixture-console", UIDir: t.TempDir(), StateDir: t.TempDir(), TidalAPIURL: up.URL, TidalAuthURL: up.URL}, j)
	t.Cleanup(s.Close)
	sp := &providers.Spotify{BaseURL: up.URL, Token: providers.Token{AccessToken: "fixture-spotify"}}
	t.Cleanup(sp.Close)
	j.spotify = func() *providers.Spotify { return sp } // New wires the owner lookup.
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	uiPost(t, srv.URL, "scope=spotify")
	if err := s.tidal.saveRealAuth(tidalAuth{AccessToken: "fixture-tidal", RefreshToken: "fixture-refresh"}); err != nil {
		t.Fatal(err)
	}
	id := s.tidal.addTrack(trackRef{session: "fixture-owner", track: providers.Track{Provider: "spotify", ID: "4uLU6hMCjMI75M1A2tKUQC"}})
	return s, srv, fmt.Sprintf("/v1/tracks/%d/", id)
}

func spotifyManifest(t *testing.T, data []byte) string {
	t.Helper()
	var info struct{ Manifest string }
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(info.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		MimeType, Codecs, EncryptionType string
		URLs                             []string
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.MimeType != "audio/ogg" || manifest.Codecs != "vorbis" || manifest.EncryptionType != "NONE" || len(manifest.URLs) != 1 || !strings.HasSuffix(manifest.URLs[0], ".ogg") {
		t.Fatalf("Spotify manifest: %s (%v)", raw, err)
	}
	return manifest.URLs[0]
}

// #269: key/CDN failures belong to the media download, never to the carrier's
// playback API. Both Engine API variants must return the known native codec.
func TestSpotifyFetchFailureIsTrackLoadFailure269(t *testing.T) {
	for _, endpoint := range []string{"playbackinfopostpaywall", "streamurl"} {
		t.Run(endpoint, func(t *testing.T) {
			var calls atomic.Int32
			s, srv, track := spotifyLoadGateway(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusForbidden)
			})
			s.jobs.p.Deemix.AllowLossy = true // Spotify's codec is fixed with either setting.
			before, err := os.ReadFile(s.tidal.authPath())
			if err != nil {
				t.Fatal(err)
			}
			data := tidalGet(t, srv.URL, track+endpoint, http.StatusOK, nil)
			var address string
			if endpoint == "playbackinfopostpaywall" {
				address = spotifyManifest(t, data)
			} else {
				var v struct{ URL, Codec string }
				if err := json.Unmarshal(data, &v); err != nil || v.Codec != "vorbis" || !strings.HasSuffix(v.URL, ".ogg") {
					t.Fatalf("streamUrl: %s (%v)", data, err)
				}
				address = v.URL
			}
			if calls.Load() != 0 {
				t.Fatal("playback API fetched audio before returning the manifest")
			}
			resp, err := http.Get(address)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode < 400 || resp.StatusCode == 401 || resp.StatusCode == 403 || len(body) == 0 || calls.Load() != 1 {
				t.Fatalf("media failure: HTTP %d, calls=%d, %q (%v)", resp.StatusCode, calls.Load(), body, err)
			}
			after, err := os.ReadFile(s.tidal.authPath())
			if err != nil || !bytes.Equal(before, after) || s.scope() != "spotify" {
				t.Fatal("track failure changed real TIDAL credentials or scope")
			}
			var carrier struct{ UserID int }
			tidalGet(t, srv.URL, "/v1/sessions", http.StatusOK, &carrier)
			if carrier.UserID != 1 {
				t.Fatal("synthetic carrier became unavailable")
			}
		})
	}
}

// #269: lifting the server write deadline cannot extend Engine's API deadline.
// A blocked media endpoint must have no effect on the manifest response time.
func TestSpotifyManifestPrecedesSlowFetch269(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	_, srv, track := spotifyLoadGateway(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		w.WriteHeader(http.StatusForbidden)
	})
	defer close(release)
	client := &http.Client{Timeout: 250 * time.Millisecond}
	resp, err := client.Get(srv.URL + track + "playbackinfopostpaywall")
	if err != nil {
		t.Fatalf("manifest hit client deadline waiting for Spotify: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest HTTP %d: %v", resp.StatusCode, err)
	}
	spotifyManifest(t, body)
	if calls.Load() != 0 {
		t.Fatal("manifest started a speculative audio fetch")
	}
}

func TestSpotifyManifestLoadsNativeOgg269(t *testing.T) {
	fixture, err := os.ReadFile("testdata/spotify-native-44100.ogg")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	_, srv, track := spotifyLoadGateway(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write(fixture)
	})
	address := spotifyManifest(t, tidalGet(t, srv.URL, track+"playbackinfopostpaywall", http.StatusOK, nil))
	if calls.Load() != 0 {
		t.Fatal("manifest fetched audio")
	}
	for range 2 { // A second load reuses the completed download.
		req, err := http.NewRequest(http.MethodGet, address, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", "bytes=0-31")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Type") != "audio/ogg" || !bytes.Equal(body, fixture[:32]) || calls.Load() != 1 {
			t.Fatalf("native Ogg Range: HTTP %d, calls=%d (%v)", resp.StatusCode, calls.Load(), err)
		}
	}
}

// #269/#224: a gateway-owned row is not a real TIDAL track. Even when the
// unpaired browser moves to TIDAL, its playback API must not return the real-login
// 401/subStatus=6001 response that invalidates Engine's carrier account.
func TestSpotifyUnpairedTrackAvoidsTidalAuthFailure269(t *testing.T) {
	for _, realLogin := range []bool{false, true} {
		t.Run(fmt.Sprint(realLogin), func(t *testing.T) {
			var calls atomic.Int32
			s, srv, track := spotifyLoadGateway(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusForbidden)
			})
			if !realLogin {
				if err := s.tidal.clearRealAuth(); err != nil {
					t.Fatal(err)
				}
			}
			before := s.tidal.realAuth()
			uiPost(t, srv.URL, "scope=tidal")
			want := http.StatusOK
			if realLogin {
				want = http.StatusForbidden
			}
			data := tidalGet(t, srv.URL, track+"playbackinfopostpaywall", want, nil)
			if !realLogin {
				spotifyManifest(t, data)
			}
			tidalGet(t, srv.URL, strings.TrimSuffix(track, "/"), want, nil)
			tidalGet(t, srv.URL, track+"streamurl", want, nil)
			wantCalls := int32(0)
			if realLogin {
				wantCalls = 3
			}
			if calls.Load() != wantCalls {
				t.Fatalf("upstream calls=%d, want %d", calls.Load(), wantCalls)
			}
			if s.tidal.realAuth() != before || s.scope() != "tidal" {
				t.Fatal("playback changed real TIDAL credentials or browser scope")
			}
			// Paired TIDAL owns colliding numeric IDs; real login errors stay real.
			want = http.StatusUnauthorized
			if realLogin {
				want = http.StatusForbidden
			}
			data = tidalGet(t, srv.URL, "/v1/sessions", want, nil)
			if !realLogin && !bytes.Contains(data, []byte(`"subStatus":6001`)) {
				t.Fatal("real-TIDAL login-required response was masked")
			}
		})
	}
}
