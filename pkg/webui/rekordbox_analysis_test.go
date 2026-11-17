package webui

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// analysedRbxLink is the computer's library with track facts and analysis over dbserver.
type analysedRbxLink struct{ fakeRbxLink }

func (analysedRbxLink) Metadata(id uint32) ([]prolink.RekordboxItem, error) {
	return []prolink.RekordboxItem{{Kind: 4, Label: "First"}, {Kind: 7, Label: "Artist One"}, {Kind: 0xb, ID: 3}, {Kind: 0xd, ID: 12000}}, nil
}

func (analysedRbxLink) Analysis(id uint32, tag, file string) ([]byte, error) {
	switch tag + "." + file {
	case "PQTZ.DAT": // beats at 0.5 s and 1 s, 120 BPM
		b := make([]byte, 24+16)
		copy(b, "PQTZ")
		binary.BigEndian.PutUint32(b[4:], 24)
		binary.BigEndian.PutUint32(b[8:], 40)
		binary.BigEndian.PutUint32(b[20:], 2)
		for i, ms := range []uint32{500, 1000} {
			p := b[24+8*i:]
			binary.BigEndian.PutUint16(p, uint16(i+1))
			binary.BigEndian.PutUint16(p[2:], 12000)
			binary.BigEndian.PutUint32(p[4:], ms)
		}
		return b, nil
	case "PCOB.DAT": // hot cue A at 2 s
		b := make([]byte, 24+56)
		copy(b, "PCOB")
		binary.BigEndian.PutUint32(b[4:], 24)
		binary.BigEndian.PutUint32(b[8:], uint32(len(b)))
		binary.BigEndian.PutUint32(b[12:], 1)
		binary.BigEndian.PutUint16(b[18:], 1)
		p := b[24:]
		copy(p, "PCPT")
		binary.BigEndian.PutUint32(p[4:], 28)
		binary.BigEndian.PutUint32(p[8:], 56)
		binary.BigEndian.PutUint32(p[12:], 1)
		p[28] = 1
		binary.BigEndian.PutUint32(p[32:], 2000)
		binary.BigEndian.PutUint32(p[36:], 0xffffffff)
		return b, nil
	}
	return nil, nil
}

// flac48k is a FLAC file at 48 kHz, 3 s, padded to size: only its header is real.
func flac48k(size int) []byte {
	b := make([]byte, size)
	copy(b, "fLaC")
	b[4], b[7] = 0x80, 34 // last block, STREAMINFO
	si := uint64(48000)<<44 | uint64(1)<<41 | uint64(15)<<36 | 3*48000
	binary.BigEndian.PutUint32(b[8:], 4096<<16|4096)
	binary.BigEndian.PutUint64(b[8+10:], si)
	return b
}

// Engine analysed every rekordbox track it streamed (maintainer: grid, cues and both
// waveforms came only after 30-45 s): a load leaves rekordbox's own analysis, as the
// m.db blobs at the file's sample rate, where the native hook hands it to Engine.
func TestRekordboxLoadLeavesItsAnalysisForEngine(t *testing.T) {
	paths := map[uint32]string{1: "/Users/dj/Music/x.flac", 2: "/Users/dj/Music/y.flac"}
	e := newRbxEnv(t, paths)
	os.WriteFile(filepath.Join(e.share, "x.flac"), flac48k(4096), 0o644)
	dialRekordbox = func(context.Context, net.IP, byte) (rbxLink, error) { return analysedRbxLink{fakeRbxLink{paths}}, nil }
	old := rbxAnalysisDir
	rbxAnalysisDir = t.TempDir()
	t.Cleanup(func() { rbxAnalysisDir = old })
	ids, _ := e.trackIDs(t, "Tracks")
	tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(ids["First"], 10)+"/playbackinfopostpaywall", 200, nil)
	var b []byte
	for i := 0; i < 200 && len(b) == 0; i++ {
		b, _ = os.ReadFile(filepath.Join(rbxAnalysisDir, strconv.FormatInt(ids["First"], 10)+".bin"))
		time.Sleep(10 * time.Millisecond)
	}
	if !bytes.HasPrefix(b, []byte(rbxAnalysisMagic)) {
		t.Fatalf("no analysis left for the hook: %q", b)
	}
	var blobs [][]byte
	for r := b[len(rbxAnalysisMagic):]; len(r) >= 4; {
		n := binary.LittleEndian.Uint32(r)
		blobs = append(blobs, r[4:4+n])
		r = r[4+n:]
	}
	want, _ := enginedb.EncodePerformance(rekordbox.Track{ID: 1, Path: paths[1], SampleRate: 48000, SampleCount: 3 * 48000, Duration: 3, BPM: 120, Title: "First", Artist: "Artist One"},
		rbxAnalysis(analysedRbxLink{}, 1))
	if len(blobs) != 5 || len(want.Beats) == 0 || len(want.QuickCues) == 0 ||
		!bytes.Equal(blobs[0], want.TrackData) || !bytes.Equal(blobs[1], want.Overview) || !bytes.Equal(blobs[2], want.Beats) ||
		!bytes.Equal(blobs[3], want.QuickCues) || !bytes.Equal(blobs[4], want.Loops) {
		t.Fatalf("blobs differ from rb-bridge's at 48 kHz: %d blobs", len(blobs))
	}
}
