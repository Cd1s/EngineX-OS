package enginedb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Tests of BuildTrack: one track from the volume as the background job stages it. They replace
// the import tests that checked the same rules through rows of m.db (sync.go and prepare.go);
// each names the old test it stands for.

// procReadChars is the bytes this process has read (rchar of /proc/self/io).
func procReadChars(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Skip("no /proc/self/io")
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "rchar:" {
			var n int64
			fmt.Sscan(f[1], &n)
			return n
		}
	}
	t.Skip("no rchar")
	return 0
}

func buildResolver(t *testing.T, root string) *compat.Resolver {
	t.Helper()
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// buildTrack is StatTrack and BuildTrack, as the job runs them.
func buildTrack(res *compat.Resolver, tr rekordbox.Track, read ReadOptions) (*rbstate.Staged, error) {
	src, err := StatTrack(context.Background(), res, tr)
	if err != nil {
		return nil, err
	}
	return BuildTrack(res, tr, src, read)
}

func mustBuild(t *testing.T, root string, tr rekordbox.Track) *rbstate.Staged {
	t.Helper()
	s, err := buildTrack(buildResolver(t, root), tr, ReadOptions{})
	if err != nil {
		t.Fatalf("BuildTrack %d: %v", tr.ID, err)
	}
	return s
}

// stagedPositions are the sample positions Engine reads from a staged track's blobs.
type stagedPositions struct {
	rate                 float64
	count                uint64
	beat0, beat1         float64
	hotA, hotB, main     float64
	loop0Start, loop0End float64
	loop2Start, loop2End float64
}

func positionsOf(t *testing.T, p rbstate.Perf) stagedPositions {
	t.Helper()
	trackRaw, err := QUncompress(p.TrackData, 4096)
	if err != nil {
		t.Fatal(err)
	}
	beatRaw, err := QUncompress(p.Beats, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	cueRaw, err := QUncompress(p.QuickCues, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	var out stagedPositions
	out.rate = math.Float64frombits(binary.BigEndian.Uint64(trackRaw))
	out.count = binary.BigEndian.Uint64(trackRaw[8:])
	out.beat0 = math.Float64frombits(binary.LittleEndian.Uint64(beatRaw[25:]))
	out.beat1 = math.Float64frombits(binary.LittleEndian.Uint64(beatRaw[25+24:]))
	r := bytes.NewReader(cueRaw[8:])
	var hot [8]float64
	for i := range hot {
		n, _ := r.ReadByte()
		_, _ = r.Seek(int64(n), 1)
		_ = binary.Read(r, binary.BigEndian, &hot[i])
		_, _ = r.Seek(4, 1)
	}
	out.hotA, out.hotB = hot[0], hot[1]
	_ = binary.Read(r, binary.BigEndian, &out.main)
	lr := bytes.NewReader(p.Loops[8:])
	var start, end [8]float64
	for i := range start {
		n, _ := lr.ReadByte()
		_, _ = lr.Seek(int64(n), 1)
		_ = binary.Read(lr, binary.LittleEndian, &start[i])
		_ = binary.Read(lr, binary.LittleEndian, &end[i])
		_, _ = lr.Seek(6, 1)
	}
	out.loop0Start, out.loop0End, out.loop2Start, out.loop2End = start[0], end[0], start[2], end[2]
	return out
}

// offsetANLZ is a .DAT with a 120 BPM grid from firstBeatMS, three hot cues (one a loop), a
// main cue, a memory loop and a PWV7.
func offsetANLZ(firstBeatMS uint32) []byte { return offsetANLZTempo(firstBeatMS, 12000) }

// offsetANLZTempo is offsetANLZ with the grid's tempo in hundredths of a BPM.
func offsetANLZTempo(firstBeatMS uint32, tempo uint16) []byte {
	beats := make([]byte, 12+4*8)
	binary.BigEndian.PutUint32(beats[4:], 0x80000)
	binary.BigEndian.PutUint32(beats[8:], 4)
	for i, bar := range []uint16{4, 1, 2, 3} {
		p := beats[12+8*i:]
		binary.BigEndian.PutUint16(p, bar)
		binary.BigEndian.PutUint16(p[2:], tempo)
		binary.BigEndian.PutUint32(p[4:], firstBeatMS+uint32(i*500))
	}
	cues := make([]byte, 8)
	binary.BigEndian.PutUint32(cues, 1)
	binary.BigEndian.PutUint16(cues[4:], 3)
	cues = append(cues, testfixture.CueExtended(1, 20, 0, "PreRoll", [3]byte{255, 0, 0})...)
	cues = append(cues, testfixture.CueExtended(2, 550, 0, "Downbeat", [3]byte{0, 255, 0})...)
	cues = append(cues, testfixture.CueExtended(3, 20, 520, "IntroHotLoop", [3]byte{0, 128, 255})...)
	mem := make([]byte, 8)
	binary.BigEndian.PutUint16(mem[4:], 2)
	mem = append(mem, testfixture.CueExtended(0, 550, 0, "MainCue", [3]byte{255, 255, 255})...)
	mem = append(mem, testfixture.CueExtended(0, 1050, 2050, "BarMemLoop", [3]byte{255, 200, 0})...)
	wave := make([]byte, 12+12)
	binary.BigEndian.PutUint32(wave, 3)
	binary.BigEndian.PutUint32(wave[4:], 4)
	copy(wave[12:], []byte{80, 60, 40, 90, 70, 50, 100, 80, 60, 70, 50, 30})
	return testfixture.PMAI(testfixture.Section("PQTZ", 24, beats), testfixture.Section("PCO2", 20, cues),
		testfixture.Section("PCO2", 20, mem), testfixture.Section("PWV7", 24, wave))
}

func wavAt(rate int) []byte {
	b := testfixture.WAV(false)
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*6))
	return b
}

// Offset rule 4 for every audio class at 44.1 and 48 kHz, with the file header's rate winning
// over a wrong export rate (track 5). Replaces TestIssue178MP3OffsetClassesAndExistingLibraryResync
// (its positions; the resync of older stamps is rbstate.Rules now) and
// TestOneOffsetRuleUSBRowMatchesLink (the USB row carries the rekordbox offset Link uses).
func TestBuildTrackOffsetRule4AcrossAudioClasses(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, tc := range []struct {
		id                   uint32
		rel, fixture         string
		rate, exportRate     int
		firstBeatMS          uint32
		wantOffset           float64
		wantStartOffsetFacts int
	}{
		{1, "MUSIC/44k-lame.mp3", "01-lame-cbr-info-44100.mp3", 44100, 44100, 50, 1105, 1105},
		{2, "MUSIC/44k-xing.mp3", "04-xing-only-44100.mp3", 44100, 44100, 50, 1152, 1152},
		{3, "MUSIC/44k-hdrless.mp3", "05-headerless-lame-44100.mp3", 44100, 44100, 24, 0, 0},
		{4, "MUSIC/44k-ref.wav", "", 44100, 44100, 0, 0, 0},
		{5, "MUSIC/48k-lame.mp3", "01-lame-cbr-info-48000.mp3", 48000, 44100, 46, 1105, 1105},
		{6, "MUSIC/48k-xing.mp3", "04-xing-only-48000.mp3", 48000, 48000, 46, 1152, 1152},
		{7, "MUSIC/48k-hdrless.mp3", "05-headerless-lame-48000.mp3", 48000, 48000, 22, 0, 0},
		{8, "MUSIC/48k-ref.wav", "", 48000, 48000, 0, 0, 0},
	} {
		if tc.fixture == "" {
			testfixture.Write(t, root, tc.rel, wavAt(tc.rate))
		} else {
			b, err := os.ReadFile(filepath.Join("..", "..", "internal", "audioheader", "testdata", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			testfixture.Write(t, root, tc.rel, b)
		}
		anlz := fmt.Sprintf("PIONEER/USBANLZ/P%03d/ANLZ0000.DAT", tc.id)
		testfixture.Write(t, root, anlz, offsetANLZ(tc.firstBeatMS))
		s := mustBuild(t, root, rekordbox.Track{ID: tc.id, Path: "/" + tc.rel, AnalysisPath: "/" + anlz, BPM: 120, Duration: 8, SampleRate: tc.exportRate})
		if s.Degraded != "" || s.Missing {
			t.Fatalf("track %d: %+v", tc.id, s)
		}
		got := positionsOf(t, s.Perf)
		sr, off := float64(tc.rate), tc.wantOffset
		if got.rate != sr || s.Facts.SampleRate != tc.rate {
			t.Fatalf("track %d (%s): rate %v / facts %d, want the file header's %v", tc.id, tc.rel, got.rate, s.Facts.SampleRate, sr)
		}
		if s.Facts.StartOffsetSamples != tc.wantStartOffsetFacts || s.Facts.BPM != 0 {
			t.Fatalf("track %d facts %+v: want start offset %d and no grid tempo (the export has one)", tc.id, s.Facts, tc.wantStartOffsetFacts)
		}
		want := stagedPositions{
			beat0: float64(tc.firstBeatMS)*sr/1000 - off, beat1: float64(tc.firstBeatMS+500)*sr/1000 - off,
			hotA: max(0, 20*sr/1000-off), hotB: 550*sr/1000 - off, main: 550*sr/1000 - off,
			loop0Start: 1050*sr/1000 - off, loop0End: 2050*sr/1000 - off,
			loop2Start: max(0, 20*sr/1000-off), loop2End: 520*sr/1000 - off,
		}
		for name, pair := range map[string][2]float64{
			"beat0": {got.beat0, want.beat0}, "beat1": {got.beat1, want.beat1}, "hotA": {got.hotA, want.hotA},
			"hotB": {got.hotB, want.hotB}, "main": {got.main, want.main}, "loop0Start": {got.loop0Start, want.loop0Start},
			"loop0End": {got.loop0End, want.loop0End}, "loop2Start": {got.loop2Start, want.loop2Start}, "loop2End": {got.loop2End, want.loop2End},
		} {
			if math.Abs(pair[0]-pair[1]) > 1e-6 {
				t.Errorf("track %d (%s) %s = %v, want %v (offset %v)", tc.id, tc.rel, name, pair[0], pair[1], off)
			}
		}
	}
}

// AAC: a truncated moov keeps the export's timing with no priming; an HE-AAC file that
// advertises only its 22.05 kHz core keeps the export's 44.1 kHz and moves grid, cues and loops
// by its 2048 samples of priming. Replaces TestAACImportPreservesExportTiming.
func TestBuildTrackAACKeepsTheExportTiming(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                     string
		fixture                                  func([]byte) []byte
		wantBeat0, wantBeat1, wantHotB, wantLoop float64
	}{
		{name: "truncated moov", fixture: func(b []byte) []byte { return b[:300] }, wantBeat1: 22050, wantHotB: 22050, wantLoop: 33075},
		{name: "HE-AAC core rate", wantBeat0: -2048, wantBeat1: 20002, wantHotB: 20002, wantLoop: 31027, fixture: func(b []byte) []byte {
			entry := bytes.Index(b, []byte("mp4a"))
			binary.BigEndian.PutUint32(b[entry+4+24:], 22050<<16)
			return b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			rel := testfixture.AACExport(t, root)
			testfixture.Write(t, root, rel, tc.fixture(testfixture.AACWithPriming()))
			l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
			if err != nil {
				t.Fatal(err)
			}
			s := mustBuild(t, root, l.Tracks[0])
			got := positionsOf(t, s.Perf)
			if got.rate != 44100 || got.count != 352800 || got.beat0 != tc.wantBeat0 || got.beat1 != tc.wantBeat1 ||
				got.hotB != tc.wantHotB || got.loop0Start != tc.wantLoop {
				t.Fatalf("want the export's 44.1 kHz grid, cue and loop; got %+v", got)
			}
			if s.Facts.SampleRate != 44100 || s.Facts.Duration != 8 {
				t.Fatalf("facts %+v: want the export's rate and length", s.Facts)
			}
		})
	}
}

// CF049: rekordbox's 500 BPM clamp of a misparsed file is no tempo. With a grid the staged
// tempo is the grid's; a grid whose first beat carries the clamp is dropped. Replaces the
// BuildTrack half of TestCF049Bogus500BPMAndGL010InformationGuard (the row's export tempo is
// the window's to clamp; GL-010 is the window's).
func TestBuildTrackDropsRekordbox500BPMClamp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	testfixture.Export(t, root)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	tr := l.Tracks[0]
	tr.BPM = 500
	s := mustBuild(t, root, tr)
	if s.Degraded != "" || s.Facts.BPM != 120 {
		t.Fatalf("500 BPM export with a 120 BPM grid: facts %+v degraded %q, want the grid's 120", s.Facts, s.Degraded)
	}
	testfixture.Write(t, root, "PIONEER/USBANLZ/P009/ANLZ0000.DAT", offsetANLZTempo(0, 50000))
	tr = rekordbox.Track{ID: 9, Path: "/MUSIC/three.wav", AnalysisPath: "/PIONEER/USBANLZ/P009/ANLZ0000.DAT", BPM: 500, Duration: 3}
	s = mustBuild(t, root, tr)
	if s.Facts.BPM != 0 {
		t.Fatalf("clamped grid: staged tempo %v, want none so Engine analyses the track", s.Facts.BPM)
	}
	if len(s.Perf.Beats) > 0 {
		raw, err := QUncompress(s.Perf.Beats, 1<<16)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte{0, 0, 0, 0, 0, 0x40, 0x7f}) { // 500.0 as a little-endian float64 tempo
			t.Fatal("the 500 BPM grid was staged")
		}
	}
	if s.Degraded != "" || s.Perf.QuickCues == nil {
		t.Fatalf("the clamped grid's track lost its cues: %+v", s)
	}
}

