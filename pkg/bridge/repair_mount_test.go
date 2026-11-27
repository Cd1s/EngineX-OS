package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

func repairStick(t *testing.T, n int) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, n)
	return mount
}

func buildTestStockOABeatsMount(t *testing.T, sr, samples, beat0Sample, bpm float64, totalBeats int64) []byte {
	t.Helper()
	spb := sr * 60.0 / bpm
	p0 := beat0Sample - 4*spb
	b0 := int64(-4)
	b1 := totalBeats
	p1 := beat0Sample + float64(b1)*spb
	next0 := uint32(b1 - b0)
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, sr)
	_ = binary.Write(&b, binary.BigEndian, samples)
	b.WriteByte(1)
	for g := 0; g < 2; g++ {
		_ = binary.Write(&b, binary.BigEndian, uint64(2))
		_ = binary.Write(&b, binary.LittleEndian, p0)
		_ = binary.Write(&b, binary.LittleEndian, b0)
		_ = binary.Write(&b, binary.LittleEndian, next0)
		_ = binary.Write(&b, binary.LittleEndian, uint32(0))
		_ = binary.Write(&b, binary.LittleEndian, p1)
		_ = binary.Write(&b, binary.LittleEndian, b1)
		_ = binary.Write(&b, binary.LittleEndian, uint32(0))
		_ = binary.Write(&b, binary.LittleEndian, uint32(0xd4a9b300))
	}
	b.Write(make([]byte, 9))
	out, err := enginedb.QCompress(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func buildTestQuickCuesMount(t *testing.T, hot0Pos float64, hot0Name string, mainPos float64, mainAdj byte, defPos float64) []byte {
	t.Helper()
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint64(8))
	for i := 0; i < 8; i++ {
		name := ""
		pos := float64(-1)
		if i == 0 {
			name = hot0Name
			pos = hot0Pos
		}
		b.WriteByte(byte(len(name)))
		b.WriteString(name)
		_ = binary.Write(&b, binary.BigEndian, pos)
		b.Write([]byte{0, 0, 0, 0})
	}
	_ = binary.Write(&b, binary.BigEndian, mainPos)
	b.WriteByte(mainAdj)
	_ = binary.Write(&b, binary.BigEndian, defPos)
	out, err := enginedb.QCompress(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func buildTestEmptyLoopsMount() []byte {
	var b bytes.Buffer
	b.WriteByte(8)
	b.Write(make([]byte, 7))
	for i := 0; i < 8; i++ {
		b.WriteByte(0)
		_ = binary.Write(&b, binary.LittleEndian, float64(-1))
		_ = binary.Write(&b, binary.LittleEndian, float64(-1))
		b.Write(make([]byte, 6))
	}
	return b.Bytes()
}

func sortedTracksByID(snap *rbstate.Snapshot) []*rbstate.Track {
	out := make([]*rbstate.Track, 0, len(snap.Tracks))
	for _, tr := range snap.Tracks {
		if !tr.Native && tr.EngineID > 0 {
			out = append(out, tr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EngineID < out[j].EngineID })
	return out
}

func openTestMDBRW(t *testing.T, mount string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func readRowBlobs(t *testing.T, mount string, id int64) (beats, cues, loops []byte, bpm float64) {
	t.Helper()
	db := openTestMDBRW(t, mount)
	defer db.Close()
	if err := db.QueryRow(`SELECT p.beatData, p.quickCues, p.loops, COALESCE(t.bpmAnalyzed, 0) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.id=?`, id).
		Scan(&beats, &cues, &loops, &bpm); err != nil {
		t.Fatalf("readRowBlobs(%d): %v", id, err)
	}
	return beats, cues, loops, bpm
}

// Fails on 8070b7d7: on Gen-2 64-bit (!OnDemand), an unchanged stick whose snapshot has
// Pending.None() previously bypassed without repairing stock-Engine-overwritten rows.
// Verifies:
//   - stock-OA auto-main-cue row and aborted empty-cue stub are repaired once inline at mount;
//   - DJ 1-sample main-cue move, DJ hot cue, #321 write-back Synced row, and native stock Engine
//     track are all 100% untouched;
//   - a second mount bypasses with Applied == 0 (idempotent, zero re-repair).
func TestGen2MountRepairsStockOverwrittenAndPreservesDJEdits(t *testing.T) {
	mount := repairStick(t, 5)
	// Initial OnDemand mount (same state as a stick coming from an arm32 deck: Pending.None(), ModeOnDemand, PerfNone).
	mountNow(t, mount, MountOptions{OnDemand: true})

	snap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	tracks := sortedTracksByID(snap)
	if len(tracks) < 5 {
		t.Fatalf("expected >=5 fixture tracks, got %d", len(tracks))
	}

	const beat0 = 11391.4248046875
	stockTD := []byte{0, 0, 0, 68, 0x78, 0x9c}
	stockOV := bytes.Repeat([]byte{0x55}, 1200)
	stockBeats := buildTestStockOABeatsMount(t, 44100, 13707967, beat0, 136.0, 704)
	qcAutoBeat0 := buildTestQuickCuesMount(t, -1, "", beat0, 1, beat0)
	qcUnadjEmpty := buildTestQuickCuesMount(t, -1, "", 0, 0, 0)
	qcMoved1 := buildTestQuickCuesMount(t, -1, "", beat0+1, 1, beat0)
	qcDJHot := buildTestQuickCuesMount(t, 220500, "DJKeep", beat0, 1, beat0)
	emptyLoops := buildTestEmptyLoopsMount()

	db := openTestMDBRW(t, mount)
	// Track 0: stock Engine 5.3.0 OA (2-marker beatData + auto-main-cue at beat0) -> MUST REPAIR.
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		stockTD, stockOV, stockBeats, qcAutoBeat0, emptyLoops, tracks[0].EngineID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET isAnalyzed=1, isBeatGridLocked=0, bpmAnalyzed=136.0 WHERE id=?`, tracks[0].EngineID); err != nil {
		t.Fatal(err)
	}
	// Track 1: aborted partial-load empty-cue stub -> MUST REPAIR.
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=NULL, overviewWaveFormData=NULL, beatData=NULL, quickCues=?, loops=NULL, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		qcUnadjEmpty, tracks[1].EngineID); err != nil {
		t.Fatal(err)
	}
	// Track 2: DJ moved main cue by 1 sample (beat0+1 vs beat0) -> MUST STAY UNTOUCHED.
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		stockTD, stockOV, stockBeats, qcMoved1, emptyLoops, tracks[2].EngineID); err != nil {
		t.Fatal(err)
	}
	// Track 3: DJ added Hot Cue 1 ("DJKeep") -> MUST STAY UNTOUCHED.
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		stockTD, stockOV, stockBeats, qcDJHot, emptyLoops, tracks[3].EngineID); err != nil {
		t.Fatal(err)
	}
	// Track 4: stock-OA blobs BUT snapshot has #321 write-back Synced marker -> MUST STAY UNTOUCHED.
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		stockTD, stockOV, stockBeats, qcAutoBeat0, emptyLoops, tracks[4].EngineID); err != nil {
		t.Fatal(err)
	}
	// Also insert a stock Engine native track (pdbImportKey=0) with stock-OA blobs -> MUST STAY UNTOUCHED.
	nativeRes, err := db.Exec(`INSERT INTO Track(path, filename, isAnalyzed, isAvailable, isBeatGridLocked, pdbImportKey) VALUES('../Native/stock.mp3', 'stock.mp3', 1, 1, 0, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	nativeID, err := nativeRes.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0 WHERE trackId=?`,
		stockTD, stockOV, stockBeats, qcAutoBeat0, emptyLoops, nativeID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	snap.Tracks[tracks[4].Key].Synced = []byte{1, 2, 3, 4}
	if err := snap.Save(dbDir(mount)); err != nil {
		t.Fatal(err)
	}

	bgStarted := false
	res := mountNow(t, mount, MountOptions{
		OnDemand:        false,
		Deadline:        time.Now().Add(17 * time.Second),
		StartBackground: func() error { bgStarted = true; return nil },
	})
	if res.Bypassed || res.Applied != 2 || bgStarted || res.Background || !res.Pending.None() {
		t.Fatalf("Gen-2 mount result = %+v (bgStarted=%v), want Bypassed=false Applied=2 Background=false Pending.None", res, bgStarted)
	}

	// Verify tracks[0] and tracks[1] are repaired (EngineX PQTZ beatgrid, not stock OA, RGB overview written, snapshot updated).
	afterSnap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int{0, 1} {
		tr := tracks[idx]
		b, q, l, bpm := readRowBlobs(t, mount, tr.EngineID)
		if len(b) == 0 || bytes.Equal(b, stockBeats) || enginedb.IsStockOverwrittenPerformance(enginedb.StockOverwrittenRow{
			TrackData: []byte{1}, Overview: []byte{1}, Beats: b, QuickCues: q, Loops: l,
		}) {
			t.Fatalf("track[%d] (id=%d) was not repaired: beatsLen=%d bpm=%.2f", idx, tr.EngineID, len(b), bpm)
		}
		if math.Abs(bpm-136.0) < 0.5 && idx == 0 {
			t.Fatalf("track[0] still has stock OA wrong BPM %.2f", bpm)
		}
		st := afterSnap.Tracks[tr.Key]
		if st.Perf != rbstate.PerfApplied || len(st.Written) == 0 || len(st.Built.ANLZ) == 0 {
			t.Fatalf("track[%d] snapshot not marked PerfApplied: %+v", idx, st)
		}
		rgbPath := filepath.Join(dbDir(mount), "OverviewData", afterSnap.LibraryUUID, fmt.Sprintf("%d.rgb", tr.EngineID))
		if _, err := os.Stat(rgbPath); err != nil {
			t.Fatalf("track[%d] RGB overview missing at %s: %v", idx, rgbPath, err)
		}
	}

	// Verify tracks[2] (1-sample main cue), tracks[3] (DJ hot cue), tracks[4] (Synced), and native track are byte-for-byte untouched.
	for _, tc := range []struct {
		name string
		id   int64
		want []byte
	}{
		{"dj_moved_1_sample", tracks[2].EngineID, qcMoved1},
		{"dj_hot_cue", tracks[3].EngineID, qcDJHot},
		{"writeback_synced", tracks[4].EngineID, qcAutoBeat0},
		{"native_engine_track", nativeID, qcAutoBeat0},
	} {
		b, q, l, _ := readRowBlobs(t, mount, tc.id)
		if !bytes.Equal(b, stockBeats) || !bytes.Equal(q, tc.want) || !bytes.Equal(l, emptyLoops) {
			t.Fatalf("%s (id=%d) was modified! beatsEq=%v cuesEq=%v loopsEq=%v", tc.name, tc.id, bytes.Equal(b, stockBeats), bytes.Equal(q, tc.want), bytes.Equal(l, emptyLoops))
		}
	}

	// Second Gen-2 mount must bypass immediately with 0 repairs (repaired ONCE).
	res2 := mountNow(t, mount, MountOptions{
		OnDemand:        false,
		Deadline:        time.Now().Add(17 * time.Second),
		StartBackground: func() error { bgStarted = true; return nil },
	})
	if !res2.Bypassed || res2.Applied != 0 || bgStarted {
		t.Fatalf("second Gen-2 mount = %+v (bgStarted=%v), want Bypassed=true Applied=0", res2, bgStarted)
	}
}

// Fails on 8070b7d7: verifies Gen-2 mount-time repair serializes with #321 write-back via
// rbstate.LockVolume (returns ErrLocked while held, repairs cleanly once released).
func TestGen2MountRepairRespectsVolumeLock(t *testing.T) {
	mount := repairStick(t, 3)
	mountNow(t, mount, MountOptions{OnDemand: true})
	snap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	tr := sortedTracksByID(snap)[0]
	const beat0 = 11391.4248046875
	stockBeats := buildTestStockOABeatsMount(t, 44100, 13707967, beat0, 136.0, 704)
	qcAutoBeat0 := buildTestQuickCuesMount(t, -1, "", beat0, 1, beat0)
	emptyLoops := buildTestEmptyLoopsMount()

	db := openTestMDBRW(t, mount)
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		[]byte{1}, []byte{1}, stockBeats, qcAutoBeat0, emptyLoops, tr.EngineID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	oldWait := lockWait
	lockWait = 50 * time.Millisecond
	defer func() { lockWait = oldWait }()

	unlock, err := rbstate.LockVolume(mount, 0)
	if err != nil {
		t.Fatalf("LockVolume: %v", err)
	}
	_, err = ConvertAtMount(context.Background(), mount, MountOptions{OnDemand: false})
	if !errors.Is(err, rbstate.ErrLocked) {
		unlock()
		t.Fatalf("ConvertAtMount while locked = %v, want rbstate.ErrLocked", err)
	}
	// Row must still be untouched while locked.
	bBefore, _, _, _ := readRowBlobs(t, mount, tr.EngineID)
	if !bytes.Equal(bBefore, stockBeats) {
		unlock()
		t.Fatal("row changed while volume lock was held")
	}
	unlock()

	res := mountNow(t, mount, MountOptions{OnDemand: false})
	if res.Applied != 1 {
		t.Fatalf("after unlock ConvertAtMount = %+v, want Applied=1", res)
	}
	bAfter, _, _, _ := readRowBlobs(t, mount, tr.EngineID)
	if bytes.Equal(bAfter, stockBeats) {
		t.Fatal("row was not repaired after volume lock released")
	}
}

// Fails on 8070b7d7: when the 4 s insert budget expires mid-repair on Gen-2 64-bit, rows that
// fit are repaired inline and remaining matching rows are handed off to FillInBackground +
// publish remount, while DJ-edited rows remain preserved through ApplyPerformance.
func TestGen2MountRepairBudgetHandoffToFillInBackground(t *testing.T) {
	mount := repairStick(t, 4)
	mountNow(t, mount, MountOptions{OnDemand: true})
	snap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	tracks := sortedTracksByID(snap)

	const beat0 = 11391.4248046875
	stockBeats := buildTestStockOABeatsMount(t, 44100, 13707967, beat0, 136.0, 704)
	qcAutoBeat0 := buildTestQuickCuesMount(t, -1, "", beat0, 1, beat0)
	qcDJHot := buildTestQuickCuesMount(t, 220500, "DJKeep", beat0, 1, beat0)
	emptyLoops := buildTestEmptyLoopsMount()

	db := openTestMDBRW(t, mount)
	// tracks[0] and tracks[1]: stock-OA overwritten; tracks[2]: DJ hot cue.
	for _, idx := range []int{0, 1} {
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
			[]byte{1}, []byte{1}, stockBeats, qcAutoBeat0, emptyLoops, tracks[idx].EngineID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		[]byte{1}, []byte{1}, stockBeats, qcDJHot, emptyLoops, tracks[2].EngineID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Stop inline repair right after tracks[0] to simulate 4 s insertBudget expiry.
	seen := 0
	oldHook := repairBeforeBuild
	repairBeforeBuild = func(_ context.Context, _ string) error {
		seen++
		if seen >= 2 {
			return errWindowDeadline
		}
		return nil
	}
	defer func() { repairBeforeBuild = oldHook }()

	bgStarted := false
	res := mountNow(t, mount, MountOptions{
		OnDemand:        false,
		Deadline:        time.Now().Add(17 * time.Second),
		StartBackground: func() error { bgStarted = true; return nil },
	})
	if res.Applied != 1 || !bgStarted || !res.Background || res.Pending.Perf < 1 {
		t.Fatalf("budget-capped Gen-2 mount = %+v (bgStarted=%v), want Applied=1 Background=true Pending.Perf>=1", res, bgStarted)
	}
	// Track 0 repaired inline, Track 1 still stock-OA awaiting FillInBackground.
	b0, _, _, _ := readRowBlobs(t, mount, tracks[0].EngineID)
	b1, _, _, _ := readRowBlobs(t, mount, tracks[1].EngineID)
	if bytes.Equal(b0, stockBeats) || !bytes.Equal(b1, stockBeats) {
		t.Fatalf("unexpected inline split: t0Stock=%v t1Stock=%v", bytes.Equal(b0, stockBeats), bytes.Equal(b1, stockBeats))
	}

	// Clear budget hook and run FillInBackground -> Publish remount (Gen-2 background handoff).
	repairBeforeBuild = nil
	fillRes, err := FillInBackground(context.Background(), mount, FillOptions{OnDemand: false})
	if err != nil || !fillRes.Publish {
		t.Fatalf("FillInBackground = (%+v, %v), want Publish=true", fillRes, err)
	}
	pubRes := mountNow(t, mount, MountOptions{OnDemand: false, Publish: true})
	if !pubRes.Pending.None() {
		t.Fatalf("publish remount left pending work: %+v", pubRes)
	}

	// Track 1 must now be cleanly repaired (PerfApplied + Written != nil, NOT PerfDeck!)
	// while Track 2 keeps DJ's "DJKeep" hot cue (PerfDeck).
	finalSnap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	b1Final, q1Final, _, _ := readRowBlobs(t, mount, tracks[1].EngineID)
	if bytes.Equal(b1Final, stockBeats) || bytes.Equal(q1Final, qcAutoBeat0) {
		t.Fatalf("track[1] not repaired by FillInBackground handoff")
	}
	if st1 := finalSnap.Tracks[tracks[1].Key]; st1.Perf != rbstate.PerfApplied || len(st1.Written) == 0 {
		t.Fatalf("track[1] snapshot after FillInBackground handoff = %+v, want PerfApplied + Written", st1)
	}
	_, q2Final, _, _ := readRowBlobs(t, mount, tracks[2].EngineID)
	rawQ2, err := enginedb.QUncompress(q2Final, 64<<10)
	if err != nil || !bytes.Contains(rawQ2, []byte("DJKeep")) {
		t.Fatalf("track[2] DJ hot cue was lost during FillInBackground handoff (err=%v)", err)
	}
	if st2 := finalSnap.Tracks[tracks[2].Key]; st2.Perf != rbstate.PerfDeck {
		t.Fatalf("track[2] snapshot perf = %d, want PerfDeck", st2.Perf)
	}
}

// Fails at runtime on 8070b7d7: when insertBudget is already exhausted on a Gen-2 64-bit
// mount, matching rows are handed off to FillInBackground + Publish remount instead of bypassed.
func TestGen2MountInsertBudgetExhaustedHandsOffToFillInBackground(t *testing.T) {
	mount := repairStick(t, 3)
	mountNow(t, mount, MountOptions{OnDemand: true})
	snap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	tr := sortedTracksByID(snap)[0]
	const beat0 = 11391.4248046875
	stockBeats := buildTestStockOABeatsMount(t, 44100, 13707967, beat0, 136.0, 704)
	qcAutoBeat0 := buildTestQuickCuesMount(t, -1, "", beat0, 1, beat0)
	emptyLoops := buildTestEmptyLoopsMount()

	db := openTestMDBRW(t, mount)
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		[]byte{1}, []byte{1}, stockBeats, qcAutoBeat0, emptyLoops, tr.EngineID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	oldBudget := insertBudget
	insertBudget = time.Nanosecond
	defer func() { insertBudget = oldBudget }()

	bgStarted := false
	res := mountNow(t, mount, MountOptions{
		OnDemand:        false,
		Deadline:        time.Now().Add(17 * time.Second),
		StartBackground: func() error { bgStarted = true; return nil },
	})
	if res.Bypassed || res.Applied != 0 || !bgStarted || !res.Background || res.Pending.Perf < 1 {
		t.Fatalf("zero-budget Gen-2 mount = %+v (bgStarted=%v), want Bypassed=false Applied=0 Background=true Pending.Perf>=1", res, bgStarted)
	}

	insertBudget = oldBudget
	fillRes, err := FillInBackground(context.Background(), mount, FillOptions{OnDemand: false})
	if err != nil || !fillRes.Publish {
		t.Fatalf("FillInBackground = (%+v, %v), want Publish=true", fillRes, err)
	}
	pubRes := mountNow(t, mount, MountOptions{OnDemand: false, Publish: true})
	if !pubRes.Pending.None() {
		t.Fatalf("publish remount left pending: %+v", pubRes)
	}
	bFinal, qFinal, _, _ := readRowBlobs(t, mount, tr.EngineID)
	if bytes.Equal(bFinal, stockBeats) || bytes.Equal(qFinal, qcAutoBeat0) {
		t.Fatal("track was not repaired by FillInBackground handoff")
	}
	finalSnap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	if st := finalSnap.Tracks[tr.Key]; st.Perf != rbstate.PerfApplied || len(st.Written) == 0 {
		t.Fatalf("track snapshot after FillInBackground handoff = %+v, want PerfApplied + Written", st)
	}
}

// Verifies arm32 OnDemand mounts keep zero mount-time scan/repair cost (left to on-load hook).
func TestOnDemandMountNeverScansOrRepairsAtMount(t *testing.T) {
	mount := repairStick(t, 3)
	mountNow(t, mount, MountOptions{OnDemand: true})
	snap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	tr := sortedTracksByID(snap)[0]
	const beat0 = 11391.4248046875
	stockBeats := buildTestStockOABeatsMount(t, 44100, 13707967, beat0, 136.0, 704)
	qcAutoBeat0 := buildTestQuickCuesMount(t, -1, "", beat0, 1, beat0)
	emptyLoops := buildTestEmptyLoopsMount()

	db := openTestMDBRW(t, mount)
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		[]byte{1}, []byte{1}, stockBeats, qcAutoBeat0, emptyLoops, tr.EngineID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	res := mountNow(t, mount, MountOptions{OnDemand: true})
	if !res.Bypassed || res.Applied != 0 {
		t.Fatalf("OnDemand mount = %+v, want Bypassed=true Applied=0", res)
	}
	bAfter, _, _, _ := readRowBlobs(t, mount, tr.EngineID)
	if !bytes.Equal(bAfter, stockBeats) {
		t.Fatal("OnDemand mount unexpectedly modified m.db at mount time")
	}
}
