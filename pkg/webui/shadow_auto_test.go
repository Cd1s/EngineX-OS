package webui

import (
	"math"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestCDJFallbackReasons(t *testing.T) {
	host := net.IPv4(192, 0, 2, 9)
	for _, c := range []struct {
		slot   byte
		host   net.IP
		failed string
		want   string
	}{
		{2, host, "", ""}, // SD and USB: copied over NFS
		{3, host, "", ""},
		{4, host, "", "link"},  // rekordbox on a computer
		{4, nil, "", "link"},   // the kind of source counts before the network
		{1, host, "", "media"}, // CD
		{7, host, "", "media"},
		{5, host, "", "streaming"},
		{6, host, "", "streaming"},
		{9, host, "", "streaming"},
		{14, host, "timeout", "streaming"},
		{3, nil, "", "network"}, // the player holding the stick is not on the link
		{3, host, "failed", "network"},
		{3, host, "unavailable", "network"},
		{2, host, "interrupted", "network"},
		{3, host, "timeout", "timeout"},
	} {
		if got := cdjFallback(prolink.Device{SourceSlot: c.slot, TrackID: 7}, c.host, c.failed); got != c.want {
			t.Errorf("slot %d, host %v, failed %q: %q, want %q", c.slot, c.host, c.failed, got, c.want)
		}
	}
}

// autoCDJ is a player (JP07, decks 1 and 3) in auto mode beside one CDJ whose
// stick is served over NFS from usb. cdj sets what the CDJ plays, deck composes
// its shadow deck.
func autoCDJ(t *testing.T, usb string) (s *Server, cdj func(slot byte, id uint32), deck func() deckState) {
	t.Helper()
	old := deviceCompatiblePath
	deviceCompatiblePath = filepath.Join(t.TempDir(), "compatible")
	t.Cleanup(func() { deviceCompatiblePath = old })
	os.WriteFile(deviceCompatiblePath, []byte("inmusic,jp07\x00"), 0600)
	nfs := &prolink.NFSServer{Root: usb}
	if err := nfs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nfs.Close)
	s = New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	s.shadow.dataRoot, s.shadow.mediaRoot = t.TempDir(), t.TempDir()
	s.shadow.cdjPortmap = nfs.PortmapPort()
	s.shadow.copyRetry, s.shadow.copyTimeout = 40*time.Millisecond, 5*time.Second
	if got := s.loadUI().ShadowMode; got != "auto" {
		t.Fatalf("default mode %q", got)
	}
	cdj = func(slot byte, id uint32) {
		dev := prolink.Device{Address: "127.0.0.1", Name: "CDJ-3000", Number: 2, SourcePlayer: 2, SourceSlot: slot, TrackID: id, PacketType: 10, Playing: true, PlayPositionMS: 500, Pitch: 1.25, BPM: 120, Title: "From the CDJ", LastSeen: time.Now()}
		prolinkMu.Lock()
		postedDevices[s] = map[string]prolink.Device{deviceKey(dev): dev}
		prolinkMu.Unlock()
	}
	deck = func() deckState {
		t.Helper()
		out := s.shadowCompose("13", time.Now())
		if len(out) != 1 || out[0].Deck != 2 {
			t.Fatalf("compose: %+v", out)
		}
		return out[0]
	}
	return s, cdj, deck
}

// Auto mode: a CDJ playing from a stick goes through Engine as an EngineX peer
// does, its file copied over NFS; nothing of the display renderer on its deck.
func TestShadowAutoCDJThroughEngine(t *testing.T) {
	usb := t.TempDir()
	testfixture.Export(t, usb)
	s, cdj, deck := autoCDJ(t, usb)
	cdj(3, 2)
	if d := deck(); d.DisplayOnly || d.Fallback != "" || !d.Loading || d.Path != "" || d.Title != "From the CDJ" || d.Pitch != 1.25 || !d.Playing {
		t.Fatalf("while the copy runs: %+v", d)
	}
	d := until(t, deck, "the copy", func(d deckState) bool { return d.Path != "" })
	want, _ := os.ReadFile(filepath.Join(usb, "MUSIC/two.flac"))
	if b, _ := os.ReadFile(d.Path); string(b) != string(want) || d.DisplayOnly || d.Fallback != "" || d.Loading || d.Error != "" || d.ID == "" {
		t.Fatalf("copied: %+v", d)
	}
	// Nothing loaded: the deck is empty, as Engine shows it.
	cdj(0, 0)
	if d := deck(); d.DisplayOnly || d.Path != "" || d.Loading || d.Fallback != "" {
		t.Fatalf("empty CDJ: %+v", d)
	}
	// The forced modes do not ask where the track comes from.
	cdj(9, 2)
	if d := s.shadowComposeLegacy("13", time.Now()); len(d) != 1 || d[0].DisplayOnly || d[0].Fallback != "" {
		t.Fatalf("Engine mode: %+v", d)
	}
	if d := s.shadowComposeDisplay("13", time.Now()); len(d) != 1 || !d[0].DisplayOnly || d[0].Fallback != "" {
		t.Fatalf("CDJ mode: %+v", d)
	}
}

// Auto mode: a track no player serves a file of stays on the display renderer
// and says why; no copy is tried.
func TestShadowAutoCDJWithoutFileFallsBack(t *testing.T) {
	usb := t.TempDir()
	testfixture.Export(t, usb)
	s, cdj, deck := autoCDJ(t, usb)
	for slot, why := range map[byte]string{9: "streaming", 6: "streaming", 4: "link", 1: "media"} {
		cdj(slot, 2)
		if d := deck(); !d.DisplayOnly || d.Fallback != why || d.Path != "" || d.Loading || d.Error != "" || d.Title != "From the CDJ" {
			t.Fatalf("slot %d: %+v", slot, d)
		}
	}
	if n := s.shadow.fetchCDJCalls.Load(); n != 0 {
		t.Fatalf("%d copies tried of tracks that have no file", n)
	}
}