// #253: with a PWV7 in the .2EX the .EXT's PWV3 and PWV5 bodies are not read; its cues are.
// Replaces TestIssue253ImportLeavesTheEXTWaveformsUnread.
func TestBuildTrackLeavesTheEXTWaveformsUnread(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rchar is Linux's")
	}
	root, plain := t.TempDir(), t.TempDir()
	testfixture.Export(t, root)
	testfixture.Export(t, plain)
	wave := func(tag string, width, count int) []byte {
		b := make([]byte, 12+width*count)
		binary.BigEndian.PutUint32(b, uint32(width))
		binary.BigEndian.PutUint32(b[4:], uint32(count))
		for i := 12; i < len(b); i++ {
			b[i] = byte(i)
		}
		return testfixture.Section(tag, 24, b)
	}
	big := append(append([]byte(nil), testfixture.EXT()...), wave("PWV5", 2, 1<<20)...)
	big = append(big, wave("PWV3", 1, 2<<20)...)
	binary.BigEndian.PutUint32(big[8:], uint32(len(big)))
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT", big)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	before := procReadChars(t)
	want := mustBuild(t, plain, l.Tracks[0])
	base := procReadChars(t) - before
	before = procReadChars(t)
	got := mustBuild(t, root, l.Tracks[0])
	read := procReadChars(t) - before
	t.Logf("build read %d bytes, %d without the extra detail", read, base)
	if read-base > 256<<10 {
		t.Fatalf("the build read %d bytes more for a .EXT with 4 MB more waveform", read-base)
	}
	if len(got.Perf.Overview) == 0 || !bytes.Equal(got.Perf.Overview, want.Perf.Overview) {
		t.Fatal("the overview is not the one the PWV7 gives")
	}
	if raw, err := QUncompress(got.Perf.QuickCues, 1<<20); err != nil || !bytes.Contains(raw, []byte("H Loop")) {
		t.Fatalf("the .EXT's cues were lost: %v", err)
	}
	if detail := DetailFiles(got.Sig.ANLZ); strings.Join(detail, ",") != "PIONEER/USBANLZ/P001/ANLZ0000.2EX,PIONEER/USBANLZ/P001/ANLZ0000.EXT" {
		t.Fatalf("detail files %q", detail)
	}
}

