package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testsmb"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
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
