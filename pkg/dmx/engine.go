package dmx

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Status struct {
	Config   Config              `json:"config"`
	Profiles map[string][]string `json:"profiles"`
	Running  bool                `json:"running"`
	Error    string              `json:"error"`
	Clock    Clock               `json:"clock"`
	Synced   bool                `json:"synced"`
	Frames   map[int][512]byte   `json:"frames"`
}

type route struct {
	output  Output
	network *net.UDPConn
	usb     usbOutput
}

// Engine owns no goroutines, timers or sockets until explicitly enabled.
// Apply and Close serialize teardown, including the last blackout frames.
type Engine struct {
	op          sync.Mutex
	mu          sync.Mutex
	config      Config
	clock       Clock
	beatBase    float64
	running     bool
	err         string
	frames      map[int][512]byte
	stop        chan struct{}
	done        chan struct{}
	cid         [16]byte
	sequence    byte
	artSequence byte
	closed      bool
	blocked     func() bool
	enabled     atomic.Bool
}

func New(blocked ...func() bool) *Engine {
	e := &Engine{config: DefaultConfig()}
	if len(blocked) > 0 {
		e.blocked = blocked[0]
	}
	return e
}

func cloneConfig(c Config) Config {
	c.Outputs = slices.Clone(c.Outputs)
	c.Patch = slices.Clone(c.Patch)
	return c
}

func (e *Engine) Enabled() bool { return e.enabled.Load() }

func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Status{Config: cloneConfig(e.config), Profiles: Profiles, Running: e.running, Error: e.err, Clock: e.clock, Synced: e.config.Sync && e.clock.Valid(time.Now()), Frames: make(map[int][512]byte, len(e.frames))}
	for k, f := range e.frames {
		s.Frames[k] = f
	}
	return s
}

// SetClock unwraps the four-beat bar. Small phase corrections follow the
// source immediately, including cue/loop jumps; timestamp ordering rejects
// delayed HTTP posts. No work is scheduled by a sample.
func (e *Engine) SetClock(c Clock) {
	if !e.Enabled() {
		return
	}
	now := time.Now()
	if !c.Valid(now) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.config.Enabled || (!e.clock.At.IsZero() && !c.At.After(e.clock.At)) {
		return
	}
	base := c.Beat
	if e.clock.Deck == c.Deck && e.clock.Valid(c.At) {
		expect := e.beatBase
		if e.clock.Playing {
			expect += c.At.Sub(e.clock.At).Seconds() * e.clock.BPM / 60
		}
		base += mathRound((expect-c.Beat)/4) * 4
	}
	e.clock, e.beatBase = c, base
}

func mathRound(v float64) float64 { // round halves away from zero
	if v < 0 {
		return -float64(int64(-v + 0.5))
	}
	return float64(int64(v + 0.5))
}

func (e *Engine) stopRun() {
	if e.stop != nil {
		close(e.stop)
		<-e.done
		e.stop, e.done = nil, nil
	}
}