// The OneLibrary cue table's cues join the ANLZ ones, and the RGB overview is built from the
// analysis. Replaces the BuildTrack part of TestEncodeRGBOverviewPWV6AndPWV4FormatAndPruneSafe's
// import path (the encoder's own test stays in rgb_overview_generate_test.go).
func TestBuildTrackStagesCuesFromTheExportAndAnRGBOverview(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	testfixture.Export(t, root)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	tr := l.Tracks[0]
	tr.Cues = []rekordbox.Cue{{HotCue: 5, Name: "From OneLibrary", TimeMS: 1500, Extended: true}}
	s := mustBuild(t, root, tr)
	raw, err := QUncompress(s.Perf.QuickCues, 1<<20)
	if err != nil || !bytes.Contains(raw, []byte("From OneLibrary")) || !bytes.Contains(raw, []byte("H Loop")) {
		t.Fatalf("cues of both sources: %v %q", err, raw)
	}
	body, err := QUncompress(s.RGB, 1<<20)
	if err != nil || !ValidRGBOverview(body) {
		t.Fatalf("staged RGB overview: %v (%d bytes)", err, len(body))
	}
	if !s.Perf.Complete || s.Facts.FileBytes == 0 || s.Sig.Rules != rbstate.Rules || len(s.Sig.ANLZ) != 3 {
		t.Fatalf("staged %+v", s)
	}
}

