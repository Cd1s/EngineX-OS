package providers

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
	authpb "github.com/devgianlu/go-librespot/proto/spotify"
	extpb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	filespb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata/audiofiles"
	metapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestSpotifyAudioFilesExtensionAndRelinking(t *testing.T) {
	original := librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, bytes.Repeat([]byte{1}, 16))
	alternative := bytes.Repeat([]byte{2}, 16)
	for _, restricted := range []bool{false, true} {
		t.Run(map[bool]string{true: "restricted", false: "empty-original"}[restricted], func(t *testing.T) {
			var audioGIDs [][]byte
			root := &metapb.Track{Gid: original.Id(), Alternative: []*metapb.Track{{Gid: alternative}}}
			if restricted {
				root.Restriction = []*metapb.Restriction{{CountryRestriction: &metapb.Restriction_CountriesForbidden{CountriesForbidden: "US"}}}
			}
			result, err := resolveSpotifyMetadata(context.Background(), original, "US", func(_ context.Context, id librespot.SpotifyId, kind extpb.ExtensionKind, out proto.Message) error {
				switch kind {
				case extpb.ExtensionKind_TRACK_V4:
					proto.Merge(out, root)
				case extpb.ExtensionKind_AUDIO_FILES:
					audioGIDs = append(audioGIDs, bytes.Clone(id.Id()))
					if bytes.Equal(id.Id(), alternative) {
						proto.Merge(out, &filespb.AudioFilesExtensionResponse{Files: []*filespb.ExtendedAudioFile{{File: spotifyTestFile(metapb.AudioFile_OGG_VORBIS_320)}}})
					}
				default:
					t.Fatal("unexpected metadata kind")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			gid, file := selectSpotifyFile(result)
			if !bytes.Equal(gid, alternative) || file.GetFormat() != metapb.AudioFile_OGG_VORBIS_320 || len(result.Alternative) != 0 {
				t.Fatal("AUDIO_FILES and audio key GID were not relinked together")
			}
			expected := 2
			if restricted {
				expected = 1
			}
			if len(audioGIDs) != expected || !bytes.Equal(audioGIDs[len(audioGIDs)-1], alternative) {
				t.Fatal("restricted or unavailable candidates resolved incorrectly")
			}
		})
	}
}
func TestSpotifyAudioFilesPreferredOverLegacyFiles(t *testing.T) {
	id := librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, make([]byte, 16))
	for _, extensionFails := range []bool{false, true} {
		meta, err := resolveSpotifyMetadata(context.Background(), id, "", func(_ context.Context, _ librespot.SpotifyId, kind extpb.ExtensionKind, out proto.Message) error {
			if kind == extpb.ExtensionKind_TRACK_V4 {
				proto.Merge(out, &metapb.Track{Gid: id.Id(), File: []*metapb.AudioFile{spotifyTestFile(metapb.AudioFile_OGG_VORBIS_96)}})
				return nil
			}
			if extensionFails {
				return errors.New("extension unavailable")
			}
			proto.Merge(out, &filespb.AudioFilesExtensionResponse{Files: []*filespb.ExtendedAudioFile{{File: spotifyTestFile(metapb.AudioFile_OGG_VORBIS_320)}}})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_, file := selectSpotifyFile(meta)
		expected := metapb.AudioFile_OGG_VORBIS_320
		if extensionFails {
			expected = metapb.AudioFile_OGG_VORBIS_96
		}
		if file.GetFormat() != expected {
			t.Fatal("legacy fallback or extension priority wrong")
		}
	}
}
func TestSpotifyOggPageHeaderStripping(t *testing.T) {
	vorbis := append([]byte("OggS"), make([]byte, 23)...)
	for _, size := range []int{139, 255, 300, 700} {
		page := testaudio.SpotifyOggMetadataPage(size)
		if size == 139 && len(page) != 167 {
			t.Fatal("bad realistic header fixture")
		}
		stream := append(bytes.Clone(page), vorbis...)
		if got := stripSpotifyOggHeader(stream); !bytes.Equal(got, vorbis) {
			t.Fatalf("Spotify metadata page of %d bytes retained", len(page))
		}
		for n := 0; n < len(page); n++ {
			truncated := stream[:n]
			if got := stripSpotifyOggHeader(truncated); len(got) != len(truncated) {
				t.Fatal("truncated page was stripped")
			}
		}
	}
	normal := testaudio.SpotifyOggMetadataPage(139)
	normal[28] = 0x01
	if got := stripSpotifyOggHeader(append(bytes.Clone(normal), vorbis...)); !bytes.Equal(got, append(normal, vorbis...)) {
		t.Fatal("standard Vorbis identification page was removed")
	}
	raw := append(make([]byte, 167), vorbis...)
	if !bytes.Equal(stripSpotifyOggHeader(raw), vorbis) {
		t.Fatal("legacy raw header compatibility lost")
	}
}
func TestSpotifyDeviceIdentityIsUniquePersistentAndConcurrent(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	id, err := LoadSpotifyDeviceID(a)
	if err != nil {
		t.Fatal(err)
	}
	other, err := LoadSpotifyDeviceID(b)
	if err != nil || id == other || len(id) != 40 || id != strings.ToLower(id) {
		t.Fatal("separate decks received colliding IDs")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := LoadSpotifyDeviceID(a)
			if e != nil || got != id {
				t.Error("concurrent identity creation changed ID")
			}
		}()
	}
	wg.Wait()
	info, err := os.Stat(filepath.Join(a, "spotify-device-id"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("identity not private")
	}
	if again, err := LoadSpotifyDeviceID(""); err != nil || again == "" {
		t.Fatal("machine fallback unavailable")
	}
}
func TestSpotifyPermanentAPFailureCancelsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := &spotifyAPLogger{cancel: cancel}
	rejected := &ap.AccesspointLoginError{Message: &authpb.APLoginFailed{ErrorCode: authpb.ErrorCode_BadCredentials.Enum()}}
	logger.WithField("ignored", "fixture").WithError(rejected).Warn("ignored")
	if ctx.Err() == nil || logger.rejection() == nil {
		t.Fatal("AP permanent rejection did not stop upstream retries")
	}
	if !errors.Is(spotifyAPAuthenticationError(rejected, false), ErrSpotifyAuthentication) {
		t.Fatal("permanent failure classification missing")
	}
}
func TestSpotifyPairingDoesNotQueueOnNativeMutex(t *testing.T) {
	p := spotifyStoredTest(t)
	blob := spotifyInnerTestBlob(p.deviceID(), "fixture-user")
	p.nativeMu.Lock()
	start := time.Now()
	err := p.ConnectZeroconf(context.Background(), "fixture-user", blob)
	p.nativeMu.Unlock()
	if !errors.Is(err, ErrSpotifyPairingBusy) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("pairing queued behind an active native request: %v", err)
	}
}
func TestSpotifyMediaHonorsContextWithoutJSONBodyTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(80 * time.Millisecond)
		io.WriteString(w, "OggS-fixture")
	}))
	defer srv.Close()
	p := &Spotify{HTTP: &http.Client{Timeout: 10 * time.Millisecond}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if body, err := p.fetchSpotifyCDN(ctx, srv.URL); err != nil || string(body) != "OggS-fixture" {
		t.Fatalf("JSON total timeout aborted audio body: %v", err)
	}
	short, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if _, err := p.fetchSpotifyCDN(short, srv.URL); err == nil {
		t.Fatal("CDN read ignored caller cancellation")
	}
}

