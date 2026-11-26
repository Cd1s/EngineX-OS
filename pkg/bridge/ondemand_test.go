package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
