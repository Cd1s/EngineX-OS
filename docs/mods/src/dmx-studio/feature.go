// Package dmxstudio is the DMX Studio helper: a generic DMX console.
// Hardware/fixture calibration is intentionally explicit.
package dmxstudio

import (
	"crypto/rand"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

// Profiles: channel numbers are offsets from the fixture start, not
// vendor-specific personalities.
var Profiles = map[string][]string{
	"LED Par":            {"dimmer", "red", "green", "blue"},
	"LED Bar":            {"dimmer", "red", "green", "blue", "strobe"},
	"Moving Head Beam":   {"pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "gobo", "strobe"},
	"Moving Head Wash":   {"pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "beam", "strobe"},
	"Moving Head Spot":   {"pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "gobo", "beam", "strobe"},
	"Moving Head 3-in-1": {"pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "gobo", "beam", "mode", "strobe"},
	"Smoke":              {"smoke"},
}

var bounds = []struct {
	key       string
	low, high float64
}{{"bpm", 20, 300}, {"speed", 1, 64}, {"phraseBeats", 0, 256}, {"pan", 0, 65535}, {"tilt", 0, 65535}, {"width", 0, 32767}, {"height", 0, 32767},
	{"intensity", 0, 255}, {"red", 0, 255}, {"green", 0, 255}, {"blue", 0, 255}, {"smokeLevel", 0, 255},
	{"smokeInterval", 1, 3600}, {"smokeDuration", 0, 60}}

type route struct {
	output  map[string]any
	adapter any // *udp, *serialPort, *udmx (or a test double with send/close)
}

// Feature is the DMX console state; exported fields are for tests.
type Feature struct {
	state               string
	Config              map[string]any
	FlashUntil          float64
	Running             bool
	Error               string
	sequence            byte
	cid                 []byte
	Routes              []route
	frames              map[int][]byte
	mu                  sync.Mutex
	stop                chan struct{}
	done                chan struct{}
	clockDone           chan struct{}
	start               time.Time
	looping             bool
	clock               beatClock
	webUIURL            string
	pollerActive        bool
	lastExternalClock   float64
	clockAPIAbsentUntil time.Time
	midiMu              sync.Mutex
	midiRunning         bool
	midiStop            chan struct{}
	midiDone            chan struct{}
	midiErr             string
	openMIDISink        func() (midiSink, error)
}

// New returns the DMX Studio feature with its 30 Hz render/output loop running.
func New(env modkit.Env) (modkit.Feature, error) {
	f, err := NewFeature(env.State)
	if err != nil {
		return nil, err
	}
	f.looping = true
	go f.loop()
	go f.clockLoop()
	f.syncMIDIClock(false)
	return f, nil
}

// NewFeature loads show.json without starting the output loop.
func NewFeature(state string) (*Feature, error) {
	f := &Feature{
		state: state,
		Config: map[string]any{
			"outputs": []any{}, "patch": []any{}, "bpm": 120.0, "effect": "Circle", "speed": 4.0,
			"followDeck": true, "phraseBeats": 0.0, "phraseScenes": []any{}, "midiClock": false,
			"pan": 32768.0, "tilt": 32768.0, "width": 16000.0, "height": 16000.0,
			"intensity": 0.0, "red": 255.0, "green": 255.0, "blue": 255.0,
			"smokeLevel": 0.0, "smokeInterval": 30.0, "smokeDuration": 2.0, "smokeArmed": false,
		},
		frames:            map[int][]byte{},
		stop:              make(chan struct{}),
		done:              make(chan struct{}),
		clockDone:         make(chan struct{}),
		start:             time.Now(),
		clock:             newBeatClock(),
		webUIURL:          envWebUIURL(),
		lastExternalClock: -1e9,
		openMIDISink:      openALSAMIDISink,
	}
	show := filepath.Join(state, "show.json")
	if _, err := os.Stat(show); err == nil {
		var saved map[string]any
		if err := modkit.LoadJSON(show, &saved); err != nil {
			return nil, err
		}
		c, err := f.Validated(saved)
		if err != nil {
			return nil, err
		}
		f.Config = c
	}
	f.Config["smokeArmed"] = false
	f.cid = make([]byte, 16)
	rand.Read(f.cid)
	f.cid[6] = f.cid[6]&0x0f | 0x40 // a version-4 UUID, as before
	f.cid[8] = f.cid[8]&0x3f | 0x80
	return f, nil
}

func (f *Feature) now() float64 { return time.Since(f.start).Seconds() }

func get(m map[string]any, key string) (any, error) {
	v, ok := m[key]
	if !ok {
		return nil, modkit.Bad("'%s'", key)
	}
	return v, nil
}

func objects(v any) ([]map[string]any, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		out[i] = m
	}
	return out, true
}

// Validated merges config into the current one and checks every value.
func (f *Feature) Validated(config map[string]any) (map[string]any, error) {
	c := map[string]any{}
	for k, v := range f.Config {
		c[k] = v
	}
	for k, v := range config {
		c[k] = v
	}
	switch c["effect"] {
	case "Circle", "8-Figure", "Pan-Tilt Sweep", "Static":
	default:
		return nil, modkit.Bad("unknown EFX")
	}
	for _, b := range bounds {
		n, err := modkit.Number(c[b.key], b.low, b.high)
		if err != nil {
			return nil, err
		}
		c[b.key] = n
	}
	if _, ok := c["followDeck"].(bool); !ok {
		return nil, modkit.Bad("followDeck must be boolean")
	}
	if _, ok := c["midiClock"].(bool); !ok {
		return nil, modkit.Bad("midiClock must be boolean")
	}
	scenes, err := validatePhraseScenes(c["phraseScenes"])
	if err != nil {
		return nil, err
	}
	c["phraseScenes"] = scenes
	if c["smokeDuration"].(float64) > c["smokeInterval"].(float64) {
		return nil, modkit.Bad("smoke duration cannot exceed interval")
	}
	if _, ok := c["smokeArmed"].(bool); !ok {
		return nil, modkit.Bad("smokeArmed must be boolean")
	}
	outputs, ok1 := objects(c["outputs"])
	patch, ok2 := objects(c["patch"])
	if !ok1 || len(outputs) > 8 || !ok2 || len(patch) > 128 {
		return nil, modkit.Bad("maximum 8 outputs / 128 fixtures")
	}
	used := map[string]bool{}
	for _, o := range outputs {
		u, err := get(o, "universe")
		if err != nil {
			return nil, err
		}
		if _, err := modkit.Integer(u, 1, 32768); err != nil {
			return nil, err
		}
		protocol, err := get(o, "protocol")
		if err != nil {
			return nil, err
		}
		switch protocol {
		case "artnet", "sacn":
			h, err := get(o, "host")
			if err != nil {
				return nil, err
			}
			host, _ := h.(string)
			if ip := net.ParseIP(host); ip == nil || ip.To4() == nil || strings.Contains(host, ":") {
				return nil, modkit.Bad("%v does not appear to be an IPv4 address", h)
			}
			if protocol == "sacn" {
				if _, err := modkit.Integer(u, 1, 63999); err != nil {
					return nil, err
				}
			}
		case "ftdi":
			d, err := get(o, "device")
			if err != nil {
				return nil, err
			}
			device := pyStr(d)
			if !strings.HasPrefix(device, "/dev/ttyUSB") && !strings.HasPrefix(device, "/dev/ttyACM") {
				return nil, modkit.Bad("invalid FTDI device")
			}
			if used[device] {
				return nil, modkit.Bad("USB outputs must use different devices")
			}
			used[device] = true
		case "udmx":
			b, err := get(o, "bus")
			if err != nil {
				return nil, err
			}
			bus, err := modkit.Integer(b, 1, 255)
			if err != nil {
				return nil, err
			}
			a, err := get(o, "address")
			if err != nil {
				return nil, err
			}
			address, err := modkit.Integer(a, 1, 127)
			if err != nil {
				return nil, err
			}
			key := "usb:" + strconv.Itoa(bus) + ":" + strconv.Itoa(address)
			if used[key] {
				return nil, modkit.Bad("USB outputs must use different devices")
			}
			used[key] = true
		default:
			return nil, modkit.Bad("protocol must be artnet, sacn, ftdi or udmx")
		}
	}
	occupied := map[[2]int]bool{}
	for _, fx := range patch {
		p, err := get(fx, "profile")
		if err != nil {
			return nil, err
		}
		name, _ := p.(string)
		profile, ok := Profiles[name]
		if !ok {
			return nil, modkit.Bad("'%v'", p)
		}
		a, err := get(fx, "address")
		if err != nil {
			return nil, err
		}
		address, err := modkit.Integer(a, 1, float64(513-len(profile)))
		if err != nil {
			return nil, err
		}
		u, err := get(fx, "universe")
		if err != nil {
			return nil, err
		}
		universe, err := modkit.Integer(u, 1, 32768)
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"x", "y"} {
			v, ok := fx[key]
			if !ok {
				v = 0.5
			}
			if _, err := modkit.Number(v, 0, 1); err != nil {
				return nil, err
			}
		}
		for _, key := range []string{"gobo", "beam", "mode", "strobe"} {
			v, ok := fx[key]
			if !ok {
				v = 0.0
			}
			if _, err := modkit.Integer(v, 0, 255); err != nil {
				return nil, err
			}
		}
		for slot := address; slot < address+len(profile); slot++ {
			k := [2]int{universe, slot}
			if occupied[k] {
				return nil, modkit.Bad("fixture channels overlap")
			}
			occupied[k] = true
		}
	}
	return c, nil
}

func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := modkitJSON(v)
	return b
}

func modkitJSON(v any) (string, error) {
	if n, ok := modkit.Float(v); ok {
		return modkit.Fmt(n), nil
	}
	return "", errors.New("not a string")
}

func num(v any) float64 {
	n, _ := modkit.Number(v, math.Inf(-1), math.Inf(1))
	return n
}

// Render computes every universe's 512 channels at monotonic time now.
func (f *Feature) Render(now float64) map[int][]byte {
	c := f.Config
	totalBeats := f.clock.beats(now, c["bpm"].(float64))
	eff := applyPhrase(c, totalBeats)
	// Without a deck clock this is the old wall-clock phase: now * bpm / 60 beats.
	phase := totalBeats / eff["speed"].(float64) * 2 * math.Pi
	x, y := 0.0, 0.0
	switch eff["effect"] {
	case "Circle":
		x, y = math.Sin(phase), math.Cos(phase)
	case "8-Figure":
		x, y = math.Sin(phase), math.Sin(2*phase)
	case "Pan-Tilt Sweep":
		x, y = 2/math.Pi*math.Asin(math.Sin(phase)), math.Sin(phase/2)
	}
	pan := int(math.Max(0, math.Min(65535, c["pan"].(float64)+x*eff["width"].(float64))))
	tilt := int(math.Max(0, math.Min(65535, c["tilt"].(float64)+y*eff["height"].(float64))))
	armed, _ := c["smokeArmed"].(bool)
	smoke := armed && (now < f.FlashUntil || math.Mod(now, c["smokeInterval"].(float64)) < c["smokeDuration"].(float64))
	values := map[string]int{"pan": pan >> 8, "panFine": pan & 255, "tilt": tilt >> 8, "tiltFine": tilt & 255,
		"dimmer": int(eff["intensity"].(float64)), "red": int(eff["red"].(float64)), "green": int(eff["green"].(float64)),
		"blue": int(eff["blue"].(float64)), "smoke": 0}
	if smoke {
		values["smoke"] = int(c["smokeLevel"].(float64))
	}
	frames := map[int][]byte{}
	outputs, _ := objects(c["outputs"])
	for _, o := range outputs {
		frames[int(num(o["universe"]))] = make([]byte, 512)
	}
	patch, _ := objects(c["patch"])
	for _, fx := range patch {
		u := int(num(fx["universe"]))
		frame, ok := frames[u]
		if !ok {
			frame = make([]byte, 512)
			frames[u] = frame
		}
		address := int(num(fx["address"]))
		for offset, channel := range Profiles[fx["profile"].(string)] {
			v, ok := values[channel]
			if !ok {
				v = int(num(fx[channel]))
			}
			frame[address-1+offset] = byte(v)
		}
	}
	return frames
}

type sender interface{ send([]byte) error }
type closer interface{ close() }

func (f *Feature) sendAll(frames map[int][]byte, terminated bool) error {
	f.sequence = f.sequence%255 + 1
	var failures []string
	for _, r := range f.Routes {
		universe := int(num(r.output["universe"]))
		frame, ok := frames[universe]
		if !ok {
			frame = make([]byte, 512)
		}
		var err error
		switch r.output["protocol"] {
		case "artnet":
			err = r.adapter.(sender).send(ArtNet(universe, f.sequence, frame))
		case "sacn":
			err = r.adapter.(sender).send(SACN(universe, f.sequence, frame, f.cid, terminated))
		default:
			err = r.adapter.(sender).send(frame)
		}
		if err != nil {
			// A failing route must not prevent blackout of remaining routes.
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// Blackout stops output: three zero frames on every route, then closes them.
func (f *Feature) Blackout() {
	f.Running = false
	f.FlashUntil = 0
	f.Config["smokeArmed"] = false
	f.frames = map[int][]byte{}
	outputs, _ := objects(f.Config["outputs"])
	for _, o := range outputs {
		f.frames[int(num(o["universe"]))] = make([]byte, 512)
	}
	var failures []string
	for i := 0; i < 3; i++ {
		if err := f.sendAll(f.frames, true); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		f.Error = strings.Join(failures, "; ")
	}
	for _, r := range f.Routes {
		r.adapter.(closer).close()
	}
	f.Routes = nil
}

func (f *Feature) loop() {
	defer close(f.done)
	tick := time.NewTicker(time.Second / 30)
	defer tick.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-tick.C:
		}
		f.mu.Lock()
		f.frames = f.Render(f.now())
		if f.Running {
			if err := f.sendAll(f.frames, false); err != nil {
				f.Error = err.Error()
				f.Blackout()
			}
		}
		f.mu.Unlock()
	}
}

func (f *Feature) Status() (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusLocked(), nil
}

func (f *Feature) clockStatusLocked(now float64) map[string]any {
	st := f.clock.status(now, f.Config["bpm"].(float64), f.Config["phraseBeats"].(float64))
	follow, _ := f.Config["followDeck"].(bool)
	st["followDeck"] = follow
	if f.midiErr != "" {
		st["midiClockError"] = f.midiErr
	}
	return st
}

func (f *Feature) statusLocked() map[string]any {
	frames := map[string][]int{}
	for k, v := range f.frames {
		list := make([]int, len(v))
		for i, b := range v {
			list[i] = int(b)
		}
		frames[strconv.Itoa(k)] = list
	}
	config := map[string]any{}
	for k, v := range f.Config {
		config[k] = v
	}
	return map[string]any{"config": config, "profiles": Profiles, "running": f.Running, "error": f.Error, "frames": frames,
		"clock": f.clockStatusLocked(f.now())}
}

func (f *Feature) Action(req map[string]any) (any, error) {
	syncMIDI := false
	defer func() {
		if syncMIDI {
			f.syncMIDIClock(false)
		}
	}()
	f.mu.Lock()
	defer f.mu.Unlock()
	action, err := get(req, "action")
	if err != nil {
		return nil, err
	}
	switch action {
	case "configure":
		raw, err := get(req, "config")
		if err != nil {
			return nil, err
		}
		in, ok := raw.(map[string]any)
		if !ok {
			return nil, modkit.Bad("config must be an object")
		}
		config, err := f.Validated(in)
		if err != nil {
			return nil, err
		}
		if f.Running && (!reflect.DeepEqual(config["outputs"], f.Config["outputs"]) || !reflect.DeepEqual(config["patch"], f.Config["patch"])) {
			return nil, modkit.Bad("stop output before changing patch or routes")
		}
		saved := map[string]any{}
		for k, v := range config {
			saved[k] = v
		}
		saved["smokeArmed"] = false
		if err := modkit.SaveJSON(filepath.Join(f.state, "show.json"), saved); err != nil {
			return nil, err
		}
		f.Config = config
		if follow, _ := f.Config["followDeck"].(bool); !follow && f.clock.deck != 0 {
			f.clock.toManual(f.now(), f.Config["bpm"].(float64))
			f.pollerActive = false
			f.lastExternalClock = -1e9
		}
		syncMIDI = true
	case "start":
		if !f.Running {
			outputs, _ := objects(f.Config["outputs"])
			if len(outputs) == 0 {
				return nil, modkit.Bad("configure an output first")
			}
			for _, o := range outputs {
				var a any
				var err error
				switch o["protocol"] {
				case "artnet", "sacn":
					port := 6454
					if o["protocol"] == "sacn" {
						port = 5568
					}
					a, err = openUDP(o["host"].(string), port)
				case "ftdi":
					a, err = openSerial(pyStr(o["device"]))
				default:
					a, err = openUDMX(int(num(o["bus"])), int(num(o["address"])))
				}
				if err != nil {
					f.Blackout()
					return nil, err
				}
				f.Routes = append(f.Routes, route{output: o, adapter: a})
			}
			f.Running, f.Error = true, ""
		}
	case "stop":
		f.Blackout()
	case "clock":
		// Memory only, a small reply (never writes show.json).
		return f.clockAction(req, f.now(), wallMillis())
	case "follow":
		en, err := get(req, "enabled")
		if err != nil {
			return nil, err
		}
		on, ok := en.(bool)
		if !ok {
			return nil, modkit.Bad("enabled must be boolean")
		}
		f.Config["followDeck"] = on
		if !on {
			f.clock.toManual(f.now(), f.Config["bpm"].(float64))
			f.pollerActive = false
			f.lastExternalClock = -1e9
		}
		return map[string]any{"clock": f.clockStatusLocked(f.now())}, nil
	case "flash":
		if held, _ := req["held"].(bool); held && f.Running {
			f.FlashUntil = f.now() + 0.5
		} else {
			f.FlashUntil = 0
		}
	default:
		return nil, modkit.Bad("unknown action")
	}
	return f.statusLocked(), nil
}

// Close stops the loop and blacks out every route.
func (f *Feature) Close() {
	select {
	case <-f.stop:
	default:
		close(f.stop)
	}
	if f.looping {
		select {
		case <-f.done:
		case <-time.After(2 * time.Second):
		}
		select {
		case <-f.clockDone:
		case <-time.After(2 * time.Second):
		}
	}
	f.syncMIDIClock(true)
	f.mu.Lock()
	f.Blackout()
	f.mu.Unlock()
}
