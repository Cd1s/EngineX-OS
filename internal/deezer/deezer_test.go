package deezer

import (
	"bytes"
	"context"
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/blowfish"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecryptStripe(t *testing.T) {
	key := blowfishKey("3135556")
	plain := bytes.Repeat([]byte("0123456789abcdef"), 2048*7/16+5)
	enc := append([]byte(nil), plain...)
	bf, _ := blowfish.NewCipher(key)
	for i := 0; i*2048+2048 <= len(enc); i++ {
		if i%3 == 0 {
			c := enc[i*2048 : i*2048+2048]
			cipher.NewCBCEncrypter(bf, []byte{0, 1, 2, 3, 4, 5, 6, 7}).CryptBlocks(c, c)
		}
	}
	var out bytes.Buffer
	if err := decryptStripe(bytes.NewReader(enc), &out, key); err != nil || !bytes.Equal(out.Bytes(), plain) {
		t.Fatalf("roundtrip failed: %v", err)
	}
}

// The complete login -> song metadata -> media -> encrypted CDN flow uses the
// same source downloader linked into the ARM daemon, with no ffmpeg process.
func TestDirectDownloadProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		opts         DownloadOptions
		lossless     bool
		wantErr      bool
	}{
		{"flac", "FLAC", DownloadOptions{Bitrate: 9, AllowLossy: true}, true, false},
		{"lossy-fallback", "MP3_320", DownloadOptions{Bitrate: 9, AllowLossy: true}, false, false},
		{"lossless-required", "MP3_320", DownloadOptions{Bitrate: 9}, false, true},
		{"reject-unrequested-format", "MP3_320", DownloadOptions{Bitrate: 9}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := append([]byte("fLaC"), bytes.Repeat([]byte("0123456789abcdef"), 1024)...)
			encrypted := append([]byte(nil), plain...)
			bf, _ := blowfish.NewCipher(blowfishKey("3135556"))
			for i := 0; i*2048+2048 <= len(encrypted); i++ {
				if i%3 == 0 {
					b := encrypted[i*2048 : i*2048+2048]
					cipher.NewCBCEncrypter(bf, []byte{0, 1, 2, 3, 4, 5, 6, 7}).CryptBlocks(b, b)
				}
			}
			transport := http.DefaultTransport
			defer func() { http.DefaultTransport = transport }()
			var seen []string
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				seen = append(seen, r.URL.Host+r.URL.Path)
				var body string
				switch r.URL.Host {
				case "www.deezer.com":
					switch r.URL.Query().Get("method") {
					case "deezer.getUserData":
						cookie, err := r.Cookie("arl")
						if err != nil || cookie.Value != strings.Repeat("a", 192) {
							t.Error("ARL cookie missing")
						}
						body = fmt.Sprintf(`{"results":{"checkForm":"csrf","USER":{"USER_ID":6850245583,"OPTIONS":{"license_token":"license","web_hq":true,"web_lossless":%t}}}}`, tc.lossless)
					case "song.getData":
						if r.URL.Query().Get("api_token") != "csrf" {
							t.Error("missing API token")
						}
						body = `{"results":{"SNG_ID":"3135556","TRACK_TOKEN":"track-token"}}`
					default:
						t.Fatal("unknown gateway request")
					}
				case "media.deezer.com":
					var req struct {
						LicenseToken string `json:"license_token"`
						Media        []struct {
							Type    string              `json:"type"`
							Formats []map[string]string `json:"formats"`
						} `json:"media"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Fatal(err)
					}
					if req.LicenseToken != "license" || len(req.Media) != 1 || req.Media[0].Type != "FULL" {
						t.Error("incorrect media request")
					}
					if !tc.opts.AllowLossy {
						for _, f := range req.Media[0].Formats {
							if f["format"] != "FLAC" {
								t.Error("lossy requested while forbidden")
							}
						}
					}
					body = fmt.Sprintf(`{"data":[{"media":[{"format":%q,"sources":[{"url":"https://cdn.deezer.com/audio"}]}]}]}`, tc.format)
				case "cdn.deezer.com":
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encrypted)), ContentLength: int64(len(encrypted)), Header: make(http.Header)}, nil
				default:
					t.Fatalf("unexpected destination: %s", r.URL.Host)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			session, err := Login(context.Background(), strings.Repeat("a", 192))
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "track.part")
			format, err := session.DownloadTrack(context.Background(), "3135556", dst, tc.opts)
			if tc.wantErr {
				if err == nil {
					t.Fatal("disallowed quality accepted")
				}
				return
			}
			expected := "mp3"
			if tc.format == "FLAC" {
				expected = "flac"
			}
			if err != nil || format != expected {
				t.Fatalf("download: %s %v", format, err)
			}
			b, err := os.ReadFile(dst)
			if err != nil || !bytes.Equal(b, plain) {
				t.Fatal("original audio not decrypted intact")
			}
			if len(seen) != 4 {
				t.Fatalf("unexpected requests: %v", seen)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRejectTruncatedCDN(t *testing.T) {
	s := &Session{hc: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("short")), ContentLength: 100, Header: make(http.Header)}, nil
	})}}
	if err := s.fetchDecrypt(context.Background(), "https://cdn.deezer.com/audio", "3135556", filepath.Join(t.TempDir(), "partial")); err == nil {
		t.Fatal("truncated response accepted")
	}
}

type truncatedBody struct{ io.Reader }

func (r truncatedBody) Close() error { return nil }
func (r truncatedBody) Read(p []byte) (int, error) {
	n, _ := r.Reader.Read(p)
	return n, io.ErrUnexpectedEOF
}

func TestRejectTruncatedChunkedCDN(t *testing.T) {
	s := &Session{hc: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: truncatedBody{strings.NewReader("short")}, ContentLength: -1, Header: make(http.Header)}, nil
	})}}
	if err := s.fetchDecrypt(context.Background(), "https://cdn.deezer.com/audio", "3135556", filepath.Join(t.TempDir(), "partial")); err == nil {
		t.Fatal("truncated chunked response accepted")
	}
}

func TestMP3QualityPolicy(t *testing.T) {
	s := &Session{HQ: true}
	formats := s.formats(DownloadOptions{Bitrate: 3})
	if len(formats) != 1 || formats[0]["format"] != "MP3_320" {
		t.Fatal("explicit 320 must not silently downgrade")
	}
	s.HQ = false
	if len(s.formats(DownloadOptions{Bitrate: 3})) != 0 {
		t.Fatal("unavailable 320 must fail without lossy fallback")
	}
	formats = s.formats(DownloadOptions{Bitrate: 3, AllowLossy: true})
	if len(formats) != 1 || formats[0]["format"] != "MP3_128" {
		t.Fatal("explicitly allowed fallback unavailable")
	}
}

func TestDownloadTrackUsesFallbackWhenPrimaryTrackTokenEmpty(t *testing.T) {
	plain := append([]byte("fLaC"), bytes.Repeat([]byte("0123456789abcdef"), 256)...)
	encrypted := append([]byte(nil), plain...)
	bf, _ := blowfish.NewCipher(blowfishKey("999999"))
	for i := 0; i*2048+2048 <= len(encrypted); i++ {
		if i%3 == 0 {
			b := encrypted[i*2048 : i*2048+2048]
			cipher.NewCBCEncrypter(bf, []byte{0, 1, 2, 3, 4, 5, 6, 7}).CryptBlocks(b, b)
		}
	}
	s := &Session{
		apiToken:     "csrf",
		licenseToken: "license",
		Lossless:     true,
		hc: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body string
			switch r.URL.Host {
			case "www.deezer.com":
				body = `{"results":{"SNG_ID":"111111","TRACK_TOKEN":"","FALLBACK":{"SNG_ID":"999999","TRACK_TOKEN":"fallback-token"}}}`
			case "media.deezer.com":
				body = `{"data":[{"media":[{"format":"FLAC","sources":[{"url":"https://cdn.deezer.com/audio"}]}]}]}`
			case "cdn.deezer.com":
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encrypted)), ContentLength: int64(len(encrypted)), Header: make(http.Header)}, nil
			default:
				t.Fatalf("unexpected host: %s", r.URL.Host)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}
	dst := filepath.Join(t.TempDir(), "fallback.part")
	format, err := s.DownloadTrack(context.Background(), "111111", dst, DownloadOptions{Bitrate: 9, AllowLossy: true})
	if err != nil || format != "flac" {
		t.Fatalf("DownloadTrack failed to use FALLBACK when primary TRACK_TOKEN is empty: format=%q err=%v", format, err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("fallback track decrypted payload mismatch")
	}
}
