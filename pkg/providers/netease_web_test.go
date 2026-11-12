package providers

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNeteaseDirectQRLogin(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("params") == "" || len(r.FormValue("encSecKey")) != 256 {
			t.Errorf("%s: not a weapi request", r.URL.Path)
		}
		switch r.URL.Path {
		case "/weapi/login/qrcode/unikey":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "unikey": "K1"})
		case "/weapi/login/qrcode/client/login":
			polls++
			if polls == 1 {
				json.NewEncoder(w).Encode(map[string]any{"code": 802})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "MUSIC_U", Value: "secret"})
			json.NewEncoder(w).Encode(map[string]any{"code": 803})
		case "/weapi/w/nuser/account/get":
			if !strings.Contains(r.Header.Get("Cookie"), "MUSIC_U=secret") {
				t.Error("login cookie not sent")
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "profile": map[string]any{"userId": 42}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	n := &Netease{WebURL: srv.URL, CookiePath: t.TempDir() + "/owner.json"}
	q, err := n.BeginQR(context.Background())
	if err != nil || q.Key != "K1" || q.URL != "https://music.163.com/login?codekey=K1" {
		t.Fatalf("%+v %v", q, err)
	}
	if code, err := n.PollQR(context.Background(), q.Key); err != nil || code != 802 {
		t.Fatalf("%d %v", code, err)
	}
	if code, err := n.PollQR(context.Background(), q.Key); err != nil || code != 803 || n.UserID != 42 {
		t.Fatalf("%d %v uid=%d", code, err, n.UserID)
	}
}

func TestNeteasePollQRRejectsNullProfileWithoutOverwritingLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/weapi/login/qrcode/client/login":
			http.SetCookie(w, &http.Cookie{Name: "MUSIC_U", Value: "unverified-cookie"})
			json.NewEncoder(w).Encode(map[string]any{"code": 803})
		case "/weapi/w/nuser/account/get":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "profile": nil})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cookiePath := t.TempDir() + "/owner.json"
	n := &Netease{WebURL: srv.URL, CookiePath: cookiePath, Cookie: "MUSIC_U=existing", UserID: 99}
	code, err := n.PollQR(context.Background(), "K2")
	if err == nil || code == 803 {
		t.Fatalf("PollQR accepted null profile: code=%d err=%v LoggedIn=%v", code, err, n.LoggedIn())
	}
	if !n.LoggedIn() || n.UserID != 99 || n.Cookie != "MUSIC_U=existing" {
		t.Fatalf("failed PollQR clobbered prior login: Cookie=%q UserID=%d", n.Cookie, n.UserID)
	}
	if _, statErr := os.Stat(cookiePath); !os.IsNotExist(statErr) {
		t.Fatalf("failed PollQR wrote CookiePath: %v", statErr)
	}
}

func TestWeapiEncryptionMatchesTheFormat(t *testing.T) {
	cipherText, err := weapiAES([]byte("hello"), weapiNonce)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(cipherText)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher([]byte(weapiNonce))
	cipher.NewCBCDecrypter(block, []byte(weapiIV)).CryptBlocks(data, data)
	if string(data[:5]) != "hello" || data[len(data)-1] != 11 {
		t.Fatalf("%q", data)
	}
	params, key, err := weapiEncrypt([]byte(`{}`))
	if err != nil || params == "" || len(key) != 256 {
		t.Fatalf("%q %q %v", params, key, err)
	}
}

func TestNeteaseAudioSkipsPreviews(t *testing.T) {
	levels := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/weapi/song/enhance/player/url/v1" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		levels = append(levels, "x")
		switch len(levels) {
		case 1: // lossless is only a preview
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []any{map[string]any{"url": "https://cdn/trial", "freeTrialInfo": map[string]any{"start": 0, "end": 30}}}})
		case 2: // exhigh has nothing
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []any{map[string]any{"url": nil}}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []any{map[string]any{"url": "https://cdn/full", "freeTrialInfo": nil}}})
		}
	}))
	defer srv.Close()
	n := &Netease{WebURL: srv.URL, Cookie: "MUSIC_U=x", UserID: 1}
	got, err := n.AudioURL(context.Background(), "7", true)
	if err != nil || got != "https://cdn/full" || len(levels) != 3 {
		t.Fatalf("%q %v %d", got, err, len(levels))
	}
	if _, err := n.AudioURL(context.Background(), "1,2", true); err == nil {
		t.Fatal("bad ID accepted")
	}
}

func TestNeteaseAudioReportsPreviewOnlyTracks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []any{map[string]any{"url": "https://cdn/trial", "freeTrialInfo": map[string]any{"end": 30}}}})
	}))
	defer srv.Close()
	n := &Netease{WebURL: srv.URL, Cookie: "MUSIC_U=x", UserID: 1}
	if _, err := n.AudioURL(context.Background(), "7", true); !errors.Is(err, ErrNoFullAudio) {
		t.Fatal(err)
	}
}

func TestNeteaseSearchReadsBothResponseShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []any{
			map[string]any{"id": 1, "name": "New", "dt": 1000, "ar": []any{map[string]any{"name": "A"}}, "al": map[string]any{"name": "L"}},
			map[string]any{"id": 2, "name": "Old", "duration": 2000, "artists": []any{map[string]any{"name": "B"}}, "album": map[string]any{"name": "M"}},
		}}})
	}))
	defer srv.Close()
	got, err := (&Netease{WebURL: srv.URL}).Search(context.Background(), "q")
	if err != nil || len(got) != 2 || got[0].Artist != "A" || got[0].Album != "L" || got[0].DurationMS != 1000 || got[1].Artist != "B" || got[1].Album != "M" || got[1].DurationMS != 2000 {
		t.Fatalf("%+v %v", got, err)
	}
}

type redirects map[string]string

func (m redirects) RoundTrip(r *http.Request) (*http.Response, error) {
	location, ok := m[r.URL.String()]
	if !ok {
		return nil, errors.New("unexpected request to " + r.URL.String())
	}
	return &http.Response{StatusCode: 301, Header: http.Header{"Location": {location}}, Body: http.NoBody, Request: r}, nil
}

func TestDeezerShortLinkResolvesToUser(t *testing.T) {
	d := &Deezer{HTTP: &http.Client{Transport: redirects{"https://link.deezer.com/s/abc": "https://link.deezer.com/?awf=https%3A%2F%2Fwww.deezer.com%2Fprofile%2F685%3Fhost%3D685"}}}
	got, err := d.ResolveUser(context.Background(), " https://link.deezer.com/s/abc ")
	if err != nil || got != "685" {
		t.Fatalf("%q %v", got, err)
	}
	for raw, want := range map[string]string{"123": "123", "https://www.deezer.com/en/profile/9?x=1": "9"} {
		if got, err := d.ResolveUser(context.Background(), raw); err != nil || got != want {
			t.Fatalf("%s: %q %v", raw, got, err)
		}
	}
}

func TestDeezerLinkNeverLeavesDeezer(t *testing.T) {
	d := &Deezer{HTTP: &http.Client{Transport: redirects{"https://link.deezer.com/s/abc": "https://evil.example/next"}}}
	if _, err := d.ResolveUser(context.Background(), "https://link.deezer.com/s/abc"); err == nil {
		t.Fatal("followed a foreign redirect")
	}
	if _, err := d.ResolveUser(context.Background(), "https://evil.example/s/abc"); err == nil {
		t.Fatal("fetched a foreign link")
	}
}

func TestDeezerPlaylistLinkFindsCreator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"creator": map[string]any{"id": 55}})
	}))
	defer srv.Close()
	got, err := (&Deezer{BaseURL: srv.URL}).ResolveUser(context.Background(), "https://www.deezer.com/playlist/77")
	if err != nil || got != "55" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestDeezerARLIdentifiesAccountWithoutEchoing(t *testing.T) {
	const arl = "abcdef0123456789abcdef0123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "arl="+arl || r.URL.Query().Get("method") != "deezer.getUserData" || strings.Contains(r.URL.RawQuery, arl) {
			t.Errorf("bad request %s", r.URL)
		}
		io.WriteString(w, `{"results":{"USER":{"USER_ID":685}}}`)
	}))
	defer srv.Close()
	d := &Deezer{GatewayURL: srv.URL}
	if got, err := d.IdentifyARL(context.Background(), arl); err != nil || got != "685" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := d.IdentifyARL(context.Background(), "x y"); err == nil || strings.Contains(err.Error(), "x y") {
		t.Fatal(err)
	}
	loggedOut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":{"USER":{"USER_ID":0}}}`)
	}))
	defer loggedOut.Close()
	d.GatewayURL = loggedOut.URL
	if _, err := d.IdentifyARL(context.Background(), arl); err == nil {
		t.Fatal("logged-out arl accepted")
	}
}

func TestCamelot(t *testing.T) {
	for _, c := range []struct {
		pitch, mode int
		want        string
	}{{9, 0, "8A"}, {0, 1, "8B"}, {1, 1, "3B"}, {7, 1, "9B"}, {11, 0, "10A"}, {12, 0, ""}} {
		if got := Camelot(c.pitch, c.mode); got != c.want {
			t.Fatalf("%+v: %s", c, got)
		}
	}
	for name, want := range map[string]string{"Am": "8A", "C": "8B", "C#": "3B", "Bb minor": "3A", "F:min": "4A", "C:maj": "8B", "x": ""} {
		if got := camelotFromName(name); got != want {
			t.Fatalf("%s: %s", name, got)
		}
	}
}

