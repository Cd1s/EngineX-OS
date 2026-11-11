package providers

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
	"github.com/devgianlu/go-librespot/audio"
	authpb "github.com/devgianlu/go-librespot/proto/spotify"
	pbhttp "github.com/devgianlu/go-librespot/proto/spotify/clienttoken/http/v0"
	storagepb "github.com/devgianlu/go-librespot/proto/spotify/download"
	metapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"golang.org/x/crypto/pbkdf2"
	"google.golang.org/protobuf/proto"
)

type fakeSpotifyNative struct {
	done         chan struct{}
	once         sync.Once
	meta         *metapb.Track
	urls         []string
	key          []byte
	err          error
	requestedGID []byte
}

func (n *fakeSpotifyNative) Done() <-chan struct{} { return n.done }
func (n *fakeSpotifyNative) Close()                { n.once.Do(func() { close(n.done) }) }
func (n *fakeSpotifyNative) Bearer(context.Context) (string, error) {
	return "login5-test-bearer", n.err
}
func (n *fakeSpotifyNative) Metadata(context.Context, librespot.SpotifyId) (*metapb.Track, error) {
	return n.meta, n.err
}
func (n *fakeSpotifyNative) Key(_ context.Context, gid, _ []byte) ([]byte, error) {
	n.requestedGID = bytes.Clone(gid)
	return n.key, n.err
}
func (n *fakeSpotifyNative) Storage(context.Context, *metapb.AudioFile) (*storagepb.StorageResolveResponse, error) {
	return &storagepb.StorageResolveResponse{Cdnurl: n.urls}, nil
}
func spotifyTestFile(f metapb.AudioFile_Format) *metapb.AudioFile {
	return &metapb.AudioFile{Format: f.Enum(), FileId: bytes.Repeat([]byte{1}, 20)}
}
func spotifyStoredTest(t *testing.T) *Spotify {
	t.Helper()
	p := &Spotify{CredsPath: filepath.Join(t.TempDir(), "spotify-librespot.json")}
	if err := SaveJSON(p.CredsPath, spotifyCredentials{"fixture-user", []byte("fixture-credentials"), p.deviceID()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}
func TestSpotifyNativeAlternativeAndCDNRetry(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := append(testaudio.SpotifyOggMetadataPage(139), []byte("OggS-native-test")...)
	encrypted := make([]byte, len(plain))
	block, _ := aes.NewCipher(key)
	cipher.NewCTR(block, spotifyIV).XORKeyStream(encrypted, plain)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer leaked to CDN")
		}
		if r.URL.Path == "/first" {
			w.WriteHeader(503)
			return
		}
		w.Write(encrypted)
	}))
	defer srv.Close()
	gid := bytes.Repeat([]byte{2}, 16)
	n := &fakeSpotifyNative{done: make(chan struct{}), key: key, urls: []string{srv.URL + "/first", srv.URL + "/second"}, meta: &metapb.Track{
		Gid: bytes.Repeat([]byte{1}, 16), File: []*metapb.AudioFile{spotifyTestFile(metapb.AudioFile_MP3_320)},
		Alternative: []*metapb.Track{{Gid: gid, File: []*metapb.AudioFile{spotifyTestFile(metapb.AudioFile_OGG_VORBIS_96), spotifyTestFile(metapb.AudioFile_OGG_VORBIS_320)}}},
	}}
	p := spotifyStoredTest(t)
	p.native = n
	path, err := p.DownloadAudio(context.Background(), "4uLU6hMCjMI75M1A2tKUQC", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "OggS-native-test" || !bytes.Equal(n.requestedGID, gid) || calls.Load() != 2 {
		t.Fatalf("bad native download: bytes=%d, requests=%d, err=%v", len(b), calls.Load(), err)
	}
	if got, _ := selectSpotifyFile(n.meta); !bytes.Equal(got, gid) {
		t.Fatal("wrong alternative GID")
	}
	n.meta.File = []*metapb.AudioFile{spotifyTestFile(metapb.AudioFile_OGG_VORBIS_160)}
	got, f := selectSpotifyFile(n.meta)
	if !bytes.Equal(got, n.meta.Gid) || f.GetFormat() != metapb.AudioFile_OGG_VORBIS_160 {
		t.Fatal("original available file must precede alternatives")
	}
}
func TestSpotifyNativePremiumAndInvalidAudio(t *testing.T) {
	p := spotifyStoredTest(t)
	// Exercise the real audio-key error mapping independently of metadata errors.
	n := &fakeSpotifyNative{done: make(chan struct{}), meta: &metapb.Track{Gid: make([]byte, 16), File: []*metapb.AudioFile{spotifyTestFile(metapb.AudioFile_OGG_VORBIS_320)}}}
	p.native = &refusedSpotifyKey{fakeSpotifyNative: n}
	if _, err := p.DownloadAudio(context.Background(), "4uLU6hMCjMI75M1A2tKUQC", t.TempDir()); err == nil || err.Error() != "spotify: audio key refused (code 1; Spotify Premium account required)" {
		t.Fatalf("wrong refusal: %v", err)
	}
	dir := t.TempDir()
	if _, err := writeSpotifyAudio(context.Background(), []byte("not an ogg"), "abc", dir); err == nil {
		t.Fatal("invalid decrypted data accepted")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("partial output retained")
	}
	for _, id := range []string{"zzzzzzzzzzzzzzzzzzzzzz", "../outside"} {
		if _, err := p.DownloadAudio(context.Background(), id, dir); err == nil {
			t.Fatal("invalid ID accepted")
		}
	}
}

