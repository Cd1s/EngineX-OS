package dmx

import (
	"bytes"
	"encoding/hex"
	"math"
	"net"
	"sync"
	"testing"
	"time"
)

func TestArtNetSpecBytes(t *testing.T) {
	var f [512]byte
	f[0], f[511] = 17, 255
	p := ArtNet(0x1235, 7, f)
	// Art-Net ID, little-endian OpDmx, big-endian protocol revision,
	// sequence, physical, little-endian Port-Address, big-endian length.
	header, _ := hex.DecodeString("4172742d4e6574000050000e070034120200")
	if len(p) != 530 || !bytes.Equal(p[:18], header) || !bytes.Equal(p[18:], f[:]) {
		t.Fatalf("ArtDMX packet: %x", p[:18])
	}
	if ArtNet(1, 1, f)[14] != 0 || ArtNet(32768, 1, f)[15] != 0x7f {
		t.Fatal("one-based universe conversion")
	}
}

func TestSACNSpecBytes(t *testing.T) {
	var f [512]byte
	f[0], f[511] = 17, 255
	cid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	p := SACN(0x1234, 7, f, cid, false)
	// Independent E1.31 wire constants: ACN preamble / identifier, each
	// flags+length PDU, vectors, priority and DMP addressing fields.
	checks := map[int]string{
		0: "001000004153432d45312e3137000000", 16: "726e00000004",
		38: "725800000002", 108: "64000007001234",
		115: "720b02a100000001020100",
	}
	if len(p) != 638 {
		t.Fatal(len(p))
	}
	for offset, wantHex := range checks {
		want, _ := hex.DecodeString(wantHex)
		if !bytes.Equal(p[offset:offset+len(want)], want) {
			t.Fatalf("offset %d: %x, want %x", offset, p[offset:offset+len(want)], want)
		}
	}
	if !bytes.Equal(p[22:38], cid[:]) || !bytes.Equal(p[126:], f[:]) || string(bytes.TrimRight(p[44:108], "\x00")) != "EngineX DMX Studio" {
		t.Fatal("CID/source/slots")
	}
	if SACN(1, 1, f, cid, true)[112] != 0x40 {
		t.Fatal("stream termination flag")
	}
}

func movingConfig() Config {
	c := DefaultConfig()
	c.Enabled = true
	c.Sync = false
	c.Intensity = 200
	c.Master = 128
	c.Patch = []Fixture{{Profile: "Moving Head Beam", Universe: 1, Address: 1, Gobo: 7, Strobe: 8}}
	return c
}

func TestMovementTiming(t *testing.T) {
	c := movingConfig()
	for _, tc := range []struct {
		effect    string
		beat      float64
		pan, tilt int
	}{
		{"Circle", 0, 32768, 48768}, {"Circle", 1, 48768, 32768},
		{"Circle", 2, 32768, 16768}, {"Circle", 4, 32767, 48768},
		{"8-Figure", 0.5, 44081, 48768}, {"Pan-Tilt Sweep", 1, 48768, 44081},
		{"Static", 1, 32768, 32768},
	} {
		c.Effect = tc.effect
		f := Render(c, tc.beat)[1]
		pan, tilt := int(f[0])<<8|int(f[1]), int(f[2])<<8|int(f[3])
		if math.Abs(float64(pan-tc.pan)) > 1 || math.Abs(float64(tilt-tc.tilt)) > 1 {
			t.Fatalf("%s beat %v: %d,%d", tc.effect, tc.beat, pan, tilt)
		}
		if f[4] != 100 || f[5] != 255 || f[8] != 7 || f[9] != 8 {
			t.Fatal("master/fixture attributes")
		}
	}
	// 120 BPM reaches beat one after 500 ms; 60 BPM takes one second.
	if Render(c, 0.5*120/60)[1] != Render(c, 1*60/60)[1] {
		t.Fatal("BPM timing")
	}
	c.Blackout = true
	f := Render(c, 1)[1]
	if f != [512]byte{} {
		t.Fatal("blackout must clear every channel")
	}
}