func TestBPMBatchFillsTracksAndCaches(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/audio-features" || r.URL.Query().Get("ids") != "US1,sp1" {
			t.Errorf("unexpected %s", r.URL)
		}
		io.WriteString(w, `{"content":[{"href":"https://open.spotify.com/track/xx","isrc":"US1","tempo":135.128,"key":7,"mode":1},{"href":"https://open.spotify.com/track/sp1","isrc":"ZZ","tempo":90,"key":9,"mode":0}]}`)
	}))
	defer srv.Close()
	path := t.TempDir() + "/bpm.json"
	b := &BPMStore{Path: path, ReccoURL: srv.URL}
	tracks := []Track{{ID: "1", Provider: "deezer", ISRC: "US-1"}, {ID: "sp1", Provider: "spotify"}}
	b.batch(context.Background(), tracks)
	b.Apply(tracks)
	if tracks[0].BPM != 135.1 || tracks[0].Key != "9B" || tracks[1].BPM != 90 || tracks[1].Key != "8A" {
		t.Fatalf("%+v", tracks)
	}
	again := &BPMStore{Path: path, ReccoURL: srv.URL}
	fresh := []Track{{ID: "1", Provider: "deezer"}}
	again.Enrich(context.Background(), fresh)
	if fresh[0].BPM != 135.1 || calls != 1 {
		t.Fatalf("%+v %d", fresh, calls)
	}
}

func TestBPMRateLimitPausesLookups(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "slow down", 429)
	}))
	defer srv.Close()
	b := &BPMStore{Path: t.TempDir() + "/bpm.json", ReccoURL: srv.URL}
	b.batch(context.Background(), []Track{{ID: "1", Provider: "deezer", ISRC: "US1"}})
	b.batch(context.Background(), []Track{{ID: "2", Provider: "deezer", ISRC: "US2"}})
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestBPMPerTrackLookupNeedsTheSameSong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/track/search":
			if got := r.URL.Query().Get("searchText"); got != "Song" {
				t.Errorf("searchText %q", got)
			}
			io.WriteString(w, `{"content":[{"id":"wrong","trackTitle":"Song Cover","artists":[{"name":"Artist"}]},{"id":"version","trackTitle":"Song, club mix","artists":[{"name":"Artist"}]},{"id":"right","trackTitle":"Song (Original Mix)","artists":[{"name":"Artist"}]}]}`)
		case "/v1/track/right/audio-features", "/v1/track/version/audio-features":
			io.WriteString(w, `{"tempo":128,"key":0,"mode":1}`)
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()
	b := &BPMStore{ReccoURL: srv.URL}
	if got := b.reccoSearch(context.Background(), Track{Title: "Song", Artist: "Artist"}); got.BPM != 128 || got.Key != "8B" {
		t.Fatalf("%+v", got)
	}
	if got := b.reccoSearch(context.Background(), Track{Title: "Song", Artist: "Other"}); got.BPM != 0 {
		t.Fatalf("%+v", got)
	}
	// With no exact title, another version of the same song is close enough.
	if got := b.reccoSearch(context.Background(), Track{Title: "Song [Radio Edit]", Artist: "Artist"}); got.BPM != 128 {
		t.Fatalf("%+v", got)
	}
}

func TestGetSongAndSoundCloudReadTheirKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/":
			if r.Header.Get("X-API-KEY") != "gsb" {
				t.Error("missing key")
			}
			io.WriteString(w, `{"search":[{"title":"Song","tempo":"120","artist":{"name":"Artist"}}]}`)
		case "/tracks":
			if r.Header.Get("Authorization") != "OAuth tok" {
				t.Error("missing token")
			}
			io.WriteString(w, `{"collection":[{"id":1,"title":"Artist - Song","bpm":126,"key_signature":"Am","user":{"username":"Artist"}}]}`)
		}
	}))
	defer srv.Close()
	b := &BPMStore{GetSongURL: srv.URL, SoundCloudURL: srv.URL}
	track := Track{Title: "Song", Artist: "Artist"}
	if got := b.getSong(context.Background(), track, "gsb"); got.BPM != 120 {
		t.Fatalf("%+v", got)
	}
	if got := b.soundCloud(context.Background(), track, BPMConfig{SoundCloudClientID: "cid", SoundCloudSecret: "shh", SoundCloudToken: "tok"}); got.BPM != 126 || got.Key != "8A" {
		t.Fatalf("%+v", got)
	}
}

