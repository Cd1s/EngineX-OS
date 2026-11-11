package dmxstudio

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

// #209: a simulated Engine deck. beats(t) is its beat count; BeatNumber is
// that count within the bar (0..4), as the panel reads it.
type simDeck struct {
	bpm    func(t float64) float64
	offset float64 // beats added at t = 0
	nudges []struct{ at, beats float64 }
}

func (d simDeck) beats(t float64) float64 {
	// integrate the tempo in 1 ms steps (tempo changes are steps)
	b, step := d.offset, 0.001
	for x := 0.0; x < t; x += step {
		b += d.bpm(x) / 60 * math.Min(step, t-x)
	}
	for _, n := range d.nudges {
		if t >= n.at {
			b += n.beats
		}
	}
	return b
}

func barPhase(b float64) float64 { return math.Mod(math.Mod(b, barBeats)+barBeats, barBeats) }

// feeder plays the panel: one sample every 100 ms, read at t, arriving lag later.
type feeder struct {
	c      *beatClock
	manual float64
	next   float64
}

func (f *feeder) feed(now float64, deck int, playing bool, d simDeck, lag float64) {
	for f.next <= now-lag {
		t := f.next
		f.c.update(t, t+lag, f.manual, clockSample{deck: deck, playing: playing, beat: barPhase(d.beats(t)), bpm: d.bpm(t), position: math.NaN()})
		f.next += 0.1
	}
}

func TestClockTracksBeatWithinOneFrameThroughTempoChangeAndMasterHandoff(t *testing.T) {
	// Deck 1 plays 120 BPM, jumps to 126 BPM at 60 s. Deck 2 is beat-matched to
	// it (Sync) with 0.02 beat of phase error and takes MASTER at 90 s.
	tempo := func(t float64) float64 {
		if t < 60 {
			return 120
		}
		return 126
	}
	deck1 := simDeck{bpm: tempo, offset: 0.7}
	deck2 := simDeck{bpm: tempo, offset: 0.72}
	c := newBeatClock()
	fd := &feeder{c: &c, manual: 120, next: 0}
	worst := 0.0
	for now := 0.0; now < 120; now += 1.0 / 38 { // the deck's DMX frame rate
		lag := 0.03 + 0.05*math.Abs(math.Sin(now*7)) // QML GUI thread and HTTP lag
		deck, src := 1, deck1
		if now >= 90 {
			deck, src = 2, deck2
		}
		fd.feed(now, deck, true, src, lag)
		if now < 1 {
			continue // the first lock from the manual phase is slewed in
		}
		err := math.Abs(wrapBar(c.beats(now, 120) - src.beats(now)))
		frame := src.bpm(now) / 60 / 38
		worst = math.Max(worst, err/frame)
		if err > frame {
			t.Fatalf("t=%.3f: phase %.4f beats off, more than one DMX frame (%.4f)", now, err, frame)
		}
	}
	t.Logf("worst phase error: %.2f of a DMX frame", worst)
}

