package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/internal/testsmb"
	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

func newTestServerWithUIDir(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		UIDir: dir,
		HTTP:  http.DefaultClient,
	}
	s := New(cfg, nil)
	return s, dir
}

func TestNASConfigCRUD(t *testing.T) {
	s, dir := newTestServerWithUIDir(t)

	// 1. Initial GET - empty config
	req := httptest.NewRequest(http.MethodGet, "/v1/enginex/nas", nil)
	w := httptest.NewRecorder()
	s.handleNASConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var view NASConfigView
	if err := json.NewDecoder(w.Body).Decode(&view); err != nil {
		t.Fatalf("decode view failed: %v", err)
	}
	if view.Enabled || view.HasPassword || view.HasToken {
		t.Fatalf("expected empty config, got %+v", view)
	}

	// 2. POST invalid protocol
	badBody := []byte(`{"protocol":"ftp","url":"http://192.168.1.10:4533"}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", bytes.NewReader(badBody))
	w = httptest.NewRecorder()
	s.handleNASConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad protocol, got %d", w.Code)
	}

	// 3. POST invalid URL
	badURLBody := []byte(`{"protocol":"subsonic","url":"not-a-url"}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", bytes.NewReader(badURLBody))
	w = httptest.NewRecorder()
	s.handleNASConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad URL, got %d", w.Code)
	}

	// 4. POST valid config
	validBody := []byte(`{
		"protocol": "subsonic",
		"url": "http://192.168.1.10:4533",
		"username": "admin",
		"password": "supersecretpassword",
		"token": "secrettoken",
		"enabled": true
	}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", bytes.NewReader(validBody))
	w = httptest.NewRecorder()
	s.handleNASConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.NewDecoder(w.Body).Decode(&view); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if view.Password != "" || !view.HasPassword {
		t.Fatalf("expected masked password, got %+v", view)
	}
	if view.Token != "" || !view.HasToken {
		t.Fatalf("expected masked token, got %+v", view)
	}

	// Check file on disk has 0600 permissions and contains plaintext secrets
	filePath := filepath.Join(dir, "nas.json")
	st, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("nas.json not created: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected 0600 permissions, got %o", perm)
	}
	rawDisk, _ := os.ReadFile(filePath)
	var onDisk providers.NASConfig
	_ = json.Unmarshal(rawDisk, &onDisk)
	if onDisk.Password != "supersecretpassword" || onDisk.Token != "secrettoken" {
		t.Fatalf("secrets not persisted accurately: %+v", onDisk)
	}

	// 5. POST update with empty / "***" password and token -> should keep existing secrets
	updateBody := []byte(`{
		"protocol": "subsonic",
		"url": "http://192.168.1.10:4533",
		"username": "admin2",
		"password": "***",
		"token": "",
		"enabled": true
	}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", bytes.NewReader(updateBody))
	w = httptest.NewRecorder()
	s.handleNASConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify secrets were retained
	rawDisk, _ = os.ReadFile(filePath)
	_ = json.Unmarshal(rawDisk, &onDisk)
	if onDisk.Username != "admin2" || onDisk.Password != "supersecretpassword" || onDisk.Token != "secrettoken" {
		t.Fatalf("secrets were unexpectedly overwritten: %+v", onDisk)
	}

	// 6. Removing the source removes saved credentials too.
	req = httptest.NewRequest(http.MethodDelete, "/v1/enginex/nas", nil)
	w = httptest.NewRecorder()
	s.handleNASConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatalf("NAS config remains after removal: %v", err)
	}
	w = httptest.NewRecorder()
	s.handleNASConfig(w, httptest.NewRequest(http.MethodPut, "/api/nas", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for PUT, got %d", w.Code)
	}
}

func TestNASTestAndPlaylists(t *testing.T) {
	mockBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/rest/getPlaylists.view" {
			fmt.Fprint(w, `{
				"subsonic-response": {
					"status": "ok",
					"version": "1.16.1",
					"playlists": {
						"playlist": [
							{"id": "p1", "name": "Favorites", "songCount": 10}
						]
					}
				}
			}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer mockBackend.Close()

	s, _ := newTestServerWithUIDir(t)

	// 1. handleNASTest with body
	testBody := fmt.Sprintf(`{
		"protocol": "subsonic",
		"url": %q,
		"username": "test",
		"password": "pwd",
		"enabled": true
	}`, mockBackend.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/enginex/nas/test", bytes.NewReader([]byte(testBody)))
	w := httptest.NewRecorder()
	s.handleNASTest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleNASTest failed (%d): %s", w.Code, w.Body.String())
	}
	var res map[string]any
	json.NewDecoder(w.Body).Decode(&res)
	if res["ok"] != true || res["playlists"] != float64(1) {
		t.Fatalf("unexpected test response: %+v", res)
	}

	// 2. Save config and test using saved config
	reqSave := httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", bytes.NewReader([]byte(testBody)))
	wSave := httptest.NewRecorder()
	s.handleNASConfig(wSave, reqSave)
	if wSave.Code != http.StatusOK {
		t.Fatalf("save failed: %s", wSave.Body.String())
	}

	reqSavedTest := httptest.NewRequest(http.MethodPost, "/v1/enginex/nas/test", nil)
	wSavedTest := httptest.NewRecorder()
	s.handleNASTest(wSavedTest, reqSavedTest)
	if wSavedTest.Code != http.StatusOK {
		t.Fatalf("saved test failed: %s", wSavedTest.Body.String())
	}

	// 3. nasPlaylists should return the playlists and provider
	pls, prov := s.nasPlaylists(context.Background())
	if prov == nil {
		t.Fatalf("expected non-nil provider")
	}
	if len(pls) != 1 || pls[0].ID != "p1" {
		t.Fatalf("unexpected playlists: %+v", pls)
	}

	// 4. If disabled, nasPlaylists returns nil, nil
	disabledBody := fmt.Sprintf(`{
		"protocol": "subsonic",
		"url": %q,
		"enabled": false
	}`, mockBackend.URL)
	reqDisable := httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", bytes.NewReader([]byte(disabledBody)))
	wDisable := httptest.NewRecorder()
	s.handleNASConfig(wDisable, reqDisable)

	dPls, dProv := s.nasPlaylists(context.Background())
	if dPls != nil || dProv != nil {
		t.Fatalf("expected nil, nil when disabled, got pls=%+v, prov=%+v", dPls, dProv)
	}
}

func TestNASConfigAndProbeRejectLoopbackAndLinkLocal(t *testing.T) {
	orig := nasAllowLoopback
	nasAllowLoopback = false
	t.Cleanup(func() { nasAllowLoopback = orig })

	s, _ := newTestServerWithUIDir(t)
	for _, target := range []string{
		"http://127.0.0.1:6595",
		"http://localhost:8080",
		"http://[::1]:6596",
		"http://0.0.0.0:8080",
		"http://169.254.169.254/latest/meta-data",
	} {
		body := fmt.Sprintf(`{"protocol":"webdav","url":%q,"enabled":true}`, target)
		reqSave := httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", bytes.NewReader([]byte(body)))
		wSave := httptest.NewRecorder()
		s.handleNASConfig(wSave, reqSave)
		if wSave.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 saving %s, got %d: %s", target, wSave.Code, wSave.Body.String())
		}

		reqTest := httptest.NewRequest(http.MethodPost, "/v1/enginex/nas/test", bytes.NewReader([]byte(body)))
		wTest := httptest.NewRecorder()
		s.handleNASTest(wTest, reqTest)
		if wTest.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 testing %s, got %d: %s", target, wTest.Code, wTest.Body.String())
		}
	}
}

func TestOmniSourceNASPlaylistsBrowseAndStream(t *testing.T) {
	mp3Bytes := testMP3(120)
	nasSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/getPlaylists.view":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlists":{"playlist":[{"id":"pl-42","name":"Club Crate","songCount":1}]}}}`)
		case "/rest/getPlaylist.view":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlist":{"id":"pl-42","name":"Club Crate","entry":[{"id":"tr-99","title":"Warehouse Dub","artist":"DJ Test","album":"NAS Vol 1","duration":210,"suffix":"mp3"}]}}}`)
		case "/rest/download.view":
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(mp3Bytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer nasSrv.Close()

	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("scope=spotify\nomni_source=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: dlDir, AllowLossy: true},
	})
	t.Cleanup(jobs.Close)

	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	s.sessions["sp-owner"] = &Session{ID: "sp-owner", Provider: "spotify", Ready: true, p: &browseTestProvider{}}

	cfgBody := fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":"dj","password":"pw","enabled":true}`, nasSrv.URL)
	wCfg := httptest.NewRecorder()
	s.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(cfgBody)))
	if wCfg.Code != http.StatusOK {
		t.Fatalf("save NAS config failed (%d): %s", wCfg.Code, wCfg.Body.String())
	}

	gw := httptest.NewServer(s)
	defer gw.Close()

	var lists struct {
		Items []map[string]any `json:"items"`
	}
	tidalGet(t, gw.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &lists)
	var nasUUID string
	for _, it := range lists.Items {
		if it["title"] == "[NAS] Club Crate" {
			nasUUID, _ = it["uuid"].(string)
		}
	}
	if nasUUID == "" {
		t.Fatalf("expected [NAS] Club Crate in OmniSource playlists, got %+v", lists.Items)
	}

	var tracks struct {
		Items []map[string]any `json:"items"`
	}
	tidalGet(t, gw.URL, "/v1/playlists/"+nasUUID+"/tracks?limit=50&offset=0", 200, &tracks)
	if len(tracks.Items) != 1 || tracks.Items[0]["title"] != "Warehouse Dub" {
		t.Fatalf("unexpected NAS playlist tracks: %+v", tracks.Items)
	}
	trackID := int64(tracks.Items[0]["id"].(float64))

	var pb map[string]any
	tidalGet(t, gw.URL, fmt.Sprintf("/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", trackID), 200, &pb)

	resp, err := http.Get(fmt.Sprintf("%s/v1/stream/%d.mp3", gw.URL, trackID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	gotAudio, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(gotAudio, mp3Bytes) {
		t.Fatalf("NAS stream status=%d len=%d want=%d body=%q", resp.StatusCode, len(gotAudio), len(mp3Bytes), string(gotAudio[:min(len(gotAudio), 120)]))
	}
}

func TestNASStandaloneSourceWithoutOmniSource(t *testing.T) {
	mp3Bytes := testMP3(120)
	nasSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/getPlaylists.view":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlists":{"playlist":[{"id":"pl-42","name":"Club Crate","songCount":1}]}}}`)
		case "/rest/getPlaylist.view":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlist":{"id":"pl-42","name":"Club Crate","entry":[{"id":"tr-99","title":"Warehouse Dub","artist":"DJ Test","album":"NAS Vol 1","duration":210,"suffix":"mp3"}]}}}`)
		case "/rest/search3.view":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"subsonic-response":{"status":"ok","searchResult3":{"song":[{"id":"tr-99","title":"Warehouse Dub","artist":"DJ Test","album":"NAS Vol 1","duration":210,"suffix":"mp3"}]}}}`)
		case "/rest/download.view":
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(mp3Bytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer nasSrv.Close()

	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	// OmniSource is OFF, all streaming services are switched off, and no streaming account is logged in.
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=\nomni_source=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: dlDir, AllowLossy: true},
	})
	t.Cleanup(jobs.Close)

	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	gw := httptest.NewServer(s)
	defer gw.Close()

	hasIn := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}

	// 1. Before NAS is configured: absent from sources/loggedIn and scope=nas returns 409.
	var status uiSettings
	tidalGet(t, gw.URL, "/v1/enginex/ui", 200, &status)
	if hasIn(status.Sources, "nas") || hasIn(status.LoggedIn, "nas") {
		t.Fatalf("unconfigured NAS should not appear in UI status: sources=%v loggedIn=%v", status.Sources, status.LoggedIn)
	}
	if code, _ := uiPost(t, gw.URL, "scope=nas"); code != http.StatusConflict {
		t.Fatalf("unconfigured scope=nas: got HTTP %d, want 409", code)
	}

	// 2. Save enabled NAS config: NAS appears dynamically in sources & loggedIn without OmniSource.
	cfgBody := fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":"dj","password":"pw","enabled":true}`, nasSrv.URL)
	wCfg := httptest.NewRecorder()
	s.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(cfgBody)))
	if wCfg.Code != http.StatusOK {
		t.Fatalf("save NAS config failed (%d): %s", wCfg.Code, wCfg.Body.String())
	}

	status = uiSettings{}
	tidalGet(t, gw.URL, "/v1/enginex/ui", 200, &status)
	if !hasIn(status.Sources, "nas") || !hasIn(status.LoggedIn, "nas") {
		t.Fatalf("configured NAS missing from UI status: sources=%v loggedIn=%v", status.Sources, status.LoggedIn)
	}

	// 3. Selecting scope=nas succeeds without any streaming login or OmniSource and does not persist nas into sources=.
	if code, updated := uiPost(t, gw.URL, "scope=nas"); code != http.StatusOK || updated.Scope != "nas" {
		t.Fatalf("scope=nas failed: code=%d scope=%q", code, updated.Scope)
	}
	if raw, err := os.ReadFile(filepath.Join(uiDir, "settings.conf")); err != nil || strings.Contains(string(raw), "sources=nas") {
		t.Fatalf("dynamic nas source must not be persisted into settings.conf sources=: %q %v", string(raw), err)
	}

	// 4. Both /playlists and /favorites/playlists list NAS playlists cleanly (no [NAS] prefix needed).
	for _, route := range []string{"/v1/users/1/playlists?limit=50&offset=0", "/v1/users/1/favorites/playlists?limit=50&offset=0"} {
		var lists struct {
			Items []map[string]any `json:"items"`
		}
		tidalGet(t, gw.URL, route, 200, &lists)
		var nasUUID string
		for _, it := range lists.Items {
			if it["title"] == "Club Crate" {
				nasUUID, _ = it["uuid"].(string)
			}
		}
		if nasUUID == "" {
			t.Fatalf("%s: expected standalone NAS playlist 'Club Crate', got %+v", route, lists.Items)
		}

		// 5. Playlist detail, track list, NAS row label, X-Enginex-Provider header, playback & stream all work.
		var detail map[string]any
		tidalGet(t, gw.URL, "/v1/playlists/"+nasUUID, 200, &detail)
		if detail["title"] != "Club Crate" {
			t.Fatalf("playlist detail title=%v, want Club Crate", detail["title"])
		}

		var tracks struct {
			Items []map[string]any `json:"items"`
		}
		tidalGet(t, gw.URL, "/v1/playlists/"+nasUUID+"/tracks?limit=50&offset=0", 200, &tracks)
		if len(tracks.Items) != 1 || tracks.Items[0]["title"] != "Warehouse Dub" {
			t.Fatalf("unexpected NAS tracks: %+v", tracks.Items)
		}
		if got, _ := tracks.Items[0]["copyright"].(string); !strings.HasPrefix(got, "NAS") {
			t.Fatalf("NAS row copyright=%q, want prefix NAS", got)
		}
		trackID := int64(tracks.Items[0]["id"].(float64))

		reqStem, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v1/enginex/stems/local?id=%d", gw.URL, trackID), nil)
		respStem, err := http.DefaultClient.Do(reqStem)
		if err != nil {
			t.Fatal(err)
		}
		respStem.Body.Close()
		if got := respStem.Header.Get(providerHeader); got != "nas" {
			t.Fatalf("%s = %q, want %q", providerHeader, got, "nas")
		}

		var pb map[string]any
		tidalGet(t, gw.URL, fmt.Sprintf("/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", trackID), 200, &pb)
		resp, err := http.Get(fmt.Sprintf("%s/v1/stream/%d.mp3", gw.URL, trackID))
		if err != nil {
			t.Fatal(err)
		}
		gotAudio, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(gotAudio, mp3Bytes) {
			t.Fatalf("NAS stream status=%d len=%d want=%d", resp.StatusCode, len(gotAudio), len(mp3Bytes))
		}
	}

	// 6. Search works inside the standalone NAS scope.
	var searchRes struct {
		Tracks struct {
			Items []map[string]any `json:"items"`
		} `json:"tracks"`
	}
	tidalGet(t, gw.URL, "/v1/search?query=Warehouse&limit=50&offset=0", 200, &searchRes)
	if len(searchRes.Tracks.Items) != 1 || searchRes.Tracks.Items[0]["title"] != "Warehouse Dub" {
		t.Fatalf("NAS search returned %+v", searchRes.Tracks.Items)
	}

	// 7. Deleting NAS removes it from UI status and rejects scope=nas with 409.
	wDel := httptest.NewRecorder()
	s.handleNASConfig(wDel, httptest.NewRequest(http.MethodDelete, "/v1/enginex/nas", nil))
	if wDel.Code != http.StatusOK {
		t.Fatalf("delete NAS failed (%d): %s", wDel.Code, wDel.Body.String())
	}
	status = uiSettings{}
	tidalGet(t, gw.URL, "/v1/enginex/ui", 200, &status)
	if hasIn(status.Sources, "nas") || hasIn(status.LoggedIn, "nas") {
		t.Fatalf("deleted NAS still in UI status: sources=%v loggedIn=%v", status.Sources, status.LoggedIn)
	}
	if code, _ := uiPost(t, gw.URL, "scope=nas"); code != http.StatusConflict {
		t.Fatalf("deleted scope=nas: got HTTP %d, want 409", code)
	}
}

