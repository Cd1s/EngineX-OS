package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
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
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func appendFilledJSONL(t *testing.T, stageDir string, entry rbstate.Filled) {
	t.Helper()
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(stageDir, "filled.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

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

// Fails at runtime on 8070b7d7: replicates the exact 2,440-row real hardware stick profile
// (2 idle-settled stock-overwritten rows: 1 aborted empty-cue stub + 1 stock-OA 136 BPM row;
// 5 EngineX-filled rows including filled.jsonl + 2-beat EngineX row; 2,433 untouched rows)
// across BOTH OnDemand (arm32) and full (arm64 Gen-2) mounts:
//   - 1st insert repairs exact 2/2 damaged rows and touches 0 of the other 2,438 rows;
//   - 2nd insert writes nothing (Bypassed=true, Applied=0, file mtime/size/sha256 unchanged).
func TestRealStickReplica2440RepairsExact2Of2AndSecondInsertWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		onDemand bool
	}{
		{"arm32_ondemand", true},
		{"arm64_gen2_full", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount := repairStick(t, 2440)
			mountNow(t, mount, MountOptions{OnDemand: true})

			snap, err := rbstate.Load(dbDir(mount))
			if err != nil {
				t.Fatal(err)
			}
			tracks := sortedTracksByID(snap)
			if len(tracks) != 2440 {
				t.Fatalf("expected 2440 tracks, got %d", len(tracks))
			}

			const beat0 = 11391.4248046875
			stockTD := []byte{0, 0, 0, 68, 0x78, 0x9c}
			stockOV := bytes.Repeat([]byte{0x55}, 1200)
			stockBeats := buildTestStockOABeatsMount(t, 44100, 13707967, beat0, 136.0, 704)
			qcAutoBeat0 := buildTestQuickCuesMount(t, -1, "", beat0, 1, beat0)
			qcUnadjEmpty := buildTestQuickCuesMount(t, -1, "", 0, 0, 0)
			emptyLoops := buildTestEmptyLoopsMount()

			// 5 EngineX-filled rows (4 normal + 1 tiny 2-beat EngineX row with len(beatData)<=256)
			// plus filled.jsonl records, matching the real hardware stick's 5 filled tracks.
			stage, err := rbstate.OpenStage(snap.LibraryUUID)
			if err != nil {
				t.Fatal(err)
			}
			db := openTestMDBRW(t, mount)
			for i := 0; i < 5; i++ {
				nBeats := 24
				if i == 4 {
					nBeats = 2
				}
				beats := make([]rekordbox.Beat, nBeats)
				for b := range beats {
					beats[b] = rekordbox.Beat{Number: uint16(b%4) + 1, BPM: 124, TimeMS: uint32(250 + b*484)}
				}
				perf, perr := enginedb.EncodePerformance(
					rekordbox.Track{SampleRate: 44100, Duration: 20, BPM: 124},
					&rekordbox.Analysis{Beats: beats},
				)
				if perr != nil {
					t.Fatal(perr)
				}
				tr := tracks[i]
				if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
					perf.TrackData, perf.Overview, perf.Beats, perf.QuickCues, perf.Loops, tr.EngineID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE Track SET isAnalyzed=1 WHERE id=?`, tr.EngineID); err != nil {
					t.Fatal(err)
				}
				appendFilledJSONL(t, stage.Dir(), rbstate.Filled{
					Key:      tr.Key,
					EngineID: tr.EngineID,
					Written:  enginedb.WrittenDigest(perf.Beats, perf.QuickCues, perf.Loops),
				})
			}
			stage.Close()

			// Row #1 (aborted empty-cue stub) and Row #2 (stock OA 136 BPM) — both idle-settled by #321!
			row1 := tracks[1957]
			row2 := tracks[1968]
			if _, err := db.Exec(`UPDATE PerformanceData SET trackData=NULL, overviewWaveFormData=NULL, beatData=NULL, quickCues=?, loops=NULL, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
				qcUnadjEmpty, row1.EngineID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
				stockTD, stockOV, stockBeats, qcAutoBeat0, emptyLoops, row2.EngineID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE Track SET isAnalyzed=1, isBeatGridLocked=0, bpmAnalyzed=136.0 WHERE id=?`, row2.EngineID); err != nil {
				t.Fatal(err)
			}

			// Read exact genre/comment/label so idle-settled Synced matches #321's exact SyncedDigest(cur).
			readTags := func(id int64) (g, c, l string) {
				if err := db.QueryRow(`SELECT COALESCE(genre,''), COALESCE(comment,''), COALESCE(label,'') FROM Track WHERE id=?`, id).Scan(&g, &c, &l); err != nil {
					t.Fatal(err)
				}
				return
			}
			g1, c1, l1 := readTags(row1.EngineID)
			g2, c2, l2 := readTags(row2.EngineID)
			snap.Tracks[row1.Key].Edit = 1795689149
			snap.Tracks[row1.Key].Synced = enginedb.SyncedDigest(nil, qcUnadjEmpty, nil, 0, g1, c1, l1)
			snap.Tracks[row2.Key].Edit = 1795690063
			snap.Tracks[row2.Key].Synced = enginedb.SyncedDigest(nil, qcAutoBeat0, emptyLoops, 0, g2, c2, l2)
			if err := snap.Save(dbDir(mount)); err != nil {
				t.Fatal(err)
			}
			if err := syncBrowseIndex(dbDir(mount), snap); err != nil {
				t.Fatal(err)
			}

			// Hash every one of the 2,440 rows before insert so we prove 0 of the other 2,438 rows change.
			hashAllRows := func() map[int64][32]byte {
				rows, err := db.Query(`SELECT t.id, COALESCE(t.bpm,0), COALESCE(t.bpmAnalyzed,0), COALESCE(t.isAnalyzed,0), p.trackData, p.overviewWaveFormData, p.beatData, p.quickCues, p.loops, COALESCE(p.activeOnLoadLoops,0) FROM Track t JOIN PerformanceData p ON p.trackId=t.id`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				out := make(map[int64][32]byte, 2440)
				for rows.Next() {
					var id, bpm, an, act int64
					var bpmA float64
					var td, ov, bd, qc, lp []byte
					if err := rows.Scan(&id, &bpm, &bpmA, &an, &td, &ov, &bd, &qc, &lp, &act); err != nil {
						t.Fatal(err)
					}
					h := sha256.New()
					fmt.Fprintf(h, "%d:%d:%.6f:%d:%d|", id, bpm, bpmA, an, act)
					for _, b := range [][]byte{td, ov, bd, qc, lp} {
						fmt.Fprintf(h, "%d:", len(b))
						h.Write(b)
					}
					var sum [32]byte
					copy(sum[:], h.Sum(nil))
					out[id] = sum
				}
				return out
			}
			beforeHashes := hashAllRows()
			db.Close()

			// 1st insert: must repair exact 2/2 rows inline, touching 0 of the other 2,438 rows.
			bgStarted := false
			res1 := mountNow(t, mount, MountOptions{
				OnDemand:        tc.onDemand,
				Deadline:        time.Now().Add(17 * time.Second),
				StartBackground: func() error { bgStarted = true; return nil },
			})
			if res1.Bypassed || res1.Applied != 2 || bgStarted || res1.Background || !res1.Pending.None() {
				t.Fatalf("1st insert (%s) = %+v (bgStarted=%v), want Bypassed=false Applied=2 Background=false Pending.None", tc.name, res1, bgStarted)
			}

			db = openTestMDBRW(t, mount)
			afterHashes := hashAllRows()
			db.Close()
			changed := 0
			for id, bh := range beforeHashes {
				if afterHashes[id] != bh {
					if id != row1.EngineID && id != row2.EngineID {
						t.Fatalf("unexpected row id=%d modified among the 2,438 protected rows", id)
					}
					changed++
				}
			}
			if changed != 2 {
				t.Fatalf("changed rows = %d, want exact 2 (row1=%d, row2=%d)", changed, row1.EngineID, row2.EngineID)
			}

			// Verify both repaired rows are PerfApplied with valid rekordbox PQTZ grids and restored BPM.
			afterSnap, err := rbstate.Load(dbDir(mount))
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range []*rbstate.Track{row1, row2} {
				b, q, l, bpm := readRowBlobs(t, mount, target.EngineID)
				if len(b) == 0 || bytes.Equal(b, stockBeats) || enginedb.IsStockOverwrittenPerformance(enginedb.StockOverwrittenRow{
					TrackData: []byte{1}, Overview: []byte{1}, Beats: b, QuickCues: q, Loops: l,
				}) {
					t.Fatalf("target id=%d still broken after repair (beatsLen=%d bpm=%.2f)", target.EngineID, len(b), bpm)
				}
				if target == row2 && math.Abs(bpm-136.0) < 0.5 {
					t.Fatalf("row2 kept stock OA wrong BPM %.2f", bpm)
				}
				if st := afterSnap.Tracks[target.Key]; st.Perf != rbstate.PerfApplied || len(st.Written) == 0 || len(st.Built.ANLZ) == 0 {
					t.Fatalf("target id=%d snapshot not PerfApplied: %+v", target.EngineID, st)
				}
			}

			// Record exact file signatures after 1st insert; 2nd insert must write NOTHING.
			type fileStamp struct {
				modNS int64
				size  int64
				sum   [32]byte
			}
			statFiles := func() map[string]fileStamp {
				out := map[string]fileStamp{}
				for _, name := range []string{"m.db", rbstate.SnapshotName, rbstate.BrowseIndexName} {
					p := filepath.Join(dbDir(mount), name)
					st, err := os.Stat(p)
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					raw, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					out[name] = fileStamp{modNS: st.ModTime().UnixNano(), size: st.Size(), sum: sha256.Sum256(raw)}
				}
				return out
			}
			filesBefore2 := statFiles()
			time.Sleep(20 * time.Millisecond)

			res2 := mountNow(t, mount, MountOptions{
				OnDemand:        tc.onDemand,
				Deadline:        time.Now().Add(17 * time.Second),
				StartBackground: func() error { bgStarted = true; return nil },
			})
			if !res2.Bypassed || res2.Applied != 0 || bgStarted || res2.Background {
				t.Fatalf("2nd insert (%s) = %+v (bgStarted=%v), want Bypassed=true Applied=0", tc.name, res2, bgStarted)
			}
			filesAfter2 := statFiles()
			if len(filesAfter2) != len(filesBefore2) {
				t.Fatalf("2nd insert changed file set: before=%v after=%v", filesBefore2, filesAfter2)
			}
			for name, prev := range filesBefore2 {
				if cur := filesAfter2[name]; cur != prev {
					t.Fatalf("2nd insert wrote %s: before=%+v after=%+v", name, prev, cur)
				}
			}
		})
	}
}

// Verifies arm32 OnDemand mounts repair matching rows once at insert and, on budget overflow,
// reset remaining damaged rows to clean untouched import stubs (0 background perf pass).
func TestOnDemandMountRepairsOnceAndBudgetOverflowResetsToStub(t *testing.T) {
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
	for _, idx := range []int{0, 1} {
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
			[]byte{1}, []byte{1}, stockBeats, qcAutoBeat0, emptyLoops, tracks[idx].EngineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET isAnalyzed=1, bpmAnalyzed=136.0 WHERE id=?`, tracks[idx].EngineID); err != nil {
			t.Fatal(err)
		}
	}
	// Track 2: DJ hot cue -> MUST stay untouched.
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		[]byte{1}, []byte{1}, stockBeats, qcDJHot, emptyLoops, tracks[2].EngineID); err != nil {
		t.Fatal(err)
	}
	// Track 3: stock-OA blobs BUT recorded in filled.jsonl -> MUST stay untouched.
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		[]byte{1}, []byte{1}, stockBeats, qcAutoBeat0, emptyLoops, tracks[3].EngineID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	stage, err := rbstate.OpenStage(snap.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	appendFilledJSONL(t, stage.Dir(), rbstate.Filled{
		Key:      tracks[3].Key,
		EngineID: tracks[3].EngineID,
		Written:  enginedb.WrittenDigest(stockBeats, qcAutoBeat0, emptyLoops),
	})
	stage.Close()

	// Stop inline build after Track 0 so Track 1 exercises OnDemand budget overflow -> stub reset.
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
		OnDemand:        true,
		Deadline:        time.Now().Add(17 * time.Second),
		StartBackground: func() error { bgStarted = true; return nil },
	})
	if res.Bypassed || res.Applied != 2 || bgStarted || res.Background || !res.Pending.None() {
		t.Fatalf("OnDemand insert repair = %+v (bgStarted=%v), want Bypassed=false Applied=2 Background=false Pending.None", res, bgStarted)
	}
	// Track 0 repaired inline to full rekordbox data; Track 1 reset to untouched stub (NULL beats/cues/loops, isAnalyzed=0).
	b0, _, _, _ := readRowBlobs(t, mount, tracks[0].EngineID)
	if len(b0) == 0 || bytes.Equal(b0, stockBeats) {
		t.Fatal("track[0] was not repaired inline on OnDemand mount")
	}
	b1, q1, l1, _ := readRowBlobs(t, mount, tracks[1].EngineID)
	if len(b1) != 0 || len(q1) != 0 || len(l1) != 0 {
		t.Fatalf("track[1] overflow was not reset to clean stub: beats=%d cues=%d loops=%d", len(b1), len(q1), len(l1))
	}
	// Track 2 (DJ hot cue) and Track 3 (filled.jsonl) must stay byte-for-byte untouched!
	b2, q2, _, _ := readRowBlobs(t, mount, tracks[2].EngineID)
	b3, q3, _, _ := readRowBlobs(t, mount, tracks[3].EngineID)
	if !bytes.Equal(b2, stockBeats) || !bytes.Equal(q2, qcDJHot) {
		t.Fatal("track[2] DJ hot cue was modified")
	}
	if !bytes.Equal(b3, stockBeats) || !bytes.Equal(q3, qcAutoBeat0) {
		t.Fatal("track[3] filled.jsonl row was modified")
	}
	// 2nd OnDemand mount bypasses cleanly with 0 work.
	repairBeforeBuild = nil
	res2 := mountNow(t, mount, MountOptions{OnDemand: true, Deadline: time.Now().Add(17 * time.Second)})
	if !res2.Bypassed || res2.Applied != 0 {
		t.Fatalf("2nd OnDemand mount = %+v, want Bypassed=true Applied=0", res2)
	}
}

