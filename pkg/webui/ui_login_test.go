package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Cd1s/sc5000-custom-os/internal/testaudio"
	"github.com/Cd1s/sc5000-custom-os/pkg/deemixclient"
	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
	"github.com/Cd1s/sc5000-custom-os/pkg/resourceguard"
)

func deezerProfiles(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": 7, "title": "Mine", "nb_tracks": 1}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPhoneLoginNeedsTokenAndLinksDeezer(t *testing.T) {
	state := t.TempDir()
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state, DeezerURL: deezerProfiles(t).URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=deezer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	flow := ui.logins["deezer"]
	if flow == nil || !regexp.MustCompile(`:8080/m/deezer\?t=`).MatchString(flow.Target) {
		t.Fatalf("flow %+v", flow)
	}
	qr, err := http.Get(srv.URL + "/v1/sc5000/login/qr?provider=deezer")
	if err != nil {
		t.Fatal(err)
	}
	qr.Body.Close()
	if qr.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("qr type %s", qr.Header.Get("Content-Type"))
	}
	bad, err := http.PostForm(srv.URL+"/m/deezer?t=wrong", url.Values{"user": {"123"}})
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != 403 {
		t.Fatalf("wrong token: %d", bad.StatusCode)
	}
	token := flow.token
	ok, err := http.PostForm(srv.URL+"/m/deezer?t="+token, url.Values{"user": {"https://www.deezer.com/profile/123?x=1"}})
	if err != nil {
		t.Fatal(err)
	}
	ok.Body.Close()
	data, err := os.ReadFile(filepath.Join(state, "deezer-owner.json"))
	if err != nil || !strings.Contains(string(data), "123") {
		t.Fatalf("not saved: %s %v", data, err)
	}
	// The token works once, and the deck learns that the login finished.
	again, err := http.PostForm(srv.URL+"/m/deezer?t="+token, url.Values{"user": {"456"}})
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != 403 {
		t.Fatalf("reused token: %d", again.StatusCode)
	}
	status, err := http.Get(srv.URL + "/v1/sc5000/login?provider=deezer")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(status.Body)
	status.Body.Close()
	if !strings.Contains(string(body), `"status":"done"`) {
		t.Fatalf("status %s", body)
	}
}

func TestSpotifyPhonePageUsesTheDefaultClient(t *testing.T) {
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=spotify", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	page, err := http.Get(srv.URL + "/m/spotify?t=" + ui.logins["spotify"].token)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), DefaultSpotifyClientID) {
		t.Fatalf("page %s", body)
	}
}

func TestSavedSpotifyClientBeatsTheDefault(t *testing.T) {
	state := t.TempDir()
	os.WriteFile(filepath.Join(state, "spotify-client-id"), []byte("mine\n"), 0o600)
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state}, nil)
	if got := ui.spotifyClientID(); got != "mine" {
		t.Fatal(got)
	}
}

func TestPhonePostAcceptsNullOriginButConsoleRejectsForeignOrigin(t *testing.T) {
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), DeezerURL: deezerProfiles(t).URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=deezer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	post := func(path, origin, form string) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := post("/m/deezer?t="+ui.logins["deezer"].token, "null", "user=123"); code != 200 {
		t.Fatalf("null origin on phone page: %d", code)
	}
	if code := post("/api/sessions", "https://evil.example", "{}"); code != 403 {
		t.Fatalf("foreign origin on console: %d", code)
	}
}

