package dmxstudio

import (
	"math"
	"time"
)

const midiPPQN = 24.0

type midiEventKind uint8

const (
	midiStart midiEventKind = iota + 1
	midiContinue
	midiStop
	midiClock
)

type midiSink interface {
	Send(kind midiEventKind) error
	Close() error
}

// midiClockTracker converts continuous beat counts into 24 PPQN MIDI real-time
// events without bursts on seeks or jumps.
type midiClockTracker struct {
	started     bool
	wasPlaying  bool
	lastTick    int64
	lastBeat    float64
	initialized bool
}

func (m *midiClockTracker) step(playing bool, beats float64) []midiEventKind {
	tick := int64(math.Floor(beats * midiPPQN))
	if !m.initialized {
		m.initialized = true
		m.lastTick = tick
		m.lastBeat = beats
		if playing {
			m.wasPlaying = true
			m.started = true
			return []midiEventKind{midiStart}
		}
		return nil
	}

	var events []midiEventKind
	if playing && !m.wasPlaying {
		m.wasPlaying = true
		if !m.started || math.Abs(beats-m.lastBeat) > 0.5 && math.Mod(math.Abs(beats), barBeats) < 0.1 {
			m.started = true
			events = append(events, midiStart)
		} else {
			events = append(events, midiContinue)
		}
		m.lastTick = tick
		m.lastBeat = beats
		return events
	}
	if !playing && m.wasPlaying {
		m.wasPlaying = false
		m.lastTick = tick
		m.lastBeat = beats
		return []midiEventKind{midiStop}
	}
	if !playing {
		m.lastTick = tick
		m.lastBeat = beats
		return nil
	}

	delta := tick - m.lastTick
	switch {
	case delta > 0 && delta <= 6:
		for i := int64(0); i < delta; i++ {
			events = append(events, midiClock)
		}
	case delta != 0:
		// Large seek or backward loop: re-sync without flooding downstream MIDI gear.
		if delta > 6 {
			events = append(events, midiClock)
		}
	}
	m.lastTick = tick
	m.lastBeat = beats
	return events
}

// syncMIDIClock starts or stops the MIDI clock goroutine to match Config["midiClock"].
// Lock order: f.midiMu -> f.mu (never called with f.mu held).
func (f *Feature) syncMIDIClock(forceStop bool) {
	f.midiMu.Lock()
	defer f.midiMu.Unlock()

	f.mu.Lock()
	enabled, _ := f.Config["midiClock"].(bool)
	want := enabled && f.looping && !forceStop
	if !want {
		f.midiErr = ""
	}
	f.mu.Unlock()

	if want {
		if f.midiRunning {
			return
		}
		f.midiStop = make(chan struct{})
		f.midiDone = make(chan struct{})
		f.midiRunning = true
		go f.midiClockLoop(f.midiStop, f.midiDone)
		return
	}

	if !f.midiRunning {
		return
	}
	close(f.midiStop)
	doneCh := f.midiDone
	f.midiRunning = false
	select {
	case <-doneCh:
	case <-time.After(time.Second):
	}
}

func (f *Feature) midiClockLoop(stopCh <-chan struct{}, doneCh chan<- struct{}) {
	defer close(doneCh)
	var sink midiSink
	var tracker midiClockTracker
	defer func() {
		if sink != nil {
			if tracker.wasPlaying {
				_ = sink.Send(midiStop)
			}
			_ = sink.Close()
		}
	}()

	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-f.stop:
			return
		case <-tick.C:
		}
		select {
		case <-stopCh:
			return
		case <-f.stop:
			return
		default:
		}

		f.mu.Lock()
		enabled, _ := f.Config["midiClock"].(bool)
		if !enabled {
			f.midiErr = ""
			f.mu.Unlock()
			return
		}
		now := f.now()
		manual := f.Config["bpm"].(float64)
		beats := f.clock.beats(now, manual)
		playing := f.clock.deck != 0 && f.clock.playing
		openFn := f.openMIDISink
		f.mu.Unlock()

		if sink == nil && openFn != nil {
			s, err := openFn()
			if err != nil {
				f.mu.Lock()
				f.midiErr = err.Error()
				f.mu.Unlock()
				select {
				case <-stopCh:
					return
				case <-f.stop:
					return
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			sink = s
			f.mu.Lock()
			f.midiErr = ""
			f.mu.Unlock()
		}
		if sink == nil {
			continue
		}
		events := tracker.step(playing, beats)
		for _, ev := range events {
			if err := sink.Send(ev); err != nil {
				_ = sink.Close()
				sink = nil
				f.mu.Lock()
				f.midiErr = err.Error()
				f.mu.Unlock()
				break
			}
		}
	}
}
