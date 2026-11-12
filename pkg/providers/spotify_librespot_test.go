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
	"errors"
	"fmt"
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
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	storagepb "github.com/devgianlu/go-librespot/proto/spotify/download"
	extpb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"golang.org/x/crypto/pbkdf2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
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
func (n *fakeSpotifyNative) LibraryPlaylists(context.Context) ([]Playlist, error) {
	if n.err != nil {
		return nil, n.err
	}
	return []Playlist{{ID: "native-p", Title: "Native Playlist"}}, nil
}
func (n *fakeSpotifyNative) LibraryTracks(context.Context, string) ([]Track, error) {
	if n.err != nil {
		return nil, n.err
	}
	return []Track{{ID: "native-t", Title: "Native Track"}}, nil
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

func TestSpotifyRateLimitFallbackToNative(t *testing.T) {
	p := spotifyStoredTest(t)
	p.native = &fakeSpotifyNative{done: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", 429)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("expected fallback on 429, got error: %v", err)
	}
	if len(lists) != 1 || lists[0].ID != "native-p" {
		t.Fatalf("unexpected playlists after 429 fallback: %+v", lists)
	}
	tracks, err := p.Tracks(context.Background(), "37i9dQZF1DXcBWIGoYBM5M")
	if err != nil {
		t.Fatalf("expected tracks fallback on 429, got error: %v", err)
	}
	if len(tracks) != 1 || tracks[0].ID != "native-t" {
		t.Fatalf("unexpected tracks after 429 fallback: %+v", tracks)
	}
}

func TestSpotifyLikedTracksRateLimitTriggersFallback(t *testing.T) {
	p := spotifyStoredTest(t)
	p.native = &fakeSpotifyNative{done: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me/tracks" {
			http.Error(w, "rate limited", 429)
			return
		}
		io.WriteString(w, `{"items":[{"id":"p1","name":"User Playlist"}]}`)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("expected fallback when liked tracks hits 429, got: %v", err)
	}
	if len(lists) != 2 || lists[0].ID != SpotifyLikedID || lists[1].ID != "p1" {
		t.Fatalf("expected Liked Songs and user playlist, got: %+v", lists)
	}
}

func TestSpotifyRateLimitWithoutStoredCredentials(t *testing.T) {
	p := &Spotify{Token: Token{AccessToken: "token"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", 429)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	_, err := p.Playlists(context.Background())
	var st StatusError
	if !errors.As(err, &st) || st != 429 {
		t.Fatalf("expected 429 error without stored credentials, got %v", err)
	}
}

func TestSpotifyLikedTracksRateLimitWithoutStoredCredentials(t *testing.T) {
	p := &Spotify{Token: Token{AccessToken: "token"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me/tracks" {
			http.Error(w, "rate limited", 429)
			return
		}
		io.WriteString(w, `{"items":[{"id":"p1","name":"User Playlist"}]}`)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("expected regular playlists preserved when only liked tracks hits 429 without credentials, got %v", err)
	}
	if len(lists) != 1 || lists[0].ID != "p1" {
		t.Fatalf("expected user playlist, got %+v", lists)
	}
}

func TestSpotifyLikedTracksRateLimitNativeFallbackFailurePreservesPlaylists(t *testing.T) {
	p := spotifyStoredTest(t)
	p.Token = Token{AccessToken: "token", RefreshToken: "refresh", ClientID: "client"}
	p.native = &fakeSpotifyNative{done: make(chan struct{}), err: errors.New("native ap failure")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me/tracks" {
			http.Error(w, "rate limited", 429)
			return
		}
		io.WriteString(w, `{"items":[{"id":"p1","name":"User Playlist"}]}`)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("expected playlists to be preserved when native fallback fails, got error: %v", err)
	}
	if len(lists) != 2 || lists[0].ID != SpotifyLikedID || lists[1].ID != "p1" {
		t.Fatalf("expected regular playlists preserved, got %+v", lists)
	}
}

func TestSpotifyEmptyLibraryLikedTracksRateLimitNativeFallbackFailure(t *testing.T) {
	p := spotifyStoredTest(t)
	p.Token = Token{AccessToken: "token", RefreshToken: "refresh", ClientID: "client"}
	p.native = &fakeSpotifyNative{done: make(chan struct{}), err: errors.New("native ap failure")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me/tracks" {
			http.Error(w, "rate limited", 429)
			return
		}
		io.WriteString(w, `{"items":[]}`)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("expected empty playlists preserved without error, got: %v", err)
	}
	if len(lists) != 1 || lists[0].ID != SpotifyLikedID {
		t.Fatalf("expected Liked Songs playlist, got %+v", lists)
	}
}