type spotifyLibraryFixture struct {
	*fakeSpotifyNative
	playlistCalls, trackCalls int
}

func (f *spotifyLibraryFixture) LibraryPlaylists(context.Context) ([]Playlist, error) {
	f.playlistCalls++
	return []Playlist{{ID: SpotifyLikedID, Title: "Liked Songs"}, {ID: "private", Title: "Private"}}, nil
}
func (f *spotifyLibraryFixture) LibraryTracks(_ context.Context, id string) ([]Track, error) {
	f.trackCalls++
	return []Track{{ID: id, Provider: "spotify", Title: "Native"}}, nil
}
func TestSpotifyStoredLoginFallsBackToProtocolOnWebAPIDenial(t *testing.T) {
	for _, status := range []int{401, 403} {
		p := spotifyStoredTest(t)
		lib := &spotifyLibraryFixture{fakeSpotifyNative: &fakeSpotifyNative{done: make(chan struct{})}}
		p.native = lib
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer login5-test-bearer" {
				t.Error("missing login5 token")
			}
			w.WriteHeader(status)
		}))
		p.BaseURL = srv.URL
		playlists, err := p.Playlists(context.Background())
		if err != nil || len(playlists) != 2 || lib.playlistCalls != 1 {
			t.Fatal("stored-credential rootlist unavailable after Web API denial")
		}
		for _, id := range []string{SpotifyLikedID, "private"} {
			tracks, err := p.Tracks(context.Background(), id)
			if err != nil || len(tracks) != 1 || tracks[0].ID != id {
				t.Fatal("native playlist or collection fallback missing")
			}
		}
		for _, bad := range []string{"../evil", "a/b", "foo?bar", ""} {
			if _, err := p.nativeTracks(context.Background(), bad); err == nil || lib.trackCalls != 2 {
				t.Fatalf("invalid playlist ID %q reached native protocol", bad)
			}
		}
		srv.Close()
	}
}

