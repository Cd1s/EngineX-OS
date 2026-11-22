package mapping

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSource plays a script: each Next waits its step, then hands over that step's events.
type fakeSource struct {
	ports      []Port
	script     []fakeStep
	subscribed []Port
	closed     int
	subErr     error
}

type fakeStep struct {
	after  time.Duration
	events []Event
}

func (f *fakeSource) Ports() ([]Port, error) { return f.ports, nil }
func (f *fakeSource) Subscribe(p Port) error {
	if f.subErr != nil {
		return f.subErr
	}
	f.subscribed = append(f.subscribed, p)
	return nil
}
func (f *fakeSource) Next(wait time.Duration) ([]Event, error) {
	if len(f.script) == 0 {
		time.Sleep(wait)
		return nil, nil
	}
	step := f.script[0]
	if step.after > wait {
		f.script[0].after -= wait
		time.Sleep(wait)
		return nil, nil
	}
	f.script = f.script[1:]
	time.Sleep(step.after)
	return step.events, nil
}
func (f *fakeSource) Close() error { f.closed++; return nil }

var sc5000Ports = []Port{
	{Client: 14, Port: 0, ClientName: "Midi Through", PortName: "Midi Through Port-0"},
	{Client: 20, Port: 0, ClientName: "SC5000 Prime Controller", PortName: "Jack 1"},
	{Client: 28, Port: 0, ClientName: "EngineX-MIDI-Mapper", PortName: "MIDI Mapper Input"},
}

func TestPickPortChoosesTheBoardsController(t *testing.T) {
	p, err := PickPort("JP07", sc5000Ports)
	if err != nil || p.Client != 20 {
		t.Fatalf("%+v %v", p, err)
	}
	two := []Port{
		{Client: 20, ClientName: "SC5000M Prime Controller"},
		{Client: 21, ClientName: "SC5000 Prime Controller"},
	}
	if p, err := PickPort("JP07", two); err != nil || p.Client != 21 {
		t.Fatalf("SC5000 must not take the SC5000M's port: %+v %v", p, err)
	}
	if p, err := PickPort("JP08", two); err != nil || p.Client != 20 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := PickPort("JP13", []Port{{Client: 14, ClientName: "Midi Through"}}); !errors.Is(err, ErrNoController) {
		t.Fatalf("%v", err)
	}
	if _, err := PickPort("JP13", []Port{{Client: 20, ClientName: "Foo Controller"}, {Client: 21, ClientName: "Bar Controller"}}); err == nil {
		t.Fatal("two unnamed controllers must not be guessed between")
	}
}

func TestLearnSubscribesReadsOneGestureAndClosesTheSource(t *testing.T) {
	src := &fakeSource{ports: sc5000Ports, script: []fakeStep{
		{after: 10 * time.Millisecond, events: []Event{{Note: noteShift, On: true}}},
		{after: 30 * time.Millisecond, events: []Event{{Note: noteRoll, On: true}}},
	}}
	r, err := Learn(context.Background(), src, "JP07", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if r.Gesture != GestureShift || r.Control != "PadModeSelect:28" || !r.Offer.Mappable {
		t.Fatalf("%+v", r)
	}
	if len(src.subscribed) != 1 || src.subscribed[0].Client != 20 {
		t.Fatalf("subscribed to %+v: only the board's controller port, once", src.subscribed)
	}
	if src.closed != 1 {
		t.Fatalf("closed %d times", src.closed)
	}
}

func TestLearnClassifiesHoldFromTheClock(t *testing.T) {
	src := &fakeSource{ports: sc5000Ports, script: []fakeStep{{after: 5 * time.Millisecond, events: []Event{{Note: noteHotCue, On: true}}}}}
	start := time.Now()
	r, err := Learn(context.Background(), src, "JP07", 3*time.Second)
	if err != nil || r.Gesture != GestureHold {
		t.Fatalf("%+v %v", r, err)
	}
	if took := time.Since(start); took < 480*time.Millisecond || took > 900*time.Millisecond {
		t.Errorf("a hold is caught at about 500 ms, took %v", took)
	}
}

func TestLearnGivesUpAfterTheWindowAndStillCloses(t *testing.T) {
	src := &fakeSource{ports: sc5000Ports}
	start := time.Now()
	if _, err := Learn(context.Background(), src, "JP07", 150*time.Millisecond); !errors.Is(err, ErrLearnTimeout) {
		t.Fatalf("%v", err)
	}
	if took := time.Since(start); took > 400*time.Millisecond {
		t.Errorf("took %v for a 150 ms window", took)
	}
	if src.closed != 1 {
		t.Fatalf("closed %d times", src.closed)
	}
}

func TestLearnStopsWhenCancelled(t *testing.T) {
	src := &fakeSource{ports: sc5000Ports}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := Learn(ctx, src, "JP07", 10*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if src.closed != 1 {
		t.Fatalf("closed %d times", src.closed)
	}
}

func TestLearnWithoutAControllerSubscribesToNothing(t *testing.T) {
	src := &fakeSource{ports: []Port{{Client: 14, ClientName: "Midi Through"}}}
	if _, err := Learn(context.Background(), src, "JP07", time.Second); !errors.Is(err, ErrNoController) {
		t.Fatalf("%v", err)
	}
	if len(src.subscribed) != 0 || src.closed != 1 {
		t.Fatalf("subscribed %v, closed %d", src.subscribed, src.closed)
	}
	src = &fakeSource{ports: sc5000Ports, subErr: errors.New("busy")}
	if _, err := Learn(context.Background(), src, "JP07", time.Second); err == nil || src.closed != 1 {
		t.Fatalf("%v closed %d", err, src.closed)
	}
	if _, err := Learn(context.Background(), &fakeSource{ports: sc5000Ports}, "JC11", time.Second); err == nil {
		t.Fatal("JC11 is not a v1 board")
	}
}
