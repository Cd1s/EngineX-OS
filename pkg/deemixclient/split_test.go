package deemixclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mewkiz/flac"
)

func pcmSamples(frames, channels, bits int) [][]int32 {
	out := make([][]int32, channels)
	peak := float64(int64(1)<<(bits-1) - 1)
	for c := range out {
		out[c] = make([]int32, frames)
		for i := range out[c] {
			v := 0.6*math.Sin(float64(i)*0.031*float64(c+1)) + 0.3*math.Sin(float64(i)*0.0007)
			out[c][i] = int32(v * peak)
		}
	}
	// The extremes must survive too.
	out[0][0], out[0][1] = int32(peak), -int32(peak)-1
	return out
}

func wavFile(s [][]int32, bits, rate int) []byte {
	var data bytes.Buffer
	for i := range s[0] {
		for c := range s {
			v := s[c][i]
			switch bits {
			case 16:
				binary.Write(&data, binary.LittleEndian, int16(v))
			case 24:
				data.Write([]byte{byte(v), byte(v >> 8), byte(v >> 16)})
			}
		}
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+8+data.Len()))
	b.WriteString("WAVE")
	// An unknown chunk before fmt is skipped.
	b.WriteString("LIST")
	binary.Write(&b, binary.LittleEndian, uint32(3))
	b.Write([]byte{1, 2, 3, 0})
	b.WriteString("fmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(len(s)), uint32(rate), uint32(rate * len(s) * bits / 8), uint16(len(s) * bits / 8), uint16(bits)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(data.Len()))
	b.Write(data.Bytes())
	return b.Bytes()
}

func aiffFile(s [][]int32, bits int) []byte {
	var data bytes.Buffer
	for i := range s[0] {
		for c := range s {
			v := s[c][i]
			data.Write([]byte{byte(v >> 16), byte(v >> 8), byte(v)}[3-bits/8:])
		}
	}
	var b bytes.Buffer
	b.WriteString("FORM")
	binary.Write(&b, binary.BigEndian, uint32(4+26+16+data.Len()))
	b.WriteString("AIFFCOMM")
	for _, v := range []any{uint32(18), uint16(len(s)), uint32(len(s[0])), uint16(bits)} {
		binary.Write(&b, binary.BigEndian, v)
	}
	b.Write([]byte{0x40, 0x0E, 0xAC, 0x44, 0, 0, 0, 0, 0, 0}) // 44100 as an 80-bit float
	b.WriteString("SSND")
	for _, v := range []any{uint32(8 + data.Len()), uint32(0), uint32(0)} {
		binary.Write(&b, binary.BigEndian, v)
	}
	b.Write(data.Bytes())
	return b.Bytes()
}

func flacSamples(t *testing.T, path string) (rate, bits int, s [][]int32) {
	t.Helper()
	stream, err := flac.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	s = make([][]int32, stream.Info.NChannels)
	for {
		f, err := stream.ParseNext()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for c, sf := range f.Subframes {
			s[c] = append(s[c], sf.Samples[:sf.NSamples]...)
		}
	}
	if uint64(len(s[0])) != stream.Info.NSamples {
		t.Fatalf("STREAMINFO says %d samples, frames hold %d", stream.Info.NSamples, len(s[0]))
	}
	return int(stream.Info.SampleRate), int(stream.Info.BitsPerSample), s
}

func equalSamples(a, b [][]int32) bool {
	if len(a) != len(b) {
		return false
	}
	for c := range a {
		if len(a[c]) != len(b[c]) {
			return false
		}
		for i := range a[c] {
			if a[c][i] != b[c][i] {
				return false
			}
		}
	}
	return true
}