func TestBPMFindsNeteaseTracksThroughOtherCatalogs(t *testing.T) {
	var featureIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			switch r.URL.Query().Get("type") {
			case "track": // Spotify
				q := r.URL.Query().Get("q")
				if strings.Contains(q, "Nowhere") {
					io.WriteString(w, `{"tracks":{"items":[]}}`)
					return
				}
				io.WriteString(w, `{"tracks":{"items":[{"id":"wrong","name":"Other Song","type":"track","artists":[{"name":"X"}]},{"id":"sp9","name":"走到底","type":"track","artists":[{"name":"A-Yue Chang"}],"external_ids":{"isrc":"TW1"}}]}}`)
			default: // Deezer
				io.WriteString(w, `{"data":[{"id":5,"title":"Deep Cut","duration":200,"artist":{"name":"Nowhere Man"}}]}`)
			}
		case "/track/5":
			io.WriteString(w, `{"id":5,"title":"Deep Cut","duration":200,"bpm":121,"isrc":"","artist":{"name":"Nowhere Man"}}`)
		case "/v1/audio-features":
			featureIDs = append(featureIDs, r.URL.Query().Get("ids"))
			io.WriteString(w, `{"content":[{"tempo":95,"key":0,"mode":1}]}`)
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()
	sp := &Spotify{BaseURL: srv.URL, Token: Token{AccessToken: "a"}}
	b := &BPMStore{ReccoURL: srv.URL, DeezerURL: srv.URL, Spotify: func() *Spotify { return sp }}

	// A Chinese name is spelled differently on Spotify, so the title decides.
	got := b.lookup(context.Background(), Track{ID: "1", Provider: "netease", Title: "走到底", Artist: "张震岳"})
	if got.BPM != 95 || len(featureIDs) != 1 || featureIDs[0] != "sp9,TW1" {
		t.Fatalf("%+v %q", got, featureIDs)
	}
	// Spotify has nothing, so Deezer's own tempo answers.
	got = b.lookup(context.Background(), Track{ID: "2", Provider: "netease", Title: "Deep Cut", Artist: "Nowhere Man"})
	if got.BPM != 121 {
		t.Fatalf("%+v", got)
	}
}

func TestBestMatchKeepsLatinArtistsStrict(t *testing.T) {
	found := []Track{{ID: "x", Title: "Song", Artists: []string{"Somebody Else"}}}
	if _, ok := bestMatch(Track{Title: "Song", Artist: "Artist"}, found); ok {
		t.Fatal("same title by another Latin-named artist accepted")
	}
	if c, ok := bestMatch(Track{Title: "Song", Artist: "歌手"}, found); !ok || c.ID != "x" {
		t.Fatal("Chinese artist name should fall back to the title")
	}
}

func TestPreviewIsTheLastResortAndOnlyFromDeezer(t *testing.T) {
	var clips []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/track/isrc:GX38U2629409":
			io.WriteString(w, `{"id":1,"title":"Transmissions","duration":300,"preview":"https://cdnt-preview.dzcdn.net/clip.mp3","artist":{"name":"Modpsy"}}`)
		case "/track/isrc:EVIL":
			io.WriteString(w, `{"id":2,"title":"X","duration":300,"preview":"https://evil.example/clip.mp3","artist":{"name":"Y"}}`)
		default:
			io.WriteString(w, `{"content":[],"data":[]}`)
		}
	}))
	defer srv.Close()
	b := &BPMStore{Path: t.TempDir() + "/bpm.json", ReccoURL: srv.URL, DeezerURL: srv.URL}
	done := make(chan struct{}, 2)
	b.PreviewBPM = func(ctx context.Context, address string) BPMInfo {
		clips = append(clips, address)
		done <- struct{}{}
		return BPMInfo{BPM: 133.4, Key: "4A"}
	}
	track := Track{ID: "sp", Provider: "spotify", ISRC: "GX38U2629409", Title: "Transmissions", Artist: "Modpsy"}
	if !b.claim(track) {
		t.Fatal("claim")
	}
	b.attempt(context.Background(), track)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("preview never analysed")
	}
	if len(clips) != 1 || clips[0] != "https://cdnt-preview.dzcdn.net/clip.mp3" {
		t.Fatal(clips)
	}
	var got []Track
	for range 100 {
		got = []Track{track}
		b.Apply(got)
		if got[0].BPM != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got[0].BPM != 133.4 || got[0].Key != "4A" {
		t.Fatalf("%+v", got[0])
	}
	// An estimate cannot replace what the deck measured from the whole file.
	b.Put(track, BPMInfo{BPM: 130, Measured: true})
	b.Put(track, BPMInfo{BPM: 133.4})
	got = []Track{track}
	b.Apply(got)
	if got[0].BPM != 130 {
		t.Fatalf("%+v", got[0])
	}
	if address := b.previewFor(context.Background(), Track{ID: "x", Provider: "spotify", ISRC: "EVIL", Title: "X", Artist: "Y"}); address != "" {
		t.Fatalf("foreign clip host accepted: %s", address)
	}
}

func TestNeteaseSkipsLosslessWhenMP3IsWanted(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []any{map[string]any{"url": "https://cdn/song"}}})
	}))
	defer srv.Close()
	n := &Netease{WebURL: srv.URL, Cookie: "c", UserID: 1}
	if _, err := n.AudioURL(context.Background(), "7", false); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}

