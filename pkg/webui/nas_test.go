package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	// An HTTP NAS keeps its protocol as the track's provider, as released (Stems list, row
	// label); only SMB is the standalone NAS source (#335).
	if resp, err := http.Get(fmt.Sprintf("%s/v1/enginex/stems/local?id=%d", gw.URL, trackID)); err != nil {
		t.Fatal(err)
	} else if resp.Body.Close(); resp.Header.Get(providerHeader) != "subsonic" {
		t.Fatalf("[NAS] track %s = %q, want subsonic", providerHeader, resp.Header.Get(providerHeader))
	}

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

	// 2. HTTP NAS protocols (subsonic, webdav, plex) keep the released [NAS] route and get no standalone entry.
	for _, cfgBody := range []string{
		`{"protocol":"subsonic","url":"http://nas.example:4533","username":"dj","password":"pw","enabled":true}`,
		`{"protocol":"webdav","url":"http://nas.example:8080/music","username":"dj","password":"pw","enabled":true}`,
		`{"protocol":"plex","url":"http://nas.example:32400","token":"tok","enabled":true}`,
	} {
		wCfg := httptest.NewRecorder()
		s.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(cfgBody)))
		if wCfg.Code != http.StatusOK {
			t.Fatalf("save HTTP NAS config failed (%d): %s", wCfg.Code, wCfg.Body.String())
		}
		status = uiSettings{}
		tidalGet(t, gw.URL, "/v1/enginex/ui", 200, &status)
		if hasIn(status.Sources, "nas") || hasIn(status.LoggedIn, "nas") {
			t.Fatalf("HTTP NAS should not appear as standalone source: cfg=%s sources=%v loggedIn=%v", cfgBody, status.Sources, status.LoggedIn)
		}
		if code, _ := uiPost(t, gw.URL, "scope=nas"); code != http.StatusConflict {
			t.Fatalf("HTTP NAS scope=nas: got HTTP %d, want 409", code)
		}
	}

	// 3. Save enabled SMB NAS config: NAS appears dynamically in sources & loggedIn without OmniSource.
	cfgBody := `{"protocol":"smb","url":"smb://nas.example/music","username":"dj","password":"pw","enabled":true}`
	wCfg := httptest.NewRecorder()
	s.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(cfgBody)))
	if wCfg.Code != http.StatusOK {
		t.Fatalf("save SMB NAS config failed (%d): %s", wCfg.Code, wCfg.Body.String())
	}

	status = uiSettings{}
	tidalGet(t, gw.URL, "/v1/enginex/ui", 200, &status)
	if !hasIn(status.Sources, "nas") || !hasIn(status.LoggedIn, "nas") {
		t.Fatalf("configured NAS missing from UI status: sources=%v loggedIn=%v", status.Sources, status.LoggedIn)
	}

	// 4. Selecting scope=nas succeeds without any streaming login or OmniSource and does not persist nas into sources=.
	if code, updated := uiPost(t, gw.URL, "scope=nas"); code != http.StatusOK || updated.Scope != "nas" {
		t.Fatalf("scope=nas failed: code=%d scope=%q", code, updated.Scope)
	}
	if raw, err := os.ReadFile(filepath.Join(uiDir, "settings.conf")); err != nil || strings.Contains(string(raw), "sources=nas") {
		t.Fatalf("dynamic nas source must not be persisted into settings.conf sources=: %q %v", string(raw), err)
	}

	// 5. Deleting NAS removes it from UI status and rejects scope=nas with 409.
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
	if err := os.WriteFile(filepath.Join(smbSrv.Root, "Root Opener.mp3"), mp3Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
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

	// Verify favorites/playlists, X-Enginex-Provider header, and standalone search on SMB NAS.
	var favLists struct {
		Total int `json:"totalNumberOfItems"`
	}
	tidalGet(t, gw.URL, "/v1/users/1/favorites/playlists?limit=50&offset=0", 200, &favLists)
	if favLists.Total != 59 {
		t.Fatalf("SMB favorites playlists total=%d, want 59", favLists.Total)
	}
	reqStem, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v1/enginex/stems/local?id=%d", gw.URL, trackID), nil)
	respStem, err := http.DefaultClient.Do(reqStem)
	if err != nil {
		t.Fatal(err)
	}
	respStem.Body.Close()
	if got := respStem.Header.Get(providerHeader); got != "nas" {
		t.Fatalf("%s = %q, want %q", providerHeader, got, "nas")
	}
	var searchRes struct {
		Tracks struct {
			Items []map[string]any `json:"items"`
		} `json:"tracks"`
	}
	tidalGet(t, gw.URL, "/v1/search?query=Root+Opener&limit=50&offset=0", 200, &searchRes)
	if len(searchRes.Tracks.Items) != 1 || searchRes.Tracks.Items[0]["title"] != "Root Opener" {
		t.Fatalf("SMB search returned %+v", searchRes.Tracks.Items)
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
			// WebDAV, Subsonic and Plex reach the gateway through OmniSource's [NAS] lists (#335).
			plUUID := s.tidal.addPlaylist(playlistRef{
				session:  nasScope,
				provider: localProvider,
				list:     providers.Playlist{ID: "nas\x00pl-1", Title: "[NAS] Crate"},
			})
			var logBuf bytes.Buffer
			log.SetOutput(&logBuf)
			defer log.SetOutput(os.Stderr)

			reqTracks := httptest.NewRequest(http.MethodGet, "/v1/playlists/"+plUUID+"/tracks?limit=50&offset=0", nil)
			reqTracks.RemoteAddr = "127.0.0.1:1234"
			wTracks := httptest.NewRecorder()
			s.tidal.ServeHTTP(wTracks, reqTracks)
			log.SetOutput(os.Stderr)

			if wTracks.Code != http.StatusNotFound {
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

	if w.Code != http.StatusNotFound { // an HTTP NAS has no standalone source (#335)
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

type smbTestImporter struct{}

func (smbTestImporter) Import(context.Context, analyzer.Import) (int64, error) { return 42, nil }

func TestNASSMBCredentialsNeverLeakInStateErrorsJobsOrLogs(t *testing.T) {
	smbSrv := testsmb.Start(t)
	setsDir := filepath.Join(smbSrv.Root, "Sets")
	if err := os.MkdirAll(setsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(setsDir, "Opener.mp3"), testMP3(120), 0o600); err != nil {
		t.Fatal(err)
	}

	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=nas\nomni_source=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{
		Guard:   resourceguard.New(resourceguard.Config{Disabled: true}),
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: dlDir, AllowLossy: false},
		Library: smbTestImporter{},
	})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	gw := httptest.NewServer(s)
	defer gw.Close()

	const badURLPass = "smb-url-secret-999"
	const wrongPass = "wrong-smb-secret-999"
	credURL := strings.Replace(smbSrv.URL, "smb://", "smb://"+smbSrv.User+":"+badURLPass+"@", 1)
	assertClean := func(label, text string, secrets ...string) {
		t.Helper()
		for _, sec := range secrets {
			if sec != "" && strings.Contains(text, sec) {
				t.Errorf("%s leaked %q: %s", label, sec, text)
			}
		}
	}

	// 1. Both /api/nas and /api/nas/test reject smb://user:pass@ URLs without echoing credentials.
	badBody := fmt.Sprintf(`{"protocol":"smb","url":%q,"enabled":true}`, credURL)
	for _, ep := range []string{"/api/nas", "/api/nas/test"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, ep, strings.NewReader(badBody))
		if ep == "/api/nas" {
			s.handleNASConfig(w, req)
		} else {
			s.handleNASTest(w, req)
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s accepted credentialed SMB URL (%d): %s", ep, w.Code, w.Body.String())
		}
		assertClean(ep+" 400", w.Body.String(), badURLPass, smbSrv.User+":"+badURLPass+"@")
	}

	// 2. Save valid SMB config; verify 0600 nas.json URL hygiene and masked GET/POST responses.
	validBody := fmt.Sprintf(`{"protocol":"smb","url":%q,"username":%q,"password":%q,"enabled":true}`, smbSrv.URL, smbSrv.User, smbSrv.Password)
	wSave := httptest.NewRecorder()
	s.handleNASConfig(wSave, httptest.NewRequest(http.MethodPost, "/api/nas", strings.NewReader(validBody)))
	if wSave.Code != http.StatusOK || !strings.Contains(wSave.Body.String(), `"hasPassword":true`) {
		t.Fatalf("POST /api/nas (%d): %s", wSave.Code, wSave.Body.String())
	}
	assertClean("POST /api/nas", wSave.Body.String(), smbSrv.Password, smbSrv.User+":"+smbSrv.Password+"@")

	wGet := httptest.NewRecorder()
	s.handleNASConfig(wGet, httptest.NewRequest(http.MethodGet, "/api/nas", nil))
	assertClean("GET /api/nas", wGet.Body.String(), smbSrv.Password, smbSrv.User+":"+smbSrv.Password+"@")
	if info, err := os.Stat(s.nasConfigPath()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("nas.json stat err=%v mode=%v, want 0600", err, info.Mode().Perm())
	}
	var savedCfg providers.NASConfig
	if raw, err := os.ReadFile(s.nasConfigPath()); err != nil || json.Unmarshal(raw, &savedCfg) != nil || savedCfg.URL != smbSrv.URL {
		t.Fatalf("nas.json url=%q err=%v, want %q", savedCfg.URL, err, smbSrv.URL)
	}

	// 3. Working SMB opens (standalone NAS and OmniSource) emit zero playlist-open failure logs.
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	var nasLists struct {
		Items []map[string]any `json:"items"`
	}
	tidalGet(t, gw.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &nasLists)
	var nasSetsUUID string
	for _, it := range nasLists.Items {
		if it["title"] == "Sets" {
			nasSetsUUID, _ = it["uuid"].(string)
		}
	}
	if nasSetsUUID == "" {
		t.Fatalf("missing Sets playlist in standalone NAS: %+v", nasLists.Items)
	}
	var nasTracksRes struct {
		Items []map[string]any `json:"items"`
	}
	tidalGet(t, gw.URL, "/v1/playlists/"+nasSetsUUID+"/tracks?limit=50&offset=0", 200, &nasTracksRes)
	if len(nasTracksRes.Items) != 1 {
		t.Fatalf("standalone NAS Sets tracks: %+v", nasTracksRes.Items)
	}
	trackID := int64(nasTracksRes.Items[0]["id"].(float64))

	setSettings := func(scope string, omni int) {
		t.Helper()
		body := fmt.Sprintf("sources=soundcloud\nscope=%s\nomni_source=%d\n", scope, omni)
		if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	setSettings("soundcloud", 1)
	var omniLists struct {
		Items []map[string]any `json:"items"`
	}
	tidalGet(t, gw.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &omniLists)
	var omniSetsUUID string
	for _, it := range omniLists.Items {
		if it["title"] == "[NAS] Sets" {
			omniSetsUUID, _ = it["uuid"].(string)
		}
	}
	if omniSetsUUID == "" {
		t.Fatalf("missing [NAS] Sets in OmniSource: %+v", omniLists.Items)
	}
	tidalGet(t, gw.URL, "/v1/playlists/"+omniSetsUUID+"/tracks?limit=50&offset=0", 200, &nasTracksRes)
	log.SetOutput(os.Stderr)
	if strings.Contains(logBuf.String(), "stream-sync: playlist open failed") {
		t.Fatalf("working SMB opens logged failure diagnostic: %q", logBuf.String())
	}

	// 4. Play SMB track, record Stems, flush state, and verify state files + /api/jobs stay clean.
	setSettings(nasScope, 0)
	var pb struct {
		Manifest string `json:"manifest"`
	}
	tidalGet(t, gw.URL, fmt.Sprintf("/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", trackID), 200, &pb)
	rawManifest, err := base64.StdEncoding.DecodeString(pb.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	assertClean("playback manifest", string(rawManifest), smbSrv.Password, smbSrv.User+":"+smbSrv.Password+"@")
	var mf struct {
		URLs []string `json:"urls"`
	}
	if err := json.Unmarshal(rawManifest, &mf); err != nil || len(mf.URLs) != 1 {
		t.Fatalf("SMB manifest=%s err=%v", rawManifest, err)
	}
	resp, err := http.Get(mf.URLs[0])
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d", resp.StatusCode)
	}
	if err := s.addStemmed(nasScope, stemmedTrack{
		Track:   providers.Track{ID: "Sets/Opener.mp3", Title: "Opener", Provider: "smb"},
		Session: nasScope,
		Path:    filepath.Join(dlDir, "opener-stems.wav"),
	}); err != nil {
		t.Fatalf("addStemmed failed: %v", err)
	}
	s.tidal.saveTracks()

	wJobs := httptest.NewRecorder()
	s.listJobs(wJobs, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	assertClean("GET /api/jobs (working)", wJobs.Body.String(), smbSrv.Password, smbSrv.User+":"+smbSrv.Password+"@")

	for name, path := range map[string]string{
		"gateway-tracks.json":  filepath.Join(stateDir, "gateway-tracks.json"),
		".sources.json":        filepath.Join(dlDir, sourcesFile),
		"streaming-stems.json": filepath.Join(stateDir, "streaming-stems.json"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("expected state file %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("state file %s mode=%o, want 0600", name, info.Mode().Perm())
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read state file %s: %v", name, err)
		}
		assertClean(name, string(raw), smbSrv.Password, smbSrv.User+":"+smbSrv.Password+"@")
	}

	// 5. Wrong SMB password: test, hint, standalone open, OmniSource open, job error, and logs never leak it.
	wrongBody := fmt.Sprintf(`{"protocol":"smb","url":%q,"username":%q,"password":%q,"enabled":true}`, smbSrv.URL, smbSrv.User, wrongPass)
	wSaveWrong := httptest.NewRecorder()
	s.handleNASConfig(wSaveWrong, httptest.NewRequest(http.MethodPost, "/api/nas", strings.NewReader(wrongBody)))
	if wSaveWrong.Code != http.StatusOK {
		t.Fatalf("save wrong-pass SMB config (%d): %s", wSaveWrong.Code, wSaveWrong.Body.String())
	}

	wTestWrong := httptest.NewRecorder()
	s.handleNASTest(wTestWrong, httptest.NewRequest(http.MethodPost, "/api/nas/test", nil))
	if wTestWrong.Code != http.StatusBadGateway {
		t.Fatalf("wrong-pass /api/nas/test status=%d, want 502", wTestWrong.Code)
	}
	assertClean("POST /api/nas/test wrong pass", wTestWrong.Body.String(), wrongPass, smbSrv.User+":"+wrongPass+"@")

	wHint := httptest.NewRecorder()
	hintReq := httptest.NewRequest(http.MethodGet, "/v1/users/1/playlists?limit=50&offset=0", nil)
	hintReq.RemoteAddr = "127.0.0.1:1234"
	s.tidal.ServeHTTP(wHint, hintReq)
	assertClean("NAS hint wrong pass", wHint.Body.String(), wrongPass, smbSrv.User+":"+wrongPass+"@")

	logBuf.Reset()
	log.SetOutput(&logBuf)
	wNasFail := httptest.NewRecorder()
	nasFailReq := httptest.NewRequest(http.MethodGet, "/v1/playlists/"+nasSetsUUID+"/tracks?limit=50&offset=0", nil)
	nasFailReq.RemoteAddr = "127.0.0.1:1234"
	s.tidal.ServeHTTP(wNasFail, nasFailReq)
	log.SetOutput(os.Stderr)
	if wNasFail.Code != http.StatusBadGateway {
		t.Fatalf("wrong-pass standalone NAS tracks status=%d, want 502", wNasFail.Code)
	}
	assertClean("standalone NAS tracks error", wNasFail.Body.String(), wrongPass, smbSrv.User+":"+wrongPass+"@")
	nasLog := logBuf.String()
	assertClean("standalone NAS failure log", nasLog, wrongPass, smbSrv.User+":"+wrongPass+"@")
	if strings.Count(nasLog, "stream-sync: playlist open failed") != 1 {
		t.Errorf("standalone NAS failure log count=%d, want 1: %q", strings.Count(nasLog, "stream-sync: playlist open failed"), nasLog)
	}
	for _, want := range []string{"stream-sync: playlist open failed", "route=/v1/playlists/", "status=502", "scope=nas", "provider=nas", `source_id=nas:"Sets"`, "error="} {
		if !strings.Contains(nasLog, want) {
			t.Errorf("standalone NAS failure log missing %q: %q", want, nasLog)
		}
	}

	setSettings("soundcloud", 1)
	logBuf.Reset()
	log.SetOutput(&logBuf)
	wOmniFail := httptest.NewRecorder()
	omniFailReq := httptest.NewRequest(http.MethodGet, "/v1/playlists/"+omniSetsUUID+"/tracks?limit=50&offset=0", nil)
	omniFailReq.RemoteAddr = "127.0.0.1:1234"
	s.tidal.ServeHTTP(wOmniFail, omniFailReq)
	log.SetOutput(os.Stderr)
	if wOmniFail.Code != http.StatusNotFound {
		t.Fatalf("wrong-pass OmniSource SMB tracks status=%d, want 404", wOmniFail.Code)
	}
	assertClean("OmniSource SMB tracks error", wOmniFail.Body.String(), wrongPass, smbSrv.User+":"+wrongPass+"@")
	omniLog := logBuf.String()
	assertClean("OmniSource SMB failure log", omniLog, wrongPass, smbSrv.User+":"+wrongPass+"@")
	if strings.Count(omniLog, "stream-sync: playlist open failed") != 1 {
		t.Errorf("OmniSource SMB failure log count=%d, want 1: %q", strings.Count(omniLog, "stream-sync: playlist open failed"), omniLog)
	}
	for _, want := range []string{"stream-sync: playlist open failed", "route=/v1/playlists/", "status=404", "scope=soundcloud", "provider=local", `source_id=nas:"./Sets"`, "error="} {
		if !strings.Contains(omniLog, want) {
			t.Errorf("OmniSource SMB failure log missing %q: %q", want, omniLog)
		}
	}

	failJob, err := jobs.Submit("nas", providers.Track{ID: "Sets/Missing.mp3", Title: "Missing", Provider: "smb"}, false)
	if err != nil {
		t.Fatalf("jobs.Submit failed: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var failedJobSeen bool
	for time.Now().Before(deadline) {
		for _, jb := range jobs.List() {
			if jb.ID == failJob.ID && jb.State == "error" {
				failedJobSeen = true
				break
			}
		}
		if failedJobSeen {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !failedJobSeen {
		t.Fatalf("SMB failure job did not reach error state: %+v", jobs.List())
	}
	wJobsFail := httptest.NewRecorder()
	s.listJobs(wJobsFail, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	assertClean("GET /api/jobs (error)", wJobsFail.Body.String(), wrongPass, smbSrv.User+":"+wrongPass+"@")

	// 6. Legacy nas.json carrying smb://user:pass@ still works without echoing credentials in GET /api/nas,
	// username-only legacy URLs preserve a saved password, and unparseable credentialed URLs are never echoed.
	legacyURL := strings.Replace(smbSrv.URL, "smb://", "smb://"+smbSrv.User+":"+smbSrv.Password+"@", 1)
	if err := providers.SaveJSON(s.nasConfigPath(), providers.NASConfig{Protocol: "smb", URL: legacyURL, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wLegacy := httptest.NewRecorder()
	s.handleNASConfig(wLegacy, httptest.NewRequest(http.MethodGet, "/api/nas", nil))
	if wLegacy.Code != http.StatusOK || !strings.Contains(wLegacy.Body.String(), `"hasPassword":true`) {
		t.Fatalf("legacy SMB GET /api/nas (%d): %s", wLegacy.Code, wLegacy.Body.String())
	}
	assertClean("legacy SMB GET /api/nas", wLegacy.Body.String(), smbSrv.Password, smbSrv.User+":"+smbSrv.Password+"@")
	if lists, prov := s.nasPlaylists(context.Background()); prov == nil || len(lists) != 2 {
		t.Fatalf("legacy SMB nasPlaylists got prov=%v lists=%+v", prov != nil, lists)
	}

	userOnlyURL := strings.Replace(smbSrv.URL, "smb://", "smb://"+smbSrv.User+"@", 1)
	if err := providers.SaveJSON(s.nasConfigPath(), providers.NASConfig{Protocol: "smb", URL: userOnlyURL, Password: smbSrv.Password, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wUserOnly := httptest.NewRecorder()
	s.handleNASConfig(wUserOnly, httptest.NewRequest(http.MethodGet, "/api/nas", nil))
	if wUserOnly.Code != http.StatusOK || !strings.Contains(wUserOnly.Body.String(), `"hasPassword":true`) {
		t.Fatalf("user-only legacy SMB GET /api/nas cleared password (%d): %s", wUserOnly.Code, wUserOnly.Body.String())
	}
	if lists, prov := s.nasPlaylists(context.Background()); prov == nil || len(lists) != 2 {
		t.Fatalf("user-only legacy SMB nasPlaylists failed (saved password cleared?): prov=%v lists=%+v", prov != nil, lists)
	}

	malformedCredURL := "smb://" + smbSrv.User + ":" + badURLPass + "@127.0.0.1:badport/music"
	if err := providers.SaveJSON(s.nasConfigPath(), providers.NASConfig{Protocol: "smb", URL: malformedCredURL, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wMalformed := httptest.NewRecorder()
	s.handleNASConfig(wMalformed, httptest.NewRequest(http.MethodGet, "/api/nas", nil))
	if wMalformed.Code != http.StatusOK {
		t.Fatalf("malformed legacy SMB GET /api/nas (%d): %s", wMalformed.Code, wMalformed.Body.String())
	}
	assertClean("malformed legacy SMB GET /api/nas", wMalformed.Body.String(), badURLPass, smbSrv.User+":"+badURLPass+"@")

	// 7. Hostile SMB folder names with control characters stay on one log line in both standalone NAS and OmniSource.
	if err := providers.SaveJSON(s.nasConfigPath(), providers.NASConfig{Protocol: "smb", URL: smbSrv.URL, Username: smbSrv.User, Password: smbSrv.Password, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	hostileFolder := "./Crate\nstream-sync: forged=1\r\x1b[31m"
	for _, tc := range []struct {
		name  string
		scope string
		omni  int
		ref   playlistRef
	}{
		{name: "standalone-smb", scope: nasScope, omni: 0, ref: playlistRef{session: nasScope, provider: nasScope, list: providers.Playlist{ID: hostileFolder, Title: "Crate"}}},
		{name: "omnisource-smb", scope: "soundcloud", omni: 1, ref: playlistRef{session: "nas", provider: localProvider, list: providers.Playlist{ID: "nas\x00" + hostileFolder, Title: "[NAS] Crate"}}},
	} {
		setSettings(tc.scope, tc.omni)
		plUUID := s.tidal.addPlaylist(tc.ref)
		logBuf.Reset()
		log.SetOutput(&logBuf)
		req := httptest.NewRequest(http.MethodGet, "/v1/playlists/"+plUUID+"/tracks?limit=50&offset=0", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.tidal.ServeHTTP(w, req)
		log.SetOutput(os.Stderr)
		line := logBuf.String()
		if !strings.Contains(line, "stream-sync: playlist open failed") {
			t.Fatalf("%s missing diagnostic log: %q", tc.name, line)
		}
		if strings.Count(strings.TrimSpace(line), "\n") != 0 || strings.ContainsAny(line, "\r\x1b") {
			t.Fatalf("%s log injection not neutralized: %q", tc.name, line)
		}
	}
}

func TestNASSMBLegacyConfigPreservesPasswordAndMasksMalformedURL(t *testing.T) {
	s, _ := newTestServerWithUIDir(t)
	const savedPass = "saved-smb-pass-777"
	const malformedPass = "malformed-smb-pass-888"

	// Username-only legacy SMB URL must not clear an explicit saved password.
	if err := providers.SaveJSON(s.nasConfigPath(), providers.NASConfig{
		Protocol: "smb",
		URL:      "smb://alice@nas.example/music",
		Password: savedPass,
		Enabled:  true,
	}); err != nil {
		t.Fatal(err)
	}
	loaded := s.loadNASConfig()
	if loaded.Username != "alice" || loaded.Password != savedPass || loaded.URL != "smb://nas.example/music" {
		t.Fatalf("loadNASConfig=%+v, want user=alice saved password preserved and clean URL", loaded)
	}
	w := httptest.NewRecorder()
	s.handleNASConfig(w, httptest.NewRequest(http.MethodGet, "/api/nas", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"hasPassword":true`) || strings.Contains(w.Body.String(), savedPass) {
		t.Fatalf("GET /api/nas=%d %s", w.Code, w.Body.String())
	}

	// Unparseable legacy SMB URL with embedded user:pass@ must never be echoed by GET /api/nas.
	if err := providers.SaveJSON(s.nasConfigPath(), providers.NASConfig{
		Protocol: "smb",
		URL:      "smb://alice:" + malformedPass + "@nas.example:badport/music",
		Enabled:  true,
	}); err != nil {
		t.Fatal(err)
	}
	wBad := httptest.NewRecorder()
	s.handleNASConfig(wBad, httptest.NewRequest(http.MethodGet, "/api/nas", nil))
	if wBad.Code != http.StatusOK {
		t.Fatalf("GET /api/nas malformed=%d %s", wBad.Code, wBad.Body.String())
	}
	if strings.Contains(wBad.Body.String(), malformedPass) || strings.Contains(wBad.Body.String(), "alice:"+malformedPass+"@") {
		t.Fatalf("GET /api/nas leaked malformed credentialed URL: %s", wBad.Body.String())
	}
}

// The review's reproduction (#335 round 2): after the SMB root moved from A to B on the
// same share, root A's IDs, search hits, download, database import and Stems source must
// not answer for root B's file at the same relative path.
func TestReviewSMBImportStemsAndSwitch(t *testing.T) {
	smbSrv := testsmb.Start(t)

	makeFLAC := func(seconds int) []byte {
		var buf bytes.Buffer
		if err := testaudio.FLAC(&buf, testaudio.Song(seconds)); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	flacA := makeFLAC(4)
	flacB := makeFLAC(6)
	if bytes.Equal(flacA, flacB) {
		t.Fatal("test fixtures must differ")
	}

	for _, tc := range []struct {
		root string
		data []byte
	}{
		{"rootA", flacA},
		{"rootB", flacB},
	} {
		dir := filepath.Join(smbSrv.Root, tc.root, "Sets")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SameTrack.flac"), tc.data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(smbSrv.Root, "rootA", "RootOnlyA.mp3"), testMP3(120), 0o600); err != nil {
		t.Fatal(err)
	}

	dbPath, libRoot, db := testlibrary.Create(t)
	lib, err := analyzer.OpenLibrary(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })

	uiDir, stateDir := t.TempDir(), t.TempDir()
	dlDir := filepath.Join(libRoot, "stream-cache")
	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=\nomni_source=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	newStack := func() (*Jobs, *Server, *httptest.Server) {
		jobs := NewJobs(Pipeline{
			Guard:   resourceguard.New(resourceguard.Config{Disabled: true}),
			Catalog: noCatalog{},
			Deemix:  &deemixclient.Client{DownloadDir: dlDir, AllowLossy: false},
			Library: lib,
		})
		s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
		gw := httptest.NewServer(s)
		return jobs, s, gw
	}

	jobs, s, gw := newStack()
	closed1 := false
	close1 := func() {
		if !closed1 {
			closed1 = true
			gw.Close()
			s.Close()
			jobs.Close()
		}
	}
	t.Cleanup(close1)

	saveSMB := func(srv *Server, root string) {
		t.Helper()
		body := fmt.Sprintf(`{"protocol":"smb","url":%q,"username":%q,"password":%q,"enabled":true}`,
			smbSrv.URL+"/"+root, smbSrv.User, smbSrv.Password)
		w := httptest.NewRecorder()
		srv.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/api/nas", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("save SMB %s failed (%d): %s", root, w.Code, w.Body.String())
		}
	}

	browseSetsTrack := func(baseURL string) (string, int64, bool) {
		t.Helper()
		var lists struct {
			Items []map[string]any `json:"items"`
		}
		tidalGet(t, baseURL, "/v1/users/1/playlists?limit=50&offset=0", 200, &lists)
		var setsUUID string
		hasStems := false
		for _, it := range lists.Items {
			switch it["title"] {
			case "Sets":
				setsUUID, _ = it["uuid"].(string)
			case "Stems":
				hasStems = true
			}
		}
		if setsUUID == "" {
			t.Fatalf("missing Sets playlist: %+v", lists.Items)
		}
		var tracks struct {
			Items []map[string]any `json:"items"`
		}
		tidalGet(t, baseURL, "/v1/playlists/"+setsUUID+"/tracks?limit=50&offset=0", 200, &tracks)
		if len(tracks.Items) != 1 || tracks.Items[0]["title"] != "SameTrack" {
			t.Fatalf("unexpected Sets tracks: %+v", tracks.Items)
		}
		return setsUUID, int64(tracks.Items[0]["id"].(float64)), hasStems
	}

	streamTrack := func(baseURL string, id int64) []byte {
		t.Helper()
		var pb struct {
			Manifest string `json:"manifest"`
		}
		tidalGet(t, baseURL, fmt.Sprintf("/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", id), 200, &pb)
		rawManifest, err := base64.StdEncoding.DecodeString(pb.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		var mf struct {
			Codecs string   `json:"codecs"`
			URLs   []string `json:"urls"`
		}
		if err := json.Unmarshal(rawManifest, &mf); err != nil || mf.Codecs != "flac" || len(mf.URLs) != 1 {
			t.Fatalf("manifest=%s err=%v", rawManifest, err)
		}
		resp, err := http.Get(mf.URLs[0])
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("stream %d status=%d", id, resp.StatusCode)
		}
		return data
	}

	waitImportedCount := func(want int) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var count int
			if err := db.QueryRow("SELECT COUNT(*) FROM Track WHERE title = 'SameTrack'").Scan(&count); err == nil && count >= want {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		var count int
		_ = db.QueryRow("SELECT COUNT(*) FROM Track WHERE title = 'SameTrack'").Scan(&count)
		t.Fatalf("Engine DB imported track count=%d, want >=%d (jobs=%+v)", count, want, jobs.List())
	}

	// 1. Configure root A, select standalone NAS, browse, stream exact FLAC A, import to DB, prepare Stems.
	saveSMB(s, "rootA")
	if code, updated := uiPost(t, gw.URL, "scope=nas"); code != http.StatusOK || updated.Scope != nasScope {
		t.Fatalf("scope=nas failed: code=%d scope=%q", code, updated.Scope)
	}
	uuidA, idA, _ := browseSetsTrack(gw.URL)
	gotA := streamTrack(gw.URL, idA)
	if !bytes.Equal(gotA, flacA) {
		t.Fatalf("root A stream bytes mismatch: got=%d want=%d", len(gotA), len(flacA))
	}
	waitImportedCount(1)

	var srcA struct {
		Path string `json:"path"`
	}
	tidalGet(t, gw.URL, fmt.Sprintf("/v1/enginex/stems/source?id=%d", idA), 200, &srcA)
	if st, err := os.Stat(srcA.Path); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("root A stems source missing: %q %v", srcA.Path, err)
	}
	var localA struct {
		Path string `json:"path"`
	}
	tidalGet(t, gw.URL, fmt.Sprintf("/v1/enginex/stems/local?id=%d", idA), 200, &localA)
	if localA.Path != srcA.Path {
		t.Fatalf("root A stems/local=%q, want %q", localA.Path, srcA.Path)
	}
	_, _, hasStemsA := browseSetsTrack(gw.URL)
	if !hasStemsA {
		t.Fatal("root A NAS Stems list missing after stems/source")
	}
	var searchA struct {
		Tracks struct {
			Items []map[string]any `json:"items"`
		} `json:"tracks"`
	}
	tidalGet(t, gw.URL, "/v1/search?query=RootOnlyA&limit=50&offset=0", 200, &searchA)
	if len(searchA.Tracks.Items) != 1 {
		t.Fatalf("root A search returned %+v, want 1 item", searchA.Tracks.Items)
	}

	// 2. Switch configured SMB root to root B on the same share.
	saveSMB(s, "rootB")

	// Stale root A gateway track/playlist IDs and search cache must be dropped immediately.
	if resp, err := http.Get(fmt.Sprintf("%s/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", gw.URL, idA)); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("stale root A track id=%d got HTTP %d, want 404", idA, resp.StatusCode)
		}
	}
	if resp, err := http.Get(gw.URL + "/v1/playlists/" + uuidA + "/tracks?limit=50&offset=0"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("stale root A playlist uuid=%s got HTTP %d, want 404", uuidA, resp.StatusCode)
		}
	}
	var searchB struct {
		Tracks struct {
			Items []map[string]any `json:"items"`
		} `json:"tracks"`
	}
	tidalGet(t, gw.URL, "/v1/search?query=RootOnlyA&limit=50&offset=0", 200, &searchB)
	if len(searchB.Tracks.Items) != 0 {
		t.Errorf("root A search hit leaked after switch to root B: %+v", searchB.Tracks.Items)
	}

	// Re-browse root B: must get a new track ID, no root A Stems list, no root A stems/local match, exact FLAC B bytes, and a second DB import.
	_, idB, hasStemsB := browseSetsTrack(gw.URL)
	if hasStemsB {
		t.Errorf("root A NAS Stems list survived switch to root B")
	}
	if idB == idA {
		t.Errorf("re-browsed root B reused root A gateway track ID %d", idA)
	}
	if resp, err := http.Get(fmt.Sprintf("%s/v1/enginex/stems/local?id=%d", gw.URL, idB)); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("root B track id=%d resolved stale root A stems file (HTTP %d, want 404)", idB, resp.StatusCode)
		}
	}
	gotB := streamTrack(gw.URL, idB)
	if bytes.Equal(gotB, flacA) || !bytes.Equal(gotB, flacB) {
		t.Fatalf("re-browsed root B stream matched root A=%v matched root B=%v (same gateway ID=%v)", bytes.Equal(gotB, flacA), bytes.Equal(gotB, flacB), idB == idA)
	}
	waitImportedCount(2)

	var srcB struct {
		Path string `json:"path"`
	}
	tidalGet(t, gw.URL, fmt.Sprintf("/v1/enginex/stems/source?id=%d", idB), 200, &srcB)
	if srcB.Path == "" || srcB.Path == srcA.Path {
		t.Fatalf("root B stems source=%q collided with root A %q", srcB.Path, srcA.Path)
	}
	if st, err := os.Stat(srcB.Path); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("root B stems source missing: %q %v", srcB.Path, err)
	}
}

// The review's second reproduction (#335 round 2): the standalone NAS entry is SMB only.
// An HTTP NAS keeps the released OmniSource [NAS] lists, whose enumeration drops upstream
// error text, and gets no standalone entry, list or search.
func TestReviewHTTPEnumerationDifference(t *testing.T) {
	const diagMarker = "upstream-diagnostic-marker-335"
	var subsonicHits, webdavHits, plexHits int
	subsonicMode := "error"

	subsonicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subsonicHits++
		w.Header().Set("Content-Type", "application/json")
		if subsonicMode == "error" {
			fmt.Fprintf(w, `{"subsonic-response":{"status":"failed","error":{"code":40,"message":%q}}}`, diagMarker)
			return
		}
		switch r.URL.Path {
		case "/rest/getPlaylists.view":
			fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlists":{"playlist":[{"id":"pl-1","name":"Club Crate","songCount":1}]}}}`)
		case "/rest/getPlaylist.view":
			fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlist":{"id":"pl-1","name":"Club Crate","entry":[{"id":"tr-1","title":"Subsonic Dub","artist":"DJ","duration":180,"suffix":"mp3"}]}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer subsonicSrv.Close()

	webdavSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webdavHits++
		http.Error(w, diagMarker, http.StatusBadGateway)
	}))
	defer webdavSrv.Close()

	plexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plexHits++
		http.Error(w, diagMarker, http.StatusUnauthorized)
	}))
	defer plexSrv.Close()

	cases := []struct {
		name    string
		cfgJSON string
		hits    *int
	}{
		{
			name:    "subsonic",
			cfgJSON: fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":"dj","password":"pw","enabled":true}`, subsonicSrv.URL),
			hits:    &subsonicHits,
		},
		{
			name:    "webdav",
			cfgJSON: fmt.Sprintf(`{"protocol":"webdav","url":%q,"username":"dj","password":"pw","enabled":true}`, webdavSrv.URL+"/music"),
			hits:    &webdavHits,
		},
		{
			name:    "plex",
			cfgJSON: fmt.Sprintf(`{"protocol":"plex","url":%q,"token":"tok","enabled":true}`, plexSrv.URL),
			hits:    &plexHits,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uiDir, stateDir := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=spotify\nscope=spotify\nomni_source=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, nil)
			t.Cleanup(s.Close)
			s.sessions["sp-owner"] = &Session{ID: "sp-owner", Provider: "spotify", Ready: true, p: &browseTestProvider{}}

			wCfg := httptest.NewRecorder()
			s.handleNASConfig(wCfg, httptest.NewRequest(http.MethodPost, "/api/nas", strings.NewReader(tc.cfgJSON)))
			if wCfg.Code != http.StatusOK {
				t.Fatalf("save %s config (%d): %s", tc.name, wCfg.Code, wCfg.Body.String())
			}

			gw := httptest.NewServer(s)
			defer gw.Close()

			// 1. Released OmniSource enumeration suppresses upstream error text and keeps working when healthy.
			respOmni, err := http.Get(gw.URL + "/v1/users/1/playlists?limit=50&offset=0")
			if err != nil {
				t.Fatal(err)
			}
			omniBody, _ := io.ReadAll(respOmni.Body)
			respOmni.Body.Close()
			if respOmni.StatusCode != 200 || strings.Contains(string(omniBody), diagMarker) {
				t.Fatalf("%s OmniSource leaked upstream marker or failed (%d): %s", tc.name, respOmni.StatusCode, omniBody)
			}
			if tc.name == "subsonic" {
				subsonicMode = "ok"
				var okOmni struct {
					Items []map[string]any `json:"items"`
				}
				tidalGet(t, gw.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &okOmni)
				foundNAS := false
				for _, it := range okOmni.Items {
					if it["title"] == "[NAS] Club Crate" {
						foundNAS = true
					}
				}
				if !foundNAS {
					t.Fatalf("released OmniSource [NAS] playlist missing for subsonic: %+v", okOmni.Items)
				}
				subsonicMode = "error"
			}

			// 2. No standalone NAS source in UI status, and scope=nas is rejected with 409.
			var status uiSettings
			tidalGet(t, gw.URL, "/v1/enginex/ui", 200, &status)
			for _, src := range append(append([]string(nil), status.Sources...), status.LoggedIn...) {
				if src == nasScope {
					t.Errorf("%s must not appear in standalone UI sources/loggedIn: sources=%v loggedIn=%v", tc.name, status.Sources, status.LoggedIn)
					break
				}
			}
			if code, _ := uiPost(t, gw.URL, "scope=nas"); code != http.StatusConflict {
				t.Errorf("%s scope=nas got HTTP %d, want 409", tc.name, code)
			}

			// 3. A stale persisted scope=nas and a stale standalone playlist give no standalone answer:
			// at most the local hint row, no search hit, 404 for the playlist's tracks, and no request
			// to the HTTP NAS (pl-1 exists on the Subsonic server).
			staleUUID := s.tidal.addPlaylist(playlistRef{
				session:  nasScope,
				provider: nasScope,
				list:     providers.Playlist{ID: "pl-1", Title: "Stale Standalone"},
			})
			if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=nas\nomni_source=0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			hitsBefore := *tc.hits
			for _, route := range []string{"/v1/users/1/playlists?limit=50&offset=0", "/v1/users/1/favorites/playlists?limit=50&offset=0"} {
				resp, err := http.Get(gw.URL + route)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if strings.Contains(string(body), diagMarker) {
					t.Errorf("%s %s exposed upstream marker in standalone response: %s", tc.name, route, body)
				}
				var list struct {
					Items []map[string]any `json:"items"`
				}
				if err := json.Unmarshal(body, &list); err != nil || resp.StatusCode != 200 {
					t.Fatalf("%s %s status=%d body=%s", tc.name, route, resp.StatusCode, body)
				}
				for _, it := range list.Items {
					if it["uuid"] != hintPlaylist {
						t.Errorf("%s %s listed standalone row %v, want only the local hint", tc.name, route, it)
					}
				}
			}
			var searchRes struct {
				Tracks struct {
					Items []map[string]any `json:"items"`
				} `json:"tracks"`
				Playlists struct {
					Items []map[string]any `json:"items"`
				} `json:"playlists"`
			}
			tidalGet(t, gw.URL, "/v1/search?query=Dub&limit=50&offset=0", 200, &searchRes)
			if len(searchRes.Tracks.Items) != 0 || len(searchRes.Playlists.Items) != 0 {
				t.Errorf("%s standalone search returned items: %+v", tc.name, searchRes)
			}
			resp, err := http.Get(gw.URL + "/v1/playlists/" + staleUUID + "/tracks?limit=50&offset=0")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound || strings.Contains(string(body), diagMarker) {
				t.Errorf("%s stale standalone playlist tracks: HTTP %d body=%s, want 404 without upstream text", tc.name, resp.StatusCode, body)
			}
			if *tc.hits != hitsBefore {
				t.Errorf("%s standalone NAS routes made %d upstream HTTP requests, want 0", tc.name, *tc.hits-hitsBefore)
			}
		})
	}
}

// Review of 8a3677ba, MEDIUM 3: the download source index names NAS files; like the other
// state files it is written owner-only.
func TestSourcesIndexWrittenMode0600(t *testing.T) {
	dlDir := t.TempDir()
	audioPath := filepath.Join(dlDir, "nas-fixture.mp3")
	if err := os.WriteFile(audioPath, testMP3(120), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: dlDir, AllowLossy: true},
	})
	t.Cleanup(jobs.Close)
	jobs.putFile("smb:Sets/Track.mp3", fetched{
		matched: providers.Track{ID: "Sets/Track.mp3", Provider: "smb"},
		dl:      &deemixclient.Download{Path: audioPath},
	})
	info, err := os.Stat(filepath.Join(dlDir, sourcesFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf(".sources.json mode=%o, want 0600", info.Mode().Perm())
	}
}
