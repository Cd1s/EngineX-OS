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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/sc5000-custom-os/internal/testaudio"
	"github.com/Cd1s/sc5000-custom-os/internal/testlibrary"
	"github.com/Cd1s/sc5000-custom-os/pkg/analyzer"
	"github.com/Cd1s/sc5000-custom-os/pkg/deemixclient"
	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
	"github.com/Cd1s/sc5000-custom-os/pkg/resourceguard"
)

type tidalList struct {
	TotalNumberOfItems int               `json:"totalNumberOfItems"`
	Items              []json.RawMessage `json:"items"`
}
type tidalPlaylist struct {
	UUID           string `json:"uuid"`
	Title          string `json:"title"`
	NumberOfTracks int    `json:"numberOfTracks"`
}

func fakeDeezer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chart/0/playlists", "/search/playlist":
			io.WriteString(w, `{"data":[{"id":77,"title":"Top","nb_tracks":1}]}`)
		case "/search":
			io.WriteString(w, `{"data":[{"id":9,"title":"Found","duration":200,"artist":{"name":"Artist"}}]}`)
		default:
			io.WriteString(w, `{"error":{"message":"unexpected"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
func tidalGet(t *testing.T, base, path string, status int, out any) []byte {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("GET %s: %v %s", path, err, data)
		}
	}
	return data
}

func TestTidalGatewayLoginAndHint(t *testing.T) {
	ui := New(Config{AccessToken: "test-console-code", DeezerURL: fakeDeezer(t).URL, StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	for _, path := range []string{"/au/oauth2/device_authorization", "/au//oauth2/token"} {
		resp, err := http.Post(srv.URL+path, "application/x-www-form-urlencoded", strings.NewReader("client_id=engine"))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !(bytes.Contains(data, []byte(`"deviceCode":"sc5000-local"`)) || bytes.Contains(data, []byte(`"access_token":"sc5000-token"`))) {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, data)
		}
	}
	tidalGet(t, srv.URL, "/v1//ping", 200, nil)
	var session map[string]any
	tidalGet(t, srv.URL, "/v1/sessions", 200, &session)
	if session["userId"] != float64(1) || session["countryCode"] != "US" {
		t.Fatal(session)
	}
	var sub map[string]any
	tidalGet(t, srv.URL, "/v1//users/1/subscription", 200, &sub)
	if sub["highestSoundQuality"] != "LOSSLESS" || sub["status"] != "ACTIVE" {
		t.Fatal(sub)
	}
	titles := func() []string {
		var lists tidalList
		tidalGet(t, srv.URL, "/v1/users/1/playlists?countryCode=US", 200, &lists)
		out := []string{}
		for _, raw := range lists.Items {
			var p tidalPlaylist
			json.Unmarshal(raw, &p)
			out = append(out, p.Title)
		}
		return out
	}
	// Each service answers alone: public Deezer charts, or a login hint.
	if got := titles(); len(got) != 1 || got[0] != "Top" {
		t.Fatalf("deezer titles=%q", got)
	}
	for _, scope := range []string{"netease", "spotify"} {
		resp, err := http.Post(srv.URL+"/v1/sc5000/ui?scope="+scope, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := titles(); len(got) != 1 || !strings.Contains(got[0], "sc5000.lan:8080") {
			t.Fatalf("%s titles=%q", scope, got)
		}
	}
	// The gateway carries no console login, so it must stay local to the deck.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/sessions", nil)
	r.RemoteAddr = "192.0.2.10:4000"
	ui.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("remote gateway access: %d", w.Code)
	}
	// Console APIs keep requiring the access code.
	tidalGet(t, srv.URL, "/api/sessions", 401, nil)
}

func TestTidalGatewayPlaylistStreamAndImport(t *testing.T) {
	path, root, db := testlibrary.Create(t)
	lib, err := analyzer.OpenLibrary(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	file := filepath.Join(root, "42.flac")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = testaudio.FLAC(f, testaudio.Song(8)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	audio, _ := os.ReadFile(file)
	var enqueued atomic.Int32
	queue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := "queued"
		if r.Method == "GET" {
			state = "completed"
		} else {
			enqueued.Add(1)
		}
		json.NewEncoder(w).Encode(deemixclient.QueueStatus{ID: "q1", State: state, Progress: 1, Path: file})
	}))
	defer queue.Close()
	jobs := NewJobs(Pipeline{Guard: resourceguard.New(resourceguard.Config{Disabled: true}), Catalog: fixtureCatalog{}, Deemix: &deemixclient.Client{BaseURL: queue.URL, DownloadDir: root, PollInterval: time.Millisecond}, Library: lib})
	defer jobs.Close()
	uiDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("scope=netease\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{AccessToken: "test-console-code", DeezerURL: fakeDeezer(t).URL, UIDir: uiDir, Factory: func(string, bool, string) (providers.Provider, error) { return fixtureProvider{}, nil }}, jobs)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	var s Session
	json.Unmarshal(request(t, srv.Client(), srv.URL, "POST", "/api/sessions", map[string]any{"provider": "netease"}, 200), &s)
	ui.mu.Lock()
	ui.sessions[s.ID].Ready = true
	ui.mu.Unlock()

	var lists tidalList
	tidalGet(t, srv.URL, "/v1/users/1/favorites/playlists", 200, &lists)
	var friend tidalPlaylist
	for _, raw := range lists.Items {
		var p struct {
			tidalPlaylist
			Item tidalPlaylist `json:"item"`
		}
		json.Unmarshal(raw, &p)
		if p.Item.UUID != p.UUID || strings.Contains(p.Title, "sc5000.lan") {
			t.Fatalf("favorite item %s", raw)
		}
		if p.Title == "Friend" {
			friend = p.tidalPlaylist
		}
	}
	if friend.UUID == "" || friend.NumberOfTracks != 1 {
		t.Fatalf("netease playlist missing: %+v", lists)
	}
	var tracks tidalList
	tidalGet(t, srv.URL, "/v1/playlists/"+friend.UUID+"/tracks?limit=100", 200, &tracks)
	var entry struct {
		Type string `json:"type"`
		Item struct {
			ID       int64  `json:"id"`
			Title    string `json:"title"`
			Duration int64  `json:"duration"`
		} `json:"item"`
	}
	var flat struct {
		ID int64 `json:"id"`
	}
	if len(tracks.Items) != 1 || json.Unmarshal(tracks.Items[0], &entry) != nil || entry.Type != "track" || entry.Item.ID <= 0 || entry.Item.Duration != 8 || json.Unmarshal(tracks.Items[0], &flat) != nil || flat.ID != entry.Item.ID {
		t.Fatalf("tracks %+v", tracks)
	}
	id := strconv.FormatInt(entry.Item.ID, 10)
	var again tidalList
	tidalGet(t, srv.URL, "/v1//playlists/"+friend.UUID+"/tracks", 200, &again)
	if !bytes.Equal(again.Items[0], tracks.Items[0]) {
		t.Fatal("track ID not stable")
	}
	var info struct {
		ManifestMimeType string `json:"manifestMimeType"`
		Manifest         string `json:"manifest"`
	}
	tidalGet(t, srv.URL, "/v1/tracks/"+id+"/playbackinfopostpaywall?audioquality=LOSSLESS", 200, &info)
	raw, _ := base64.StdEncoding.DecodeString(info.Manifest)
	var manifest struct {
		MimeType       string   `json:"mimeType"`
		EncryptionType string   `json:"encryptionType"`
		URLs           []string `json:"urls"`
	}
	json.Unmarshal(raw, &manifest)
	stream := srv.URL + "/v1/stream/" + id + ".flac"
	if info.ManifestMimeType != "application/vnd.tidal.bts" || manifest.MimeType != "audio/flac" || manifest.EncryptionType != "NONE" || len(manifest.URLs) != 1 || manifest.URLs[0] != stream {
		t.Fatalf("manifest %s %s", info.ManifestMimeType, raw)
	}
	var su map[string]any
	tidalGet(t, srv.URL, "/v1/tracks/"+id+"/streamurl", 200, &su)
	if su["url"] != stream || su["soundQuality"] != "LOSSLESS" {
		t.Fatal(su)
	}
	if body := tidalGet(t, srv.URL, "/v1/stream/"+id+".flac", 200, nil); !bytes.Equal(body, audio) {
		t.Fatal("stream body differs from download")
	}
	r, _ := http.NewRequest("GET", stream, nil)
	r.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(part) != "fLaC" || enqueued.Load() != 1 {
		t.Fatalf("range %d %q enqueued=%d", resp.StatusCode, part, enqueued.Load())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		list := jobs.List()
		if len(list) == 1 && list[0].State == "done" {
			break
		}
		if len(list) == 1 && list[0].State == "error" || time.Now().After(deadline) {
			t.Fatalf("background import %+v", list)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if enqueued.Load() != 1 {
		t.Fatalf("background import downloaded again: %d", enqueued.Load())
	}
	var playlist string
	if err = db.QueryRow("SELECT p.title FROM Track t JOIN PlaylistEntity pe ON pe.trackId=t.id JOIN Playlist p ON p.id=pe.listId").Scan(&playlist); err != nil || playlist != providers.OwnerPlaylist {
		t.Fatalf("import %q %v", playlist, err)
	}

	// A guest playing the owner's download is imported again for the guest
	// playlist without a second download.
	ui.tidal.mu.Lock()
	ref := ui.tidal.tracks[entry.Item.ID]
	ui.tidal.mu.Unlock()
	for range 2 {
		if _, err = jobs.Fetch(context.Background(), "guest", ref.track, true); err != nil {
			t.Fatal(err)
		}
	}
	for deadline = time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		list := jobs.List()
		if len(list) == 2 && list[0].State == "done" && list[0].Playlist == providers.GuestPlaylist {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("guest import %+v", list)
		}
	}
	if enqueued.Load() != 1 {
		t.Fatalf("guest play downloaded again: %d", enqueued.Load())
	}

	var found struct {
		Tracks    tidalList `json:"tracks"`
		Playlists tidalList `json:"playlists"`
		Albums    tidalList `json:"albums"`
	}
	// Search follows the active service; the catalog answers for Deezer.
	if resp, err := http.Post(srv.URL+"/v1/sc5000/ui?scope=deezer", "", nil); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	tidalGet(t, srv.URL, "/v1/search?query=Found&types=TRACKS,PLAYLISTS", 200, &found)
	if found.Tracks.TotalNumberOfItems != 1 || found.Playlists.TotalNumberOfItems != 1 || found.Albums.Items == nil {
		t.Fatalf("search %+v", found)
	}
	tidalGet(t, srv.URL, "/v1/search", 400, nil)
	tidalGet(t, srv.URL, "/v1/tracks/999999", 404, nil)
	tidalGet(t, srv.URL, "/v1/stream/"+id+".exe", 404, nil)
}
