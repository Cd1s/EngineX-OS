package enginedb

import (
	"bytes"
	"database/sql"
	"math"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func buildStockOABeats(t *testing.T, sr, samples, beat0Sample, bpm float64, totalBeats int64, editAdjShiftSamples float64, tailBytes int) []byte {
	t.Helper()
	spb := sr * 60.0 / bpm
	p0 := beat0Sample - 4*spb
	b0 := int64(-4)
	b1 := totalBeats
	p1 := beat0Sample + float64(b1)*spb
	next0 := uint32(b1 - b0)

	var b bytes.Buffer
	put(&b, big, sr)
	put(&b, big, samples)
	b.WriteByte(1)
	writeGrid := func(shift float64) {
		put(&b, big, uint64(2))
		put(&b, little, p0+shift)
		put(&b, little, b0)
		put(&b, little, next0)
		put(&b, little, uint32(0))
		put(&b, little, p1+shift)
		put(&b, little, b1)
		put(&b, little, uint32(0))
		put(&b, little, uint32(0xd4a9b300))
	}
	writeGrid(0)
	writeGrid(editAdjShiftSamples)
	b.Write(make([]byte, tailBytes))
	out, err := QCompress(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func buildQuickCuesBlob(t *testing.T, hotPos [8]float64, hotNames [8]string, mainPos float64, mainAdj byte, defPos float64) []byte {
	t.Helper()
	var b bytes.Buffer
	put(&b, big, uint64(8))
	for i := 0; i < 8; i++ {
		label(&b, hotNames[i])
		put(&b, big, hotPos[i])
		b.Write([]byte{0, 0, 0, 0})
	}
	put(&b, big, mainPos)
	b.WriteByte(mainAdj)
	put(&b, big, defPos)
	out, err := QCompress(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func buildLoopsBlob(t *testing.T, slot0Start, slot0End float64, set0, set1 byte, name0 string) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteByte(8)
	b.Write(make([]byte, 7))
	for i := 0; i < 8; i++ {
		if i == 0 {
			label(&b, name0)
			put(&b, little, slot0Start)
			put(&b, little, slot0End)
			b.Write([]byte{set0, set1, 0, 0, 0, 0})
		} else {
			label(&b, "")
			put(&b, little, float64(-1))
			put(&b, little, float64(-1))
			b.Write(make([]byte, 6))
		}
	}
	return b.Bytes()
}

func TestIsStockOverwrittenPerformanceSignatures(t *testing.T) {
	emptyPos := [8]float64{-1, -1, -1, -1, -1, -1, -1, -1}
	var emptyNames [8]string
	emptyLoops := buildLoopsBlob(t, -1, -1, 0, 0, "")

	const sr = 44100.0
	const beat0 = 11391.4248046875 // matches real 136 BPM FLAC auto-cue at beat 0
	oaBeats530 := buildStockOABeats(t, sr, 13707967, beat0, 136.0, 704, 0, 9)
	oaBeats504 := buildStockOABeats(t, sr, 13707967, beat0, 136.0, 704, 0, 1)

	// 1. Stock Engine 5.3.0 OA with auto-cue at beat 0 (main_set=1, main==def==beat0) -> REPAIR.
	qcAutoBeat0 := buildQuickCuesBlob(t, emptyPos, emptyNames, beat0, 1, beat0)
	if !IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530, QuickCues: qcAutoBeat0, Loops: emptyLoops,
	}) {
		t.Fatal("stock 5.3.0 OA row with auto main cue at beat 0 was not recognized")
	}

	// 2. Stock Engine 5.0.4 OA with unadjusted main cue (main_set=0) -> REPAIR.
	qcUnadj := buildQuickCuesBlob(t, emptyPos, emptyNames, 0, 0, 0)
	if !IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats504, QuickCues: qcUnadj, Loops: emptyLoops,
	}) {
		t.Fatal("stock 5.0.4 OA row with unadjusted main cue was not recognized")
	}

	// 3. Aborted/partial load empty-cue stub (NULL td/ov/bd/loops, 26 B empty unadjusted quickCues) -> REPAIR.
	if !IsStockOverwrittenPerformance(StockOverwrittenRow{QuickCues: qcUnadj}) {
		t.Fatal("aborted partial-load empty-cue stub was not recognized")
	}

	// 4. Pure untouched row (all NULL) returns false (handled by normal untouched fill path).
	if IsStockOverwrittenPerformance(StockOverwrittenRow{}) {
		t.Fatal("all-NULL untouched row should not match stock-overwritten signature")
	}

	// 5. Row filled by EngineX EncodePerformance (both normal multi-beat AND tiny 2-beat track) -> NEVER touch.
	for _, nBeats := range []int{2, 40} {
		beats := make([]rekordbox.Beat, nBeats)
		for i := range beats {
			beats[i] = rekordbox.Beat{Number: uint16(i%4) + 1, BPM: 128, TimeMS: uint32(250 + i*469)}
		}
		perf, err := EncodePerformance(
			rekordbox.Track{SampleRate: 44100, Duration: 20, BPM: 128},
			&rekordbox.Analysis{Beats: beats},
		)
		if err != nil {
			t.Fatal(err)
		}
		if IsStockOverwrittenPerformance(StockOverwrittenRow{
			TrackData: perf.TrackData, Overview: perf.Overview, Beats: perf.Beats, QuickCues: perf.QuickCues, Loops: perf.Loops,
		}) {
			t.Fatalf("EngineX-filled %d-beat PQTZ row matched stock-OA signature", nBeats)
		}
		if got := BeatDataFirstBPM(perf.Beats); math.Abs(got-128.0) > 0.3 {
			t.Fatalf("BeatDataFirstBPM(%d beats) = %.3f, want ~128.0", nBeats, got)
		}
	}

	// 6. DJ set Hot Cue 1 (even unnamed, even at sample 0) -> NEVER touch.
	hotPos := emptyPos
	hotPos[0] = 0
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530,
		QuickCues: buildQuickCuesBlob(t, hotPos, emptyNames, beat0, 1, beat0), Loops: emptyLoops,
	}) {
		t.Fatal("DJ hot cue at sample 0 was overwritten")
	}
	// DJ named cue slot -> NEVER touch.
	hotNames := emptyNames
	hotNames[2] = "Drop"
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530,
		QuickCues: buildQuickCuesBlob(t, emptyPos, hotNames, beat0, 1, beat0), Loops: emptyLoops,
	}) {
		t.Fatal("DJ named cue slot was overwritten")
	}

	// 7. DJ moved Main Cue (main != def OR main != beat0) -> NEVER touch.
	for _, tc := range []struct {
		name      string
		main, def float64
	}{
		{"moved_from_default", 220500, beat0},
		{"moved_both_off_beat0", 220500, 220500},
	} {
		if IsStockOverwrittenPerformance(StockOverwrittenRow{
			TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530,
			QuickCues: buildQuickCuesBlob(t, emptyPos, emptyNames, tc.main, 1, tc.def), Loops: emptyLoops,
		}) {
			t.Fatalf("DJ moved main cue (%s) was overwritten", tc.name)
		}
	}

	// 8. DJ saved a Loop or set ActiveOnLoadLoops -> NEVER touch.
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530, QuickCues: qcAutoBeat0,
		Loops: buildLoopsBlob(t, 44100, 88200, 1, 1, ""),
	}) {
		t.Fatal("DJ saved loop was overwritten")
	}
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530, QuickCues: qcAutoBeat0,
		Loops: emptyLoops, ActiveOnLoadLoops: 1,
	}) {
		t.Fatal("DJ active-on-load loop was overwritten")
	}

	// 9. DJ edited Beatgrid (shifted adjusted grid by even 10 samples) -> NEVER touch.
	oaEdited := buildStockOABeats(t, sr, 13707967, beat0, 136.0, 704, 10.0, 9)
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaEdited, QuickCues: qcAutoBeat0, Loops: emptyLoops,
	}) {
		t.Fatal("DJ-edited beatgrid was overwritten")
	}

	// 10. DJ locked Beatgrid -> NEVER touch.
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530, QuickCues: qcAutoBeat0,
		Loops: emptyLoops, BeatGridLocked: true,
	}) {
		t.Fatal("DJ-locked beatgrid was overwritten")
	}

	// 11. ThirdPartySourceId != NULL -> NEVER touch via repair.
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530, QuickCues: qcAutoBeat0,
		Loops: emptyLoops, ThirdPartySourceID: sql.NullInt64{Int64: 1, Valid: true},
	}) {
		t.Fatal("marked thirdPartySourceId row matched stock-OA repair")
	}

	// 12. Corrupt blobs fail closed -> NEVER touch.
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: []byte("not-zlib"), QuickCues: qcAutoBeat0, Loops: emptyLoops,
	}) {
		t.Fatal("corrupt beatData did not fail closed")
	}

	// 13. Alternative stock Engine OA version with marker[0].beat == 0 and beats_until_next > 1 -> REPAIR.
	var b0Zero bytes.Buffer
	put(&b0Zero, big, sr)
	put(&b0Zero, big, float64(44100*60))
	b0Zero.WriteByte(1)
	for g := 0; g < 2; g++ {
		put(&b0Zero, big, uint64(2))
		put(&b0Zero, little, beat0)
		put(&b0Zero, little, int64(0))
		put(&b0Zero, little, uint32(128))
		put(&b0Zero, little, uint32(0))
		put(&b0Zero, little, beat0+128*(sr*60.0/128.0))
		put(&b0Zero, little, int64(128))
		put(&b0Zero, little, uint32(0))
		put(&b0Zero, little, uint32(0))
	}
	b0Zero.WriteByte(0)
	oaBeat0Zero, err := QCompress(b0Zero.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeat0Zero, QuickCues: qcAutoBeat0, Loops: emptyLoops,
	}) {
		t.Fatal("stock OA 2-marker grid starting at beat 0 was not recognized")
	}

	// 14. OneLibrary fast-import baseline: untouched rekordbox cues/loops + stock OA -> REPAIR,
	//     including MP3 2292-sample Engine load shift; ANY DJ edit/add/delete/toggle -> NEVER touch.
	fastTrack := rekordbox.Track{
		SampleRate: 44100,
		Duration:   180,
		BPM:        136,
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 1000, Name: "Intro"},
			{HotCue: 2, TimeMS: 2000, EndMS: 4000, Loop: true, Active: true, Name: "LoopA"},
		},
	}
	fb := FastBaseForTrack(fastTrack)
	// Exact FastBase + stock OA -> REPAIR.
	if !IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530,
		QuickCues: fb.Cues, Loops: fb.Loops, ActiveOnLoadLoops: int64(fb.Active),
		FastKnown: true, FastCues: fb.Cues, FastLoops: fb.Loops, FastActive: fb.Active,
	}) {
		t.Fatal("untouched OneLibrary FastBase + stock OA was not recognized")
	}
	// MP3 load shift (-2292 samples on cues & loops) + stock OA -> REPAIR.
	shiftTrack := fastTrack
	shiftTrack.StartOffsetSamples = 2292
	shifted, err := encodeCuesOnly(shiftTrack, shiftTrack.Cues)
	if err != nil {
		t.Fatal(err)
	}
	if !IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530,
		QuickCues: shifted.QuickCues, Loops: shifted.Loops, ActiveOnLoadLoops: int64(shifted.ActiveLoops),
		FastKnown: true, FastCues: fb.Cues, FastLoops: fb.Loops, FastActive: fb.Active,
	}) {
		t.Fatal("MP3-shifted OneLibrary FastBase + stock OA was not recognized")
	}
	// DJ toggled activeOnLoadLoops on OneLibrary track -> NEVER touch.
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530,
		QuickCues: fb.Cues, Loops: fb.Loops, ActiveOnLoadLoops: 0,
		FastKnown: true, FastCues: fb.Cues, FastLoops: fb.Loops, FastActive: fb.Active,
	}) {
		t.Fatal("DJ active-loop toggle on OneLibrary row was overwritten")
	}
	// DJ added Hot Cue 3 on OneLibrary track -> NEVER touch.
	editedTrack := fastTrack
	editedTrack.Cues = append(append([]rekordbox.Cue(nil), fastTrack.Cues...), rekordbox.Cue{HotCue: 3, TimeMS: 8000, Name: "DJ"})
	djAdded := FastBaseForTrack(editedTrack)
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530,
		QuickCues: djAdded.Cues, Loops: fb.Loops, ActiveOnLoadLoops: int64(fb.Active),
		FastKnown: true, FastCues: fb.Cues, FastLoops: fb.Loops, FastActive: fb.Active,
	}) {
		t.Fatal("DJ hot cue added to OneLibrary row was overwritten")
	}
	// DJ deleted Hot Cue 1 on OneLibrary track -> NEVER touch.
	deletedTrack := fastTrack
	deletedTrack.Cues = fastTrack.Cues[1:]
	djDeleted := FastBaseForTrack(deletedTrack)
	if IsStockOverwrittenPerformance(StockOverwrittenRow{
		TrackData: []byte{1}, Overview: []byte{1}, Beats: oaBeats530,
		QuickCues: djDeleted.Cues, Loops: fb.Loops, ActiveOnLoadLoops: int64(fb.Active),
		FastKnown: true, FastCues: fb.Cues, FastLoops: fb.Loops, FastActive: fb.Active,
	}) {
		t.Fatal("DJ hot cue deletion on OneLibrary row was overwritten")
	}
}
