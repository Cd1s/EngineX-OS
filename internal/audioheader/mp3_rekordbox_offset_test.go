package audioheader

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #318: SC LIVE 4's imported MP3 grids were 47 ms (2257 samples at 48 kHz) early where
// about 23 ms (1105) is right. 5.3.0.4 moved a rekordbox position by StartOffsetSamples,
// which is where Engine's decoder starts (the Info frame it drops, 1152, plus start_pad
// 576 and 529), so a rekordbox that does not play the Info frame was corrected for it
// twice. The two numbers are different things since 729ea7c1; they are pinned apart here
// on real headers, one per class, because every other consumer needs the first one.
//
// Engine's decoder is the same on every 5.3.0 player: SC5000 and SC LIVE 4 ship the same
// libavformat.so.58.76.100 and libavcodec.so.58.134.100 bytes (AZ04's is a 64-bit build of
// the same release), so one table holds for Engine 5.3.0 and 5.3.0-sclive4 alike.
//
// Fixtures 07-10 were written by ffmpeg 7.1.5 (libmp3lame, encoder field "Lavc61.19", start_pad
// 576), the shape of the files in #318; 08 and 10 are 07 and 09 without their ID3v2 tag.
// The others are the #178 pack.
func TestRekordboxOffsetPerHeaderClass(t *testing.T) {
	cases := []struct {
		file      string
		sr        int
		tag       string // Xing/Info tag, "" if none
		encoder   string // prefix of the LAME extension's encoder field
		decoder   int    // StartOffsetSamples: what Engine's libavformat leaves out
		rekordbox int    // RekordboxOffsetSamples: rekordbox position - Engine position
		class     string
	}{
		// Info (CBR) with a LAME / Lavc extension: rekordbox does not play the Info frame,
		// so only start_pad + 529 = 1105 separates it from Engine. Checked on real decks
		// (SC5000, 2026-11-19) and by #318's own SC LIVE 4 analysis (-21.5 ms at 48 kHz).
		{"01-lame-cbr-info-44100.mp3", 44100, "Info", "LAME3.100", 2257, 1105, "lame:2257"},
		{"01-lame-cbr-info-48000.mp3", 48000, "Info", "LAME3.100", 2257, 1105, "lame:2257"},
		{"07-lavc-cbr-info-48000-id3.mp3", 48000, "Info", "Lavc61.19", 2257, 1105, "lame:2257"},
		{"08-lavc-cbr-info-48000-noid3.mp3", 48000, "Info", "Lavc61.19", 2257, 1105, "lame:2257"},
		// Xing (VBR) with an extension: still the #178 pack's number. Nobody has measured a
		// VBR file against a deck yet; do not move these without one (checklist row 371).
		{"03-lavc-vbr-xing-44100.mp3", 44100, "Xing", "Lavc", 2257, 2257, "lame:2257"},
		{"09-lavc-vbr-xing-48000-id3.mp3", 48000, "Xing", "Lavc61.19", 2257, 2257, "lame:2257"},
		{"10-lavc-vbr-xing-48000-noid3.mp3", 48000, "Xing", "Lavc61.19", 2257, 2257, "lame:2257"},
		// Xing without an extension, and no tag at all: as #178 calibrated them.
		{"04-xing-only-44100.mp3", 44100, "Xing", "", 1152, 1152, "xing:1152"},
		{"04-xing-only-48000.mp3", 48000, "Xing", "", 1152, 1152, "xing:1152"},
		{"05-headerless-lame-44100.mp3", 44100, "", "", 0, 0, ""},
		{"05-headerless-lame-48000.mp3", 48000, "", "", 0, 0, ""},
		{"06-headerless-shine-44100.mp3", 44100, "", "", 0, 0, ""},
	}
	byFile := map[string]MP3Info{}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			got, err := OpenMP3(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			byFile[tc.file] = got
			if got.SampleRate != tc.sr || got.Tag != tc.tag || !strings.HasPrefix(got.Encoder, tc.encoder) {
				t.Fatalf("header = %d Hz tag %q encoder %q, want %d Hz %q %q...", got.SampleRate, got.Tag, got.Encoder, tc.sr, tc.tag, tc.encoder)
			}
			if got.StartOffsetSamples != tc.decoder || got.RekordboxOffsetSamples != tc.rekordbox || got.OffsetClass != tc.class {
				t.Fatalf("offsets = decoder %d, rekordbox %d, class %q; want %d, %d, %q",
					got.StartOffsetSamples, got.RekordboxOffsetSamples, got.OffsetClass, tc.decoder, tc.rekordbox, tc.class)
			}
			// The frame rekordbox skips is counted once, not twice: with an Info frame and an
			// extension the two numbers differ by exactly that frame.
			if got.Tag == "Info" && got.LAMEApplied {
				if want := got.EncoderDelay + lameDecoderPad; got.RekordboxOffsetSamples != want || got.StartOffsetSamples-got.RekordboxOffsetSamples != got.SamplesPerFrame {
					t.Fatalf("Info frame counted twice: rekordbox %d, decoder %d, start_pad+529 %d, frame %d",
						got.RekordboxOffsetSamples, got.StartOffsetSamples, want, got.SamplesPerFrame)
				}
			}
		})
	}

	// A tag in front never moves a position: the same stream with and without its ID3v2.
	for _, pair := range [][2]string{
		{"07-lavc-cbr-info-48000-id3.mp3", "08-lavc-cbr-info-48000-noid3.mp3"},
		{"09-lavc-vbr-xing-48000-id3.mp3", "10-lavc-vbr-xing-48000-noid3.mp3"},
	} {
		a, b := byFile[pair[0]], byFile[pair[1]]
		if a == (MP3Info{}) || a != b {
			t.Errorf("%s and %s differ:\n%+v\n%+v", pair[0], pair[1], a, b)
		}
	}
	for _, name := range []string{"07-lavc-cbr-info-48000-id3.mp3", "09-lavc-vbr-xing-48000-id3.mp3"} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil || string(raw[:3]) != "ID3" {
			t.Fatalf("%s must start with an ID3v2 tag: %v", name, err)
		}
	}
}

// pkg/analyzer plays the Info frame (its decoder does not drop it) and trims what Engine's
// decoder leaves out, so it needs the decoder's start, 2257 for an Info frame and an
// extension, and #318 must not be fixed by lowering that number. This also holds the
// shared mechanism's neighbours: PeekMP3StartOffset and ReadMP3 agree on every real header.
func TestAnalyzerTrimStaysTheDecoderStart(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.mp3"))
	if err != nil || len(files) < 12 {
		t.Fatalf("fixtures: %v %v", files, err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			info, err := OpenMP3(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			br := bufio.NewReaderSize(f, 1<<16)
			if err := SkipID3(br); err != nil {
				t.Fatal(err)
			}
			if got := PeekMP3StartOffset(br); got != info.StartOffsetSamples {
				t.Fatalf("analyzer trim = %d, decoder start = %d", got, info.StartOffsetSamples)
			}
		})
	}
}
