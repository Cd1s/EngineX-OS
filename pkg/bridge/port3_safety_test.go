package bridge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestP3MergeEngineCuesPreservesUncolouredImportedCues(t *testing.T) {
	deck := enginedb.DeckCues{SampleRate: 1000, SampleCount: 10000}
	deck.HotCues[0] = &enginedb.DeckHotCue{Samples: 1000, Color: [4]byte{255, 0, 255, 0}}
	deck.Loops[1] = &enginedb.DeckLoop{StartSamples: 2000, EndSamples: 3000, Color: [4]byte{255, 255, 127, 0}}
	got := mergeEngineCues([]rekordbox.Cue{{HotCue: 1, TimeMS: 1000}, {HotCue: 0, TimeMS: 2000, EndMS: 3000, Loop: true}}, deck, 0, false)
	for _, cue := range got {
		if cue.HotCue == 1 && (cue.Color != [4]byte{} || cue.ColorCode != 0) {
			t.Fatalf("uncoloured hot cue gained color: %+v", cue)
		}
		if cue.HotCue == 0 && cue.Loop && (cue.Color != [4]byte{} || cue.ColorCode != 0 || cue.ColorID != 0) {
			t.Fatalf("uncoloured memory loop gained color: %+v", cue)
		}
	}
}

func TestP3MergeEngineCuesClampAtZeroPreserved(t *testing.T) {
	orig := []rekordbox.Cue{{HotCue: 1, TimeMS: 7, Extended: true}, {HotCue: 2, TimeMS: 10, EndMS: 2000, Loop: true, Extended: true}}
	deck := enginedb.DeckCues{SampleRate: 44100, SampleCount: 44100 * 60}
	deck.HotCues[0] = &enginedb.DeckHotCue{Samples: 0, Color: [4]byte{255, 0, 255, 0}}
	deck.HotCues[1] = &enginedb.DeckHotCue{Samples: 0, Color: [4]byte{255, 255, 127, 0}}
	deck.Loops[1] = &enginedb.DeckLoop{StartSamples: 0, EndSamples: 2*44100 - 1105, Color: [4]byte{255, 255, 127, 0}}
	got := mergeEngineCues(orig, deck, 1105, false)
	if !cuesEquivalent(orig, got) {
		t.Fatalf("clamp-at-zero cues drifted: orig=%+v got=%+v", orig, got)
	}
}

func TestP3MergeEngineCuesPreservesLegacyMP3OffsetDrift(t *testing.T) {
	red := [4]byte{255, 255, 0, 23}
	orig := []rekordbox.Cue{{HotCue: 1, TimeMS: 116089, Color: red, ColorCode: 1, Extended: true}}
	deck := enginedb.DeckCues{SampleRate: 44100, SampleCount: 44100 * 300}
	deck.HotCues[0] = &enginedb.DeckHotCue{Samples: 5117232.9, Name: "", Color: red}
	if got := mergeEngineCuesOpts(orig, deck, 1105, true); !cuesEquivalent(orig, got) {
		t.Fatalf("legacy unnamed offset drift changed cue: orig=%+v got=%+v", orig, got)
	}
	deck.HotCues[0].Name = "Cue 1"
	if got := mergeEngineCuesOpts(orig, deck, 1105, true); cuesEquivalent(orig, got) {
		t.Fatalf("deck-named Cue 1 was suppressed: %+v", got)
	}
}

func TestP3TrackRekordboxOffsetRejectsUnreadableAudio(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Contents", "bad.mp3"), []byte("ID3\\x04\\x00not-valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Contents", "truncated.m4a"), []byte("\\x00\\x00\\x00\\x14ftypM4A "), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if off, ok := trackRekordboxOffset(r, "/Contents/bad.mp3"); ok || off != 0 {
		t.Fatalf("corrupt MP3 returned ok=%v off=%v", ok, off)
	}
	if off, ok := trackRekordboxOffset(r, "/Contents/truncated.m4a"); ok || off != 0 {
		t.Fatalf("truncated M4A returned ok=%v off=%v", ok, off)
	}
}
