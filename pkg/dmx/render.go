package dmx

import (
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"time"
)

// Profiles are generic channel layouts, not vendor personalities. Check the
// fixture's DMX chart before patching it (in particular smoke and strobe).
var Profiles = map[string][]string{
	"LED Par":            {"dimmer", "red", "green", "blue"},
	"LED Bar":            {"dimmer", "red", "green", "blue", "strobe"},
	"Moving Head Beam":   {"pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "gobo", "strobe"},
	"Moving Head Wash":   {"pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "beam", "strobe"},
	"Moving Head Spot":   {"pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "gobo", "beam", "strobe"},
	"Moving Head 3-in-1": {"pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "gobo", "beam", "mode", "strobe"},
	"Smoke":              {"smoke"},
}

type Output struct {
	Protocol string `json:"protocol"`
	Universe int    `json:"universe"`
	Host     string `json:"host,omitempty"`
	Device   string `json:"device,omitempty"`
	Bus      int    `json:"bus,omitempty"`
	Address  int    `json:"address,omitempty"`
}

type Fixture struct {
	Profile  string `json:"profile"`
	Universe int    `json:"universe"`
	Address  int    `json:"address"`
	Gobo     byte   `json:"gobo,omitempty"`
	Beam     byte   `json:"beam,omitempty"`
	Mode     byte   `json:"mode,omitempty"`
	Strobe   byte   `json:"strobe,omitempty"`
}

type Config struct {
	Enabled   bool      `json:"enabled"`
	Blackout  bool      `json:"blackout"`
	Outputs   []Output  `json:"outputs"`
	Patch     []Fixture `json:"patch"`
	Effect    string    `json:"effect"`
	Speed     float64   `json:"speed"` // beats per cycle
	BPM       float64   `json:"bpm"`   // fallback only when sync is off
	Sync      bool      `json:"sync"`
	Master    byte      `json:"master"`
	Intensity byte      `json:"intensity"`
	Red       byte      `json:"red"`
	Green     byte      `json:"green"`
	Blue      byte      `json:"blue"`
	Pan       int       `json:"pan"`
	Tilt      int       `json:"tilt"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
}

func DefaultConfig() Config {
	return Config{Effect: "Circle", Speed: 4, BPM: 120, Sync: true, Master: 255,
		Pan: 32768, Tilt: 32768, Width: 16000, Height: 16000,
		Red: 255, Green: 255, Blue: 255, Outputs: []Output{}, Patch: []Fixture{}}
}

func (c Config) Validate() error {
	if c.Effect != "Circle" && c.Effect != "8-Figure" && c.Effect != "Pan-Tilt Sweep" && c.Effect != "Static" {
		return errors.New("unknown effect")
	}
	if math.IsNaN(c.BPM) || math.IsInf(c.BPM, 0) || c.BPM < 20 || c.BPM > 300 || math.IsNaN(c.Speed) || math.IsInf(c.Speed, 0) || c.Speed < 1 || c.Speed > 64 {
		return errors.New("BPM must be 20..300; beats per cycle must be 1..64")
	}
	if c.Pan < 0 || c.Pan > 65535 || c.Tilt < 0 || c.Tilt > 65535 || c.Width < 0 || c.Width > 32767 || c.Height < 0 || c.Height > 32767 {
		return errors.New("invalid pan/tilt range")
	}
	if len(c.Outputs) > 8 || len(c.Patch) > 128 {
		return errors.New("maximum 8 outputs / 128 fixtures")
	}
	devices := map[string]bool{}
	for _, o := range c.Outputs {
		if o.Universe < 1 || o.Universe > 32768 {
			return errors.New("universe must be 1..32768")
		}
		switch o.Protocol {
		case "artnet", "sacn":
			if ip := net.ParseIP(o.Host); ip == nil || ip.To4() == nil {
				return errors.New("output host must be an IPv4 address")
			}
		case "ftdi", "udmx":
			key := o.Device
			if o.Protocol == "ftdi" {
				suffix := strings.TrimPrefix(key, "/dev/ttyUSB")
				if suffix == key { suffix = strings.TrimPrefix(key, "/dev/ttyACM") }
				if suffix == key || strings.Contains(key, "..") || strings.Contains(suffix, "/") {
					return errors.New("invalid FTDI device")
				}
			} else {
				if o.Bus < 1 || o.Bus > 255 || o.Address < 1 || o.Address > 127 {
					return errors.New("invalid uDMX bus/address")
				}
				key = fmt.Sprintf("udmx:%d:%d", o.Bus, o.Address)
			}
			if devices[key] {
				return errors.New("duplicate USB device")
			}
			devices[key] = true
		default:
			return errors.New("protocol must be artnet, sacn, ftdi or udmx")
		}
	}
	used := map[[2]int]bool{}
	for _, f := range c.Patch {
		p, ok := Profiles[f.Profile]
		if !ok || f.Universe < 1 || f.Universe > 32768 || f.Address < 1 || f.Address+len(p) > 513 {
			return errors.New("invalid fixture profile, universe or address")
		}
		for ch := f.Address; ch < f.Address+len(p); ch++ {
			key := [2]int{f.Universe, ch}
			if used[key] {
				return errors.New("fixture channels overlap")
			}
			used[key] = true
		}
	}
	return nil
}

// Clock is a timestamped sample from Engine's selected master. BPM already
// includes pitch. Beat is Engine's BeatNumber, modulo four beats.
type Clock struct {
	Deck    int       `json:"deck"`
	BPM     float64   `json:"bpm"`
	Beat    float64   `json:"beat"`
	Playing bool      `json:"playing"`
	At      time.Time `json:"at"`
}

func (c Clock) Valid(now time.Time) bool {
	return c.Deck >= 1 && c.Deck <= 4 && c.BPM >= 20 && c.BPM <= 300 && !math.IsNaN(c.BPM) && !math.IsNaN(c.Beat) && !math.IsInf(c.Beat, 0) && c.Beat >= 0 && c.Beat <= 4 && !c.At.IsZero() && now.Sub(c.At) >= 0 && now.Sub(c.At) <= time.Second
}

// Render is deterministic: beats is the master beat position, extrapolated
// from the last sample only while playing. It deliberately never emits smoke.
// Smoke requires a separate momentary/armed workflow and is outside this MVP.
func Render(c Config, beats float64) map[int][512]byte {
	frames := make(map[int][512]byte)
	for _, o := range c.Outputs {
		frames[o.Universe] = [512]byte{}
	}
	phase := beats / c.Speed * 2 * math.Pi
	x, y := 0.0, 0.0
	switch c.Effect {
	case "Circle":
		x, y = math.Sin(phase), math.Cos(phase)
	case "8-Figure":
		x, y = math.Sin(phase), math.Sin(2*phase)
	case "Pan-Tilt Sweep":
		x, y = 2/math.Pi*math.Asin(math.Sin(phase)), math.Sin(phase/2)
	}
	pan := int(math.Max(0, math.Min(65535, float64(c.Pan)+x*float64(c.Width))))
	tilt := int(math.Max(0, math.Min(65535, float64(c.Tilt)+y*float64(c.Height))))
	values := map[string]byte{"pan": byte(pan >> 8), "panFine": byte(pan), "tilt": byte(tilt >> 8), "tiltFine": byte(tilt), "dimmer": byte(int(c.Intensity) * int(c.Master) / 255), "red": c.Red, "green": c.Green, "blue": c.Blue}
	for _, f := range c.Patch {
		frame := frames[f.Universe]
		for offset, ch := range Profiles[f.Profile] {
			v, ok := values[ch]
			if !ok {
				v = map[string]byte{"gobo": f.Gobo, "beam": f.Beam, "mode": f.Mode, "strobe": f.Strobe}[ch]
			}
			if c.Blackout {
				v = 0
			}
			frame[f.Address-1+offset] = v
		}
		frames[f.Universe] = frame
	}
	return frames
}
