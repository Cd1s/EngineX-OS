package bridge

import (
	"context"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestReviewR10ExactRGBWithinPalette(t *testing.T) {
	ctx := context.Background()
	rbBlue := [4]byte{255, 0x30, 0x5a, 0xff}   // #305AFF, rekordbox palette 1
	pureBlue := [4]byte{255, 0x00, 0x00, 0xff} // #0000FF, also rekordbox palette 1
	if code := rekordbox.NearestCueColorCode(rbBlue); code != 1 {
		t.Fatalf("rbBlue palette code = %d, want 1", code)
	}
	if code := rekordbox.NearestCueColorCode(pureBlue); code != 1 {
		t.Fatalf("pureBlue palette code = %d, want 1", code)
	}

	assertExactANLZAndOneLib := func(t *testing.T, root, extRel, twoExRel, onePath string, wantMS uint32, isLoop bool, wantRGB [4]byte, wantOneCode byte) {
		t.Helper()
		for label, list := range map[string][]rekordbox.Cue{
			"EXT": readSingleANLZCues(t, root, extRel),
			"2EX": readSingleANLZCues(t, root, twoExRel),
		} {
			if len(list) != 1 || !near(list[0].TimeMS, wantMS) || list[0].Loop != isLoop || list[0].ColorCode != 1 || list[0].Color != wantRGB {
				t.Errorf("%s: expected %dms loop=%v code=1 rgb=%v, got %+v", label, wantMS, isLoop, wantRGB, list)
			}
		}
		oneCues := readOneLibTrack1(t, onePath).Cues
		if len(oneCues) != 1 || !near(oneCues[0].TimeMS, wantMS) || oneCues[0].Loop != isLoop || oneCues[0].ColorCode != wantOneCode {
			t.Errorf("OneLibrary: expected %dms loop=%v code=%d, got %+v", wantMS, isLoop, wantOneCode, oneCues)
		}
	}

	t.Run("move_and_recolour_same_palette_entry", func(t *testing.T) {
		extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: rbBlue, ColorCode: 1}
		oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: rbBlue, ColorCode: 1}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", rbBlue)
		settleFixture(t, root, dbDir, false)

		writeDeckPad1Cue(t, dbDir, rbOffset, 2400, "Shared 1", pureBlue)
		if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 1 {
			t.Fatalf("WriteBackCues: res=%+v err=%v", res, err)
		}
		assertExactANLZAndOneLib(t, root, extRel, twoExRel, onePath, 2400, false, pureBlue, 1)

		if res2, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
			t.Fatalf("repeat WriteBackCues: res=%+v err=%v", res2, err)
		}
	})

	t.Run("colour_only_same_palette_entry", func(t *testing.T) {
		extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: rbBlue, ColorCode: 1}
		oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: rbBlue, ColorCode: 1}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", rbBlue)
		settleFixture(t, root, dbDir, false)

		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", pureBlue)
		res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true})
		if err != nil || res.TracksChanged != 1 || res.OneLibraryUpdated {
			t.Fatalf("WriteBackCues: res=%+v err=%v (want TracksChanged=1, OneLibraryUpdated=false)", res, err)
		}
		assertExactANLZAndOneLib(t, root, extRel, twoExRel, onePath, 800, false, pureBlue, 1)

		if res2, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
			t.Fatalf("repeat WriteBackCues: res=%+v err=%v", res2, err)
		}
	})

	t.Run("colour_only_same_palette_entry_uncoloured_onelibrary", func(t *testing.T) {
		extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: rbBlue, ColorCode: 1}
		oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", rbBlue)
		settleFixture(t, root, dbDir, false)

		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", pureBlue)
		res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true})
		if err != nil || res.TracksChanged != 1 || !res.OneLibraryUpdated {
			t.Fatalf("WriteBackCues: res=%+v err=%v (want TracksChanged=1, OneLibraryUpdated=true)", res, err)
		}
		assertExactANLZAndOneLib(t, root, extRel, twoExRel, onePath, 800, false, pureBlue, 1)

		if res2, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
			t.Fatalf("repeat WriteBackCues: res=%+v err=%v", res2, err)
		}
	})

	t.Run("hot_loop_move_and_recolour_same_palette_entry", func(t *testing.T) {
		extLoop := rekordbox.Cue{HotCue: 1, Name: "Loop 1", TimeMS: 800, EndMS: 2800, Loop: true, Color: rbBlue, ColorCode: 1}
		oneLoop := rekordbox.Cue{HotCue: 1, Name: "Loop 1", TimeMS: 800, EndMS: 2800, Loop: true, Color: rbBlue, ColorCode: 1}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extLoop, oneLoop, true)

		writeDeckPad1HotLoop(t, dbDir, rbOffset, 800, 2800, "Loop 1", rbBlue)
		settleFixture(t, root, dbDir, false)

		writeDeckPad1HotLoop(t, dbDir, rbOffset, 2400, 4400, "Loop 1", pureBlue)
		if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 1 {
			t.Fatalf("WriteBackCues: res=%+v err=%v", res, err)
		}
		assertExactANLZAndOneLib(t, root, extRel, twoExRel, onePath, 2400, true, pureBlue, 1)

		if res2, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
			t.Fatalf("repeat WriteBackCues: res=%+v err=%v", res2, err)
		}
	})
}
