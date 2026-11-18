package webui

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
)

// Small real containers with low-bit values that a lossy conversion would erase.
func rekordboxPCM(form string, bits int, floating bool) []byte {
	var pcm bytes.Buffer
	for i := 0; i < 64; i++ {
		if floating {
			binary.Write(&pcm, binary.LittleEndian, math.Float32bits([]float32{0, .5, -.25, 0x1p-60}[i%4]))
			continue
		}
		n := []int32{0, 1, -1, 1<<(bits-1) - 1, -1 << (bits - 1), 12345, -23456, 42}[i%8]
		for j := 0; j < bits/8; j++ {
			shift := j * 8
			if form == "AIFF" {
				shift = bits - 8 - shift
			}
			pcm.WriteByte(byte(n >> shift))
		}
	}
	var out bytes.Buffer
	if form == "WAVE" {
		out.WriteString("RIFF")
		binary.Write(&out, binary.LittleEndian, uint32(36+pcm.Len()))
		out.WriteString("WAVEfmt ")
		binary.Write(&out, binary.LittleEndian, uint32(16))
		kind := uint16(1)
		if floating {
			kind = 3
		}
		for _, v := range []any{kind, uint16(2), uint32(48000), uint32(48000 * bits / 4), uint16(bits / 4), uint16(bits)} {
			binary.Write(&out, binary.LittleEndian, v)
		}
		out.WriteString("data")
		binary.Write(&out, binary.LittleEndian, uint32(pcm.Len()))
	} else {
		comm := new(bytes.Buffer)
		binary.Write(comm, binary.BigEndian, uint16(2))
		binary.Write(comm, binary.BigEndian, uint32(32))
		binary.Write(comm, binary.BigEndian, uint16(bits))
		comm.Write([]byte{0x40, 0x0e, 0xbb, 0x80, 0, 0, 0, 0, 0, 0}) // 48000 Hz, extended80
		if form == "AIFC" {
			comm.WriteString("sowt\x00\x00")
		}
		out.WriteString("FORM")
		size := uint32(4 + 8 + comm.Len() + 16 + pcm.Len())
		if form == "AIFC" {
			size += 12
		}
		binary.Write(&out, binary.BigEndian, size)
		out.WriteString(form)
		if form == "AIFC" {
			out.WriteString("FVER")
			binary.Write(&out, binary.BigEndian, uint32(4))
			binary.Write(&out, binary.BigEndian, uint32(0xa2805140))
		}
		out.WriteString("COMM")
		binary.Write(&out, binary.BigEndian, uint32(comm.Len()))
		out.Write(comm.Bytes())
		out.WriteString("SSND")
		binary.Write(&out, binary.BigEndian, uint32(pcm.Len()+8))
		out.Write(make([]byte, 8))
	}
	out.Write(pcm.Bytes())
	return out.Bytes()
}

func TestRekordboxPCMStream(t *testing.T) {
	for _, tc := range []struct {
		name, mime, codec, suffix string
		data                      []byte
	}{
		{"16.WAV", "audio/wav", "mp3", ".mp3", rekordboxPCM("WAVE", 16, false)},
		{"24.wav", "audio/wav", "mp3", ".mp3", rekordboxPCM("WAVE", 24, false)},
		{"float.wav", "audio/wav", "mp3", ".mp3", rekordboxPCM("WAVE", 32, true)},
		{"16.AIF", "audio/aiff", "mp3", ".mp3", rekordboxPCM("AIFF", 16, false)},
		{"24.aiff", "audio/aiff", "mp3", ".mp3", rekordboxPCM("AIFF", 24, false)},
		{"24.aifc", "audio/aiff", "mp3", ".mp3", rekordboxPCM("AIFC", 24, false)},
		{"control.mp3", "audio/mpeg", "mp3", ".mp3", bytes.Repeat([]byte("ID3 control"), 20)},
		{"control.flac", "audio/flac", "flac", ".flac", bytes.Repeat([]byte("fLaC control"), 20)},
		{"control.m4a", "audio/mp4", "mp4a.40.2", ".mp4", bytes.Repeat([]byte("ftypM4A control"), 20)},
	} {
		for _, source := range []string{"computer", "media"} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				e := newRbxEnv(t, map[uint32]string{1: "/Users/dj/Music/" + tc.name})
				t.Cleanup(e.s.Close)
				want := tc.data
				if err := os.WriteFile(filepath.Join(e.share, tc.name), want, 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.mime == "audio/wav" || tc.mime == "audio/aiff" {
					info, err := compat.AudioHeader(filepath.Join(e.share, tc.name))
					if err != nil || info.SampleRate != 48000 || info.Channels != 2 || info.Samples != 32 {
						t.Fatalf("PCM fixture/import header: %+v: %v", info, err)
					}
				}
				ids, _ := e.trackIDs(t, "Tracks")
				track := ids["First"]
				if source == "media" { // CDJ-staged media uses the same drive-row path as OmniSource.
					ref, ok := localTrack(drive{root: e.share}, 1, "../"+tc.name, tc.name, "", "", 1, 0)
					if !ok {
						t.Fatal("PCM media row was filtered out")
					}
					track = e.s.tidal.addTrack(ref)
				}
				id := strconv.FormatInt(track, 10)
				var info struct{ Manifest string }
				tidalGet(t, e.base, "/v1/tracks/"+id+"/playbackinfopostpaywall", 200, &info)
				raw, err := base64.StdEncoding.DecodeString(info.Manifest)
				if err != nil {
					t.Fatal(err)
				}
				var manifest struct {
					MimeType, Codecs string
					URLs             []string
				}
				if err := json.Unmarshal(raw, &manifest); err != nil {
					t.Fatal(err)
				}
				if manifest.MimeType != tc.mime || manifest.Codecs != tc.codec || len(manifest.URLs) != 1 || !strings.HasSuffix(manifest.URLs[0], tc.suffix) {
					t.Fatalf("PCM manifest: %s", raw)
				}
				var legacy struct{ URL, Codec string }
				tidalGet(t, e.base, "/v1/tracks/"+id+"/streamurl", 200, &legacy)
				if legacy.URL != manifest.URLs[0] || legacy.Codec != tc.codec {
					t.Fatalf("legacy stream: %+v", legacy)
				}
				for _, partial := range []bool{false, true} {
					request, _ := http.NewRequest("GET", manifest.URLs[0], nil)
					status, body := http.StatusOK, want
					if partial {
						request.Header.Set("Range", "bytes="+strconv.Itoa(len(want)-16)+"-"+strconv.Itoa(len(want)-1))
						status, body = http.StatusPartialContent, want[len(want)-16:]
					}
					response, err := http.DefaultClient.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil || response.StatusCode != status || response.Header.Get("Content-Type") != tc.mime || !bytes.Equal(got, body) {
						t.Fatalf("PCM stream: status=%d mime=%s bytes=%d: %v", response.StatusCode, response.Header.Get("Content-Type"), len(got), err)
					}
				}
			})
		}
	}
}
