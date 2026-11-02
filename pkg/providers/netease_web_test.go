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
