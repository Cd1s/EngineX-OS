package providers

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	authpb "github.com/devgianlu/go-librespot/proto/spotify"
	"golang.org/x/crypto/pbkdf2"
)

// SpotifyConnectInfo represents the Zeroconf getInfo response payload.
type SpotifyConnectInfo struct {
	Status         int    `json:"status"`
	StatusString   string `json:"statusString"`
	SpotifyError   int    `json:"spotifyError"`
	Version        string `json:"version"`
	DeviceID       string `json:"deviceID"`
	RemoteName     string `json:"remoteName"`
	PublicKey      string `json:"publicKey"`
	DeviceType     string `json:"deviceType"`
	LibraryVersion string `json:"libraryVersion"`
	AccountReq     string `json:"accountReq"`
	TokenType      string `json:"tokenType"`
	Scope          string `json:"scope"`
}

// SpotifyConnectUser represents the Zeroconf addUser request payload.
type SpotifyConnectUser struct {
	Action      string `json:"action"`
	UserName    string `json:"userName"`
	Blob        string `json:"blob"`
	AccessToken string `json:"accessToken"`
	ClientKey   string `json:"clientKey"`
}

// SpotifyConnectResponse represents standard response for Spotify Connect operations.
type SpotifyConnectResponse struct {
	Status       int    `json:"status"`
	StatusString string `json:"statusString"`
	SpotifyError int    `json:"spotifyError"`
}

// spotifyDHPrime is the 768-bit Oakley group 1 prime (generator 2) that Spotify's
// Zeroconf login uses: getInfo's publicKey is g^x mod p, 96 bytes, base64.
var spotifyDHPrime, _ = new(big.Int).SetString("ffffffffffffffffc90fdaa22168c234c4c6628b80dc1cd129024e088a67cc74020bbea63b139b22514a08798e3404ddef9519b3cd3a431b302b0a6df25f14374fe1356d6d51c245e485b576625e7ec6f44c42e9a63a3620ffffffffffffffff", 16)

// SpotifyConnectKey is this process's Zeroconf key pair.
type SpotifyConnectKey struct {
	priv *big.Int
	pub  []byte
}

var spotifyConnectKey = sync.OnceValue(func() *SpotifyConnectKey {
	priv, err := rand.Int(rand.Reader, spotifyDHPrime)
	if err != nil {
		panic(err) // crypto/rand failing is not recoverable
	}
	return &SpotifyConnectKey{priv, new(big.Int).Exp(big.NewInt(2), priv, spotifyDHPrime).FillBytes(make([]byte, 96))}
})

// SpotifyConnectKeyPair returns the key pair, generated once per process.
func SpotifyConnectKeyPair() *SpotifyConnectKey { return spotifyConnectKey() }

// PublicKey is what getInfo advertises.
func (k *SpotifyConnectKey) PublicKey() string { return base64.StdEncoding.EncodeToString(k.pub) }

// DecryptBlob opens addUser's blob with the phone's clientKey: iv(16) | AES-128-CTR
// data | HMAC-SHA1(20), keyed from SHA1 of the shared secret. A wrong MAC is an error.
func (k *SpotifyConnectKey) DecryptBlob(clientKey, blob string) ([]byte, error) {
	ck, err := base64.StdEncoding.DecodeString(clientKey)
	raw, err2 := base64.StdEncoding.DecodeString(blob)
	if err != nil || err2 != nil || len(ck) == 0 || len(ck) > 96 || len(raw) < 16+20 {
		return nil, errors.New("spotify connect: malformed blob")
	}
	remote := new(big.Int).SetBytes(ck)
	if remote.Cmp(big.NewInt(1)) <= 0 || remote.Cmp(new(big.Int).Sub(spotifyDHPrime, big.NewInt(1))) >= 0 {
		return nil, errors.New("spotify connect: invalid client key")
	}
	shared := new(big.Int).Exp(remote, k.priv, spotifyDHPrime)
	base := sha1.Sum(shared.Bytes())
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha1.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	iv, data, sum := raw[:16], raw[16:len(raw)-20], raw[len(raw)-20:]
	h := hmac.New(sha1.New, mac(base[:16], "checksum"))
	h.Write(data)
	if subtle.ConstantTimeCompare(h.Sum(nil), sum) != 1 {
		return nil, errors.New("spotify connect: blob checksum mismatch")
	}
	block, err := aes.NewCipher(mac(base[:16], "encryption")[:16])
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCTR(block, iv).XORKeyStream(out, data)
	return out, nil
}

var spotifyTrackIDRe = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// spotifyIV is the constant 16-byte IV for Spotify audio CTR decryption: 72e067fbddcbcf77ebe8bc643f630d93
var spotifyIV = []byte{
	0x72, 0xe0, 0x67, 0xfb, 0xdd, 0xcb, 0xcf, 0x77,
	0xeb, 0xe8, 0xbc, 0x64, 0x3f, 0x63, 0x0d, 0x93,
}