// A large phase change (MASTER to an unsynced deck, a seek or a big nudge) is
// slewed in: no frame jumps, and the clock locks within a few seconds.
func TestClockSlewsLargeErrorsWithoutAJump(t *testing.T) {
	for _, tc := range []struct {
		name string
		swap func(now float64) (int, simDeck)
	}{
		{"master to unsynced deck", func(now float64) (int, simDeck) {
			if now < 10 {
				return 1, simDeck{bpm: func(float64) float64 { return 124 }}
			}
			return 3, simDeck{bpm: func(float64) float64 { return 128 }, offset: 1.3}
		}},
		{"nudge of 0.3 beat", func(now float64) (int, simDeck) {
			return 1, simDeck{bpm: func(float64) float64 { return 124 }, nudges: []struct{ at, beats float64 }{{10, 0.3}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newBeatClock()
			fd := &feeder{c: &c, manual: 120}
			prev := math.NaN()
			for now := 0.0; now < 20; now += 1.0 / 38 {
				deck, src := tc.swap(now)
				fd.feed(now, deck, true, src, 0.04)
				b := c.beats(now, 120)
				if !math.IsNaN(prev) {
					step := b - prev
					tempo := src.bpm(now) / 60 / 38
					if step < 0 || step > tempo*1.5*1.01 {
						t.Fatalf("t=%.3f: beat step %.4f, normal frame %.4f: a visible jump", now, step, tempo)
					}
				}
				prev = b
				if now > 16 {
					if err := math.Abs(wrapBar(b - src.beats(now))); err > src.bpm(now)/60/38 {
						t.Fatalf("t=%.3f: not locked after the slew, %.4f beats off", now, err)
					}
				}
			}
		})
	}
}

func TestClockSmallNudgeIsTakenAtOnce(t *testing.T) {
	c := newBeatClock()
	c.update(0, 0, 120, clockSample{deck: 1, playing: true, beat: 0, bpm: 120, position: math.NaN()}) // locked: the manual phase is 0 too
	c.update(0.1, 0.1, 120, clockSample{deck: 1, playing: true, beat: 0.2 + 0.015, bpm: 120, position: math.NaN()})
	if got := c.beats(0.1, 120); math.Abs(barPhase(got)-0.215) > 1e-9 {
		t.Fatalf("a nudge below one frame was not taken at once: %.4f", barPhase(got))
	}
}

func TestClockPauseHoldsAndResumesWithoutAJump(t *testing.T) {
	c := newBeatClock()
	c.update(0, 0, 120, clockSample{deck: 1, playing: true, beat: 0, bpm: 120, position: 1000})
	c.update(1, 1, 120, clockSample{deck: 1, playing: false, beat: 2, bpm: 120, position: 1000}) // 2 beats later
	held := c.beats(1, 120)
	for now := 1.0; now <= 1.9; now += 0.1 { // the panel keeps feeding the paused master
		c.update(now, now, 120, clockSample{deck: 1, playing: false, beat: 2, bpm: 120, position: 1000})
		if b := c.beats(now+0.05, 120); math.Abs(b-held) > 1e-9 {
			t.Fatalf("paused master: beat count moved %.4f -> %.4f", held, b)
		}
	}
	c.update(2, 2, 120, clockSample{deck: 1, playing: true, beat: 2, bpm: 120, position: 1000})
	if b := c.beats(2.5, 120); math.Abs(b-(held+1)) > 1e-9 {
		t.Fatalf("resume: %.4f, want %.4f", b, held+1)
	}
	// CUE held: PlayState is false but the position moves, so the beat advances.
	c.update(3, 3, 120, clockSample{deck: 1, playing: false, beat: 0, bpm: 120, position: 1000})
	c.update(3.1, 3.1, 120, clockSample{deck: 1, playing: false, beat: 0.2, bpm: 120, position: 5410})
	if !c.playing {
		t.Fatal("a moving position with PlayState false (CUE held) must count as playing")
	}
}

func TestClockFallsBackToManualBPMWithoutAJump(t *testing.T) {
	c := newBeatClock()
	if got := c.beats(10, 120); got != 20 {
		t.Fatalf("no deck clock: beats must be the old wall-clock phase, got %v", got)
	}
	c.update(10, 10, 120, clockSample{deck: 1, playing: true, beat: 0, bpm: 128, position: math.NaN()})
	before := c.beats(10+clockStale, 120)
	after := c.beats(10+clockStale+1.0/30, 120)
	if c.deck != 0 {
		t.Fatal("a deck clock without updates for clockStale must hand back to the manual BPM")
	}
	if step := after - before; step < 0 || step > 128.0/60/30*1.01 {
		t.Fatalf("stale hand-over jumped by %.4f beats", step)
	}
	if b := c.beats(20, 120) - c.beats(19, 120); math.Abs(b-2) > 1e-9 {
		t.Fatalf("manual after hand-over runs at the show BPM: %.4f beats/s", b)
	}
	// deck 0 (no deck playing) hands over at once.
	c.update(30, 30, 120, clockSample{deck: 2, playing: true, beat: 1, bpm: 100, position: math.NaN()})
	x := c.beats(30.5, 120)
	c.update(30.5, 30.5, 120, clockSample{deck: 0})
	if c.deck != 0 || math.Abs(c.beats(30.5, 120)-x) > 1e-9 {
		t.Fatal("deck 0 must switch to the manual BPM from the current phase")
	}
}

func TestClockOutOfOrderSampleIsIgnored(t *testing.T) {
	c := newBeatClock()
	c.update(1, 1, 120, clockSample{deck: 1, playing: true, beat: 1, bpm: 120, position: math.NaN()})
	b := c.beats(1.2, 120)
	c.update(0.9, 1.2, 120, clockSample{deck: 1, playing: true, beat: 3, bpm: 120, position: math.NaN()})
	if c.beats(1.2, 120) != b {
		t.Fatal("an older sample must not move the clock")
	}
}

func snapshot(t *testing.T, dir string) string {
	t.Helper()
	out := ""
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		out += fmt.Sprintf("%s %d %d\n", p, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return out
}

func action(t *testing.T, f *Feature, body string) (any, error) {
	t.Helper()
	req, err := modkit.Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return f.Action(req)
}

// #209 done-when: no writes to /data while the clock runs.
func TestClockActionNeverWritesState(t *testing.T) {
	dir := t.TempDir()
	f, err := NewFeature(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := action(t, f, fmt.Sprintf(`{"action":"clock","deck":%d,"playing":true,"beat":%v,"bpm":124.5,"position":%d,"at":%d}`,
			1+i%2, float64(i%40)/10, i*4410, int64(1700000000000)+int64(i)*100)); err != nil {
			t.Fatal(err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("clock wrote to the state directory: %v", entries)
	}
	if _, err := action(t, f, `{"action":"configure","config":{"bpm":128}}`); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	for i := 0; i < 200; i++ {
		v, err := action(t, f, fmt.Sprintf(`{"action":"clock","deck":3,"playing":%v,"beat":"%v","bpm":128}`, i%3 != 0, float64(i%40)/10))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := v.(map[string]any)["frames"]; ok {
			t.Fatal("the clock reply must stay small (no frames)")
		}
	}
	if _, err := action(t, f, `{"action":"clock","deck":0}`); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); after != before {
		t.Fatalf("clock changed the state directory:\n%s\n%s", before, after)
	}
	st, _ := f.Status()
	if st.(map[string]any)["config"].(map[string]any)["bpm"] != 128.0 {
		t.Fatal("the clock must not change the saved show BPM")
	}
	if _, ok := st.(map[string]any)["clock"]; !ok {
		t.Fatal("status reports the clock")
	}
}

func TestClockActionRejectsBadInput(t *testing.T) {
	f, err := NewFeature(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"action":"clock"}`,
		`{"action":"clock","deck":5}`,
		`{"action":"clock","deck":1.5,"playing":true,"beat":1,"bpm":120}`,
		`{"action":"clock","deck":1,"beat":1,"bpm":120}`,
		`{"action":"clock","deck":1,"playing":"yes","beat":1,"bpm":120}`,
		`{"action":"clock","deck":1,"playing":true,"bpm":120}`,
		`{"action":"clock","deck":1,"playing":true,"beat":"NaN","bpm":120}`,
		`{"action":"clock","deck":1,"playing":true,"beat":1,"bpm":1000}`,
		`{"action":"clock","deck":1,"playing":true,"beat":1,"bpm":120,"at":"soon"}`,
	} {
		if _, err := action(t, f, body); err == nil || !modkit.IsBad(err) {
			t.Fatalf("%s: want a 400, got %v", body, err)
		}
	}
	if f.clock.deck != 0 {
		t.Fatal("rejected input must not engage the clock")
	}
}

// The render loop's phase follows the clock; without one it is unchanged.
func TestRenderFollowsTheClock(t *testing.T) {
	f, err := NewFeature(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.Config["outputs"] = []any{map[string]any{"protocol": "artnet", "universe": 1.0, "host": "127.0.0.1"}}
	f.Config["patch"] = []any{map[string]any{"profile": "Moving Head Beam", "address": 1.0, "universe": 1.0}}
	f.Config["effect"], f.Config["speed"] = "Circle", 4.0
	pan := func(fr map[int][]byte) int { return int(fr[1][0])<<8 | int(fr[1][1]) }
	expect := func(beats float64) int {
		return int(math.Max(0, math.Min(65535, 32768+math.Sin(beats/4*2*math.Pi)*16000)))
	}
	if got, want := pan(f.Render(3.3)), expect(3.3*120/60); got != want {
		t.Fatalf("manual: pan %d, want %d", got, want)
	}
	f.clock.update(0, 0, 120, clockSample{deck: 1, playing: true, beat: 1, bpm: 120, position: math.NaN()})
	// The deck is 1 beat ahead of the manual phase: the render follows the clock's
	// count (slewing towards the deck), no longer the wall clock alone.
	b := f.clock.beats(0.5, 120)
	if got, want := pan(f.Render(0.5)), expect(b); got != want || b == 0.5*120/60 {
		t.Fatalf("deck clock: pan %d, want %d (beats %.3f)", got, want, b)
	}
}
