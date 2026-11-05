package prolink_test

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestDiscoveryStatusAndExpiry(t *testing.T) {
	b := make([]byte, 54)
	copy(b, "Qspt1WmJOL")
	b[10] = 6
	copy(b[12:], "rekordbox\x00garbage") // bytes after the NUL are not part of the name
	b[0x24] = 17
	ip := net.IPv4(192, 168, 1, 2)
	d, e := prolink.ParsePacket(b, 50000, ip)
	if e != nil || d.Name != "rekordbox" || d.Number != 17 {
		t.Fatal(d, e)
	}
	discovery := prolink.NewDiscovery()
	discovery.Observe(d)
	if len(discovery.Devices(time.Now())) != 1 || len(discovery.Devices(time.Now().Add(11*time.Second))) != 0 {
		t.Fatal("discovery TTL")
	}
	status := make([]byte, 0xd4)
	copy(status, "Qspt1WmJOL")
	status[10] = 0xa
	copy(status[12:], "CDJ-3000")
	status[0x21] = 1
	status[0x28] = 17
	status[0x29] = 3
	binary.BigEndian.PutUint32(status[0x2c:], 42)
	d, e = prolink.ParsePacket(status, 50002, ip)
	if e != nil || d.TrackID != 42 || d.SourcePlayer != 17 || d.SourceSlot != 3 {
		t.Fatal(d, e)
	}
	for _, bad := range [][]byte{nil, b[:20], status[:54], []byte("untrusted UDP")} {
		if _, e = prolink.ParsePacket(bad, 50002, ip); e == nil {
			t.Fatal("accepted bad packet")
		}
	}
}

func TestObserveReturnsMergedStatusAndExpiresOldTrack(t *testing.T) {
	d := prolink.NewDiscovery()
	now := time.Now()
	status := prolink.Device{Address: "192.168.1.2", Number: 1, PacketType: 10, TrackID: 42, SourcePlayer: 2, SourceSlot: 3, LastSeen: now}
	d.Observe(status)
	status.PacketType = 6
	status.TrackID = 0
	status.SourcePlayer = 0
	status.SourceSlot = 0
	merged := d.Observe(status)
	if merged.TrackID != 42 || merged.SourcePlayer != 2 || merged.SourceSlot != 3 {
		t.Fatal("notification lost status", merged)
	}
	d.Devices(now.Add(11 * time.Second))
	if got := d.Observe(status); got.TrackID != 0 {
		t.Fatal("expired status was resurrected", got)
	}
}

func TestParseBeatPacket(t *testing.T) {
	ip := net.IPv4(192, 168, 1, 5)

	// Truncated beat packet
	bad := make([]byte, 0x50)
	copy(bad, "Qspt1WmJOL")
	bad[10] = 0x28
	if _, err := prolink.ParsePacket(bad, 50001, ip); err == nil {
		t.Fatal("expected error for truncated beat packet")
	}

	// Valid beat packet
	pkt := make([]byte, 0x64)
	copy(pkt, "Qspt1WmJOL")
	pkt[10] = 0x28
	copy(pkt[12:], "CDJ-3000")
	pkt[0x21] = 2 // player number

	// Raw pitch: 0x108000 -> (0x108000 - 0x100000) / 0x100000 * 100 = 3.125%
	binary.BigEndian.PutUint32(pkt[0x54:], 0x108000)
	// Raw BPM: 12800 -> 128.0
	binary.BigEndian.PutUint16(pkt[0x5a:], 12800)
	// Beat in bar: 3
	pkt[0x5c] = 3

	d, err := prolink.ParsePacket(pkt, 50001, ip)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Number != 2 {
		t.Errorf("got number %d, want 2", d.Number)
	}
	if math.Abs(d.Pitch-3.125) > 0.001 {
		t.Errorf("got pitch %f, want 3.125", d.Pitch)
	}
	if math.Abs(d.BPM-128.0) > 0.001 {
		t.Errorf("got BPM %f, want 128.0", d.BPM)
	}
	expectedEff := 128.0 * (1 + 3.125/100)
	if math.Abs(d.EffectiveBPM-expectedEff) > 0.001 {
		t.Errorf("got effective BPM %f, want %f", d.EffectiveBPM, expectedEff)
	}
	if d.BeatInBar != 3 {
		t.Errorf("got beat in bar %d, want 3", d.BeatInBar)
	}
	if !d.Playing {
		t.Error("expected playing to be true for beat packet")
	}
}