// Verifies at insert level all three PM-approved provenance states (Edit==0+nil Synced,
// Edit==0+mount baseline Synced, idle-settled current Synced) are repaired while a
// mismatched non-baseline Synced digest (even at Edit==0) is strictly refused.
func TestMountRepairProvenanceThreeAcceptedStatesAndMismatchedSyncedRefused(t *testing.T) {
	mount := repairStick(t, 5)
	mountNow(t, mount, MountOptions{OnDemand: true})
	snap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	tracks := sortedTracksByID(snap)

	const beat0 = 11391.4248046875
	stockBeats := buildTestStockOABeatsMount(t, 44100, 13707967, beat0, 136.0, 704)
	qcAutoBeat0 := buildTestQuickCuesMount(t, -1, "", beat0, 1, beat0)
	emptyLoops := buildTestEmptyLoopsMount()

	db := openTestMDBRW(t, mount)
	readTags := func(id int64) (g, c, l string) {
		if err := db.QueryRow(`SELECT COALESCE(genre,''), COALESCE(comment,''), COALESCE(label,'') FROM Track WHERE id=?`, id).Scan(&g, &c, &l); err != nil {
			t.Fatal(err)
		}
		return
	}
	for i := 0; i < 4; i++ {
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
			[]byte{1}, []byte{1}, stockBeats, qcAutoBeat0, emptyLoops, tracks[i].EngineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET isAnalyzed=1, bpmAnalyzed=136.0 WHERE id=?`, tracks[i].EngineID); err != nil {
			t.Fatal(err)
		}
	}
	// State 1: older pre-#321 / non-OnDemand snapshot row (Edit==0, Synced==nil) -> REPAIR.
	snap.Tracks[tracks[0].Key].Edit = 0
	snap.Tracks[tracks[0].Key].Synced = nil
	// State 2: untouched OnDemand mount baseline (Edit==0, Synced==mountBaseline) -> REPAIR.
	g1, c1, l1 := readTags(tracks[1].EngineID)
	snap.Tracks[tracks[1].Key].Edit = 0
	snap.Tracks[tracks[1].Key].Synced = enginedb.SyncedDigest(nil, tracks[1].FastCues, tracks[1].FastLoops, int64(tracks[1].FastActive), g1, c1, l1)
	// State 3: idle-settled row (maintainer restore rule: Synced==SyncedDigest(cur)) -> REPAIR.
	g2, c2, l2 := readTags(tracks[2].EngineID)
	snap.Tracks[tracks[2].Key].Edit = 1795690063
	snap.Tracks[tracks[2].Key].Synced = enginedb.SyncedDigest(nil, qcAutoBeat0, emptyLoops, 0, g2, c2, l2)
	// Rejected: mismatched non-baseline Synced at Edit==0 -> REFUSED.
	snap.Tracks[tracks[3].Key].Edit = 0
	snap.Tracks[tracks[3].Key].Synced = []byte{9, 8, 7, 6}
	db.Close()
	if err := snap.Save(dbDir(mount)); err != nil {
		t.Fatal(err)
	}
	if err := syncBrowseIndex(dbDir(mount), snap); err != nil {
		t.Fatal(err)
	}

	res := mountNow(t, mount, MountOptions{OnDemand: true, Deadline: time.Now().Add(17 * time.Second)})
	if res.Bypassed || res.Applied != 3 || res.Background || !res.Pending.None() {
		t.Fatalf("mount repair = %+v, want Bypassed=false Applied=3 Background=false Pending.None", res)
	}
	for i := 0; i < 3; i++ {
		b, q, _, _ := readRowBlobs(t, mount, tracks[i].EngineID)
		if len(b) == 0 || bytes.Equal(b, stockBeats) || bytes.Equal(q, qcAutoBeat0) {
			t.Fatalf("accepted provenance track[%d] was not repaired", i)
		}
	}
	b3, q3, _, _ := readRowBlobs(t, mount, tracks[3].EngineID)
	if !bytes.Equal(b3, stockBeats) || !bytes.Equal(q3, qcAutoBeat0) {
		t.Fatal("mismatched non-baseline Synced track[3] was unexpectedly modified")
	}
	res2 := mountNow(t, mount, MountOptions{OnDemand: true, Deadline: time.Now().Add(17 * time.Second)})
	if !res2.Bypassed || res2.Applied != 0 {
		t.Fatalf("2nd mount = %+v, want Bypassed=true Applied=0", res2)
	}
}

// Fails at runtime on cb636815: verifies the soft insertBudget reserves a bounded fallback
// window for OnDemand stub-reset + commit (finishing strictly inside insertBudget), and when
// an uninterruptible slow build consumes the soft window, no unbounded reset/commit runs past
// insertBudget — instead outstanding rows are recorded durably for background or next insert.
func TestOnDemandMountSoftDeadlineBoundedFallbackAndCutoff(t *testing.T) {
	setupStick := func(t *testing.T) (string, []*rbstate.Track, []byte, []byte, []byte) {
		t.Helper()
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
		return mount, tracks, stockBeats, qcDJHot, qcAutoBeat0
	}

	t.Run("reserved_fallback_finishes_within_insert_budget", func(t *testing.T) {
		mount, tracks, stockBeats, qcDJHot, _ := setupStick(t)
		oldBudget := insertBudget
		insertBudget = 1200 * time.Millisecond
		defer func() { insertBudget = oldBudget }()

		seen := 0
		oldHook := repairBeforeBuild
		repairBeforeBuild = func(ctx context.Context, _ string) error {
			seen++
			if seen == 2 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
					return nil
				}
			}
			return nil
		}
		defer func() { repairBeforeBuild = oldHook }()

		t0 := time.Now()
		bgStarted := false
		res := mountNow(t, mount, MountOptions{
			OnDemand:        true,
			Deadline:        time.Now().Add(17 * time.Second),
			StartBackground: func() error { bgStarted = true; return nil },
		})
		elapsed := time.Since(t0)
		if elapsed >= insertBudget {
			t.Fatalf("soft-deadline fallback overran insertBudget: elapsed=%v budget=%v res=%+v", elapsed, insertBudget, res)
		}
		if res.Bypassed || res.Applied != 2 || bgStarted || res.Background || !res.Pending.None() {
			t.Fatalf("soft-deadline bounded fallback = %+v (bgStarted=%v elapsed=%v), want Applied=2 Background=false Pending.None", res, bgStarted, elapsed)
		}
		b0, _, _, _ := readRowBlobs(t, mount, tracks[0].EngineID)
		if len(b0) == 0 || bytes.Equal(b0, stockBeats) {
			t.Fatal("track[0] was not repaired inline")
		}
		b1, q1, l1, _ := readRowBlobs(t, mount, tracks[1].EngineID)
		if len(b1) != 0 || len(q1) != 0 || len(l1) != 0 {
			t.Fatalf("track[1] was not reset to untouched stub inside reserved window: beats=%d cues=%d loops=%d", len(b1), len(q1), len(l1))
		}
		b2, q2, _, _ := readRowBlobs(t, mount, tracks[2].EngineID)
		if !bytes.Equal(b2, stockBeats) || !bytes.Equal(q2, qcDJHot) {
			t.Fatal("track[2] DJ hot cue was modified")
		}
	})

	t.Run("uninterruptible_slow_build_past_soft_cutoff_defers_durably", func(t *testing.T) {
		mount, tracks, stockBeats, qcDJHot, _ := setupStick(t)
		oldBudget := insertBudget
		insertBudget = 1200 * time.Millisecond
		defer func() { insertBudget = oldBudget }()

		seen := 0
		oldHook := repairBeforeBuild
		repairBeforeBuild = func(_ context.Context, _ string) error {
			seen++
			if seen == 2 {
				// Simulate an uninterruptible slow read consuming the remaining soft window.
				time.Sleep(1100 * time.Millisecond)
			}
			return nil
		}
		defer func() { repairBeforeBuild = oldHook }()

		bgStarted := false
		res := mountNow(t, mount, MountOptions{
			OnDemand:        true,
			Deadline:        time.Now().Add(17 * time.Second),
			StartBackground: func() error { bgStarted = true; return nil },
		})
		if res.Bypassed || res.Applied != 0 || !bgStarted || !res.Background || res.Pending.Perf < 2 {
			t.Fatalf("post-soft-cutoff mount = %+v (bgStarted=%v), want Applied=0 Background=true Pending.Perf>=2", res, bgStarted)
		}
		onDisk, err := rbstate.Load(dbDir(mount))
		if err != nil {
			t.Fatal(err)
		}
		if onDisk.Pending.Perf < 2 {
			t.Fatalf("on-disk Pending.Perf = %d, want >= 2 durably recorded", onDisk.Pending.Perf)
		}

		// Recovery on the next insert with normal budget repairs both rows and preserves DJ edit.
		insertBudget = oldBudget
		repairBeforeBuild = nil
		res2 := mountNow(t, mount, MountOptions{OnDemand: true, Deadline: time.Now().Add(17 * time.Second)})
		if res2.Bypassed || res2.Applied != 2 || res2.Background || !res2.Pending.None() {
			t.Fatalf("recovery next insert = %+v, want Applied=2 Background=false Pending.None", res2)
		}
		for _, idx := range []int{0, 1} {
			b, _, _, _ := readRowBlobs(t, mount, tracks[idx].EngineID)
			if len(b) == 0 || bytes.Equal(b, stockBeats) {
				t.Fatalf("track[%d] not repaired on next insert", idx)
			}
		}
		b2, q2, _, _ := readRowBlobs(t, mount, tracks[2].EngineID)
		if !bytes.Equal(b2, stockBeats) || !bytes.Equal(q2, qcDJHot) {
			t.Fatal("track[2] DJ hot cue was modified during recovery")
		}
	})
}

// Fails at runtime on cb636815: when the hard mount context (Deadline - spare) expires
// during OnDemand candidate work, outstanding rows must be recorded durably in the snapshot
// so either FillInBackground(OnDemand: true) + Publish remount OR the next insert repairs them.
func TestOnDemandMountHardDeadlineRecordsPendingAndRecovers(t *testing.T) {
	setupExpiredMount := func(t *testing.T) (string, []*rbstate.Track, []byte, []byte) {
		t.Helper()
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

		// Choose Deadline so st.owed = now + 850 ms (enters candidate work) and mount ctx
		// expires at Deadline - spare = now + 1600 ms while 3 s spare remains before kill.
		kill := time.Now().Add(commitReserve + 850*time.Millisecond)
		mountCtxLimit := kill.Add(-spare)
		oldHook := repairBeforeBuild
		repairBeforeBuild = func(_ context.Context, _ string) error {
			if wait := time.Until(mountCtxLimit) + 60*time.Millisecond; wait > 0 {
				time.Sleep(wait)
			}
			return nil
		}
		defer func() { repairBeforeBuild = oldHook }()

		bgStarted := false
		res := mountNow(t, mount, MountOptions{
			OnDemand:        true,
			Deadline:        kill,
			StartBackground: func() error { bgStarted = true; return nil },
		})
		if res.Bypassed || res.Applied != 0 || !bgStarted || !res.Background || res.Pending.Perf != 2 {
			t.Fatalf("hard-deadline OnDemand mount = %+v (bgStarted=%v), want Applied=0 Background=true Pending.Perf=2", res, bgStarted)
		}
		onDisk, err := rbstate.Load(dbDir(mount))
		if err != nil {
			t.Fatal(err)
		}
		if onDisk.Pending.Perf != 2 {
			t.Fatalf("on-disk snapshot Pending.Perf = %d, want 2 durably recorded", onDisk.Pending.Perf)
		}
		return mount, tracks, stockBeats, qcDJHot
	}

	t.Run("recover_via_background_build", func(t *testing.T) {
		mount, tracks, stockBeats, qcDJHot := setupExpiredMount(t)
		fres, err := FillInBackground(context.Background(), mount, FillOptions{OnDemand: true})
		if err != nil || !fres.Publish {
			t.Fatalf("FillInBackground(OnDemand: true) = (%+v, %v), want Publish=true", fres, err)
		}
		pubRes := mountNow(t, mount, MountOptions{OnDemand: true, Publish: true})
		if !pubRes.Pending.None() || pubRes.Background {
			t.Fatalf("publish remount after OnDemand background repair = %+v, want Pending.None Background=false", pubRes)
		}
		for _, idx := range []int{0, 1} {
			b, _, _, _ := readRowBlobs(t, mount, tracks[idx].EngineID)
			if len(b) == 0 || bytes.Equal(b, stockBeats) {
				t.Fatalf("track[%d] not repaired by OnDemand background build + publish", idx)
			}
		}
		b2, q2, _, _ := readRowBlobs(t, mount, tracks[2].EngineID)
		if !bytes.Equal(b2, stockBeats) || !bytes.Equal(q2, qcDJHot) {
			t.Fatal("track[2] DJ hot cue was modified by OnDemand background build")
		}
		finalSnap, err := rbstate.Load(dbDir(mount))
		if err != nil {
			t.Fatal(err)
		}
		if !finalSnap.Pending.None() {
			t.Fatalf("final snapshot still has pending work: %+v", finalSnap.Pending)
		}
		reinsert := mountNow(t, mount, MountOptions{OnDemand: true, Deadline: time.Now().Add(17 * time.Second)})
		if !reinsert.Bypassed || reinsert.Applied != 0 {
			t.Fatalf("post-recovery insert = %+v, want Bypassed=true Applied=0", reinsert)
		}
	})

	t.Run("recover_via_next_insert", func(t *testing.T) {
		mount, tracks, stockBeats, qcDJHot := setupExpiredMount(t)
		reinsert := mountNow(t, mount, MountOptions{OnDemand: true, Deadline: time.Now().Add(17 * time.Second)})
		if reinsert.Bypassed || reinsert.Applied != 2 || reinsert.Background || !reinsert.Pending.None() {
			t.Fatalf("next-insert recovery = %+v, want Applied=2 Background=false Pending.None", reinsert)
		}
		for _, idx := range []int{0, 1} {
			b, _, _, _ := readRowBlobs(t, mount, tracks[idx].EngineID)
			if len(b) == 0 || bytes.Equal(b, stockBeats) {
				t.Fatalf("track[%d] not repaired on next insert", idx)
			}
		}
		b2, q2, _, _ := readRowBlobs(t, mount, tracks[2].EngineID)
		if !bytes.Equal(b2, stockBeats) || !bytes.Equal(q2, qcDJHot) {
			t.Fatal("track[2] DJ hot cue was modified on next-insert recovery")
		}
		third := mountNow(t, mount, MountOptions{OnDemand: true, Deadline: time.Now().Add(17 * time.Second)})
		if !third.Bypassed || third.Applied != 0 {
			t.Fatalf("3rd insert = %+v, want Bypassed=true Applied=0", third)
		}
	})
}

// Fails at runtime on dc7a8b2e (reviewer's round-5 BLOCK): pausing at cut("repair next saved")
// until the transaction's soft-budget context expires rolls back SQLite; the pending snapshot
// saved after rollback MUST be based on the last committed snapshot (original Commit, nil
// Written, PerfNone) so the next insert repairs the row instead of bypassing it.
func TestReviewerR5CommitExpiryKeepsPendingRecoverable(t *testing.T) {
	mount := repairStick(t, 3)
	mountNow(t, mount, MountOptions{OnDemand: true})
	origSnap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	origCommit := origSnap.Commit
	tracks := sortedTracksByID(origSnap)

	const beat0 = 11391.4248046875
	stockBeats := buildTestStockOABeatsMount(t, 44100, 13707967, beat0, 136.0, 704)
	qcAutoBeat0 := buildTestQuickCuesMount(t, -1, "", beat0, 1, beat0)
	qcDJHot := buildTestQuickCuesMount(t, 220500, "DJKeep", beat0, 1, beat0)
	emptyLoops := buildTestEmptyLoopsMount()

	db := openTestMDBRW(t, mount)
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		[]byte{1}, []byte{1}, stockBeats, qcAutoBeat0, emptyLoops, tracks[0].EngineID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		[]byte{1}, []byte{1}, stockBeats, qcDJHot, emptyLoops, tracks[1].EngineID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	oldBudget, oldCut := insertBudget, cut
	insertBudget = 1500 * time.Millisecond
	paused := false
	cut = func(step string) {
		if step == "repair next saved" && !paused {
			paused = true
			time.Sleep(1600 * time.Millisecond)
		}
	}
	defer func() { insertBudget, cut = oldBudget, oldCut }()

	bgStarted := false
	res := mountNow(t, mount, MountOptions{
		OnDemand:        true,
		Deadline:        time.Now().Add(17 * time.Second),
		StartBackground: func() error { bgStarted = true; return nil },
	})
	if !paused {
		t.Fatal(`cut("repair next saved") was not hit`)
	}
	if res.Bypassed || res.Applied != 0 || !bgStarted || !res.Background || res.Pending.Perf != 1 {
		t.Fatalf("commit-expired mount = %+v (bgStarted=%v), want Applied=0 Background=true Pending.Perf=1", res, bgStarted)
	}

	// SQLite must have rolled back to the stock grid, and .rb-bridge-snapshot.next must be gone.
	bAfter, _, _, _ := readRowBlobs(t, mount, tracks[0].EngineID)
	if !bytes.Equal(bAfter, stockBeats) {
		t.Fatal("track[0] SQLite row was not rolled back after commit timeout")
	}
	if _, err := os.Stat(filepath.Join(dbDir(mount), rbstate.SnapshotName+".next")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".next file left behind after rollback: err=%v", err)
	}

	// Snapshot on disk must reflect the last committed snapshot (original Commit, nil Written, PerfNone) + Pending.Perf=1.
	afterSnap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	if afterSnap.Commit != origCommit {
		t.Fatalf("snapshot Commit after rollback = %d, want last committed %d", afterSnap.Commit, origCommit)
	}
	tr0 := afterSnap.Tracks[tracks[0].Key]
	if tr0 == nil || len(tr0.Written) != 0 || tr0.Perf != rbstate.PerfNone || len(tr0.Built.ANLZ) != 0 {
		t.Fatalf("track[0] snapshot kept uncommitted provenance after rollback: %+v", tr0)
	}
	if afterSnap.Pending.Perf != 1 {
		t.Fatalf("afterSnap.Pending.Perf = %d, want 1", afterSnap.Pending.Perf)
	}

	// Next insert with normal budget MUST repair track[0] instead of bypassing it, and keep track[1] DJ hot cue untouched.
	insertBudget, cut = oldBudget, oldCut
	res2 := mountNow(t, mount, MountOptions{OnDemand: true, Deadline: time.Now().Add(17 * time.Second)})
	if res2.Bypassed || res2.Applied != 1 || res2.Background || !res2.Pending.None() {
		t.Fatalf("recovery next insert = %+v, want Bypassed=false Applied=1 Background=false Pending.None", res2)
	}
	bRep, qRep, _, _ := readRowBlobs(t, mount, tracks[0].EngineID)
	if len(bRep) == 0 || bytes.Equal(bRep, stockBeats) || bytes.Equal(qRep, qcAutoBeat0) {
		t.Fatal("track[0] stayed broken on next insert after commit-expiry rollback")
	}
	b1, q1, _, _ := readRowBlobs(t, mount, tracks[1].EngineID)
	if !bytes.Equal(b1, stockBeats) || !bytes.Equal(q1, qcDJHot) {
		t.Fatal("track[1] DJ hot cue was modified during recovery")
	}
	res3 := mountNow(t, mount, MountOptions{OnDemand: true, Deadline: time.Now().Add(17 * time.Second)})
	if !res3.Bypassed || res3.Applied != 0 {
		t.Fatalf("3rd insert = %+v, want Bypassed=true Applied=0", res3)
	}
}

// Fails at runtime on dc7a8b2e: a pending-save error in deferOutstanding while parentCtx is live
// must NOT be swallowed (at either soft cutoff or hard mount-context deadline).
func TestOnDemandMountPendingSaveErrorNotSwallowed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		hardDeadline bool
	}{
		{"soft_cutoff", false},
		{"hard_deadline", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount := repairStick(t, 3)
			mountNow(t, mount, MountOptions{OnDemand: true})
			snap, err := rbstate.Load(dbDir(mount))
			if err != nil {
				t.Fatal(err)
			}
			tracks := sortedTracksByID(snap)

			const beat0 = 11391.4248046875
			stockBeats := buildTestStockOABeatsMount(t, 44100, 13707967, beat0, 136.0, 704)
			qcAutoBeat0 := buildTestQuickCuesMount(t, -1, "", beat0, 1, beat0)
			emptyLoops := buildTestEmptyLoopsMount()
			db := openTestMDBRW(t, mount)
			if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
				[]byte{1}, []byte{1}, stockBeats, qcAutoBeat0, emptyLoops, tracks[0].EngineID); err != nil {
				t.Fatal(err)
			}
			db.Close()

			snapPath := filepath.Join(dbDir(mount), rbstate.SnapshotName)
			blockSnapshotSave := func() {
				// Replacing the snapshot file with a non-empty directory makes atomic os.Rename
				// fail with EISDIR/ENOTEMPTY deterministically even when tests run as root.
				_ = os.Remove(snapPath)
				if err := os.MkdirAll(snapPath, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(snapPath, "block"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			oldBudget, oldHook := insertBudget, repairBeforeBuild
			defer func() { insertBudget, repairBeforeBuild = oldBudget, oldHook }()

			var dl time.Time
			if tc.hardDeadline {
				dl = time.Now().Add(commitReserve + 850*time.Millisecond)
				ctxLimit := dl.Add(-spare)
				repairBeforeBuild = func(_ context.Context, _ string) error {
					blockSnapshotSave()
					if wait := time.Until(ctxLimit) + 60*time.Millisecond; wait > 0 {
						time.Sleep(wait)
					}
					return nil
				}
			} else {
				insertBudget = 1200 * time.Millisecond
				dl = time.Now().Add(17 * time.Second)
				repairBeforeBuild = func(_ context.Context, _ string) error {
					blockSnapshotSave()
					time.Sleep(1100 * time.Millisecond)
					return nil
				}
			}

			res, err := ConvertAtMount(context.Background(), mount, MountOptions{
				OnDemand:        true,
				Deadline:        dl,
				StartBackground: func() error { return nil },
			})
			if err == nil {
				t.Fatalf("ConvertAtMount(%s) swallowed pending-save error: res=%+v err=nil", tc.name, res)
			}
		})
	}
}
