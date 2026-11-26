package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func buildTestMP3(t *testing.T, dst string) {
	t.Helper()
	// Minimal valid MPEG1 Layer III 44.1 kHz 128 kbps stereo file with Xing/LAME header
	// (384 bytes/frame at 44100 Hz, 128 kbps, no padding: 144*128000/44100 = 417 bytes).
	const frameSize = 417
	frame := make([]byte, frameSize)
	// Sync 0xFFE + MPEG1 (11) + Layer3 (01) + no CRC (1) = 0xFF 0xFB
	// Bitrate index 9 (128 kbps) + SR 0 (44.1 kHz) + pad 0 + priv 0 = 0x90
	// Stereo (00) = 0x00
	frame[0], frame[1], frame[2], frame[3] = 0xFF, 0xFB, 0x90, 0x00
	// Side info for MPEG1 stereo is 32 bytes -> Xing tag at offset 4+32 = 36
	copy(frame[36:], "Xing")
	binary.BigEndian.PutUint32(frame[40:], 0x01) // frames field present
	binary.BigEndian.PutUint32(frame[44:], 200)  // 200 frames (~5.22 s)
	// LAME tag at 36 + 8 + 4 = 48? Standard LAME header starts at offset 156 from frame start (36 + 120).
	copy(frame[156:], "LAME3.100")
	// Delay 576 (0x240), padding 1728 (0x6C0) at offset 156+21 = 177 (3 bytes: 12-bit delay, 12-bit pad)
	frame[177] = 0x24
	frame[178] = 0x06
	frame[179] = 0xC0

	audioFrame := make([]byte, frameSize)
	audioFrame[0], audioFrame[1], audioFrame[2], audioFrame[3] = 0xFF, 0xFB, 0x90, 0x00

	var buf bytes.Buffer
	buf.Write(frame)
	for i := 0; i < 4; i++ {
		buf.Write(audioFrame)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOnDemandMatchesBuildTrackByteForByte(t *testing.T) {
	ctx := context.Background()
	stick := t.TempDir()
	mp3Path := filepath.Join(stick, "seed.mp3")
	// Unused helper mp3Path no longer needed in LargeExport tests.
	_ = mp3Path

	// Build a realistic Device Library + ANLZ stick using testfixture.LargeExport.
	testfixture.LargeExport(t, stick, 3)
	// Mount once (rows + playlists + snapshot with AnalysisPath; NO background fill, NO stage dir).
	res, err := ConvertAtMount(ctx, stick, MountOptions{})
	if err != nil {
		t.Fatalf("ConvertAtMount: %v", err)
	}
	if res.Added != 3 {
		t.Fatalf("Added=%d, want 3", res.Added)
	}

	dbDir := filepath.Join(stick, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatalf("Load snapshot: %v", err)
	}
	var tr *rbstate.Track
	for _, cand := range snap.Tracks {
		if cand.EngineID == 1 {
			tr = cand
			break
		}
	}
	if tr == nil || tr.AnalysisPath == "" {
		t.Fatalf("expected Track 1 with AnalysisPath in snapshot, got %+v", tr)
	}

	// Build reference via enginedb.StatTrack + enginedb.BuildTrack from the real export.
	resolver, err := compat.NewResolver(stick)
	if err != nil {
		t.Fatal(err)
	}
	sigs, err := FindSources(resolver, stick)
	if err != nil {
		t.Fatal(err)
	}
	libs, _, err := ReadSources(ctx, resolver, stick, sigs, nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := BuildViewCtx(ctx, libs, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var expTrack rekordbox.Track
	for _, vt := range view.Tracks {
		if vt.Key == tr.Key {
			expTrack = vt.Track
			break
		}
	}
	src, err := enginedb.StatTrack(ctx, resolver, expTrack)
	if err != nil {
		t.Fatalf("StatTrack: %v", err)
	}
	wantStaged, err := enginedb.BuildTrack(resolver, expTrack, src, enginedb.ReadOptions{})
	if err != nil {
		t.Fatalf("BuildTrack: %v", err)
	}
	wantRBXA1 := rbstate.EncodePerf(wantStaged.Perf)
	wantWrittenNoMain := writtenDigestIgnoringMainCueTail(wantStaged.Perf.Beats, wantStaged.Perf.QuickCues, wantStaged.Perf.Loops)

	engineKey := -1
	if k, ok := enginedb.EngineKey(expTrack.Key).(int); ok {
		engineKey = k
	}

	builder := NewOnDemandBuilder()
	got, err := builder.Build(ctx, OnDemandRequest{
		Mount:      stick,
		UUID:       snap.LibraryUUID,
		EnginePath: "../" + tr.Rel,
		EngineID:   tr.EngineID,
		EngineKey:  engineKey,
		EngineBPM:  expTrack.BPM,
		EngineLen:  int(math.Round(expTrack.Duration)),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.Timing.UsedExport {
		t.Fatalf("hot path unexpectedly read export DB: %+v", got.Timing)
	}
	if got.Key != tr.Key || got.RowHash != tr.Row || got.Rules != rbstate.Rules {
		t.Fatalf("metadata mismatch: got key=%q row=%q rules=%d, want %q %q %d", got.Key, got.RowHash, got.Rules, tr.Key, tr.Row, rbstate.Rules)
	}
	if got.ActiveLoops != wantStaged.Perf.ActiveLoops {
		t.Fatalf("ActiveLoops=%d, want %d", got.ActiveLoops, wantStaged.Perf.ActiveLoops)
	}
	if !bytes.Equal(got.Written, wantStaged.Digest) {
		t.Fatalf("Written digest mismatch: got %x, want %x", got.Written, wantStaged.Digest)
	}
	if !bytes.Equal(got.WrittenNoMain, wantWrittenNoMain) {
		t.Fatalf("WrittenNoMain mismatch: got %x, want %x", got.WrittenNoMain, wantWrittenNoMain)
	}
	if !bytes.Equal(got.RBXA1, wantRBXA1) {
		t.Fatalf("RBXA1 mismatch: len got=%d want=%d", len(got.RBXA1), len(wantRBXA1))
	}
}

func TestOnDemandNonASCIIFallbackSingleflightAndErrors(t *testing.T) {
	ctx := context.Background()
	stick := t.TempDir()
	mp3Path := filepath.Join(stick, "seed.mp3")
	// Unused helper mp3Path no longer needed in LargeExport tests.
	_ = mp3Path

	testfixture.LargeExport(t, stick, 2)
	if _, err := ConvertAtMount(ctx, stick, MountOptions{}); err != nil {
		t.Fatalf("ConvertAtMount: %v", err)
	}
	dbDir := filepath.Join(stick, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	var tr *rbstate.Track
	for _, cand := range snap.Tracks {
		if cand.EngineID == 1 {
			tr = cand
			break
		}
	}
	if tr == nil {
		t.Fatal("missing track 1")
	}

	builder := NewOnDemandBuilder()

	// 1. UUID mismatch must be rejected with ErrOnDemandUUIDMismatch.
	if _, err := builder.Build(ctx, OnDemandRequest{
		Mount:      stick,
		UUID:       "wrong-uuid",
		EnginePath: "../" + tr.Rel,
	}); !errors.Is(err, ErrOnDemandUUIDMismatch) {
		t.Fatalf("UUID mismatch err=%v, want ErrOnDemandUUIDMismatch", err)
	}

	// 2. Missing track returns ErrOnDemandNotFound.
	if _, err := builder.Build(ctx, OnDemandRequest{
		Mount:      stick,
		UUID:       snap.LibraryUUID,
		EnginePath: "../Contents/DoesNotExist.mp3",
	}); !errors.Is(err, ErrOnDemandNotFound) {
		t.Fatalf("missing track err=%v, want ErrOnDemandNotFound", err)
	}

	// 3. Concurrent requests coalesce cleanly under -race.
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	results := make(chan []byte, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := builder.Build(ctx, OnDemandRequest{
				Mount:      stick,
				UUID:       snap.LibraryUUID,
				EnginePath: "../" + tr.Rel,
				EngineID:   tr.EngineID,
				EngineKey:  0,
			})
			if err != nil {
				errs <- err
				return
			}
			results <- r.RBXA1
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatalf("concurrent Build failed: %v", err)
	}
	var first []byte
	for b := range results {
		if first == nil {
			first = b
		} else if !bytes.Equal(first, b) {
			t.Fatal("concurrent Build returned differing payloads")
		}
	}

	// 4. Fallback when an older snapshot lacks AnalysisPath (triggers lazy export lookup).
	tr.AnalysisPath = ""
	tr.Built.ANLZ = nil
	if err := snap.SaveNext(dbDir); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.Promote(dbDir); err != nil {
		t.Fatal(err)
	}
	// Reset snapshot cache via fresh builder to ensure new file is loaded.
	builder2 := NewOnDemandBuilder()
	resFallback, err := builder2.Build(ctx, OnDemandRequest{
		Mount:      stick,
		UUID:       snap.LibraryUUID,
		EnginePath: "../" + tr.Rel,
		EngineID:   tr.EngineID,
		EngineKey:  -1,
	})
	if err != nil {
		t.Fatalf("fallback Build failed: %v", err)
	}
	if !resFallback.Timing.UsedExport {
		t.Fatalf("expected UsedExport=true when snapshot AnalysisPath is empty")
	}
	if !bytes.Equal(resFallback.RBXA1, first) {
		t.Fatalf("fallback RBXA1 differs from direct path")
	}

	// 5. Missing audio file produces ErrOnDemandDegraded.
	if err := os.Remove(filepath.Join(stick, filepath.FromSlash(tr.Rel))); err != nil {
		t.Fatal(err)
	}
	if _, err := builder2.Build(ctx, OnDemandRequest{
		Mount:      stick,
		UUID:       snap.LibraryUUID,
		EnginePath: "../" + tr.Rel,
	}); !errors.Is(err, ErrOnDemandDegraded) {
		t.Fatalf("missing audio err=%v, want ErrOnDemandDegraded", err)
	}
}

func TestOnDemandNonASCIIAndCF049Clamp(t *testing.T) {
	ctx := context.Background()
	stick := t.TempDir()
	// Create a non-ASCII (NFC) audio file and ANLZ with 500 BPM (CF049) + valid beats fallback check.
	relAudio := "Contents/Caf\u00e9/M\u00fasica.mp3"
	buildTestMP3(t, filepath.Join(stick, filepath.FromSlash(relAudio)))

	// Write a valid .DAT with 4 beats at 128 BPM + 1 hot cue + 1 memory cue.
	anlzRel := "PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT"
	anlzFull := filepath.Join(stick, filepath.FromSlash(anlzRel))
	if err := os.MkdirAll(filepath.Dir(anlzFull), 0o755); err != nil {
		t.Fatal(err)
	}
	// Build PMAI with PQTZ + PCOB using rekordbox helper or synthetic bytes.
	dat := buildSyntheticDAT(128.0, 500, 1500)
	if err := os.WriteFile(anlzFull, dat, 0o644); err != nil {
		t.Fatal(err)
	}

	dbDir := filepath.Join(stick, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := rbstate.New("uuid-nonascii")
	key := rbstate.FoldPath(relAudio)
	snap.Tracks[key] = &rbstate.Track{
		Key:          key,
		EngineID:     7,
		Rel:          relAudio,
		AnalysisPath: anlzRel,
		Row:          "row-nonascii",
	}
	if err := snap.SaveNext(dbDir); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.Promote(dbDir); err != nil {
		t.Fatal(err)
	}

	// Request with NFD spelling ("Cafe\u0301/Mu\u0301sica.mp3") and CF049 BPM (500.0).
	builder := NewOnDemandBuilder()
	res, err := builder.Build(ctx, OnDemandRequest{
		Mount:      stick,
		UUID:       "uuid-nonascii",
		EnginePath: "../Contents/Cafe\u0301/Mu\u0301sica.mp3",
		EngineID:   7,
		EngineKey:  3, // 9A
		EngineBPM:  500.0,
		EngineLen:  10,
	})
	if err != nil {
		t.Fatalf("non-ASCII NFD Build failed: %v", err)
	}
	if res.Key != key || len(res.RBXA1) < 25 || len(res.Written) != 16 || len(res.WrittenNoMain) != 16 {
		t.Fatalf("unexpected result: key=%q rbxa1=%d wr=%d wrnm=%d", res.Key, len(res.RBXA1), len(res.Written), len(res.WrittenNoMain))
	}
	// Now test CF049 on the beat grid itself (>= 499.5 BPM grid is cleared -> incomplete -> degraded).
	dat500 := buildSyntheticDAT(500.0, 500, 1500)
	if err := os.WriteFile(anlzFull, dat500, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Build(ctx, OnDemandRequest{
		Mount:      stick,
		UUID:       "uuid-nonascii",
		EnginePath: "../" + relAudio,
		EngineID:   7,
	}); !errors.Is(err, ErrOnDemandDegraded) {
		t.Fatalf("CF049 500 BPM grid err=%v, want ErrOnDemandDegraded", err)
	}
}

func buildSyntheticDAT(bpm float64, memMS, hot1MS uint32) []byte {
	var pqtz bytes.Buffer
	pqtz.WriteString("PQTZ")
	binary.Write(&pqtz, binary.BigEndian, uint32(24))
	binary.Write(&pqtz, binary.BigEndian, uint32(24+4*8))
	binary.Write(&pqtz, binary.BigEndian, uint32(0))
	binary.Write(&pqtz, binary.BigEndian, uint32(0x80000))
	binary.Write(&pqtz, binary.BigEndian, uint32(4))
	bpm100 := uint16(math.Round(bpm * 100))
	for i := 0; i < 4; i++ {
		binary.Write(&pqtz, binary.BigEndian, uint16(i+1))
		binary.Write(&pqtz, binary.BigEndian, bpm100)
		binary.Write(&pqtz, binary.BigEndian, uint32(500+i*469))
	}

	makePCOB := func(kind uint32, hot uint32, timeMS uint32) []byte {
		var entry bytes.Buffer
		entry.WriteString("PCPT")
		binary.Write(&entry, binary.BigEndian, uint32(28))
		binary.Write(&entry, binary.BigEndian, uint32(56))
		binary.Write(&entry, binary.BigEndian, hot)
		binary.Write(&entry, binary.BigEndian, uint32(1)) // status active
		binary.Write(&entry, binary.BigEndian, uint32(0x10000))
		binary.Write(&entry, binary.BigEndian, uint16(1))
		binary.Write(&entry, binary.BigEndian, uint16(0xffff))
		entry.WriteByte(1) // single cue
		entry.Write([]byte{0, 0, 0})
		binary.Write(&entry, binary.BigEndian, timeMS)
		binary.Write(&entry, binary.BigEndian, uint32(0xffffffff))
		entry.Write(make([]byte, 56-entry.Len()))

		var sec bytes.Buffer
		sec.WriteString("PCOB")
		binary.Write(&sec, binary.BigEndian, uint32(24))
		binary.Write(&sec, binary.BigEndian, uint32(24+entry.Len()))
		binary.Write(&sec, binary.BigEndian, kind)
		binary.Write(&sec, binary.BigEndian, uint32(1))
		binary.Write(&sec, binary.BigEndian, uint32(0xffffffff))
		sec.Write(entry.Bytes())
		return sec.Bytes()
	}

	pcobMem := makePCOB(0, 0, memMS)
	pcobHot := makePCOB(1, 1, hot1MS)

	// Minimal PWAV overview (400 bytes)
	var pwav bytes.Buffer
	pwav.WriteString("PWAV")
	binary.Write(&pwav, binary.BigEndian, uint32(20))
	binary.Write(&pwav, binary.BigEndian, uint32(20+400))
	binary.Write(&pwav, binary.BigEndian, uint32(400))
	binary.Write(&pwav, binary.BigEndian, uint32(0x10000))
	for i := 0; i < 400; i++ {
		pwav.WriteByte(0x18)
	}

	total := 28 + pqtz.Len() + len(pcobMem) + len(pcobHot) + pwav.Len()
	var out bytes.Buffer
	out.WriteString("PMAI")
	binary.Write(&out, binary.BigEndian, uint32(28))
	binary.Write(&out, binary.BigEndian, uint32(total))
	out.Write(make([]byte, 16))
	out.Write(pqtz.Bytes())
	out.Write(pcobMem)
	out.Write(pcobHot)
	out.Write(pwav.Bytes())
	return out.Bytes()
}

func TestOnDemandSymlinkRootConfinementMetadataKeyMemoryLoopsAndAACSBR(t *testing.T) {
	ctx := context.Background()

	// 1. Confined under os.Root: intermediate symlink inside stick escaping root must be rejected.
	stick1 := t.TempDir()
	outside := t.TempDir()
	outsideDir := filepath.Join(outside, "Contents")
	buildTestMP3(t, filepath.Join(outsideDir, "escape.mp3"))
	if err := os.Symlink(outsideDir, filepath.Join(stick1, "Contents")); err != nil {
		t.Fatal(err)
	}
	anlzRel := "PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT"
	anlzFull := filepath.Join(stick1, filepath.FromSlash(anlzRel))
	if err := os.MkdirAll(filepath.Dir(anlzFull), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anlzFull, buildSyntheticDAT(128.0, 500, 1500), 0o644); err != nil {
		t.Fatal(err)
	}
	dbDir1 := filepath.Join(stick1, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir1, 0o755); err != nil {
		t.Fatal(err)
	}
	snap1 := rbstate.New("uuid-sym")
	rel1 := "Contents/escape.mp3"
	snap1.Tracks[rbstate.FoldPath(rel1)] = &rbstate.Track{
		Key: rbstate.FoldPath(rel1), EngineID: 1, Rel: rel1, AnalysisPath: anlzRel, Row: "r1",
	}
	if err := snap1.SaveNext(dbDir1); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.Promote(dbDir1); err != nil {
		t.Fatal(err)
	}
	b := NewOnDemandBuilder()
	if _, err := b.Build(ctx, OnDemandRequest{Mount: stick1, UUID: "uuid-sym", EnginePath: "../" + rel1, EngineID: 1}); err == nil {
		t.Fatal("expected symlink escape under stick Contents to fail")
	}

	// 2. Concurrent requests with different EngineKey do not coalesce onto one key.
	stick2 := t.TempDir()
	testfixture.LargeExport(t, stick2, 1)
	if _, err := ConvertAtMount(ctx, stick2, MountOptions{OnDemand: true}); err != nil {
		t.Fatal(err)
	}
	snap2, err := rbstate.Load(filepath.Join(stick2, "Engine Library", "Database2"))
	if err != nil {
		t.Fatal(err)
	}
	var tr2 *rbstate.Track
	for _, c := range snap2.Tracks {
		tr2 = c
		break
	}
	var resK0, resK1 *OnDemandResult
	var errK0, errK1 error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		resK0, errK0 = b.Build(ctx, OnDemandRequest{Mount: stick2, UUID: snap2.LibraryUUID, EnginePath: "../" + tr2.Rel, EngineID: tr2.EngineID, EngineKey: 0})
	}()
	go func() {
		defer wg.Done()
		resK1, errK1 = b.Build(ctx, OnDemandRequest{Mount: stick2, UUID: snap2.LibraryUUID, EnginePath: "../" + tr2.Rel, EngineID: tr2.EngineID, EngineKey: 2})
	}()
	wg.Wait()
	if errK0 != nil || errK1 != nil {
		t.Fatalf("concurrent metadata builds: err0=%v err1=%v", errK0, errK1)
	}
	if bytes.Equal(resK0.RBXA1, resK1.RBXA1) {
		t.Fatal("requests with differing EngineKey coalesced onto identical RBXA1")
	}

	// 3. mergeFastCues preserves standalone saved memory loops (HotCues empty in slot).
	basePerf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 60, SampleRate: 44100, SampleCount: 60 * 44100}, &rekordbox.Analysis{
		Cues: []rekordbox.Cue{{HotCue: 0, Loop: true, TimeMS: 1000, EndMS: 3000, Name: "saved-loop", Active: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	an := &rekordbox.Analysis{}
	mergeFastCues(an, &rbstate.Track{FastCues: basePerf.QuickCues, FastLoops: basePerf.Loops, FastActive: basePerf.ActiveLoops}, 44100, 60*44100, 60)
	if len(an.Cues) != 1 || !an.Cues[0].Loop || an.Cues[0].HotCue != 0 || an.Cues[0].TimeMS != 1000 || an.Cues[0].EndMS != 3000 || !an.Cues[0].Active {
		t.Fatalf("mergeFastCues lost standalone memory loop: %+v", an.Cues)
	}

	// 4. Implicit AAC SBR (22.05 kHz core container header + 44.1 kHz export metadata + 2048 priming)
	// matches enginedb.BuildTrack byte-for-byte.
	aacStick := t.TempDir()
	aacRel := testfixture.AACExport(t, aacStick)
	rawAAC := testfixture.AACWithPriming()
	mp4aOff := bytes.Index(rawAAC, []byte("mp4a"))
	binary.BigEndian.PutUint32(rawAAC[mp4aOff+4+24:], 22050<<16)
	testfixture.Write(t, aacStick, aacRel, rawAAC)
	if _, err := ConvertAtMount(ctx, aacStick, MountOptions{OnDemand: true}); err != nil {
		t.Fatalf("ConvertAtMount AAC SBR: %v", err)
	}
	aacSnap, err := rbstate.Load(filepath.Join(aacStick, "Engine Library", "Database2"))
	if err != nil {
		t.Fatal(err)
	}
	var aacTR *rbstate.Track
	for _, c := range aacSnap.Tracks {
		aacTR = c
		break
	}
	pdb, err := rekordbox.ReadPDB(filepath.Join(aacStick, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	expTrack := pdb.Tracks[0]
	aacResv, err := compat.NewResolver(aacStick)
	if err != nil {
		t.Fatal(err)
	}
	srcAAC, err := enginedb.StatTrack(ctx, aacResv, expTrack)
	if err != nil {
		t.Fatal(err)
	}
	wantAAC, err := enginedb.BuildTrack(aacResv, expTrack, srcAAC, enginedb.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantAACBytes := rbstate.EncodePerf(wantAAC.Perf)
	ek := -1
	if k, ok := enginedb.EngineKey(expTrack.Key).(int); ok {
		ek = k
	}
	gotAAC, err := NewOnDemandBuilder().Build(ctx, OnDemandRequest{
		Mount:      aacStick,
		UUID:       aacSnap.LibraryUUID,
		EnginePath: "../" + aacTR.Rel,
		EngineID:   aacTR.EngineID,
		EngineKey:  ek,
		EngineBPM:  expTrack.BPM,
		EngineLen:  int(math.Round(expTrack.Duration)),
	})
	if err != nil {
		t.Fatalf("OnDemand AAC SBR Build: %v", err)
	}
	if gotAAC.Timing.UsedExport {
		t.Fatal("current snapshot AAC SBR unexpectedly reparsed whole export DB")
	}
	if !bytes.Equal(gotAAC.RBXA1, wantAACBytes) {
		t.Fatalf("OnDemand AAC SBR RBXA1 differs from enginedb.BuildTrack (%d vs %d)", len(gotAAC.RBXA1), len(wantAACBytes))
	}

	// Legacy snapshot lacking SampleRate falls back to rooted export metadata cleanly.
	aacTR.SampleRate = 0
	if err := aacSnap.SaveNext(filepath.Join(aacStick, "Engine Library", "Database2")); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.Promote(filepath.Join(aacStick, "Engine Library", "Database2")); err != nil {
		t.Fatal(err)
	}
	gotAACLegacy, err := NewOnDemandBuilder().Build(ctx, OnDemandRequest{
		Mount:      aacStick,
		UUID:       aacSnap.LibraryUUID,
		EnginePath: "../" + aacTR.Rel,
		EngineID:   aacTR.EngineID,
		EngineKey:  ek,
		EngineBPM:  expTrack.BPM,
		EngineLen:  int(math.Round(expTrack.Duration)),
	})
	if err != nil {
		t.Fatalf("OnDemand legacy AAC SBR Build: %v", err)
	}
	if !gotAACLegacy.Timing.UsedExport {
		t.Fatal("legacy AAC SBR without snapshot SampleRate did not fall back to export")
	}
	if !bytes.Equal(gotAACLegacy.RBXA1, wantAACBytes) {
		t.Fatalf("legacy AAC SBR RBXA1 differs from enginedb.BuildTrack (%d vs %d)", len(gotAACLegacy.RBXA1), len(wantAACBytes))
	}
}

func TestOnDemandConcurrencyLastWaiterCancelSharedMetaFIFOAndLimits(t *testing.T) {
	ctx := context.Background()
	stick := t.TempDir()
	testfixture.LargeExport(t, stick, 2)
	if _, err := ConvertAtMount(ctx, stick, MountOptions{OnDemand: true}); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(stick, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	var tr1, tr2 *rbstate.Track
	for _, c := range snap.Tracks {
		if c.EngineID == 1 {
			tr1 = c
		} else if c.EngineID == 2 {
			tr2 = c
		}
	}
	if tr1 == nil || tr2 == nil {
		t.Fatal("missing tracks 1 and 2")
	}

	// 1. Last waiter leaving cancels active build context immediately.
	enteredANLZ := make(chan struct{})
	canceledANLZ := make(chan struct{})
	oldHook := onDemandBuildHook
	onDemandBuildHook = func(hctx context.Context, stage, _ string) error {
		if stage == "anlz" {
			close(enteredANLZ)
			<-hctx.Done()
			close(canceledANLZ)
			return hctx.Err()
		}
		return nil
	}
	t.Cleanup(func() { onDemandBuildHook = oldHook })

	b := NewOnDemandBuilder()
	cctx, ccancel := context.WithCancel(ctx)
	doneCh := make(chan error, 1)
	go func() {
		_, err := b.Build(cctx, OnDemandRequest{Mount: stick, UUID: snap.LibraryUUID, EnginePath: "../" + tr1.Rel, EngineID: tr1.EngineID})
		doneCh <- err
	}()
	<-enteredANLZ
	ccancel()
	select {
	case <-canceledANLZ:
	case <-time.After(time.Second):
		t.Fatal("last waiter cancellation did not cancel active worker prompt")
	}
	<-doneCh

	// 2. Concurrent cold misses share one snapshot load and Caller 1 cancelling does not close Caller 2's root.
	var snapLoads atomic.Int32
	gate1 := make(chan struct{})
	releaseGate := make(chan struct{})
	onDemandBuildHook = func(hctx context.Context, stage, _ string) error {
		if stage == "snapshot" {
			snapLoads.Add(1)
			close(gate1)
			select {
			case <-releaseGate:
			case <-hctx.Done():
				return hctx.Err()
			}
		}
		return nil
	}
	bShared := NewOnDemandBuilder()
	ctx1, cancel1 := context.WithCancel(ctx)
	var err1, err2 error
	var res2 *OnDemandResult
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = bShared.Build(ctx1, OnDemandRequest{Mount: stick, UUID: snap.LibraryUUID, EnginePath: "../" + tr1.Rel, EngineID: tr1.EngineID})
	}()
	go func() {
		defer wg.Done()
		<-gate1
		res2, err2 = bShared.Build(ctx, OnDemandRequest{Mount: stick, UUID: snap.LibraryUUID, EnginePath: "../" + tr2.Rel, EngineID: tr2.EngineID})
	}()
	<-gate1
	// Wait deterministically until caller 2 has joined snapFlight (waiters == 2) before cancelling caller 1.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		bShared.metaMu.Lock()
		waiters := 0
		for _, c := range bShared.snapFlight {
			waiters = c.waiters
		}
		bShared.metaMu.Unlock()
		if waiters >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel1()
	close(releaseGate)
	wg.Wait()
	onDemandBuildHook = nil
	if !errors.Is(err1, context.Canceled) {
		t.Fatalf("canceled caller1 err=%v, want context.Canceled", err1)
	}
	if err2 != nil || res2 == nil || len(res2.RBXA1) == 0 {
		t.Fatalf("caller2 failed after shared snapshot load: res=%v err=%v", res2, err2)
	}
	if snapLoads.Load() != 1 {
		t.Fatalf("cold snapshot loads across 2 concurrent tracks = %d, want 1", snapLoads.Load())
	}

	// 3. RequireMountpoint rejects non-mountpoint subdirectory under MediaRoot.
	bStrict := NewOnDemandBuilderWithConfig(OnDemandBuilderConfig{
		MediaRoot:         filepath.Dir(stick),
		RequireMountpoint: true,
	})
	if _, err := bStrict.Build(ctx, OnDemandRequest{Mount: stick, UUID: snap.LibraryUUID, EnginePath: "../" + tr1.Rel}); !errors.Is(err, ErrOnDemandNotFound) {
		t.Fatalf("RequireMountpoint subdir err=%v, want ErrOnDemandNotFound", err)
	}

	// 4. FIFO at ANLZ path must fail cleanly without hanging (O_NONBLOCK + IsRegular check).
	fifoStick := t.TempDir()
	testfixture.LargeExport(t, fifoStick, 1)
	if _, err := ConvertAtMount(ctx, fifoStick, MountOptions{OnDemand: true}); err != nil {
		t.Fatal(err)
	}
	fsnap, _ := rbstate.Load(filepath.Join(fifoStick, "Engine Library", "Database2"))
	var ftr *rbstate.Track
	for _, c := range fsnap.Tracks {
		ftr = c
		break
	}
	datRel, extRel, ex2Rel := anlzCandidates(ftr, "")
	for _, rel := range []string{datRel, extRel, ex2Rel} {
		if rel == "" {
			continue
		}
		full := filepath.Join(fifoStick, filepath.FromSlash(strings.TrimLeft(rel, "/")))
		_ = os.Remove(full)
		if rel == ex2Rel {
			if err := syscall.Mkfifo(full, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	t0 := time.Now()
	if _, err := NewOnDemandBuilder().Build(ctx, OnDemandRequest{Mount: fifoStick, UUID: fsnap.LibraryUUID, EnginePath: "../" + ftr.Rel}); err == nil || time.Since(t0) > time.Second {
		t.Fatalf("FIFO ANLZ did not fail fast: err=%v elapsed=%s", err, time.Since(t0))
	}

	// 5. Oversized ANLZ and snapshot are rejected before reading.
	bigANLZ := filepath.Join(fifoStick, filepath.FromSlash(strings.TrimLeft(ex2Rel, "/")))
	_ = os.Remove(bigANLZ)
	bf, err := os.Create(bigANLZ)
	if err != nil {
		t.Fatal(err)
	}
	if err := bf.Truncate(onDemandMaxANLZBytes + 4096); err != nil {
		bf.Close()
		t.Fatal(err)
	}
	bf.Close()
	if _, err := NewOnDemandBuilder().Build(ctx, OnDemandRequest{Mount: fifoStick, UUID: fsnap.LibraryUUID, EnginePath: "../" + ftr.Rel}); !errors.Is(err, ErrOnDemandLimit) {
		t.Fatalf("oversized ANLZ err=%v, want ErrOnDemandLimit", err)
	}

	bigSnapStick := t.TempDir()
	bigSnapPath := filepath.Join(bigSnapStick, "Engine Library", "Database2", rbstate.SnapshotName)
	if err := os.MkdirAll(filepath.Dir(bigSnapPath), 0o755); err != nil {
		t.Fatal(err)
	}
	sf, err := os.Create(bigSnapPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.Truncate(onDemandMaxSnapshotBytes + 4096); err != nil {
		sf.Close()
		t.Fatal(err)
	}
	sf.Close()
	if _, err := NewOnDemandBuilder().Build(ctx, OnDemandRequest{Mount: bigSnapStick, EnginePath: "../Contents/a.mp3"}); !errors.Is(err, ErrOnDemandLimit) {
		t.Fatalf("oversized snapshot err=%v, want ErrOnDemandLimit", err)
	}

	// 6. Rooted export fallback refuses symlinked PIONEER/rekordbox/export.pdb pointing outside mount.
	symStick := t.TempDir()
	testfixture.LargeExport(t, symStick, 1)
	if _, err := ConvertAtMount(ctx, symStick, MountOptions{OnDemand: true}); err != nil {
		t.Fatal(err)
	}
	outsideExport := filepath.Join(t.TempDir(), "outside-export.pdb")
	pdbOrig := filepath.Join(symStick, "PIONEER", "rekordbox", "export.pdb")
	if err := os.Rename(pdbOrig, outsideExport); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideExport, pdbOrig); err != nil {
		t.Fatal(err)
	}
	symDB := filepath.Join(symStick, "Engine Library", "Database2")
	symSnap, err := rbstate.Load(symDB)
	if err != nil {
		t.Fatal(err)
	}
	var symTR *rbstate.Track
	for _, c := range symSnap.Tracks {
		c.AnalysisPath, c.Built.ANLZ = "", nil
		symTR = c
	}
	_ = symSnap.SaveNext(symDB)
	_ = rbstate.Promote(symDB)
	if _, err := NewOnDemandBuilder().Build(ctx, OnDemandRequest{Mount: symStick, UUID: symSnap.LibraryUUID, EnginePath: "../" + symTR.Rel}); !errors.Is(err, ErrOnDemandDegraded) {
		t.Fatalf("symlinked export.pdb in rooted fallback err=%v, want ErrOnDemandDegraded", err)
	}

	// 7. RD-3 proof (fails on f65d4fdf): an encrypted exportLibrary.db is NEVER opened or decrypted
	// on the load path even when a legacy snapshot lacks AnalysisPath (0 KeyDerivations, fast ErrOnDemandDegraded).
	encStick := t.TempDir()
	encSrc := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary.db")
	encRaw, err := os.ReadFile(encSrc)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, encStick, "PIONEER/rekordbox/exportLibrary.db", encRaw)
	encAudio := filepath.Join(encStick, "Contents", "enc.mp3")
	buildTestMP3(t, encAudio)
	encDBDir := filepath.Join(encStick, "Engine Library", "Database2")
	if err := os.MkdirAll(encDBDir, 0o755); err != nil {
		t.Fatal(err)
	}
	encSnap := rbstate.New("uuid-enc-only")
	encSnap.Tracks[rbstate.FoldPath("Contents/enc.mp3")] = &rbstate.Track{
		Key: rbstate.FoldPath("Contents/enc.mp3"), EngineID: 1, Rel: "Contents/enc.mp3", Row: "r-enc",
	}
	if err := encSnap.SaveNext(encDBDir); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.Promote(encDBDir); err != nil {
		t.Fatal(err)
	}
	kdBefore := rekordbox.KeyDerivations()
	tEnc := time.Now()
	_, encErr := NewOnDemandBuilder().Build(ctx, OnDemandRequest{
		Mount:      encStick,
		UUID:       encSnap.LibraryUUID,
		EnginePath: "../Contents/enc.mp3",
		EngineID:   1,
	})
	if !errors.Is(encErr, ErrOnDemandDegraded) {
		t.Fatalf("legacy OneLibrary-only without AnalysisPath: err=%v, want ErrOnDemandDegraded", encErr)
	}
	if time.Since(tEnc) > 500*time.Millisecond || rekordbox.KeyDerivations() != kdBefore {
		t.Fatalf("load path unexpectedly touched/decrypted exportLibrary.db: elapsed=%s kd=%d->%d", time.Since(tEnc), kdBefore, rekordbox.KeyDerivations())
	}
}
