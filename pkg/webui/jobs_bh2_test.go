package webui

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func minimalADTSFrame() []byte {
	// Valid 7-byte ADTS header (44.1 kHz index 4, stereo, frame length 8) + 1 payload byte.
	const frameLen = 8
	return []byte{
		0xff, 0xf1,
		(1 << 6) | (4 << 2),
		(2 << 6) | byte(frameLen>>11),
		byte(frameLen >> 3),
		byte((frameLen&7)<<5) | 0x1f,
		0xfc,
		0x21,
	}
}

func pkcs7Pad(b []byte) []byte {
	pad := aes.BlockSize - (len(b) % aes.BlockSize)
	out := make([]byte, len(b)+pad)
	copy(out, b)
	for i := len(b); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}

func TestSaveBeatportStreamToleratesFATChmodEPERM(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := make([]byte, aes.BlockSize)
	plain := pkcs7Pad(minimalADTSFrame())
	enc := make([]byte, len(plain))
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, plain)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog/tracks/777/stream/":
			w.Write([]byte(`{"stream_url":"` + srv.URL + `/hls/index.m3u8"}`))
		case "/hls/index.m3u8":
			w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-KEY:METHOD=AES-128,URI=\"" + srv.URL + "/hls/key.bin\"\n#EXTINF:0.02,\n" + srv.URL + "/hls/seg0.aac\n#EXT-X-ENDLIST\n"))
		case "/hls/key.bin":
			w.Write(key)
		case "/hls/seg0.aac":
			w.Write(enc)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	origChmod := chmodJobFile
	chmodJobFile = func(name string, mode os.FileMode) error {
		return syscall.EPERM
	}
	t.Cleanup(func() { chmodJobFile = origChmod })

	dir := t.TempDir()
	j := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: dir, AllowLossy: true}})
	defer j.Close()

	bpPath := filepath.Join(t.TempDir(), "beatport.json")
	if err := os.WriteFile(bpPath, []byte(`{"access_token":"tok","expires_at":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bp := &providers.Beatport{BaseURL: srv.URL, HTTP: srv.Client(), Path: bpPath}

	dl, err := j.saveBeatportStream(context.Background(), bp, "beatport-777", "777")
	if err != nil {
		t.Fatalf("saveBeatportStream failed on FAT/exFAT chmod EPERM: %v", err)
	}
	if st, err := os.Stat(dl.Path); err != nil || st.Size() == 0 {
		t.Fatalf("expected saved mp4 at %s: %v", dl.Path, err)
	}
}
