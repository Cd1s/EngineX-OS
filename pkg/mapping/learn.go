package mapping

import (
	"fmt"
	"time"
)

// Event is one note message of the controller: a note-on with a velocity, or a note-off
// (a note-on with velocity 0 is a note-off). At is time since learning began.
type Event struct {
	At      time.Duration
	Channel int
	Note    int
	On      bool
}

// Result is the one gesture MIDI learn caught.
type Result struct {
	Control    string   `json:"control"`              // the inventory id, empty if the board's inventory has no such button
	Candidates []string `json:"candidates,omitempty"` // set when more than one control listens to the note
	Note       int      `json:"note"`
	Channel    int      `json:"channel"`
	Gesture    string   `json:"gesture"`
	Offer      Offer    `json:"offer"`
}

// Learner turns the controller's note events into one gesture, the way stock's
// ComboActionOutput reads them: Shift held when the button goes down is a shift gesture;
// a second press within doubleMs of the first is a double; a button still down after
// holdMs is a hold; a button released with no second press within doubleMs is a press.
// It is a pure state machine: the caller supplies the clock.
type Learner struct {
	board     string
	inv       *Inventory
	cat       *Catalogue
	shiftNote int
	hold      time.Duration
	double    time.Duration

	shift   bool
	pending *tap
	done    bool
}

type tap struct {
	note, channel int
	first         time.Duration
	down          bool
}

// NewLearner starts a learner for one of the player boards.
func NewLearner(board string) (*Learner, error) {
	cat := Default()
	inv, ok := Board(board)
	if !ok || !cat.Supports(board) {
		return nil, fmt.Errorf("button mapping supports JP07, JP08, JP13 and JP14, not %q", board)
	}
	shift, ok := inv.ShiftNote()
	if !ok {
		return nil, fmt.Errorf("%s has no Shift button in its inventory", board)
	}
	return &Learner{
		board: board, inv: inv, cat: cat, shiftNote: shift,
		hold:   time.Duration(cat.Timing.HoldMS) * time.Millisecond,
		double: time.Duration(cat.Timing.DoubleMS) * time.Millisecond,
	}, nil
}

// Feed takes the next event. It returns the gesture once there is one.
func (l *Learner) Feed(e Event) *Result {
	if l.done {
		return nil
	}
	if e.Note == l.shiftNote {
		l.shift = e.On
		return nil
	}
	// A pending press may have resolved before this event arrived.
	if r := l.Tick(e.At); r != nil {
		return r
	}
	switch {
	case !e.On:
		if l.pending != nil && l.pending.note == e.Note {
			l.pending.down = false
		}
	case l.pending == nil:
		if l.shift {
			return l.finish(e.Note, e.Channel, GestureShift)
		}
		l.pending = &tap{note: e.Note, channel: e.Channel, first: e.At, down: true}
	case l.pending.note == e.Note && e.At-l.pending.first < l.double:
		return l.finish(e.Note, e.Channel, GestureDouble)
	}
	return l.Tick(e.At)
}

// Tick lets time pass: a button held long enough is a hold, one released and not pressed
// again in the window is a press.
func (l *Learner) Tick(now time.Duration) *Result {
	if l.done || l.pending == nil {
		return nil
	}
	p := l.pending
	switch {
	case p.down && now-p.first >= l.hold:
		return l.finish(p.note, p.channel, GestureHold)
	case !p.down && now-p.first >= l.double:
		return l.finish(p.note, p.channel, GesturePress)
	}
	return nil
}

func (l *Learner) finish(note, channel int, gesture string) *Result {
	l.done = true
	l.pending = nil
	r := &Result{Note: note, Channel: channel, Gesture: gesture}
	owners := l.inv.ByNote(note)
	for _, c := range owners {
		r.Candidates = append(r.Candidates, c.ID)
	}
	if len(owners) == 1 {
		r.Control = owners[0].ID
		r.Candidates = nil
		r.Offer = l.cat.Offer(l.board, r.Control, gesture)
	} else {
		r.Offer = Offer{Status: StatusStock, Reason: ReasonControl}
	}
	return r
}