// #53: one unusable file stages its track without performance data, with the reason, and the
// other tracks are built. Replaces TestIssue53OneBadTrackKeepsTheRest (the record; that it is
// not read again while unchanged is the job's test) and TestArtworkRejectsDirectoryAndOversizedFile.
func TestBuildTrackDegradesOnlyTheTrackWithTheBadFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	testfixture.Export(t, root)
	testfixture.Write(t, root, "MUSIC/three.wav", []byte("not a wav file at all"))
	if err := os.Mkdir(filepath.Join(root, "cover-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	big, err := os.Create(filepath.Join(root, "cover-big.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if err = big.Truncate(32<<20 + 1); err != nil {
		t.Fatal(err)
	}
	big.Close()
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		track rekordbox.Track
		bad   bool
	}{
		{"good", l.Tracks[0], false},
		{"bad audio header", l.Tracks[2], true},
		{"cover is a directory", func() rekordbox.Track { tr := l.Tracks[0]; tr.ArtworkPath = "/cover-dir"; return tr }(), true},
		{"cover over 32 MiB", func() rekordbox.Track { tr := l.Tracks[0]; tr.ArtworkPath = "/cover-big.jpg"; return tr }(), true},
		{"missing ANLZ files", func() rekordbox.Track {
			tr := l.Tracks[0]
			tr.AnalysisPath = "/PIONEER/USBANLZ/P404/ANLZ0000.DAT"
			return tr
		}(), true},
	} {
		s := mustBuild(t, root, tc.track)
		if tc.bad != (s.Degraded != "") {
			t.Fatalf("%s: degraded %q", tc.name, s.Degraded)
		}
		if tc.bad && (s.Perf.TrackData != nil || s.Perf.QuickCues != nil || s.RGB != nil || s.Art != nil || !s.Sig.Artwork.IsZero()) {
			t.Fatalf("%s: a degraded track carries performance data or a cover: %+v", tc.name, s)
		}
	}
}

