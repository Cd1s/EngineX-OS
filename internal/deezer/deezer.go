// Package deezer implements direct Deezer login and streaming track decryption.
package deezer

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"
)

import "golang.org/x/crypto/blowfish"

const (
	deezerGW  = "https://www.deezer.com/ajax/gw-light.php"
	userAgent = "Mozilla/5.0 (X11; Linux armv7l) AppleWebKit/537.36 Chrome/130.0 Safari/537.36"
)

var arlRe = regexp.MustCompile(`^[0-9a-fA-F]{120,300}$`)

func ValidARL(a string) bool { return arlRe.MatchString(a) }

// Session is a logged-in Deezer web session derived from an ARL cookie.
type Session struct {
	hc           *http.Client
	apiToken     string
	licenseToken string
	UserID       string
	UserName     string
	Lossless     bool
	HQ           bool
}

func Login(ctx context.Context, arl string) (*Session, error) {
	if !ValidARL(arl) {
		return nil, errors.New("ARL 格式不对（应为 120~300 位十六进制）")
	}
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse("https://www.deezer.com/")
	jar.SetCookies(u, []*http.Cookie{{Name: "arl", Value: arl, Path: "/", Domain: ".deezer.com"}})
	s := &Session{hc: &http.Client{Timeout: 30 * time.Second, Jar: jar}}
	var r struct {
		Results struct {
			CheckForm string `json:"checkForm"`
			USER      struct {
				USERID   json.Number `json:"USER_ID"`
				BLOGNAME string      `json:"BLOG_NAME"`
				OPTIONS  struct {
					LicenseToken   string `json:"license_token"`
					WebHQ          bool   `json:"web_hq"`
					WebLossless    bool   `json:"web_lossless"`
					MobileLossless bool   `json:"mobile_lossless"`
				} `json:"OPTIONS"`
			} `json:"USER"`
		} `json:"results"`
	}
	q := url.Values{"method": {"deezer.getUserData"}, "input": {"3"}, "api_version": {"1.0"}, "api_token": {"null"}}
	if err := s.gw(ctx, q, nil, &r); err != nil {
		return nil, err
	}
	id, _ := r.Results.USER.USERID.Int64()
	if id <= 0 {
		return nil, errors.New("ARL 无效或已过期（Deezer 返回 USER_ID=0）")
	}
	s.UserID = strconv.FormatInt(id, 10)
	s.UserName = r.Results.USER.BLOGNAME
	s.apiToken = r.Results.CheckForm
	s.licenseToken = r.Results.USER.OPTIONS.LicenseToken
	s.Lossless = r.Results.USER.OPTIONS.WebLossless || r.Results.USER.OPTIONS.MobileLossless
	s.HQ = r.Results.USER.OPTIONS.WebHQ
	return s, nil
}

