package webui

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

type spotifyRoundTrip func(*http.Request) (*http.Response, error)

func (f spotifyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSpotifyDeviceQRUsesOfficialConfirmation(t *testing.T) {
	var calls int
	hc := &http.Client{Transport: spotifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.String() != "https://accounts.spotify.com/oauth2/device/authorize" {
			t.Errorf("unexpected device request: %s", r.URL.Path)
		}
		r.ParseForm()
		if r.Form.Get("client_id") != DefaultSpotifyClientID || !strings.Contains(r.Form.Get("scope"), "streaming") {
			t.Error("wrong built-in grant")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"device_code":"private-fixture-code","user_code":"ABCD","verification_uri_complete":"https://accounts.spotify.com/pair?user_code=ABCD","expires_in":600,"interval":5}`))}, nil
	})}
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "spotify-client-id"), []byte(testSpotifyClientID), 0600); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{StateDir: state, UIDir: t.TempDir(), HTTP: hc, RelayURL: "https://relay.example.invalid"}, nil)
	defer ui.Close()
	w := httptest.NewRecorder()
	ui.loginStart(w, httptest.NewRequest("POST", "/v1/enginex/login?provider=spotify", nil))
	if w.Code != 200 || calls != 1 || strings.Contains(w.Body.String(), "private-fixture-code") {
		t.Fatal("device code leaked or request failed")
	}
	var out struct {
		Target   string
		UserCode string
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Target != "https://accounts.spotify.com/pair?user_code=ABCD" || out.UserCode != "ABCD" {
		t.Fatal("QR did not use official confirmation")
	}
	flow := ui.logins["spotify"]
	if flow.viaRelay {
		t.Fatal("device QR was routed through the configured relay")
	}
	page := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/m/spotify?t="+flow.token, nil)
	req.SetPathValue("provider", "spotify")
	ui.phoneGet(page, req)
	if !strings.Contains(page.Body.String(), "https://accounts.spotify.com/pair?user_code=ABCD") || strings.Contains(page.Body.String(), "127.0.0.1") {
		t.Fatal("device phone page did not offer direct confirmation")
	}
	w = httptest.NewRecorder()
	ui.loginStatus(w, httptest.NewRequest("GET", "/v1/enginex/login/status?provider=spotify", nil))
	if calls != 1 || flow.Status != "waiting" {
		t.Fatal("polling interval ignored")
	}
	ui.mu.Lock()
	flow.expires = time.Now().Add(-time.Minute)
	ui.mu.Unlock()
	ui.loginStatus(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/enginex/login/status?provider=spotify", nil))
	if flow.Status != "expired" {
		t.Fatal("expired device code still offered")
	}
}
func TestSpotifyDeviceEndpointOfflineKeepsPKCE(t *testing.T) {
	ui := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), HTTP: &http.Client{Transport: spotifyRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}}, nil)
	defer ui.Close()
	w := httptest.NewRecorder()
	ui.loginStart(w, httptest.NewRequest("POST", "/v1/enginex/login?provider=spotify", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/m/spotify?t=") {
		t.Fatal("offline login fallback missing")
	}
	flow := ui.logins["spotify"]
	w = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/m/spotify?t="+flow.token, nil)
	req.SetPathValue("provider", "spotify")
	ui.phoneGet(w, req)
	if !strings.Contains(w.Body.String(), "code_challenge") || !strings.Contains(w.Body.String(), "callback") {
		t.Fatal("PKCE/paste form missing")
	}
}
func TestSpotifyStoredReadinessDeviceIDAndLogout(t *testing.T) {
	state := t.TempDir()
	creds := filepath.Join(state, "spotify-librespot.json")
	id, identityErr := providers.LoadSpotifyDeviceID(state)
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	if err := providers.SaveJSON(creds, map[string]any{"username": "fixture-user", "credentials": []byte("fixture-credentials"), "device_id": id}); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{StateDir: state, UIDir: t.TempDir()}, nil)
	defer ui.Close()
	session, err := ui.ownerSession("spotify")
	if err != nil {
		t.Fatal(err)
	}
	p := session.p.(*providers.Spotify)
	if p.HasToken() || !p.HasStoredCredentials() || !session.Ready || !ready(p) || p.DeviceID != id {
		t.Fatal("LAN credentials did not unlock owner session")
	}
	w := httptest.NewRecorder()
	ui.spotifyConnectGet(w, httptest.NewRequest("GET", "/spotify-connect", nil))
	var info providers.SpotifyConnectInfo
	json.Unmarshal(w.Body.Bytes(), &info)
	if info.DeviceID != p.DeviceID || len(info.DeviceID) != 40 {
		t.Fatal("advertised and AP IDs differ")
	}
	guest, err := ui.makeProvider("spotify", true, "")
	if err != nil || guest.(*providers.Spotify).CredsPath != "" {
		t.Fatal("guest received owner credentials path")
	}
	if err = p.SetToken(providers.Token{AccessToken: "fixture-token"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("DELETE", "/api/sessions/"+session.ID, nil)
	req.SetPathValue("id", session.ID)
	ui.deleteSession(httptest.NewRecorder(), req)
	for _, path := range []string{creds, p.TokenPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("logout retained credentials")
		}
	}
	if p.HasToken() || p.HasStoredCredentials() {
		t.Fatal("logout retained login")
	}
	if err = p.SetToken(providers.Token{AccessToken: "late-token"}); err == nil {
		t.Fatal("late login resurrected logged-out session")
	}
}
func TestSpotifyMalformedLANPairingWithoutModalIsRefused(t *testing.T) {
	ui := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	defer ui.Close()
	w := httptest.NewRecorder()
	ui.spotifyConnectPost(w, httptest.NewRequest("POST", "/spotify-connect", bytes.NewBufferString("action=addUser&userName=fixture&blob=bad&clientKey=bad")))
	var response providers.SpotifyConnectResponse
	json.Unmarshal(w.Body.Bytes(), &response)
	if response.Status != 402 || ui.ownerSpotify() != nil {
		t.Fatal("malformed LAN pairing was acknowledged")
	}
}

func TestSpotifyBuiltinGrantSurvivesSavedLegacyClientID(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "spotify-client-id"), []byte(testSpotifyClientID), 0600); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(filepath.Join(state, "spotify-token.json"), providers.Token{AccessToken: "fixture-device-token", ClientID: providers.SpotifyBuiltinClientID}); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{StateDir: state, UIDir: t.TempDir()}, nil)
	defer ui.Close()
	made, err := ui.makeProvider("spotify", false, "")
	if err != nil {
		t.Fatal(err)
	}
	sp := made.(*providers.Spotify)
	if !sp.HasToken() || sp.ClientID != providers.SpotifyBuiltinClientID {
		t.Fatal("built-in grant was discarded because of a saved legacy client ID")
	}
}

// #210 joins native decryption, real Vorbis inspection/analysis and Engine SQL
// import. The fixture is original 44.1-kHz stereo C-major audio with 120-BPM
// onsets, encoded once as Vorbis; this test needs neither ffmpeg nor Spotify.
func TestSpotifyDownloadAnalyzeImportEndToEnd(t *testing.T) {
	fixture, err := os.ReadFile("testdata/spotify-native-44100.ogg")
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef")
	iv := []byte{0x72, 0xe0, 0x67, 0xfb, 0xdd, 0xcb, 0xcf, 0x77, 0xeb, 0xe8, 0xbc, 0x64, 0x3f, 0x63, 0x0d, 0x93}
	plain := append(testaudio.SpotifyOggMetadataPage(139), fixture...)
	encrypted := make([]byte, len(plain))
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCTR(block, iv).XORKeyStream(encrypted, plain)
	var requests atomic.Int32
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/encrypted-vorbis" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("unexpected Spotify mock request")
			w.WriteHeader(400)
			return
		}
		w.Write(encrypted)
	}))
	defer stream.Close()
	dbPath, root, db := testlibrary.Create(t)
	lib, err := analyzer.OpenLibrary(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	var catalogCalls, bridgeCalls atomic.Int32
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bridgeCalls.Add(1); w.WriteHeader(500) }))
	defer bridge.Close()
	jobs := NewJobs(Pipeline{
		Guard:   resourceguard.New(resourceguard.Config{Disabled: true}),
		Catalog: forbiddenSpotifyCatalog{calls: &catalogCalls},
		Deemix:  &deemixclient.Client{DownloadDir: root, BaseURL: bridge.URL}, Library: lib,
	})
	defer jobs.Close()
	sp := &providers.Spotify{StreamURL: stream.URL + "/encrypted-vorbis", AudioKey: key, Token: providers.Token{AccessToken: "fixture-token"}}
	defer sp.Close()
	jobs.spotify = func() *providers.Spotify { return sp }
	track := providers.Track{ID: "4uLU6hMCjMI75M1A2tKUQC", Provider: "spotify", Title: "Native Vorbis fixture", Artist: "EngineX fixture", DurationMS: 8000, CoverURL: bridge.URL + "/cover"}
	submitted, err := jobs.Submit("spotify-owner", track, false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var completed Job
	for {
		for _, job := range jobs.List() {
			if job.ID == submitted.ID {
				completed = job
			}
		}
		if completed.State == "done" || completed.State == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Spotify job timed out in state %s", completed.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed.State != "done" || completed.TrackID <= 0 || completed.Progress != 1 {
		t.Fatalf("Spotify job failed: state=%s error=%s", completed.State, completed.Error)
	}
	path := filepath.Join(root, "spotify-"+track.ID+".ogg")
	saved, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(saved, fixture) {
		t.Fatal("AES decryption, Spotify header stripping, or cover handling changed Vorbis data")
	}
	quality, err := deemixclient.Inspect(path)
	if err != nil || quality.Format != "ogg" || quality.SampleRate != 44100 || quality.Bitrate != 320000 || quality.Lossless {
		t.Fatalf("incorrect Vorbis inspection: %+v, %v", quality, err)
	}
	audio, analysis, err := analyzer.AnalyzeFile(path, analyzer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if audio.Format != quality.Format || audio.SampleRate != quality.SampleRate || audio.Channels != 2 || audio.BitsPerSample != 16 {
		t.Fatal("decoded audio and inspected quality disagree")
	}
	for _, result := range []*analyzer.Result{analysis, jobs.Result(submitted.ID)} {
		if result == nil || math.Abs(result.BPM-120) > 3 || result.Key.Camelot == "" || len(result.Waveform) == 0 || len(result.Overview) != analyzer.OverviewEntries || len(result.Beatgrid) < 2 {
			t.Fatal("BPM, key, beatgrid or waveform missing from full analysis")
		}
	}
	var filename, fileType, comment, playlist string
	var analyzed int
	var overview, beats, trackData []byte
	err = db.QueryRow(`SELECT t.filename,t.fileType,t.comment,p.title,t.isAnalyzed,pd.overviewWaveFormData,pd.beatData,pd.trackData FROM Track t JOIN PerformanceData pd ON pd.trackId=t.id JOIN PlaylistEntity pe ON pe.trackId=t.id JOIN Playlist p ON p.id=pe.listId WHERE t.id=?`, completed.TrackID).Scan(&filename, &fileType, &comment, &playlist, &analyzed, &overview, &beats, &trackData)
	if err != nil {
		t.Fatal(err)
	}
	if filename != filepath.Base(path) || fileType != "ogg" || !strings.Contains(comment, "OGG 44100 Hz") || strings.Contains(comment, "MP3") || playlist != providers.OwnerPlaylist || analyzed != 1 || len(overview) == 0 || len(beats) == 0 || len(trackData) == 0 {
		t.Fatal("Vorbis import did not persist verified Engine analysis and playlist membership")
	}
	if requests.Load() != 1 || catalogCalls.Load() != 0 || bridgeCalls.Load() != 0 {
		t.Fatal("Spotify job downloaded twice or used another service")
	}
}