// A missing audio file is a record that says so (the window decides what the row gets); a file
// that is there but cannot be read now is no record, and the build succeeds once it can be
// read. Replaces TestPrepare252UnreadableAudioDoesNotAbortImport (missing, stat-permission,
// header-permission) and TestPrepare252FileCancellationRemainsFatal.
func TestBuildTrackMissingAndUnreadableAudio(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 2)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(root, testfixture.LargeTrackPath(2))
	oldStat, oldHeader := trackStat, trackAudioHeader
	t.Cleanup(func() { trackStat, trackAudioHeader = oldStat, oldHeader })

	saved, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	s := mustBuild(t, root, l.Tracks[1])
	if !s.Missing || s.Rel != testfixture.LargeTrackPath(2) || s.Perf.TrackData != nil || !s.Sig.Audio.IsZero() {
		t.Fatalf("missing file: %+v", s)
	}
	testfixture.Write(t, root, testfixture.LargeTrackPath(2), saved)

	for _, failure := range []string{"stat-permission", "header-permission"} {
		trackStat, trackAudioHeader = oldStat, oldHeader
		switch failure {
		case "stat-permission":
			trackStat = func(p string) (os.FileInfo, error) {
				if p == bad {
					return nil, &os.PathError{Op: "stat", Path: p, Err: os.ErrPermission}
				}
				return oldStat(p)
			}
		case "header-permission":
			trackAudioHeader = func(p string) (compat.AudioInfo, error) {
				if p == bad {
					return compat.AudioInfo{}, &os.PathError{Op: "open", Path: p, Err: os.ErrPermission}
				}
				return oldHeader(p)
			}
		}
		if s, err := buildTrack(buildResolver(t, root), l.Tracks[1], ReadOptions{}); err == nil || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("%s: staged %+v, err %v; want no record and the permission error", failure, s, err)
		}
		if s := mustBuild(t, root, l.Tracks[0]); s.Degraded != "" || s.Perf.TrackData == nil {
			t.Fatalf("%s: the other track: %+v", failure, s)
		}
	}
	trackStat, trackAudioHeader = oldStat, oldHeader
	if s := mustBuild(t, root, l.Tracks[1]); s.Degraded != "" || s.Missing || s.Perf.TrackData == nil {
		t.Fatalf("readable again: %+v", s)
	}

	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		trackAudioHeader = func(p string) (compat.AudioInfo, error) {
			return compat.AudioInfo{}, &os.PathError{Op: "read", Path: p, Err: cause}
		}
		if _, err := buildTrack(buildResolver(t, root), l.Tracks[0], ReadOptions{}); !errors.Is(err, cause) {
			t.Fatalf("a cancellation inside a file error became %v", err)
		}
	}
}

