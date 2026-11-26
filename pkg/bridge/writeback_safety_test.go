package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

// buildSyntheticMP3 builds a valid MPEG-1 Layer III stream with optional Xing/Info +
// encoder tag and LAME 12-bit delay/padding, matching audioheader.ReadMP3 rules.
func buildSyntheticMP3(srIdx int, tag, encoder string, delay, pad int, audioFrames int) []byte {
	// MPEG1 (3), Layer III (1), no CRC (1), 320 kbps (14 -> idx 14), srIdx (0=44100, 1=48000), stereo (0)
	h := uint32(0xffe00000) | (3 << 19) | (1 << 17) | (1 << 16) | (14 << 12) | (uint32(srIdx) << 10)
	sr := 44100
	if srIdx == 1 {
		sr = 48000
	}
	frameSize := (320 * 144000) / sr
	makeFrame := func() []byte {
		f := make([]byte, frameSize)
		binary.BigEndian.PutUint32(f, h)
		return f
	}
	var out []byte
	if tag != "" {
		f0 := makeFrame()
		p := 4 + 32 // MPEG-1 stereo side info
		copy(f0[p:p+4], tag)
		binary.BigEndian.PutUint32(f0[p+4:p+8], 0x03) // FRAMES | SIZE
		binary.BigEndian.PutUint32(f0[p+8:p+12], uint32(audioFrames))
		binary.BigEndian.PutUint32(f0[p+12:p+16], uint32(frameSize*(audioFrames+1)))
		p += 16
		if encoder != "" {
			copy(f0[p:p+9], encoder)
			v := uint32(delay&0xfff)<<12 | uint32(pad&0xfff)
			f0[p+21] = byte(v >> 16)
			f0[p+22] = byte(v >> 8)
			f0[p+23] = byte(v)
		}
		out = append(out, f0...)
	}
	for range audioFrames {
		out = append(out, makeFrame()...)
	}
	return out
}

type wbFixtureTrack struct {
	contentID uint32
	title     string
	relPath   string
	anlzBase  string // e.g. "PIONEER/USBANLZ/P001/ANLZ0000"
	audio     []byte
	cues      []rekordbox.Cue
	bpmx100   int
	lengthSec int
	sr        int
}

