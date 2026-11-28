package webui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testsmb"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

func TestDeckShareWrongCodeRateLimitAndGrantSecurity(t *testing.T) {
	deckA, _ := copyTestServer(t)
	deckA.cfg.AccessToken = "ABCD-EFGH"
	if err := providers.SaveJSON(deckA.deezerOwnerPath(), deezerOwner{UserID: "101", ARL: "secret-arl-value"}); err != nil {
		t.Fatal(err)
	}
	srvA := httptest.NewServer(deckA)
	defer srvA.Close()

	deckB, _ := copyTestServer(t)
	linkPeerForTest(t, deckB, deckA, srvA.URL)

	// 1. Console /api/deck-share requires auth; /v1/enginex/deck-share works locally.
	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"no auth", "", http.StatusUnauthorized},
		{"wrong auth", "Bearer WRONG", http.StatusUnauthorized},
		{"valid auth", "Bearer TEST-CODE", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := copyTestRequest(deckB, http.MethodGet, "/api/deck-share", "", "192.0.2.10:1234", tc.auth)
			if w.Code != tc.want {
				t.Fatalf("GET /api/deck-share status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}

	// 2. Empty code rejected cleanly without touching B state.
	w := copyTestRequest(deckB, http.MethodPost, "/v1/enginex/deck-share", `{"action":"pull","peer":"127.0.0.1","code":""}`, "127.0.0.1:1234", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty code status=%d body=%s", w.Code, w.Body.String())
	}

	// 3. 5 wrong attempts from a LAN peer lock it out (429), and even the right code fails during lockout.
	tok := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for i := 0; i < 4; i++ {
		rw := copyTestRequest(deckA, http.MethodPost, "/peer/deck-share/request", `{"code":"BAD1-BAD1","token":"`+tok+`"}`, "192.168.100.20:1234", "")
		if rw.Code != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d: status=%d body=%s", i+1, rw.Code, rw.Body.String())
		}
	}
	rw := copyTestRequest(deckA, http.MethodPost, "/peer/deck-share/request", `{"code":"BAD2-BAD2","token":"`+tok+`"}`, "192.168.100.20:1234", "")
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("5th wrong attempt: status=%d, want 429", rw.Code)
	}
	rw = copyTestRequest(deckA, http.MethodPost, "/peer/deck-share/request", `{"code":"ABCD-EFGH","token":"`+tok+`"}`, "192.168.100.20:1234", "")
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("right code during lockout: status=%d, want 429", rw.Code)
	}
	deckA.shareMu.Lock()
	grantCount := len(deckA.shareGrants)
	deckA.shareMu.Unlock()
	if grantCount != 0 {
		t.Fatalf("expected zero staged grants during lockout, got %d", grantCount)
	}

	// Pulling via deckB with wrong code leaves deckB state empty.
	for i := 0; i < 5; i++ {
		w := copyTestRequest(deckB, http.MethodPost, "/v1/enginex/deck-share", `{"action":"pull","peer":"127.0.0.1","code":"WRONG-9999"}`, "127.0.0.1:1234", "")
		if i < 4 && w.Code != http.StatusUnauthorized {
			t.Fatalf("pull wrong code %d: status=%d body=%s", i+1, w.Code, w.Body.String())
		}
		if i == 4 && w.Code != http.StatusTooManyRequests {
			t.Fatalf("pull 5th wrong code: status=%d, want 429", w.Code)
		}
	}
	for _, p := range []string{deckB.deezerOwnerPath(), deckB.nasConfigPath(), deckB.deckShareUndoPath()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("file %s appeared after wrong codes", p)
		}
	}

	// 4. Single-use and peer-IP bound transfer grant from a fresh peer IP.
	tok2 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rw = copyTestRequest(deckA, http.MethodPost, "/peer/deck-share/request", `{"code":"ab cd ef gh","token":"`+tok2+`"}`, "192.168.100.30:1234", "")
	if rw.Code != http.StatusOK || strings.Contains(rw.Body.String(), "secret-arl-value") {
		t.Fatalf("valid request failed or leaked secret: %d %s", rw.Code, rw.Body.String())
	}
	rw = copyTestRequest(deckA, http.MethodGet, "/peer/deck-share/transfer", "", "192.168.100.31:1234", "DeckShare "+tok2)
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("wrong peer IP transfer status=%d, want 401", rw.Code)
	}
	rw = copyTestRequest(deckA, http.MethodGet, "/peer/deck-share/transfer", "", "192.168.100.30:1234", "DeckShare "+tok2)
	if rw.Code != http.StatusOK {
		t.Fatalf("valid peer IP transfer status=%d body=%s", rw.Code, rw.Body.String())
	}
	rw = copyTestRequest(deckA, http.MethodGet, "/peer/deck-share/transfer", "", "192.168.100.30:1234", "DeckShare "+tok2)
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("replayed grant status=%d, want 401", rw.Code)
	}

	// 5. Non-LAN and proxied requests refused; ForwardHandler returns 404.
	wNonLAN := copyTestRequest(deckA, http.MethodPost, "/peer/deck-share/request", `{"code":"ABCD-EFGH","token":"`+tok2+`"}`, "8.8.8.8:1234", "")
	if wNonLAN.Code != http.StatusForbidden {
		t.Fatalf("non-LAN request status=%d, want 403", wNonLAN.Code)
	}
	reqProxied := httptest.NewRequest(http.MethodGet, "/peer/deck-share/transfer", nil)
	reqProxied.RemoteAddr = "192.168.100.30:1234"
	reqProxied.Header.Set("Cf-Ray", "edge")
	wProxied := httptest.NewRecorder()
	deckA.ServeHTTP(wProxied, reqProxied)
	if wProxied.Code != http.StatusNotFound {
		t.Fatalf("proxied transfer status=%d, want 404", wProxied.Code)
	}

	fwd := httptest.NewServer(deckA.ForwardHandler())
	defer fwd.Close()
	resp, err := http.Post(fwd.URL+"/peer/deck-share/request", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("forwarded /peer/deck-share/request: HTTP %d, want 404", resp.StatusCode)
	}
}