func TestNASSMBTrackRoutesToSelfHostedDownloader(t *testing.T) {
	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	jobs := NewJobs(Pipeline{
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: dlDir, AllowLossy: false},
	})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)

	// 1. jobs.Fetch must dispatch smb tracks to fromSelfHosted (not "no audio source for this service").
	_, err := jobs.Fetch(context.Background(), "nas", providers.Track{ID: "./Sets/track.mp3", Title: "Track", Provider: "smb"}, false)
	if err == nil {
		t.Fatal("expected error when NAS is not enabled")
	}
	if strings.Contains(err.Error(), "no audio source for this service") {
		t.Fatalf("smb track was not dispatched to fromSelfHosted: %v", err)
	}
	if !strings.Contains(err.Error(), "NAS server is not enabled") {
		t.Fatalf("unexpected error for disabled SMB NAS: %v", err)
	}

	// 2. knownCodec must treat smb as variable-codec even when lossless (AllowLossy=false),
	// so it calls Fetch before building the playback manifest instead of claiming FLAC.
	ref := trackRef{track: providers.Track{ID: "./Sets/track.mp3", Title: "Track", Provider: "smb"}, session: "nas"}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tracks/1/playbackinfopostpaywall", nil)
	if ok := s.tidal.knownCodec(w, req, ref); ok || w.Code != http.StatusBadGateway {
		t.Fatalf("knownCodec(smb) ok=%t status=%d, want ok=false status=502 (Fetch attempted)", ok, w.Code)
	}
}

func TestNASStandaloneSMBSource59PlaylistsEndToEnd(t *testing.T) {
	smbSrv := testsmb.Start(t)
	mp3Bytes := testMP3(120)
	// Mirror the #335 Synology SMB layout: Root + 58 subfolders = 59 playlists.
	for i := 1; i <= 58; i++ {
		dir := filepath.Join(smbSrv.Root, fmt.Sprintf("Crate %02d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if i == 58 {
			if err := os.WriteFile(filepath.Join(dir, "Closing Track.mp3"), mp3Bytes, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=\nomni_source=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: dlDir, AllowLossy: false},
	})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	gw := httptest.NewServer(s)
	defer gw.Close()

	cfgBody := fmt.Sprintf(`{"protocol":"smb","url":%q,"username":%q,"password":%q,"enabled":true}`, smbSrv.URL, smbSrv.User, smbSrv.Password)
	wCfg := httptest.NewRecorder()
	s.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(cfgBody)))
	if wCfg.Code != http.StatusOK {
		t.Fatalf("save SMB NAS config (%d): %s", wCfg.Code, wCfg.Body.String())
	}

	// Verify /v1/enginex/nas/test reports 59 playlists.
	wTest := httptest.NewRecorder()
	s.handleNASTest(wTest, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas/test", nil))
	if wTest.Code != http.StatusOK {
		t.Fatalf("nas/test failed (%d): %s", wTest.Code, wTest.Body.String())
	}
	var testRes struct {
		OK        bool `json:"ok"`
		Playlists int  `json:"playlists"`
	}
	if err := json.Unmarshal(wTest.Body.Bytes(), &testRes); err != nil || !testRes.OK || testRes.Playlists != 59 {
		t.Fatalf("nas/test got %+v %v, want ok=true playlists=59", testRes, err)
	}

	// Select standalone NAS scope with OmniSource OFF and paginate all 59 folders.
	if code, updated := uiPost(t, gw.URL, "scope=nas"); code != http.StatusOK || updated.Scope != "nas" {
		t.Fatalf("scope=nas failed: code=%d scope=%q", code, updated.Scope)
	}
	var page1, page2 struct {
		Total int              `json:"totalNumberOfItems"`
		Items []map[string]any `json:"items"`
	}
	tidalGet(t, gw.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &page1)
	tidalGet(t, gw.URL, "/v1/users/1/playlists?limit=50&offset=50", 200, &page2)
	if page1.Total != 59 || len(page1.Items) != 50 || len(page2.Items) != 9 {
		t.Fatalf("SMB pagination: total=%d p1=%d p2=%d, want 59/50/9", page1.Total, len(page1.Items), len(page2.Items))
	}

	var crate58UUID string
	for _, it := range append(page1.Items, page2.Items...) {
		if it["title"] == "Crate 58" {
			crate58UUID, _ = it["uuid"].(string)
		}
	}
	if crate58UUID == "" {
		t.Fatal("missing Crate 58 across paginated SMB NAS listings")
	}

	var detail map[string]any
	tidalGet(t, gw.URL, "/v1/playlists/"+crate58UUID, 200, &detail)
	if detail["title"] != "Crate 58" {
		t.Fatalf("detail title=%v", detail["title"])
	}
	var tracks struct {
		Items []map[string]any `json:"items"`
	}
	tidalGet(t, gw.URL, "/v1/playlists/"+crate58UUID+"/tracks?limit=50&offset=0", 200, &tracks)
	if len(tracks.Items) != 1 || tracks.Items[0]["title"] != "Closing Track" {
		t.Fatalf("SMB folder tracks: %+v", tracks.Items)
	}
	if got, _ := tracks.Items[0]["copyright"].(string); !strings.HasPrefix(got, "NAS") {
		t.Fatalf("SMB track copyright=%q, want NAS prefix", got)
	}
	trackID := int64(tracks.Items[0]["id"].(float64))

	// Playback manifest must detect MP3 via knownCodec+Fetch (not claim FLAC) and stream exact bytes.
	var pb struct {
		Manifest string `json:"manifest"`
	}
	tidalGet(t, gw.URL, fmt.Sprintf("/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", trackID), 200, &pb)
	rawManifest, err := base64.StdEncoding.DecodeString(pb.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var mf struct {
		MimeType string   `json:"mimeType"`
		Codecs   string   `json:"codecs"`
		URLs     []string `json:"urls"`
	}
	if err := json.Unmarshal(rawManifest, &mf); err != nil || mf.Codecs != "mp3" || mf.MimeType != "audio/mpeg" || len(mf.URLs) != 1 {
		t.Fatalf("SMB manifest=%s err=%v", rawManifest, err)
	}
	resp, err := http.Get(mf.URLs[0])
	if err != nil {
		t.Fatal(err)
	}
	gotAudio, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(gotAudio, mp3Bytes) {
		t.Fatalf("SMB stream status=%d len=%d want=%d", resp.StatusCode, len(gotAudio), len(mp3Bytes))
	}
}

func TestNASErrorsRedactCredentialsInGatewayAndLog(t *testing.T) {
	dropSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}))
	defer dropSrv.Close()

	const (
		dummyUser  = "dummy-nas-user"
		dummyPass  = "dummy-nas-pass-999"
		dummyToken = "dummy-nas-token-999"
	)
	forbidden := []string{dummyPass, dummyToken, dummyUser, "t=", "s=", "p=", "X-Plex-Token", "?"}
	assertNoLeak := func(t *testing.T, label, text string) {
		t.Helper()
		for _, bad := range forbidden {
			if strings.Contains(text, bad) {
				t.Errorf("%s leaked %q: %s", label, bad, text)
			}
		}
	}

	cases := []struct {
		name    string
		cfgJSON string
	}{
		{
			name:    "subsonic-password",
			cfgJSON: fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":%q,"password":%q,"enabled":true}`, dropSrv.URL, dummyUser, dummyPass),
		},
		{
			name:    "subsonic-token",
			cfgJSON: fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":%q,"token":%q,"enabled":true}`, dropSrv.URL, dummyUser, dummyToken),
		},
		{
			name:    "plex-token",
			cfgJSON: fmt.Sprintf(`{"protocol":"plex","url":%q,"token":%q,"enabled":true}`, dropSrv.URL, dummyToken),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uiDir, stateDir := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=nas\nomni_source=0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			s := New(Config{UIDir: uiDir, StateDir: stateDir, HTTP: dropSrv.Client()}, nil)
			defer s.Close()

			wCfg := httptest.NewRecorder()
			s.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(tc.cfgJSON)))
			if wCfg.Code != http.StatusOK {
				t.Fatalf("save NAS config failed (%d): %s", wCfg.Code, wCfg.Body.String())
			}

			// 1. /v1/users/1/playlists hint playlist JSON must not leak credentials.
			reqList := httptest.NewRequest(http.MethodGet, "/v1/users/1/playlists?limit=50&offset=0", nil)
			reqList.RemoteAddr = "127.0.0.1:1234"
			wList := httptest.NewRecorder()
			s.tidal.ServeHTTP(wList, reqList)
			if wList.Code != http.StatusOK {
				t.Fatalf("playlists status=%d body=%s", wList.Code, wList.Body.String())
			}
			hintBody := wList.Body.String()
			if !strings.Contains(hintBody, hintPlaylist) {
				t.Fatalf("expected hint playlist on dropped NAS, got: %s", hintBody)
			}
			assertNoLeak(t, "hint playlist JSON", hintBody)

			// 2. /v1/playlists/{uuid}/tracks HTTP error body and stream-sync log line must not leak credentials.
			plUUID := s.tidal.addPlaylist(playlistRef{
				session:  nasScope,
				provider: nasScope,
				list:     providers.Playlist{ID: "pl-1", Title: "Crate"},
			})
			var logBuf bytes.Buffer
			log.SetOutput(&logBuf)
			defer log.SetOutput(os.Stderr)

			reqTracks := httptest.NewRequest(http.MethodGet, "/v1/playlists/"+plUUID+"/tracks?limit=50&offset=0", nil)
			reqTracks.RemoteAddr = "127.0.0.1:1234"
			wTracks := httptest.NewRecorder()
			s.tidal.ServeHTTP(wTracks, reqTracks)
			log.SetOutput(os.Stderr)

			if wTracks.Code != http.StatusBadGateway {
				t.Fatalf("tracks status=%d body=%s", wTracks.Code, wTracks.Body.String())
			}
			assertNoLeak(t, "tracks HTTP error", wTracks.Body.String())
			logged := logBuf.String()
			if !strings.Contains(logged, "stream-sync: playlist open failed") {
				t.Fatalf("expected playlist open failure log, got: %q", logged)
			}
			for _, bad := range []string{dummyPass, dummyToken, dummyUser, "X-Plex-Token", "?"} {
				if strings.Contains(logged, bad) {
					t.Errorf("playlist open failure log leaked %q: %s", bad, logged)
				}
			}
			_, errPart, ok := strings.Cut(logged, "error=")
			if !ok {
				t.Fatalf("playlist open failure log missing error= field: %q", logged)
			}
			assertNoLeak(t, "playlist open failure log error=", errPart)

			// 3. /v1/enginex/nas/test error body must not leak credentials.
			wTest := httptest.NewRecorder()
			s.handleNASTest(wTest, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas/test", strings.NewReader("{}")))
			if wTest.Code != http.StatusBadGateway {
				t.Fatalf("nas test status=%d body=%s", wTest.Code, wTest.Body.String())
			}
			assertNoLeak(t, "nas test error", wTest.Body.String())
		})
	}
}