func TestParsePlayerStatus(t *testing.T) {
	ip := net.IPv4(192, 168, 1, 10)

	// Truncated status packet
	bad := make([]byte, 0xc0)
	copy(bad, "Qspt1WmJOL")
	bad[10] = 0x0a
	if _, err := prolink.ParsePacket(bad, 50002, ip); err == nil {
		t.Fatal("expected error for truncated player status")
	}

	pkt := make([]byte, 0xd0)
	copy(pkt, "Qspt1WmJOL")
	pkt[10] = 0x0a
	copy(pkt[12:], "CDJ-3000")
	pkt[0x21] = 1 // player number
	pkt[0x28] = 1 // source player
	pkt[0x29] = 2 // source slot
	binary.BigEndian.PutUint32(pkt[0x2c:], 101)

	// Flags at 0x7b: playing (bit 6: 0x40), master (bit 5: 0x20), sync (bit 4: 0x10), on_air (bit 3: 0x08)
	pkt[0x7b] = 0x40 | 0x20 | 0x10 | 0x08

	// Pitch at 0x8d: 0x100000 -> 0%
	binary.BigEndian.PutUint32(pkt[0x8d:], 0x100000)
	// BPM at 0x92: 12000 -> 120.0
	binary.BigEndian.PutUint16(pkt[0x92:], 12000)
	// Beat at 0xa0: 17
	binary.BigEndian.PutUint32(pkt[0xa0:], 17)
	// Beat in bar at 0xa6: 2
	pkt[0xa6] = 2
	// Play position at 0xb4: 8000 ms
	binary.BigEndian.PutUint32(pkt[0xb4:], 8000)

	d, err := prolink.ParsePacket(pkt, 50002, ip)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !d.Playing || !d.Master || !d.Sync || !d.OnAir {
		t.Errorf("flags mismatch: playing=%v, master=%v, sync=%v, onAir=%v", d.Playing, d.Master, d.Sync, d.OnAir)
	}
	if d.BPM != 120.0 || d.EffectiveBPM != 120.0 || d.Pitch != 0 {
		t.Errorf("bpm/pitch mismatch: bpm=%f, eff=%f, pitch=%f", d.BPM, d.EffectiveBPM, d.Pitch)
	}
	if d.Beat != 17 || d.BeatInBar != 2 || d.PlayPositionMS != 8000 {
		t.Errorf("position mismatch: beat=%d, beatInBar=%d, pos=%d", d.Beat, d.BeatInBar, d.PlayPositionMS)
	}

	// Test fallback PlayPositionMS calculation when 0xb4 is 0
	binary.BigEndian.PutUint32(pkt[0xb4:], 0)
	d2, err := prolink.ParsePacket(pkt, 50002, ip)
	if err != nil {
		t.Fatal(err)
	}
	// Beat 17 at 120 BPM: (17-1)*60000/120 = 16 * 500 = 8000 ms
	if d2.PlayPositionMS != 8000 {
		t.Errorf("got fallback play position %d, want 8000", d2.PlayPositionMS)
	}
}

