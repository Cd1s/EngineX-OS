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
	"strconv"
	"strings"
	"sync/atomic"
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

func TestSoundCloudV2UsesTheClientIDAndISRC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/tracks":
			if r.URL.Query().Get("client_id") != "cid" {
				t.Error("missing client ID")
			}
			io.WriteString(w, `{"collection":[{"title":"Artist - Song","user":{"username":"Some Label"},"publisher_metadata":{"artist":"Artist","isrc":"US1"}}]}`)
		case "/v1/audio-features":
			if r.URL.Query().Get("ids") != "US1" {
				t.Errorf("ids %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"content":[{"tempo":130,"key":0,"mode":1}]}`)
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()
	b := &BPMStore{SoundCloudV2URL: srv.URL, ReccoURL: srv.URL}
	got := b.soundCloud(context.Background(), Track{Title: "Song", Artist: "Artist"}, BPMConfig{SoundCloudClientID: "cid"})
	if got.BPM != 130 || got.Key != "8B" {
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

func TestEnrichWaitsForTracksWithoutAnID(t *testing.T) {
	var searches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/track/search":
			searches.Add(1)
			io.WriteString(w, `{"content":[{"id":"x","trackTitle":"`+r.URL.Query().Get("searchText")+`","artists":[{"name":"Artist"}]}]}`)
		default:
			io.WriteString(w, `{"tempo":128,"key":0,"mode":1}`)
		}
	}))
	defer srv.Close()
	b := &BPMStore{Path: t.TempDir() + "/bpm.json", ReccoURL: srv.URL}
	tracks := make([]Track, 30)
	for i := range tracks {
		tracks[i] = Track{ID: strconv.Itoa(i), Provider: "netease", Title: "S" + strconv.Itoa(i), Artist: "Artist"}
	}
	b.Enrich(context.Background(), tracks)
	for i := range bpmListLookups {
		if tracks[i].BPM != 128 {
			t.Fatalf("track %d not filled: %+v", i, tracks[i])
		}
	}
}