// Auto mode: a copy that fails leaves the deck to the display renderer, with
// the reason, and is tried again; Engine takes the deck once it succeeds.
func TestShadowAutoCDJCopyFailsThenRecovers(t *testing.T) {
	usb := t.TempDir()
	testfixture.Export(t, usb)
	audio := filepath.Join(usb, "MUSIC/two.flac")
	want, _ := os.ReadFile(audio)
	os.Remove(audio)
	s, cdj, deck := autoCDJ(t, usb)
	cdj(3, 2)
	if d := deck(); d.DisplayOnly || !d.Loading {
		t.Fatalf("the first attempt is Engine's: %+v", d)
	}
	d := until(t, deck, "the fallback", func(d deckState) bool { return d.DisplayOnly })
	if d.Fallback != "network" || d.Path != "" || d.Loading {
		t.Fatalf("after the failure: %+v", d)
	}
	// It stays there while the next attempts fail: no flapping between the paths.
	tried := s.shadow.fetchCDJCalls.Load()
	until(t, deck, "more attempts", func(d deckState) bool {
		if !d.DisplayOnly || d.Fallback != "network" {
			t.Fatalf("left the display path while the copy still fails: %+v", d)
		}
		s.shadow.mu.Lock()
		defer s.shadow.mu.Unlock()
		for _, c := range s.shadow.copies {
			return c.fails >= 3
		}
		return false
	})
	if s.shadow.fetchCDJCalls.Load() == tried {
		t.Fatal("the copy was not tried again")
	}
	os.WriteFile(audio, want, 0600)
	d = until(t, deck, "Engine's turn", func(d deckState) bool { return !d.DisplayOnly })
	if b, _ := os.ReadFile(d.Path); string(b) != string(want) || d.Fallback != "" || d.Loading {
		t.Fatalf("recovered: %+v", d)
	}

	// No answer from the player in time.
	s.shadow.mu.Lock()
	s.shadow.cdjPortmap, s.shadow.copyTimeout = closedUDPPort(t), 150*time.Millisecond
	s.shadow.mu.Unlock()
	cdj(3, 3)
	d = until(t, deck, "the timeout", func(d deckState) bool { return d.DisplayOnly })
	if d.Fallback != "timeout" {
		t.Fatalf("a player that does not answer: %+v", d)
	}
}

func closedUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() }) // held open and never read: requests get no answer
	return c.LocalAddr().(*net.UDPAddr).Port
}

// The CDJ's playhead line: a late packet does not move it, eight late ones in
// a row move it back by the least of them, and the rate is the exact pitch.
func TestCDJObserveLine(t *testing.T) {
	sh := newShadowState()
	t0 := time.Now()
	const rate = 1.032534
	at := func(ms float64) time.Time { return t0.Add(time.Duration(ms * float64(time.Millisecond))) }
	// The deck is at 10 s at t0 and runs at rate. A packet sent at sendMS arrives lateMS after it.
	pos := func(sendMS, lateMS, pitch float64) float64 {
		sh.observeAt(prolink.Device{Number: 2, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: uint32(math.Round(10000 + sendMS*rate)), Pitch: pitch}, at(sendMS+lateMS))
		p := sh.precise[2]
		// How far the line is behind the deck when the packet arrives.
		return 10000 + (sendMS+lateMS)*rate - p.ms
	}
	sh.observeAt(prolink.Device{Number: 2, PacketType: 0x0a, TrackID: 5, Playing: true, Pitch: 3.2534}, t0)
	if got := sh.cdj[2].exact(3.25); got != 3.2534 {
		t.Fatalf("the rounded pitch won: %v", got)
	}
	if got := sh.cdj[2].exact(4); got != 4 {
		t.Fatalf("a moved fader keeps the old exact pitch: %v", got)
	}
	send := 0.0
	next := func(late float64) float64 { send += 30; return pos(send, late, 3.25) }
	next(0)
	next(0)
	for i, late := range []float64{0, 12, 3, 25, 0, 7, 40, 1} {
		if off := next(late); math.Abs(off) > 0.6 { // the packet's position is in whole ms
			t.Fatalf("packet %d, %v ms late: the line is %.2f ms off", i, late, off)
		}
	}
	if sh.cdj[2].jumps != 0 {
		t.Fatalf("jitter counted as %d jumps", sh.cdj[2].jumps)
	}
	// The deck is held back 20 ms by hand (no pitch change in the packets): every
	// packet is late from then on, and the line follows once eight in a row were
	// (two rounds of eight: the one running had packets from before).
	var off float64
	for range 16 {
		off = next(20) - 20*rate
	}
	if math.Abs(off) > 0.6 {
		t.Fatalf("the line did not follow the deck back: %.2f ms", off)
	}
	// Paused: the line is the packet's position.
	sh.observeAt(prolink.Device{Number: 2, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: 20000, Pitch: 3.25}, at(send+500))
	sh.observeAt(prolink.Device{Number: 2, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: 20000, Pitch: 3.25}, at(send+530))
	if p := sh.precise[2]; p.ms != 20000 || !p.at.Equal(at(send+530)) {
		t.Fatalf("paused: %+v", p)
	}
}