func makeOneLibraryStick(t *testing.T, tracks []wbFixtureTrack) (mount, expPath string) {
	t.Helper()
	mount = t.TempDir()
	expPath = filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")
	if err := os.MkdirAll(filepath.Dir(expPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: expPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		PRAGMA journal_mode=DELETE;
		CREATE TABLE content(
			content_id INTEGER PRIMARY KEY,
			title TEXT,
			path TEXT,
			bpmx100 INTEGER,
			length INTEGER,
			samplingRate INTEGER,
			analysisDataFilePath TEXT,
			hasModified INTEGER,
			cueUpdateCount INTEGER
		);
		CREATE TABLE cue(
			cue_id INTEGER PRIMARY KEY AUTOINCREMENT,
			content_id INTEGER,
			kind INTEGER,
			colorTableIndex INTEGER,
			cueComment TEXT,
			isActiveLoop INTEGER,
			beatLoopNumerator INTEGER,
			beatLoopDenominator INTEGER,
			inUsec INTEGER,
			outUsec INTEGER,
			in150FramePerSec INTEGER,
			out150FramePerSec INTEGER
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range tracks {
		testfixture.Write(t, mount, tr.relPath, tr.audio)
		datBytes, _, err := rekordbox.RewriteANLZCues(testfixture.DAT(), tr.cues, false, false)
		if err != nil {
			t.Fatal(err)
		}
		extBytes, _, err := rekordbox.RewriteANLZCues(testfixture.EXT(), tr.cues, true, false)
		if err != nil {
			t.Fatal(err)
		}
		testfixture.Write(t, mount, tr.anlzBase+".DAT", datBytes)
		testfixture.Write(t, mount, tr.anlzBase+".EXT", extBytes)
		if _, err := db.Exec(`INSERT INTO content VALUES(?, ?, ?, ?, ?, ?, ?, 0, NULL)`,
			tr.contentID, tr.title, "/"+tr.relPath, tr.bpmx100, tr.lengthSec, tr.sr, "/"+tr.anlzBase+".DAT"); err != nil {
			t.Fatal(err)
		}
		for _, c := range tr.cues {
			outUsec := int64(-1)
			out150 := int64(-1)
			if c.Loop && c.EndMS > c.TimeMS {
				outUsec = int64(c.EndMS) * 1000
				out150 = (int64(c.EndMS)*150 + 500) / 1000
			}
			active := 0
			if c.Loop && c.Active {
				active = 1
			}
			colorIdx := int(c.ColorCode)
			if c.HotCue == 0 {
				colorIdx = int(c.ColorID)
			}
			in150 := (int64(c.TimeMS)*150 + 500) / 1000
			if _, err := db.Exec(`INSERT INTO cue(content_id,kind,colorTableIndex,cueComment,isActiveLoop,beatLoopNumerator,beatLoopDenominator,inUsec,outUsec,in150FramePerSec,out150FramePerSec) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				tr.contentID, c.HotCue, colorIdx, c.Name, active, c.LoopNumerator, c.LoopDenominator, int64(c.TimeMS)*1000, outUsec, in150, out150); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{}); err != nil {
		t.Fatalf("ConvertAtMount: %v", err)
	}
	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatalf("FillInBackground: %v", err)
	}
	if res, err := ConvertAtMount(context.Background(), mount, MountOptions{Publish: true}); err != nil || (!res.Pending.None() && !res.Bypassed) {
		t.Fatalf("publish ConvertAtMount: res=%+v err=%v", res, err)
	}
	return mount, expPath
}

func makeRawQuickCuesBlob(t *testing.T, slots [8]float64, names [8]string, colors [8][4]byte, mainPos float64, flag byte, defPos float64) []byte {
	t.Helper()
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint64(8))
	for i := range 8 {
		buf.WriteByte(byte(len(names[i])))
		buf.WriteString(names[i])
		_ = binary.Write(&buf, binary.BigEndian, slots[i])
		buf.Write(colors[i][:])
	}
	_ = binary.Write(&buf, binary.BigEndian, mainPos)
	buf.WriteByte(flag)
	_ = binary.Write(&buf, binary.BigEndian, defPos)
	out, err := enginedb.QCompress(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readAllANLZCues(t *testing.T, mount, anlzBase string) []rekordbox.Cue {
	t.Helper()
	var merged *rekordbox.Analysis
	for _, ext := range []string{".DAT", ".EXT"} {
		a, err := rekordbox.ReadAnalysis(filepath.Join(mount, filepath.FromSlash(anlzBase+ext)))
		if err != nil {
			t.Fatalf("ReadAnalysis(%s%s): %v", anlzBase, ext, err)
		}
		if merged == nil {
			merged = a
		} else {
			merged.Merge(a)
		}
	}
	return merged.Cues
}

// Item 4(a) fail-before / pass-after proof (exact content 76 / track 3222 reproduction):
// A 48 kHz Info+Lavc MP3 (RekordboxOffset = 1105 samples = 23.02 ms) with 0 cues in
// rekordbox and all 8 Engine quickCues slots at -420.0 (main=-420.0, def=-420.0) must
// parse to 0 deck hot cues and cause ZERO write-back (never emitting 8 phantom hot cues
// A..H at 0.023 s).
func TestWriteBackNegative420QuickCuesProducesZeroPhantomCues(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()

	mp3_48k := buildSyntheticMP3(1, "Info", "Lavc58.13", 576, 1601, 200)
	mount, expPath := makeOneLibraryStick(t, []wbFixtureTrack{{
		contentID: 76,
		title:     "Content76Shape",
		relPath:   "Contents/Artist/track76.mp3",
		anlzBase:  "PIONEER/USBANLZ/P042/ANLZ0000",
		audio:     mp3_48k,
		cues:      nil, // 0 cues originally in rekordbox
		bpmx100:   13000,
		lengthSec: 435,
		sr:        48000,
	}})

	// Verify exact MP3 offset matches real content 76 (1105 samples = 23.02 ms at 48 kHz).
	info, err := compat.AudioHeader(filepath.Join(mount, "Contents/Artist/track76.mp3"))
	if err != nil || info.SampleRate != 48000 || info.RekordboxOffset() != 1105 {
		t.Fatalf("audio header = (%+v, %v), want sr=48000 RekordboxOffset=1105", info, err)
	}

	// Inject exact kdata-engine track 3222 quickCues: Cue 1..Cue 8 at -420.0 + default palette.
	colors := [8][4]byte{
		{0xff, 0xf4, 0xd3, 0x38}, {0xff, 0xef, 0x81, 0x30}, {0xff, 0xaa, 0x55, 0xc4}, {0xff, 0xce, 0x32, 0x39},
		{0xff, 0x86, 0xc6, 0x4b}, {0xff, 0x20, 0xc6, 0x70}, {0xff, 0x00, 0xa8, 0xa9}, {0xff, 0x15, 0x71, 0xe2},
	}
	var names [8]string
	var slots [8]float64
	for i := range 8 {
		names[i] = fmt.Sprintf("Cue %d", i+1)
		slots[i] = -420.0
	}
	qc := makeRawQuickCuesBlob(t, slots, names, colors, -420.0, 1, -420.0)

	db := p4DB(t, mount)
	id := p4TrackID(t, db)
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, qc, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// Clear Written and Synced in snapshot to mirror track 3222's legacy take-over state (BridgeWritten absent).
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range snap.Tracks {
		tr.Written = nil
		tr.Synced = nil
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}

	// Dry-run and real pass must both report 0 changes and leave ANLZ + exportLibrary.db untouched.
	dry, err := WriteBackCues(ctx, mount, WriteBackOptions{SkipPreparingCheck: true, DryRun: true})
	if err != nil || dry.TracksChanged != 0 {
		t.Fatalf("dry-run on -420.0 quickCues reported false diff: res=%+v err=%v", dry, err)
	}
	res, err := WriteBackCues(ctx, mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res.TracksChanged != 0 || res.FilesUpdated != 0 || res.OneLibraryUpdated {
		t.Fatalf("write-back on -420.0 quickCues produced phantom cues: res=%+v err=%v", res, err)
	}
	if cues := readAllANLZCues(t, mount, "PIONEER/USBANLZ/P042/ANLZ0000"); len(cues) != 0 {
		t.Fatalf("ANLZ acquired phantom cues: %+v", cues)
	}
	one, err := rekordbox.ReadSQLite(ctx, expPath, nil)
	if err != nil || len(one.Tracks) != 1 || len(one.Tracks[0].Cues) != 0 {
		t.Fatalf("OneLibrary acquired phantom cues: tracks=%+v err=%v", one, err)
	}
}

func clearQuickCueSlots(t *testing.T, db *sql.DB, trackID int64, slots ...int) {
	t.Helper()
	clearSet := map[int]bool{}
	for _, s := range slots {
		clearSet[s] = true
	}
	var blob []byte
	if err := db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=?`, trackID).Scan(&blob); err != nil {
		t.Fatalf("read quickCues for track %d: %v", trackID, err)
	}
	raw, err := enginedb.QUncompress(blob, 64<<10)
	if err != nil || len(raw) < 8 || binary.BigEndian.Uint64(raw) != 8 {
		t.Fatalf("invalid quickCues blob for track %d: err=%v len=%d", trackID, err, len(raw))
	}
	out := append([]byte(nil), raw[:8]...)
	off := 8
	var negOne [8]byte
	binary.BigEndian.PutUint64(negOne[:], math.Float64bits(-1))
	for i := range 8 {
		if off >= len(raw) || off+1+int(raw[off])+12 > len(raw) {
			t.Fatalf("truncated quickCues slot %d", i)
		}
		end := off + 1 + int(raw[off]) + 12
		if !clearSet[i] {
			out = append(out, raw[off:end]...)
		} else {
			out = append(out, 0)            // empty label
			out = append(out, negOne[:]...) // position = -1.0 (unset)
			out = append(out, 0, 0, 0, 0)   // cleared color
		}
		off = end
	}
	out = append(out, raw[off:]...)
	enc, err := enginedb.QCompress(out)
	if err != nil {
		t.Fatalf("compress quickCues: %v", err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, enc, trackID); err != nil {
		t.Fatalf("update quickCues: %v", err)
	}
}

// Item 4(b) proof: deleting ONE hot cue or deleting ALL 8 hot cues on a deck must propagate
// cleanly as a deletion to .DAT, .EXT, and exportLibrary.db (preserving any memory cues).
func TestWriteBackDeckDeletionOneAndAllEightHotCues(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()

	var eightHot []rekordbox.Cue
	for s := uint32(1); s <= 8; s++ {
		eightHot = append(eightHot, rekordbox.Cue{
			HotCue:    s,
			TimeMS:    s * 200, // 200..1600 ms, all within testfixture.WAV(false)'s 2000 ms duration
			Name:      fmt.Sprintf("Pad%d", s),
			Color:     [4]byte{255, 0x30, 0x5a, 0xff},
			ColorCode: 1,
		})
	}
	withMem := append([]rekordbox.Cue{{HotCue: 0, TimeMS: 100, Name: "Mem0", ColorID: 2}}, eightHot...)

	mount, expPath := makeOneLibraryStick(t, []wbFixtureTrack{
		{
			contentID: 101,
			title:     "WithMemoryAndEightHot",
			relPath:   "Contents/del/track1.wav",
			anlzBase:  "PIONEER/USBANLZ/P001/ANLZ0000",
			audio:     testfixture.WAV(false),
			cues:      withMem,
			bpmx100:   12000,
			lengthSec: 2,
			sr:        48000,
		},
		{
			contentID: 102,
			title:     "OnlyEightHot",
			relPath:   "Contents/del/track2.wav",
			anlzBase:  "PIONEER/USBANLZ/P002/ANLZ0000",
			audio:     testfixture.WAV(false),
			cues:      eightHot,
			bpmx100:   12000,
			lengthSec: 2,
			sr:        48000,
		},
	})

	lookupTrackID := func(rel string) int64 {
		db := p4DB(t, mount)
		defer db.Close()
		var id int64
		if err := db.QueryRow(`SELECT id FROM Track WHERE path=?`, "../"+rel).Scan(&id); err != nil {
			t.Fatalf("lookup %s: %v", rel, err)
		}
		return id
	}
	id1 := lookupTrackID("Contents/del/track1.wav")
	id2 := lookupTrackID("Contents/del/track2.wav")

	// Step 1: Delete ONE hot cue (Pad 4 / slot 3) on track 1 -> 7 hot cues + 1 memory cue remain.
	db := p4DB(t, mount)
	clearQuickCueSlots(t, db, id1, 3)
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+10 WHERE id=?`, id1); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	res1, err := WriteBackCues(ctx, mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res1.TracksChanged != 1 || !res1.OneLibraryUpdated {
		t.Fatalf("single hot cue deletion write-back failed: res=%+v err=%v", res1, err)
	}
	cues1 := readAllANLZCues(t, mount, "PIONEER/USBANLZ/P001/ANLZ0000")
	var hotCount1, memCount1 int
	for _, c := range cues1 {
		if c.HotCue == 4 {
			t.Fatalf("deleted hot cue 4 still present in ANLZ: %+v", cues1)
		}
		if c.HotCue > 0 {
			hotCount1++
		} else {
			memCount1++
		}
	}
	if hotCount1 != 7 || memCount1 != 1 {
		t.Fatalf("after deleting 1 hot cue: hot=%d mem=%d (want 7, 1); cues=%+v", hotCount1, memCount1, cues1)
	}

	// Step 2: Delete ALL 8 hot cues on BOTH track 1 (keeps memory cue) and track 2 (0 cues total).
	db = p4DB(t, mount)
	for _, tid := range []int64{id1, id2} {
		clearQuickCueSlots(t, db, tid, 0, 1, 2, 3, 4, 5, 6, 7)
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+20 WHERE id=?`, tid); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	res2, err := WriteBackCues(ctx, mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res2.TracksChanged != 2 || !res2.OneLibraryUpdated {
		t.Fatalf("delete-all-8 write-back failed: res=%+v err=%v", res2, err)
	}

	// Track 1: 0 hot cues, 1 memory cue preserved in both ANLZ and OneLibrary.
	anlz1 := readAllANLZCues(t, mount, "PIONEER/USBANLZ/P001/ANLZ0000")
	for _, c := range anlz1 {
		if c.HotCue != 0 {
			t.Fatalf("track 1 still has hot cue after deleting all 8: %+v", anlz1)
		}
	}
	if len(anlz1) != 1 {
		t.Fatalf("track 1 memory cue not preserved: %+v", anlz1)
	}
	// Track 2: 0 cues total in both ANLZ (.DAT and .EXT) and OneLibrary.
	if anlz2 := readAllANLZCues(t, mount, "PIONEER/USBANLZ/P002/ANLZ0000"); len(anlz2) != 0 {
		t.Fatalf("track 2 still has cues in ANLZ after deleting all 8: %+v", anlz2)
	}
	one, err := rekordbox.ReadSQLite(ctx, expPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uint32]rekordbox.Track{}
	for _, tr := range one.Tracks {
		byID[tr.ID] = tr
	}
	if len(byID[101].Cues) != 1 || byID[101].Cues[0].HotCue != 0 {
		t.Fatalf("OneLibrary track 101 cues = %+v, want 1 memory cue only", byID[101].Cues)
	}
	if len(byID[102].Cues) != 0 {
		t.Fatalf("OneLibrary track 102 cues = %+v, want empty after deleting all 8", byID[102].Cues)
	}

	// Verify exportLibrary.db cue table has 0 rows for content 102 and hasModified=1.
	expDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: expPath, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer expDB.Close()
	var cueRows, hasMod, updCnt int
	if err := expDB.QueryRow(`SELECT count(*) FROM cue WHERE content_id=102`).Scan(&cueRows); err != nil || cueRows != 0 {
		t.Fatalf("cue table rows for content 102 = %d (err=%v), want 0", cueRows, err)
	}
	if err := expDB.QueryRow(`SELECT hasModified, cueUpdateCount FROM content WHERE content_id=102`).Scan(&hasMod, &updCnt); err != nil || hasMod != 1 || updCnt < 1 {
		t.Fatalf("content 102 marks = (hasModified=%d, cueUpdateCount=%d, err=%v), want (1, >=1)", hasMod, updCnt, err)
	}

	// Follow-up pass must converge to 0 changes.
	res3, err := WriteBackCues(ctx, mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res3.TracksChanged != 0 || res3.FilesUpdated != 0 {
		t.Fatalf("post-deletion convergence failed: res=%+v err=%v", res3, err)
	}
}

// Item 4(c) proof (#318 MP3 encoder-delay offsets): verifies RekordboxOffset() across
// Info+Lavc 48 kHz (+23 ms), Info+Lavf 44.1 kHz (+25 ms), lowercase Info+Lame3.100 44.1 kHz
// (+26 ms Xing-only, preserving a 20 ms cue clamped at sample 0 in Engine with zero spurious
// diff — content 5373 shape), Xing+Lavc 44.1 kHz (+51 ms), and headerless MP3 (+0 ms).
func TestWriteBackMP3EncoderDelayOffsetsIssue318(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()

	cases := []struct {
		id              uint32
		name            string
		srIdx           int
		sr              int
		tag             string
		enc             string
		wantStartOffset int
		wantRBOffset    int
		origCueMS       uint32
		newDeckCueMS    float64
		wantWrittenMS   uint32
	}{
		{
			id: 401, name: "info_lavc_48k", srIdx: 1, sr: 48000, tag: "Info", enc: "Lavc58.13",
			wantStartOffset: 2257, wantRBOffset: 1105, origCueMS: 500,
			newDeckCueMS: 1000.0, wantWrittenMS: uint32(math.Round(1000.0 + 1105.0*1000.0/48000.0)), // 1023 ms (+23 ms)
		},
		{
			id: 402, name: "info_lavf_44k", srIdx: 0, sr: 44100, tag: "Info", enc: "Lavf56.4.",
			wantStartOffset: 2257, wantRBOffset: 1105, origCueMS: 500,
			newDeckCueMS: 1000.0, wantWrittenMS: uint32(math.Round(1000.0 + 1105.0*1000.0/44100.0)), // 1025 ms (+25 ms)
		},
		{
			// Content 5373 shape: lowercase "Lame3.100" does not match FFmpeg's case-sensitive
			// LAME/Lavf/Lavc check, so Engine trims only the Info frame (1152 samples = 26.12 ms).
			// An original 20 ms cue clamps to sample 0 on import and must stay 20 ms on write-back.
			id: 5373, name: "info_lowercase_lame_44k", srIdx: 0, sr: 44100, tag: "Info", enc: "Lame3.100",
			wantStartOffset: 1152, wantRBOffset: 1152, origCueMS: 20,
			newDeckCueMS: 1000.0, wantWrittenMS: uint32(math.Round(1000.0 + 1152.0*1000.0/44100.0)), // 1026 ms (+26 ms)
		},
		{
			id: 404, name: "xing_lavc_44k", srIdx: 0, sr: 44100, tag: "Xing", enc: "Lavc58.13",
			wantStartOffset: 2257, wantRBOffset: 2257, origCueMS: 500,
			newDeckCueMS: 1000.0, wantWrittenMS: uint32(math.Round(1000.0 + 2257.0*1000.0/44100.0)), // 1051 ms (+51 ms)
		},
		{
			id: 405, name: "headerless_44k", srIdx: 0, sr: 44100, tag: "", enc: "",
			wantStartOffset: 0, wantRBOffset: 0, origCueMS: 500,
			newDeckCueMS: 1000.0, wantWrittenMS: 1000, // +0 ms
		},
	}

	var tracks []wbFixtureTrack
	for i, tc := range cases {
		tracks = append(tracks, wbFixtureTrack{
			contentID: tc.id,
			title:     tc.name,
			relPath:   fmt.Sprintf("Contents/mp3/%s.mp3", tc.name),
			anlzBase:  fmt.Sprintf("PIONEER/USBANLZ/P%03d/ANLZ0000", i+1),
			audio:     buildSyntheticMP3(tc.srIdx, tc.tag, tc.enc, 576, 800, 120),
			cues: []rekordbox.Cue{
				{HotCue: 1, TimeMS: tc.origCueMS, Name: "Orig", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
			},
			bpmx100:   12000,
			lengthSec: 3,
			sr:        tc.sr,
		})
	}
	mount, expPath := makeOneLibraryStick(t, tracks)

	// Verify audio header classification matches expected StartOffsetSamples / RekordboxOffset().
	for _, tc := range cases {
		info, err := compat.AudioHeader(filepath.Join(mount, "Contents/mp3", tc.name+".mp3"))
		if err != nil {
			t.Fatalf("%s AudioHeader: %v", tc.name, err)
		}
		if info.StartOffsetSamples != tc.wantStartOffset || info.RekordboxOffset() != tc.wantRBOffset {
			t.Fatalf("%s offsets = (start=%d, rb=%d), want (%d, %d)",
				tc.name, info.StartOffsetSamples, info.RekordboxOffset(), tc.wantStartOffset, tc.wantRBOffset)
		}
	}

	// Clear Written/Synced and bump lastEditTime on all 5 tracks: untouched cues (including
	// content 5373's 20 ms cue clamped to 0.0 in Engine) must produce ZERO spurious write-backs.
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range snap.Tracks {
		tr.Written = nil
		tr.Synced = nil
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	db := p4DB(t, mount)
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+50`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	untouched, err := WriteBackCues(ctx, mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || untouched.TracksChanged != 0 {
		t.Fatalf("untouched #318 MP3 tracks caused spurious write-back: res=%+v err=%v", untouched, err)
	}

	// Add Hot Cue B at 1000.0 ms (Engine frame) on each track and verify write-back restores
	// each track's exact RekordboxOffset() while preserving Orig (including 5373's 20 ms cue).
	db = p4DB(t, mount)
	for _, tc := range cases {
		var tid int64
		if err := db.QueryRow(`SELECT id FROM Track WHERE path=?`, "../Contents/mp3/"+tc.name+".mp3").Scan(&tid); err != nil {
			t.Fatalf("lookup %s: %v", tc.name, err)
		}
		pos := tc.newDeckCueMS
		if err := enginedb.SetQuickCue(ctx, db, tid, 1, "DeckB", "#00ff00", &pos); err != nil {
			t.Fatalf("SetQuickCue %s: %v", tc.name, err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=?`, tid); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	res, err := WriteBackCues(ctx, mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res.TracksChanged != len(cases) || !res.OneLibraryUpdated {
		t.Fatalf("MP3 offset write-back: res=%+v err=%v", res, err)
	}
	one, err := rekordbox.ReadSQLite(ctx, expPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uint32]rekordbox.Track{}
	for _, tr := range one.Tracks {
		byID[tr.ID] = tr
	}
	for _, tc := range cases {
		tr := byID[tc.id]
		var gotOrig, gotB *rekordbox.Cue
		for i := range tr.Cues {
			c := &tr.Cues[i]
			if c.HotCue == 1 {
				gotOrig = c
			} else if c.HotCue == 2 {
				gotB = c
			}
		}
		if gotOrig == nil || gotOrig.TimeMS != tc.origCueMS {
			t.Fatalf("%s Orig cue = %+v, want TimeMS=%d", tc.name, gotOrig, tc.origCueMS)
		}
		if gotB == nil || gotB.TimeMS != tc.wantWrittenMS {
			t.Fatalf("%s DeckB cue = %+v, want TimeMS=%d", tc.name, gotB, tc.wantWrittenMS)
		}
	}
}

// Item 4(d) proof (Content 4790 / Engine track 5479 shape): two tracks sharing the exact
// same filename in different folders must be mapped strictly by full relative path during
// write-back so editing one never writes cues onto its duplicate-filename sibling.
func TestWriteBackDuplicateFilenameMappedStrictlyByPath(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()

	mp3 := buildSyntheticMP3(0, "Info", "Lavc57.10", 576, 942, 120)
	mount, expPath := makeOneLibraryStick(t, []wbFixtureTrack{
		{
			contentID: 3466,
			title:     "SameFileFolderA",
			relPath:   "Contents/FolderA/DuplicateTrack.mp3",
			anlzBase:  "PIONEER/USBANLZ/P010/ANLZ0000",
			audio:     mp3,
			cues:      nil,
			bpmx100:   12000,
			lengthSec: 3,
			sr:        44100,
		},
		{
			contentID: 4790,
			title:     "SameFileFolderB",
			relPath:   "Contents/FolderB/DuplicateTrack.mp3",
			anlzBase:  "PIONEER/USBANLZ/P020/ANLZ0000",
			audio:     mp3,
			cues:      nil,
			bpmx100:   12000,
			lengthSec: 3,
			sr:        44100,
		},
	})

	origA_DAT, err := os.ReadFile(filepath.Join(mount, "PIONEER/USBANLZ/P010/ANLZ0000.DAT"))
	if err != nil {
		t.Fatal(err)
	}
	origA_EXT, err := os.ReadFile(filepath.Join(mount, "PIONEER/USBANLZ/P010/ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}

	// Edit ONLY FolderB's track (content 4790) in m.db.
	db := p4DB(t, mount)
	var idB int64
	if err := db.QueryRow(`SELECT id FROM Track WHERE path=?`, "../Contents/FolderB/DuplicateTrack.mp3").Scan(&idB); err != nil {
		t.Fatal(err)
	}
	posMS := 1500.0
	if err := enginedb.SetQuickCue(ctx, db, idB, 0, "FolderBOnly", "#00ff00", &posMS); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+50 WHERE id=?`, idB); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	res, err := WriteBackCues(ctx, mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if err != nil || res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("duplicate-filename write-back: res=%+v err=%v", res, err)
	}

	// FolderA's ANLZ files (content 3466) must remain byte-for-byte untouched.
	afterA_DAT, err := os.ReadFile(filepath.Join(mount, "PIONEER/USBANLZ/P010/ANLZ0000.DAT"))
	if err != nil || !bytes.Equal(origA_DAT, afterA_DAT) {
		t.Fatalf("FolderA DAT modified by FolderB edit: err=%v", err)
	}
	afterA_EXT, err := os.ReadFile(filepath.Join(mount, "PIONEER/USBANLZ/P010/ANLZ0000.EXT"))
	if err != nil || !bytes.Equal(origA_EXT, afterA_EXT) {
		t.Fatalf("FolderA EXT modified by FolderB edit: err=%v", err)
	}

	// FolderB's ANLZ (content 4790) must carry Hot Cue A at 1500 + 25 = 1525 ms.
	cuesB := readAllANLZCues(t, mount, "PIONEER/USBANLZ/P020/ANLZ0000")
	if len(cuesB) != 1 || cuesB[0].HotCue != 1 || cuesB[0].Name != "FolderBOnly" || cuesB[0].TimeMS != 1525 {
		t.Fatalf("FolderB ANLZ cues = %+v, want 1 hot cue A at 1525 ms", cuesB)
	}

	// OneLibrary must update ONLY content_id 4790 and leave content_id 3466 unmodified.
	one, err := rekordbox.ReadSQLite(ctx, expPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uint32]rekordbox.Track{}
	for _, tr := range one.Tracks {
		byID[tr.ID] = tr
	}
	if len(byID[3466].Cues) != 0 {
		t.Fatalf("content 3466 received FolderB's cue: %+v", byID[3466].Cues)
	}
	if len(byID[4790].Cues) != 1 || byID[4790].Cues[0].TimeMS != 1525 {
		t.Fatalf("content 4790 cues = %+v, want 1 cue at 1525 ms", byID[4790].Cues)
	}
	expDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: expPath, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer expDB.Close()
	var modA, modB int
	if err := expDB.QueryRow(`SELECT hasModified FROM content WHERE content_id=3466`).Scan(&modA); err != nil || modA != 0 {
		t.Fatalf("content 3466 hasModified = %d (err=%v), want 0", modA, err)
	}
	if err := expDB.QueryRow(`SELECT hasModified FROM content WHERE content_id=4790`).Scan(&modB); err != nil || modB != 1 {
		t.Fatalf("content 4790 hasModified = %d (err=%v), want 1", modB, err)
	}
}