func TestSearchPreviewDoesNotWaitForPerTrackLookups(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.WriteString(w, `{"content":[]}`)
	}))
	defer srv.Close()
	defer close(release)
	b := &BPMStore{Path: t.TempDir() + "/bpm.json", ReccoURL: srv.URL}
	done := make(chan struct{})
	go func() {
		b.EnrichPreview(context.Background(), []Track{{ID: "1", Provider: "netease", Title: "S", Artist: "A"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("search waited for a lookup")
	}
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

func soundCloudServer(t *testing.T, transcodings string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/seg0.ts" && r.URL.Path != "/list.m3u8" && r.Header.Get("Authorization") == "" && r.URL.Query().Get("client_id") == "" {
			t.Errorf("no credentials on %s", r.URL)
		}
		switch r.URL.Path {
		case "/resolve":
			io.WriteString(w, `{"kind":"user","id":7,"username":"cave"}`)
		case "/me":
			io.WriteString(w, `{"id":8,"username":"me"}`)
		case "/users/7/playlists":
			if r.URL.Query().Get("offset") == "" {
				io.WriteString(w, `{"collection":[{"id":1,"title":"Sets","track_count":2}],"next_href":"`+srv.URL+`/users/7/playlists?offset=1&client_id=leak"}`)
			} else {
				io.WriteString(w, `{"collection":[{"id":2,"title":"More","track_count":1}],"next_href":"https://evil.example/next"}`)
			}
		case "/users/7/track_likes":
			io.WriteString(w, `{"collection":[{"track":{"id":11,"title":"Liked","duration":7200000,"user":{"username":"DJ"}}}]}`)
		case "/playlists/1":
			io.WriteString(w, `{"tracks":[{"id":21,"title":"Full","duration":1000,"user":{"username":"A"},"publisher_metadata":{"artist":"Real","isrc":"US1"}},{"id":22}]}`)
		case "/tracks":
			if r.URL.Query().Get("ids") != "22" {
				t.Errorf("ids %s", r.URL.RawQuery)
			}
			io.WriteString(w, `[{"id":22,"title":"Second","duration":2000,"user":{"username":"B"}}]`)
		case "/search/tracks":
			io.WriteString(w, `{"collection":[{"id":31,"title":"Found set","duration":5400000,"user":{"username":"C"}}]}`)
		case "/search/playlists":
			io.WriteString(w, `{"collection":[{"id":41,"title":"Found list","track_count":3}]}`)
		case "/tracks/99":
			io.WriteString(w, `{"id":99,"track_authorization":"auth","media":{"transcodings":[`+strings.ReplaceAll(transcodings, "BASE", srv.URL)+`]}}`)
		case "/media/hls", "/media/prog":
			if r.URL.Query().Get("track_authorization") != "auth" {
				t.Error("track authorization missing")
			}
			io.WriteString(w, `{"url":"`+srv.URL+map[string]string{"/media/hls": "/list.m3u8", "/media/prog": "/audio.mp3"}[r.URL.Path]+`"}`)
		case "/list.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:10,\nseg0.ts\n#EXTINF:10,\n"+srv.URL+"/seg1.ts\n#EXT-X-ENDLIST\n")
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSoundCloudBrowsesAProfile(t *testing.T) {
	srv := soundCloudServer(t, "")
	s := &SoundCloud{BaseURL: srv.URL, ClientID: "cid"}
	if err := s.ResolveUser(context.Background(), "https://soundcloud.com/cave"); err != nil || s.UserID != 7 {
		t.Fatal(err, s.UserID)
	}
	if err := s.ResolveUser(context.Background(), "https://evil.example/cave"); err == nil {
		t.Fatal("foreign profile link accepted")
	}
	lists, err := s.Playlists(context.Background())
	if err != nil || len(lists) != 3 || lists[0].ID != "likes" || lists[1].Title != "Sets" || lists[2].Title != "More" {
		t.Fatalf("%+v %v", lists, err)
	}
	likes, err := s.Tracks(context.Background(), "likes")
	if err != nil || len(likes) != 1 || likes[0].DurationMS != 7200000 || likes[0].Provider != "soundcloud" {
		t.Fatalf("%+v %v", likes, err)
	}
	tracks, err := s.Tracks(context.Background(), "1")
	if err != nil || len(tracks) != 2 || tracks[0].Artist != "Real" || tracks[0].ISRC != "US1" || tracks[1].Title != "Second" {
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
	tokenOnly := &SoundCloud{BaseURL: srv.URL, Token: "tok"}
	if err := tokenOnly.Identify(context.Background()); err != nil || tokenOnly.UserID != 8 {
		t.Fatal(err, tokenOnly.UserID)
	}
}

func TestSoundCloudMediaPrefersProgressiveMP3AndJoinsHLS(t *testing.T) {
	prog := `{"url":"BASE/media/prog","format":{"protocol":"progressive","mime_type":"audio/mpeg"}}`
	hlsAAC := `{"url":"BASE/media/hls","format":{"protocol":"hls","mime_type":"audio/mp4; codecs=\"mp4a.40.2\""}}`
	both := soundCloudServer(t, hlsAAC+","+prog)
	m, err := (&SoundCloud{BaseURL: both.URL, ClientID: "cid"}).Media(context.Background(), "99")
	if err != nil || m.Ext != "mp3" || len(m.URLs) != 1 || !strings.HasSuffix(m.URLs[0], "/audio.mp3") {
		t.Fatalf("%+v %v", m, err)
	}
	hls := soundCloudServer(t, hlsAAC)
	m, err = (&SoundCloud{BaseURL: hls.URL, ClientID: "cid"}).Media(context.Background(), "99")
	if err != nil || m.Ext != "mp4" || len(m.URLs) != 3 || !strings.HasSuffix(m.URLs[0], "/init.mp4") || !strings.HasSuffix(m.URLs[1], "/seg0.ts") || !strings.HasSuffix(m.URLs[2], "/seg1.ts") {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestSoundCloudRefusesPreviewsAndEncryptedStreams(t *testing.T) {
	for _, list := range []string{
		`{"url":"BASE/media/prog","snipped":true,"format":{"protocol":"progressive","mime_type":"audio/mpeg"}}`,
		`{"url":"BASE/media/hls","format":{"protocol":"ctr-encrypted-hls","mime_type":"audio/mp4"}}`,
		`{"url":"BASE/media/hls","format":{"protocol":"hls","mime_type":"audio/ogg; codecs=\"opus\""}}`,
	} {
		srv := soundCloudServer(t, list)
		if _, err := (&SoundCloud{BaseURL: srv.URL, ClientID: "c"}).Media(context.Background(), "99"); !errors.Is(err, ErrNoFullStream) {
			t.Fatalf("%s: %v", list, err)
		}
	}
	keyed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"k\"\nseg.ts\n")
	}))
	defer keyed.Close()
	if _, err := (&SoundCloud{}).segments(context.Background(), keyed.URL); !errors.Is(err, ErrNoFullStream) {
		t.Fatal(err)
	}
}