type refusedSpotifyKey struct{ *fakeSpotifyNative }

func (*refusedSpotifyKey) Key(context.Context, []byte, []byte) ([]byte, error) {
	return nil, &audio.KeyProviderError{Code: 1}
}

func TestSpotifyStoredAuthorizationReconnectAndLogout(t *testing.T) {
	p := spotifyStoredTest(t)
	n := &fakeSpotifyNative{done: make(chan struct{})}
	p.native = n
	var reconnects int
	p.nativeFactory = func(context.Context) (spotifyNative, error) {
		reconnects++
		return &fakeSpotifyNative{done: make(chan struct{})}, nil
	}
	for _, tok := range []Token{{}, {AccessToken: "expired", Expiry: time.Now().Add(-time.Hour)}, {AccessToken: "no-refresh"}} {
		p.Token = tok
		h, err := p.authorization(context.Background())
		if err != nil || h.Get("Authorization") != "Bearer login5-test-bearer" {
			t.Fatalf("login5 bearer unavailable: %v", err)
		}
	}
	n.Close()
	if _, err := p.authorization(context.Background()); err != nil || reconnects != 1 {
		t.Fatalf("no automatic reconnect: %v", err)
	}
	// An in-flight closed-accesspoint error also triggers one bounded retry.
	p.native = &fakeSpotifyNative{done: make(chan struct{}), err: ap.ErrAccesspointClosed}
	if _, err := p.authorization(context.Background()); err != nil || reconnects != 2 {
		t.Fatalf("no closed-AP retry: %v", err)
	}
	info, _ := os.Stat(p.CredsPath)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credentials permissions")
	}
	if err := p.Logout(); err != nil {
		t.Fatal(err)
	}
	if p.HasStoredCredentials() || p.HasToken() {
		t.Fatal("logout retained credentials")
	}
	if _, err := p.authorization(context.Background()); err == nil {
		t.Fatal("logout resurrected session")
	}
}
func TestSpotifyStoredLoginUnlocksPlaylists(t *testing.T) {
	p := spotifyStoredTest(t)
	p.native = &fakeSpotifyNative{done: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer login5-test-bearer" {
			t.Error("missing login5 bearer")
		}
		switch r.URL.Path {
		case "/me/tracks":
			io.WriteString(w, `{"total":2}`)
		case "/me/playlists":
			io.WriteString(w, `{"items":[{"id":"p","name":"Private"}]}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	list, err := p.Playlists(context.Background())
	if err != nil || len(list) != 2 || list[0].ID != SpotifyLikedID {
		t.Fatalf("stored login did not unlock library: %v", err)
	}
}

func spotifyInnerTestBlob(device, username string) []byte {
	raw := []byte{0, 0, 0, 1, 0, 4, 't', 'e', 's', 't'}
	raw = append(raw, make([]byte, 32-len(raw))...)
	// Undo the reverse XOR pass before encrypting each ECB block.
	for i := 16; i < len(raw); i++ {
		raw[i] ^= raw[i-16]
	}
	secret := sha1.Sum([]byte(device))
	base := pbkdf2.Key(secret[:], []byte(username), 256, 20, sha1.New)
	digest := sha1.Sum(base)
	key := make([]byte, 24)
	copy(key, digest[:])
	binary.BigEndian.PutUint32(key[20:], 20)
	block, _ := aes.NewCipher(key)
	for i := 0; i < len(raw); i += 16 {
		block.Encrypt(raw[i:i+16], raw[i:i+16])
	}
	return []byte(base64.StdEncoding.EncodeToString(raw))
}
func TestSpotifyInnerBlobDeviceBinding(t *testing.T) {
	id := SpotifyDeviceID("state")
	blob := spotifyInnerTestBlob(id, "fixture-user")
	if err := validateSpotifyInnerBlob(id, "fixture-user", blob); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte("?"), []byte(base64.StdEncoding.EncodeToString([]byte("short")))} {
		if err := validateSpotifyInnerBlob(id, "fixture-user", bad); err == nil {
			t.Fatal("malformed inner blob accepted")
		}
	}
	if err := validateSpotifyInnerBlob(SpotifyDeviceID("other"), "fixture-user", blob); err == nil {
		t.Fatal("blob accepted for a different device")
	}
}

func TestSpotifyDeviceAuthorizationPolling(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("client_id") != SpotifyBuiltinClientID {
			t.Error("wrong client")
		}
		switch r.URL.Path {
		case "/oauth2/device/authorize":
			if !strings.Contains(r.Form.Get("scope"), "streaming") {
				t.Error("streaming scope missing")
			}
			io.WriteString(w, `{"device_code":"fixture-device-code","user_code":"ABCD","verification_uri_complete":"https://accounts.spotify.com/pair?user_code=ABCD","expires_in":600,"interval":5}`)
		case "/api/token":
			polls++
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("device_code") != "fixture-device-code" {
				t.Error("invalid device grant")
			}
			if polls == 1 {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"authorization_pending"}`)
				return
			}
			if polls == 2 {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"slow_down"}`)
				return
			}
			io.WriteString(w, `{"access_token":"fixture-device-token","expires_in":3600}`)
		default:
			t.Error("unexpected device URL")
		}
	}))
	defer srv.Close()
	p := &Spotify{ClientID: "fixture-client", AccountsURL: srv.URL, TokenPath: filepath.Join(t.TempDir(), "token.json")}
	d, err := p.BeginDeviceAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := p.PollDeviceAuthorization(context.Background(), d); ok || err != nil || polls != 0 {
		t.Fatal("interval ignored")
	}
	for i := 0; i < 3; i++ {
		d.next = time.Time{}
		ok, err := p.PollDeviceAuthorization(context.Background(), d)
		if err != nil || ok != (i == 2) {
			t.Fatalf("poll %d: %v %v", i, ok, err)
		}
	}
	if d.interval != 10*time.Second || d.code != "" || !p.HasToken() || p.Token.ClientID != SpotifyBuiltinClientID {
		t.Fatal("device state not finalized")
	}
	if _, err := p.PollDeviceAuthorization(context.Background(), d); err != nil || polls != 3 {
		t.Fatal("approved code was polled again")
	}
	// Public JSON never contains the private device code.
	u, _ := url.Parse(d.VerificationURL)
	if u.Query().Get("user_code") != "ABCD" {
		t.Fatal("confirmation link lost")
	}
}

func TestSpotifyProductionNeverUsesWebAPIAudio(t *testing.T) {
	var requests int
	p := &Spotify{Token: Token{AccessToken: "fixture-token"}, HTTP: &http.Client{Transport: spotifyProtocolRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "apresolve.spotify.com" || strings.Contains(r.URL.Path, "/audio/") {
			t.Errorf("unexpected production endpoint: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}}
	defer p.Close()
	if _, err := p.DownloadAudio(context.Background(), "4uLU6hMCjMI75M1A2tKUQC", t.TempDir()); err == nil || requests == 0 {
		t.Fatal("production download did not use AP resolution")
	}
}

type spotifyProtocolRoundTrip func(*http.Request) (*http.Response, error)

func (f spotifyProtocolRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSpotifyClientTokenProtocol(t *testing.T) {
	p := &Spotify{DeviceID: SpotifyDeviceID("fixture-state")}
	p.HTTP = &http.Client{Transport: spotifyProtocolRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://clienttoken.spotify.com/v1/clienttoken" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Error("invalid client token request")
		}
		b, _ := io.ReadAll(r.Body)
		var request pbhttp.ClientTokenRequest
		if proto.Unmarshal(b, &request) != nil || request.GetClientData().GetClientId() != librespot.ClientIdHex || request.GetClientData().GetConnectivitySdkData().GetDeviceId() != p.DeviceID {
			t.Error("client token identity mismatch")
		}
		b, _ = proto.Marshal(&pbhttp.ClientTokenResponse{ResponseType: pbhttp.ClientTokenResponseType_RESPONSE_GRANTED_TOKEN_RESPONSE, Response: &pbhttp.ClientTokenResponse_GrantedToken{GrantedToken: &pbhttp.GrantedTokenResponse{Token: "fixture-client-token"}}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b))}, nil
	})}
	token, err := p.retrieveClientToken(context.Background())
	if err != nil || token != "fixture-client-token" {
		t.Fatalf("client token handshake failed: %v", err)
	}
}

func TestSpotifyVorbisQualityAndAlternativeOrder(t *testing.T) {
	gid := bytes.Repeat([]byte{1}, 16)
	m := &metapb.Track{Gid: gid, File: []*metapb.AudioFile{spotifyTestFile(metapb.AudioFile_OGG_VORBIS_96), spotifyTestFile(metapb.AudioFile_OGG_VORBIS_320), spotifyTestFile(metapb.AudioFile_OGG_VORBIS_160)}}
	_, f := selectSpotifyFile(m)
	if f.GetFormat() != metapb.AudioFile_OGG_VORBIS_320 {
		t.Fatal("highest Vorbis quality not selected")
	}
	m.File = nil
	m.Alternative = []*metapb.Track{{Gid: bytes.Repeat([]byte{2}, 16), File: []*metapb.AudioFile{spotifyTestFile(metapb.AudioFile_OGG_VORBIS_96)}}, {Gid: bytes.Repeat([]byte{3}, 16), File: []*metapb.AudioFile{spotifyTestFile(metapb.AudioFile_OGG_VORBIS_320)}}}
	got, f := selectSpotifyFile(m)
	if !bytes.Equal(got, m.Alternative[0].Gid) || f.GetFormat() != metapb.AudioFile_OGG_VORBIS_96 {
		t.Fatal("alternatives were not tried in metadata order")
	}
}

// Upstream hashes the minimal big-endian DH secret, without 96-byte padding.
// A tiny deterministic secret catches the old padding bug on every run.
func TestSpotifyConnectUnpaddedDHSecret(t *testing.T) {
	k := &SpotifyConnectKey{priv: big.NewInt(1), pub: []byte{2}}
	base := sha1.Sum([]byte{2})
	derive := func(label string) []byte {
		m := hmac.New(sha1.New, base[:16])
		m.Write([]byte(label))
		return m.Sum(nil)
	}
	iv := make([]byte, 16)
	block, _ := aes.NewCipher(derive("encryption")[:16])
	plain := []byte("fixture-inner-blob")
	encrypted := make([]byte, len(plain))
	cipher.NewCTR(block, iv).XORKeyStream(encrypted, plain)
	mac := hmac.New(sha1.New, derive("checksum"))
	mac.Write(encrypted)
	raw := append(append(iv, encrypted...), mac.Sum(nil)...)
	got, err := k.DecryptBlob(base64.StdEncoding.EncodeToString([]byte{2}), base64.StdEncoding.EncodeToString(raw))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("unpadded DH secret refused: %v", err)
	}
}

func TestSpotifyLegacyTokenExplainsNativeLoginMigration(t *testing.T) {
	rejected := &ap.AccesspointLoginError{Message: &authpb.APLoginFailed{ErrorCode: authpb.ErrorCode_BadCredentials.Enum()}}
	if got := spotifyAPAuthenticationError(rejected, true); got.Error() != spotifyLegacyTokenMessage {
		t.Fatalf("missing migration guidance: %v", got)
	}
	if got := spotifyAPAuthenticationError(rejected, false); got.Error() == spotifyLegacyTokenMessage {
		t.Fatal("stored credentials were mislabeled as a legacy OAuth login")
	}
}

func TestSpotifyDeviceAuthAcceptsOfficialPairURI(t *testing.T) {
	p := &Spotify{ClientID: "legacy-developer-app", HTTP: &http.Client{Transport: spotifyProtocolRoundTrip(func(r *http.Request) (*http.Response, error) {
		r.ParseForm()
		if r.Form.Get("client_id") != librespot.ClientIdHex {
			t.Error("legacy app ID was sent for device authorization")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"device_code":"fixture-code","user_code":"ABCD","verification_uri":"https://spotify.com/pair","expires_in":3599,"interval":5}`))}, nil
	})}}
	d, err := p.BeginDeviceAuth(context.Background())
	if err != nil || d.VerificationURL != "https://spotify.com/pair" || d.UserCode != "ABCD" {
		t.Fatalf("official pair URI was rejected: %v", err)
	}
}