func TestSpotifyLibraryBatchedMetadataAndCap(t *testing.T) {
	ids := make([]librespot.SpotifyId, 5120)
	for i := range ids {
		var gid [16]byte
		binary.BigEndian.PutUint64(gid[8:], uint64(i+1))
		ids[i] = librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, gid[:])
	}
	var batchCalls int
	tracks, err := resolveSpotifyLibraryTracks(context.Background(), ids, func(_ context.Context, req *extpb.BatchedEntityRequest) (*extpb.BatchedExtensionResponse, error) {
		batchCalls++
		if len(req.EntityRequest) > spotifyMetadataBatchSize {
			t.Fatalf("batch size %d exceeded %d", len(req.EntityRequest), spotifyMetadataBatchSize)
		}
		data := make([]*extpb.EntityExtensionData, len(req.EntityRequest))
		for i, er := range req.EntityRequest {
			name := er.EntityUri
			packed, err := anypb.New(&metapb.Track{Name: &name})
			if err != nil {
				t.Fatal(err)
			}
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
	if err != nil || len(tracks) != maxNativeLibraryTracks || batchCalls != maxNativeLibraryTracks/spotifyMetadataBatchSize {
		t.Fatalf("got %d tracks in %d batches, err=%v", len(tracks), batchCalls, err)
	}
	if tracks[0].ID != ids[0].Base62() || tracks[len(tracks)-1].ID != ids[maxNativeLibraryTracks-1].Base62() {
		t.Fatal("batched track order was not preserved")
	}
}

func TestSpotifyNilFormatAndTransientAudioFilesRetryAndLogout(t *testing.T) {
	gid := bytes.Repeat([]byte{7}, 16)
	if gotGID, gotFile := selectSpotifyFile(&metapb.Track{
		Gid:  gid,
		File: []*metapb.AudioFile{{FileId: bytes.Repeat([]byte{1}, 20)}},
	}); gotGID != nil || gotFile != nil {
		t.Fatal("nil AudioFile.Format matched enum 0")
	}

	id := librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, gid)
	var attempts int
	p := spotifyStoredTest(t)
	p.nativeFactory = func(context.Context) (spotifyNative, error) {
		return &callbackSpotifyNative{
			fakeSpotifyNative: &fakeSpotifyNative{done: make(chan struct{})},
			metadataFn: func(ctx context.Context, spotID librespot.SpotifyId) (*metapb.Track, error) {
				return resolveSpotifyMetadata(ctx, spotID, "US", func(_ context.Context, _ librespot.SpotifyId, kind extpb.ExtensionKind, out proto.Message) error {
					if kind == extpb.ExtensionKind_TRACK_V4 {
						proto.Merge(out, &metapb.Track{Gid: gid})
						return nil
					}
					attempts++
					if attempts == 1 {
						return &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}
					}
					proto.Merge(out, &filespb.AudioFilesExtensionResponse{
						Files: []*filespb.ExtendedAudioFile{{File: spotifyTestFile(metapb.AudioFile_OGG_VORBIS_320)}},
					})
					return nil
				})
			},
		}, nil
	}
	err := p.withNative(context.Background(), func(n spotifyNative) error {
		meta, err := n.Metadata(context.Background(), id)
		if err != nil {
			return err
		}
		if _, f := selectSpotifyFile(meta); f == nil {
			t.Fatal("missing audio file after reconnect")
		}
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("transient AUDIO_FILES transport error did not reconnect: attempts=%d err=%v", attempts, err)
	}

	dir := t.TempDir()
	nonEmptyDir := filepath.Join(dir, "creds-dir")
	if err := os.MkdirAll(filepath.Join(nonEmptyDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "spotify-token.json")
	if err := os.WriteFile(tokenFile, []byte(`{"access_token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sp := &Spotify{CredsPath: nonEmptyDir, TokenPath: tokenFile, Token: Token{AccessToken: "x"}}
	if err := sp.Logout(); err == nil {
		t.Fatal("expected CredsPath removal error")
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatal("Logout stopped on first error without removing TokenPath")
	}
}

type callbackSpotifyNative struct {
	*fakeSpotifyNative
	metadataFn func(context.Context, librespot.SpotifyId) (*metapb.Track, error)
}

func (n *callbackSpotifyNative) Metadata(ctx context.Context, id librespot.SpotifyId) (*metapb.Track, error) {
	return n.metadataFn(ctx, id)
}