// serve answers byte ranges like a CDN, counting the requests and how many ran at once.
func serve(t *testing.T, body []byte, ranges bool) (*httptest.Server, *int32, *int32) {
	var requests, running, most int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		now := atomic.AddInt32(&running, 1)
		defer atomic.AddInt32(&running, -1)
		for {
			m := atomic.LoadInt32(&most)
			if now <= m || atomic.CompareAndSwapInt32(&most, m, now) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		if !ranges {
			r.Header.Del("Range")
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &requests, &most
}

func TestSaveSplitTurnsAnOriginalWAVIntoTheSameSamplesAsFLAC(t *testing.T) {
	want := pcmSamples(700_000, 2, 16) // 2.8 MB, more than one range
	wav := wavFile(want, 16, 48000)
	srv, requests, _ := serve(t, wav, true)
	dir := t.TempDir()
	c := &Client{DownloadDir: dir}
	os.WriteFile(filepath.Join(dir, "soundcloud-5.mp3"), mp3Frames(9, 3), 0o644)
	dl, err := c.SaveSplit(context.Background(), "soundcloud-5", srv.URL+"/a.wav")
	if err != nil || filepath.Base(dl.Path) != "soundcloud-5.flac" || !dl.Quality.Lossless || dl.Quality.Bits != 16 || dl.Quality.SampleRate != 48000 {
		t.Fatalf("%+v %v", dl, err)
	}
	if *requests < 2 {
		t.Fatalf("%d requests: not split", *requests)
	}
	rate, bits, got := flacSamples(t, dl.Path)
	if rate != 48000 || bits != 16 || !equalSamples(got, want) {
		t.Fatalf("FLAC differs from the WAV (%d Hz %d bit)", rate, bits)
	}
	if st, _ := os.Stat(dl.Path); st.Size() >= int64(len(wav)) {
		t.Fatalf("FLAC %d bytes is no smaller than the WAV %d", st.Size(), len(wav))
	}
	if c.Cached("soundcloud-5") == nil || c.Cached("soundcloud-5").Quality.Format != "flac" {
		t.Fatal("the lossless file is not the cached one")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftovers %v", entries)
	}
}

func TestSaveSplitConvertsA24BitAIFFFromAHostWithoutRanges(t *testing.T) {
	want := pcmSamples(30_000, 1, 24)
	srv, requests, _ := serve(t, aiffFile(want, 24), false)
	c := &Client{DownloadDir: t.TempDir()}
	dl, err := c.SaveSplit(context.Background(), "soundcloud-6", srv.URL)
	if err != nil || dl.Quality.Format != "flac" || dl.Quality.Bits != 24 || *requests != 1 {
		t.Fatalf("%+v %v requests=%d", dl, err, *requests)
	}
	rate, _, got := flacSamples(t, dl.Path)
	if rate != 44100 || !equalSamples(got, want) {
		t.Fatalf("FLAC differs from the AIFF (%d Hz)", rate)
	}
}

func TestPCMToFLAC24BitAndHighResidualNo32BitOverflow(t *testing.T) {
	dir := t.TempDir()
	// 24-bit stereo WAV and 24-bit AIFF: on 32-bit armhf, mewkiz/flac's int bit-cost
	// accumulator overflowed negative at k=0 and inflated a 90 KB clip to >20 GiB.
	s24 := pcmSamples(8192, 2, 24)
	rawWAV := wavFile(s24, 24, 48000)
	wavPath := filepath.Join(dir, "in24.wav")
	if err := os.WriteFile(wavPath, rawWAV, 0o644); err != nil {
		t.Fatal(err)
	}
	flacWAV, err := pcmToFLAC(wavPath)
	if err != nil {
		t.Fatal(err)
	}
	stWAV, err := os.Stat(flacWAV)
	if err != nil {
		t.Fatal(err)
	}
	if stWAV.Size() >= int64(len(rawWAV)) {
		t.Fatalf("24-bit WAV FLAC size %d >= raw WAV %d", stWAV.Size(), len(rawWAV))
	}
	if rate, bits, got := flacSamples(t, flacWAV); rate != 48000 || bits != 24 || !equalSamples(got, s24) {
		t.Fatalf("24-bit WAV roundtrip mismatch (%d Hz, %d-bit)", rate, bits)
	}

	// Full-scale alternating 16-bit block whose order-4 residuals exceed 262,144 per sample
	// (4096 * 524280 > math.MaxInt32 at k=0), verifying verbatim fallback never inflates.
	alt16 := [][]int32{make([]int32, 4096)}
	for i := range alt16[0] {
		if i%2 == 0 {
			alt16[0][i] = 32767
		} else {
			alt16[0][i] = -32768
		}
	}
	rawAlt := wavFile(alt16, 16, 44100)
	altPath := filepath.Join(dir, "alt16.wav")
	if err := os.WriteFile(altPath, rawAlt, 0o644); err != nil {
		t.Fatal(err)
	}
	flacAlt, err := pcmToFLAC(altPath)
	if err != nil {
		t.Fatal(err)
	}
	stAlt, err := os.Stat(flacAlt)
	if err != nil {
		t.Fatal(err)
	}
	if stAlt.Size() > int64(len(rawAlt))+1024 {
		t.Fatalf("high-residual 16-bit FLAC inflated to %d bytes (raw %d)", stAlt.Size(), len(rawAlt))
	}
	if rate, bits, got := flacSamples(t, flacAlt); rate != 44100 || bits != 16 || !equalSamples(got, alt16) {
		t.Fatalf("16-bit alternating roundtrip mismatch (%d Hz, %d-bit)", rate, bits)
	}
}

func TestSaveSplitKeepsAnMP3AndRefusesOtherFiles(t *testing.T) {
	frames := mp3Frames(14, 8000) // about 8 MB
	srv, _, most := serve(t, frames, true)
	c := &Client{DownloadDir: t.TempDir()}
	dl, err := c.SaveSplit(context.Background(), "soundcloud-7", srv.URL)
	if err != nil || filepath.Base(dl.Path) != "soundcloud-7.mp3" || dl.Quality.Bitrate != 320000 {
		t.Fatalf("%+v %v", dl, err)
	}
	if got, _ := os.ReadFile(dl.Path); !bytes.Equal(got, frames) {
		t.Fatal("ranges joined out of order")
	}
	if *most < 2 || *most > partsAtOnce {
		t.Fatalf("%d requests at once", *most)
	}
	float := wavFile(pcmSamples(100, 1, 16), 16, 44100)
	float[32] = 3 // format tag: IEEE float
	other, _, _ := serve(t, float, true)
	if _, err := c.SaveSplit(context.Background(), "soundcloud-8", other.URL); err == nil {
		t.Fatal("float WAV accepted")
	}
}

func TestFetchPartsKeepsOrderWithManyParts(t *testing.T) {
	var parts []part
	var want bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Later parts answer first.
		var n int
		for _, ch := range r.URL.Path[1:] {
			n = n*10 + int(ch-'0')
		}
		time.Sleep(time.Duration(50-n) * time.Millisecond)
		w.Write(bytes.Repeat([]byte{byte(n)}, 100+n))
	}))
	defer srv.Close()
	for i := 0; i < 50; i++ {
		parts = append(parts, part{srv.URL + "/" + string(rune('0'+i/10)) + string(rune('0'+i%10)), -1, -1})
		want.Write(bytes.Repeat([]byte{byte(i)}, 100+i))
	}
	var got bytes.Buffer
	if err := (&Client{}).fetchParts(context.Background(), &got, parts); err != nil || !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("%v: %d bytes, want %d", err, got.Len(), want.Len())
	}
}
