// Package deezer implements direct Deezer login and streaming track decryption.
package deezer

import (
	"bufio"
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
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

import "golang.org/x/crypto/blowfish"

const (
	deezerGW       = "https://www.deezer.com/ajax/gw-light.php"
	userAgent      = "Mozilla/5.0 (X11; Linux armv7l) AppleWebKit/537.36 Chrome/130.0 Safari/537.36"
	stripeSize     = 2048
	stripePeriod   = 3 * stripeSize
	splitBytes     = 128 * stripePeriod // 786432 bytes (~768 KiB), aligned to BF_CBC_STRIPE period
	partsAtOnce    = 16
	maxAudioBytes  = 512 << 20
	cdnPartTimeout = 60 * time.Second
)

// ErrInvalidARL reports that Deezer rejected the ARL (USER_ID=0).
var ErrInvalidARL = errors.New("ARL 无效或已过期（Deezer 返回 USER_ID=0）")

var (
	origDefaultTransport = http.DefaultTransport
	defaultCDNClient     = func() *http.Client {
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			t := base.Clone()
			t.ResponseHeaderTimeout = 30 * time.Second
			t.MaxIdleConnsPerHost = partsAtOnce
			return &http.Client{Transport: t}
		}
		return &http.Client{}
	}()
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
		return nil, ErrInvalidARL
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
	try := func(sngID, token string) (string, error) {
		if token == "" || sngID == "" {
			return "", errors.New("deezer: no track token")
		}
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

type byteRange struct {
	from, to int64 // inclusive
}

func (s *Session) cdnClient() *http.Client {
	if s.hc != nil && s.hc.Transport != nil {
		return &http.Client{Transport: s.hc.Transport}
	}
	if http.DefaultTransport != origDefaultTransport {
		return &http.Client{Transport: http.DefaultTransport}
	}
	return defaultCDNClient
}

func (s *Session) fetchDecrypt(ctx context.Context, srcURL, sngID, dst string) error {
	if _, err := http.NewRequestWithContext(ctx, "GET", srcURL, nil); err != nil {
		return errors.New("deezer: invalid media URL")
	}
	key := blowfishKey(sngID)
	bf, err := blowfish.NewCipher(key)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".part-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	bw := bufio.NewWriterSize(f, 256*1024)
	if err := s.downloadTo(ctx, srcURL, key, bf, bw); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func (s *Session) downloadTo(ctx context.Context, srcURL string, key []byte, bf cipher.Block, w io.Writer) error {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", srcURL, nil)
	if err != nil {
		return errors.New("deezer: invalid media URL")
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", splitBytes-1))
	resp, err := s.cdnClient().Do(req)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusRequestedRangeNotSatisfiable:
		resp.Body.Close()
		return s.downloadSingle(ctx, srcURL, key, w)
	case http.StatusOK:
		defer resp.Body.Close()
		return decryptResponse(resp, key, w)
	case http.StatusPartialContent:
	default:
		resp.Body.Close()
		return fmt.Errorf("deezer cdn: HTTP %d", resp.StatusCode)
	}
	var from, to, total int64
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &from, &to, &total); err != nil || from != 0 || to < 0 || to >= total || total <= 0 || total > maxAudioBytes {
		resp.Body.Close()
		return errors.New("deezer: unexpected byte range")
	}
	// Parallel decryption requires each subsequent part to begin on a 6144-byte
	// BF_CBC_STRIPE period boundary. If the CDN clamped the first range off-grid,
	// fall back cleanly to a plain sequential stream.
	if to+1 < total && (to+1)%stripePeriod != 0 {
		resp.Body.Close()
		return s.downloadSingle(ctx, srcURL, key, w)
	}
	first, err := io.ReadAll(io.LimitReader(transportReader{resp.Body}, to+2))
	resp.Body.Close()
	if err != nil || int64(len(first)) != to+1 {
		return errors.New("deezer: empty or truncated audio")
	}
	decryptAligned(first, bf)
	if _, err := w.Write(first); err != nil {
		return err
	}
	if to+1 == total {
		return nil
	}
	var parts []byteRange
	for at := to + 1; at < total; at += splitBytes {
		parts = append(parts, byteRange{at, min(at+splitBytes, total) - 1})
	}
	return s.fetchRangedParts(ctx, srcURL, bf, w, parts)
}

func (s *Session) downloadSingle(ctx context.Context, srcURL string, key []byte, w io.Writer) error {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", srcURL, nil)
	if err != nil {
		return errors.New("deezer: invalid media URL")
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.cdnClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deezer cdn: HTTP %d", resp.StatusCode)
	}
	return decryptResponse(resp, key, w)
}

func decryptResponse(resp *http.Response, key []byte, w io.Writer) error {
	cw := &countWriter{w: w}
	br := bufio.NewReaderSize(io.LimitReader(transportReader{resp.Body}, maxAudioBytes+1), 64*1024)
	if err := decryptStripe(br, cw, key); err != nil {
		return err
	}
	if cw.n == 0 || cw.n > maxAudioBytes || (resp.ContentLength >= 0 && cw.n != resp.ContentLength) {
		return errors.New("deezer: empty or truncated audio")
	}
	return nil
}

func (s *Session) fetchRangedParts(ctx context.Context, srcURL string, bf cipher.Block, w io.Writer, parts []byteRange) error {
	partCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	client := s.cdnClient()
	type answer struct {
		data []byte
		err  error
	}
	answers := make([]chan answer, len(parts))
	for i := range answers {
		answers[i] = make(chan answer, 1)
	}
	slots := make(chan struct{}, partsAtOnce)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i, p := range parts {
			select {
			case slots <- struct{}{}:
			case <-partCtx.Done():
				return
			}
			wg.Add(1)
			go func(idx int, pr byteRange) {
				defer wg.Done()
				data, err := fetchRangedPart(partCtx, client, srcURL, pr)
				if err == nil {
					decryptAligned(data, bf)
				}
				answers[idx] <- answer{data, err}
			}(i, p)
		}
	}()
	for i := range parts {
		var a answer
		select {
		case a = <-answers[i]:
		case <-partCtx.Done():
			return partCtx.Err()
		}
		if a.err != nil {
			return a.err
		}
		if _, err := w.Write(a.data); err != nil {
			return err
		}
		<-slots
	}
	return nil
}

func fetchRangedPart(ctx context.Context, client *http.Client, srcURL string, p byteRange) ([]byte, error) {
	wantLen := p.to - p.from + 1
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, cdnPartTimeout)
		req, err := http.NewRequestWithContext(attemptCtx, "GET", srcURL, nil)
		if err != nil {
			cancel()
			return nil, errors.New("deezer: invalid media URL")
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", p.from, p.to))
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			last = err
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(transportReader{resp.Body}, wantLen+1))
		resp.Body.Close()
		cancel()
		switch {
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			return nil, fmt.Errorf("deezer cdn: HTTP %d", resp.StatusCode)
		case resp.StatusCode != http.StatusPartialContent:
			last = fmt.Errorf("deezer cdn: HTTP %d", resp.StatusCode)
		case rerr != nil || int64(len(data)) != wantLen || (resp.ContentLength >= 0 && resp.ContentLength != wantLen):
			last = errors.New("deezer: empty or truncated audio")
		default:
			if cr := resp.Header.Get("Content-Range"); cr != "" {
				var from, to, total int64
				if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &from, &to, &total); err != nil || from != p.from || to != p.to {
					last = errors.New("deezer: unexpected byte range")
					continue
				}
			}
			return data, nil
		}
	}
	return nil, last
}

// decryptAligned decrypts a buffer starting on a stripePeriod (6144-byte) boundary.
func decryptAligned(b []byte, bf cipher.Block) {
	iv := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	for off := 0; off+stripeSize <= len(b); off += stripePeriod {
		cipher.NewCBCDecrypter(bf, iv).CryptBlocks(b[off:off+stripeSize], b[off:off+stripeSize])
	}
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