func TestSpotifyPaginationRateLimitPreservesFirstPage(t *testing.T) {
	p := spotifyStoredTest(t)
	p.Token = Token{AccessToken: "token", RefreshToken: "refresh", ClientID: "client"}
	p.native = &fakeSpotifyNative{done: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me/tracks" {
			io.WriteString(w, `{"total":0}`)
			return
		}
		if strings.Contains(r.URL.RawQuery, "offset=0") {
			next := "https://api.spotify.com/v1/me/playlists?offset=50&limit=50"
			io.WriteString(w, fmt.Sprintf(`{"items":[{"id":"p0","name":"Page 0 Playlist"}],"total":100,"next":%q}`, next))
			return
		}
		http.Error(w, "rate limited on page 1", 429)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("expected fallback on page 1 rate limit, got error: %v", err)
	}
	foundP0, foundNative := false, false
	for _, l := range lists {
		if l.ID == "p0" {
			foundP0 = true
		}
		if l.ID == "native-p" {
			foundNative = true
		}
	}
	if !foundP0 || !foundNative {
		t.Fatalf("expected both p0 and native-p preserved, got: %+v", lists)
	}
}

func TestSpotifyPageZeroAndNativeFallbackBothFailReturnsError(t *testing.T) {
	p := spotifyStoredTest(t)
	p.Token = Token{AccessToken: "token", RefreshToken: "refresh", ClientID: "client"}
	p.native = &fakeSpotifyNative{done: make(chan struct{}), err: errors.New("native ap failure")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited on page 0", 429)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err == nil {
		t.Fatal("expected error when both page 0 and native fallback fail, got nil")
	}
	if lists != nil {
		t.Fatalf("expected nil list on failure, got %+v", lists)
	}
}

func TestSpotifyPaginationFailureFallsBackToLinkedPlaylists(t *testing.T) {
	dir := t.TempDir()
	p := spotifyStoredTest(t)
	p.Token = Token{AccessToken: "token", RefreshToken: "refresh", ClientID: "client"}
	p.LinksPath = filepath.Join(dir, "spotify-links.json")
	p.native = &fakeSpotifyNative{done: make(chan struct{}), err: errors.New("native ap failure")}
	if err := SaveJSON(p.LinksPath, []spLinked{{ID: "linked1", Name: "Linked Playlist", Tracks: []Track{{ID: "t1"}}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "offset=0") {
			next := "https://api.spotify.com/v1/me/playlists?offset=50&limit=50"
			io.WriteString(w, fmt.Sprintf(`{"items":[{"id":"p0","name":"Page 0"}],"total":100,"next":%q}`, next))
			return
		}
		http.Error(w, "rate limited on page 1", 429)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("expected fallback to linked playlists, got %v", err)
	}
	if len(lists) != 1 || lists[0].ID != "linked1" {
		t.Fatalf("expected linked playlist fallback, got: %+v", lists)
	}
}

func TestSpotifyPaginationFailureWithoutLinkedReturnsError(t *testing.T) {
	p := spotifyStoredTest(t)
	p.Token = Token{AccessToken: "token", RefreshToken: "refresh", ClientID: "client"}
	p.native = &fakeSpotifyNative{done: make(chan struct{}), err: errors.New("native ap failure")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "offset=0") {
			next := "https://api.spotify.com/v1/me/playlists?offset=50&limit=50"
			io.WriteString(w, fmt.Sprintf(`{"items":[{"id":"p0","name":"Page 0"}],"total":100,"next":%q}`, next))
			return
		}
		http.Error(w, "rate limited on page 1", 429)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err == nil {
		t.Fatal("expected error on pagination failure without linked playlists, got nil")
	}
	if lists != nil {
		t.Fatalf("expected nil list on failure, got %+v", lists)
	}
}

func TestSpotifyPaginationFailureDoesNotBlockOnHangingLikedTracks(t *testing.T) {
	p := &Spotify{Token: Token{AccessToken: "token"}}
	likedStarted := make(chan struct{})
	likedCanceled := make(chan struct{})
	testDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me/tracks" {
			close(likedStarted)
			select {
			case <-r.Context().Done():
				close(likedCanceled)
			case <-testDone:
			}
			return
		}
		select {
		case <-likedStarted:
		case <-testDone:
			return
		}
		http.Error(w, "page 0 rate limited immediately", 429)
	}))
	defer srv.Close()
	defer close(testDone)
	p.BaseURL = srv.URL

	type result struct {
		lists []Playlist
		err   error
	}
	resCh := make(chan result, 1)
	go func() {
		lists, err := p.Playlists(context.Background())
		resCh <- result{lists, err}
	}()

	select {
	case res := <-resCh:
		if res.err == nil {
			t.Fatal("expected 429 error, got nil")
		}
		if res.lists != nil {
			t.Fatalf("expected nil list on failure, got %+v", res.lists)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Playlists call timed out; did not cancel liked tracks promptly")
	}

	select {
	case <-likedCanceled:
		// Observed context cancellation on hanging liked tracks request!
	case <-time.After(2 * time.Second):
		t.Fatal("/me/tracks was not canceled")
	}
}

