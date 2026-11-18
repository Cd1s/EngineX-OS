package bridge_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func copyMP3Fixture(t *testing.T, dstRoot, rel, fixtureName string) {
	t.Helper()
	src := filepath.Join("..", "..", "internal", "audioheader", "testdata", fixtureName)
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, dstRoot, rel, b)
}

func makeOffsetANLZ(firstBeatMS uint32) []byte {
	beats := make([]byte, 12+4*8)
	binary.BigEndian.PutUint32(beats[4:], 0x80000)
	binary.BigEndian.PutUint32(beats[8:], 4)
	barNums := []uint16{4, 1, 2, 3}
	for i := 0; i < 4; i++ {
		p := beats[12+8*i:]
		binary.BigEndian.PutUint16(p, barNums[i])
		binary.BigEndian.PutUint16(p[2:], 12000)
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
	return testfixture.PMAI(
		testfixture.Section("PQTZ", 24, beats),
		testfixture.Section("PCO2", 20, cues),
		testfixture.Section("PCO2", 20, mem),
		testfixture.Section("PWV7", 24, wave),
	)
}

type decodedTrackPositions struct {
	beat0, beat1    float64
	hotA, hotB      float64
	mainCue         float64
	loop0Start      float64
	loop0End        float64
	loop2Start      float64
	loop2End        float64
	trackSampleRate float64
}

func readTrackPositions(t *testing.T, db *sql.DB, pdbID int) decodedTrackPositions {
	t.Helper()
	var trackBlob, beatBlob, cueBlob, loopBlob []byte
	err := db.QueryRow(`SELECT p.trackData, p.beatData, p.quickCues, p.loops FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, pdbID).
		Scan(&trackBlob, &beatBlob, &cueBlob, &loopBlob)
	if err != nil {
		t.Fatalf("query track %d: %v", pdbID, err)
	}
	trackRaw, err := enginedb.QUncompress(trackBlob, 4096)
	if err != nil {
		t.Fatal(err)
	}
	beatRaw, err := enginedb.QUncompress(beatBlob, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	cueRaw, err := enginedb.QUncompress(cueBlob, 1<<16)
	if err != nil {
		t.Fatal(err)
	}

	var out decodedTrackPositions
	out.trackSampleRate = math.Float64frombits(binary.BigEndian.Uint64(trackRaw))
	out.beat0 = math.Float64frombits(binary.LittleEndian.Uint64(beatRaw[25:]))
	out.beat1 = math.Float64frombits(binary.LittleEndian.Uint64(beatRaw[25+24:]))

	r := bytes.NewReader(cueRaw[8:])
	var hot [8]float64
	for i := 0; i < 8; i++ {
		n, _ := r.ReadByte()
		_, _ = r.Seek(int64(n), 1)
		_ = binary.Read(r, binary.BigEndian, &hot[i])
		_, _ = r.Seek(4, 1)
	}
	out.hotA, out.hotB = hot[0], hot[1]
	_ = binary.Read(r, binary.BigEndian, &out.mainCue)

	lr := bytes.NewReader(loopBlob[8:])
	var loopStart, loopEnd [8]float64
	for i := 0; i < 8; i++ {
		n, _ := lr.ReadByte()
		_, _ = lr.Seek(int64(n), 1)
		_ = binary.Read(lr, binary.LittleEndian, &loopStart[i])
		_ = binary.Read(lr, binary.LittleEndian, &loopEnd[i])
		_, _ = lr.Seek(6, 1)
	}
	// Slot 0 receives BarMemLoop (first free slot because Hot Cue 1 is not a loop);
	// Slot 2 holds Hot Cue 3's IntroHotLoop.
	out.loop0Start, out.loop0End = loopStart[0], loopEnd[0]
	out.loop2Start, out.loop2End = loopStart[2], loopEnd[2]
	return out
}

func makeWAVWithSampleRate(sr int) []byte {
	b := testfixture.WAV(false)
	f := b[20:]
	binary.LittleEndian.PutUint32(f[4:], uint32(sr))
	binary.LittleEndian.PutUint32(f[8:], uint32(sr*6))
	return b
}

// #178: Verify Rekordbox -> Engine sample-offset subtraction across all 4 audio
// classes at both 44.1 kHz and 48 kHz, real file sample-rate authority over a
// mismatched export rate, and one-shot re-conversion of already-imported sticks.
func TestIssue178MP3OffsetClassesAndExistingLibraryResync(t *testing.T) {
	root := t.TempDir()

	// 8 tracks: 4 classes at 44.1 kHz + 4 classes at 48 kHz.
	// Track 5 intentionally exports a wrong samplingRate (44100) for a 48000 Hz MP3
	// to verify the bridge uses the file's real header sample rate.
	type spec struct {
		id          int
		rel         string
		fixture     string // empty = WAV
		sr          int
		exportSR    int
		firstBeatMS uint32
		wantOffset  int
	}
	tracks := []spec{
		{1, "MUSIC/44k-lame.mp3", "01-lame-cbr-info-44100.mp3", 44100, 44100, 50, 2257},
		{2, "MUSIC/44k-xing.mp3", "04-xing-only-44100.mp3", 44100, 44100, 50, 1152},
		{3, "MUSIC/44k-hdrless.mp3", "05-headerless-lame-44100.mp3", 44100, 44100, 24, 0},
		{4, "MUSIC/44k-ref.wav", "", 44100, 44100, 0, 0},
		{5, "MUSIC/48k-lame.mp3", "01-lame-cbr-info-48000.mp3", 48000, 44100, 46, 2257},
		{6, "MUSIC/48k-xing.mp3", "04-xing-only-48000.mp3", 48000, 48000, 46, 1152},
		{7, "MUSIC/48k-hdrless.mp3", "05-headerless-lame-48000.mp3", 48000, 48000, 22, 0},
		{8, "MUSIC/48k-ref.wav", "", 48000, 48000, 0, 0},
	}

	exportPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	if err := os.MkdirAll(filepath.Dir(exportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	expDB, err := sql.Open("sqlite", exportPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expDB.Exec(`
		CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT, bpmx100 INTEGER, length INTEGER, samplingRate INTEGER, analysisDataFilePath TEXT);
		CREATE TABLE playlist(playlist_id INTEGER PRIMARY KEY, playlist_id_parent INTEGER, sequenceNo INTEGER, name TEXT, attribute INTEGER);
		CREATE TABLE playlist_content(playlist_id INTEGER, content_id INTEGER, sequenceNo INTEGER);
		INSERT INTO playlist VALUES(1, NULL, 1, 'Issue178', 0);
	`); err != nil {
		t.Fatal(err)
	}

	for _, tr := range tracks {
		if tr.fixture == "" {
			testfixture.Write(t, root, tr.rel, makeWAVWithSampleRate(tr.sr))
		} else {
			copyMP3Fixture(t, root, tr.rel, tr.fixture)
		}
		anlzRel := fmt.Sprintf("PIONEER/USBANLZ/P%03d/ANLZ0000.DAT", tr.id)
		testfixture.Write(t, root, anlzRel, makeOffsetANLZ(tr.firstBeatMS))
		if _, err := expDB.Exec(`INSERT INTO content VALUES(?,?,?,?,8,?,?)`, tr.id, tr.rel, "/"+tr.rel, 12000, tr.exportSR, "/"+anlzRel); err != nil {
			t.Fatal(err)
		}
		if _, err := expDB.Exec(`INSERT INTO playlist_content VALUES(1,?,?)`, tr.id, tr.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := expDB.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil || res.Added != len(tracks) || res.Degraded != 0 {
		t.Fatalf("initial ConvertAtMount: %+v, err=%v", res, err)
	}

	db := dbOpen(t, root)
	for _, tr := range tracks {
		got := readTrackPositions(t, db, tr.id)
		sr := float64(tr.sr)
		off := float64(tr.wantOffset)
		if got.trackSampleRate != sr {
			t.Fatalf("track %d (%s): sample rate = %v, want real file header %v", tr.id, tr.rel, got.trackSampleRate, sr)
		}
		wantBeat0 := float64(tr.firstBeatMS)*sr/1000 - off
		wantBeat1 := float64(tr.firstBeatMS+500)*sr/1000 - off
		wantHotA := max(0.0, 20.0*sr/1000-off)
		wantHotB := 550.0*sr/1000 - off
		wantMain := 550.0*sr/1000 - off
		wantLoop0Start := 1050.0*sr/1000 - off
		wantLoop0End := 2050.0*sr/1000 - off
		wantLoop2Start := max(0.0, 20.0*sr/1000-off)
		wantLoop2End := 520.0*sr/1000 - off

		if math.Abs(got.beat0-wantBeat0) > 1e-6 || math.Abs(got.beat1-wantBeat1) > 1e-6 {
			t.Fatalf("track %d (%s) beats: got (%v, %v), want (%v, %v)", tr.id, tr.rel, got.beat0, got.beat1, wantBeat0, wantBeat1)
		}
		if math.Abs(got.hotA-wantHotA) > 1e-6 || math.Abs(got.hotB-wantHotB) > 1e-6 || math.Abs(got.mainCue-wantMain) > 1e-6 {
			t.Fatalf("track %d (%s) cues: got A=%v B=%v main=%v, want A=%v B=%v main=%v", tr.id, tr.rel, got.hotA, got.hotB, got.mainCue, wantHotA, wantHotB, wantMain)
		}
		if math.Abs(got.loop0Start-wantLoop0Start) > 1e-6 || math.Abs(got.loop0End-wantLoop0End) > 1e-6 ||
			math.Abs(got.loop2Start-wantLoop2Start) > 1e-6 || math.Abs(got.loop2End-wantLoop2End) > 1e-6 {
			t.Fatalf("track %d (%s) loops: got 0=(%v..%v) 2=(%v..%v), want 0=(%v..%v) 2=(%v..%v)",
				tr.id, tr.rel, got.loop0Start, got.loop0End, got.loop2Start, got.loop2End,
				wantLoop0Start, wantLoop0End, wantLoop2Start, wantLoop2End)
		}
		if bpm := scalar[float64](t, db, `SELECT bpmAnalyzed FROM Track WHERE pdbImportKey=?`, tr.id); bpm != 120 {
			t.Fatalf("track %d BPM changed: %v", tr.id, bpm)
		}
	}

	// Simulate an already-converted pre-fix library:
	// 1) Replace each track's BridgeState fingerprint with the legacy JSON digest
	//    (which had no MP3Offset field) and overwrite PerformanceData with unshifted positions.
	// 2) Write a legacy v1 .rb-bridge-stamp without the "v2 " prefix.
	lib, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.Validate(); err != nil {
		t.Fatal(err)
	}
	type legacyAnalysisFile struct {
		Path        string
		Size, MTime int64
	}
	type legacyFingerprint struct {
		Encoding     int
		Record       string
		ResolvedPath string
		Size, MTime  int64
		Analysis     []legacyAnalysisFile
		Artwork      *legacyAnalysisFile
	}
	for i, tr := range tracks {
		lt := lib.Tracks[i]
		audioSt, err := os.Stat(filepath.Join(root, tr.rel))
		if err != nil {
			t.Fatal(err)
		}
		anlzRel := fmt.Sprintf("PIONEER/USBANLZ/P%03d/ANLZ0000.DAT", tr.id)
		anlzSt, err := os.Stat(filepath.Join(root, anlzRel))
		if err != nil {
			t.Fatal(err)
		}
		fpBytes, err := json.Marshal(legacyFingerprint{
			Encoding:     3,
			Record:       lt.RecordHash + ":" + rekordbox.RecordHash(lt),
			ResolvedPath: tr.rel,
			Size:         audioSt.Size(),
			MTime:        audioSt.ModTime().UnixNano(),
			Analysis:     []legacyAnalysisFile{{Path: anlzRel, Size: anlzSt.Size(), MTime: anlzSt.ModTime().UnixNano()}},
		})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(fpBytes)
		oldHash := hex.EncodeToString(sum[:])

		anlz, err := rekordbox.ReadAnalysis(filepath.Join(root, anlzRel))
		if err != nil {
			t.Fatal(err)
		}
		unshifted, err := enginedb.EncodePerformance(rekordbox.Track{SampleRate: tr.sr, Duration: 8}, anlz)
		if err != nil {
			t.Fatal(err)
		}
		trackID := scalar[int64](t, db, `SELECT id FROM Track WHERE pdbImportKey=?`, tr.id)
		if _, err := db.Exec(`UPDATE BridgeState SET fingerprint=? WHERE trackId=?`, oldHash, trackID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=? WHERE trackId=?`,
			unshifted.TrackData, unshifted.Overview, unshifted.Beats, unshifted.QuickCues, unshifted.Loops, trackID); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	expSt, err := os.Stat(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	legacyStamp := fmt.Sprintf("PIONEER/rekordbox/exportLibrary.db %d %d\n", expSt.Size(), expSt.ModTime().UnixNano())
	if err := os.WriteFile(filepath.Join(root, "Engine Library/Database2/.rb-bridge-stamp"), []byte(legacyStamp), 0o644); err != nil {
		t.Fatal(err)
	}

	// Next mount must NOT bypass the legacy stamp, and must re-convert ONLY the 4 affected
	// MP3 tracks (1, 2, 5, 6), leaving the 2 headerless MP3s and 2 WAVs untouched.
	res, err = bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bypassed || res.Added != 0 || res.Updated != 4 || res.Unchanged != 4 {
		t.Fatalf("upgrade mount sync: got %+v, want Updated=4 Unchanged=4", res)
	}

	db = dbOpen(t, root)
	for _, id := range []int{1, 2, 5, 6} {
		tr := tracks[id-1]
		got := readTrackPositions(t, db, id)
		wantHotB := 550.0*float64(tr.sr)/1000 - float64(tr.wantOffset)
		if math.Abs(got.hotB-wantHotB) > 1e-6 {
			t.Fatalf("upgraded track %d hotB = %v, want %v", id, got.hotB, wantHotB)
		}
	}
	db.Close()

	// Subsequent mount bypasses via the upgraded v2 stamp, and a forced full sync is a 0-write no-op.
	if res, err = bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !res.Bypassed {
		t.Fatalf("post-upgrade mount was not bypassed: %+v, err=%v", res, err)
	}
	if res, err = bridge.Convert(context.Background(), root, enginedb.Options{}); err != nil || res.Updated != 0 || res.Unchanged != len(tracks) {
		t.Fatalf("post-upgrade full sync re-converted tracks: %+v, err=%v", res, err)
	}
}
