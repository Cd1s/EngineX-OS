package compat_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"golang.org/x/text/unicode/norm"
)

func TestNormalizeUnicodeAndAmbiguity(t *testing.T) {
	root := t.TempDir()
	name := "éガ한Ａ.wav"
	disk := norm.NFD.String(name)
	testfixture.Write(t, root, "MUSIC/"+disk, []byte("audio"))
	r, e := compat.NewResolver(root)
	if e != nil {
		t.Fatal(e)
	}
	p, e := r.Resolve("/music/" + name)
	if e != nil || p != "MUSIC/"+disk {
		t.Fatalf("%q %v", p, e)
	}
	for _, p := range []string{"../../etc/passwd", "Music/../outside", "/Music/\x00.wav", "C:/Music/a.wav"} {
		if _, e = r.Resolve(p); e == nil {
			t.Errorf("accepted unsafe path %q", p)
		}
	}
	if e = os.Symlink("/etc", filepath.Join(root, "escape")); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Resolve("escape/passwd"); e == nil {
		t.Fatal("followed symlink")
	}
	testfixture.Write(t, root, "AA/File", nil)
	testfixture.Write(t, root, "AA/FILE", nil)
	r, _ = compat.NewResolver(root)
	if _, e = r.Resolve("AA/file"); e == nil {
		t.Fatal("ambiguous case fold accepted")
	}
	if got, e := r.Resolve("AA/FILE"); e != nil || got != "AA/FILE" {
		t.Fatal("exact spelling should win", got, e)
	}
}

func TestWAVExtensibleAndFLACLargeCover(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name   string
		b      []byte
		format string
	}{{"pcm.wav", testfixture.WAV(false), "wav"}, {"extensible.wav", testfixture.WAV(true), "wav"}, {"large-cover.flac", testfixture.FLAC(), "flac"}} {
		t.Run(tc.name, func(t *testing.T) {
			testfixture.Write(t, root, tc.name, tc.b)
			a, e := compat.AudioHeader(filepath.Join(root, tc.name))
			if e != nil {
				t.Fatal(e)
			}
			if a.SampleRate != 48000 || a.BitDepth != 24 || a.Channels != 2 || a.Duration != 2 || a.Samples != 96000 || a.Format != tc.format {
				t.Fatalf("%+v", a)
			}
			after, e := os.ReadFile(filepath.Join(root, tc.name))
			if e != nil || string(after) != string(tc.b) {
				t.Fatal("audio modified", e)
			}
		})
	}
	for _, tc := range []struct {
		name string
		b    []byte
	}{{"broken.wav", testfixture.WAV(true)[:28]}, {"broken.flac", testfixture.FLAC()[:60]}} {
		testfixture.Write(t, root, tc.name, tc.b)
		if _, e := compat.AudioHeader(filepath.Join(root, tc.name)); e == nil {
			t.Errorf("accepted truncated %s", tc.name)
		}
	}
}

func TestFLACHeaderRejectsImplausibleRateAndDuration(t *testing.T) {
	for _, tc := range []struct {
		rate    uint64
		samples uint64
	}{{768001, 48000}, {1, 10_000_001}} {
		data := testfixture.FLAC()
		binary.BigEndian.PutUint64(data[18:], tc.rate<<44|1<<41|15<<36|tc.samples)
		p := filepath.Join(t.TempDir(), "bad.flac")
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := compat.AudioHeader(p); err == nil {
			t.Fatal("accepted implausible FLAC", tc)
		}
	}
}

func TestResolveMacHiddenPioneer(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".PIONEER/USBANLZ/P001"), 0755); err != nil {
		t.Fatal(err)
	}
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"PIONEER": ".PIONEER", "/PIONEER/USBANLZ/P001": ".PIONEER/USBANLZ/P001"} {
		got, err := r.Resolve(in)
		if err != nil || got != want {
			t.Fatalf("Resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err = r.Resolve("MUSIC/PIONEER"); err == nil {
		t.Fatal("only the volume root maps PIONEER to .PIONEER")
	}
}

func TestAIFFHeaderAndFAT32TrailingSpaceDotResolution(t *testing.T) {
	root := t.TempDir()
	// Build a valid 48kHz, 2-channel, 24-bit, 96000-frame AIFF container (CF049).
	aiff := make([]byte, 12+26+16+96000*6)
	copy(aiff[0:4], "FORM")
	binary.BigEndian.PutUint32(aiff[4:8], uint32(len(aiff)-8))
	copy(aiff[8:12], "AIFF")
	copy(aiff[12:16], "COMM")
	binary.BigEndian.PutUint32(aiff[16:20], 18)
	binary.BigEndian.PutUint16(aiff[20:22], 2)     // channels
	binary.BigEndian.PutUint32(aiff[22:26], 96000) // frames
	binary.BigEndian.PutUint16(aiff[26:28], 24)    // bit depth
	// 48000.0 in 80-bit IEEE 754 extended float: exp = 16383 + 15 = 0x400E, mant = 0xBB80000000000000
	binary.BigEndian.PutUint16(aiff[28:30], 0x400e)
	binary.BigEndian.PutUint64(aiff[30:38], 0xbb80000000000000)
	copy(aiff[38:42], "SSND")
	binary.BigEndian.PutUint32(aiff[42:46], uint32(8+96000*6))

	testfixture.Write(t, root, "Club Set/track.aiff", aiff)
	info, err := compat.AudioHeader(filepath.Join(root, "Club Set/track.aiff"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "aiff" || info.SampleRate != 48000 || info.Channels != 2 || info.BitDepth != 24 || info.Samples != 96000 || info.Duration != 2 {
		t.Fatalf("unexpected AIFF info: %+v", info)
	}
	testfixture.Write(t, root, "bad.aiff", aiff[:40])
	if _, err := compat.AudioHeader(filepath.Join(root, "bad.aiff")); err == nil {
		t.Fatal("accepted truncated AIFF")
	}
	// H019: Directory exported with trailing space/period resolved against stripped FAT32 folder.
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve("/Club Set. /track.aiff")
	if err != nil || got != "Club Set/track.aiff" {
		t.Fatalf("Resolve trailing dot/space = %q, %v", got, err)
	}
}

func TestMP3AudioHeader(t *testing.T) {
	src := "../../internal/audioheader/testdata"
	for _, tc := range []struct {
		file       string
		sr         int
		wantOffset int
		wantClass  string
	}{
		{"01-lame-cbr-info-44100.mp3", 44100, 2257, "lame:2257"},
		{"01-lame-cbr-info-48000.mp3", 48000, 2257, "lame:2257"},
		{"04-xing-only-44100.mp3", 44100, 1152, "xing:1152"},
		{"05-headerless-lame-44100.mp3", 44100, 0, ""},
	} {
		t.Run(tc.file, func(t *testing.T) {
			info, err := compat.AudioHeader(filepath.Join(src, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if info.Format != "mp3" || info.SampleRate != tc.sr || info.Channels != 2 || info.StartOffsetSamples != tc.wantOffset || info.OffsetClass != tc.wantClass {
				t.Fatalf("unexpected MP3 AudioInfo: %+v", info)
			}
		})
	}
	bad := filepath.Join(t.TempDir(), "broken.mp3")
	if err := os.WriteFile(bad, []byte("not an mp3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := compat.AudioHeader(bad); err == nil {
		t.Fatal("accepted invalid MP3")
	}
}
