package prolink

import (
	"testing"
	"time"
)

// Bug hunt 3: a tick read just before the deck was stored gave beat -1 (beat-in-bar 0 on
// the wire).
func TestExportDeckBeatIsNeverNegative(t *testing.T) {
	t0 := time.Now()
	deck := ExportDeck{Master: true, Playing: true, BPM: 120, Beat: 0.05, At: t0}
	beat, frac, ok := deck.beatAt(t0.Add(-100 * time.Millisecond))
	if !ok || beat != 0 || frac < 0 || frac >= 1 {
		t.Fatalf("a tick 100 ms older than the deck: beat %d frac %v ok %v, want beat 0 with a fraction in [0,1)", beat, frac, ok)
	}
	// Later ticks are unchanged: 120 BPM is two beats a second.
	if beat, _, ok := deck.beatAt(t0.Add(600 * time.Millisecond)); !ok || beat != 1 {
		t.Fatalf("600 ms later: beat %d ok %v, want beat 1", beat, ok)
	}
}

func TestExportDeckCarriedDoesNotGoBackInTime(t *testing.T) {
	t0 := time.Now()
	deck := ExportDeck{Master: true, Playing: true, BPM: 120, Beat: 0.05, At: t0}
	got := deck.Carried(t0.Add(-100 * time.Millisecond))
	if got.Beat != deck.Beat || !got.At.Equal(t0) {
		t.Fatalf("Carried with an earlier time changed the deck: beat %v at %v", got.Beat, got.At)
	}
	// Forward it still carries the beat on: one second at 120 BPM is two beats.
	fwd := deck.Carried(t0.Add(time.Second))
	if fwd.Beat < 2.04 || fwd.Beat > 2.06 || !fwd.At.Equal(t0.Add(time.Second)) {
		t.Fatalf("Carried one second on: beat %v at %v, want about 2.05", fwd.Beat, fwd.At)
	}
}