func TestPatchValidation(t *testing.T) {
	c := movingConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Patch = append(c.Patch, Fixture{Profile: "LED Par", Universe: 1, Address: 9})
	if c.Validate() == nil {
		t.Fatal("overlap accepted")
	}
	c.Patch[1].Universe = 2
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Patch[1].Address = 510
	if c.Validate() == nil {
		t.Fatal("channel 513 accepted")
	}
	c = movingConfig()
	c.BPM = math.NaN()
	if c.Validate() == nil {
		t.Fatal("NaN accepted")
	}
}

func TestMasterPhaseUnwrapAndPause(t *testing.T) {
	e := New()
	defer e.Close()
	c := movingConfig()
	c.Sync = true
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-500 * time.Millisecond)
	e.SetClock(Clock{Deck: 1, BPM: 120, Beat: 3.8, Playing: true, At: at})
	e.SetClock(Clock{Deck: 1, BPM: 120, Beat: 0.2, Playing: true, At: at.Add(200 * time.Millisecond)})
	if math.Abs(e.beatBase-4.2) > 0.0001 {
		t.Fatalf("bar wrapped to %v", e.beatBase)
	}
	e.SetClock(Clock{Deck: 1, BPM: 130, Beat: 0.3, Playing: false, At: at.Add(250 * time.Millisecond)})
	if math.Abs(e.beatBase-4.3) > 0.0001 || e.clock.Playing {
		t.Fatal("pause / pitch update")
	}
	e.SetClock(Clock{Deck: 1, BPM: 99, Beat: 1, At: at})
	if e.clock.BPM != 130 {
		t.Fatal("old sample accepted")
	}
	e.SetClock(Clock{Deck: 2, BPM: 100, Beat: 2, At: at.Add(300 * time.Millisecond)})
	if e.beatBase != 2 {
		t.Fatal("new master phase")
	}
	if e.clock.Valid(time.Now().Add(2 * time.Second)) {
		t.Fatal("stale master accepted")
	}
}

func TestDisabledAndMissingUSB(t *testing.T) {
	e := New()
	defer e.Close()
	if e.stop != nil || e.running {
		t.Fatal("disabled engine started resources")
	}
	c := DefaultConfig()
	c.Enabled = true
	c.Outputs = []Output{{Protocol: "ftdi", Universe: 1, Device: "/dev/ttyUSB-dmx134-missing"}}
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	if st := e.Status(); st.Running || st.Error == "" {
		t.Fatal("missing optional USB not reported")
	}
	c.Enabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	if e.stop != nil {
		t.Fatal("disabled engine retained worker")
	}
}

func TestSoundSwitchGuard(t *testing.T) {
	e := New(func() bool { return true })
	defer e.Close()
	c := DefaultConfig()
	c.Enabled = true
	if e.Apply(c) == nil || e.Status().Config.Enabled {
		t.Fatal("SoundSwitch conflict accepted")
	}
}

func TestConcurrentConfigurationAndSamples(t *testing.T) {
	e := New()
	defer e.Close()
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				c := movingConfig()
				_ = e.Apply(c)
				e.SetClock(Clock{Deck: 1, BPM: 120, Beat: 1, Playing: true, At: time.Now().Add(-time.Millisecond)})
				_ = e.Status()
			}
		}()
	}
	wg.Wait()
}

func TestNetworkFramesAndShutdown(t *testing.T) {
	recv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	send, err := net.DialUDP("udp4", nil, recv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	e := New()
	e.config = movingConfig()
	stop, done := make(chan struct{}), make(chan struct{})
	go e.run([]route{{output: Output{Protocol: "sacn", Universe: 1}, network: send}}, stop, done)
	buf := make([]byte, 1024)
	recv.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := recv.ReadFromUDP(buf)
	if err != nil || n != 638 || buf[126+4] != 100 || buf[112] != 0 {
		t.Fatalf("live packet n=%d err=%v", n, err)
	}
	close(stop)
	<-done
	terminated := 0
	for i := 0; i < 4; i++ {
		recv.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _, err = recv.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if n == 638 && buf[112] == 0x40 {
			terminated++
			if !bytes.Equal(buf[126:638], make([]byte, 512)) {
				t.Fatal("shutdown frame not zero")
			}
		}
	}
	if terminated != 3 {
		t.Fatalf("got %d termination packets", terminated)
	}
}