// Paths the volume refuses are no record: traversal, NUL, a symlink, a directory, a name two
// files share. Replaces TestPrepare252ErrorsRemainFatal (its duplicate case is the view's: one
// key, one track).
func TestBuildTrackRefusesUnsafePaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	testfixture.Write(t, root, "MUSIC/a.mp3", testfixture.MP3WithLAMETag())
	testfixture.Write(t, root, "AMB/a.mp3", []byte("one"))
	testfixture.Write(t, root, "AMB/A.mp3", []byte("two"))
	if err := os.Symlink(filepath.Join(root, "MUSIC/a.mp3"), filepath.Join(root, "link.mp3")); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"traversal":           "/elsewhere/../../MUSIC/a.mp3",
		"backslash-traversal": `\elsewhere\..\..\MUSIC\a.mp3`,
		"nul":                 "/MUSIC/a.mp3\x00",
		"symlink":             "/link.mp3",
		"nonregular":          "/MUSIC",
		"ambiguous":           "/AMB/a.MP3",
	} {
		if s, err := buildTrack(buildResolver(t, root), rekordbox.Track{ID: 1, Path: p}, ReadOptions{}); err == nil {
			t.Errorf("%s: staged %+v", name, s)
		}
	}
	if s := mustBuild(t, root, rekordbox.Track{ID: 1, Path: "/music/A.MP3"}); s.Rel != "MUSIC/a.mp3" || s.Key != "music/a.mp3" {
		t.Fatalf("case-different export spelling: %+v", s)
	}
}

// The job compares StatTrack's signature with the snapshot's and the stage's; BuildTrack must
// stage the same one for unchanged files, and a changed file changes it.
func TestStatTrackSignatureMatchesTheBuild(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	testfixture.Export(t, root)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	r := buildResolver(t, root)
	src, err := StatTrack(context.Background(), r, l.Tracks[0])
	if err != nil {
		t.Fatal(err)
	}
	s := mustBuild(t, root, l.Tracks[0])
	if !src.Sig.Equal(s.Sig) || src.Rel != s.Rel {
		t.Fatalf("stat %+v, build %+v", src.Sig, s.Sig)
	}
	later := time.Now().Add(time.Hour)
	if err = os.Chtimes(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT"), later, later); err != nil {
		t.Fatal(err)
	}
	again, err := StatTrack(context.Background(), r, l.Tracks[0])
	if err != nil || again.Sig.Equal(src.Sig) {
		t.Fatalf("a changed .EXT kept the signature: %v", err)
	}
}