func TestDeckShareAppleMusicHelperFilterAndReload(t *testing.T) {
	amLoopback, err := url.Parse("http://127.0.0.1:12340")
	_ = amLoopback
	_ = err

	for _, tc := range []struct {
		name       string
		helperURL  string
		wantCopied bool
	}{
		{"loopback helper skipped", "http://127.0.0.1:12340", false},
		{"LAN helper copied", "http://192.0.2.55:12340", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deckA, _ := copyTestServer(t)
			if err := providers.SaveJSON(filepath.Join(deckA.cfg.StateDir, "applemusic-owner.json"), providers.AppleMusicOwner{
				Storefront: "us",
				UserToken:  "am-token-fixture",
			}); err != nil {
				t.Fatal(err)
			}
			if err := providers.SaveJSON(deckA.appleMusicHelperPath(), appleMusicHelperSettings{
				AppleMusicHelperConfig: providers.AppleMusicHelperConfig{URL: tc.helperURL, Token: "helper-token-fixture"},
			}); err != nil {
				t.Fatal(err)
			}
			_ = deckA.appleMusic.Reload()
			deckA.loadAppleMusicHelper()

			bundle, _, err := deckA.exportDeckShareBundle()
			if err != nil {
				t.Fatal(err)
			}
			if bundle.AppleMusic == nil {
				t.Fatal("expected applemusic bundle")
			}
			if tc.wantCopied && (bundle.AppleMusic.Helper == nil || bundle.AppleMusic.Helper.URL != tc.helperURL) {
				t.Fatalf("expected non-loopback helper %q, got %+v", tc.helperURL, bundle.AppleMusic.Helper)
			}
			if !tc.wantCopied && bundle.AppleMusic.Helper != nil {
				t.Fatalf("expected loopback helper to be omitted, got %+v", bundle.AppleMusic.Helper)
			}
		})
	}
}