type mockPageResolver struct {
	pages map[int][]*connectpb.ContextTrack
	errs  map[int]error
}

func (m *mockPageResolver) Page(_ context.Context, page int) ([]*connectpb.ContextTrack, error) {
	if m.errs != nil {
		if err := m.errs[page]; err != nil {
			return nil, err
		}
	}
	if tracks, ok := m.pages[page]; ok {
		return tracks, nil
	}
	return nil, io.EOF
}

func mockProtoTrack(i int) *connectpb.ContextTrack {
	var gid [16]byte
	binary.BigEndian.PutUint64(gid[8:], uint64(i+1))
	spotID := librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, gid[:])
	return &connectpb.ContextTrack{Uri: spotID.Uri()}
}

func TestCollectSpotifyContextTracksLimit5001Errors(t *testing.T) {
	res := &mockPageResolver{pages: make(map[int][]*connectpb.ContextTrack)}
	total := maxNativeLibraryTracks + 1 // 5001
	perPage := 100
	for i := 0; i < total; i++ {
		page := i / perPage
		res.pages[page] = append(res.pages[page], mockProtoTrack(i))
	}
	_, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, _ *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		return &extpb.BatchedExtensionResponse{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum track limit") {
		t.Fatalf("expected track limit error for 5001 tracks, got: %v", err)
	}
}

func TestCollectSpotifyContextTracksPageLimit2001Errors(t *testing.T) {
	res := &mockPageResolver{pages: make(map[int][]*connectpb.ContextTrack)}
	for p := 0; p <= 2000; p++ {
		res.pages[p] = []*connectpb.ContextTrack{mockProtoTrack(p)}
	}
	_, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, _ *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		return &extpb.BatchedExtensionResponse{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum page limit") {
		t.Fatalf("expected page limit error for 2001 pages, got: %v", err)
	}
}

func TestCollectSpotifyContextTracksIntermediateEmptyPageDoesNotHalt(t *testing.T) {
	res := &mockPageResolver{
		pages: map[int][]*connectpb.ContextTrack{
			0: {mockProtoTrack(0)},
			1: {}, // empty intermediate page
			2: {mockProtoTrack(1)},
		},
	}
	tracks, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, req *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		data := make([]*extpb.EntityExtensionData, len(req.EntityRequest))
		for i, er := range req.EntityRequest {
			name := er.EntityUri
			packed, _ := anypb.New(&metapb.Track{Name: &name})
			data[i] = &extpb.EntityExtensionData{
				Header:        &extpb.EntityExtensionDataHeader{StatusCode: 200},
				EntityUri:     er.EntityUri,
				ExtensionData: packed,
			}
		}
		return &extpb.BatchedExtensionResponse{
			ExtendedMetadata: []*extpb.EntityExtensionDataArray{{
				ExtensionKind: extpb.ExtensionKind_TRACK_V4,
				ExtensionData: data,
			}},
		}, nil
	})
	if err != nil {
		t.Fatalf("expected successful collection across empty page, got error: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks collected across empty page, got %d", len(tracks))
	}
}