func TestNASPlaylistOpenFailureEscapesLogInjection(t *testing.T) {
	dropSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}))
	defer dropSrv.Close()

	uiDir, stateDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=nas\nomni_source=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: uiDir, StateDir: stateDir, HTTP: dropSrv.Client()}, nil)
	defer s.Close()

	cfgJSON := fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":"dj","password":"pw","enabled":true}`, dropSrv.URL)
	wCfg := httptest.NewRecorder()
	s.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(cfgJSON)))
	if wCfg.Code != http.StatusOK {
		t.Fatalf("save NAS config failed (%d): %s", wCfg.Code, wCfg.Body.String())
	}

	hostileID := "./Club\nstream-sync: forged=1\r\x1b[31m"
	plUUID := s.tidal.addPlaylist(playlistRef{
		session:  nasScope,
		provider: nasScope,
		list:     providers.Playlist{ID: hostileID, Title: "Club"},
	})

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	req := httptest.NewRequest(http.MethodGet, "/v1/playlists/"+plUUID+"/tracks?limit=50&offset=0", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.tidal.ServeHTTP(w, req)
	log.SetOutput(os.Stderr)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("tracks status=%d body=%s", w.Code, w.Body.String())
	}
	line := logBuf.String()
	if !strings.Contains(line, "stream-sync: playlist open failed") {
		t.Fatalf("missing playlist open failure log: %q", line)
	}
	if strings.Count(strings.TrimSpace(line), "\n") != 0 || strings.ContainsAny(line, "\r\x1b") {
		t.Fatalf("log injection not neutralized: %q", line)
	}
	wantSource := `source_id=nas:"Club\nstream-sync: forged=1\r\x1b[31m"`
	if !strings.Contains(line, wantSource) {
		t.Fatalf("log line missing quoted source_id %q: %q", wantSource, line)
	}

	// Verify OmniSource localProvider nas: IDs are also quoted and cannot inject newlines.
	omniRef := &playlistRef{
		session:  localProvider,
		provider: localProvider,
		list:     providers.Playlist{ID: "nas\x00./Crate\nforged=2"},
	}
	if got := playlistOpenSourceID(omniRef); got != `nas:"./Crate\nforged=2"` {
		t.Fatalf("playlistOpenSourceID(local nas)=%q, want quoted", got)
	}

	// Verify newline in err (e.g. SMB path error echoing a hostile folder name) stays on one log line.
	logBuf.Reset()
	log.SetOutput(&logBuf)
	s.tidal.logPlaylistOpenFailure(req, http.StatusBadGateway, plUUID, &playlistRef{
		session:  nasScope,
		provider: nasScope,
		list:     providers.Playlist{ID: hostileID},
	}, fmt.Errorf("open %s: file does not exist", hostileID))
	log.SetOutput(os.Stderr)
	errLine := logBuf.String()
	if strings.Count(strings.TrimSpace(errLine), "\n") != 0 || strings.ContainsAny(errLine, "\r\x1b") {
		t.Fatalf("error log injection not neutralized: %q", errLine)
	}
}

func TestNASRedirectAndUpstreamErrorsNeverLeakInGatewayAndStream(t *testing.T) {
	const (
		dummyUser  = "dummy-nas-user"
		dummyPass  = "dummy-nas-pass-999"
		dummyToken = "dummy-nas-token-999"
		redirUser  = "redir-user"
		redirPass  = "redir-pass-999"
		redirToken = "redir-tok-999"
		redirPlex  = "redir-plex-999"
	)
	forbidden := []string{
		dummyUser, dummyPass, dummyToken,
		redirUser, redirPass, redirToken, redirPlex,
		"evil.invalid", "t=", "s=", "p=", "X-Plex-Token", "?",
		"forged=1",
	}
	assertNoLeak := func(t *testing.T, label, text string) {
		t.Helper()
		for _, bad := range forbidden {
			if strings.Contains(text, bad) {
				t.Errorf("%s leaked %q: %s", label, bad, text)
			}
		}
	}

	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://"+redirUser+":"+redirPass+"@evil.invalid/steal%zz?u="+redirUser+"&t="+redirToken+"&s=salt&X-Plex-Token="+redirPlex)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirSrv.Close()

	upstreamErrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"subsonic-response":{"status":"failed","error":{"code":40,"message":"auth failed for u=%s&p=%s\nstream-sync: forged=1"}}}`, dummyUser, dummyPass)
	}))
	defer upstreamErrSrv.Close()

	cases := []struct {
		name     string
		client   *http.Client
		provider string
		trackID  string
		cfgJSON  string
	}{
		{
			name:     "subsonic-malformed-redirect",
			client:   redirSrv.Client(),
			provider: "subsonic",
			trackID:  "s-1",
			cfgJSON:  fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":%q,"password":%q,"enabled":true}`, redirSrv.URL, dummyUser, dummyPass),
		},
		{
			name:     "plex-malformed-redirect",
			client:   redirSrv.Client(),
			provider: "plex",
			trackID:  "/library/parts/1/song.flac",
			cfgJSON:  fmt.Sprintf(`{"protocol":"plex","url":%q,"token":%q,"enabled":true}`, redirSrv.URL, dummyToken),
		},
		{
			name:     "subsonic-upstream-message",
			client:   upstreamErrSrv.Client(),
			provider: "subsonic",
			trackID:  "s-1",
			cfgJSON:  fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":%q,"password":%q,"enabled":true}`, upstreamErrSrv.URL, dummyUser, dummyPass),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=nas\nomni_source=0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			jobs := NewJobs(Pipeline{
				Catalog: noCatalog{},
				Deemix:  &deemixclient.Client{DownloadDir: dlDir},
			})
			defer jobs.Close()
			s := New(Config{UIDir: uiDir, StateDir: stateDir, HTTP: tc.client}, jobs)
			defer s.Close()

			wCfg := httptest.NewRecorder()
			s.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(tc.cfgJSON)))
			if wCfg.Code != http.StatusOK {
				t.Fatalf("save NAS config (%d): %s", wCfg.Code, wCfg.Body.String())
			}

			// 1. NAS hint playlist JSON (/v1/users/1/playlists).
			reqList := httptest.NewRequest(http.MethodGet, "/v1/users/1/playlists?limit=50&offset=0", nil)
			reqList.RemoteAddr = "127.0.0.1:1234"
			wList := httptest.NewRecorder()
			s.tidal.ServeHTTP(wList, reqList)
			if wList.Code != http.StatusOK || !strings.Contains(wList.Body.String(), hintPlaylist) {
				t.Fatalf("expected hint playlist, status=%d body=%s", wList.Code, wList.Body.String())
			}
			assertNoLeak(t, "hint playlist JSON", wList.Body.String())

			// 2. Playlist open error body + stream-sync log line.
			plUUID := s.tidal.addPlaylist(playlistRef{
				session:  nasScope,
				provider: nasScope,
				list:     providers.Playlist{ID: "pl-1", Title: "Crate"},
			})
			var logBuf bytes.Buffer
			log.SetOutput(&logBuf)
			defer log.SetOutput(os.Stderr)

			reqTracks := httptest.NewRequest(http.MethodGet, "/v1/playlists/"+plUUID+"/tracks?limit=50&offset=0", nil)
			reqTracks.RemoteAddr = "127.0.0.1:1234"
			wTracks := httptest.NewRecorder()
			s.tidal.ServeHTTP(wTracks, reqTracks)
			log.SetOutput(os.Stderr)

			if wTracks.Code != http.StatusBadGateway {
				t.Fatalf("tracks status=%d body=%s", wTracks.Code, wTracks.Body.String())
			}
			assertNoLeak(t, "tracks HTTP error", wTracks.Body.String())
			logged := logBuf.String()
			if !strings.Contains(logged, "stream-sync: playlist open failed") {
				t.Fatalf("missing playlist open failure log: %q", logged)
			}
			_, errPart, ok := strings.Cut(logged, "error=")
			if !ok {
				t.Fatalf("log missing error=: %q", logged)
			}
			assertNoLeak(t, "playlist open failure log error=", errPart)
			if strings.Count(strings.TrimSpace(logged), "\n") != 0 {
				t.Fatalf("log line split by newline: %q", logged)
			}

			// 3. Stream endpoint error body (/v1/stream/{id}.flac).
			tid := s.tidal.addTrack(trackRef{
				session: nasScope,
				track:   providers.Track{ID: tc.trackID, Title: "Track", Provider: tc.provider},
			})
			reqStream := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/stream/%d.flac", tid), nil)
			reqStream.RemoteAddr = "127.0.0.1:1234"
			wStream := httptest.NewRecorder()
			s.tidal.ServeHTTP(wStream, reqStream)
			if wStream.Code != http.StatusBadGateway {
				t.Fatalf("stream status=%d body=%s", wStream.Code, wStream.Body.String())
			}
			assertNoLeak(t, "stream HTTP error", wStream.Body.String())

			// 4. NAS test endpoint error body (/v1/enginex/nas/test).
			wTest := httptest.NewRecorder()
			s.handleNASTest(wTest, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas/test", strings.NewReader("{}")))
			if wTest.Code != http.StatusBadGateway {
				t.Fatalf("nas test status=%d body=%s", wTest.Code, wTest.Body.String())
			}
			assertNoLeak(t, "nas test error", wTest.Body.String())
		})
	}
}