func (e *Engine) Apply(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed {
		return errors.New("DMX engine is closed")
	}
	if c.Enabled && e.blocked != nil && e.blocked() {
		return errors.New("stop SoundSwitch before enabling standalone DMX")
	}
	e.mu.Lock()
	// Performance controls change at the next frame without resetting phase,
	// reopening USB or emitting intermediate blackout frames.
	if c.Enabled && e.config.Enabled && e.running && reflect.DeepEqual(c.Outputs, e.config.Outputs) && reflect.DeepEqual(c.Patch, e.config.Patch) {
		e.config = cloneConfig(c)
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()
	e.stopRun()
	e.mu.Lock()
	e.config, e.err, e.running, e.frames = cloneConfig(c), "", false, nil
	e.enabled.Store(c.Enabled)
	if !c.Enabled {
		e.clock, e.beatBase = Clock{}, 0
	}
	e.mu.Unlock()
	if !c.Enabled {
		return nil
	}
	if e.cid == [16]byte{} {
		if _, err := rand.Read(e.cid[:]); err != nil {
			return err
		}
		e.cid[6] = e.cid[6]&0x0f | 0x40 // RFC 4122 version 4 CID
		e.cid[8] = e.cid[8]&0x3f | 0x80
	}
	var routes []route
	var warnings []string
	for _, o := range c.Outputs {
		r := route{output: o}
		var err error
		if o.Protocol == "artnet" || o.Protocol == "sacn" {
			port := 6454
			if o.Protocol == "sacn" {
				port = 5568
			}
			r.network, err = net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(o.Host), Port: port})
			if err == nil {
				err = allowBroadcast(r.network)
			}
		} else {
			r.usb, err = openUSB(o)
		}
		if err != nil {
			if r.network != nil {
				r.network.Close()
			}
			warnings = append(warnings, fmt.Sprintf("%s universe %d: %v", o.Protocol, o.Universe, err))
			continue // missing USB / failed route must not prevent the others
		}
		routes = append(routes, r)
	}
	e.mu.Lock()
	e.err = strings.Join(warnings, "; ")
	e.running = len(routes) > 0
	e.mu.Unlock()
	if len(routes) > 0 {
		e.stop, e.done = make(chan struct{}), make(chan struct{})
		go e.run(routes, e.stop, e.done)
	}
	return nil
}

func (e *Engine) Close() {
	e.op.Lock()
	defer e.op.Unlock()
	e.stopRun()
	e.enabled.Store(false)
	e.closed = true
}

func phaseAt(c Config, clock Clock, base float64, start, now time.Time) (float64, bool) {
	if !c.Sync {
		return now.Sub(start).Seconds() * c.BPM / 60, true
	}
	if !clock.Valid(now) {
		return base, false
	}
	if clock.Playing {
		base += now.Sub(clock.At).Seconds() * clock.BPM / 60
	}
	return base, true
}

func (e *Engine) run(routes []route, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Second / 30)
	defer ticker.Stop()
	start := time.Now()
	checked := start
	defer func() {
		zero := map[int][512]byte{}
		for i := 0; i < 3; i++ {
			e.send(routes, zero, true)
		}
		for _, r := range routes {
			if r.network != nil {
				r.network.Close()
			}
			if r.usb != nil {
				r.usb.Close()
			}
		}
		e.mu.Lock()
		e.running = false
		e.frames = zero
		e.mu.Unlock()
	}()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			now := time.Now()
			if now.Sub(checked) >= time.Second {
				checked = now
				if e.blocked != nil && e.blocked() {
					e.mu.Lock()
					e.err = "SoundSwitch started; standalone DMX stopped"
					e.mu.Unlock()
					return
				}
			}
			e.mu.Lock()
			c := e.config
			beats, live := phaseAt(c, e.clock, e.beatBase, start, now)
			c.Blackout = c.Blackout || !live // no guessed live phase
			frames := Render(c, beats)
			e.frames = frames
			e.mu.Unlock()
			e.send(routes, frames, false)
		}
	}
}

func (e *Engine) send(routes []route, frames map[int][512]byte, terminated bool) {
	e.sequence++                          // E1.31 uses the full modulo-256 sequence space.
	e.artSequence = e.artSequence%255 + 1 // Art-Net reserves zero for disabled sequencing.
	var failures []string
	for _, r := range routes {
		f := frames[r.output.Universe]
		var err error
		if r.network != nil {
			p := ArtNet(r.output.Universe, e.artSequence, f)
			if r.output.Protocol == "sacn" {
				p = SACN(r.output.Universe, e.sequence, f, e.cid, terminated)
			}
			r.network.SetWriteDeadline(time.Now().Add(30 * time.Millisecond))
			_, err = r.network.Write(p)
		} else {
			err = r.usb.Send(f)
		}
		if err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		e.mu.Lock()
		e.err = strings.Join(failures, "; ")
		e.mu.Unlock()
	}
}

type usbOutput interface {
	Send([512]byte) error
	Close() error
}
