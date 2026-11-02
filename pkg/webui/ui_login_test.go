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

func TestRelayRequestsCarryThePassword(t *testing.T) {
	var got string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Deemix-Password") + "|" + r.Header.Get("Authorization")
		io.WriteString(w, `{}`)
	}))
	defer relay.Close()
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: t.TempDir(), RelayURL: relay.URL}, nil)
	if err := ui.updateRelayConfig(nil, ptr("pw")); err != nil {
		t.Fatal(err)
	}
	ui.relayRegister(context.Background(), &loginFlow{Provider: "deezer", token: "t"})
	if got != "pw|Bearer pw" {
		t.Fatalf("%q", got)
	}
}

func TestRelayRefusesToFollowRedirects(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer other.Close()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer relay.Close()
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: t.TempDir(), RelayURL: relay.URL}, nil)
	ui.updateRelayConfig(nil, ptr("pw"))
	ui.relayRegister(context.Background(), &loginFlow{Provider: "deezer", token: "t"})
	if leaked {
		t.Fatal("password followed a redirect")
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

func TestRelayPasswordIsNeverReadBack(t *testing.T) {
	state := t.TempDir()
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state, RelayURL: "https://default.example"}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	r, err := http.Post(srv.URL+"/v1/sc5000/relay", "application/json", strings.NewReader(`{"deemixPassword":"hunter2"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if strings.Contains(string(body), "hunter2") || !strings.Contains(string(body), `"deemixPasswordSet":true`) || !strings.Contains(string(body), "default.example") {
		t.Fatalf("status %s", body)
	}
	if relay, password := ui.relaySettings(); relay != "https://default.example" || password != "hunter2" {
		t.Fatal(relay, password)
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
	if _, password := ui.relaySettings(); password != "changed" {
		t.Fatal(password)
	}
	r, err = http.Post(srv.URL+"/v1/sc5000/relay", "application/json", strings.NewReader(`{"relayUrl":"http://public.example"}`))
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

func TestFetchPrefersLocalDeemixThenFallsBackToNetease(t *testing.T) {
	var flac bytes.Buffer
	if err := testaudio.FLAC(&flac, testaudio.Song(2)); err != nil {
		t.Fatal(err)
	}
	var neteaseCalls, bridgeCalls atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("login cookie sent to the audio host")
		}
		w.Write(flac.Bytes())
	}))
	defer cdn.Close()
	netease := netEaseServer(t, func() string { neteaseCalls.Add(1); return cdn.URL + "/song.flac" })
	dir := t.TempDir()
	bridgeUp := atomic.Bool{}
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridgeCalls.Add(1)
		if !bridgeUp.Load() {
			http.Error(w, "no match", 500)
			return
		}
		os.WriteFile(filepath.Join(dir, "9.flac"), flac.Bytes(), 0o644)
		json.NewEncoder(w).Encode(deemixclient.QueueStatus{ID: "j", State: "done", Progress: 1, Path: "9.flac"})
	}))
	defer bridge.Close()
	catalog := &providers.Deezer{BaseURL: fakeDeezer(t).URL}
	jobs := NewJobs(Pipeline{Guard: resourceguard.New(resourceguard.Config{Disabled: true}), Catalog: catalog, Deemix: &deemixclient.Client{BaseURL: bridge.URL, DownloadDir: dir, AllowLossy: true}})
	defer jobs.Close()
	New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) {
		return &providers.Netease{WebURL: netease.URL, Cookie: "MUSIC_U=secret", UserID: 1}, nil
	}}, jobs)
	ctx := context.Background()

	// With Deemix working, NetEase's download quota is never touched.
	bridgeUp.Store(true)
	deezer := providers.Track{ID: "9", Provider: "deezer", Title: "Rare", Artist: "Nobody", DurationMS: 200000}
	if p, err := jobs.Fetch(ctx, "s", deezer, false); err != nil || filepath.Base(p) != "9.flac" {
		t.Fatalf("%q %v", p, err)
	}
	if neteaseCalls.Load() != 0 {
		t.Fatal("NetEase used while Deemix works")
	}

	// A NetEase-only track falls back to the login, by its own ID.
	bridgeUp.Store(false)
	own := providers.Track{ID: "77", Provider: "netease", Title: "Song", Artist: "Artist", DurationMS: 200000}
	if p, err := jobs.Fetch(ctx, "s", own, false); err != nil || filepath.Base(p) != "netease-77.flac" {
		t.Fatalf("%q %v", p, err)
	}
	// Another service's track falls back by name and duration.
	other := providers.Track{ID: "abc", Provider: "spotify", Title: "Song", Artist: "Artist", DurationMS: 200000}
	if p, err := jobs.Fetch(ctx, "s", other, false); err != nil || filepath.Base(p) != "netease-5.flac" {
		t.Fatalf("%q %v", p, err)
	}
	// A saved NetEase file is reused without asking anyone.
	jobs2 := NewJobs(Pipeline{Guard: resourceguard.New(resourceguard.Config{Disabled: true}), Catalog: catalog, Deemix: &deemixclient.Client{BaseURL: bridge.URL, DownloadDir: dir, AllowLossy: true}})
	defer jobs2.Close()
	calls := bridgeCalls.Load()
	if p, err := jobs2.Fetch(ctx, "s", own, false); err != nil || filepath.Base(p) != "netease-77.flac" || bridgeCalls.Load() != calls {
		t.Fatalf("%q %v", p, err)
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

func TestRelayBPMKeysSyncToTheDeck(t *testing.T) {
	var auth string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("X-Deemix-Password")
		if r.URL.RequestURI() != "/api/config?secrets=1" {
			t.Errorf("unexpected %s", r.URL)
		}
		io.WriteString(w, `{"ok":true,"config":{"soundcloudClientId":"from-relay","getSongBpmKey":""}}`)
	}))
	defer relay.Close()
	state := t.TempDir()
	bpm := &providers.BPMStore{Path: filepath.Join(state, "bpm.json"), ConfigPath: filepath.Join(state, "bpm-config.json")}
	bpm.SaveConfig(providers.BPMConfig{GetSongBPMKey: "local", SoundCloudClientID: "old"})
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, RelayURL: relay.URL, BPM: bpm}, nil)

	// Nothing is sent or asked without the relay's password.
	if err := ui.pullBPMConfig(context.Background()); err != nil || auth != "" {
		t.Fatalf("%v %q", err, auth)
	}
	ui.updateRelayConfig(nil, ptr("pw"))
	if err := ui.pullBPMConfig(context.Background()); err != nil || auth != "pw" {
		t.Fatalf("%v %q", err, auth)
	}
	got := bpm.Config()
	if got.SoundCloudClientID != "from-relay" || got.GetSongBPMKey != "local" {
		t.Fatalf("%+v", got)
	}
}

func TestPreviewAnalyzerMeasuresAClipAndKeepsNothing(t *testing.T) {
	var clip bytes.Buffer
	if err := testaudio.FLAC(&clip, testaudio.Song(30)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/short" {
			var short bytes.Buffer
			testaudio.FLAC(&short, testaudio.Song(2))
			w.Write(short.Bytes())
			return
		}
		w.Write(clip.Bytes())
	}))
	defer srv.Close()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	analyze := PreviewAnalyzer(resourceguard.New(resourceguard.Config{Disabled: true}))
	if got := analyze(context.Background(), srv.URL+"/clip"); got.BPM <= 0 || got.Measured {
		t.Fatalf("%+v", got)
	}
	if got := analyze(context.Background(), srv.URL+"/short"); got.BPM != 0 {
		t.Fatalf("short clip: %+v", got)
	}
	if got := analyze(context.Background(), srv.URL+"/missing\x7f"); got.BPM != 0 {
		t.Fatalf("%+v", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind %v", entries)
	}
}

func TestUnblockRadiosLeavesOtherSwitchesAlone(t *testing.T) {
	root := t.TempDir()
	for name, kind := range map[string]string{"rfkill0": "bluetooth", "rfkill1": "wlan", "rfkill2": "nfc"} {
		os.MkdirAll(filepath.Join(root, name), 0o755)
		os.WriteFile(filepath.Join(root, name, "type"), []byte(kind+"\n"), 0o644)
		os.WriteFile(filepath.Join(root, name, "soft"), []byte("1"), 0o644)
	}
	UnblockRadios(root)
	for name, want := range map[string]string{"rfkill0": "0", "rfkill1": "0", "rfkill2": "1"} {
		if got, _ := os.ReadFile(filepath.Join(root, name, "soft")); string(got) != want {
			t.Fatalf("%s: %s", name, got)
		}
	}
}

func TestLongTracksSkipAnalysis(t *testing.T) {
	if !tooLongToAnalyse(providers.Track{DurationMS: 2 * 60 * 60 * 1000}, "/x/a.mp3") {
		t.Fatal("two-hour set would be analysed")
	}
	if tooLongToAnalyse(providers.Track{DurationMS: 5 * 60 * 1000}, "/missing/a.flac") {
		t.Fatal("ordinary track skipped")
	}
}

func TestRelayCollectsSoundCloudAndDeckChecksIt(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_id") != "cid" {
			t.Errorf("client id missing: %s", r.URL)
		}
		switch r.URL.Path {
		case "/resolve":
			io.WriteString(w, `{"kind":"user","id":7,"username":"cave"}`)
		default:
			io.WriteString(w, `{"collection":[]}`)
		}
	}))
	defer api.Close()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"done","soundcloud":{"user":"cave","clientId":"cid","token":""}}`)
	}))
	defer relay.Close()
	state := t.TempDir()
	bpm := &providers.BPMStore{Path: filepath.Join(state, "bpm.json"), ConfigPath: filepath.Join(state, "bpm-config.json")}
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, RelayURL: relay.URL, SoundCloudURL: api.URL, BPM: bpm}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	r, err := http.Post(srv.URL+"/v1/sc5000/login?provider=soundcloud", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if !strings.HasPrefix(ui.logins["soundcloud"].Target, relay.URL+"/m/soundcloud?t=") {
		t.Fatalf("%+v", ui.logins["soundcloud"])
	}
	status, err := http.Get(srv.URL + "/v1/sc5000/login?provider=soundcloud")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(status.Body)
	status.Body.Close()
	if !strings.Contains(string(body), `"status":"done"`) {
		t.Fatalf("%s", body)
	}
	if p := ui.ownerSoundCloud(); p == nil || p.UserID != 7 || p.ClientID != "cid" {
		t.Fatalf("%+v", p)
	}
	if got := bpm.Config().SoundCloudClientID; got != "cid" {
		t.Fatal(got)
	}
	info, err := os.Stat(filepath.Join(state, "soundcloud-owner.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
}