func TestNASJobResponsesAndSavedTracksStripCoverCredentials(t *testing.T) {
	const (
		dummyUser  = "dummy-nas-user"
		dummyPass  = "dummy-nas-pass-999"
		dummyToken = "dummy-nas-token-999"
	)
	var flacBuf bytes.Buffer
	if err := testaudio.FLAC(&flacBuf, testaudio.Song(8)); err != nil {
		t.Fatal(err)
	}
	flacBytes := flacBuf.Bytes()

	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: 40, G: 120, B: 200, A: 255})
		}
	}
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	jpegBytes := jpegBuf.Bytes()

	forbidden := []string{
		dummyUser, dummyPass, dummyToken,
		"X-Plex-Token", "t=", "s=", "p=", "u=",
	}
	assertNoLeak := func(t *testing.T, label, text string) {
		t.Helper()
		for _, bad := range forbidden {
			if strings.Contains(text, bad) {
				t.Errorf("%s leaked %q: %s", label, bad, text)
			}
		}
	}

	for _, proto := range []string{"subsonic", "plex"} {
		t.Run(proto, func(t *testing.T) {
			var coverFetched atomic.Int32
			nasSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch proto {
				case "subsonic":
					switch r.URL.Path {
					case "/rest/getPlaylists.view":
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlists":{"playlist":[{"id":"pl-1","name":"NAS Crate","songCount":1}]}}}`)
					case "/rest/getPlaylist.view":
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlist":{"id":"pl-1","name":"NAS Crate","entry":[{"id":"s-1","title":"Cover Track","artist":"DJ Test","album":"NAS Album","duration":8,"coverArt":"art-42"}]}}}`)
					case "/rest/download.view":
						w.Header().Set("Content-Type", "audio/flac")
						w.Header().Set("Content-Disposition", `attachment; filename="cover.flac"`)
						_, _ = w.Write(flacBytes)
					case "/rest/getCoverArt.view":
						q := r.URL.Query()
						if q.Get("id") != "art-42" || q.Get("u") != dummyUser || (q.Get("p") == "" && (q.Get("t") == "" || q.Get("s") == "")) {
							http.Error(w, "unauthorized cover", http.StatusUnauthorized)
							return
						}
						coverFetched.Add(1)
						w.Header().Set("Content-Type", "image/jpeg")
						_, _ = w.Write(jpegBytes)
					default:
						http.NotFound(w, r)
					}
				case "plex":
					switch r.URL.Path {
					case "/playlists":
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"pl-1","title":"NAS Crate","leafCount":1}]}}`)
					case "/playlists/pl-1/items":
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"tr-1","title":"Cover Track","grandparentTitle":"DJ Test","parentTitle":"NAS Album","duration":8000,"thumb":"/library/metadata/tr-1/thumb/42","Media":[{"Part":[{"key":"/library/parts/1/song.flac"}]}]}]}}`)
					case "/library/parts/1/song.flac":
						w.Header().Set("Content-Type", "audio/flac")
						_, _ = w.Write(flacBytes)
					case "/library/metadata/tr-1/thumb/42":
						if r.URL.Query().Get("X-Plex-Token") != "" {
							http.Error(w, "plex cover token must not be in query", http.StatusBadRequest)
							return
						}
						if r.Header.Get("X-Plex-Token") != dummyToken {
							http.Error(w, "unauthorized cover", http.StatusUnauthorized)
							return
						}
						coverFetched.Add(1)
						w.Header().Set("Content-Type", "image/jpeg")
						_, _ = w.Write(jpegBytes)
					default:
						http.NotFound(w, r)
					}
				}
			}))
			defer nasSrv.Close()

			expectedCover := nasSrv.URL + "/rest/getCoverArt.view?id=art-42"
			if proto == "plex" {
				expectedCover = nasSrv.URL + "/library/metadata/tr-1/thumb/42"
			}

			libPath, libRoot, _ := testlibrary.Create(t)
			lib, err := analyzer.OpenLibrary(libPath)
			if err != nil {
				t.Fatal(err)
			}
			defer lib.Close()

			uiDir, stateDir := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=nas\nomni_source=0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			guard := resourceguard.New(resourceguard.Config{Disabled: true})
			jobs := NewJobs(Pipeline{
				Guard:   guard,
				Catalog: noCatalog{},
				Deemix:  &deemixclient.Client{DownloadDir: libRoot, Media: nasSrv.Client()},
				Library: lib,
			})
			defer jobs.Close()

			s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir, HTTP: nasSrv.Client()}, jobs)
			defer s.Close()
			gw := httptest.NewServer(s)
			defer gw.Close()

			var cfgJSON string
			if proto == "subsonic" {
				cfgJSON = fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":%q,"password":%q,"enabled":true}`, nasSrv.URL, dummyUser, dummyPass)
			} else {
				cfgJSON = fmt.Sprintf(`{"protocol":"plex","url":%q,"token":%q,"enabled":true}`, nasSrv.URL, dummyToken)
			}
			wCfg := httptest.NewRecorder()
			s.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(cfgJSON)))
			if wCfg.Code != http.StatusOK {
				t.Fatalf("save NAS config (%d): %s", wCfg.Code, wCfg.Body.String())
			}

			// 1. Browse playlists & tracks via gateway.
			var plResp struct {
				Items []map[string]any `json:"items"`
			}
			tidalGet(t, gw.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &plResp)
			if len(plResp.Items) != 1 {
				t.Fatalf("expected 1 playlist, got %+v", plResp.Items)
			}
			plUUID, _ := plResp.Items[0]["uuid"].(string)

			var trResp struct {
				Items []map[string]any `json:"items"`
			}
			tidalGet(t, gw.URL, "/v1/playlists/"+plUUID+"/tracks?limit=50&offset=0", 200, &trResp)
			if len(trResp.Items) != 1 {
				t.Fatalf("expected 1 track, got %+v", trResp.Items)
			}
			trackID := int64(trResp.Items[0]["id"].(float64))

			s.tidal.mu.Lock()
			storedCover := s.tidal.tracks[trackID].track.CoverURL
			s.tidal.mu.Unlock()
			if storedCover != expectedCover {
				t.Fatalf("gateway track CoverURL = %q, want %q", storedCover, expectedCover)
			}

			// 2. Persisted gateway track map keeps the credential-free cover URL and leaks no credentials.
			s.tidal.saveTracks()
			savedBytes, err := os.ReadFile(s.tidal.tracksPath())
			if err != nil {
				t.Fatalf("read saved tracks: %v", err)
			}
			assertNoLeak(t, "gateway-tracks.json", string(savedBytes))
			if !strings.Contains(string(savedBytes), expectedCover) {
				t.Fatalf("gateway-tracks.json dropped credential-free cover %q: %s", expectedCover, string(savedBytes))
			}

			// 3. Stream the track: downloads audio, embeds authenticated cover, and queues import job.
			var pb struct {
				Manifest string `json:"manifest"`
			}
			tidalGet(t, gw.URL, fmt.Sprintf("/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", trackID), 200, &pb)
			rawManifest, err := base64.StdEncoding.DecodeString(pb.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			var mf struct {
				URLs []string `json:"urls"`
			}
			if err := json.Unmarshal(rawManifest, &mf); err != nil || len(mf.URLs) != 1 {
				t.Fatalf("manifest=%s err=%v", rawManifest, err)
			}
			resp, err := http.Get(mf.URLs[0])
			if err != nil {
				t.Fatal(err)
			}
			streamedFLAC, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("stream status=%d", resp.StatusCode)
			}
			if coverFetched.Load() < 1 {
				t.Fatalf("internal cover fetcher did not fetch authenticated %s cover (count=%d)", proto, coverFetched.Load())
			}
			if !bytes.Contains(streamedFLAC, []byte("image/jpeg")) {
				t.Fatalf("streamed FLAC missing embedded JPEG picture block")
			}

			// 4. Wait for the queued import job to finish and inspect GET /api/jobs JSON.
			deadline := time.Now().Add(10 * time.Second)
			var jobsBody string
			for {
				wJobs := httptest.NewRecorder()
				s.listJobs(wJobs, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
				if wJobs.Code != http.StatusOK {
					t.Fatalf("listJobs status=%d body=%s", wJobs.Code, wJobs.Body.String())
				}
				jobsBody = wJobs.Body.String()
				assertNoLeak(t, "GET /api/jobs response", jobsBody)
				var payload struct {
					Jobs []Job `json:"jobs"`
				}
				if err := json.Unmarshal(wJobs.Body.Bytes(), &payload); err != nil {
					t.Fatalf("decode jobs: %v", err)
				}
				if len(payload.Jobs) >= 1 && (payload.Jobs[0].State == "done" || payload.Jobs[0].State == "error") {
					if payload.Jobs[0].State != "done" {
						t.Fatalf("import job ended in state=%q err=%q", payload.Jobs[0].State, payload.Jobs[0].Error)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("timed out waiting for import job; last /api/jobs=%s", jobsBody)
				}
				time.Sleep(25 * time.Millisecond)
			}
			if !strings.Contains(jobsBody, `"Cover Track"`) {
				t.Fatalf("expected NAS job in /api/jobs response: %s", jobsBody)
			}

			// 5. Direct Submit with a credentialed cover URL strips credentials from both returned and stored Job.
			var authCover string
			if proto == "subsonic" {
				authCover = nasSrv.URL + "/rest/getCoverArt.view?id=art-42&u=" + dummyUser + "&p=" + dummyPass
			} else {
				authCover = nasSrv.URL + "/library/metadata/tr-1/thumb/42?X-Plex-Token=" + dummyToken
			}
			subJob, err := jobs.Submit(nasScope, providers.Track{
				ID:       "direct-submit-1",
				Title:    "Submitted Track",
				Provider: proto,
				CoverURL: authCover,
			}, false)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			subJSON, err := json.Marshal(subJob)
			if err != nil {
				t.Fatal(err)
			}
			assertNoLeak(t, "Jobs.Submit returned Job JSON", string(subJSON))
			jobs.mu.Lock()
			internalCover := jobs.items[subJob.ID].Track.CoverURL
			jobs.mu.Unlock()
			if internalCover != expectedCover {
				t.Fatalf("internal job CoverURL = %q, want credential-free %q", internalCover, expectedCover)
			}
		})
	}

	// Non-NAS public cover URLs must remain intact on job copies.
	pubJobs := NewJobs(Pipeline{Catalog: noCatalog{}})
	defer pubJobs.Close()
	pubJob, err := pubJobs.Submit("sess-pub", providers.Track{
		ID:       "dz-1",
		Title:    "Public Track",
		Provider: "deezer",
		CoverURL: "https://cdn-images.dzcdn.net/images/cover/abc/500x500.jpg",
	}, false)
	if err != nil {
		t.Fatalf("Submit public track: %v", err)
	}
	if pubJob.Track.CoverURL != "https://cdn-images.dzcdn.net/images/cover/abc/500x500.jpg" {
		t.Fatalf("public non-NAS CoverURL was altered: %q", pubJob.Track.CoverURL)
	}
}

