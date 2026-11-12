package prolink

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestCheckPDBBytesRejectsLegacyAndCorruptLayouts(t *testing.T) {
	// 1. Legacy testfixture.PDB() lacks index pages on tables 1..5, sets empty_candidate=0,
	//    leaves autoload_hot_cues="" and file_type=0: CheckPDBBytes must reject it.
	if _, err := CheckPDBBytes(testfixture.PDB()); err == nil {
		t.Fatal("CheckPDBBytes accepted legacy PDB without index/empty pages")
	}

	valid, err := buildPDB([]rekordbox.Track{
		{
			ID:           1,
			Path:         "/Music/one.mp3",
			AnalysisPath: "/PIONEER/USBANLZ/P001/ANLZ0000.DAT",
			ArtworkPath:  "/PIONEER/Artwork/A1.jpg",
			Title:        "One",
			Artist:       "Artist A",
			Key:          "8A",
			BPM:          128,
			Duration:     240,
			SampleRate:   44100,
			FileSize:     8000000,
		},
		{
			ID:           2,
			Path:         "/Music/two.flac",
			AnalysisPath: "/PIONEER/USBANLZ/P002/ANLZ0000.DAT",
			ArtworkPath:  "/PIONEER/Artwork/A1.jpg", // shared artwork -> deduplicated ID 1!
			Title:        "Two 東京",
			Artist:       "Artist A",
			Key:          "8A",
			BPM:          126,
			Duration:     210,
			SampleRate:   48000,
			FileSize:     24000000,
		},
	}, []rekordbox.Playlist{
		{ID: 10, Name: "Folder", Folder: true, SortOrder: 1},
		{ID: 11, ParentID: 10, Name: "Club", Tracks: []uint32{2, 1}, SortOrder: 2},
	})
	if err != nil {
		t.Fatalf("buildPDB: %v", err)
	}

	rep, err := CheckPDBBytes(valid)
	if err != nil {
		t.Fatalf("CheckPDBBytes rejected valid buildPDB output: %v", err)
	}
	if rep.ArtworksCount != 1 || len(rep.Tracks) != 2 || rep.PlaylistsCount != 2 {
		t.Fatalf("unexpected report summary: %+v", rep)
	}
	if rep.Tracks[0].HotCueAutoLoad != "ON" || rep.Tracks[0].FileType != pdbFileTypeMP3 || rep.Tracks[1].FileType != pdbFileTypeFLAC {
		t.Fatalf("unexpected track wire fields: %+v", rep.Tracks)
	}

	mutate := func(fn func([]byte)) []byte {
		cp := append([]byte(nil), valid...)
		fn(cp)
		return cp
	}

	cases := []struct {
		name string
		buf  []byte
		want string
	}{
		{
			name: "missing_index_page",
			buf: mutate(func(b []byte) {
				first := binary.LittleEndian.Uint32(b[28+8:])
				b[int(first)*4096+27] = 0x24
			}),
			want: "index page",
		},
		{
			name: "broken_empty_candidate_link",
			buf: mutate(func(b []byte) {
				last := binary.LittleEndian.Uint32(b[28+12:])
				binary.LittleEndian.PutUint32(b[int(last)*4096+12:], 0)
			}),
			want: "empty_candidate",
		},
		{
			name: "dirty_empty_candidate_page",
			buf: mutate(func(b []byte) {
				empty := binary.LittleEndian.Uint32(b[28+4:])
				b[int(empty)*4096+100] = 0xff
			}),
			want: "not blank",
		},
		{
			name: "zero_track_string_offset",
			buf: mutate(func(b []byte) {
				// Table 0 first data page is right after its index page (page 2)
				first := binary.LittleEndian.Uint32(b[28+8:])
				dataPage := binary.LittleEndian.Uint32(b[int(first)*4096+12:])
				binary.LittleEndian.PutUint16(b[int(dataPage)*4096+40+94:], 0)
			}),
			want: "offset=0",
		},
		{
			name: "missing_autoload_hot_cues_ON",
			buf: mutate(func(b []byte) {
				first := binary.LittleEndian.Uint32(b[28+8:])
				dataPage := binary.LittleEndian.Uint32(b[int(first)*4096+12:])
				rowStart := int(dataPage)*4096 + 40
				off7 := int(binary.LittleEndian.Uint16(b[rowStart+94+2*7:]))
				b[rowStart+off7+1] = 'N'
				b[rowStart+off7+2] = 'O'
			}),
			want: "autoload_hot_cues",
		},
		{
			name: "zero_file_type",
			buf: mutate(func(b []byte) {
				first := binary.LittleEndian.Uint32(b[28+8:])
				dataPage := binary.LittleEndian.Uint32(b[int(first)*4096+12:])
				rowStart := int(dataPage)*4096 + 40
				binary.LittleEndian.PutUint16(b[rowStart+90:], 0)
			}),
			want: "file_type=0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CheckPDBBytes(tc.buf)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCheckANLZFilesRejectsBrokenCuesAndSections(t *testing.T) {
	a := &rekordbox.Analysis{
		Beats: []rekordbox.Beat{
			{Number: 1, BPM: 120, TimeMS: 0},
			{Number: 2, BPM: 120, TimeMS: 500},
		},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 500, Name: "Cue A", Color: [4]byte{255, 230, 40, 40}},
			{HotCue: 2, TimeMS: 1000, EndMS: 3000, Loop: true, Name: "4-Beat Loop", Color: [4]byte{255, 40, 226, 20}},
			{HotCue: 0, TimeMS: 250},
			{HotCue: 0, TimeMS: 750},
		},
		Waveforms: []rekordbox.Waveform{
			{Tag: "PWV5", EntryBytes: 2, Data: make([]byte, 300*2)},
			{Tag: "PWV7", EntryBytes: 3, Data: make([]byte, 300*3)},
		},
	}
	root := t.TempDir()
	_, err := writeANLZ(root, 1, "/Music/test.mp3", a, anlzExtra{
		isMP3:       true,
		pvbrSamples: 100 * 1152,
		bpm:         120,
		pwav:        make([]byte, 400),
		pwv2:        make([]byte, 100),
	})
	if err != nil {
		t.Fatalf("writeANLZ failed: %v", err)
	}
	rep, err := CheckANLZFilesForTest(root, 1, "/Music/test.mp3", true)
	if err != nil || !rep {
		t.Fatalf("expected valid ANLZ triple, got %v", err)
	}

	// Verify old hot-cue chaining bug is caught if injected into PCOB:
	datWithoutPVBR := pmai(ppthSection("/Music/test.mp3"), pqtzSection(a.Beats))
	extValid := pmai(ppthSection("/Music/test.mp3"), pqt2Section())
	twoValid := pmai(ppthSection("/Music/test.mp3"))
	if err := CheckANLZFiles(datWithoutPVBR, extValid, twoValid, "/Music/test.mp3", true); err == nil || !strings.Contains(err.Error(), "PVBR") {
		t.Fatalf("expected missing PVBR error on MP3, got %v", err)
	}
	if err := CheckANLZFiles(datWithoutPVBR, pmai(ppthSection("/Music/test.mp3")), twoValid, "/Music/test.mp3", false); err == nil || !strings.Contains(err.Error(), "PQT2") {
		t.Fatalf("expected missing PQT2 error in EXT, got %v", err)
	}
	if err := CheckANLZFiles(datWithoutPVBR, extValid, pmai(ppthSection("/Music/other.mp3")), "/Music/test.mp3", false); err == nil || !strings.Contains(err.Error(), "PPTH") {
		t.Fatalf("expected PPTH mismatch error, got %v", err)
	}
}

