package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

type noCatalog struct{}

func (noCatalog) ByISRC(context.Context, string) (providers.Track, error) {
	return providers.Track{}, errors.New("not in catalog")
}
func (noCatalog) Search(context.Context, string) ([]providers.Track, error) { return nil, nil }

// A minimal MP4: isMP4 only looks for the ftyp box.
var beatportAAC = append([]byte("\x00\x00\x00\x14ftypM4A \x00\x00\x00\x00M4A "), make([]byte, 64)...)

// fakeBeatportAPI serves track 501 as a 256k file (Professional without FLAC) and
// track 502 only as a stream (Essential). cdn counts audio transfers.
func fakeBeatportAPI(t *testing.T, cdn *int) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/cdn/") {
			*cdn++
			w.Write(beatportAAC)
			return
		}
		if r.URL.Path == "/hls/502.m3u8" { // the CDN needs no login
			io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"/cdn/init.mp4\"\n#EXTINF:10,\n/cdn/s1.m4s\n#EXT-X-ENDLIST\n")
			return
		}
		if r.Header.Get("Authorization") != "Bearer a" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/my/playlists/":
			io.WriteString(w, `{"results":[{"id":11,"name":"Peak","track_count":1}]}`)
		case "/catalog/charts/":
			io.WriteString(w, `{"results":[{"id":7,"name":"Top","track_count":1}]}`)
		case "/my/playlists/11/tracks/":
			io.WriteString(w, `{"results":[{"track":{"id":501,"name":"Losing It","mix_name":"Original Mix","bpm":125,"key":{"camelot_number":8,"camelot_letter":"A"},"artists":[{"name":"Fisher"}],"length_ms":379000}}]}`)
		case "/catalog/tracks/501/download/":
			if r.URL.Query().Get("quality") != "high" {
				w.WriteHeader(403)
				return
			}
			io.WriteString(w, `{"location":"`+strings.Replace(srv.URL, "http://", "https://", 1)+`/cdn/501.256k.aac.mp4","stream_quality":".256k.aac.mp4"}`)
		case "/catalog/tracks/502/download/":
			w.WriteHeader(403)
		case "/catalog/tracks/502/stream/":
			io.WriteString(w, `{"stream_url":"`+srv.URL+`/hls/502.m3u8"}`)
		default:
			t.Errorf("unexpected %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The deck's QR code opens the phone page, a pasted token logs in, and Beatport's
// playlists and charts play through the deck's TIDAL browser.
func TestBeatportPhoneLoginBrowseAndPlay(t *testing.T) {
	cdn := 0
	api := fakeBeatportAPI(t, &cdn)
	// The file location is https; the test CDN is plain http on the same host.
	media := &http.Client{Transport: rewriteHTTPS{}}
	state := t.TempDir()
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir(), Media: media}})
	defer jobs.Close()
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, BeatportURL: api.URL}, jobs)
	srv := httptest.NewServer(ui)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/enginex/login?provider=beatport", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	target, _ := url.Parse(ui.logins["beatport"].Target)
	page := srv.URL + target.Path + "?" + target.RawQuery
	resp, err = http.PostForm(page, url.Values{"token": {`{"access_token":"a","refresh_token":"r","expires_in":36000}`}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Connected") {
		t.Fatalf("phone page: %s", body)
	}
	if !strings.Contains(string(tidalGet(t, srv.URL, "/v1/enginex/login?provider=beatport", 200, nil)), `"status":"done"`) {
		t.Fatal("deck login not done")
	}
	if info, err := os.Stat(filepath.Join(state, "beatport-owner.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}

	if resp, err = http.Post(srv.URL+"/v1/enginex/ui?scope=beatport", "", nil); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var lists tidalList
	tidalGet(t, srv.URL, "/v1/users/1/playlists", 200, &lists)
	var titles []string
	var peak string
	for _, raw := range lists.Items {
		var p tidalPlaylist
		json.Unmarshal(raw, &p)
		titles = append(titles, p.Title)
		if p.Title == "Peak" {
			peak = p.UUID
		}
	}
	if strings.Join(titles, "|") != "Peak|【榜单】Top" {
		t.Fatalf("titles %q", titles)
	}
	var items struct {
		Items []struct {
			ID        int64  `json:"id"`
			Title     string `json:"title"`
			Copyright string `json:"copyright"`
		} `json:"items"`
	}
	tidalGet(t, srv.URL, "/v1/playlists/"+peak+"/items", 200, &items)
	if len(items.Items) != 1 || items.Items[0].Title != "Losing It (Original Mix)" || items.Items[0].Copyright != "Beatport [125 BPM 8A]" {
		t.Fatalf("items %+v", items)
	}
	audio := tidalGet(t, srv.URL, "/v1/stream/"+strconv.FormatInt(items.Items[0].ID, 10)+".mp4", 200, nil)
	if string(audio) != string(beatportAAC) || cdn != 1 {
		t.Fatalf("stream: %d bytes, cdn=%d", len(audio), cdn)
	}
	// Lossless was asked for, but 256k is all there is: the file on disk stays.
	if dl, err := jobs.fromBeatport(context.Background(), providers.Track{ID: "501", Provider: "beatport"}); err != nil || filepath.Base(dl.Path) != "beatport-501.mp4" || cdn != 1 {
		t.Fatalf("%+v %v cdn=%d", dl, err, cdn)
	}

	// Essential: no file at any quality, so the stream is saved instead.
	dl, err := jobs.fromBeatport(context.Background(), providers.Track{ID: "502", Provider: "beatport"})
	if err != nil || filepath.Base(dl.Path) != "beatport-502.mp4" || cdn != 3 {
		t.Fatalf("%+v %v cdn=%d", dl, err, cdn)
	}
}

type rewriteHTTPS struct{}

func (rewriteHTTPS) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme = "http"
	return http.DefaultTransport.RoundTrip(r)
}

// A relay login hands the deck a token, which becomes the owner's Beatport login.
func TestRelayDeliversABeatportToken(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"done","beatport":{"access_token":"a","refresh_token":"r"}}`)
	}))
	defer relay.Close()
	state := t.TempDir()
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, RelayURL: relay.URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/enginex/login?provider=beatport", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(ui.logins["beatport"].Target, relay.URL+"/m/beatport?t=") {
		t.Fatalf("%+v", ui.logins["beatport"])
	}
	if !strings.Contains(string(tidalGet(t, srv.URL, "/v1/enginex/login?provider=beatport", 200, nil)), `"status":"done"`) {
		t.Fatal("relay login not collected")
	}
	if p := ui.ownerBeatport(); p == nil || p.Token().Refresh != "r" {
		t.Fatalf("%+v", p)
	}
}