func TestExtractANLZ(t *testing.T) {
	// 1. Nil analysis
	wf, beats, dur := prolink.ExtractANLZ(nil)
	if wf != nil || beats != nil || dur != 0 {
		t.Fatalf("expected nil for nil analysis, got wf=%v, beats=%v, dur=%d", wf, beats, dur)
	}

	// 2. PWV7 3-band waveform + Beats
	a := &rekordbox.Analysis{
		Beats: []rekordbox.Beat{
			{Number: 1, BPM: 125.0, TimeMS: 0},
			{Number: 2, BPM: 125.0, TimeMS: 480},
			{Number: 3, BPM: 125.0, TimeMS: 960},
		},
		Waveforms: []rekordbox.Waveform{
			{
				Tag:        "PWV7",
				EntryBytes: 3,
				// 3 entries: each is [mid, high, low] -> [Low, Mid, High]
				Data: []byte{
					10, 20, 30, // mid=10, high=20, low=30 -> [30, 10, 20]
					40, 50, 60, // mid=40, high=50, low=60 -> [60, 40, 50]
					70, 80, 90, // mid=70, high=80, low=90 -> [90, 70, 80]
				},
			},
		},
	}
	wf, beats, dur = prolink.ExtractANLZ(a)
	if dur != 960 {
		t.Errorf("got duration %d, want 960", dur)
	}
	if len(beats) != 3 || beats[2].TimeMS != 960 || beats[1].BPM != 125.0 {
		t.Errorf("beats mismatch: %v", beats)
	}
	if len(wf) != 3 {
		t.Fatalf("got %d waveform columns, want 3", len(wf))
	}
	if wf[0] != [3]byte{30, 10, 20} {
		t.Errorf("col 0: got %v, want [30, 10, 20]", wf[0])
	}
	if wf[1] != [3]byte{60, 40, 50} {
		t.Errorf("col 1: got %v, want [60, 40, 50]", wf[1])
	}

	// 3. Downsampling > 512 beats limits to 512, keeping durationMS of last beat
	manyBeats := make([]rekordbox.Beat, 600)
	for i := range manyBeats {
		manyBeats[i] = rekordbox.Beat{Number: uint16(i + 1), BPM: 130.0, TimeMS: uint32(i * 460)}
	}
	a.Beats = manyBeats
	_, beats, dur = prolink.ExtractANLZ(a)
	if len(beats) != 512 {
		t.Errorf("got %d beats, want 512", len(beats))
	}
	if dur != 599*460 {
		t.Errorf("got duration %d, want %d", dur, 599*460)
	}

	// 4. Downsampling > 256 waveform columns downsamples to 256
	manyWave := make([]byte, 300*3)
	for i := 0; i < 300; i++ {
		manyWave[3*i] = byte(i % 100)
		manyWave[3*i+1] = byte(i % 100)
		manyWave[3*i+2] = byte(i % 100)
	}
	a.Waveforms = []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: manyWave}}
	wf, _, _ = prolink.ExtractANLZ(a)
	if len(wf) != 256 {
		t.Errorf("got %d waveform columns, want 256", len(wf))
	}

	// 5. PWV5 format priority
	a5 := &rekordbox.Analysis{
		Waveforms: []rekordbox.Waveform{
			{
				Tag:        "PWV5",
				EntryBytes: 2,
				Data: func() []byte {
					// 2-byte entry: bits 13..15 R, 10..12 G, 7..9 B, 2..6 height
					// R=7, G=0, B=0, height=31 -> (7<<13) | (0<<10) | (0<<7) | (31<<2) = 0xe07c
					b := make([]byte, 2)
					binary.BigEndian.PutUint16(b, 0xe07c)
					return b
				}(),
			},
		},
	}
	wf5, _, _ := prolink.ExtractANLZ(a5)
	if len(wf5) != 1 {
		t.Fatalf("got %d columns for PWV5, want 1", len(wf5))
	}
	if wf5[0][0] != 255 || wf5[0][1] != 0 || wf5[0][2] != 0 {
		t.Errorf("PWV5 col: got %v, want [255, 0, 0]", wf5[0])
	}
}

func TestDiscoveryObserveWithResolver(t *testing.T) {
	d := prolink.NewDiscovery()
	resolverCalls := 0
	d.SetResolver(func(dev prolink.Device) (*rekordbox.Analysis, string, string) {
		resolverCalls++
		return &rekordbox.Analysis{
			Beats: []rekordbox.Beat{
				{Number: 1, BPM: 124.0, TimeMS: 0},
				{Number: 2, BPM: 124.0, TimeMS: 484},
				{Number: 3, BPM: 124.0, TimeMS: 968},
				{Number: 4, BPM: 124.0, TimeMS: 1452},
			},
			Waveforms: []rekordbox.Waveform{
				{Tag: "PWV7", EntryBytes: 3, Data: []byte{10, 20, 30, 40, 50, 60}},
			},
		}, "Test Title", "Test Artist"
	})

	status := prolink.Device{
		Address:        "192.168.1.100",
		Number:         1,
		PacketType:     10,
		TrackID:        77,
		SourcePlayer:   1,
		SourceSlot:     1,
		Beat:           3,
		PlayPositionMS: 0, // should be set from Beats[3-1].TimeMS = 968
		LastSeen:       time.Now(),
	}

	obs := d.Observe(status)
	if resolverCalls != 1 {
		t.Errorf("got %d resolver calls, want 1", resolverCalls)
	}
	if obs.Title != "Test Title" || obs.Artist != "Test Artist" {
		t.Errorf("metadata mismatch: title=%q, artist=%q", obs.Title, obs.Artist)
	}
	if len(obs.Waveform) != 2 || len(obs.Beats) != 4 {
		t.Errorf("waveform/beats missing: wf=%d, beats=%d", len(obs.Waveform), len(obs.Beats))
	}
	if obs.PlayPositionMS != 968 {
		t.Errorf("got PlayPositionMS %d, want 968", obs.PlayPositionMS)
	}

	// Second observe of same track uses cache (resolverCalls remains 1)
	obs2 := d.Observe(status)
	if resolverCalls != 1 {
		t.Errorf("resolver called again on cached track: %d calls", resolverCalls)
	}
	if obs2.Title != "Test Title" {
		t.Errorf("cached title mismatch: %q", obs2.Title)
	}

	// Subsequent Beat packet (0x28) preserves track metadata
	beatPkt := prolink.Device{
		Address:      "192.168.1.100",
		Number:       1,
		PacketType:   0x28,
		BPM:          126.0,
		EffectiveBPM: 126.0,
		LastSeen:     time.Now(),
	}
	obs3 := d.Observe(beatPkt)
	if obs3.TrackID != 77 || obs3.Title != "Test Title" || len(obs3.Waveform) != 2 {
		t.Errorf("beat packet observe lost track metadata: %+v", obs3)
	}
	if obs3.BPM != 126.0 {
		t.Errorf("beat packet did not update BPM: %f", obs3.BPM)
	}

	// Subsequent Announcement packet (6) preserves all status & track metadata
	annPkt := prolink.Device{
		Address:    "192.168.1.100",
		Number:     1,
		PacketType: 6,
		LastSeen:   time.Now(),
	}
	obs4 := d.Observe(annPkt)
	if obs4.TrackID != 77 || obs4.Title != "Test Title" || obs4.BPM != 126.0 {
		t.Errorf("announcement observe lost fields: %+v", obs4)
	}
}