func soundCloudServer(t *testing.T, streams string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token", "/list.m3u8":
		default:
			if r.Header.Get("Authorization") != "OAuth app-token" {
				t.Errorf("no credentials on %s", r.URL)
			}
		}
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if id, secret, ok := r.BasicAuth(); !ok || id != "cid" || secret != "shh" || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_secret") != "" {
				t.Errorf("token request %v", r.Form)
			}
			io.WriteString(w, `{"access_token":"app-token","expires_in":3600}`)
		case "/resolve":
			io.WriteString(w, `{"kind":"user","id":7,"username":"cave"}`)
		case "/users":
			io.WriteString(w, `{"collection":[{"id":7,"username":"cave"}]}`)
		case "/users/7/playlists":
			if r.URL.Query().Get("offset") == "" {
				io.WriteString(w, `{"collection":[{"id":1,"title":"Sets","track_count":2}],"next_href":"`+srv.URL+`/users/7/playlists?offset=1"}`)
			} else {
				io.WriteString(w, `{"collection":[{"id":2,"title":"More","track_count":1}],"next_href":"https://evil.example/next"}`)
			}
		case "/users/7/likes/playlists":
			io.WriteString(w, `{"collection":[{"id":3,"title":"Liked set","track_count":4}]}`)
		case "/users/7/likes/tracks":
			io.WriteString(w, `{"collection":[{"id":11,"title":"Liked","duration":7200000,"user":{"username":"DJ"}}]}`)
		case "/playlists/1/tracks":
			io.WriteString(w, `{"collection":[{"id":21,"title":"Full","duration":1000,"bpm":128,"key_signature":"Am","isrc":"US1","user":{"username":"A"}},{"id":22,"access":"blocked"}]}`)
		case "/tracks":
			io.WriteString(w, `{"collection":[{"id":31,"title":"Found set","duration":5400000,"user":{"username":"C"}}]}`)
		case "/playlists":
			io.WriteString(w, `{"collection":[{"id":41,"title":"Found list","track_count":3}]}`)
		case "/tracks/99":
			io.WriteString(w, `{"id":99,"access":"playable"}`)
		case "/tracks/98":
			io.WriteString(w, `{"id":98,"access":"preview"}`)
		case "/tracks/99/streams":
			io.WriteString(w, strings.ReplaceAll(streams, "BASE", srv.URL))
		case "/stream/mp3":
			http.Redirect(w, r, "https://cdn.example/audio.mp3?sig=1", http.StatusFound)
		case "/stream/hls":
			http.Redirect(w, r, srv.URL+"/list.m3u8", http.StatusFound)
		case "/list.m3u8":
			if r.Header.Get("Authorization") != "" {
				t.Error("credentials sent to the CDN")
			}
			io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:10,\nseg0.ts\n#EXTINF:10,\n"+srv.URL+"/seg1.ts\n#EXT-X-ENDLIST\n")
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testSoundCloud(t *testing.T, srv *httptest.Server) *SoundCloud {
	return &SoundCloud{BaseURL: srv.URL, ClientID: "cid", Secret: "shh", Auth: &SoundCloudAuth{TokenURL: srv.URL + "/token", Path: t.TempDir() + "/token.json"}}
}

func TestSoundCloudBrowsesAProfile(t *testing.T) {
	srv := soundCloudServer(t, "")
	s := testSoundCloud(t, srv)
	if err := s.ResolveUser(context.Background(), "https://soundcloud.com/cave"); err != nil || s.UserID != 7 {
		t.Fatal(err, s.UserID)
	}
	if err := s.ResolveUser(context.Background(), "https://evil.example/cave"); err == nil {
		t.Fatal("foreign profile link accepted")
	}
	byName := testSoundCloud(t, srv)
	if err := byName.ResolveUser(context.Background(), "cave"); err != nil || byName.UserID != 7 {
		t.Fatal(err, byName.UserID)
	}
	lists, err := s.Playlists(context.Background())
	if err != nil || len(lists) != 4 || lists[0].ID != "likes" || lists[1].Title != "Sets" || lists[2].Title != "More" || lists[3].Title != "【收藏】Liked set" {
		t.Fatalf("%+v %v", lists, err)
	}
	likes, err := s.Tracks(context.Background(), "likes")
	if err != nil || len(likes) != 1 || likes[0].DurationMS != 7200000 || likes[0].Provider != "soundcloud" {
		t.Fatalf("%+v %v", likes, err)
	}
	tracks, err := s.Tracks(context.Background(), "1")
	if err != nil || len(tracks) != 1 || tracks[0].BPM != 128 || tracks[0].Key != "8A" || tracks[0].ISRC != "US1" {
		t.Fatalf("%+v %v", tracks, err)
	}
	found, err := s.Search(context.Background(), "set")
	if err != nil || found[0].Title != "Found set" {
		t.Fatalf("%+v %v", found, err)
	}
	list, err := s.SearchPlaylists(context.Background(), "set")
	if err != nil || list[0].Count != 3 {
		t.Fatalf("%+v %v", list, err)
	}
}

