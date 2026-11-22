package mapping

import (
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func on(at, note int) Event  { return Event{At: ms(at), Note: note, On: true} }
func off(at, note int) Event { return Event{At: ms(at), Note: note} }

// run feeds recorded events and ticks the clock at the end times, as the learn loop does
// every 20 ms: it returns the first result and when it came.
func run(t *testing.T, board string, events []Event, ticks ...int) (*Result, time.Duration) {
	t.Helper()
	l, err := NewLearner(board)
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		at time.Duration
		ev *Event
	}
	var steps []step
	for i := range events {
		steps = append(steps, step{events[i].At, &events[i]})
	}
	for _, tk := range ticks {
		steps = append(steps, step{ms(tk), nil})
	}
	// events and ticks in time order, an event first on a tie
	for i := range steps {
		for j := i + 1; j < len(steps); j++ {
			if steps[j].at < steps[i].at || steps[j].at == steps[i].at && steps[j].ev != nil && steps[i].ev == nil {
				steps[i], steps[j] = steps[j], steps[i]
			}
		}
	}
	for _, s := range steps {
		var r *Result
		if s.ev != nil {
			r = l.Feed(*s.ev)
		} else {
			r = l.Tick(s.at)
		}
		if r != nil {
			return r, s.at
		}
	}
	return nil, 0
}

const (
	noteShift   = 26
	noteHotCue  = 27
	noteLoop    = 30
	noteRoll    = 28
	noteBack    = 16
	noteSync    = 20
	noteUnknown = 99
)

func TestLearnPress(t *testing.T) {
	r, at := run(t, "JP07", []Event{on(0, noteLoop), off(80, noteLoop)}, 100, 200, 299, 300, 320)
	if r == nil || r.Gesture != GesturePress || r.Control != "PadModeSelect:30" || r.Note != noteLoop {
		t.Fatalf("%+v", r)
	}
	if at != ms(300) {
		t.Errorf("a press is known when the 300 ms double window has passed, not at %v", at)
	}
	if r.Offer.Mappable || r.Offer.Reason != ReasonStockUsed {
		t.Errorf("a press on a pad-mode button is stock's: %+v", r.Offer)
	}
}

func TestLearnDoubleWithinTheWindow(t *testing.T) {
	r, at := run(t, "JP07", []Event{on(0, noteLoop), off(60, noteLoop), on(180, noteLoop), off(240, noteLoop)}, 100)
	if r == nil || r.Gesture != GestureDouble || at != ms(180) {
		t.Fatalf("%+v at %v", r, at)
	}
	if !r.Offer.Mappable {
		t.Errorf("a double on a pad-mode button can be mapped: %+v", r.Offer)
	}
	// the second press arrives with the window already gone: the first was a press
	r, _ = run(t, "JP07", []Event{on(0, noteLoop), off(50, noteLoop), on(310, noteLoop)})
	if r == nil || r.Gesture != GesturePress {
		t.Fatalf("a second press at 310 ms: %+v", r)
	}
	r, _ = run(t, "JP07", []Event{on(0, noteLoop), off(50, noteLoop), on(299, noteLoop)})
	if r == nil || r.Gesture != GestureDouble {
		t.Fatalf("a second press at 299 ms: %+v", r)
	}
}

func TestLearnHoldAtFiveHundredMilliseconds(t *testing.T) {
	r, at := run(t, "JP07", []Event{on(0, noteHotCue)}, 100, 300, 499, 500, 520)
	if r == nil || r.Gesture != GestureHold || at != ms(500) || r.Control != "PadModeSelect:27" {
		t.Fatalf("%+v at %v", r, at)
	}
	if !r.Offer.Mappable {
		t.Errorf("a hold on a pad-mode button can be mapped: %+v", r.Offer)
	}
	// released at 499 ms: a press, not a hold
	r, _ = run(t, "JP07", []Event{on(0, noteHotCue), off(499, noteHotCue)}, 300, 600)
	if r == nil || r.Gesture != GesturePress {
		t.Fatalf("released at 499 ms: %+v", r)
	}
	// released between the double window and the hold time: still a press
	r, _ = run(t, "JP07", []Event{on(0, noteHotCue), off(420, noteHotCue)}, 300, 450)
	if r == nil || r.Gesture != GesturePress {
		t.Fatalf("released at 420 ms: %+v", r)
	}
}

