package webui

import (
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

// cdjLine is a synthetic CDJ-3000 on the wire for #217: its packets are built
// as bytes (status 0x0a, 0x200 long, every 200 ms; absolute position 0x0b every
// 30 ms), parsed by prolink.ParsePacket and given to the server the way the
// export's Observe does, on a clock the test moves.
type cdjLine struct {
	t      *testing.T
	s      *Server
	now    time.Time
	number byte
	track  uint32
}

func newCDJLine(t *testing.T, track uint32) *cdjLine {
	t.Helper()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	return &cdjLine{t: t, s: s, now: time.Now(), number: 2, track: track}
}

func (c *cdjLine) feed(b []byte, port int) {
	c.t.Helper()
	d, err := prolink.ParsePacket(b, port, net.IPv4(192, 0, 2, 30))
	if err != nil {
		c.t.Fatal(err)
	}
	d.LastSeen = c.now
	c.s.shadow.observeAt(d, c.now)
	prolinkMu.Lock()
	defer prolinkMu.Unlock()
	if postedDevices[c.s] == nil {
		postedDevices[c.s] = map[string]prolink.Device{}
	}
	postedDevices[c.s][deviceKey(d)] = mergeDevice(postedDevices[c.s][deviceKey(d)], d)
}

func (c *cdjLine) packet(kind byte, size int) []byte {
	b := make([]byte, size)
	copy(b, "Qspt1WmJOL")
	b[10] = kind
	copy(b[11:], "CDJ-3000")
	b[0x1f], b[0x21] = 1, c.number
	return b
}

// status: p1 is the play mode (3 playing, 5 paused, 6 paused at the cue point,
// 7 CUE held), play the flag bit; beat is the beat the deck is in at 120 BPM.
func (c *cdjLine) status(p1 byte, play bool, beat uint32) {
	b := c.packet(0x0a, 0x200)
	b[0x28], b[0x29] = c.number, 3
	binary.BigEndian.PutUint32(b[0x2c:], c.track)
	b[0x7b], b[0x89] = p1, 0x84
	if play {
		b[0x89] |= 0x40
	}
	binary.BigEndian.PutUint32(b[0x8c:], 0x100000)
	binary.BigEndian.PutUint16(b[0x92:], 12000)
	binary.BigEndian.PutUint32(b[0xa0:], beat)
	c.feed(b, 50002)
}

func (c *cdjLine) position(ms float64) {
	b := c.packet(0x0b, 0x3c)
	binary.BigEndian.PutUint32(b[0x24:], 300)
	binary.BigEndian.PutUint32(b[0x28:], uint32(math.Round(ms)))
	binary.BigEndian.PutUint32(b[0x38:], 1200)
	c.feed(b, 50001)
}

// run moves the deck from ms for d at speed (1 is the track's own): a position
// packet every 30 ms, and with status a status packet every 200 ms saying p1/play.
// poll, when set, is asked every 40 ms.
func (c *cdjLine) run(ms float64, d time.Duration, speed float64, status bool, p1 byte, play bool, poll func(at float64)) float64 {
	for t := time.Duration(0); t < d; t += 10 * time.Millisecond {
		if t%(30*time.Millisecond) == 0 {
			c.position(ms)
		}
		if status && t%(200*time.Millisecond) == 0 {
			c.status(p1, play, uint32(ms/500)+1)
		}
		if poll != nil && t%(40*time.Millisecond) == 0 {
			poll(ms)
		}
		c.now = c.now.Add(10 * time.Millisecond)
		ms += 10 * speed
	}
	return ms
}

// display is the CDJ's deck as the display renderer gets it (CDJ display path,
// a console's Line In channel).
func (c *cdjLine) display() deckState {
	c.t.Helper()
	decks := c.s.shadowDisplayCDJs([]int{2, 4}, false, c.now, nil)
	if len(decks) != 1 {
		c.t.Fatalf("display decks: %+v", decks)
	}
	return decks[0]
}

// engine is the CDJ's deck as the follower of an Engine copy gets it.
func (c *cdjLine) engine() deckState {
	c.t.Helper()
	decks, _, _, _ := c.s.shadowLegacy("13", c.now, true)
	if len(decks) != 1 {
		c.t.Fatalf("engine decks: %+v", decks)
	}
	return decks[0]
}

// #217: "While the CDJ is paused, its waveform on the Denon jumps forward and
// back, then stays in place." The status packet that says "paused" is up to
// 200 ms behind the position packets, and waits behind the media lookups of
// the ones before it; until it came the deck ran on from each position.
func TestCDJPausedHoldsStillBeforeItsStatusSaysSo(t *testing.T) {
	c := newCDJLine(t, 0)
	at := c.run(20000, time.Second, 1, true, 3, true, nil)
	// Paused: the positions stand still, the last status still says "playing".
	c.run(at, 150*time.Millisecond, 0, false, 0, false, nil)
	for i := 0; i < 15; i++ {
		c.run(at, 40*time.Millisecond, 0, false, 0, false, nil)
		for name, d := range map[string]deckState{"display": c.display(), "engine": c.engine()} {
			if d.Playing || d.PositionMS != at {
				t.Fatalf("%s, %d ms after the pause: playing %v at %.1f, paused at %.1f", name, 150+40*(i+1), d.Playing, d.PositionMS, at)
			}
		}
	}
	// PLAY again: it runs on from there, on the positions' line.
	at = c.run(at, 500*time.Millisecond, 1, true, 3, true, nil)
	if d := c.display(); !d.Playing || math.Abs(d.PositionMS-at) > 35 {
		t.Fatalf("after PLAY: %+v, deck at %.1f", d, at)
	}
}

// Paused with the jog wheel in CDJ mode a player repeats one frame (13 ms).
func TestCDJPausedInCDJModeHoldsStill(t *testing.T) {
	c := newCDJLine(t, 0)
	at := c.run(20000, time.Second, 1, true, 3, true, nil)
	c.status(5, false, uint32(at/500)+1)
	var seen []float64
	for i := 0; i < 40; i++ {
		c.position(at + float64(i%2)*13)
		if c.now = c.now.Add(30 * time.Millisecond); i >= 5 {
			d := c.display()
			if d.Playing {
				t.Fatalf("playing while paused: %+v", d)
			}
			seen = append(seen, d.PositionMS)
		}
	}
	for _, ms := range seen {
		if ms != seen[0] || math.Abs(ms-at) > 13 {
			t.Fatalf("a paused deck moved: %v (paused at %.1f)", seen, at)
		}
	}
}

// Paused, and the position packets do not arrive for a while: the status
// packet's position is the start of its beat, somewhere else.
func TestCDJPausedKeepsItsAbsolutePosition(t *testing.T) {
	c := newCDJLine(t, 7)
	at := c.run(20210, time.Second, 1, true, 3, true, nil)
	c.run(at, 300*time.Millisecond, 0, true, 5, false, nil)
	for i := 0; i < 15; i++ { // 3 s of status packets only
		c.status(5, false, uint32(at/500)+1)
		c.now = c.now.Add(200 * time.Millisecond)
		if d := c.display(); d.Playing || d.PositionMS != at {
			t.Fatalf("%d ms without position packets: %+v, paused at %.1f", 200*(i+1), d, at)
		}
	}
	// Another track: its position is not the old track's.
	c.track = 8
	c.status(5, false, 1)
	if d := c.display(); d.PositionMS == at {
		t.Fatalf("the other track at the old track's position: %+v", d)
	}
}

// CUE held plays from the cue point (play mode 7, no play flag); released, the
// deck is back on the cue point and stays.
func TestCDJCueHeldThenReleased(t *testing.T) {
	c := newCDJLine(t, 0)
	const cue = 8000.0
	c.run(cue, 300*time.Millisecond, 0, true, 6, false, nil)
	if d := c.engine(); d.Playing || d.Cue || d.PositionMS != cue {
		t.Fatalf("on the cue point: %+v", d)
	}
	last := cue
	at := c.run(cue, 800*time.Millisecond, 1, true, 7, false, func(at float64) {
		if at < cue+250 {
			return // the status packet with play mode 7 is on its way
		}
		d := c.engine()
		if !d.Playing || !d.Cue || d.PositionMS < last || math.Abs(d.PositionMS-at) > 35 {
			t.Fatalf("CUE held at %.1f: %+v (last %.1f)", at, d, last)
		}
		last = d.PositionMS
	})
	jumps := c.engine().Jumps
	c.run(cue, 300*time.Millisecond, 0, true, 6, false, nil)
	for i := 0; i < 10; i++ {
		c.run(cue, 40*time.Millisecond, 0, false, 0, false, nil)
		if d := c.engine(); d.Playing || d.Cue || d.PositionMS != cue || d.Jumps != jumps+1 {
			t.Fatalf("CUE released (held until %.1f): %+v, jumps before %d", at, d, jumps)
		}
	}
}

// Playing: every poll is on the deck's line and never behind the one before; a
// hot cue is one jump, and the deck goes on from there.
func TestCDJPlayingAndSeekDoNotJumpBack(t *testing.T) {
	c := newCDJLine(t, 0)
	last, jumps := 0.0, -1
	check := func(at float64) {
		for name, d := range map[string]deckState{"display": c.display(), "engine": c.engine()} {
			if !d.Playing || math.Abs(d.PositionMS-at) > 35 {
				t.Fatalf("%s at %.1f: %+v", name, at, d)
			}
			if name == "engine" {
				if d.PositionMS < last {
					t.Fatalf("went back: %.1f after %.1f", d.PositionMS, last)
				}
				last = d.PositionMS
				if jumps < 0 {
					jumps = d.Jumps
				} else if d.Jumps != jumps {
					t.Fatalf("a jump while playing on: %d, was %d", d.Jumps, jumps)
				}
			}
		}
	}
	at := c.run(5000, 300*time.Millisecond, 1, true, 3, true, nil)
	at = c.run(at, 2*time.Second, 1, true, 3, true, check)
	// Hot cue to 60 s.
	last, jumps = 0, -1
	c.run(60000, 100*time.Millisecond, 1, false, 0, false, nil)
	if d := c.engine(); d.Jumps == 0 || d.PositionMS < 60000 {
		t.Fatalf("after the hot cue (from %.1f): %+v", at, d)
	}
	c.run(60100, 2*time.Second, 1, true, 3, true, check)
	// Half speed is playing, not paused (15 ms a packet).
	c2 := newCDJLine(t, 0)
	c2.run(5000, 2*time.Second, 0.5, true, 3, true, func(at float64) {
		if at > 5300 && !c2.display().Playing {
			t.Fatalf("half speed taken for a paused deck at %.1f", at)
		}
	})
}