func TestSoundCloudTokenIsReusedAcrossRestarts(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"access_token":"a","expires_in":3600}`)
	}))
	defer srv.Close()
	path := t.TempDir() + "/token.json"
	for range 2 {
		a := &SoundCloudAuth{TokenURL: srv.URL, Path: path}
		if got, err := a.Token(context.Background(), "cid", "shh"); err != nil || got != "a" {
			t.Fatal(got, err)
		}
	}
	if calls != 1 {
		t.Fatalf("%d token requests", calls)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "shh") {
		t.Fatal("secret written to the token file")
	}
}

func TestSoundCloudMediaPrefersAACAndJoinsHLS(t *testing.T) {
	both := soundCloudServer(t, `{"http_mp3_128_url":"BASE/stream/mp3","hls_aac_160_url":"BASE/stream/hls","preview_mp3_128_url":"BASE/preview"}`)
	m, err := testSoundCloud(t, both).Media(context.Background(), "99", false)
	if err != nil || m.Ext != "mp4" || len(m.URLs) != 3 {
		t.Fatalf("%+v %v", m, err)
	}
	mp3 := soundCloudServer(t, `{"http_mp3_128_url":"BASE/stream/mp3"}`)
	m, err = testSoundCloud(t, mp3).Media(context.Background(), "99", false)
	if err != nil || m.Ext != "mp3" || len(m.URLs) != 1 || m.URLs[0] != "https://cdn.example/audio.mp3?sig=1" {
		t.Fatalf("%+v %v", m, err)
	}
	hls := soundCloudServer(t, `{"hls_aac_160_url":"BASE/stream/hls"}`)
	m, err = testSoundCloud(t, hls).Media(context.Background(), "99", false)
	if err != nil || m.Ext != "mp4" || len(m.URLs) != 3 || !strings.HasSuffix(m.URLs[0], "/init.mp4") || !strings.HasSuffix(m.URLs[2], "/seg1.ts") {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestSoundCloudRefusesPreviews(t *testing.T) {
	onlyPreview := soundCloudServer(t, `{"preview_mp3_128_url":"BASE/preview"}`)
	if _, err := testSoundCloud(t, onlyPreview).Media(context.Background(), "99", false); !errors.Is(err, ErrNoFullStream) {
		t.Fatal(err)
	}
	if _, err := testSoundCloud(t, onlyPreview).Media(context.Background(), "98", false); !errors.Is(err, ErrNoFullStream) {
		t.Fatal(err)
	}
	keyed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"k\"\nseg.ts\n")
	}))
	defer keyed.Close()
	if _, err := (&SoundCloud{}).segments(context.Background(), keyed.URL); !errors.Is(err, ErrNoFullStream) {
		t.Fatal(err)
	}
}

func TestMeasuredTempoCannotBeOverwrittenByACatalog(t *testing.T) {
	b := &BPMStore{Path: t.TempDir() + "/bpm.json"}
	track := Track{ID: "1", Provider: "netease"}
	read := func() BPMInfo {
		list := []Track{track}
		b.Apply(list)
		return BPMInfo{BPM: list[0].BPM, Key: list[0].Key}
	}
	b.Put(track, BPMInfo{BPM: 140, Key: "1A"})
	b.Put(track, BPMInfo{BPM: 141.2, Key: "2A", Measured: true})
	b.Put(track, BPMInfo{BPM: 99, Key: "3A"})
	if got := read(); got.BPM != 141.2 || got.Key != "2A" {
		t.Fatalf("%+v", got)
	}
}

func TestNeteaseSavedAlbumsAndRadios(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/weapi/user/playlist":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "playlist": []any{map[string]any{"id": 1, "name": "Mine", "trackCount": 2}}})
		case "/weapi/album/sublist":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []any{map[string]any{"id": 5, "name": "LP", "size": 9, "artists": []any{map[string]any{"name": "A"}}}}})
		case "/weapi/djradio/get/subed":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "djRadios": []any{map[string]any{"id": 6, "name": "Show", "programCount": 3}}})
		case "/weapi/v1/album/5":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "songs": []any{map[string]any{"id": 50, "name": "S", "dt": 1000, "ar": []any{map[string]any{"name": "A"}}}}})
		case "/weapi/dj/program/byradio":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "programs": []any{
				map[string]any{"name": "Episode", "duration": 2000, "dj": map[string]any{"nickname": "Host"}, "mainSong": map[string]any{"id": 60, "name": "x", "duration": 3000, "artists": []any{map[string]any{"name": "B"}}, "album": map[string]any{"name": "Show"}}},
				map[string]any{"name": "Empty", "mainSong": map[string]any{"id": 0}},
			}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	n := &Netease{WebURL: srv.URL, Cookie: "MUSIC_U=x", UserID: 1}
	lists, err := n.Playlists(context.Background())
	if err != nil || len(lists) != 3 || lists[0].Title != "Mine" || lists[1] != (Playlist{"album:5", "【专辑】LP", 9}) || lists[2] != (Playlist{"dj:6", "【电台】Show", 3}) {
		t.Fatalf("%+v %v", lists, err)
	}
	album, err := n.Tracks(context.Background(), "album:5")
	if err != nil || len(album) != 1 || album[0].ID != "50" || album[0].Provider != "netease" {
		t.Fatalf("%+v %v", album, err)
	}
	radio, err := n.Tracks(context.Background(), "dj:6")
	if err != nil || len(radio) != 1 || radio[0].ID != "60" || radio[0].Title != "Episode" || radio[0].Artist != "Host" || radio[0].DurationMS != 3000 || radio[0].Album != "Show" {
		t.Fatalf("%+v %v", radio, err)
	}
	if _, err := n.Tracks(context.Background(), "album:1/../2"); err == nil {
		t.Fatal("bad album ID accepted")
	}
}

func TestNeteasePlaylistsSurviveSavedListFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/weapi/user/playlist" {
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "playlist": []any{map[string]any{"id": 1, "name": "Mine"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 301})
	}))
	defer srv.Close()
	lists, err := (&Netease{WebURL: srv.URL, Cookie: "c", UserID: 1}).Playlists(context.Background())
	if err != nil || len(lists) != 1 {
		t.Fatalf("%+v %v", lists, err)
	}
}

func TestEnrichNeverWaitsForPerTrackLookups(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/track/search" {
			<-release
			io.WriteString(w, `{"content":[{"id":"x","trackTitle":"S","artists":[{"name":"A"}]}]}`)
			return
		}
		io.WriteString(w, `{"tempo":128,"key":0,"mode":1,"data":[]}`)
	}))
	defer srv.Close()
	defer close(release)
	b := &BPMStore{Path: t.TempDir() + "/bpm.json", ReccoURL: srv.URL, DeezerURL: srv.URL}
	track := Track{ID: "1", Provider: "netease", Title: "S", Artist: "A"}
	done := make(chan struct{})
	go func() {
		b.Enrich(context.Background(), []Track{track})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a list waited for a lookup")
	}
	// The worker finishes it in the background for the next listing.
	release <- struct{}{}
	for range 300 {
		list := []Track{track}
		b.Apply(list)
		if list[0].BPM == 128 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background lookup never finished")
}

func TestCutShortLookupStaysEligible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"content":[]}`)
	}))
	defer srv.Close()
	b := &BPMStore{Path: t.TempDir() + "/bpm.json", ReccoURL: srv.URL}
	track := Track{ID: "1", Provider: "netease", Title: "S", Artist: "A"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.claim(track)
	b.attempt(ctx, track)
	if !b.claim(track) {
		t.Fatal("cut-short lookup was recorded as a miss")
	}
	b.attempt(context.Background(), track)
	if b.claim(track) {
		t.Fatal("a real miss was forgotten")
	}
}

