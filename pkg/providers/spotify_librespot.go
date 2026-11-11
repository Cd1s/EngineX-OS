package providers

// Only the protocol packages belong here. session/player pull in CGO decoders.
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
	"github.com/devgianlu/go-librespot/apresolve"
	"github.com/devgianlu/go-librespot/audio"
	"github.com/devgianlu/go-librespot/login5"
	authpb "github.com/devgianlu/go-librespot/proto/spotify"
	pbdata "github.com/devgianlu/go-librespot/proto/spotify/clienttoken/data/v0"
	pbhttp "github.com/devgianlu/go-librespot/proto/spotify/clienttoken/http/v0"
	storagepb "github.com/devgianlu/go-librespot/proto/spotify/download"
	credpb "github.com/devgianlu/go-librespot/proto/spotify/login5/v3/credentials"
	metapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/devgianlu/go-librespot/spclient"
	"google.golang.org/protobuf/proto"
)

type spotifyCredentials struct {
	Username string `json:"username"`
	Data     []byte `json:"credentials"`
	DeviceID string `json:"device_id"`
}

type spotifyNative interface {
	Done() <-chan struct{}
	Close()
	Bearer(context.Context) (string, error)
	Metadata(context.Context, librespot.SpotifyId) (*metapb.Track, error)
	Key(context.Context, []byte, []byte) ([]byte, error)
	Storage(context.Context, *metapb.AudioFile) (*storagepb.StorageResolveResponse, error)
}

type spotifyProtocol struct {
	username     string
	ap           *ap.Accesspoint
	login        *login5.Login5
	sp           *spclient.Spclient
	key          *audio.KeyProvider
	country      atomic.Pointer[string]
	countryReady chan struct{}
	countryOnce  sync.Once
}

func (n *spotifyProtocol) Done() <-chan struct{} { return n.ap.Done() }
func (n *spotifyProtocol) Close()                { n.ap.Close() }
func (n *spotifyProtocol) Bearer(ctx context.Context) (string, error) {
	return n.login.AccessToken()(ctx, false)
}
func (n *spotifyProtocol) Metadata(ctx context.Context, id librespot.SpotifyId) (*metapb.Track, error) {
	return resolveSpotifyMetadata(ctx, id, n.countryCode(ctx), n.sp.ExtendedMetadataSimple)
}
func (n *spotifyProtocol) Key(ctx context.Context, gid, file []byte) ([]byte, error) {
	return n.key.Request(ctx, gid, file)
}
func (n *spotifyProtocol) Storage(ctx context.Context, f *metapb.AudioFile) (*storagepb.StorageResolveResponse, error) {
	return n.sp.ResolveStorageInteractive(ctx, f.FileId, f.Format, false)
}

func (s *Spotify) deviceID() string {
	if s.DeviceID != "" {
		return s.DeviceID
	}
	stateDir := ""
	if s.CredsPath != "" {
		stateDir = filepath.Dir(s.CredsPath)
	}
	id, _ := LoadSpotifyDeviceID(stateDir)
	return id
}
func (s *Spotify) storedCredentials() (spotifyCredentials, error) {
	var c spotifyCredentials
	b, err := os.ReadFile(s.CredsPath)
	if err != nil {
		return c, err
	}
	if json.Unmarshal(b, &c) != nil || c.Username == "" || len(c.Data) == 0 || c.DeviceID != s.deviceID() {
		return c, errors.New("spotify: invalid stored credentials")
	}
	return c, nil
}
func (s *Spotify) HasStoredCredentials() bool { _, err := s.storedCredentials(); return err == nil }