func TestRelayLoginCollectsBothServices(t *testing.T) {
	const arl = "abcdef0123456789abcdef0123456789"
	var registered []map[string]string
	status := "waiting"
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/relay/session" {
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			registered = append(registered, v)
			io.WriteString(w, `{}`)
			return
		}
		if r.URL.Path != "/api/relay/session" || r.URL.Query().Get("t") == "" {
			t.Errorf("unexpected %s", r.URL)
		}
		out := map[string]any{"status": status}
		if status == "done" {
			out["spotifyToken"] = map[string]any{"access_token": "AT", "refresh_token": "RT", "expires_in": 3600, "client_id": "cid"}
			out["deezer"] = map[string]any{"userId": "42", "arl": arl}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer relay.Close()
	state := t.TempDir()
	tokenPath := filepath.Join(state, "spotify-token.json")
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state, SpotifyTokenPath: tokenPath, RelayURL: relay.URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	poll := func(provider string) string {
		r, err := http.Get(srv.URL + "/v1/sc5000/login?provider=" + provider)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var v struct{ Status string }
		json.NewDecoder(r.Body).Decode(&v)
		return v.Status
	}
	for _, provider := range []string{"spotify", "deezer"} {
		status = "scanned"
		r, err := http.Post(srv.URL+"/v1/sc5000/login?provider="+provider, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		var flow struct{ Target string }
		json.NewDecoder(r.Body).Decode(&flow)
		r.Body.Close()
		if !strings.HasPrefix(flow.Target, relay.URL+"/m/"+provider+"?t=") {
			t.Fatalf("target %s", flow.Target)
		}
		if got := poll(provider); got != "scanned" {
			t.Fatalf("%s: %s", provider, got)
		}
		status = "done"
		if got := poll(provider); got != "done" {
			t.Fatalf("%s: %s", provider, got)
		}
	}
	if len(registered) != 2 || registered[0]["spotifyClientId"] != DefaultSpotifyClientID {
		t.Fatalf("registered %v", registered)
	}
	saved, err := os.ReadFile(tokenPath)
	if err != nil || !strings.Contains(string(saved), `"refresh_token":"RT"`) {
		t.Fatalf("token %s %v", saved, err)
	}
	owner := ui.savedDeezerOwner()
	if owner.UserID != "42" || owner.ARL != arl {
		t.Fatalf("owner %+v", owner)
	}
}

func TestRelayPasswordOnlyGoesToTheDeemixHost(t *testing.T) {
	var got []string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-Deemix-Password"))
		io.WriteString(w, `{}`)
	}))
	defer relay.Close()
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: t.TempDir(), RelayURL: relay.URL, RemoteDeemixURL: relay.URL}, nil)
	if err := ui.updateDeemixConfig(nil, ptr("pw")); err != nil {
		t.Fatal(err)
	}
	ui.relayRegister(context.Background(), &loginFlow{Provider: "deezer", token: "t"})
	if err := ui.updateDeemixConfig(ptr("https://other.example"), nil); err != nil {
		t.Fatal(err)
	}
	ui.relayRegister(context.Background(), &loginFlow{Provider: "deezer", token: "t"})
	if len(got) != 2 || got[0] != "pw" || got[1] != "" {
		t.Fatalf("%q", got)
	}
}

func TestRelayOutageStillShowsItsQRCode(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	defer relay.Close()
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), RelayURL: relay.URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	r, err := http.Post(srv.URL+"/v1/sc5000/login?provider=spotify", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 || !strings.HasPrefix(ui.logins["spotify"].Target, relay.URL+"/m/spotify") {
		t.Fatalf("%d %+v", r.StatusCode, ui.logins["spotify"])
	}
}