// The export's cover is staged with the track, read at the job's pace; a cover inside the audio
// file is taken only when the export names none. Replaces the import half of
// TestExtractEmbeddedArtwork (the extractor's own tests stay).
func TestBuildTrackTakesTheEmbeddedCoverOnlyWithoutAnExportCover(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	testfixture.Export(t, root)
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{7}, 300<<10)...)
	tag := id3WithPicture(jpeg)
	mp3 := append(tag, testfixture.MP3WithLAMETag()...)
	testfixture.Write(t, root, "MUSIC/cover.mp3", mp3)
	exportCover := append([]byte{0xFF, 0xD8, 0xFF, 1}, bytes.Repeat([]byte{9}, 200<<10)...)
	testfixture.Write(t, root, "PIONEER/Artwork/a.jpg", exportCover)
	tr := rekordbox.Track{ID: 7, Path: "/MUSIC/cover.mp3", AnalysisPath: "/PIONEER/USBANLZ/P001/ANLZ0000.DAT", BPM: 120, Duration: 1, SampleRate: 44100}
	var pauses, largest int
	s, err := buildTrack(buildResolver(t, root), tr, ReadOptions{Chunk: 64 << 10, Pause: func(n int, _ time.Duration) error {
		pauses++
		largest = max(largest, n)
		return nil
	}})
	if err != nil || !bytes.Equal(s.Art, jpeg) {
		t.Fatalf("embedded cover: %v (%d bytes)", err, len(s.Art))
	}
	if largest > 64<<10 || pauses < 5 {
		t.Fatalf("a 300 KiB cover was read in %d paced reads, the largest %d bytes", pauses, largest)
	}
	tr.ArtworkPath = "/PIONEER/Artwork/a.jpg"
	pauses, largest = 0, 0
	s, err = buildTrack(buildResolver(t, root), tr, ReadOptions{Chunk: 64 << 10, Pause: func(n int, _ time.Duration) error {
		pauses++
		largest = max(largest, n)
		return nil
	}})
	if err != nil || !bytes.Equal(s.Art, exportCover) || s.Sig.Artwork.Rel != "PIONEER/Artwork/a.jpg" {
		t.Fatalf("export cover: %v, art %d bytes, sig %+v", err, len(s.Art), s.Sig.Artwork)
	}
	if largest > 64<<10 || pauses < 4 {
		t.Fatalf("a 200 KiB cover file was read in %d paced reads, the largest %d bytes", pauses, largest)
	}
	if s.Digest == nil || !bytes.Equal(s.Digest, WrittenDigest(s.Perf.Beats, s.Perf.QuickCues, s.Perf.Loops)) {
		t.Fatal("the record does not carry the digest of its grid, cues and loops")
	}
}

func id3WithPicture(jpeg []byte) []byte {
	frame := append([]byte{0}, []byte("image/jpeg\x00")...)
	frame = append(frame, 3, 0)
	frame = append(frame, jpeg...)
	hdr := make([]byte, 10)
	copy(hdr, "APIC")
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(frame)))
	body := append(hdr, frame...)
	tag := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 0}
	n := len(body)
	tag[6], tag[7], tag[8], tag[9] = byte(n>>21&0x7f), byte(n>>14&0x7f), byte(n>>7&0x7f), byte(n&0x7f)
	return append(tag, body...)
}