func (s *Session) gw(ctx context.Context, q url.Values, body any, out any) error {
	var rd io.Reader
	method := "GET"
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
		method = "POST"
	}
	req, _ := http.NewRequestWithContext(ctx, method, deezerGW+"?"+q.Encode(), rd)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("deezer gw: HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// download fetches and decrypts trackID into dst, best format first.
// It returns "flac" or "mp3".
func (s *Session) DownloadTrack(ctx context.Context, trackID, dst string, opts DownloadOptions) (string, error) {
	var song struct {
		Error   json.RawMessage `json:"error"`
		Results struct {
			SNGID      json.Number `json:"SNG_ID"`
			TrackToken string      `json:"TRACK_TOKEN"`
			Fallback   *struct {
				SNGID      json.Number `json:"SNG_ID"`
				TrackToken string      `json:"TRACK_TOKEN"`
			} `json:"FALLBACK"`
		} `json:"results"`
	}
	q := url.Values{"method": {"song.getData"}, "input": {"3"}, "api_version": {"1.0"}, "api_token": {s.apiToken}}
	if err := s.gw(ctx, q, map[string]string{"sng_id": trackID}, &song); err != nil {
		return "", err
	}
	sngID, token := song.Results.SNGID.String(), song.Results.TrackToken
	if token == "" {
		return "", errors.New("deezer: no track token")
	}
	try := func(sngID, token string) (string, error) {
		srcURL, format, err := s.mediaURL(ctx, token, opts)
		if err != nil {
			return "", err
		}
		return format, s.fetchDecrypt(ctx, srcURL, sngID, dst)
	}
	format, err := try(sngID, token)
	if err != nil && song.Results.Fallback != nil && song.Results.Fallback.TrackToken != "" {
		format, err = try(song.Results.Fallback.SNGID.String(), song.Results.Fallback.TrackToken)
	}
	return format, err
}

func (s *Session) mediaURL(ctx context.Context, trackToken string, opts DownloadOptions) (string, string, error) {
	formats := s.formats(opts)
	if len(formats) == 0 {
		return "", "", errors.New("deezer: account lacks requested audio quality")
	}
	body, _ := json.Marshal(map[string]any{
		"license_token": s.licenseToken,
		"media":         []any{map[string]any{"type": "FULL", "formats": formats}},
		"track_tokens":  []string{trackToken},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://media.deezer.com/v1/get_url", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("deezer media: HTTP %d", resp.StatusCode)
	}
	var r struct {
		Data []struct {
			Media []struct {
				Format  string `json:"format"`
				Sources []struct {
					URL string `json:"url"`
				} `json:"sources"`
			} `json:"media"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return "", "", fmt.Errorf("deezer media: %v", err)
	}
	for _, d := range r.Data {
		for _, m := range d.Media {
			allowed := false
			for _, wanted := range formats {
				if wanted["format"] == m.Format {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
			if len(m.Sources) > 0 {
				f := "mp3"
				if m.Format == "FLAC" {
					f = "flac"
				}
				return m.Sources[0].URL, f, nil
			}
		}
		if len(d.Errors) > 0 {
			return "", "", fmt.Errorf("deezer media: %s", d.Errors[0].Message)
		}
	}
	return "", "", errors.New("deezer media: no source (ARL lacks rights?)")
}

func blowfishKey(sngID string) []byte {
	const secret = "g4el58wc0zvf9na1"
	sum := md5.Sum([]byte(sngID))
	h := hex.EncodeToString(sum[:])
	key := make([]byte, 16)
	for i := range key {
		key[i] = h[i] ^ h[i+16] ^ secret[i]
	}
	return key
}

func (s *Session) fetchDecrypt(ctx context.Context, srcURL, sngID, dst string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", srcURL, nil)
	if err != nil {
		return errors.New("deezer: invalid media URL")
	}
	req.Header.Set("User-Agent", userAgent)
	hc := &http.Client{Timeout: 5 * time.Minute, Transport: s.hc.Transport}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("deezer cdn: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	cw := &countWriter{w: f}
	if err := decryptStripe(transportReader{resp.Body}, cw, blowfishKey(sngID)); err != nil {
		return err
	}
	if cw.n == 0 || (resp.ContentLength >= 0 && cw.n != resp.ContentLength) {
		return errors.New("deezer: empty or truncated audio")
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// decryptStripe undoes Deezer's BF_CBC_STRIPE: every third 2048-byte chunk
// is Blowfish-CBC encrypted with a fixed IV.
func decryptStripe(r io.Reader, w io.Writer, key []byte) error {
	bf, err := blowfish.NewCipher(key)
	if err != nil {
		return err
	}
	iv := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	buf := make([]byte, 2048)
	for i := 0; ; i++ {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			chunk := buf[:n]
			if i%3 == 0 && n == 2048 {
				cipher.NewCBCDecrypter(bf, iv).CryptBlocks(chunk, chunk)
			}
			if _, werr := w.Write(chunk); werr != nil {
				return werr
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// DownloadOptions chooses the source format without transcoding. Bitrate uses
// Deemix values 9 (FLAC), 3 (MP3 320), and 1 (MP3 128); zero selects FLAC.
type DownloadOptions struct {
	Bitrate    int
	AllowLossy bool
}

func (s *Session) formats(opts DownloadOptions) []map[string]string {
	var out []map[string]string
	add := func(f string) { out = append(out, map[string]string{"cipher": "BF_CBC_STRIPE", "format": f}) }
	if opts.Bitrate == 0 || opts.Bitrate == 9 {
		if s.Lossless {
			add("FLAC")
		}
		if !opts.AllowLossy {
			return out
		}
	}
	if opts.Bitrate != 1 && s.HQ {
		add("MP3_320")
	}
	if opts.Bitrate == 3 && !opts.AllowLossy {
		return out
	}
	add("MP3_128")
	return out
}

type countWriter struct {
	w io.Writer
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

// io.ReadFull also uses ErrUnexpectedEOF for a legitimate final partial
// stripe. Preserve a transport's own ErrUnexpectedEOF as a hard failure.
type transportReader struct{ io.Reader }

func (r transportReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.ErrUnexpectedEOF {
		err = errors.New("deezer: truncated audio stream")
	}
	return n, err
}