func (s *Spotify) retrieveClientToken(ctx context.Context) (string, error) {
	b, err := proto.Marshal(&pbhttp.ClientTokenRequest{
		RequestType: pbhttp.ClientTokenRequestType_REQUEST_CLIENT_DATA_REQUEST,
		Request: &pbhttp.ClientTokenRequest_ClientData{ClientData: &pbhttp.ClientDataRequest{
			ClientId: librespot.ClientIdHex, ClientVersion: librespot.SpotifyLikeClientVersion(),
			Data: &pbhttp.ClientDataRequest_ConnectivitySdkData{ConnectivitySdkData: &pbdata.ConnectivitySdkData{
				DeviceId: s.deviceID(), PlatformSpecificData: librespot.GetPlatformSpecificData(),
			}},
		}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://clienttoken.spotify.com/v1/clienttoken", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/x-protobuf")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("User-Agent", librespot.UserAgent())
	resp, err := client(s.HTTP).Do(req)
	if err != nil {
		return "", errors.New("spotify: client token request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("spotify: client token HTTP %d", resp.StatusCode)
	}
	b, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out pbhttp.ClientTokenResponse
	if proto.Unmarshal(b, &out) != nil || out.GetGrantedToken().GetToken() == "" {
		return "", errors.New("spotify: client token refused")
	}
	return out.GetGrantedToken().GetToken(), nil
}

// connectProtocol is called with nativeMu held, so replacement/logout cannot race
// an in-flight authentication or resurrect a session after logout.
func (s *Spotify) connectProtocol(ctx context.Context, token Token, username string, blob []byte) (_ spotifyNative, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id := s.deviceID()
	raw, e := hex.DecodeString(id)
	if e != nil || len(raw) != 20 || id != strings.ToLower(id) {
		return nil, errors.New("spotify: invalid device ID")
	}
	log := &spotifyAPLogger{cancel: cancel} // No upstream messages reach application logs.
	resolver := apresolve.NewApResolver(log, client(s.HTTP), true)
	addr, err := resolver.GetAccesspoint(ctx)
	if err != nil {
		return nil, errors.New("spotify: accesspoint resolution failed")
	}
	a := ap.NewAccesspoint(log, addr, id)
	defer func() {
		if err != nil {
			a.Close()
		}
	}()
	switch {
	case blob != nil:
		err = a.ConnectBlob(ctx, username, blob)
	case token.AccessToken != "":
		if username == "" && token.ClientID != SpotifyBuiltinClientID {
			var me struct {
				ID string `json:"id"`
			}
			// Some device-flow tokens omit username; AP can also derive it from a token.
			if JSON(ctx, s.HTTP, "GET", s.base()+"/me", nil, http.Header{"Authorization": {"Bearer " + token.AccessToken}}, &me) == nil {
				username = me.ID
			}
		}
		err = a.ConnectSpotifyToken(ctx, username, token.AccessToken)
	default:
		var c spotifyCredentials
		c, err = s.storedCredentials()
		if err == nil {
			err = a.ConnectStored(ctx, c.Username, c.Data)
		}
	}
	if rejected := log.rejection(); rejected != nil {
		err = rejected
	}
	if err != nil {
		return nil, spotifyAPAuthenticationError(err, token.AccessToken != "" && !s.HasStoredCredentials())
	}
	n := &spotifyProtocol{ap: a, username: a.Username(), countryReady: make(chan struct{})}
	n.watchCountry(a.Receive(ap.PacketTypeCountryCode))
	ct, err := s.retrieveClientToken(ctx)
	if err != nil {
		return nil, err
	}
	l := login5.NewLogin5(log, client(s.HTTP), id, ct)
	if err = l.Login(ctx, &credpb.StoredCredential{Username: a.Username(), Data: a.StoredCredentials()}); err != nil {
		return nil, &spotifyAuthFailure{message: "spotify: login5 authentication failed", cause: err, permanent: func() bool { var rejected *login5.LoginError; return errors.As(err, &rejected) }()}
	}
	spaddr, err := resolver.GetSpclient(ctx)
	if err != nil {
		return nil, errors.New("spotify: storage resolution failed")
	}
	sp, err := spclient.NewSpclient(ctx, log, client(s.HTTP), spaddr, l.AccessToken(), id, ct)
	if err != nil {
		return nil, errors.New("spotify: storage initialization failed")
	}
	if err = SaveJSON(s.CredsPath, spotifyCredentials{a.Username(), a.StoredCredentials(), id}); err != nil {
		return nil, err
	}
	n.login = l
	n.sp = sp
	n.key = audio.NewAudioKeyProvider(log, a)
	return n, nil
}

func (s *Spotify) withNative(ctx context.Context, fn func(spotifyNative) error) error {
	s.mu.Lock()
	tok := s.Token
	s.mu.Unlock()
	if !s.HasStoredCredentials() && !tok.Expiry.IsZero() && time.Now().Add(30*time.Second).After(tok.Expiry) {
		if _, err := s.authorization(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		tok = s.Token
		s.mu.Unlock()
	}
	s.nativeMu.Lock()
	defer s.nativeMu.Unlock()
	if s.nativeClosed {
		return errors.New("spotify: session closed; login required")
	}
	s.mu.Lock()
	tok = s.Token
	s.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if s.native != nil {
			select {
			case <-s.native.Done():
				s.native.Close()
				s.native = nil
			default:
			}
		}
		if s.native == nil {
			loginToken := tok
			if s.HasStoredCredentials() {
				loginToken = Token{}
			}
			if loginToken.AccessToken == "" && !s.HasStoredCredentials() {
				return errors.New("spotify: please log in via Spotify Connect or QR code on the deck first")
			}
			var err error
			if s.nativeFactory != nil {
				s.native, err = s.nativeFactory(ctx)
			} else {
				s.native, err = s.connectProtocol(ctx, loginToken, tok.Username, nil)
			}
			if err != nil {
				return err
			}
		}
		err := fn(s.native)
		var ne net.Error
		if err == nil || (!errors.Is(err, ap.ErrAccesspointClosed) && !errors.As(err, &ne)) || ctx.Err() != nil {
			return err
		}
		s.native.Close()
		s.native = nil
		if attempt == 1 {
			return err
		}
	}
	return errors.New("spotify: reconnect failed")
}

// ConnectZeroconf accepts the decrypted outer DH payload. ConnectBlob implements
// the inner SHA1(deviceID)/PBKDF2/AES-192-ECB/XOR authentication envelope.
func (s *Spotify) ConnectZeroconf(ctx context.Context, username string, blob []byte) error {
	// Validate upstream's unchecked block/length parsing before calling it.
	if err := validateSpotifyInnerBlob(s.deviceID(), username, blob); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !s.nativeMu.TryLock() {
		return ErrSpotifyPairingBusy
	}
	defer s.nativeMu.Unlock()
	if s.nativeClosed {
		return errors.New("spotify: session closed")
	}
	n, err := s.connectProtocol(ctx, Token{}, username, blob)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.Token = Token{}
	if s.TokenPath != "" {
		err = os.Remove(s.TokenPath)
		if os.IsNotExist(err) {
			err = nil
		}
	}
	s.mu.Unlock()
	if err != nil {
		n.Close()
		return err
	}

	if s.native != nil {
		s.native.Close()
	}
	s.native = n
	return nil
}

// ExchangeOAuthCredentials turns streaming-scoped OAuth into reusable AP login.
// Custom HTTP endpoints retain the offline test/PKCE path.
func (s *Spotify) ExchangeOAuthCredentials(ctx context.Context) error {
	s.nativeMu.Lock()
	defer s.nativeMu.Unlock()
	if s.nativeClosed {
		return errors.New("spotify: session closed")
	}
	s.mu.Lock()
	t := s.Token
	s.mu.Unlock()
	if (s.BaseURL != "" && !strings.HasPrefix(s.BaseURL, "https://api.spotify.com")) || (s.AccountsURL != "" && s.AccountsURL != "https://accounts.spotify.com") {
		return nil
	}
	if t.Scope != "" && !strings.Contains(" "+t.Scope+" ", " streaming ") {
		return nil
	}
	if t.AccessToken == "" {
		return errors.New("spotify: login required")
	}
	n, err := s.connectProtocol(ctx, t, t.Username, nil)
	if err != nil {
		return err
	}
	if s.native != nil {
		s.native.Close()
	}
	s.native = n
	return nil
}
func (s *Spotify) AcceptToken(ctx context.Context, t Token) error {
	if err := s.SetToken(t); err != nil {
		return err
	}
	return s.ExchangeOAuthCredentials(ctx)
}
func (s *Spotify) Close() {
	s.nativeMu.Lock()
	defer s.nativeMu.Unlock()
	s.nativeClosed = true
	if s.native != nil {
		s.native.Close()
		s.native = nil
	}
}
func (s *Spotify) Logout() error {
	s.nativeMu.Lock()
	defer s.nativeMu.Unlock()
	s.nativeClosed = true
	if s.native != nil {
		s.native.Close()
		s.native = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Token = Token{}
	var errs []error
	for _, p := range []string{s.CredsPath, s.TokenPath} {
		if p != "" {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// selectSpotifyFile keeps the selected alternative's GID paired with its file.
func selectSpotifyFile(m *metapb.Track) ([]byte, *metapb.AudioFile) {
	if m == nil {
		return nil, nil
	}
	tracks := append([]*metapb.Track{m}, m.Alternative...)
	for _, t := range tracks {
		if t == nil || len(t.Gid) != 16 {
			continue
		}
		for _, format := range []metapb.AudioFile_Format{metapb.AudioFile_OGG_VORBIS_320, metapb.AudioFile_OGG_VORBIS_160, metapb.AudioFile_OGG_VORBIS_96} {
			for _, f := range t.File {
				if f != nil && f.Format != nil && f.GetFormat() == format && len(f.FileId) > 0 {
					return t.Gid, f
				}
			}
		}
	}
	return nil, nil
}
func (s *Spotify) downloadNativeAudio(ctx context.Context, trackID, dstDir string) (string, error) {
	// SpotifyIdFromUri is the v0.10.5 spelling of FromSpotifyUri.
	if len(trackID) != 22 {
		return "", errors.New("spotify: invalid track ID")
	}
	v, ok := new(big.Int).SetString(trackID, 62)
	if !ok || v.BitLen() > 128 {
		return "", errors.New("spotify: invalid track ID")
	}
	id, err := librespot.SpotifyIdFromUri("spotify:track:" + trackID)
	if err != nil {
		return "", errors.New("spotify: invalid track ID")
	}
	var key []byte
	var urls []string
	err = s.withNative(ctx, func(n spotifyNative) error {
		meta, err := n.Metadata(ctx, *id)
		if err != nil {
			return err
		}
		gid, file := selectSpotifyFile(meta)
		if file == nil {
			return errors.New("spotify: no Ogg Vorbis audio available")
		}
		key, err = n.Key(ctx, gid, file.FileId)
		var refused *audio.KeyProviderError
		if errors.As(err, &refused) {
			return fmt.Errorf("spotify: audio key refused (code %d; Spotify Premium account required)", refused.Code)
		}
		if err != nil {
			return err
		}
		if len(key) != 16 {
			return errors.New("spotify: invalid audio key")
		}
		storage, err := n.Storage(ctx, file)
		if err == nil {
			urls = storage.GetCdnurl()
		}
		return err
	})
	if err != nil {
		return "", err
	}
	last := errors.New("spotify: no CDN URL available")
	for _, u := range urls {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		body, err := s.fetchSpotifyCDN(ctx, u)
		if err != nil {
			last = err
			continue
		}
		dec, err := audio.NewAesAudioDecryptor(bytes.NewReader(body), key)
		if err != nil {
			return "", err
		}
		// The decryptor reads into the ciphertext buffer and XORs it in place.
		if _, err = dec.ReadAt(body, 0); err != nil {
			last = err
			continue
		}
		path, err := writeSpotifyAudio(ctx, body, trackID, dstDir)
		if err == nil {
			return path, nil
		}
		last = err
	}
	return "", last
}
func (s *Spotify) fetchSpotifyCDN(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, errors.New("spotify: invalid CDN URL")
	}
	s.mediaOnce.Do(func() { s.mediaHTTP = spotifyMediaClient(s.HTTP) })
	resp, err := s.mediaHTTP.Do(req)
	if err != nil {
		return nil, errors.New("spotify: CDN request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spotify: CDN HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (256<<20)+1))
	if len(body) > 256<<20 {
		return nil, errors.New("spotify: audio response too large")
	}
	return body, err
}

// Ensure the base64 encoding is bounded and block aligned before the upstream
// ConnectBlob decoder (which assumes well-formed input) can see LAN input.
func decodeSpotifyInnerBlob(blob []byte) ([]byte, error) {
	if len(blob) > 65536 {
		return nil, errors.New("spotify: malformed login blob")
	}
	raw, err := base64.StdEncoding.DecodeString(string(blob))
	if err != nil || len(raw) == 0 || len(raw)%16 != 0 {
		return nil, errors.New("spotify: malformed login blob")
	}
	return raw, nil
}

const spotifyLegacyTokenMessage = "spotify: legacy Web API token cannot decrypt native audio; please select 'EngineX OS' in Spotify Connect or re-scan the Spotify QR code once"

func spotifyAPAuthenticationError(err error, tokenWithoutStoredCredentials bool) error {
	var rejected *ap.AccesspointLoginError
	permanent := errors.As(err, &rejected)
	message := "spotify: accesspoint authentication failed"
	if tokenWithoutStoredCredentials && permanent && rejected.Message.GetErrorCode() == authpb.ErrorCode_BadCredentials {
		message = spotifyLegacyTokenMessage
	}
	return &spotifyAuthFailure{message: message, cause: err, permanent: permanent}
}
