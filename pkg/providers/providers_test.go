package providers

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNeteaseQRAndCompletePlaylist(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Error("cookie metadata must use POST")
		}
		r.ParseForm()
		if r.URL.RawQuery != "" {
			t.Error("credentials leaked into URL")
		}
		var response any
		switch r.URL.Path {
		case "/login/qr/key":
			response = map[string]any{"code": 200, "data": map[string]string{"unikey": "qr-key"}}
		case "/login/qr/create":
			if r.Form.Get("key") != "qr-key" {
				t.Error("key missing")
			}
			response = map[string]any{"code": 200, "data": map[string]string{"qrurl": "https://music.163.com/login?key=qr-key"}}
		case "/login/qr/check":
			polls++
			response = map[string]any{"code": 803, "cookie": "private-cookie"}
		case "/login/status":
			if r.Form.Get("cookie") != "private-cookie" {
				t.Error("cookie missing")
			}
			response = map[string]any{"data": map[string]any{"code": 200, "profile": map[string]int{"userId": 7}}}
		case "/user/playlist":
			if r.Form.Get("uid") != "7" {
				t.Error("wrong account")
			}
			response = map[string]any{"code": 200, "playlist": []any{map[string]any{"id": 11, "name": "Guest songs", "trackCount": 1}}, "more": false}
		case "/playlist/detail":
			response = map[string]any{"code": 200, "playlist": map[string]any{"tracks": []any{}, "trackIds": []any{map[string]int{"id": 99}}}}
		case "/song/detail":
			if r.Form.Get("ids") != "99" {
				t.Error("incomplete tracks not expanded")
			}
			response = map[string]any{"code": 200, "songs": []any{map[string]any{"id": 99, "name": "Title", "dt": 200000, "ar": []any{map[string]string{"name": "Artist"}}, "al": map[string]string{"name": "Album"}}}}
		case "/album/sublist", "/dj/sublist":
			response = map[string]any{"code": 200}
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "owner.json")
	owner := &Netease{BaseURL: srv.URL, CookiePath: path}
	qr, err := owner.BeginQR(ctx)
	if err != nil || qr.Key != "qr-key" || !strings.Contains(qr.URL, "music.163.com") {
		t.Fatalf("QR %+v %v", qr, err)
	}
	if code, err := owner.PollQR(ctx, qr.Key); err != nil || code != 803 {
		t.Fatalf("login %d %v", code, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("credential mode %v %v", st, err)
	}
	playlists, err := owner.Playlists(ctx)
	if err != nil || len(playlists) != 1 || playlists[0].ID != "11" {
		t.Fatalf("playlists %v %v", playlists, err)
	}
	tracks, err := owner.Tracks(ctx, "11")
	if err != nil || len(tracks) != 1 || tracks[0].DurationMS != 200000 {
		t.Fatalf("tracks %v %v", tracks, err)
	}
	guest := &Netease{BaseURL: srv.URL}
	if guest.Cookie != "" || guest.UserID != 0 {
		t.Fatal("guest shares credentials")
	}
	if _, err = guest.PollQR(ctx, qr.Key); err != nil {
		t.Fatal(err)
	}
	if guest.CookiePath != "" || polls != 2 {
		t.Fatal("guest persisted")
	}
}
func TestSpotifyTokenRefreshAndPKCE(t *testing.T) {
	var refresh, exchange, playlistPages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/token":
			r.ParseForm()
			if r.Form.Get("client_id") != "public-client" || r.Form.Get("client_secret") != "" {
				t.Error("wrong PKCE client")
			}
			if r.Form.Get("grant_type") == "refresh_token" {
				refresh++
				if r.Form.Get("refresh_token") != "refresh-private" {
					t.Error("refresh token")
				}
			} else {
				exchange++
				if len(r.Form.Get("code_verifier")) < 43 || r.Form.Get("redirect_uri") != "https://device/callback" {
					t.Error("PKCE verifier/redirect")
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-private", "expires_in": 3600, "token_type": "Bearer"})
		case "/me/playlists":
			if r.Header.Get("Authorization") != "Bearer fresh-private" {
				t.Error("expired bearer token")
			}
			playlistPages++
			if r.URL.Query().Get("offset") == "0" {
				ioJSON(w, `{"items":[{"id":"p1","name":"First","tracks":{"total":1}}],"next":"https://untrusted.invalid/token-steal"}`)
			} else {
				ioJSON(w, `{"items":[{"id":"p2","name":"Second"}],"next":null}`)
			}
		case "/me/tracks":
			if r.URL.Query().Get("limit") == "1" {
				ioJSON(w, `{"total":2,"items":[]}`)
			} else {
				ioJSON(w, `{"items":[{"track":{"id":"l1","name":"One","type":"track"}},{"item":{"id":"l2","name":"Two","type":"track"}}],"next":null}`)
			}
		case "/playlists/p1/tracks", "/playlists/p1/items":
			ioJSON(w, `{"items":[{"track":null},{"is_local":true,"track":{"id":"local"}},{"item":{"id":"s1","name":"Song","type":"track","duration_ms":180001,"artists":[{"name":"Artist"}],"album":{"name":"Album"},"external_ids":{"isrc":"US1234567890"}}}]}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "token.json")
	SaveJSON(path, Token{AccessToken: "expired-private", RefreshToken: "refresh-private", Expiry: time.Now().Add(-time.Hour), ClientID: "public-client"})
	sp := &Spotify{TokenPath: path, BaseURL: srv.URL, AccountsURL: srv.URL}
	if err := sp.LoadToken(); err != nil {
		t.Fatal(err)
	}
	lists, err := sp.Playlists(context.Background())
	if err != nil || len(lists) != 3 || lists[0].ID != "liked" || refresh != 1 || playlistPages != 2 {
		t.Fatalf("pagination/refresh %v %v", lists, err)
	}
	tracks, err := sp.Tracks(context.Background(), "p1")
	if err != nil || len(tracks) != 1 || tracks[0].ISRC != "US1234567890" || tracks[0].DurationMS != 180001 {
		t.Fatalf("metadata %v %v", tracks, err)
	}
	auth, verifier := sp.BeginPKCE("state-bound", "https://device/callback")
	if !strings.Contains(auth, "code_challenge_method=S256") || !strings.Contains(auth, "state=state-bound") {
		t.Fatal(auth)
	}
	if err = sp.CompletePKCE(context.Background(), "code", verifier, "https://device/callback"); err != nil {
		t.Fatal(err)
	}
	if exchange != 1 || sp.Token.RefreshToken != "refresh-private" {
		t.Fatal("refresh credential lost")
	}
}
func ioJSON(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(s))
}
func TestDeezerPublicPlaylistAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/7/playlists":
			ioJSON(w, `{"data":[{"id":8,"title":"Public","nb_tracks":1}]}`)
		case "/playlist/8/tracks", "/search":
			ioJSON(w, `{"data":[{"id":9,"title":"Song","duration":200,"artist":{"name":"Artist"}}]}`)
		case "/track/isrc:US123":
			ioJSON(w, `{"id":9,"isrc":"US123","duration":200,"title":"Song"}`)
		default:
			ioJSON(w, `{"error":{"message":"private token must not appear"}}`)
		}
	}))
	defer srv.Close()
	d := &Deezer{BaseURL: srv.URL, UserID: "7"}
	if p, e := d.Playlists(context.Background()); e != nil || len(p) != 1 || p[0].ID != "8" {
		t.Fatalf("playlists %v %v", p, e)
	}
	if tracks, e := d.Tracks(context.Background(), "8"); e != nil || len(tracks) != 1 || tracks[0].DurationMS != 200000 {
		t.Fatalf("tracks %v %v", tracks, e)
	}
	if tr, e := d.ByISRC(context.Background(), "US123"); e != nil || tr.ID != "9" {
		t.Fatalf("isrc %v %v", tr, e)
	}
	if _, e := d.ByISRC(context.Background(), "bad"); e == nil || strings.Contains(e.Error(), "private token") {
		t.Fatal("API error leaked body")
	}
}

func TestSpotifyTracksFallBackOnlyWhenItemsIsGone(t *testing.T) {
	for _, c := range []struct {
		status   int
		wantHits int
		wantErr  bool
	}{{429, 1, true}, {500, 1, true}, {404, 2, false}} {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			if r.URL.Path == "/playlists/p1/items" {
				w.WriteHeader(c.status)
				return
			}
			ioJSON(w, `{"items":[]}`)
		}))
		sp := &Spotify{BaseURL: srv.URL, Token: Token{AccessToken: "a", Expiry: time.Now().Add(time.Hour)}}
		_, err := sp.Tracks(context.Background(), "p1")
		srv.Close()
		if hits != c.wantHits || (err != nil) != c.wantErr || (c.wantErr && !strings.Contains(err.Error(), strconv.Itoa(c.status))) {
			t.Errorf("status %d: hits=%d err=%v", c.status, hits, err)
		}
	}
}

func TestSpotifyDownloadAudio(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// 1. Not logged in
	var unauth Spotify
	if _, err := unauth.DownloadAudio(ctx, "t1", tmpDir); err == nil {
		t.Fatal("expected error when not logged in")
	}

	rawKey, _ := hex.DecodeString("0102030405060708090a0b0c0d0e0f10")
	iv, _ := hex.DecodeString("72e067fbddcbcf77ebe8bc643f630d93")

	// Payload with 167 bytes private header followed by OggS
	privHeader := make([]byte, 167)
	for i := range privHeader {
		privHeader[i] = byte(i)
	}
	realPayload := append(privHeader, []byte("OggS-stream-content")...)

	block, err := aes.NewCipher(rawKey)
	if err != nil {
		t.Fatal(err)
	}
	cipherPayload := make([]byte, len(realPayload))
	cipher.NewCTR(block, iv).XORKeyStream(cipherPayload, realPayload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer valid-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/audio/encryptedheader":
			w.Header().Set("X-Spotify-Audio-Key", hex.EncodeToString(rawKey))
			w.Write(cipherPayload)
		case "/audio/encryptedfield":
			w.Write(cipherPayload)
		case "/audio/plain":
			w.Write([]byte("OggS-plain-content"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sp := &Spotify{
		BaseURL: srv.URL,
		Token:   Token{AccessToken: "valid-token"},
	}

	// 2. Encrypted with X-Spotify-Audio-Key header and 0xa7 stripping
	path1, err := sp.DownloadAudio(ctx, "encryptedheader", tmpDir)
	if err != nil {
		t.Fatalf("DownloadAudio encryptedheader: %v", err)
	}
	if filepath.Base(path1) != "spotify-encryptedheader.ogg" {
		t.Fatalf("wrong file name: %s", path1)
	}
	data1, err := os.ReadFile(path1)
	if err != nil {
		t.Fatal(err)
	}
	if string(data1) != "OggS-stream-content" {
		t.Fatalf("unexpected content: %q", string(data1))
	}

	// 3. Encrypted with sp.AudioKey
	sp.AudioKey = rawKey
	path2, err := sp.DownloadAudio(ctx, "encryptedfield", tmpDir)
	if err != nil {
		t.Fatalf("DownloadAudio encryptedfield: %v", err)
	}
	data2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if string(data2) != "OggS-stream-content" {
		t.Fatalf("unexpected content: %q", string(data2))
	}

	// 4. Plain download
	sp.AudioKey = nil
	path3, err := sp.DownloadAudio(ctx, "plain", tmpDir)
	if err != nil {
		t.Fatalf("DownloadAudio plain: %v", err)
	}
	data3, err := os.ReadFile(path3)
	if err != nil {
		t.Fatal(err)
	}
	if string(data3) != "OggS-plain-content" {
		t.Fatalf("unexpected plain content: %q", string(data3))
	}
}

func TestSpotifyPublicLinks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed/playlist/37i9dQZF1DXcBWIGoYBM5M" {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `<html><script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"state":{"data":{"entity":{"name":"Mix","trackList":[{"uri":"spotify:track:abc","title":"T","subtitle":"A, B","duration":1000},{"uri":"spotify:episode:x","title":"E"}]}}}}}}</script></html>`)
	}))
	defer srv.Close()
	sp := &Spotify{LinksPath: filepath.Join(t.TempDir(), "l.json"), EmbedURL: srv.URL}
	n, err := sp.SetLinks(context.Background(), "see https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M?si=1")
	if err != nil || n != 1 || !sp.HasLinks() {
		t.Fatal(n, err)
	}
	pl, err := sp.Playlists(context.Background())
	if err != nil || len(pl) != 1 || pl[0].Title != "Mix" {
		t.Fatal(pl, err)
	}
	tr, err := sp.Tracks(context.Background(), pl[0].ID)
	if err != nil || len(tr) != 1 || tr[0].ID != "abc" || tr[0].Artist != "A, B" || tr[0].DurationMS != 1000 {
		t.Fatalf("%+v %v", tr, err)
	}
}

func TestSpotifyConnectBlobRoundTrip(t *testing.T) {
	k := SpotifyConnectKeyPair()
	pub, err := base64.StdEncoding.DecodeString(k.PublicKey())
	if err != nil || len(pub) != 96 {
		t.Fatalf("public key: %d bytes, %v", len(pub), err)
	}
	// The phone's side: its own key pair, the same derivation.
	priv, _ := rand.Int(rand.Reader, spotifyDHPrime)
	clientPub := new(big.Int).Exp(big.NewInt(2), priv, spotifyDHPrime).FillBytes(make([]byte, 96))
	shared := new(big.Int).Exp(new(big.Int).SetBytes(pub), priv, spotifyDHPrime)
	base := sha1.Sum(shared.FillBytes(make([]byte, 96)))
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha1.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	iv := bytes.Repeat([]byte{7}, 16)
	block, _ := aes.NewCipher(mac(base[:16], "encryption")[:16])
	data := []byte("login-credentials")
	enc := make([]byte, len(data))
	cipher.NewCTR(block, iv).XORKeyStream(enc, data)
	sum := hmac.New(sha1.New, mac(base[:16], "checksum"))
	sum.Write(enc)
	blob := append(append(append([]byte{}, iv...), enc...), sum.Sum(nil)...)
	ck := base64.StdEncoding.EncodeToString(clientPub)
	got, err := k.DecryptBlob(ck, base64.StdEncoding.EncodeToString(blob))
	if err != nil || string(got) != string(data) {
		t.Fatalf("decrypt: %q %v", got, err)
	}
	blob[20] ^= 1
	if _, err := k.DecryptBlob(ck, base64.StdEncoding.EncodeToString(blob)); err == nil {
		t.Fatal("a tampered blob was accepted")
	}
}