func TestNASStemsStateAndRestartCoverRecovery(t *testing.T) {
	const (
		dummyUser  = "dummy-nas-stems-user"
		dummyPass  = "dummy-nas-stems-pass-999"
		dummyToken = "dummy-nas-stems-token-999"
	)
	var flacBuf bytes.Buffer
	if err := testaudio.FLAC(&flacBuf, testaudio.Song(8)); err != nil {
		t.Fatal(err)
	}
	flacBytes := flacBuf.Bytes()

	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: 20, G: 180, B: 90, A: 255})
		}
	}
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	jpegBytes := jpegBuf.Bytes()

	forbidden := []string{
		dummyUser, dummyPass, dummyToken,
		"X-Plex-Token", "t=", "s=", "p=", "u=",
	}
	assertNoLeak := func(t *testing.T, label, text string) {
		t.Helper()
		for _, bad := range forbidden {
			if strings.Contains(text, bad) {
				t.Errorf("%s leaked %q: %s", label, bad, text)
			}
		}
	}

	cases := []struct {
		name  string
		proto string
		tls   bool
	}{
		{"subsonic-http", "subsonic", false},
		{"subsonic-https", "subsonic", true},
		{"plex-http", "plex", false},
		{"plex-https", "plex", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var coverFetched atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch tc.proto {
				case "subsonic":
					switch r.URL.Path {
					case "/rest/getPlaylists.view":
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlists":{"playlist":[{"id":"pl-1","name":"NAS Crate","songCount":1}]}}}`)
					case "/rest/getPlaylist.view":
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlist":{"id":"pl-1","name":"NAS Crate","entry":[{"id":"s-1","title":"Cover Track","artist":"DJ Test","album":"NAS Album","duration":8,"coverArt":"art-42"}]}}}`)
					case "/rest/download.view":
						w.Header().Set("Content-Type", "audio/flac")
						w.Header().Set("Content-Disposition", `attachment; filename="cover.flac"`)
						_, _ = w.Write(flacBytes)
					case "/rest/getCoverArt.view":
						q := r.URL.Query()
						if q.Get("id") != "art-42" || q.Get("u") != dummyUser || (q.Get("p") == "" && (q.Get("t") == "" || q.Get("s") == "")) {
							http.Error(w, "unauthorized cover", http.StatusUnauthorized)
							return
						}
						coverFetched.Add(1)
						w.Header().Set("Content-Type", "image/jpeg")
						_, _ = w.Write(jpegBytes)
					default:
						http.NotFound(w, r)
					}
				case "plex":
					switch r.URL.Path {
					case "/playlists":
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"pl-1","title":"NAS Crate","leafCount":1}]}}`)
					case "/playlists/pl-1/items":
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"tr-1","title":"Cover Track","grandparentTitle":"DJ Test","parentTitle":"NAS Album","duration":8000,"thumb":"/library/metadata/tr-1/thumb/42","Media":[{"Part":[{"key":"/library/parts/1/song.flac"}]}]}]}}`)
					case "/library/parts/1/song.flac":
						w.Header().Set("Content-Type", "audio/flac")
						_, _ = w.Write(flacBytes)
					case "/library/metadata/tr-1/thumb/42":
						tok := r.Header.Get("X-Plex-Token")
						if tok == "" {
							tok = r.URL.Query().Get("X-Plex-Token")
						}
						if tok != dummyToken {
							http.Error(w, "unauthorized cover", http.StatusUnauthorized)
							return
						}
						coverFetched.Add(1)
						w.Header().Set("Content-Type", "image/jpeg")
						_, _ = w.Write(jpegBytes)
					default:
						http.NotFound(w, r)
					}
				}
			})
			var nasSrv *httptest.Server
			if tc.tls {
				nasSrv = httptest.NewTLSServer(handler)
			} else {
				nasSrv = httptest.NewServer(handler)
			}
			defer nasSrv.Close()

			expectedCover := nasSrv.URL + "/rest/getCoverArt.view?id=art-42"
			if tc.proto == "plex" {
				expectedCover = nasSrv.URL + "/library/metadata/tr-1/thumb/42"
			}

			uiDir, stateDir := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=nas\nomni_source=0\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			// First server instance: browse, trigger stems persistence, and shut down.
			dl1 := t.TempDir()
			jobs1 := NewJobs(Pipeline{
				Guard:   resourceguard.New(resourceguard.Config{Disabled: true}),
				Catalog: noCatalog{},
				Deemix:  &deemixclient.Client{DownloadDir: dl1, Media: nasSrv.Client()},
			})
			s1 := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir, HTTP: nasSrv.Client()}, jobs1)
			gw1 := httptest.NewServer(s1)

			var cfgJSON string
			if tc.proto == "subsonic" {
				cfgJSON = fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":%q,"password":%q,"enabled":true}`, nasSrv.URL, dummyUser, dummyPass)
			} else {
				cfgJSON = fmt.Sprintf(`{"protocol":"plex","url":%q,"token":%q,"enabled":true}`, nasSrv.URL, dummyToken)
			}
			wCfg := httptest.NewRecorder()
			s1.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(cfgJSON)))
			if wCfg.Code != http.StatusOK {
				t.Fatalf("save NAS config (%d): %s", wCfg.Code, wCfg.Body.String())
			}

			var plResp struct {
				Items []map[string]any `json:"items"`
			}
			tidalGet(t, gw1.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &plResp)
			plUUID, _ := plResp.Items[0]["uuid"].(string)

			var trResp struct {
				Items []map[string]any `json:"items"`
			}
			tidalGet(t, gw1.URL, "/v1/playlists/"+plUUID+"/tracks?limit=50&offset=0", 200, &trResp)
			trackID := int64(trResp.Items[0]["id"].(float64))

			// 1. Stems endpoint writes streaming-stems.json without any credentials.
			reqStems := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/enginex/stems/source?id=%d", trackID), nil)
			reqStems.RemoteAddr = "127.0.0.1:1234"
			wStems := httptest.NewRecorder()
			s1.tidal.ServeHTTP(wStems, reqStems)
			if wStems.Code != http.StatusOK {
				t.Fatalf("stemsSource status=%d body=%s", wStems.Code, wStems.Body.String())
			}
			stemsBytes, err := os.ReadFile(filepath.Join(stateDir, "streaming-stems.json"))
			if err != nil {
				t.Fatalf("read streaming-stems.json: %v", err)
			}
			assertNoLeak(t, "streaming-stems.json", string(stemsBytes))
			if !strings.Contains(string(stemsBytes), expectedCover) {
				t.Errorf("streaming-stems.json missing credential-free cover %q: %s", expectedCover, string(stemsBytes))
			}

			gw1.Close()
			s1.Close()
			jobs1.Close()

			savedBytes, err := os.ReadFile(filepath.Join(stateDir, "gateway-tracks.json"))
			if err != nil {
				t.Fatalf("read gateway-tracks.json: %v", err)
			}
			assertNoLeak(t, "gateway-tracks.json", string(savedBytes))
			if !strings.Contains(string(savedBytes), expectedCover) {
				t.Errorf("gateway-tracks.json missing credential-free cover %q: %s", expectedCover, string(savedBytes))
			}

			// 2. Cold restart: brand-new Server, Jobs, and empty download cache on the same uiDir/stateDir.
			// Do not browse playlists or tracks again before requesting cover art and streaming.
			dl2 := t.TempDir()
			jobs2 := NewJobs(Pipeline{
				Guard:   resourceguard.New(resourceguard.Config{Disabled: true}),
				Catalog: noCatalog{},
				Deemix:  &deemixclient.Client{DownloadDir: dl2, Media: nasSrv.Client()},
			})
			defer jobs2.Close()
			s2 := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir, HTTP: nasSrv.Client()}, jobs2)
			defer s2.Close()
			gw2 := httptest.NewServer(s2)
			defer gw2.Close()

			s2.tidal.mu.Lock()
			restored, ok := s2.tidal.tracks[trackID]
			s2.tidal.mu.Unlock()
			if !ok {
				t.Fatalf("track %d was not restored from gateway-tracks.json", trackID)
			}
			if restored.track.CoverURL != expectedCover {
				t.Errorf("restored track CoverURL = %q, want %q", restored.track.CoverURL, expectedCover)
			}

			// 2a. Staging album art on the restored track succeeds without re-browsing.
			coverFetched.Store(0)
			dataDir := t.TempDir()
			stmPath := filepath.Join(dataDir, "AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/stm.db")
			if err := os.MkdirAll(filepath.Dir(stmPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stmPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			stageDir := t.TempDir()
			oldShadow, oldRbx := shadowDataRoot, rbxAnalysisDir
			shadowDataRoot, rbxAnalysisDir = dataDir, stageDir
			s2.tidal.stageAlbumArt(context.Background(), trackID, restored.track)
			shadowDataRoot, rbxAnalysisDir = oldShadow, oldRbx

			stagedFile := filepath.Join(stageDir, fmt.Sprintf("%d.cover", trackID))
			stagedBytes, err := os.ReadFile(stagedFile)
			if err != nil {
				t.Errorf("restored track failed to stage album art: %v", err)
			} else if !bytes.HasPrefix(stagedBytes, []byte(rbxCoverMagic)) || !bytes.HasSuffix(stagedBytes, jpegBytes) {
				t.Errorf("staged cover malformed (len=%d)", len(stagedBytes))
			}
			if coverFetched.Load() < 1 {
				t.Errorf("stageAlbumArt did not fetch authenticated cover")
			}

			// 2b. /v1/enginex/art serves the restored track's thumbnail after clearing in-memory cache.
			s2.tidal.artMu.Lock()
			s2.tidal.artCache = map[string][]byte{}
			s2.tidal.artAge = nil
			s2.tidal.artMu.Unlock()
			coverFetched.Store(0)
			artResp, err := http.Get(gw2.URL + "/v1/enginex/art?song=Cover+Track&artist=DJ+Test")
			if err != nil {
				t.Fatal(err)
			}
			artBody, _ := io.ReadAll(artResp.Body)
			artResp.Body.Close()
			if artResp.StatusCode != http.StatusOK || !bytes.Equal(artBody, jpegBytes) {
				t.Errorf("restored /v1/enginex/art status=%d len=%d", artResp.StatusCode, len(artBody))
			}
			if coverFetched.Load() < 1 {
				t.Errorf("/v1/enginex/art did not fetch authenticated cover")
			}

			// 2c. Cold stream after restart downloads audio and embeds the authenticated cover into the FLAC.
			coverFetched.Store(0)
			var pb struct {
				Manifest string `json:"manifest"`
			}
			tidalGet(t, gw2.URL, fmt.Sprintf("/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", trackID), 200, &pb)
			rawManifest, err := base64.StdEncoding.DecodeString(pb.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			var mf struct {
				URLs []string `json:"urls"`
			}
			if err := json.Unmarshal(rawManifest, &mf); err != nil || len(mf.URLs) != 1 {
				t.Fatalf("manifest=%s err=%v", rawManifest, err)
			}
			resp, err := http.Get(mf.URLs[0])
			if err != nil {
				t.Fatal(err)
			}
			streamedFLAC, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("cold restart stream status=%d", resp.StatusCode)
			}
			if coverFetched.Load() < 1 {
				t.Errorf("cold restart stream did not fetch authenticated cover")
			}
			if !bytes.Contains(streamedFLAC, []byte("image/jpeg")) {
				t.Errorf("cold restart streamed FLAC missing embedded JPEG picture block")
			}
		})
	}
}

func TestNASLegacyStateScrubbedOnStartup(t *testing.T) {
	const (
		dummyUser  = "legacy-nas-user"
		dummyPass  = "legacy-nas-pass-321"
		dummyToken = "legacy-plex-tok-321"
	)
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	jpegBytes := jpegBuf.Bytes()

	for _, proto := range []string{"subsonic", "plex"} {
		t.Run(proto, func(t *testing.T) {
			nasSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch proto {
				case "subsonic":
					q := r.URL.Query()
					if r.URL.Path == "/rest/getCoverArt.view" && q.Get("id") == "art-99" && q.Get("u") == dummyUser && (q.Get("p") != "" || (q.Get("t") != "" && q.Get("s") != "")) {
						w.Header().Set("Content-Type", "image/jpeg")
						_, _ = w.Write(jpegBytes)
						return
					}
				case "plex":
					if r.URL.Path == "/library/metadata/tr-99/thumb/99" && r.Header.Get("X-Plex-Token") == dummyToken {
						w.Header().Set("Content-Type", "image/jpeg")
						_, _ = w.Write(jpegBytes)
						return
					}
				}
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}))
			defer nasSrv.Close()

			uiDir, stateDir := t.TempDir(), t.TempDir()
			var legacyCover, cleanCover, nasCfg string
			if proto == "subsonic" {
				legacyCover = nasSrv.URL + "/rest/getCoverArt.view?c=EngineX&f=json&id=art-99&p=" + dummyPass + "&u=" + dummyUser + "&v=1.16.1"
				cleanCover = nasSrv.URL + "/rest/getCoverArt.view?id=art-99"
				nasCfg = fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":%q,"password":%q,"enabled":true}`, nasSrv.URL, dummyUser, dummyPass)
			} else {
				legacyCover = nasSrv.URL + "/library/metadata/tr-99/thumb/99?X-Plex-Token=" + dummyToken
				cleanCover = nasSrv.URL + "/library/metadata/tr-99/thumb/99"
				nasCfg = fmt.Sprintf(`{"protocol":"plex","url":%q,"token":%q,"enabled":true}`, nasSrv.URL, dummyToken)
			}
			if err := os.WriteFile(filepath.Join(uiDir, "nas.json"), []byte(nasCfg), 0o600); err != nil {
				t.Fatal(err)
			}

			legacyStems := fmt.Sprintf(`{"nas":[{"track":{"id":"s-99","title":"Legacy Track","artist":"DJ Old","provider":%q,"coverUrl":%q},"session":"nas","path":"/tmp/legacy.flac"}]}`, proto, legacyCover)
			if err := os.WriteFile(filepath.Join(stateDir, "streaming-stems.json"), []byte(legacyStems), 0o600); err != nil {
				t.Fatal(err)
			}
			legacyTracks := fmt.Sprintf(`{"next":7,"tracks":[{"id":7,"track":{"id":"s-99","title":"Legacy Track","artist":"DJ Old","provider":%q,"coverUrl":%q},"session":"nas"}]}`, proto, legacyCover)
			if err := os.WriteFile(filepath.Join(stateDir, "gateway-tracks.json"), []byte(legacyTracks), 0o600); err != nil {
				t.Fatal(err)
			}

			s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir, HTTP: nasSrv.Client()}, nil)
			defer s.Close()
			gw := httptest.NewServer(s)
			defer gw.Close()

			for _, name := range []string{"streaming-stems.json", "gateway-tracks.json"} {
				b, err := os.ReadFile(filepath.Join(stateDir, name))
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				text := string(b)
				for _, bad := range []string{dummyUser, dummyPass, dummyToken, "X-Plex-Token", "t=", "s=", "p=", "u="} {
					if strings.Contains(text, bad) {
						t.Errorf("%s still contains %q after startup scrub: %s", name, bad, text)
					}
				}
				if !strings.Contains(text, cleanCover) {
					t.Errorf("%s did not preserve credential-free cover %q: %s", name, cleanCover, text)
				}
			}

			artResp, err := http.Get(gw.URL + "/v1/enginex/art?song=Legacy+Track&artist=DJ+Old")
			if err != nil {
				t.Fatal(err)
			}
			artBody, _ := io.ReadAll(artResp.Body)
			artResp.Body.Close()
			if artResp.StatusCode != http.StatusOK || !bytes.Equal(artBody, jpegBytes) {
				t.Fatalf("scrubbed track /v1/enginex/art status=%d len=%d", artResp.StatusCode, len(artBody))
			}
		})
	}
}