func TestCollectSpotifyContextTracksEmptyPageWithoutURLContinues(t *testing.T) {
	res := &mockPageResolver{
		pages: map[int][]*connectpb.ContextTrack{
			0: {mockProtoTrack(0)},
			2: {mockProtoTrack(1)},
		},
		errs: map[int]error{
			1: errors.New("invalid empty page without url"),
		},
	}
	tracks, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, req *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		data := make([]*extpb.EntityExtensionData, len(req.EntityRequest))
		for i, er := range req.EntityRequest {
			name := er.EntityUri
			packed, _ := anypb.New(&metapb.Track{Name: &name})
			data[i] = &extpb.EntityExtensionData{
				Header:        &extpb.EntityExtensionDataHeader{StatusCode: 200},
				EntityUri:     er.EntityUri,
				ExtensionData: packed,
			}
		}
		return &extpb.BatchedExtensionResponse{
			ExtendedMetadata: []*extpb.EntityExtensionDataArray{{
				ExtensionKind: extpb.ExtensionKind_TRACK_V4,
				ExtensionData: data,
			}},
		}, nil
	})
	if err != nil {
		t.Fatalf("expected successful collection skipping invalid empty page, got error: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks collected, got %d", len(tracks))
	}
}

func TestCollectSpotifyContextTracksPage2001EmptyErrorTriggersPageLimit(t *testing.T) {
	res := &mockPageResolver{
		pages: make(map[int][]*connectpb.ContextTrack),
		errs: map[int]error{
			2000: errors.New("invalid empty page without url"),
		},
	}
	for p := 0; p < 2000; p++ {
		res.pages[p] = []*connectpb.ContextTrack{mockProtoTrack(p)}
	}
	_, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, _ *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		return &extpb.BatchedExtensionResponse{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum page limit") {
		t.Fatalf("expected page limit error when 2001st page is empty error, got: %v", err)
	}
}

func TestCollectSpotifyContextTracksConsecutiveEmptyErrorsTriggerPageLimit(t *testing.T) {
	res := &mockPageResolver{
		pages: make(map[int][]*connectpb.ContextTrack),
		errs:  make(map[int]error),
	}
	for p := 0; p <= 2000; p++ {
		res.errs[p] = errors.New("invalid empty page without url")
	}
	_, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, _ *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		return &extpb.BatchedExtensionResponse{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum page limit") {
		t.Fatalf("expected page limit error on consecutive empty errors, got: %v", err)
	}
}

func TestCollectSpotifyContextTracksExact5000Succeeds(t *testing.T) {
	res := &mockPageResolver{pages: make(map[int][]*connectpb.ContextTrack)}
	total := maxNativeLibraryTracks // 5000
	perPage := 100
	for i := 0; i < total; i++ {
		page := i / perPage
		res.pages[page] = append(res.pages[page], mockProtoTrack(i))
	}
	tracks, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, req *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		data := make([]*extpb.EntityExtensionData, len(req.EntityRequest))
		for i, er := range req.EntityRequest {
			name := er.EntityUri
			packed, _ := anypb.New(&metapb.Track{Name: &name})
			data[i] = &extpb.EntityExtensionData{
				Header:        &extpb.EntityExtensionDataHeader{StatusCode: 200},
				EntityUri:     er.EntityUri,
				ExtensionData: packed,
			}
		}
		return &extpb.BatchedExtensionResponse{
			ExtendedMetadata: []*extpb.EntityExtensionDataArray{{
				ExtensionKind: extpb.ExtensionKind_TRACK_V4,
				ExtensionData: data,
			}},
		}, nil
	})
	if err != nil {
		t.Fatalf("expected exact 5000 tracks to succeed, got error: %v", err)
	}
	if len(tracks) != total {
		t.Fatalf("expected 5000 tracks, got %d", len(tracks))
	}
}

func TestCollectSpotifyContextTracksExact2000PagesSucceeds(t *testing.T) {
	res := &mockPageResolver{pages: make(map[int][]*connectpb.ContextTrack)}
	for p := 0; p < 2000; p++ {
		res.pages[p] = []*connectpb.ContextTrack{mockProtoTrack(p)}
	}
	tracks, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, req *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		data := make([]*extpb.EntityExtensionData, len(req.EntityRequest))
		for i, er := range req.EntityRequest {
			name := er.EntityUri
			packed, _ := anypb.New(&metapb.Track{Name: &name})
			data[i] = &extpb.EntityExtensionData{
				Header:        &extpb.EntityExtensionDataHeader{StatusCode: 200},
				EntityUri:     er.EntityUri,
				ExtensionData: packed,
			}
		}
		return &extpb.BatchedExtensionResponse{
			ExtendedMetadata: []*extpb.EntityExtensionDataArray{{
				ExtensionKind: extpb.ExtensionKind_TRACK_V4,
				ExtensionData: data,
			}},
		}, nil
	})
	if err != nil {
		t.Fatalf("expected exact 2000 pages to succeed, got error: %v", err)
	}
	if len(tracks) != 2000 {
		t.Fatalf("expected 2000 tracks, got %d", len(tracks))
	}
}