func TestDiscoveryDefaultLookup(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)

	d := prolink.NewDiscovery()
	d.SetANLZRoot(root)

	status := prolink.Device{
		Address:      "192.168.1.50",
		Number:       1,
		PacketType:   10,
		TrackID:      1, // Track 1 is in export.pdb and has ANLZ0000.DAT/.EXT/.2EX
		SourcePlayer: 1,
		SourceSlot:   1,
		LastSeen:     time.Now(),
	}

	obs := d.Observe(status)
	if obs.Title == "" {
		t.Error("expected non-empty title from default lookup")
	}
	if len(obs.Waveform) == 0 {
		t.Error("expected waveform to be resolved from ANLZ files")
	}
	if len(obs.Beats) == 0 {
		t.Error("expected beats to be resolved from ANLZ files")
	}
}

func TestBuildSnapshot(t *testing.T) {
	now := time.Now()
	d1 := prolink.Device{Number: 1, Name: "CDJ-3000", LastSeen: now, BPM: 128.0}
	d2 := prolink.Device{Number: 2, Name: "CDJ-3000", LastSeen: now, BPM: 125.0}

	// 2 decks: 1 & 2 -> 3 mirrors 1, 4 mirrors 2
	snap := prolink.BuildSnapshot([]prolink.Device{d1, d2})
	if len(snap.Devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(snap.Devices))
	}
	if snap.Decks["1"].Number != 1 || snap.Decks["2"].Number != 2 {
		t.Errorf("deck 1 or 2 mismatch: %+v", snap.Decks)
	}
	if snap.Decks["3"].Number != 1 {
		t.Errorf("deck 3 did not mirror deck 1: %+v", snap.Decks["3"])
	}
	if snap.Decks["4"].Number != 2 {
		t.Errorf("deck 4 did not mirror deck 2: %+v", snap.Decks["4"])
	}

	// 4 decks: 1, 2, 3, 4 -> each keeps its own
	d3 := prolink.Device{Number: 3, Name: "CDJ-3000", LastSeen: now, BPM: 130.0}
	d4 := prolink.Device{Number: 4, Name: "CDJ-3000", LastSeen: now, BPM: 132.0}
	snap4 := prolink.BuildSnapshot([]prolink.Device{d1, d2, d3, d4})
	if snap4.Decks["3"].Number != 3 || snap4.Decks["4"].Number != 4 {
		t.Errorf("decks 3 or 4 overwritten when explicitly present: %+v", snap4.Decks)
	}

	// Inactive device (>10s) filtered out
	expired := prolink.Device{Number: 1, LastSeen: now.Add(-15 * time.Second)}
	snapExpired := prolink.BuildSnapshot([]prolink.Device{expired})
	if len(snapExpired.Devices) != 0 || len(snapExpired.Decks) != 0 {
		t.Errorf("expired device not filtered: %+v", snapExpired)
	}
}

func TestListenContextCancellationAndPort50001Busy(t *testing.T) {
	// Occupy port 50001 to simulate busy beat port
	c50001, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50001})
	if err != nil {
		t.Skip("cannot bind port 50001 in test environment")
	}
	defer c50001.Close()

	d := prolink.NewDiscovery()
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately after starting
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err = d.Listen(ctx, net.IPv4(127, 0, 0, 1), nil)
	if err != nil && err != context.Canceled {
		t.Fatalf("unexpected listen error: %v", err)
	}
}
