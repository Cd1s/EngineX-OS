package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func copyAudioFixture(t *testing.T, root, rel, fixture string) {
	t.Helper()
	src := filepath.Join("..", "..", "internal", "audioheader", "testdata", fixture)
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeVBRIFixture(t *testing.T, root, rel string) {
	t.Helper()
	// MPEG1 Layer III, 48000 Hz, 192 kbps (frameSize = 576), joint stereo -> sideInfo=32.
	h := uint32(0xffe00000) | (3 << 19) | (1 << 17) | (1 << 16) | (11 << 12) | (1 << 10)
	frameSize := 576
	f0 := make([]byte, frameSize)
	binary.BigEndian.PutUint32(f0, h)
	copy(f0[36:40], "VBRI")
	binary.BigEndian.PutUint16(f0[40:42], 1)
	binary.BigEndian.PutUint16(f0[42:44], 1152) // ignored by FFmpeg -> StartOffsetSamples == 0
	binary.BigEndian.PutUint32(f0[46:50], uint32(frameSize*2))
	binary.BigEndian.PutUint32(f0[50:54], 2)
	f1 := make([]byte, frameSize)
	binary.BigEndian.PutUint32(f1, h)
	f2 := make([]byte, frameSize)
	binary.BigEndian.PutUint32(f2, h)
	stream := append(append(f0, f1...), f2...)

	dst := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, stream, 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeTrackDataBlob(t *testing.T, sr float64, totalSamples uint64, key uint32, peaks [3]float64, sixtyEight bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, sr)
	_ = binary.Write(&buf, binary.BigEndian, totalSamples)
	_ = binary.Write(&buf, binary.BigEndian, key)
	for _, pk := range peaks {
		_ = binary.Write(&buf, binary.BigEndian, pk)
	}
	if sixtyEight {
		for range 3 {
			_ = binary.Write(&buf, binary.BigEndian, 0.0)
		}
	}
	b, err := QCompress(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func makeOverviewBlob(t *testing.T, totalSamples float64) []byte {
	t.Helper()
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint64(1024))
	_ = binary.Write(&buf, binary.BigEndian, uint64(1024))
	_ = binary.Write(&buf, binary.BigEndian, totalSamples/1024)
	for range 1024 {
		buf.Write([]byte{40, 30, 20})
	}
	buf.Write([]byte{40, 30, 20})
	b, err := QCompress(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type testMarker struct {
	pos  float64
	beat int64
	num  uint32
	unk  uint32
}

func makeBeatDataBlob(t *testing.T, sr, sampleLen float64, defGrid, adjGrid []testMarker, tail []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, sr)
	_ = binary.Write(&buf, binary.BigEndian, sampleLen)
	buf.WriteByte(1)
	for _, g := range [][]testMarker{defGrid, adjGrid} {
		_ = binary.Write(&buf, binary.BigEndian, uint64(len(g)))
		for _, m := range g {
			_ = binary.Write(&buf, binary.LittleEndian, m.pos)
			_ = binary.Write(&buf, binary.LittleEndian, m.beat)
			_ = binary.Write(&buf, binary.LittleEndian, m.num)
			_ = binary.Write(&buf, binary.LittleEndian, m.unk)
		}
	}
	buf.Write(tail)
	b, err := QCompress(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func makeAnalyzerBeatBlob(t *testing.T, sr, sampleLen, pos0, pos1 float64, n int64) []byte {
	t.Helper()
	grid := []testMarker{
		{pos: pos0, beat: -4, num: uint32(n + 4), unk: 0},
		{pos: pos1, beat: n, num: 0, unk: 0},
	}
	return makeBeatDataBlob(t, sr, sampleLen, grid, grid, []byte{0})
}

func makeQuickCuesBlob(t *testing.T, slots [8]float64, mainPos, defPos float64) []byte {
	t.Helper()
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint64(8))
	for i, pos := range slots {
		name := ""
		if pos != -1 {
			name = "Cue" + string(rune('1'+i))
		}
		buf.WriteByte(byte(len(name)))
		buf.WriteString(name)
		_ = binary.Write(&buf, binary.BigEndian, pos)
		if pos != -1 {
			buf.Write([]byte{255, 0, 255, 0})
		} else {
			buf.Write([]byte{0, 0, 0, 0})
		}
	}
	_ = binary.Write(&buf, binary.BigEndian, mainPos)
	buf.WriteByte(0)
	_ = binary.Write(&buf, binary.BigEndian, defPos)
	b, err := QCompress(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func makeLoopsBlob(t *testing.T, starts, ends [8]float64, set [8][2]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteByte(8)
	buf.Write(make([]byte, 7))
	for i := range 8 {
		name := ""
		if set[i][0] == 1 {
			name = "Loop" + string(rune('1'+i))
		}
		buf.WriteByte(byte(len(name)))
		buf.WriteString(name)
		_ = binary.Write(&buf, binary.LittleEndian, starts[i])
		_ = binary.Write(&buf, binary.LittleEndian, ends[i])
		buf.WriteByte(set[i][0])
		buf.WriteByte(set[i][1])
		if set[i][0] == 1 {
			buf.Write([]byte{255, 255, 127, 0})
		} else {
			buf.Write([]byte{0, 0, 0, 0})
		}
	}
	return buf.Bytes()
}

func readBeatPositions(t *testing.T, blob []byte) (def, adj []float64) {
	t.Helper()
	raw, err := QUncompress(blob, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	off := 17
	for g := range 2 {
		cnt := int(binary.BigEndian.Uint64(raw[off : off+8]))
		off += 8
		var out []float64
		for range cnt {
			out = append(out, math.Float64frombits(binary.LittleEndian.Uint64(raw[off:off+8])))
			off += 24
		}
		if g == 0 {
			def = out
		} else {
			adj = out
		}
	}
	return def, adj
}

func readQuickCuePositions(t *testing.T, blob []byte) (slots [8]float64, mainPos, defPos float64) {
	t.Helper()
	raw, err := QUncompress(blob, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	off := 8
	for i := range 8 {
		n := int(raw[off])
		off += 1 + n
		slots[i] = math.Float64frombits(binary.BigEndian.Uint64(raw[off : off+8]))
		off += 12
	}
	mainPos = math.Float64frombits(binary.BigEndian.Uint64(raw[off : off+8]))
	defPos = math.Float64frombits(binary.BigEndian.Uint64(raw[off+9 : off+17]))
	return slots, mainPos, defPos
}

func readLoopPositions(t *testing.T, blob []byte) (starts, ends [8]float64) {
	t.Helper()
	off := 8
	for i := range int(blob[0]) {
		n := int(blob[off])
		off += 1 + n
		starts[i] = math.Float64frombits(binary.LittleEndian.Uint64(blob[off : off+8]))
		ends[i] = math.Float64frombits(binary.LittleEndian.Uint64(blob[off+8 : off+16]))
		off += 22
	}
	return starts, ends
}

func insertAnalyzerTrack(t *testing.T, db *sql.DB, id int64, relPath, fileType, comment string, locked int, trackBlob, ovBlob, beatBlob, cuesBlob, loopsBlob []byte) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO Track(id,path,filename,fileType,comment,isAnalyzed,isAvailable,isMetadataImported,isBeatGridLocked) VALUES(?,?,?,?,?,1,1,1,?)`,
		id, relPath, filepath.Base(relPath), fileType, comment, locked)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=? WHERE trackId=?`,
		trackBlob, ovBlob, beatBlob, cuesBlob, loopsBlob, id)
	if err != nil {
		t.Fatal(err)
	}
}

func TestFixMP3GridsExactShiftAndClamping(t *testing.T) {
	dbPath, root, db := testlibrary.Create(t)
	defer db.Close()

	// 1. Streaming LAME CBR 44100 Hz MP3 (StartOffsetSamples == 2257) with negative beat-4 anchor,
	// early hot/main cues clamping at 0, later cues shifting exactly, and early + later loops.
	copyAudioFixture(t, root, "Music/Stream/101.mp3", "01-lame-cbr-info-44100.mp3")
	td1 := makeTrackDataBlob(t, 44100, 441000, 1, [3]float64{0.68, 0.40, 0.18}, false)
	ov1 := makeOverviewBlob(t, 441000)
	bt1 := makeAnalyzerBeatBlob(t, 44100, 441000, -79000.0, 460000.0, 20)
	qc1 := makeQuickCuesBlob(t, [8]float64{500.0, 5000.0, -1, -1, -1, -1, -1, -1}, 1000.0, 3000.0)
	lp1 := makeLoopsBlob(t,
		[8]float64{500.0, 10000.0, -1, -1, -1, -1, -1, -1},
		[8]float64{1500.0, 20000.0, -1, -1, -1, -1, -1, -1},
		[8][2]byte{{1, 1}, {1, 1}})
	insertAnalyzerTrack(t, db, 1, "../Music/Stream/101.mp3", "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 0, td1, ov1, bt1, qc1, lp1)

	// 2. Wi-Fi Drop Xing-only 48000 Hz MP3 (StartOffsetSamples == 1152) with NULL quickCues/loops.
	copyAudioFixture(t, root, "Wi-Fi Drop/Club/drop.mp3", "04-xing-only-48000.mp3")
	td2 := makeTrackDataBlob(t, 48000, 480000, 2, [3]float64{0.5, 0.3, 0.1}, false)
	ov2 := makeOverviewBlob(t, 480000)
	bt2 := makeAnalyzerBeatBlob(t, 48000, 480000, -1000.0, 481000.0, 16)
	insertAnalyzerTrack(t, db, 2, "../Wi-Fi Drop/Club/drop.mp3", "mp3", "[320k] MP3 48000 Hz; FLAC unavailable", 0, td2, ov2, bt2, nil, nil)

	// 3. Relocated/custom-path EngineX MP3 identified via Quality.Comment() (Lavc VBR Xing, 2257).
	copyAudioFixture(t, root, "Custom/lavc.mp3", "03-lavc-vbr-xing-44100.mp3")
	td3 := makeTrackDataBlob(t, 44100, 441000, 3, [3]float64{0.6, 0.4, 0.2}, false)
	ov3 := makeOverviewBlob(t, 441000)
	bt3 := makeAnalyzerBeatBlob(t, 44100, 441000, 2000.0, 443000.0, 18)
	insertAnalyzerTrack(t, db, 3, "../Custom/lavc.mp3", "mp3", "[192k] MP3 44100 Hz; FLAC unavailable", 0, td3, ov3, bt3, nil, nil)

	db.Close()

	rep, err := FixMP3Grids(context.Background(), root, MP3GridOptions{})
	if err != nil {
		t.Fatalf("FixMP3Grids: %v", err)
	}
	if rep.Candidates != 3 || rep.Corrected != 3 || rep.Skipped != 0 || rep.Unchanged != 0 || rep.Bypassed || rep.Interrupted {
		t.Fatalf("unexpected report: %+v", rep)
	}

	ro, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	var tr1, ovOut1, btOut1, qcOut1, lpOut1 []byte
	if err := ro.QueryRow(`SELECT trackData,overviewWaveFormData,beatData,quickCues,loops FROM PerformanceData WHERE trackId=1`).Scan(&tr1, &ovOut1, &btOut1, &qcOut1, &lpOut1); err != nil {
		t.Fatal(err)
	}
	rawTr1, err := QUncompress(tr1, 4096)
	if err != nil || len(rawTr1) != 68 {
		t.Fatalf("track 1 trackData not upgraded to 68 bytes: len=%d err=%v", len(rawTr1), err)
	}
	def1, adj1 := readBeatPositions(t, btOut1)
	if math.Abs(def1[0]-(-79000.0-2257)) > 1e-6 || math.Abs(def1[1]-(460000.0-2257)) > 1e-6 || math.Abs(adj1[0]-def1[0]) > 1e-6 {
		t.Fatalf("track 1 beatData mismatch: def=%v adj=%v", def1, adj1)
	}
	slots1, main1, defCue1 := readQuickCuePositions(t, qcOut1)
	if slots1[0] != 0 || math.Abs(slots1[1]-(5000.0-2257)) > 1e-6 || slots1[2] != -1 || main1 != 0 || math.Abs(defCue1-(3000.0-2257)) > 1e-6 {
		t.Fatalf("track 1 quickCues mismatch: slots=%v main=%v def=%v", slots1, main1, defCue1)
	}
	starts1, ends1 := readLoopPositions(t, lpOut1)
	if starts1[0] != 0 || math.Abs(ends1[0]-1000.0) > 1e-6 || math.Abs(starts1[1]-(10000.0-2257)) > 1e-6 || math.Abs(ends1[1]-(20000.0-2257)) > 1e-6 {
		t.Fatalf("track 1 loops mismatch: starts=%v ends=%v", starts1, ends1)
	}
	if _, err := ParseDisplayPerformance(tr1, ovOut1, btOut1, qcOut1, lpOut1); err != nil {
		t.Fatalf("Engine display rejected corrected track 1: %v", err)
	}

	var btOut2, qcOut2, lpOut2 []byte
	if err := ro.QueryRow(`SELECT beatData,quickCues,loops FROM PerformanceData WHERE trackId=2`).Scan(&btOut2, &qcOut2, &lpOut2); err != nil {
		t.Fatal(err)
	}
	def2, adj2 := readBeatPositions(t, btOut2)
	if math.Abs(def2[0]-(-1000.0-1152)) > 1e-6 || math.Abs(def2[1]-(481000.0-1152)) > 1e-6 || math.Abs(adj2[0]-def2[0]) > 1e-6 {
		t.Fatalf("track 2 beatData mismatch: def=%v adj=%v", def2, adj2)
	}
	if qcOut2 != nil || lpOut2 != nil {
		t.Fatalf("track 2 NULL quickCues/loops modified: qc=%v lp=%v", qcOut2, lpOut2)
	}

	var btOut3 []byte
	if err := ro.QueryRow(`SELECT beatData FROM PerformanceData WHERE trackId=3`).Scan(&btOut3); err != nil {
		t.Fatal(err)
	}
	def3, _ := readBeatPositions(t, btOut3)
	// Negative pre-zero start anchor kept (2000 - 2257 = -257).
	if math.Abs(def3[0]-(2000.0-2257)) > 1e-6 || math.Abs(def3[1]-(443000.0-2257)) > 1e-6 {
		t.Fatalf("track 3 beatData mismatch: def=%v", def3)
	}
	ro.Close()

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rep2, err := FixMP3Grids(context.Background(), root, MP3GridOptions{})
	if err != nil || !rep2.Bypassed || rep2.Corrected != 0 {
		t.Fatalf("second run should bypass: %+v err=%v", rep2, err)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("second run modified m.db")
	}
}

func TestFixMP3GridsUntouchedTrackClasses(t *testing.T) {
	dbPath, root, db := testlibrary.Create(t)
	defer db.Close()

	type rowSnap struct {
		id         int64
		track      []byte
		beat       []byte
		cues       []byte
		loops      []byte
		lastEdited sql.NullInt64
	}
	var snaps []rowSnap

	// 1. All 5 real stock Engine 5.0.4 performance samples from docs/re-report/ENGINE-5.0.4-performance-samples.json.
	rawJSON, err := os.ReadFile(filepath.Join("..", "..", "docs", "re-report", "ENGINE-5.0.4-performance-samples.json"))
	if err != nil {
		t.Fatal(err)
	}
	var samples struct {
		Records []struct {
			TrackID   int64 `json:"trackId"`
			TrackData struct {
				SampleRate float64    `json:"sample_rate"`
				SampleLen  uint64     `json:"sample_length"`
				Key        uint32     `json:"offset16_u32_candidate_analyzed_key"`
				Peaks      [3]float64 `json:"unknown_doubles"`
			} `json:"trackData"`
			BeatData struct {
				SampleRate float64 `json:"sample_rate"`
				SampleLen  float64 `json:"sample_length"`
				Default    []struct {
					SamplePos float64 `json:"sample_position"`
					BeatIdx   int64   `json:"beat_index"`
					UntilNext uint32  `json:"beats_until_next"`
					Unk       uint32  `json:"unknown_u32"`
				} `json:"default_grid"`
				Adjusted []struct {
					SamplePos float64 `json:"sample_position"`
					BeatIdx   int64   `json:"beat_index"`
					UntilNext uint32  `json:"beats_until_next"`
					Unk       uint32  `json:"unknown_u32"`
				} `json:"adjusted_grid"`
				TailHex string `json:"unknown_tail_hex"`
			} `json:"beatData"`
		} `json:"records"`
	}
	if err := json.Unmarshal(rawJSON, &samples); err != nil {
		t.Fatal(err)
	}
	if len(samples.Records) != 5 {
		t.Fatalf("expected 5 stock 5.0.4 records, got %d", len(samples.Records))
	}
	var nextID int64 = 1
	for _, rec := range samples.Records {
		rel := "Music/Stream/stock504-" + string(rune('0'+rec.TrackID)) + ".mp3"
		copyAudioFixture(t, root, rel, "01-lame-cbr-info-44100.mp3")
		td := makeTrackDataBlob(t, rec.TrackData.SampleRate, rec.TrackData.SampleLen, rec.TrackData.Key, rec.TrackData.Peaks, false)
		ov := makeOverviewBlob(t, float64(rec.TrackData.SampleLen))
		var def, adj []testMarker
		for _, m := range rec.BeatData.Default {
			def = append(def, testMarker{m.SamplePos, m.BeatIdx, m.UntilNext, m.Unk})
		}
		for _, m := range rec.BeatData.Adjusted {
			adj = append(adj, testMarker{m.SamplePos, m.BeatIdx, m.UntilNext, m.Unk})
		}
		tail, err := hex.DecodeString(rec.BeatData.TailHex)
		if err != nil {
			t.Fatal(err)
		}
		bt := makeBeatDataBlob(t, rec.BeatData.SampleRate, rec.BeatData.SampleLen, def, adj, tail)
		insertAnalyzerTrack(t, db, nextID, "../"+rel, "mp3", "", 0, td, ov, bt, nil, nil)
		nextID++
	}

	// 2. Stock 5.3.0 analysis (68-byte trackData + 138-byte beatData with non-zero unk).
	copyAudioFixture(t, root, "Music/Stream/stock530.mp3", "01-lame-cbr-info-44100.mp3")
	td530 := makeTrackDataBlob(t, 44100, 441000, 1, [3]float64{0.6, 0.3, 0.1}, true)
	ov530 := makeOverviewBlob(t, 441000)
	bt530 := makeBeatDataBlob(t, 44100, 441000,
		[]testMarker{{-79000, -4, 24, 12345}, {460000, 20, 0, 67890}},
		[]testMarker{{-79000, -4, 24, 12345}, {460000, 20, 0, 67890}},
		make([]byte, 9))
	insertAnalyzerTrack(t, db, nextID, "../Music/Stream/stock530.mp3", "mp3", "", 0, td530, ov530, bt530, nil, nil)
	nextID++

	// 3. r4 68-byte EngineX analysis (already offset-trimmed).
	copyAudioFixture(t, root, "Music/Stream/r4.mp3", "01-lame-cbr-info-44100.mp3")
	tdR4 := makeTrackDataBlob(t, 44100, 441000, 1, [3]float64{0.6, 0.3, 0.1}, true)
	btR4 := makeAnalyzerBeatBlob(t, 44100, 441000, -79000, 460000, 20)
	insertAnalyzerTrack(t, db, nextID, "../Music/Stream/r4.mp3", "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 0, tdR4, ov530, btR4, nil, nil)
	nextID++

	// 4. Rekordbox-bridge track (pdbImportKey + thirdPartySourceId=1).
	copyAudioFixture(t, root, "Music/Stream/rb.mp3", "01-lame-cbr-info-44100.mp3")
	tdRB := makeTrackDataBlob(t, 44100, 441000, 1, [3]float64{0.6, 0.3, 0.1}, false)
	btRB := makeAnalyzerBeatBlob(t, 44100, 441000, -79000, 460000, 20)
	insertAnalyzerTrack(t, db, nextID, "../Music/Stream/rb.mp3", "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 0, tdRB, ov530, btRB, nil, nil)
	if _, err := db.Exec(`UPDATE Track SET pdbImportKey=99 WHERE id=?`, nextID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET thirdPartySourceId=1 WHERE trackId=?`, nextID); err != nil {
		t.Fatal(err)
	}
	nextID++

	// 5. Locked beat grid (isBeatGridLocked=1).
	copyAudioFixture(t, root, "Music/Stream/locked.mp3", "01-lame-cbr-info-44100.mp3")
	insertAnalyzerTrack(t, db, nextID, "../Music/Stream/locked.mp3", "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 1, tdRB, ov530, btRB, nil, nil)
	nextID++

	// 6. User-edited adjusted_grid without lock (adjusted_grid != default_grid).
	copyAudioFixture(t, root, "Music/Stream/edited.mp3", "01-lame-cbr-info-44100.mp3")
	btEdited := makeBeatDataBlob(t, 44100, 441000,
		[]testMarker{{-79000, -4, 24, 0}, {460000, 20, 0, 0}},
		[]testMarker{{-78500, -4, 24, 0}, {460500, 20, 0, 0}},
		[]byte{0})
	insertAnalyzerTrack(t, db, nextID, "../Music/Stream/edited.mp3", "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 0, tdRB, ov530, btEdited, nil, nil)
	nextID++

	// 7. Non-MP3 file (FLAC).
	if err := os.WriteFile(filepath.Join(root, "Music/Stream/song.flac"), []byte("fLaC"), 0o644); err != nil {
		t.Fatal(err)
	}
	insertAnalyzerTrack(t, db, nextID, "../Music/Stream/song.flac", "flac", "[FLAC] 16-bit/44100 Hz", 0, tdRB, ov530, btRB, nil, nil)
	nextID++

	// 8. VBRI MP3 (StartOffsetSamples == 0).
	writeVBRIFixture(t, root, "Music/Stream/vbri.mp3")
	tdVBRI := makeTrackDataBlob(t, 48000, 480000, 1, [3]float64{0.6, 0.3, 0.1}, false)
	btVBRI := makeAnalyzerBeatBlob(t, 48000, 480000, -1000, 481000, 16)
	insertAnalyzerTrack(t, db, nextID, "../Music/Stream/vbri.mp3", "mp3", "[192k] MP3 48000 Hz; FLAC unavailable", 0, tdVBRI, ov530, btVBRI, nil, nil)

	rows, err := db.Query(`SELECT p.trackId,p.trackData,p.beatData,p.quickCues,p.loops,t.lastEditTime FROM PerformanceData p JOIN Track t ON t.id=p.trackId ORDER BY p.trackId`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s rowSnap
		if err := rows.Scan(&s.id, &s.track, &s.beat, &s.cues, &s.loops, &s.lastEdited); err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, s)
	}
	rows.Close()
	db.Close()

	rep, err := FixMP3Grids(context.Background(), root, MP3GridOptions{})
	if err != nil {
		t.Fatalf("FixMP3Grids: %v", err)
	}
	// Only the VBRI MP3 passes the SQL + pre-r4 fingerprint filter, and its offset is 0 -> Unchanged=1, Corrected=0.
	if rep.Corrected != 0 || rep.Skipped != 0 || rep.Candidates != 1 || rep.Unchanged != 1 {
		t.Fatalf("unexpected report on untouched classes: %+v", rep)
	}

	ro, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	for _, want := range snaps {
		var tr, bt, qc, lp []byte
		var le sql.NullInt64
		if err := ro.QueryRow(`SELECT p.trackData,p.beatData,p.quickCues,p.loops,t.lastEditTime FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE p.trackId=?`, want.id).
			Scan(&tr, &bt, &qc, &lp, &le); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(tr, want.track) || !bytes.Equal(bt, want.beat) || !bytes.Equal(qc, want.cues) || !bytes.Equal(lp, want.loops) || le != want.lastEdited {
			t.Fatalf("track %d was modified unexpectedly", want.id)
		}
	}

	// Second run must also do nothing.
	rep2, err := FixMP3Grids(context.Background(), root, MP3GridOptions{})
	if err != nil || !rep2.Bypassed || rep2.Corrected != 0 {
		t.Fatalf("second run unexpected: %+v err=%v", rep2, err)
	}
}

func TestFixMP3GridsMissingAudioAndInterruptedResume(t *testing.T) {
	dbPath, root, db := testlibrary.Create(t)
	defer db.Close()

	// 3 valid affected LAME MP3s + 1 missing file + 1 corrupt file.
	for i := int64(1); i <= 3; i++ {
		rel := "Music/Stream/t" + string(rune('0'+i)) + ".mp3"
		copyAudioFixture(t, root, rel, "01-lame-cbr-info-44100.mp3")
		td := makeTrackDataBlob(t, 44100, 441000, 1, [3]float64{0.68, 0.40, 0.18}, false)
		ov := makeOverviewBlob(t, 441000)
		bt := makeAnalyzerBeatBlob(t, 44100, 441000, 5000.0, 445000.0, 20)
		insertAnalyzerTrack(t, db, i, "../"+rel, "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 0, td, ov, bt, nil, nil)
	}
	tdMiss := makeTrackDataBlob(t, 44100, 441000, 1, [3]float64{0.68, 0.40, 0.18}, false)
	ovMiss := makeOverviewBlob(t, 441000)
	btMiss := makeAnalyzerBeatBlob(t, 44100, 441000, 5000.0, 445000.0, 20)
	insertAnalyzerTrack(t, db, 4, "../Music/Stream/missing.mp3", "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 0, tdMiss, ovMiss, btMiss, nil, nil)

	if err := os.WriteFile(filepath.Join(root, "Music/Stream/corrupt.mp3"), []byte("not-mp3-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	insertAnalyzerTrack(t, db, 5, "../Music/Stream/corrupt.mp3", "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 0, tdMiss, ovMiss, btMiss, nil, nil)
	db.Close()

	// Interrupted after 1 batch of 1 track -> no stamp written, track 1 already committed.
	base := time.Unix(1700000000, 0)
	calls := 0
	now := func() time.Time {
		calls++
		if calls >= 4 {
			return base.Add(time.Minute)
		}
		return base
	}
	rep1, err := FixMP3Grids(context.Background(), root, MP3GridOptions{
		Budget:    10 * time.Second,
		BatchSize: 1,
		Now:       now,
	})
	if err != nil {
		t.Fatalf("interrupted run error: %v", err)
	}
	if !rep1.Interrupted || rep1.Corrected != 1 {
		t.Fatalf("expected Interrupted=true Corrected=1, got %+v", rep1)
	}
	stampPath := filepath.Join(filepath.Dir(dbPath), MP3GridStampFile)
	if _, err := os.Stat(stampPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stamp must not be written on interrupted run: %v", err)
	}

	// Resume without budget -> corrects remaining 2 valid tracks (not track 1 again!), skips 2 missing/corrupt, writes stamp.
	rep2, err := FixMP3Grids(context.Background(), root, MP3GridOptions{BatchSize: 2})
	if err != nil {
		t.Fatalf("resume run error: %v", err)
	}
	if rep2.Interrupted || rep2.Corrected != 2 || rep2.Skipped != 2 {
		t.Fatalf("expected Corrected=2 Skipped=2 Interrupted=false, got %+v", rep2)
	}
	stampBytes, err := os.ReadFile(stampPath)
	if err != nil || !strings.Contains(string(stampBytes), "corrected=3") || !strings.Contains(string(stampBytes), "skipped=2") {
		t.Fatalf("expected stamp recording corrected=3 skipped=2, got %q err=%v", stampBytes, err)
	}
	cursorPath := filepath.Join(filepath.Dir(dbPath), MP3GridCursorFile)
	if _, err := os.Stat(cursorPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cursor file must be removed after completion: %v", err)
	}

	ro, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	for i := int64(1); i <= 3; i++ {
		var bt []byte
		if err := ro.QueryRow(`SELECT beatData FROM PerformanceData WHERE trackId=?`, i).Scan(&bt); err != nil {
			t.Fatal(err)
		}
		def, _ := readBeatPositions(t, bt)
		// Track 1 must have been shifted ONCE (5000 - 2257), not twice.
		if math.Abs(def[0]-(5000.0-2257)) > 1e-6 {
			t.Fatalf("track %d beat0=%v, want %v (double-shifted?)", i, def[0], 5000.0-2257)
		}
	}
	ro.Close()

	// Third run must bypass via stamp (no retry loop on missing/corrupt files).
	rep3, err := FixMP3Grids(context.Background(), root, MP3GridOptions{})
	if err != nil || !rep3.Bypassed || rep3.Corrected != 0 || rep3.Skipped != 0 {
		t.Fatalf("third run should bypass: %+v err=%v", rep3, err)
	}
}

func TestFixMP3GridsInterruptedResumeAdvancesPastUnchangedAndSkipped(t *testing.T) {
	dbPath, root, db := testlibrary.Create(t)
	defer db.Close()

	for i := int64(1); i <= 4; i++ {
		rel := "Music/Stream/vbri-" + string(rune('0'+i)) + ".mp3"
		writeVBRIFixture(t, root, rel)
		td := makeTrackDataBlob(t, 48000, 480000, 1, [3]float64{0.6, 0.3, 0.1}, false)
		ov := makeOverviewBlob(t, 480000)
		bt := makeAnalyzerBeatBlob(t, 48000, 480000, 1000.0, 481000.0, 16)
		insertAnalyzerTrack(t, db, i, "../"+rel, "mp3", "[192k] MP3 48000 Hz; FLAC unavailable", 0, td, ov, bt, nil, nil)
	}
	tdMiss := makeTrackDataBlob(t, 44100, 441000, 1, [3]float64{0.68, 0.40, 0.18}, false)
	ovMiss := makeOverviewBlob(t, 441000)
	btMiss := makeAnalyzerBeatBlob(t, 44100, 441000, 5000.0, 445000.0, 20)
	insertAnalyzerTrack(t, db, 5, "../Music/Stream/missing-5.mp3", "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 0, tdMiss, ovMiss, btMiss, nil, nil)

	copyAudioFixture(t, root, "Music/Stream/lame-6.mp3", "01-lame-cbr-info-44100.mp3")
	insertAnalyzerTrack(t, db, 6, "../Music/Stream/lame-6.mp3", "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 0, tdMiss, ovMiss, btMiss, nil, nil)
	db.Close()

	base := time.Unix(1700000000, 0)
	limitedNow := func(allowChecks int) func() time.Time {
		calls := 0
		return func() time.Time {
			calls++
			if calls > allowChecks {
				return base.Add(time.Minute)
			}
			return base
		}
	}

	// Run 1: handles VBRI 1..2, then interrupts before 3 -> cursor saved at 2.
	rep1, err := FixMP3Grids(context.Background(), root, MP3GridOptions{Budget: 10 * time.Second, BatchSize: 2, Now: limitedNow(4)})
	if err != nil || !rep1.Interrupted || rep1.Unchanged != 2 || rep1.Corrected != 0 {
		t.Fatalf("run 1: %+v err=%v", rep1, err)
	}
	// Run 2: resumes at >2, handles VBRI 3..4, interrupts before 5 -> cursor advances to 4.
	rep2, err := FixMP3Grids(context.Background(), root, MP3GridOptions{Budget: 10 * time.Second, BatchSize: 2, Now: limitedNow(4)})
	if err != nil || !rep2.Interrupted || rep2.Unchanged != 2 || rep2.Corrected != 0 {
		t.Fatalf("run 2: %+v err=%v", rep2, err)
	}
	// Run 3: resumes at >4, skips missing 5, corrects LAME 6, writes stamp, drops cursor.
	rep3, err := FixMP3Grids(context.Background(), root, MP3GridOptions{BatchSize: 2})
	if err != nil || rep3.Interrupted || rep3.Skipped != 1 || rep3.Corrected != 1 {
		t.Fatalf("run 3: %+v err=%v", rep3, err)
	}

	stampPath := filepath.Join(filepath.Dir(dbPath), MP3GridStampFile)
	stampBytes, err := os.ReadFile(stampPath)
	if err != nil || !strings.Contains(string(stampBytes), "candidates=6 corrected=1 unchanged=4 skipped=1") {
		t.Fatalf("unexpected stamp: %q err=%v", stampBytes, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dbPath), MP3GridCursorFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cursor file left behind: %v", err)
	}

	ro, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var bt6 []byte
	if err := ro.QueryRow(`SELECT beatData FROM PerformanceData WHERE trackId=6`).Scan(&bt6); err != nil {
		t.Fatal(err)
	}
	def6, _ := readBeatPositions(t, bt6)
	if math.Abs(def6[0]-(5000.0-2257)) > 1e-6 {
		t.Fatalf("track 6 beat0=%v, want %v", def6[0], 5000.0-2257)
	}
}

func TestFixMP3GridsRefusesOpenLibrary(t *testing.T) {
	dbPath, root, db := testlibrary.Create(t)
	copyAudioFixture(t, root, "Music/Stream/101.mp3", "01-lame-cbr-info-44100.mp3")
	td := makeTrackDataBlob(t, 44100, 441000, 1, [3]float64{0.68, 0.40, 0.18}, false)
	ov := makeOverviewBlob(t, 441000)
	bt := makeAnalyzerBeatBlob(t, 44100, 441000, 5000.0, 445000.0, 20)
	insertAnalyzerTrack(t, db, 1, "../Music/Stream/101.mp3", "mp3", "[320k] MP3 44100 Hz; FLAC unavailable", 0, td, ov, bt, nil, nil)
	db.Close()

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	release := holdOpen(t, dbPath)
	rep, err := FixMP3Grids(context.Background(), root, MP3GridOptions{})
	if !errors.Is(err, ErrLibraryInUse) || !rep.Interrupted {
		t.Fatalf("expected ErrLibraryInUse + Interrupted, got rep=%+v err=%v", rep, err)
	}
	release()

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("FixMP3Grids modified open m.db")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dbPath), MP3GridStampFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stamp written while library was open")
	}
}

// #246: completed pages, including those with no UPDATE, survive a hard exit.
func TestFixMP3GridsCursorSurvivesProcessExit(t *testing.T) {
	if root := os.Getenv("ENGINEX_TEST_MP3_CURSOR_ROOT"); root != "" {
		calls := 0
		base := time.Unix(1700000000, 0)
		_, _ = FixMP3Grids(context.Background(), root, MP3GridOptions{Budget: time.Second, BatchSize: 1, Now: func() time.Time {
			calls++
			// The first page has finished; stop before the next page begins.
			if calls == 4 {
				os.Exit(73)
			}
			return base
		}})
		os.Exit(74)
	}
	for _, kind := range []string{"unchanged", "skipped", "corrected"} {
		t.Run(kind, func(t *testing.T) {
			dbPath, root, db := testlibrary.Create(t)
			switch kind {
			case "unchanged":
				writeVBRIFixture(t, root, "Music/Stream/a.mp3")
			case "corrected":
				copyAudioFixture(t, root, "Music/Stream/a.mp3", "01-lame-cbr-info-48000.mp3")
			}
			td := makeTrackDataBlob(t, 48000, 480000, 1, [3]float64{0.6, 0.3, 0.1}, false)
			bt := makeAnalyzerBeatBlob(t, 48000, 480000, 1000, 481000, 16)
			insertAnalyzerTrack(t, db, 1, "../Music/Stream/a.mp3", "mp3", "", 0, td, nil, bt, nil, nil)
			db.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestFixMP3GridsCursorSurvivesProcessExit$")
			cmd.Env = append(os.Environ(), "ENGINEX_TEST_MP3_CURSOR_ROOT="+root)
			output, err := cmd.CombinedOutput()
			if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 73 {
				t.Fatalf("helper: %v %s", err, output)
			}
			cursor := filepath.Join(filepath.Dir(dbPath), MP3GridCursorFile)
			if _, err := os.Stat(cursor); err != nil {
				t.Fatalf("completed page has no persisted cursor after process exit: %v", err)
			}
			if got := readMP3GridCursor(cursor); got.lastID != 1 || got.candidates != 1 {
				t.Fatalf("cursor=%+v, want track 1 processed", got)
			}
			rep, err := FixMP3Grids(context.Background(), root, MP3GridOptions{})
			if err != nil || rep.Candidates != 0 || rep.Interrupted {
				t.Fatalf("resume re-inspected completed page: %+v, %v", rep, err)
			}
		})
	}
}

func TestFixMP3GridsReturnsCursorWriteError(t *testing.T) {
	dbPath, root, db := testlibrary.Create(t)
	writeVBRIFixture(t, root, "Music/Stream/a.mp3")
	td := makeTrackDataBlob(t, 48000, 480000, 1, [3]float64{}, false)
	bt := makeAnalyzerBeatBlob(t, 48000, 480000, 1000, 481000, 16)
	insertAnalyzerTrack(t, db, 1, "../Music/Stream/a.mp3", "mp3", "", 0, td, nil, bt, nil, nil)
	db.Close()
	calls := 0
	_, err := FixMP3Grids(context.Background(), root, MP3GridOptions{Budget: time.Second, BatchSize: 1, Now: func() time.Time {
		calls++
		if calls == 3 {
			// Make the atomic rename fail after initial path validation.
			if err := os.Mkdir(filepath.Join(filepath.Dir(dbPath), MP3GridCursorFile), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return time.Unix(1700000000, 0)
	}})
	if err == nil {
		t.Fatal("cursor write failure was ignored")
	}
}