func readANLZTripleForTest(root string, id uint32) ([]byte, []byte, []byte, error) {
	stem := filepath.Join(root, fmt.Sprintf("PIONEER/USBANLZ/P%03d/ANLZ0000", id))
	dat, err := os.ReadFile(stem + ".DAT")
	if err != nil {
		return nil, nil, nil, err
	}
	ext, err := os.ReadFile(stem + ".EXT")
	if err != nil {
		return nil, nil, nil, err
	}
	two, err := os.ReadFile(stem + ".2EX")
	if err != nil {
		return nil, nil, nil, err
	}
	return dat, ext, two, nil
}

func CheckANLZFilesForTest(root string, id uint32, wantPath string, isMP3 bool) (bool, error) {
	dat, ext, two, err := readANLZTripleForTest(root, id)
	if err != nil {
		return false, err
	}
	if err := CheckANLZFiles(dat, ext, two, wantPath, isMP3); err != nil {
		return false, err
	}
	// Verify loop numerator/denominator (2000 ms @ 120 BPM = 4/1 beats)
	parsedExt, err := rekordbox.ParseAnalysis(ext)
	if err != nil {
		return false, err
	}
	for _, c := range parsedExt.Cues {
		if c.HotCue == 2 && (c.LoopNumerator != 4 || c.LoopDenominator != 1) {
			return false, errInvalidLoopFraction(c.LoopNumerator, c.LoopDenominator)
		}
	}
	return true, nil
}

func errInvalidLoopFraction(num, den uint16) error {
	return &loopFracErr{num, den}
}

type loopFracErr struct{ num, den uint16 }

func (e *loopFracErr) Error() string {
	return "expected 4/1 beat loop fraction"
}
