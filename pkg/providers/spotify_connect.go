package providers

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	p.mu.Lock()
	tok := p.Token.AccessToken
	p.mu.Unlock()
	if tok == "" {
		return "", errors.New("spotify: please log in via Spotify Connect or QR code on the deck first")
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

	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("spotify: audio request failed (%d)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
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
		dec := make([]byte, len(body))
		stream.XORKeyStream(dec, body)
		body = dec
	}

	// If decrypted (or unencrypted) data does not start with "OggS", has length >= 0xab (171),
	// and buf[0xa7:0xab] == "OggS", strip the first 0xa7 (167) bytes private header.
	if len(body) >= 0xab && string(body[:4]) != "OggS" && string(body[0xa7:0xab]) == "OggS" {
		body = body[0xa7:]
	}

	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}

	finalPath := filepath.Join(dstDir, "spotify-"+trackID+".ogg")
	tmp, err := os.CreateTemp(dstDir, ".tmp-spotify-*")
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

	if err := os.Rename(tmpName, finalPath); err != nil {
		return "", err
	}
	tmpName = ""
	return finalPath, nil
}
