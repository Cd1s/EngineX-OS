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
	"strings"
	"testing"
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
	got, err := n.AudioURL(context.Background(), "7")
	if err != nil || got != "https://cdn/full" || len(levels) != 3 {
		t.Fatalf("%q %v %d", got, err, len(levels))
	}
	if _, err := n.AudioURL(context.Background(), "1,2"); err == nil {
		t.Fatal("bad ID accepted")
	}
}

func TestNeteaseAudioReportsPreviewOnlyTracks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []any{map[string]any{"url": "https://cdn/trial", "freeTrialInfo": map[string]any{"end": 30}}}})
	}))
	defer srv.Close()
	n := &Netease{WebURL: srv.URL, Cookie: "MUSIC_U=x", UserID: 1}
	if _, err := n.AudioURL(context.Background(), "7"); !errors.Is(err, ErrNoFullAudio) {
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
	for name, want := range map[string]string{"Am": "8A", "C": "8B", "C#": "3B", "Bb minor": "3A", "x": ""} {
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
			io.WriteString(w, `{"content":[{"id":"wrong","trackTitle":"Song Cover","artists":[{"name":"Artist"}]},{"id":"right","trackTitle":"Song (Original Mix)","artists":[{"name":"Artist"}]}]}`)
		case "/v1/track/right/audio-features":
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
			io.WriteString(w, `[{"title":"Song","bpm":126,"key_signature":"Am","user":{"username":"Artist"}}]`)
		}
	}))
	defer srv.Close()
	b := &BPMStore{GetSongURL: srv.URL, SoundCloudURL: srv.URL}
	track := Track{Title: "Song", Artist: "Artist"}
	if got := b.getSong(context.Background(), track, "gsb"); got.BPM != 120 {
		t.Fatalf("%+v", got)
	}
	if got := b.soundCloud(context.Background(), track, BPMConfig{SoundCloudToken: "tok"}); got.BPM != 126 || got.Key != "8A" {
		t.Fatalf("%+v", got)
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