func TestLearnShiftCombination(t *testing.T) {
	r, at := run(t, "JP07", []Event{on(0, noteShift), on(150, noteRoll), off(200, noteRoll)})
	if r == nil || r.Gesture != GestureShift || at != ms(150) || r.Control != "PadModeSelect:28" {
		t.Fatalf("%+v at %v", r, at)
	}
	if !r.Offer.Mappable {
		t.Errorf("%+v", r.Offer)
	}
	// Shift let go before the button: an ordinary press
	r, _ = run(t, "JP07", []Event{on(0, noteShift), off(100, noteShift), on(200, noteRoll), off(250, noteRoll)}, 600)
	if r == nil || r.Gesture != GesturePress {
		t.Fatalf("%+v", r)
	}
	// Shift pressed after the button went down does not turn the press into a shift gesture
	r, _ = run(t, "JP07", []Event{on(0, noteRoll), on(50, noteShift), off(80, noteRoll)}, 400)
	if r == nil || r.Gesture != GesturePress {
		t.Fatalf("%+v", r)
	}
}

// Shift + LOOP is caught like any gesture and reported as not mappable: EngineX's Beat Jump pad mode owns it.
func TestLearnReportsShiftLoopAsTakenByTheBeatJumpPadMode(t *testing.T) {
	r, _ := run(t, "JP13", []Event{on(0, noteShift), on(120, noteLoop), off(160, noteLoop)})
	if r == nil || r.Gesture != GestureShift || r.Control != "PadModeSelect:30" {
		t.Fatalf("%+v", r)
	}
	if r.Offer.Mappable || r.Offer.Reason != ReasonTaken || r.Offer.Status != StatusTaken {
		t.Fatalf("%+v", r.Offer)
	}
}

func TestLearnShiftAloneIsNoGesture(t *testing.T) {
	if r, _ := run(t, "JP07", []Event{on(0, noteShift), off(900, noteShift)}, 400, 800, 2000); r != nil {
		t.Fatalf("Shift is a modifier: %+v", r)
	}
}

func TestLearnNamesTheControlAndWhatCannotBeMapped(t *testing.T) {
	r, _ := run(t, "JP13", []Event{on(0, noteBack), off(40, noteBack)}, 300)
	if r == nil || r.Control != "Back:16" || r.Offer.Mappable || r.Offer.Reason != ReasonReserved {
		t.Fatalf("a navigation control is reserved: %+v", r)
	}
	r, _ = run(t, "JP07", []Event{on(0, noteUnknown), off(40, noteUnknown)}, 300)
	if r == nil || r.Control != "" || r.Note != noteUnknown || r.Offer.Reason != ReasonControl {
		t.Fatalf("a note the board's inventory does not know: %+v", r)
	}
	r, _ = run(t, "JP07", []Event{on(0, noteSync), on(900, noteSync)}, 600)
	if r == nil || r.Control != "Sync:20" || r.Gesture != GestureHold {
		t.Fatalf("%+v", r)
	}
	if !r.Offer.Mappable || r.Offer.Status != StatusTakeover {
		t.Errorf("stock's Sync hold can be taken: %+v", r.Offer)
	}
}

func TestLearnFollowsTheFirstControlOnly(t *testing.T) {
	r, _ := run(t, "JP07", []Event{on(0, noteLoop), on(40, noteHotCue), off(60, noteHotCue), off(90, noteLoop)}, 300)
	if r == nil || r.Control != "PadModeSelect:30" || r.Gesture != GesturePress {
		t.Fatalf("%+v", r)
	}
}

func TestLearnIsSingleShot(t *testing.T) {
	l, _ := NewLearner("JP07")
	l.Feed(on(0, noteShift))
	if r := l.Feed(on(10, noteLoop)); r == nil {
		t.Fatal("no result")
	}
	if r := l.Feed(on(20, noteHotCue)); r != nil {
		t.Fatalf("a second result: %+v", r)
	}
	if r := l.Tick(ms(1000)); r != nil {
		t.Fatalf("a result from a tick after the first: %+v", r)
	}
}

func TestLearnNeedsASupportedBoard(t *testing.T) {
	if _, err := NewLearner("JC11"); err == nil {
		t.Fatal("JC11 is not a v1 board")
	}
}