func TestCollectSpotifyContextTracksWrappedEOFIsReturnedAsError(t *testing.T) {
	res := &mockPageResolver{
		pages: map[int][]*connectpb.ContextTrack{
			0: {mockProtoTrack(0)},
		},
		errs: map[int]error{
			1: fmt.Errorf("failed fetching next page: %w", io.EOF),
		},
	}
	_, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, _ *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		return &extpb.BatchedExtensionResponse{}, nil
	})
	if err == nil {
		t.Fatal("expected error on wrapped EOF network failure, got nil")
	}
	if !strings.Contains(err.Error(), "failed fetching next page") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCollectSpotifyContextTracksWrappedEOFContainingEmptyPageStringReturnsError(t *testing.T) {
	res := &mockPageResolver{
		pages: map[int][]*connectpb.ContextTrack{
			0: {mockProtoTrack(0)},
		},
		errs: map[int]error{
			1: fmt.Errorf("invalid empty page without url: %w", io.EOF),
		},
	}
	_, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, _ *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		return &extpb.BatchedExtensionResponse{}, nil
	})
	if err == nil {
		t.Fatal("expected error on wrapped EOF containing empty page string, got nil")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected wrapped EOF, got: %v", err)
	}
}

func TestCollectSpotifyContextTracksPage2001WrappedEOFReturnsOriginalError(t *testing.T) {
	origErr := fmt.Errorf("failed fetching next page: %w", io.EOF)
	res := &mockPageResolver{
		pages: make(map[int][]*connectpb.ContextTrack),
		errs: map[int]error{
			2000: origErr,
		},
	}
	for p := 0; p < 2000; p++ {
		res.pages[p] = []*connectpb.ContextTrack{mockProtoTrack(p)}
	}
	_, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, _ *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		return &extpb.BatchedExtensionResponse{}, nil
	})
	if err == nil {
		t.Fatal("expected error on page 2000 wrapped EOF, got nil")
	}
	if !errors.Is(err, origErr) {
		t.Fatalf("expected original error %v, got %v", origErr, err)
	}
}

func TestCollectSpotifyContextTracksNonEOFErrorContainingEmptyPageStringReturnsOriginalError(t *testing.T) {
	origErr := errors.New("network failure: invalid empty page without url occurred")
	res := &mockPageResolver{
		pages: map[int][]*connectpb.ContextTrack{
			0: {mockProtoTrack(0)},
		},
		errs: map[int]error{
			1: origErr,
		},
	}
	_, err := collectSpotifyContextTracks(context.Background(), res, func(_ context.Context, _ *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		return &extpb.BatchedExtensionResponse{}, nil
	})
	if err == nil {
		t.Fatal("expected error on non-exact empty page string error, got nil")
	}
	if !errors.Is(err, origErr) {
		t.Fatalf("expected original error %v, got %v", origErr, err)
	}
}

func TestSpotifyLikedTracksMissingScopePreservesPlaylists(t *testing.T) {
	p := &Spotify{Token: Token{AccessToken: "token"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me/tracks" {
			http.Error(w, "forbidden", 403)
			return
		}
		io.WriteString(w, `{"items":[{"id":"p1","name":"User Playlist"}]}`)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("expected playlists to succeed without liked tracks on 403, got error: %v", err)
	}
	if len(lists) != 1 || lists[0].ID != "p1" {
		t.Fatalf("expected regular playlists preserved on 403, got %+v", lists)
	}
}

func TestSpotifyLinkedPlaylistsReturnedOnRateLimitWithoutStoredCredentials(t *testing.T) {
	dir := t.TempDir()
	p := &Spotify{
		Token:     Token{AccessToken: "token"},
		LinksPath: filepath.Join(dir, "spotify-links.json"),
	}
	if err := SaveJSON(p.LinksPath, []spLinked{{ID: "linked1", Name: "Linked Playlist", Tracks: []Track{{ID: "t1"}}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", 429)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	lists, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("expected linked playlists to succeed without error, got %v", err)
	}
	if len(lists) != 1 || lists[0].ID != "linked1" {
		t.Fatalf("expected linked playlist preserved on 429, got %+v", lists)
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