func TestDeckShareBundleCopyBrowsePlayAllServicesAndUndo(t *testing.T) {
	dzAudio := testMP3(120)
	neAudio := testMP3(128)
	amAudio := testMP3(135)
	scAudio := testMP3(140)
	davAudio := testMP3(145)
	smbAudio := testMP3(150)

	const (
		dzARL      = "0123456789abcdef0123456789abcdef"
		neCookie   = "MUSIC_U=ne-cookie-fixture-001;"
		spAccess   = "sp-access-fixture-002"
		spRefresh  = "sp-refresh-fixture-003"
		amUserTok  = "am-user-token-fixture-004"
		amHelTok   = "am-helper-token-fixture-005"
		scCID      = "sc-cid-fixture-006"
		scSec      = "sc-sec-fixture-007"
		scStatic   = "sc-static-token-fixture-008"
		scRotRef   = "sc-rotating-refresh-do-not-copy"
		bpSecret   = "bp-rotating-secret-do-not-copy"
		tdSecret   = "td-rotating-secret-do-not-copy"
		davUser    = "davshare"
		davPass    = "dav-share-password-fixture"
		amDevToken = "eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJURUFNIiwiZXhwIjo0MTAyNDQ0ODAwfQ.c2lnbmF0dXJlLWJ5dGVzLWxvbmctZW5vdWdo"
	)

	// Shared Deemix cache + mock queue HTTP verifying B uses copied deezer ARL.
	dlDirB := t.TempDir()
	dzCacheFile := filepath.Join(dlDirB, "9001.mp3")
	if err := os.WriteFile(dzCacheFile, dzAudio, 0o644); err != nil {
		t.Fatal(err)
	}
	var stateBDir string
	deemixSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stateBDir != "" {
			if b, err := os.ReadFile(filepath.Join(stateBDir, "deezer-owner.json")); err != nil || !bytes.Contains(b, []byte(dzARL)) {
				http.Error(w, "missing deezer ARL on deck B", http.StatusUnauthorized)
				return
			}
		}
		state := "queued"
		if r.Method == http.MethodGet {
			state = "completed"
		}
		json.NewEncoder(w).Encode(deemixclient.QueueStatus{ID: "dq1", State: state, Progress: 1, Path: dzCacheFile})
	}))
	defer deemixSrv.Close()

	// Deezer API + Gateway
	dzSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/101/playlists":
			io.WriteString(w, `{"data":[{"id":701,"title":"DZ Share Set","nb_tracks":1}]}`)
		case "/chart/0/playlists":
			io.WriteString(w, `{"data":[]}`)
		case "/playlist/701/tracks":
			io.WriteString(w, `{"data":[{"id":9001,"title":"Deezer Track","duration":180,"isrc":"USRC11111111","artist":{"name":"DZ Artist"},"album":{"title":"DZ Album"}}]}`)
		case "/track/isrc:USRC22222222":
			io.WriteString(w, `{"id":9001,"title":"Spotify Matched","duration":180,"isrc":"USRC22222222","artist":{"name":"SP Artist"},"album":{"title":"SP Album"}}`)
		case "/search":
			io.WriteString(w, `{"data":[{"id":9001,"title":"Deezer Track","duration":180,"artist":{"name":"DZ Artist"},"album":{"title":"DZ Album"}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer dzSrv.Close()

	// NetEase API
	var neSrv *httptest.Server
	neSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/audio/ne.mp3" {
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(neAudio)
			return
		}
		_ = r.ParseForm()
		if !strings.Contains(r.FormValue("cookie"), "MUSIC_U=ne-cookie-fixture-001") && !strings.Contains(r.Header.Get("Cookie"), "MUSIC_U=ne-cookie-fixture-001") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/playlist":
			io.WriteString(w, `{"code":200,"playlist":[{"id":601,"name":"NE Share Set","trackCount":1}]}`)
		case "/album/sublist":
			io.WriteString(w, `{"code":200,"data":[],"hasMore":false}`)
		case "/dj/sublist":
			io.WriteString(w, `{"code":200,"djRadios":[],"hasMore":false}`)
		case "/playlist/detail":
			io.WriteString(w, `{"code":200,"playlist":{"id":601,"name":"NE Share Set","trackIds":[{"id":6001}],"tracks":[{"id":6001,"name":"NetEase Track","dt":190000,"ar":[{"name":"NE Artist"}],"al":{"name":"NE Album"}}]}}`)
		case "/song/detail":
			io.WriteString(w, `{"code":200,"songs":[{"id":6001,"name":"NetEase Track","dt":190000,"ar":[{"name":"NE Artist"}],"al":{"name":"NE Album"}}]}`)
		case "/song/url/v1":
			fmt.Fprintf(w, `{"code":200,"data":[{"id":6001,"url":"%s/audio/ne.mp3","type":"mp3"}]}`, neSrv.URL)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer neSrv.Close()

	// Spotify API
	spSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+spAccess {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me":
			io.WriteString(w, `{"id":"spuser"}`)
		case "/me/playlists":
			io.WriteString(w, `{"items":[{"id":"sp1","name":"SP Share Set","tracks":{"total":1}}]}`)
		case "/me/tracks":
			io.WriteString(w, `{"total":1,"items":[{"track":{"id":"spt1","name":"Spotify Track","duration_ms":180000,"external_ids":{"isrc":"USRC22222222"},"artists":[{"name":"SP Artist"}],"album":{"name":"SP Album"}}}]}`)
		case "/playlists/sp1/items":
			io.WriteString(w, `{"items":[{"track":{"id":"spt1","name":"Spotify Track","duration_ms":180000,"external_ids":{"isrc":"USRC22222222"},"artists":[{"name":"SP Artist"}],"album":{"name":"SP Album"}}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer spSrv.Close()

	// Apple Music API + Web + Helper
	var amSrv *httptest.Server
	amSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/us/browse":
			io.WriteString(w, `<script type="module" src="/assets/index-Ab12.js"></script>`)
			return
		case "/assets/index-Ab12.js":
			fmt.Fprintf(w, `const k="%s";`, amDevToken)
			return
		case "/v1/download":
			if r.Header.Get("Authorization") != "Bearer "+amHelTok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"file":"/file.mp3","quality":{"lossless":false,"sampleRate":44100,"channels":2}}`)
			return
		case "/file.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(amAudio)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+amDevToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		song := `{"id":"1440","type":"songs","attributes":{"name":"Apple Track","artistName":"AM Artist","albumName":"AM Album","url":"https://music.apple.com/us/song/1440","durationInMillis":200000}}`
		switch r.URL.Path {
		case "/v1/me/library/playlists", "/v1/me/library/albums", "/v1/me/library/songs":
			io.WriteString(w, `{"data":[]}`)
		case "/v1/catalog/us/playlists/pl.decka":
			io.WriteString(w, `{"data":[{"id":"pl.decka","type":"playlists","attributes":{"name":"AM Share Set"},"relationships":{"tracks":{"data":[`+song+`]}}}]}`)
		case "/v1/catalog/us/playlists/pl.decka/tracks":
			io.WriteString(w, `{"data":[`+song+`]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer amSrv.Close()

	// SoundCloud API
	var scSrv *httptest.Server
	scSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sc.mp3" {
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(scAudio)
			return
		}
		if r.Header.Get("Authorization") != "OAuth "+scStatic {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me":
			io.WriteString(w, `{"id":42,"username":"dj-a","consumer_subscription":{"product":{"id":"consumer-high-tier"}}}`)
		case "/me/playlists", "/users/42/playlists":
			io.WriteString(w, `{"collection":[{"id":801,"title":"SC Share Set","track_count":1}]}`)
		case "/me/likes/playlists", "/me/playlist_likes", "/users/42/likes/playlists", "/users/42/playlist_likes":
			io.WriteString(w, `{"collection":[]}`)
		case "/me/likes/tracks", "/me/track_likes", "/users/42/likes/tracks", "/users/42/track_likes":
			io.WriteString(w, `{"collection":[{"id":901,"title":"SC Track","duration":180000,"access":"playable","user":{"username":"SC Artist"}}]}`)
		case "/playlists/801/tracks":
			io.WriteString(w, `{"collection":[{"id":901,"title":"SC Track","duration":180000,"access":"playable","user":{"username":"SC Artist"}}]}`)
		case "/tracks/901":
			io.WriteString(w, `{"id":901,"title":"SC Track","duration":180000,"access":"playable","user":{"username":"SC Artist"}}`)
		case "/tracks/901/streams":
			fmt.Fprintf(w, `{"http_mp3_128_url":"%s/sc.mp3"}`, scSrv.URL)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer scSrv.Close()

	// WebDAV + SMB
	davSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(davUser+":"+davPass))
		if r.Header.Get("Authorization") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == "PROPFIND" {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			if strings.TrimSuffix(r.URL.Path, "/") == "/music" {
				io.WriteString(w, davXML("/music/", "/music/DeckA/", "/music/root-dav.mp3"))
			} else {
				io.WriteString(w, davXML("/music/DeckA/", "/music/DeckA/a.mp3"))
			}
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(davAudio)
	}))
	defer davSrv.Close()
	smb := testsmb.Start(t)
	if err := os.WriteFile(filepath.Join(smb.Root, "smb-share-track.mp3"), smbAudio, 0o600); err != nil {
		t.Fatal(err)
	}

	// Build Deck A with ALL services (shareable + non-shareable Beatport/TIDAL + device-bound exclusions).
	uiDirA, stateDirA := t.TempDir(), t.TempDir()
	bpmA := &providers.BPMStore{Path: filepath.Join(stateDirA, "bpm-cache.json"), ConfigPath: filepath.Join(stateDirA, "bpm-config.json")}
	if err := os.WriteFile(filepath.Join(uiDirA, "settings.conf"), []byte("webdav=1\nnas=1\nsources=deezer,netease\nscope=deezer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(filepath.Join(stateDirA, "deezer-owner.json"), deezerOwner{UserID: "101", ARL: dzARL}); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(filepath.Join(stateDirA, "netease-owner.json"), struct {
		Cookie string
		UserID int64
	}{neCookie, 202}); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(filepath.Join(stateDirA, "spotify-token.json"), providers.Token{
		AccessToken:  spAccess,
		RefreshToken: spRefresh,
		TokenType:    "Bearer",
		ClientID:     providers.SpotifyBuiltinClientID,
		Expiry:       time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDirA, "spotify-links.json"), []byte(`[{"id":"37i9dQZF1DXcBWIGoYBM5M","name":"SP Linked Set","tracks":[{"ID":"sptl1","Title":"SP Linked Track","Artist":"SP Artist","Album":"SP Album","ISRC":"USRC22222222","DurationMS":180000,"Provider":"spotify"}]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDirA, "spotify-device-id"), []byte("deck-a-device-id-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDirA, "spotify-librespot.json"), []byte(`{"username":"librespot-device-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(filepath.Join(stateDirA, "applemusic-owner.json"), providers.AppleMusicOwner{
		Storefront: "us",
		UserToken:  amUserTok,
		Links:      []string{"https://music.apple.com/us/playlist/am/pl.decka"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(filepath.Join(stateDirA, "soundcloud-owner.json"), soundCloudOwner{
		UserID:   42,
		Username: "dj-a",
		ClientID: scCID,
		Secret:   scSec,
		Token:    scStatic,
	}); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(filepath.Join(stateDirA, "soundcloud-token.json"), struct {
		RefreshToken string    `json:"refresh_token"`
		AccessToken  string    `json:"access_token"`
		ExpiresAt    time.Time `json:"expires_at"`
	}{RefreshToken: scRotRef, AccessToken: "sc-rot-access", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := bpmA.UpdateConfig(func(c *providers.BPMConfig) {
		c.SoundCloudClientID = scCID
		c.SoundCloudSecret = scSec
		c.SoundCloudToken = scStatic
	}); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(filepath.Join(stateDirA, "beatport-owner.json"), struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
	}{"bp-acc", bpSecret, time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := providers.SaveJSON(filepath.Join(stateDirA, "tidal-auth.json"), map[string]any{
		"accessToken":  "td-acc",
		"refreshToken": tdSecret,
		"expiresAt":    time.Now().Add(time.Hour),
		"userId":       "777",
	}); err != nil {
		t.Fatal(err)
	}
	rawNASA, _ := json.Marshal(nasFile{
		NASConfig: providers.NASConfig{Protocol: "smb", URL: smb.URL, Username: smb.User, Password: smb.Password, Enabled: true},
		Name:      "Deck A NAS",
		WebDAV:    []savedServer{{Name: "Deck A WebDAV", URL: davSrv.URL + "/music/", Username: davUser, Password: davPass, Enabled: true}},
	})
	if err := os.WriteFile(filepath.Join(uiDirA, "nas.json"), rawNASA, 0o600); err != nil {
		t.Fatal(err)
	}

	deckA := New(Config{
		AccessToken:  "SHOW-9876",
		UIDir:        uiDirA,
		StateDir:     stateDirA,
		BPM:          bpmA,
		BeatportURL:  "http://127.0.0.1:1",
		TidalAPIURL:  "http://127.0.0.1:1",
		TidalAuthURL: "http://127.0.0.1:1",
	}, nil)
	defer deckA.Close()
	srvA := httptest.NewServer(deckA)
	defer srvA.Close()

	// Build Deck B initially with ZERO logins, ZERO servers, and custom settings.conf formatting.
	uiDirB := t.TempDir()
	stateDirB := t.TempDir()
	stateBDir = stateDirB
	origSettingsB := []byte("# local formatting preserved\nlang=en\nwaveform=denon\nsources=tidal\nscope=tidal\n")
	if err := os.WriteFile(filepath.Join(uiDirB, "settings.conf"), origSettingsB, 0o600); err != nil {
		t.Fatal(err)
	}
	bpmB := &providers.BPMStore{Path: filepath.Join(stateDirB, "bpm-cache.json"), ConfigPath: filepath.Join(stateDirB, "bpm-config.json")}
	jobsB := NewJobs(Pipeline{
		Guard:   resourceguard.New(resourceguard.Config{Disabled: true}),
		Catalog: &providers.Deezer{BaseURL: dzSrv.URL},
		Deemix:  &deemixclient.Client{BaseURL: deemixSrv.URL, QueuePath: "/api/queue", DownloadDir: dlDirB, AllowLossy: true, PollInterval: time.Millisecond},
	})
	defer jobsB.Close()
	deckB := New(Config{
		AccessToken:           "DEST-1111",
		UIDir:                 uiDirB,
		StateDir:              stateDirB,
		BPM:                   bpmB,
		DeezerURL:             dzSrv.URL,
		NeteaseURL:            neSrv.URL,
		SpotifyBaseURL:        spSrv.URL,
		SoundCloudURL:         scSrv.URL,
		AppleMusicURL:         amSrv.URL,
		AppleMusicWebURL:      amSrv.URL,
		AppleMusicHelperURL:   amSrv.URL,
		AppleMusicHelperToken: amHelTok,
	}, jobsB)
	defer deckB.Close()
	srvB := httptest.NewServer(deckB)
	defer srvB.Close()

	linkPeerForTest(t, deckB, deckA, srvA.URL)

	// Enter Deck A's device code ONCE (case-insensitive, spaces permitted).
	w := copyTestRequest(deckB, http.MethodPost, "/v1/enginex/deck-share", `{"action":"pull","peer":"127.0.0.1","code":"show 9876"}`, "127.0.0.1:1234", "")
	if w.Code != http.StatusOK {
		t.Fatalf("deck share pull failed: %d %s", w.Code, w.Body.String())
	}
	bodyStr := w.Body.String()
	for _, secret := range []string{dzARL, neCookie, spAccess, spRefresh, amUserTok, amHelTok, scSec, scStatic, scRotRef, bpSecret, tdSecret, davPass, smb.Password} {
		if strings.Contains(bodyStr, secret) {
			t.Fatalf("deck share response leaked secret %q: %s", secret, bodyStr)
		}
	}
	var pullRes struct {
		OK      bool             `json:"ok"`
		Undo    bool             `json:"undo"`
		Summary deckShareSummary `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pullRes); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"deezer", "netease", "spotify", "applemusic", "soundcloud"} {
		if !slices.Contains(pullRes.Summary.Copied, want) {
			t.Fatalf("missing copied service %q in %+v", want, pullRes.Summary)
		}
	}
	if !slices.Contains(pullRes.Summary.WebDAV, "Deck A WebDAV") || !slices.Contains(pullRes.Summary.NAS, "Deck A NAS") {
		t.Fatalf("missing copied servers in summary: %+v", pullRes.Summary)
	}
	skippedJoined := strings.Join(pullRes.Summary.Skipped, "\n")
	if !strings.Contains(skippedJoined, "beatport") || !strings.Contains(skippedJoined, "tidal") {
		t.Fatalf("expected skipped Beatport and TIDAL in %+v", pullRes.Summary.Skipped)
	}
	if !pullRes.Undo {
		t.Fatal("expected undo=true after pull")
	}

	// Device-bound / rotating secrets were NOT copied.
	for _, excluded := range []string{"spotify-librespot.json", "soundcloud-token.json", "beatport-owner.json", "tidal-auth.json"} {
		if _, err := os.Stat(filepath.Join(stateDirB, excluded)); !os.IsNotExist(err) {
			t.Fatalf("excluded credential %s was copied to B", excluded)
		}
	}
	if bDev, err := os.ReadFile(filepath.Join(stateDirB, "spotify-device-id")); err == nil && strings.Contains(string(bDev), "deck-a-device-id-only") {
		t.Fatalf("deck A spotify-device-id leaked to B: %q", bDev)
	}

	// Shut down Deck A completely so every browse/load/play on B is proven independent of A!
	srvA.Close()
	deckA.Close()

	// Check B's uiStatus reflects all copied logins & servers and zero duplicate linkedServers.
	var statusAfter uiSettings
	tidalGet(t, srvB.URL, "/v1/enginex/ui", 200, &statusAfter)
	for _, want := range []string{"deezer", "netease", "spotify", "applemusic", "soundcloud", "nas"} {
		if !slices.Contains(statusAfter.LoggedIn, want) {
			t.Fatalf("missing %q in B loggedIn after copy: %v", want, statusAfter.LoggedIn)
		}
	}
	if !statusAfter.WebDAVOn || !statusAfter.NASOn || len(statusAfter.WebDAV) == 0 {
		t.Fatalf("expected copied WebDAV and NAS on B: webdavOn=%v nasOn=%v webdav=%+v", statusAfter.WebDAVOn, statusAfter.NASOn, statusAfter.WebDAV)
	}
	wdScopeB := statusAfter.WebDAV[0].Scope

	// Helper to switch scope on B, browse first playlist + first track, and stream it!
	browseAndPlay := func(scope string, wantAudio []byte) {
		t.Helper()
		if code, body := uiPost(t, srvB.URL, "scope="+url.QueryEscape(scope)); code != 200 {
			t.Fatalf("scope=%s: HTTP %d (%+v)", scope, code, body)
		}
		var plPage struct{ Items []gwItem }
		tidalGet(t, srvB.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &plPage)
		if len(plPage.Items) == 0 {
			t.Fatalf("scope=%s: no playlists returned", scope)
		}
		plID := fmt.Sprint(plPage.Items[0]["uuid"])
		for _, it := range plPage.Items {
			title := fmt.Sprint(it["title"])
			cnt, _ := it["numberOfTracks"].(float64)
			if cnt > 0 && title != "SP Linked Set" {
				plID = fmt.Sprint(it["uuid"])
				break
			}
		}
		var trPage struct{ Items []gwItem }
		tidalGet(t, srvB.URL, "/v1/playlists/"+url.PathEscape(plID)+"/tracks?limit=50&offset=0", 200, &trPage)
		if len(trPage.Items) == 0 {
			t.Fatalf("scope=%s playlist=%s: no tracks returned", scope, plID)
		}
		trackID := rowID(trPage.Items[0])
		r := &webdavRig{s: deckB, gw: srvB}
		code, got := r.stream(t, trackID)
		if code != http.StatusOK || !bytes.Equal(got, wantAudio) {
			t.Fatalf("scope=%s stream track %d: HTTP %d len=%d want=%d", scope, trackID, code, len(got), len(wantAudio))
		}
	}

	browseAndPlay("deezer", dzAudio)
	browseAndPlay("netease", neAudio)
	browseAndPlay("spotify", dzAudio) // Spotify matches against copied Deezer ISRC/catalog + downloads via Deemix!
	browseAndPlay("applemusic", amAudio)
	browseAndPlay("soundcloud", scAudio)
	browseAndPlay(wdScopeB, davAudio)
	browseAndPlay("nas", smbAudio)

	// Verify status endpoint exposes undo=true + lastCopy, then Undo cleans up everything byte-for-byte.
	wStat := copyTestRequest(deckB, http.MethodGet, "/v1/enginex/deck-share", "", "127.0.0.1:1234", "")
	if wStat.Code != http.StatusOK || !strings.Contains(wStat.Body.String(), `"undo":true`) {
		t.Fatalf("deck-share status before undo: %d %s", wStat.Code, wStat.Body.String())
	}
	wUndo := copyTestRequest(deckB, http.MethodPost, "/v1/enginex/deck-share", `{"action":"undo"}`, "127.0.0.1:1234", "")
	if wUndo.Code != http.StatusOK || !strings.Contains(wUndo.Body.String(), `"undo":false`) {
		t.Fatalf("deck-share undo failed: %d %s", wUndo.Code, wUndo.Body.String())
	}

	gotSettings, err := os.ReadFile(filepath.Join(uiDirB, "settings.conf"))
	if err != nil || !bytes.Equal(gotSettings, origSettingsB) {
		t.Fatalf("settings.conf after undo = %q (%v), want %q", gotSettings, err, origSettingsB)
	}
	for _, removed := range []string{
		filepath.Join(stateDirB, "deezer-owner.json"),
		filepath.Join(stateDirB, "netease-owner.json"),
		filepath.Join(stateDirB, "spotify-token.json"),
		filepath.Join(stateDirB, "applemusic-owner.json"),
		filepath.Join(stateDirB, "soundcloud-owner.json"),
		filepath.Join(uiDirB, "nas.json"),
		deckB.deckShareUndoPath(),
	} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed after undo", removed)
		}
	}
	var statusUndo uiSettings
	tidalGet(t, srvB.URL, "/v1/enginex/ui", 200, &statusUndo)
	if len(statusUndo.LoggedIn) != 0 || statusUndo.WebDAVOn || statusUndo.NASOn {
		t.Fatalf("expected clean state on B after undo, got loggedIn=%v webdavOn=%v nasOn=%v", statusUndo.LoggedIn, statusUndo.WebDAVOn, statusUndo.NASOn)
	}
}
