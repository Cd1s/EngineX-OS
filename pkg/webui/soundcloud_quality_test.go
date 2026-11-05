package webui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func testMP3(frames int) []byte {
	var b bytes.Buffer
	for i := 0; i < frames; i++ {
		b.Write([]byte{0xff, 0xfb, 0x90, 0}) // MPEG1 Layer III, 128 kbit/s, 44.1 kHz
		b.Write(make([]byte, 144*128000/44100-4))
	}
	return b.Bytes()
}

// The deck's Streaming Quality decides whether SoundCloud's original upload is tried.
func TestSoundCloudFollowsStreamingQuality(t *testing.T) {
	stream := testMP3(200)
	original := append([]byte("fLaC-but-broken"), make([]byte, 64)...)
	var originals int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tracks/5":
			w.Write([]byte(`{"id":5,"access":"playable","downloadable":true,"download_url":"x"}`))
		case "/tracks/5/download":
			http.Redirect(w, r, srv.URL+"/cdn/original", http.StatusFound)
		case "/tracks/5/streams":
			w.Write([]byte(`{"http_mp3_128_url":"` + srv.URL + `/stream"}`))
		case "/stream":
			http.Redirect(w, r, srv.URL+"/cdn/stream.mp3", http.StatusFound)
		case "/cdn/original":
			originals++
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(original))
		case "/cdn/stream.mp3":
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(stream))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	mp3 := true
	d := &deemixclient.Client{DownloadDir: dir, AllowLossy: true, PreferMP3: func() bool { return mp3 }}
	j := NewJobs(Pipeline{Deemix: d})
	defer j.Close()
	sc := &providers.SoundCloud{BaseURL: srv.URL, ClientID: "cid", Secret: "shh", Token: "t"}
	j.soundcloud = func() *providers.SoundCloud { return sc }
	track := providers.Track{ID: "5", Provider: "soundcloud"}

	dl, err := j.fromSoundCloud(context.Background(), track)
	if err != nil || filepath.Base(dl.Path) != "soundcloud-5.mp3" || originals != 0 {
		t.Fatalf("320kbps: %+v %v originals=%d", dl, err, originals)
	}
	// Lossless tries the original; an unusable one leaves the stream already on disk.
	mp3 = false
	dl, err = j.fromSoundCloud(context.Background(), track)
	if err != nil || filepath.Base(dl.Path) != "soundcloud-5.mp3" || originals != 1 {
		t.Fatalf("lossless: %+v %v originals=%d", dl, err, originals)
	}
	if got, _ := os.ReadFile(dl.Path); !bytes.Equal(got, stream) {
		t.Fatal("stream replaced")
	}
}