func TestDeemixPasswordIsNeverReadBack(t *testing.T) {
	state := t.TempDir()
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state, RemoteDeemixURL: "https://default.example"}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	r, err := http.Post(srv.URL+"/v1/sc5000/deemix", "application/json", strings.NewReader(`{"deemixPassword":"hunter2"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if strings.Contains(string(body), "hunter2") || !strings.Contains(string(body), `"deemixPasswordSet":true`) || !strings.Contains(string(body), "default.example") {
		t.Fatalf("status %s", body)
	}
	if address, password := ui.deemixRemote(); address != "https://default.example" || password != "hunter2" {
		t.Fatal(address, password)
	}
	info, err := os.Stat(filepath.Join(state, "deemix-config.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", info, err)
	}
	r, err = http.Post(srv.URL+"/v1/sc5000/ui?deemixPassword=changed", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if strings.Contains(string(body), "changed") {
		t.Fatalf("status %s", body)
	}
	if _, password := ui.deemixRemote(); password != "changed" {
		t.Fatal(password)
	}
	r, err = http.Post(srv.URL+"/v1/sc5000/deemix", "application/json", strings.NewReader(`{"deemixUrl":"http://public.example"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Fatalf("plain HTTP accepted: %d", r.StatusCode)
	}
}

func netEaseServer(t *testing.T, audioURL func() string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/weapi/song/enhance/player/url/v1":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []any{map[string]any{"url": audioURL()}}})
		case "/weapi/cloudsearch/pc":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []any{map[string]any{"id": 5, "name": "Song", "dt": 203000, "ar": []any{map[string]any{"name": "Artist"}}}}}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchTriesNeteaseThenBackendForEveryService(t *testing.T) {
	var flac bytes.Buffer
	if err := testaudio.FLAC(&flac, testaudio.Song(2)); err != nil {
		t.Fatal(err)
	}
	var backendHits atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("login cookie sent to the audio host")
		}
		if r.URL.Path == "/api/audio" {
			backendHits.Add(1)
			if r.Header.Get("X-Deemix-Password") != "pw" {
				http.Error(w, "denied", 401)
				return
			}
		}
		w.Write(flac.Bytes())
	}))
	defer cdn.Close()
	netease := netEaseServer(t, func() string { return cdn.URL + "/song.flac" })
	bridgeDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	defer bridgeDown.Close()
	catalog := &providers.Deezer{BaseURL: fakeDeezer(t).URL}
	dir := t.TempDir()
	jobs := NewJobs(Pipeline{Guard: resourceguard.New(resourceguard.Config{Disabled: true}), Catalog: catalog, Deemix: &deemixclient.Client{BaseURL: bridgeDown.URL, DownloadDir: dir, AllowLossy: true}})
	defer jobs.Close()
	state := t.TempDir()
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, RemoteDeemixURL: cdn.URL, Factory: func(string, bool, string) (providers.Provider, error) {
		return &providers.Netease{WebURL: netease.URL, Cookie: "MUSIC_U=secret", UserID: 1}, nil
	}}, jobs)
	if err := ui.updateDeemixConfig(nil, ptr("pw")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A NetEase track comes straight from the login, by its own ID.
	own := providers.Track{ID: "77", Provider: "netease", Title: "x", Artist: "y"}
	if p, err := jobs.Fetch(ctx, "s", own, false); err != nil || filepath.Base(p) != "netease-77.flac" {
		t.Fatalf("%q %v", p, err)
	}
	// Another service's track is matched by name and duration to a NetEase one.
	other := providers.Track{ID: "abc", Provider: "spotify", Title: "Song", Artist: "Artist", DurationMS: 200000}
	if p, err := jobs.Fetch(ctx, "s", other, false); err != nil || filepath.Base(p) != "netease-5.flac" {
		t.Fatalf("%q %v", p, err)
	}
	if backendHits.Load() != 0 {
		t.Fatal("backend asked for tracks NetEase has")
	}
	// A track NetEase does not have falls through to the backend.
	rare := providers.Track{ID: "9", Provider: "deezer", Title: "Rare", Artist: "Nobody", DurationMS: 200000}
	if p, err := jobs.Fetch(ctx, "s", rare, false); err != nil || filepath.Base(p) != "remote-deezer-9.flac" {
		t.Fatalf("%q %v", p, err)
	}
	if backendHits.Load() != 1 {
		t.Fatalf("backend hits %d", backendHits.Load())
	}
}

func TestGuestNeverUsesTheOwnersNeteaseLogin(t *testing.T) {
	state := t.TempDir()
	os.WriteFile(filepath.Join(state, "netease-owner.json"), []byte(`{"Cookie":"MUSIC_U=owner","UserID":1}`), 0o600)
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state}, nil)
	if ui.neteaseFor("nobody", false) == nil {
		t.Fatal("owner login not found")
	}
	if ui.neteaseFor("nobody", true) != nil {
		t.Fatal("guest got the owner's login")
	}
}

func ptr[T any](v T) *T { return &v }