func TestArtworkProxyNASRouteProvenanceAndImageOnly(t *testing.T) {
	const dummyToken = "plex-art-owner-token-999"

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 50, G: 180, B: 90, A: 255})
		}
	}
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	jpegBytes := jpegBuf.Bytes()

	var prefsHits, htmlHits, authedCoverHits atomic.Int32
	nasSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != dummyToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/:/prefs":
			prefsHits.Add(1)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer secret="owner-prefs"/>`))
		case "/library/metadata/1/thumb/html":
			htmlHits.Add(1)
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>not an image</body></html>`))
		case "/library/metadata/1/thumb/42":
			authedCoverHits.Add(1)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer nasSrv.Close()

	uiDir, stateDir := t.TempDir(), t.TempDir()
	nasCfg := fmt.Sprintf(`{"protocol":"plex","url":%q,"token":%q,"enabled":true}`, nasSrv.URL, dummyToken)
	if err := os.WriteFile(filepath.Join(uiDir, "nas.json"), []byte(nasCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	dlDir := t.TempDir()
	dc := &deemixclient.Client{DownloadDir: dlDir, Media: nasSrv.Client()}
	jobs := NewJobs(Pipeline{Deemix: dc, Catalog: &providers.Deezer{HTTP: nasSrv.Client()}})
	defer jobs.Close()

	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir, HTTP: nasSrv.Client()}, jobs)
	defer s.Close()
	gw := httptest.NewServer(s)
	defer gw.Close()

	// 1. Non-cover Plex path on a NAS track must never be fetched or served.
	t.Run("non-cover-plex-route", func(t *testing.T) {
		s.tidal.addTrack(trackRef{track: providers.Track{
			ID:       "/library/parts/1/a.flac",
			Title:    "PrefsTrack",
			Artist:   "NASArtist",
			Provider: "plex",
			CoverURL: nasSrv.URL + "/:/prefs",
		}})
		resp, err := http.Get(gw.URL + "/v1/enginex/art?song=PrefsTrack&artist=NASArtist")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("/:/prefs via /v1/enginex/art status=%d, want 404", resp.StatusCode)
		}
		if prefsHits.Load() != 0 {
			t.Fatalf("/:/prefs received %d authenticated hits, want 0", prefsHits.Load())
		}
	})

	// 2. Non-image body on a valid Plex cover path must be rejected and not cached.
	t.Run("non-image-body", func(t *testing.T) {
		s.tidal.addTrack(trackRef{track: providers.Track{
			ID:       "/library/parts/2/b.flac",
			Title:    "HTMLTrack",
			Artist:   "NASArtist",
			Provider: "plex",
			CoverURL: nasSrv.URL + "/library/metadata/1/thumb/html",
		}})
		resp, err := http.Get(gw.URL + "/v1/enginex/art?song=HTMLTrack&artist=NASArtist")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("HTML cover via /v1/enginex/art status=%d, want 404", resp.StatusCode)
		}
	})

	// 3. Another source (Deezer) naming the Plex cover URL must never attach the owner NAS token.
	t.Run("non-nas-provenance", func(t *testing.T) {
		spoof := providers.Track{
			ID:       "dz-spoof-1",
			Title:    "SpoofTrack",
			Artist:   "OtherArtist",
			Provider: "deezer",
			CoverURL: nasSrv.URL + "/library/metadata/1/thumb/42",
		}
		s.tidal.addTrack(trackRef{track: spoof})
		resp, err := http.Get(gw.URL + "/v1/enginex/art?song=SpoofTrack&artist=OtherArtist")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("Deezer spoof cover via /v1/enginex/art status=%d, want 404", resp.StatusCode)
		}
		s.tidal.prewarmArt([]trackRef{{track: spoof}})
		for end := time.Now().Add(3 * time.Second); s.tidal.warming.Load() && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		}
		var flacBuf bytes.Buffer
		if err := testaudio.FLAC(&flacBuf, testaudio.Song(2)); err != nil {
			t.Fatal(err)
		}
		flacPath := filepath.Join(dlDir, "spoof.flac")
		if err := os.WriteFile(flacPath, flacBuf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		jobs.addCover(context.Background(), spoof, &deemixclient.Download{Path: flacPath})
		if authedCoverHits.Load() != 0 {
			t.Fatalf("non-NAS source triggered %d authenticated Plex cover requests, want 0", authedCoverHits.Load())
		}
	})

	// 4. Genuine NAS track with valid Plex cover path and JPEG body succeeds.
	t.Run("valid-nas-cover", func(t *testing.T) {
		s.tidal.addTrack(trackRef{track: providers.Track{
			ID:       "/library/parts/3/c.flac",
			Title:    "GoodTrack",
			Artist:   "NASArtist",
			Provider: "plex",
			CoverURL: nasSrv.URL + "/library/metadata/1/thumb/42",
		}})
		resp, err := http.Get(gw.URL + "/v1/enginex/art?song=GoodTrack&artist=NASArtist")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, jpegBytes) {
			t.Fatalf("valid Plex cover status=%d len=%d, want 200 JPEG", resp.StatusCode, len(body))
		}
		if authedCoverHits.Load() != 1 {
			t.Fatalf("valid Plex cover hits=%d, want 1", authedCoverHits.Load())
		}
	})
}