func TestSoundCloudTokenReachesPrivateLists(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "OAuth mine" {
			t.Errorf("token missing on %s", r.URL)
		}
		switch r.URL.Path {
		case "/me":
			io.WriteString(w, `{"id":8,"username":"me"}`)
		default:
			io.WriteString(w, `{"collection":[]}`)
		}
	}))
	defer srv.Close()
	s := &SoundCloud{BaseURL: srv.URL, ClientID: "cid", Secret: "shh", Token: "mine"}
	if err := s.Identify(context.Background()); err != nil || s.UserID != 8 {
		t.Fatal(err, s.UserID)
	}
	if _, err := s.Playlists(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tracks(context.Background(), "likes"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/me", "/me/playlists", "/me/likes/playlists", "/me/likes/tracks"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("%v", paths)
	}
}

func TestSpotifyLikedSongs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/tracks":
			if r.URL.Query().Get("limit") == "1" {
				io.WriteString(w, `{"total":2,"items":[]}`)
				return
			}
			io.WriteString(w, `{"items":[{"track":{"id":"a","name":"One","type":"track"}},{"item":{"id":"b","name":"Two","type":"track"}}],"next":null}`)
		case "/me/playlists":
			io.WriteString(w, `{"items":[{"id":"p","name":"Mine"}],"next":null}`)
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()
	s := &Spotify{BaseURL: srv.URL, Token: Token{AccessToken: "a"}}
	lists, err := s.Playlists(context.Background())
	if err != nil || len(lists) != 2 || lists[0].ID != "liked" || lists[0].Count != 2 || lists[1].ID != "p" {
		t.Fatalf("%+v %v", lists, err)
	}
	tracks, err := s.Tracks(context.Background(), "liked")
	if err != nil || len(tracks) != 2 || tracks[1].ID != "b" {
		t.Fatalf("%+v %v", tracks, err)
	}
}