// While a deck plays the reads are at most 64 KiB, each followed by the pause; a stop from the
// pause ends the build at once with its error and stages nothing. Replaces
// TestIssue253ChecksArePacedAndStopCleanly (the pause and the clean stop, for the build).
func TestBuildTrackPacesItsReadsAndStopsCleanly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	testfixture.Export(t, root)
	wave := make([]byte, 12+3*200000)
	binary.BigEndian.PutUint32(wave, 3)
	binary.BigEndian.PutUint32(wave[4:], 200000)
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.PMAI(testfixture.Section("PWV7", 24, wave)))
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	var reads, largest int
	s, err := buildTrack(buildResolver(t, root), l.Tracks[0], ReadOptions{Chunk: 64 << 10, Pause: func(n int, _ time.Duration) error {
		reads++
		largest = max(largest, n)
		return nil
	}})
	if err != nil || s.Perf.Overview == nil {
		t.Fatalf("paced build: %v", err)
	}
	if largest > 64<<10 || reads < 600000/(64<<10) {
		t.Fatalf("a 600 KB PWV7 in %d paced reads, the largest %d bytes", reads, largest)
	}
	stop := errors.New("the deck needs the stick")
	calls := 0
	s, err = buildTrack(buildResolver(t, root), l.Tracks[0], ReadOptions{Chunk: 64 << 10, Pause: func(int, time.Duration) error {
		calls++
		return stop
	}})
	if s != nil || !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("stopped build: %+v %v after %d pauses", s, err, calls)
	}
}

// #253 review 3: a section the parser refuses, in the .2EX and the .EXT, degrades the track to
// the export's metadata; the build does not stop. Replaces the build half of
// TestIssue253BrokenANLZKeepsItsWaveformRow (malformed kinds, truncated files; the waveform copy
// rows it also pinned are gone with the state database, design §1.11 item 3).
func TestBuildTrackDegradesOnMalformedOrTruncatedAnalysis(t *testing.T) {
	t.Parallel()
	for _, kind := range append(append([]string(nil), testfixture.MalformedANLZKinds...), "truncated") {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			testfixture.Export(t, root)
			if kind == "truncated" {
				ex2 := testfixture.EX2()
				testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", ex2[:len(ex2)/2])
			} else {
				sec := testfixture.MalformedANLZSection(kind)
				testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.WithANLZSectionFirst(testfixture.EX2(), sec))
				testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT", testfixture.WithANLZSectionFirst(testfixture.EXT(), sec))
			}
			l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
			if err != nil {
				t.Fatal(err)
			}
			s := mustBuild(t, root, l.Tracks[0])
			if s.Degraded == "" || s.Perf.TrackData != nil || len(s.Sig.ANLZ) != 3 {
				t.Fatalf("%s: %+v", kind, s)
			}
		})
	}
}

// A symlink in a directory the job cannot list is refused, not taken for a missing file.
// Replaces TestPrepare252UnreadableDirectoryCannotHideSymlink (the same boundary, run as a user).
func TestStatTrackUnreadableDirectoryCannotHideSymlink(t *testing.T) {
	if os.Geteuid() == 0 {
		dir, err := os.MkdirTemp("", "trackbuild-permission-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		if err = os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		tmpDir := filepath.Join(dir, "tmp")
		if err = os.Mkdir(tmpDir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(tmpDir, 0o777); err != nil {
			t.Fatal(err)
		}
		binary, err := os.ReadFile(os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		exe := filepath.Join(dir, "permission.test")
		if err = os.WriteFile(exe, binary, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestStatTrackUnreadableDirectoryCannotHideSymlink$")
		cmd.Dir = dir
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "TMPDIR=") && !strings.HasPrefix(value, "GOTMPDIR=") && !strings.HasPrefix(value, "GOCACHE=") {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "TMPDIR="+tmpDir)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("unprivileged boundary: %v\n%s", err, out)
		}
		return
	}
	root, outside := t.TempDir(), t.TempDir()
	testfixture.Write(t, outside, "audio.mp3", []byte("audio"))
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "audio.mp3"), filepath.Join(private, "audio.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(private, 0o755) })
	src, err := StatTrack(context.Background(), buildResolver(t, root), rekordbox.Track{ID: 1, Path: "/private/audio.mp3"})
	if !errors.Is(err, os.ErrPermission) || src.Missing {
		t.Fatalf("unverified path: %+v %v", src, err)
	}
}