func TestNASLegacyDirtyIDsScrubbedPreservingGatewayIDs(t *testing.T) {
	const (
		dirtyPlexUser = "legacy-plex-u"
		dirtyPlexPass = "legacy-plex-p"
		dirtyPlexTok  = "legacy-plex-tok"
		dirtyDavTok   = "legacy-dav-tok"
	)
	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	var flacBuf bytes.Buffer
	if err := testaudio.FLAC(&flacBuf, testaudio.Song(2)); err != nil {
		t.Fatal(err)
	}
	cachedFile := filepath.Join(dlDir, "nas-cached.flac")
	if err := os.WriteFile(cachedFile, flacBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dirtyPlexID := "http://" + dirtyPlexUser + ":" + dirtyPlexPass + "@nas.local:32400/library/parts/301/song.flac?X-Plex-Token=" + dirtyPlexTok
	dirtyDavID := "http://nas.local:8080/music/Mixes/track.flac?sig=" + dirtyDavTok
	dirtyDavList := "http://nas.local:8080/music/Mixes/?token=" + dirtyDavTok

	legacyTracks := fmt.Sprintf(`{
		"next": 42,
		"tracks": [
			{"id": 42, "track": {"id": %q, "title": "Plex Track", "artist": "A", "preview": %q, "provider": "plex"}, "session": "nas"},
			{"id": 43, "track": {"id": %q, "title": "DAV Track", "artist": "B", "album": %q, "provider": "webdav"}, "session": "nas"}
		],
		"lists": [
			{"id": "list-uuid-1", "session": "nas", "provider": "nas", "list": {"id": %q, "title": "Mixes", "count": 1}}
		]
	}`, dirtyPlexID, dirtyPlexID, dirtyDavID, dirtyDavList, dirtyDavList)
	if err := os.WriteFile(filepath.Join(stateDir, "gateway-tracks.json"), []byte(legacyTracks), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyStems := fmt.Sprintf(`{"nas":[{"track":{"id":%q,"title":"Plex Track","preview":%q,"provider":"plex"},"session":"nas","path":"/tmp/a.flac"}]}`,
		dirtyPlexID, dirtyPlexID)
	if err := os.WriteFile(filepath.Join(stateDir, "streaming-stems.json"), []byte(legacyStems), 0o600); err != nil {
		t.Fatal(err)
	}
	legacySources := fmt.Sprintf(`{%q:"nas-cached.flac",%q:"nas-cached.flac"}`, "plex:"+dirtyPlexID, "webdav:"+dirtyDavID)
	if err := os.WriteFile(filepath.Join(dlDir, sourcesFile), []byte(legacySources), 0o644); err != nil {
		t.Fatal(err)
	}

	dc := &deemixclient.Client{DownloadDir: dlDir}
	jobs := NewJobs(Pipeline{Deemix: dc, Catalog: &providers.Deezer{}})
	defer jobs.Close()

	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	defer s.Close()

	forbidden := []string{dirtyPlexUser, dirtyPlexPass, dirtyPlexTok, dirtyDavTok, "X-Plex-Token", "sig=", "token=", "?", "://"}
	for _, p := range []string{
		filepath.Join(stateDir, "gateway-tracks.json"),
		filepath.Join(stateDir, "streaming-stems.json"),
		filepath.Join(dlDir, sourcesFile),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		text := string(b)
		for _, bad := range forbidden {
			if strings.Contains(text, bad) {
				t.Errorf("%s still contains %q after migration: %s", filepath.Base(p), bad, text)
			}
		}
	}

	// Gateway numeric ID 42 is preserved and maps to the normalized Plex track.
	s.tidal.mu.Lock()
	ref42, ok42 := s.tidal.tracks[42]
	mappedID := s.tidal.ids[refKey(ref42)]
	listRef, okList := s.tidal.lists["list-uuid-1"]
	s.tidal.mu.Unlock()
	if !ok42 || ref42.track.ID != "/library/parts/301/song.flac" || mappedID != 42 {
		t.Fatalf("gateway numeric ID 42 not preserved cleanly: ok=%v track=%+v mapped=%d", ok42, ref42.track, mappedID)
	}
	wantDavList := providers.SanitizeSelfHostedID("webdav", dirtyDavList)
	if !okList || listRef.list.ID != wantDavList || listRef.list.ID != "/music/Mixes/" {
		t.Fatalf("gateway playlist ID not normalized: ok=%v list=%+v want=%q", okList, listRef.list, wantDavList)
	}

	// Migrated .sources.json resolves cached downloads under the normalized IDs.
	if got := jobs.File(providers.Track{ID: "/library/parts/301/song.flac", Provider: "plex"}); got != cachedFile {
		t.Fatalf("jobs.File after .sources.json migration = %q, want %q", got, cachedFile)
	}
	if got := jobs.File(providers.Track{ID: providers.SanitizeSelfHostedID("webdav", dirtyDavID), Provider: "webdav"}); got != cachedFile {
		t.Fatalf("jobs.File for webdav after .sources.json migration = %q, want %q", got, cachedFile)
	}

	// Job submission and cover-error logs scrub URL-shaped IDs too.
	job, err := jobs.submitTo("nas", providers.Track{ID: dirtyPlexID, Preview: dirtyPlexID, Provider: "plex", Title: "Job Track"}, "", providers.Track{}, nil)
	if err != nil {
		t.Fatalf("submitTo: %v", err)
	}
	jobBytes, _ := json.Marshal(job)
	for _, bad := range forbidden {
		if strings.Contains(string(jobBytes), bad) {
			t.Errorf("job response leaked %q: %s", bad, string(jobBytes))
		}
	}
	var logBuf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	jobs.addCover(context.Background(), providers.Track{
		ID:       dirtyPlexID,
		Provider: "plex",
		CoverURL: "https://127.0.0.1:1/unreachable.jpg",
	}, &deemixclient.Download{Path: cachedFile})
	log.SetOutput(origOut)
	for _, bad := range forbidden {
		if strings.Contains(logBuf.String(), bad) {
			t.Errorf("cover error log leaked %q: %s", bad, logBuf.String())
		}
	}
}

func TestNASConfigRejectsQueryAndMigratesLegacyURL(t *testing.T) {
	s, dir := newTestServerWithUIDir(t)
	defer s.Close()

	// 1. POST /v1/enginex/nas rejects query parameters and fragments without echoing secrets.
	for _, tc := range []struct {
		name, body, secret string
	}{
		{"plex-query", `{"protocol":"plex","url":"http://192.168.1.10:32400/?X-Plex-Token=secret-q-tok","enabled":true}`, "secret-q-tok"},
		{"subsonic-query", `{"protocol":"subsonic","url":"http://192.168.1.10:4533/?u=alice&p=secret-q-pass","enabled":true}`, "secret-q-pass"},
		{"fragment", `{"protocol":"webdav","url":"http://192.168.1.10:8080/dav/#secret-frag","enabled":true}`, "secret-frag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(tc.body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("POST /v1/enginex/nas status=%d, want 400 (body=%s)", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), tc.secret) {
				t.Fatalf("error echoed secret %q: %s", tc.secret, w.Body.String())
			}
			wt := httptest.NewRecorder()
			s.handleNASTest(wt, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas/test", strings.NewReader(tc.body)))
			if wt.Code != http.StatusBadRequest {
				t.Fatalf("POST /v1/enginex/nas/test status=%d, want 400", wt.Code)
			}
		})
	}

	// 2. Legacy nas.json carrying a query token is scrubbed on load/GET and migrated on disk.
	const legacyTok = "legacy-url-query-tok-777"
	legacyCfg := fmt.Sprintf(`{"protocol":"plex","url":"http://192.168.1.10:32400/base?X-Plex-Token=%s#frag","enabled":true}`, legacyTok)
	if err := os.WriteFile(filepath.Join(dir, "nas.json"), []byte(legacyCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleNASConfig(w, httptest.NewRequest(http.MethodGet, "/v1/enginex/nas", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/enginex/nas status=%d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), legacyTok) || strings.Contains(w.Body.String(), "?") || strings.Contains(w.Body.String(), "#") {
		t.Fatalf("GET /v1/enginex/nas leaked query/fragment: %s", w.Body.String())
	}
	var view NASConfigView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.URL != "http://192.168.1.10:32400/base" || !view.HasToken || view.Token != "" {
		t.Fatalf("unexpected migrated view: %+v", view)
	}
	onDiskBytes, err := os.ReadFile(filepath.Join(dir, "nas.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk providers.NASConfig
	if err := json.Unmarshal(onDiskBytes, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.URL != "http://192.168.1.10:32400/base" || onDisk.Token != legacyTok {
		t.Fatalf("on-disk nas.json not migrated properly: %+v", onDisk)
	}
}

func TestStartupStemsScrubPreservesUndecodableFile(t *testing.T) {
	for _, tc := range []struct {
		name, content string
	}{
		{
			name:    "partial-decode-wrong-field-type",
			content: `{"nas":[{"track":{"id":"s-1","title":"Keep","provider":"plex","coverUrl":"http://127.0.0.1:32400/library/metadata/1/thumb/1?X-Plex-Token=secret-keep-tok"},"session":"nas","path":"/tmp/a.flac"}],"deezer":"wrong-type-entry"}`,
		},
		{
			name:    "malformed-json",
			content: `{"nas": [broken secret-keep-tok`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uiDir, stateDir := t.TempDir(), t.TempDir()
			stemsPath := filepath.Join(stateDir, "streaming-stems.json")
			if err := os.WriteFile(stemsPath, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}

			var logBuf bytes.Buffer
			origOut := log.Writer()
			log.SetOutput(&logBuf)
			s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, nil)
			log.SetOutput(origOut)
			defer s.Close()

			after, err := os.ReadFile(stemsPath)
			if err != nil {
				t.Fatalf("read stems file after startup: %v", err)
			}
			if string(after) != tc.content {
				t.Fatalf("startup scrub overwrote undecodable stems file:\ngot:  %s\nwant: %s", string(after), tc.content)
			}
			logs := logBuf.String()
			if !strings.Contains(logs, "skip stems state scrub") {
				t.Fatalf("expected diagnostic log when skipping stems scrub, got: %q", logs)
			}
			for _, bad := range []string{"secret-keep-tok", "wrong-type-entry", "X-Plex-Token"} {
				if strings.Contains(logs, bad) {
					t.Errorf("stems scrub diagnostic log leaked file content %q: %s", bad, logs)
				}
			}
		})
	}
}

func TestNASRound6TwoStartsLosslessAndIdempotent(t *testing.T) {
	writeJSON := func(t *testing.T, path string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var sampleFLAC bytes.Buffer
	if err := testaudio.FLAC(&sampleFLAC, testaudio.Song(2)); err != nil {
		t.Fatal(err)
	}
	writeAudio := func(t *testing.T, dir string, names ...string) {
		t.Helper()
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(dir, name), sampleFLAC.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("two-starts-lossless-all-four-files", func(t *testing.T) {
		ui, state, downloads := t.TempDir(), t.TempDir(), t.TempDir()
		paths := []string{
			filepath.Join(state, "gateway-tracks.json"),
			filepath.Join(state, "streaming-stems.json"),
			filepath.Join(downloads, sourcesFile),
			filepath.Join(ui, "nas.json"),
		}
		base := url.URL{Scheme: "https", Host: "nas.example.invalid"}
		legacy := base
		legacy.User = url.User("owner")
		const explicitPass = "explicit-saved-pass-999"
		writeJSON(t, paths[3], providers.NASConfig{URL: legacy.String(), Protocol: "webdav", Password: explicitPass, Enabled: true})

		a, b := "/music/item?edition=original", "/music/item?edition=remix"
		group := "/music/mixes?edition=first"
		first := providers.Track{Provider: "webdav", ID: a, Title: "Original", Rating: 4}
		second := providers.Track{Provider: "webdav", ID: b, Title: "Remix", Rating: 5}
		writeJSON(t, paths[0], savedTracks{
			Next: 90,
			Tracks: []savedRef{
				{ID: 41, Track: first, Session: "nas"},
				{ID: 42, Track: second, Session: "nas"},
			},
			Lists: []savedList{
				{ID: "omni", Session: "nas", Provider: localProvider, List: providers.Playlist{ID: "nas\x00" + group, Title: "Mixes"}},
			},
			Albums: []savedAlbum{
				{ID: 71, Provider: "nas", ListID: group},
			},
		})
		writeJSON(t, paths[1], map[string][]stemmedTrack{"nas": {{Track: first, Path: "original.flac"}, {Track: second, Path: "remix.flac"}}})
		writeJSON(t, paths[2], map[string]string{"webdav:" + a: "original.flac", "webdav:" + b: "remix.flac", "deezer:8": "keep.flac"})
		writeAudio(t, downloads, "original.flac", "remix.flac", "keep.flac")

		prior := make([][]byte, len(paths))
		for boot := 1; boot <= 2; boot++ {
			jobs := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: downloads}, Catalog: &providers.Deezer{}})
			s := New(Config{UIDir: ui, StateDir: state}, jobs)
			s.tidal.mu.Lock()
			rev41 := s.tidal.ids[refKey(s.tidal.tracks[41])]
			rev42 := s.tidal.ids[refKey(s.tidal.tracks[42])]
			s.tidal.mu.Unlock()
			gotFile1 := jobs.File(first)
			gotFile2 := jobs.File(second)
			s.Close()
			jobs.Close()

			if rev41 != 41 || rev42 != 42 {
				t.Errorf("boot %d: reverse gateway IDs lost: rev41=%d rev42=%d", boot, rev41, rev42)
			}
			if gotFile1 != filepath.Join(downloads, "original.flac") || gotFile2 != filepath.Join(downloads, "remix.flac") {
				t.Errorf("boot %d: jobs.File lost distinct mappings: %q vs %q", boot, gotFile1, gotFile2)
			}
			var g savedTracks
			var st map[string][]stemmedTrack
			var src map[string]string
			var cfg providers.NASConfig
			into := []any{&g, &st, &src, &cfg}
			for i, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, into[i]); err != nil {
					t.Fatal(err)
				}
				if boot == 2 && !bytes.Equal(prior[i], data) {
					t.Errorf("boot %d: %s changed on second start", boot, filepath.Base(path))
				}
				prior[i] = data
			}
			wantGroup := providers.SanitizeSelfHostedID("webdav", group)
			if len(g.Tracks) != 2 || g.Next != 90 || g.Tracks[0].ID != 41 || g.Tracks[1].ID != 42 ||
				g.Tracks[0].Track.Rating != 4 || g.Tracks[1].Track.Rating != 5 ||
				len(g.Albums) != 1 || g.Albums[0].ID != 71 || g.Albums[0].ListID != wantGroup {
				t.Errorf("boot %d: gateway rows, ratings, album, or counter changed: %+v", boot, g)
			}
			if g.Tracks[0].Track.ID == g.Tracks[1].Track.ID ||
				strings.Contains(g.Tracks[0].Track.ID, "?") || strings.Contains(g.Tracks[1].Track.ID, "?") {
				t.Errorf("boot %d: gateway track IDs collided or retained query: %q vs %q", boot, g.Tracks[0].Track.ID, g.Tracks[1].Track.ID)
			}
			if len(g.Lists) != 1 || g.Lists[0].ID != "omni" || g.Lists[0].List.ID != "nas\x00"+wantGroup || strings.Contains(g.Lists[0].List.ID, "?") {
				t.Errorf("boot %d: OmniSource playlist not cleanly normalized: %+v", boot, g.Lists)
			}
			rows := st["nas"]
			if len(rows) != 2 || rows[0].Path != "original.flac" || rows[1].Path != "remix.flac" || sameTrack(rows[0].Track, rows[1].Track) {
				t.Errorf("boot %d: Stems rows lost order/paths or collided: %+v", boot, rows)
			}
			if len(src) != 3 || src["deezer:8"] != "keep.flac" ||
				src["webdav:"+g.Tracks[0].Track.ID] != "original.flac" ||
				src["webdav:"+g.Tracks[1].Track.ID] != "remix.flac" {
				t.Errorf("boot %d: download mappings lost: %+v", boot, src)
			}
			if cfg.Password != explicitPass || cfg.Username != "owner" || cfg.URL != base.String() || !cfg.Enabled {
				t.Errorf("boot %d: nas config migration lost fields: %+v", boot, cfg)
			}
		}
	})

	t.Run("url-password-migrated-with-existing-username", func(t *testing.T) {
		ui := t.TempDir()
		writeJSON(t, filepath.Join(ui, "nas.json"), providers.NASConfig{
			URL: "https://owner:url-secret-pass@nas.example.invalid", Protocol: "webdav", Username: "owner", Enabled: true,
		})
		s := New(Config{UIDir: ui, StateDir: t.TempDir()}, nil)
		defer s.Close()
		cfg := s.loadNASConfig()
		if cfg.Username != "owner" || cfg.Password != "url-secret-pass" || cfg.URL != "https://nas.example.invalid" {
			t.Fatalf("password migration with populated username failed: %+v", cfg)
		}
	})

	t.Run("nas-migration-persistence-error-logged-cleanly", func(t *testing.T) {
		// Make uiDir/nas.json fit under Linux PATH_MAX (4096) while SaveJSON's
		// longer ".credential-*" temp file fails with ENAMETOOLONG even as root.
		ui := t.TempDir()
		for len(ui) < 4080 {
			segLen := min(200, 4080-len(ui)-1)
			if segLen <= 0 {
				break
			}
			ui = filepath.Join(ui, strings.Repeat("d", segLen))
		}
		if err := os.MkdirAll(ui, 0o700); err != nil {
			t.Fatal(err)
		}
		const secretPass = "persist-err-pass-777"
		writeJSON(t, filepath.Join(ui, "nas.json"), providers.NASConfig{
			URL: "https://owner:" + secretPass + "@nas.example.invalid/dav?tok=secret-q", Protocol: "webdav", Enabled: true,
		})

		var logBuf bytes.Buffer
		origOut := log.Writer()
		log.SetOutput(&logBuf)
		s := New(Config{UIDir: ui, StateDir: t.TempDir()}, nil)
		log.SetOutput(origOut)
		defer s.Close()

		logs := logBuf.String()
		if !strings.Contains(logs, "migrate nas config failed") {
			t.Fatalf("expected persistence failure log, got: %q", logs)
		}
		for _, bad := range []string{secretPass, "secret-q", "owner", "nas.example.invalid"} {
			if strings.Contains(logs, bad) {
				t.Errorf("persistence error log leaked %q: %s", bad, logs)
			}
		}
		w := httptest.NewRecorder()
		s.handleNASConfig(w, httptest.NewRequest(http.MethodGet, "/v1/enginex/nas", nil))
		if strings.Contains(w.Body.String(), secretPass) || strings.Contains(w.Body.String(), "secret-q") || strings.Contains(w.Body.String(), "?") {
			t.Fatalf("GET /v1/enginex/nas leaked credentials after save failure: %s", w.Body.String())
		}
	})

	t.Run("collision-preflight-leaves-files-untouched", func(t *testing.T) {
		ui, state, downloads := t.TempDir(), t.TempDir(), t.TempDir()
		gwPath := filepath.Join(state, "gateway-tracks.json")
		stemsPath := filepath.Join(state, "streaming-stems.json")
		srcPath := filepath.Join(downloads, sourcesFile)

		writeIndented := func(path string, v any) {
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		dirtyPlex := "http://col-u:col-p@nas.local:32400/library/parts/1/a.flac?X-Plex-Token=col-tok"
		cleanPlex := "/library/parts/1/a.flac"
		t1 := providers.Track{Provider: "plex", ID: dirtyPlex, Title: "First"}
		t2 := providers.Track{Provider: "plex", ID: cleanPlex, Title: "Second"}
		writeIndented(gwPath, savedTracks{Next: 10, Tracks: []savedRef{{ID: 1, Track: t1, Session: "nas"}, {ID: 2, Track: t2, Session: "nas"}}})
		writeIndented(stemsPath, map[string][]stemmedTrack{"nas": {{Track: t1, Path: "one.flac"}, {Track: t2, Path: "two.flac"}}})
		writeIndented(srcPath, map[string]string{"plex:" + dirtyPlex: "one.flac", "plex:" + cleanPlex: "two.flac"})
		writeAudio(t, downloads, "one.flac", "two.flac", "fresh.flac")
		origGW, _ := os.ReadFile(gwPath)
		origStems, _ := os.ReadFile(stemsPath)
		origSrc, _ := os.ReadFile(srcPath)

		var logBuf bytes.Buffer
		origOut := log.Writer()
		log.SetOutput(&logBuf)
		jobs := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: downloads}, Catalog: &providers.Deezer{}})
		s := New(Config{UIDir: ui, StateDir: state}, jobs)
		log.SetOutput(origOut)

		s.tidal.mu.Lock()
		rev1 := s.tidal.ids[refKey(s.tidal.tracks[1])]
		rev2 := s.tidal.ids[refKey(s.tidal.tracks[2])]
		s.tidal.mu.Unlock()
		if rev1 != 1 || rev2 != 2 {
			t.Errorf("colliding gateway rows lost reverse mappings: rev1=%d rev2=%d", rev1, rev2)
		}
		if got1, got2, got1Again := jobs.File(t1), jobs.File(t2), jobs.File(t1); got1 != filepath.Join(downloads, "one.flac") || got2 != filepath.Join(downloads, "two.flac") || got1Again != filepath.Join(downloads, "one.flac") {
			t.Errorf("colliding download mappings lost: %q vs %q vs %q", got1, got2, got1Again)
		}

		// Playback Fetch after warming both entries must return each track's own file (reviewer playback probe).
		if play1, err := jobs.Fetch(context.Background(), "", t1, false); err != nil || play1 != filepath.Join(downloads, "one.flac") {
			t.Errorf("playback Fetch(t1) = %q (err=%v), want %q", play1, err, filepath.Join(downloads, "one.flac"))
		}
		if play2, err := jobs.Fetch(context.Background(), "", t2, false); err != nil || play2 != filepath.Join(downloads, "two.flac") {
			t.Errorf("playback Fetch(t2) = %q (err=%v), want %q", play2, err, filepath.Join(downloads, "two.flac"))
		}

		// Cold Fetch in reverse order (t2 before t1) with an Importer must return each file and keep distinct import queue keys.
		libPath, _, _ := testlibrary.Create(t)
		lib, err := analyzer.OpenLibrary(libPath)
		if err != nil {
			t.Fatal(err)
		}
		defer lib.Close()
		jobsImp := NewJobs(Pipeline{
			Guard:   resourceguard.New(resourceguard.Config{Disabled: true}),
			Deemix:  &deemixclient.Client{DownloadDir: downloads},
			Catalog: &providers.Deezer{},
			Library: lib,
		})
		if play2, err := jobsImp.Fetch(context.Background(), "", t2, false); err != nil || play2 != filepath.Join(downloads, "two.flac") {
			t.Errorf("cold Fetch(t2) = %q (err=%v), want two.flac", play2, err)
		}
		if play1, err := jobsImp.Fetch(context.Background(), "", t1, false); err != nil || play1 != filepath.Join(downloads, "one.flac") {
			t.Errorf("cold Fetch(t1) = %q (err=%v), want one.flac", play1, err)
		}
		jobsImp.mu.Lock()
		queuedCount := len(jobsImp.queued)
		jobsImp.mu.Unlock()
		jobsImp.Close()
		if queuedCount != 2 {
			t.Errorf("colliding tracks collapsed in import queue: queued=%d want 2", queuedCount)
		}

		// Normal subsequent writes must never copy unresolved legacy credentials forward on disk.
		freshTrack := providers.Track{Provider: "deezer", ID: "9001", Title: "Fresh"}
		s.tidal.addTrack(trackRef{track: freshTrack, session: "nas"})
		s.tidal.saveTracks()
		jobs.putFile(trackKey(freshTrack), fetched{matched: freshTrack, dl: &deemixclient.Download{Path: filepath.Join(downloads, "fresh.flac")}})
		if err := s.addStemmed("nas", stemmedTrack{Track: freshTrack, Path: "fresh.flac"}); err == nil {
			t.Errorf("addStemmed succeeded on colliding stems state; want refusal")
		}

		for _, tc := range []struct {
			name string
			path string
			want []byte
		}{
			{"gateway-tracks.json", gwPath, origGW},
			{"streaming-stems.json", stemsPath, origStems},
			{".sources.json", srcPath, origSrc},
		} {
			got, _ := os.ReadFile(tc.path)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("colliding %s was modified on startup or subsequent write", tc.name)
			}
		}
		logs := logBuf.String()
		for _, wantLog := range []string{"skip gateway track migration", "skip stems state scrub", "skip download source index migration"} {
			if !strings.Contains(logs, wantLog) {
				t.Errorf("expected diagnostic %q in logs: %s", wantLog, logs)
			}
		}
		for _, bad := range []string{"col-u", "col-p", "col-tok"} {
			if strings.Contains(logs, bad) {
				t.Errorf("collision log leaked %q: %s", bad, logs)
			}
		}
		// Returned JSON, stems list, and playlist error logs still clean unmigrated values.
		out := s.tidal.trackJSON(1, providers.Track{Provider: "webdav", ID: "http://col-u:col-p@nas/a.flac?sig=col-tok", Album: "http://col-u:col-p@nas/dir/?sig=col-tok"})
		outJSON, _ := json.Marshal(out)
		stemsJSON, _ := json.Marshal(s.stemmed("nas"))
		srcLog := playlistOpenSourceID(&playlistRef{provider: localProvider, list: providers.Playlist{ID: "nas\x00http://col-u:col-p@nas/dir/?sig=col-tok"}})
		for _, bad := range []string{"col-u", "col-p", "col-tok", "?"} {
			if strings.Contains(string(outJSON), bad) || strings.Contains(string(stemsJSON), bad) || strings.Contains(srcLog, bad) {
				t.Errorf("sanitized output/log leaked %q: json=%s stems=%s log=%s", bad, string(outJSON), string(stemsJSON), srcLog)
			}
		}
		// A second start also preserves all three colliding files byte-for-byte.
		s.Close()
		jobs.Close()
		jobs2 := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: downloads}, Catalog: &providers.Deezer{}})
		s2 := New(Config{UIDir: ui, StateDir: state}, jobs2)
		s2.Close()
		jobs2.Close()
		for _, tc := range []struct {
			name string
			path string
			want []byte
		}{
			{"gateway-tracks.json", gwPath, origGW},
			{"streaming-stems.json", stemsPath, origStems},
			{".sources.json", srcPath, origSrc},
		} {
			got, _ := os.ReadFile(tc.path)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("colliding %s was modified after second start", tc.name)
			}
		}
		if got := playlistOpenSourceID(&playlistRef{provider: localProvider, list: providers.Playlist{ID: "nas\x00http://col-u:col-p@nas.local"}}); got != `nas:""` {
			t.Errorf("empty inner OmniSource locator = %q, want %q", got, `nas:""`)
		}
		for _, smbDir := range []string{"90s #1 Hits", "Who? Mix"} {
			if got := providers.SanitizeSelfHostedID(nasScope, smbDir); got != smbDir {
				t.Errorf("SMB folder %q mangled to %q", smbDir, got)
			}
		}
	})

	t.Run("clean-indented-gateway-untouched-on-close", func(t *testing.T) {
		ui, state, downloads := t.TempDir(), t.TempDir(), t.TempDir()
		gwPath := filepath.Join(state, "gateway-tracks.json")
		indented, err := json.MarshalIndent(savedTracks{
			Next:   5,
			Tracks: []savedRef{{ID: 5, Track: providers.Track{Provider: "plex", ID: "/library/parts/5/clean.flac", Title: "Clean"}, Session: "nas"}},
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gwPath, indented, 0o600); err != nil {
			t.Fatal(err)
		}
		writeAudio(t, downloads, "clean.flac")
		jobs := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: downloads}, Catalog: &providers.Deezer{}})
		s := New(Config{UIDir: ui, StateDir: state}, jobs)
		// Normal clean saveSource writes .sources.json with mode 0600, never 0644.
		cleanTrack := providers.Track{Provider: "plex", ID: "/library/parts/5/clean.flac", Title: "Clean"}
		jobs.putFile(trackKey(cleanTrack), fetched{matched: cleanTrack, dl: &deemixclient.Download{Path: filepath.Join(downloads, "clean.flac")}})
		if st, err := os.Stat(filepath.Join(downloads, sourcesFile)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf(".sources.json mode = %v (err=%v), want 0600", st.Mode().Perm(), err)
		}
		s.Close()
		jobs.Close()
		got, err := os.ReadFile(gwPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, indented) {
			t.Fatalf("clean indented gateway-tracks.json was rewritten on Close")
		}
	})

	t.Run("authority-only-id-blocked-all-three-files", func(t *testing.T) {
		ui, state, downloads := t.TempDir(), t.TempDir(), t.TempDir()
		gwPath := filepath.Join(state, "gateway-tracks.json")
		stemsPath := filepath.Join(state, "streaming-stems.json")
		srcPath := filepath.Join(downloads, sourcesFile)

		writeIndented := func(path string, v any) {
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		// Single entry per file (no duplicate sibling): only the nonempty-to-empty ID check can block each migration.
		authOnly := "http://empty-u:empty-p@nas.local"
		only := providers.Track{Provider: "plex", ID: authOnly, Title: "AuthorityOnly"}
		writeIndented(gwPath, savedTracks{Next: 3, Tracks: []savedRef{{ID: 3, Track: only, Session: "nas"}}})
		writeIndented(stemsPath, map[string][]stemmedTrack{"nas": {{Track: only, Path: "only.flac"}}})
		writeIndented(srcPath, map[string]string{"plex:" + authOnly: "only.flac"})
		writeAudio(t, downloads, "only.flac", "other.flac")
		origGW, _ := os.ReadFile(gwPath)
		origStems, _ := os.ReadFile(stemsPath)
		origSrc, _ := os.ReadFile(srcPath)

		var logBuf bytes.Buffer
		origOut := log.Writer()
		log.SetOutput(&logBuf)
		jobs := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: downloads}, Catalog: &providers.Deezer{}})
		s := New(Config{UIDir: ui, StateDir: state}, jobs)
		log.SetOutput(origOut)

		if got := jobs.File(only); got != filepath.Join(downloads, "only.flac") {
			t.Errorf("jobs.File(authority-only) = %q, want only.flac", got)
		}
		if got, err := jobs.Fetch(context.Background(), "", only, false); err != nil || got != filepath.Join(downloads, "only.flac") {
			t.Errorf("jobs.Fetch(authority-only) = %q (err=%v), want only.flac", got, err)
		}
		other := providers.Track{Provider: "deezer", ID: "77", Title: "Other"}
		s.tidal.addTrack(trackRef{track: other, session: "nas"})
		s.tidal.saveTracks()
		jobs.putFile(trackKey(other), fetched{matched: other, dl: &deemixclient.Download{Path: filepath.Join(downloads, "other.flac")}})
		if err := s.addStemmed("nas", stemmedTrack{Track: other, Path: "other.flac"}); err == nil {
			t.Errorf("addStemmed succeeded on authority-only stems state; want refusal")
		}
		s.Close()
		jobs.Close()

		for _, tc := range []struct {
			name string
			path string
			want []byte
		}{
			{"gateway-tracks.json", gwPath, origGW},
			{"streaming-stems.json", stemsPath, origStems},
			{".sources.json", srcPath, origSrc},
		} {
			got, _ := os.ReadFile(tc.path)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("authority-only %s was modified", tc.name)
			}
		}
		logs := logBuf.String()
		for _, wantLog := range []string{"skip gateway track migration", "skip stems state scrub", "skip download source index migration"} {
			if !strings.Contains(logs, wantLog) {
				t.Errorf("expected diagnostic %q for authority-only ID: %s", wantLog, logs)
			}
		}
		for _, bad := range []string{"empty-u", "empty-p"} {
			if strings.Contains(logs, bad) {
				t.Errorf("authority-only log leaked %q: %s", bad, logs)
			}
		}
	})
}