func TestSoundCloudAccountLoginRenewsAndReachesPrivateLists(t *testing.T) {
	var refreshes int
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			r.ParseForm()
			// SoundCloud answers 400 to a refresh with a Basic header; the app's keys go in the form.
			if _, _, basic := r.BasicAuth(); basic || r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "shh" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "r1" {
				t.Errorf("token request %v", r.Form)
			}
			refreshes++
			io.WriteString(w, `{"access_token":"u2","refresh_token":"r2","expires_in":3600}`)
			return
		}
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "OAuth u2" {
			t.Errorf("wrong token on %s: %s", r.URL, r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"collection":[]}`)
	}))
	defer srv.Close()
	path := t.TempDir() + "/token.json"
	auth := &SoundCloudAuth{TokenURL: srv.URL + "/token", Path: path}
	if err := auth.SetUser("u1", "r1", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s := &SoundCloud{BaseURL: srv.URL, ClientID: "cid", Secret: "shh", Auth: auth, UserID: 8}
	if _, err := s.Playlists(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tracks(context.Background(), "likes"); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 || !strings.HasPrefix(paths[0], "/me/") {
		t.Fatalf("%d refreshes, paths %v", refreshes, paths)
	}
	// The rotated refresh token survives a restart.
	again := &SoundCloudAuth{Path: path}
	if !again.HasUser() {
		t.Fatal("login lost on restart")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "r2") || strings.Contains(string(b), "shh") {
		t.Fatalf("%s", b)
	}
	// An app token request must not wipe the login.
	if err := auth.SetUser("", "", time.Time{}); err != nil || auth.HasUser() {
		t.Fatal(err)
	}
}

func TestSpotifyLookupsArePacedAndPauseOnRateLimit(t *testing.T) {
	var calls []time.Time
	limited := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, time.Now())
		if limited {
			http.Error(w, "slow", 429)
			return
		}
		io.WriteString(w, `{"tracks":{"items":[]}}`)
	}))
	defer srv.Close()
	sp := &Spotify{BaseURL: srv.URL, Token: Token{AccessToken: "a"}}
	b := &BPMStore{Spotify: func() *Spotify { return sp }}
	track := Track{Title: "S", Artist: "A"}
	b.spotifySearch(context.Background(), track)
	b.spotifySearch(context.Background(), track)
	if len(calls) != 2 || calls[1].Sub(calls[0]) < spotifyLookupGap-100*time.Millisecond {
		t.Fatalf("%v", calls)
	}
	limited = true
	b.spotifySearch(context.Background(), track)
	n := len(calls)
	b.spotifySearch(context.Background(), track)
	if len(calls) != n {
		t.Fatal("kept calling Spotify after a 429")
	}
}

func TestSoundCloudLosslessTakesTheOriginalOnlyWhenAsked(t *testing.T) {
	var srv *httptest.Server
	downloads := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			io.WriteString(w, `{"access_token":"app-token","expires_in":3600}`)
		case "/tracks/97":
			// The reply's address is ignored; the login only goes to the API.
			io.WriteString(w, `{"id":97,"access":"playable","downloadable":true,"download_url":"https://evil.example/steal"}`)
		case "/tracks/96":
			io.WriteString(w, `{"id":96,"access":"playable","downloadable":false}`)
		case "/tracks/97/download":
			downloads++
			if r.Header.Get("Authorization") != "OAuth app-token" {
				t.Error("no credentials on the download")
			}
			http.Redirect(w, r, "https://cdn.example/original.wav?sig=1", http.StatusFound)
		case "/tracks/97/streams", "/tracks/96/streams":
			io.WriteString(w, `{"http_mp3_128_url":"`+srv.URL+`/stream/mp3"}`)
		case "/stream/mp3":
			http.Redirect(w, r, "https://cdn.example/audio.mp3?sig=1", http.StatusFound)
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()
	s := testSoundCloud(t, srv)
	m, err := s.Media(context.Background(), "97", true)
	if err != nil || m.Original != "https://cdn.example/original.wav?sig=1" || m.Ext != "mp3" || len(m.URLs) != 1 {
		t.Fatalf("%+v %v", m, err)
	}
	if m, err = s.Media(context.Background(), "97", false); err != nil || m.Original != "" || downloads != 1 {
		t.Fatalf("320kbps asked for the original: %+v %v", m, err)
	}
	if m, err = s.Media(context.Background(), "96", true); err != nil || m.Original != "" || len(m.URLs) != 1 {
		t.Fatalf("%+v %v", m, err)
	}
}
