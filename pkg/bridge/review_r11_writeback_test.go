package bridge

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestReviewR11AlphaOnlyColorUnchanged(t *testing.T) {
	ctx := context.Background()
	rbBlue255 := [4]byte{255, 0x30, 0x5a, 0xff} // #305AFF, alpha 255
	rbBlue128 := [4]byte{128, 0x30, 0x5a, 0xff} // #305AFF, alpha 128
	rbBlue0 := [4]byte{0, 0x30, 0x5a, 0xff}     // #305AFF, alpha 0 (parser normalizes -> 255)
	pureBlue128 := [4]byte{128, 0x00, 0x00, 0xff}
	pureBlue255 := [4]byte{255, 0x00, 0x00, 0xff}

	assertUnchangedDestinations := func(t *testing.T, root, extRel, twoExRel, onePath string, wantMS uint32, wantUpdateCount int64) {
		t.Helper()
		for label, list := range map[string][]rekordbox.Cue{
			"EXT": readSingleANLZCues(t, root, extRel),
			"2EX": readSingleANLZCues(t, root, twoExRel),
		} {
			if len(list) != 1 || !near(list[0].TimeMS, wantMS) || list[0].ColorCode != 1 || list[0].Color != rbBlue255 {
				t.Errorf("%s: expected %dms code=1 rgb=%v, got %+v", label, wantMS, rbBlue255, list)
			}
		}
		oneCues := readOneLibTrack1(t, onePath).Cues
		if len(oneCues) != 1 || !near(oneCues[0].TimeMS, wantMS) || !uncoloured(oneCues[0]) {
			t.Errorf("OneLibrary: expected %dms uncoloured (palette 0), got %+v", wantMS, oneCues)
		}
		meta := snapshotOneLibCueRows(t, onePath, 1)
		if !meta.cueUpdateCount.Valid || meta.cueUpdateCount.Int64 != wantUpdateCount {
			t.Errorf("OneLibrary cueUpdateCount = %+v, want %d", meta.cueUpdateCount, wantUpdateCount)
		}
	}

	for _, dir := range []struct {
		name       string
		firstAlpha [4]byte
		nextAlpha  [4]byte
	}{
		{name: "settled_255_then_stored_128", firstAlpha: rbBlue255, nextAlpha: rbBlue128},
		{name: "settled_128_then_engine_255", firstAlpha: rbBlue128, nextAlpha: rbBlue255},
		{name: "settled_255_then_alpha_0_nonzero_rgb", firstAlpha: rbBlue255, nextAlpha: rbBlue0},
	} {
		t.Run("alpha_only_noop/"+dir.name, func(t *testing.T) {
			extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: rbBlue255, ColorCode: 1}
			oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800}
			root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

			beforeMeta := snapshotOneLibCueRows(t, onePath, 1)
			if !beforeMeta.cueUpdateCount.Valid || beforeMeta.cueUpdateCount.Int64 != 1 {
				t.Fatalf("unexpected initial cueUpdateCount=%+v", beforeMeta.cueUpdateCount)
			}

			writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", dir.firstAlpha)
			settleFixture(t, root, dbDir, false)

			writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", dir.nextAlpha)
			res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true})
			if err != nil || res.TracksChanged != 0 || res.FilesUpdated != 0 || res.OneLibraryUpdated {
				t.Fatalf("alpha-only diff triggered write-back: res=%+v err=%v", res, err)
			}
			assertUnchangedDestinations(t, root, extRel, twoExRel, onePath, 800, 1)
		})
	}

	t.Run("alpha_128_move_only_preserves_uncoloured_onelib", func(t *testing.T) {
		extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: rbBlue255, ColorCode: 1}
		oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", rbBlue255)
		settleFixture(t, root, dbDir, false)

		writeDeckPad1Cue(t, dbDir, rbOffset, 2400, "Shared 1", rbBlue128)
		if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 1 {
			t.Fatalf("move WriteBackCues: res=%+v err=%v", res, err)
		}
		assertUnchangedDestinations(t, root, extRel, twoExRel, onePath, 2400, 2)
	})

	t.Run("alpha_128_with_rgb_edit_propagates_and_stabilizes", func(t *testing.T) {
		extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: rbBlue255, ColorCode: 1}
		oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", rbBlue255)
		settleFixture(t, root, dbDir, false)

		// Change RGB (#305AFF -> #0000FF) even if stored with alpha 128; exports always store alpha 255.
		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", pureBlue128)
		res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true})
		if err != nil || res.TracksChanged != 1 || !res.OneLibraryUpdated {
			t.Fatalf("RGB edit WriteBackCues: res=%+v err=%v", res, err)
		}
		for label, list := range map[string][]rekordbox.Cue{
			"EXT": readSingleANLZCues(t, root, extRel),
			"2EX": readSingleANLZCues(t, root, twoExRel),
		} {
			if len(list) != 1 || list[0].ColorCode != 1 || list[0].Color != pureBlue255 {
				t.Errorf("%s: expected code=1 rgb=%v, got %+v", label, pureBlue255, list)
			}
		}
		oneCues := readOneLibTrack1(t, onePath).Cues
		if len(oneCues) != 1 || oneCues[0].ColorCode != 1 {
			t.Errorf("OneLibrary: expected code=1, got %+v", oneCues)
		}

		if res2, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
			t.Fatalf("repeat WriteBackCues: res=%+v err=%v", res2, err)
		}
	})

	t.Run("zero_unknown_marker_distinct_from_black", func(t *testing.T) {
		if sameDeckCueColor([4]byte{}, [4]byte{255, 0, 0, 0}) {
			t.Error("empty sentinel [0,0,0,0] must not match opaque black [255,0,0,0]")
		}
		if !sameDeckCueColor([4]byte{128, 0, 0, 0}, [4]byte{255, 0, 0, 0}) {
			t.Error("alpha-128 black [128,0,0,0] must match alpha-255 black [255,0,0,0]")
		}
		if !sameDeckCueColor(rbBlue0, rbBlue255) || !sameDeckCueColor(rbBlue128, rbBlue255) {
			t.Error("sameDeckCueColor should ignore alpha for non-empty RGB values")
		}
	})

	t.Run("hot_loop_alpha_only_128_noop", func(t *testing.T) {
		extLoop := rekordbox.Cue{HotCue: 1, Name: "Loop 1", TimeMS: 800, EndMS: 2800, Loop: true, Color: rbBlue255, ColorCode: 1}
		oneLoop := rekordbox.Cue{HotCue: 1, Name: "Loop 1", TimeMS: 800, EndMS: 2800, Loop: true}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extLoop, oneLoop, true)

		writeDeckPad1HotLoop(t, dbDir, rbOffset, 800, 2800, "Loop 1", rbBlue255)
		settleFixture(t, root, dbDir, false)

		// Overwrite Pad 1 both quickCues AND loops color alpha -> 128 in m.db.
		writeDeckPad1HotLoop(t, dbDir, rbOffset, 800, 2800, "Loop 1", rbBlue128)
		db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		var loopsBlob []byte
		if err := db.QueryRow(`SELECT loops FROM PerformanceData WHERE trackId=1`).Scan(&loopsBlob); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		// loops layout: 8-byte header, then slot 0 = (1-byte nameLen + name + 8-byte start + 8-byte end + 2 flags + 4-byte ARGB).
		alphaIdx := 8 + 1 + len("Loop 1") + 18
		if len(loopsBlob) <= alphaIdx {
			_ = db.Close()
			t.Fatalf("unexpected loops blob length %d", len(loopsBlob))
		}
		loopsBlob[alphaIdx] = 128
		var prevEdit, curEdit int64
		_ = db.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=1`).Scan(&prevEdit)
		if _, err := db.Exec(`UPDATE PerformanceData SET loops=? WHERE trackId=1`, loopsBlob); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		_ = db.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=1`).Scan(&curEdit)
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=? WHERE id=1`, max(prevEdit, curEdit)+500); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		_ = db.Close()

		res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true})
		if err != nil || res.TracksRead != 1 || res.TracksChanged != 0 || res.FilesUpdated != 0 || res.OneLibraryUpdated {
			t.Fatalf("hot-loop alpha-only diff triggered write-back: res=%+v err=%v", res, err)
		}
		for label, list := range map[string][]rekordbox.Cue{
			"EXT": readSingleANLZCues(t, root, extRel),
			"2EX": readSingleANLZCues(t, root, twoExRel),
		} {
			if len(list) != 1 || !list[0].Loop || list[0].ColorCode != 1 || list[0].Color != rbBlue255 {
				t.Errorf("%s: expected loop code=1 rgb=%v, got %+v", label, rbBlue255, list)
			}
		}
		oneCues := readOneLibTrack1(t, onePath).Cues
		if len(oneCues) != 1 || !oneCues[0].Loop || !uncoloured(oneCues[0]) {
			t.Errorf("OneLibrary: expected loop uncoloured, got %+v", oneCues)
		}
	})
}