// DownloadAudio fetches audio stream for trackID and writes it to dstDir as spotify-<trackID>.ogg.
func (p *Spotify) DownloadAudio(ctx context.Context, trackID, dstDir string) (string, error) {
	if p == nil {
		return "", errors.New("spotify: please log in via Spotify Connect or QR code on the deck first")
	}
	if !spotifyTrackIDRe.MatchString(trackID) {
		return "", errors.New("spotify: invalid track ID")
	}
	if p.StreamURL == "" && (p.BaseURL == "" || strings.HasPrefix(p.BaseURL, "https://api.spotify.com")) {
		return p.downloadNativeAudio(ctx, trackID, dstDir)
	}
	p.mu.Lock()
	expired := p.Token.RefreshToken != "" && !p.Token.Expiry.IsZero() && time.Now().Add(30*time.Second).After(p.Token.Expiry)
	p.mu.Unlock()
	if expired {
		// authorization renews the token; an expired one would only earn a 401.
		if _, err := p.authorization(ctx); err != nil {
			return "", err
		}
	}
	p.mu.Lock()
	tok := p.Token.AccessToken
	p.mu.Unlock()
	if tok == "" {
		return "", errors.New("spotify: please log in via Spotify Connect or QR code on the deck first")
	}

	if !spotifyTrackIDRe.MatchString(trackID) {
		return "", errors.New("spotify: invalid track ID")
	}
	streamBase := p.StreamURL
	if streamBase == "" {
		base := p.BaseURL
		if base == "" {
			base = p.base()
		}
		streamBase = strings.TrimRight(base, "/") + "/audio/" + url.PathEscape(trackID)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", streamBase, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := client(p.HTTP).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("spotify: audio request failed (%d)", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20+1))
	if err != nil {
		return "", err
	}
	if len(body) > 256<<20 {
		return "", errors.New("spotify: audio response too large")
	}

	var audioKey []byte
	if headerKey := resp.Header.Get("X-Spotify-Audio-Key"); headerKey != "" {
		if k, err := hex.DecodeString(strings.TrimSpace(headerKey)); err == nil {
			audioKey = k
		}
	}
	if len(audioKey) == 0 && len(p.AudioKey) > 0 {
		audioKey = p.AudioKey
	}

	if len(audioKey) > 0 {
		block, err := aes.NewCipher(audioKey)
		if err != nil {
			return "", err
		}
		stream := cipher.NewCTR(block, spotifyIV)
		stream.XORKeyStream(body, body)
	}

	return writeSpotifyAudio(ctx, body, trackID, dstDir)
}

func writeSpotifyAudio(ctx context.Context, body []byte, trackID, dstDir string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	body = stripSpotifyOggHeader(body)
	if len(body) < 4 || string(body[:4]) != "OggS" {
		return "", errors.New("spotify: decrypted audio is not Ogg Vorbis")
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}

	finalPath := filepath.Join(dstDir, "spotify-"+trackID+".ogg")
	tmp, err := os.CreateTemp(dstDir, ".part-spotify-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, finalPath); err != nil {
		return "", err
	}
	tmpName = ""
	return finalPath, nil
}

// Validate the inner record before handing it to upstream's permissive parser.
func validateSpotifyInnerBlob(deviceID, username string, blob []byte) error {
	raw, err := decodeSpotifyInnerBlob(blob)
	if err != nil {
		return err
	}
	secret := sha1.Sum([]byte(deviceID))
	base := pbkdf2.Key(secret[:], []byte(username), 256, 20, sha1.New)
	digest := sha1.Sum(base)
	key := make([]byte, 24)
	copy(key, digest[:])
	binary.BigEndian.PutUint32(key[20:], 20)
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	for i := 0; i < len(raw); i += 16 {
		block.Decrypt(raw[i:i+16], raw[i:i+16])
	}
	for i := len(raw) - 1; i >= 16; i-- {
		raw[i] ^= raw[i-16]
	}
	r := bytes.NewReader(raw)
	bad := errors.New("spotify: malformed login blob")
	if _, err = r.ReadByte(); err != nil {
		return bad
	}
	n, err := binary.ReadUvarint(r)
	if err != nil || n > uint64(r.Len()) {
		return bad
	}
	r.Seek(int64(n), io.SeekCurrent)
	if _, err = r.ReadByte(); err != nil {
		return bad
	}
	typ, err := binary.ReadUvarint(r)
	if err != nil || typ > uint64(authpb.AuthenticationType_AUTHENTICATION_FACEBOOK_TOKEN) {
		return bad
	}
	if _, err = r.ReadByte(); err != nil {
		return bad
	}
	n, err = binary.ReadUvarint(r)
	if err != nil || n == 0 || n > uint64(r.Len()) {
		return bad
	}
	return nil
}

// Spotify's seek/replaygain packet is a separate Ogg page, not Vorbis audio.
func stripSpotifyOggHeader(body []byte) []byte {
	if len(body) >= 28 && string(body[:4]) == "OggS" {
		header := 27 + int(body[26])
		if len(body) >= header {
			payload := 0
			for _, n := range body[27:header] {
				payload += int(n)
			}
			page := header + payload
			if payload > 0 && len(body) >= page+4 && body[header] == 0x81 && string(body[page:page+4]) == "OggS" {
				return body[page:]
			}
		}
	} else if len(body) >= 0xab && string(body[0xa7:0xab]) == "OggS" {
		return body[0xa7:]
	}
	return body
}
