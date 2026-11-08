package broadcast

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestLANStreamerRTP(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("failed to listen UDP: %v", err)
	}
	defer ln.Close()

	streamer, err := NewLANStreamer(LANConfig{
		Mode:        "rtp",
		Target:      ln.LocalAddr().String(),
		SampleRate:  48000,
		Channels:    2,
		PayloadType: 96,
		SSRC:        0x12345678,
	})
	if err != nil {
		t.Fatalf("NewLANStreamer failed: %v", err)
	}

	if err := streamer.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer streamer.Close()

	// 336 stereo 16-bit samples = 1344 bytes = exactly one MTU-safe RTP chunk
	pcm1 := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 336)
	pcm2 := bytes.Repeat([]byte{0x05, 0x06, 0x07, 0x08}, 336)

	if err := streamer.Push(pcm1); err != nil {
		t.Fatalf("Push 1 failed: %v", err)
	}
	if err := streamer.Push(pcm2); err != nil {
		t.Fatalf("Push 2 failed: %v", err)
	}

	buf := make([]byte, 4096)

	// Read packet 1
	_ = ln.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := ln.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom packet 1 error: %v", err)
	}
	pkt1, err := ParseRTPPacket(buf[:n])
	if err != nil {
		t.Fatalf("ParseRTPPacket 1 error: %v", err)
	}
	if pkt1.Version != 2 {
		t.Errorf("expected version 2, got %d", pkt1.Version)
	}
	if pkt1.PayloadType != 96 {
		t.Errorf("expected payload type 96, got %d", pkt1.PayloadType)
	}
	if pkt1.SequenceNumber != 0 {
		t.Errorf("expected seq 0, got %d", pkt1.SequenceNumber)
	}
	if pkt1.Timestamp != 0 {
		t.Errorf("expected ts 0, got %d", pkt1.Timestamp)
	}
	if pkt1.SSRC != 0x12345678 {
		t.Errorf("expected SSRC 0x12345678, got 0x%X", pkt1.SSRC)
	}
	if !bytes.Equal(pkt1.Payload, pcm1) {
		t.Errorf("packet 1 payload mismatch")
	}

	// Read packet 2
	_ = ln.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err = ln.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom packet 2 error: %v", err)
	}
	pkt2, err := ParseRTPPacket(buf[:n])
	if err != nil {
		t.Fatalf("ParseRTPPacket 2 error: %v", err)
	}
	if pkt2.SequenceNumber != 1 {
		t.Errorf("expected seq 1, got %d", pkt2.SequenceNumber)
	}
	if pkt2.Timestamp != 336 {
		t.Errorf("expected ts 336, got %d", pkt2.Timestamp)
	}
	if !bytes.Equal(pkt2.Payload, pcm2) {
		t.Errorf("packet 2 payload mismatch")
	}

	if streamer.BytesSent() <= 0 {
		t.Errorf("expected BytesSent > 0, got %d", streamer.BytesSent())
	}
}

func TestLANStreamerRTPChunksUnderMTU(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	streamer, err := NewLANStreamer(LANConfig{Mode: "rtp", Target: ln.LocalAddr().String(), SampleRate: 48000, Channels: 2, PayloadType: 96})
	if err != nil {
		t.Fatal(err)
	}
	if err := streamer.Start(); err != nil {
		t.Fatal(err)
	}
	defer streamer.Close()

	// 1152 stereo frames (4608 B) must become 4 packets: 336+336+336+144 frames.
	if err := streamer.Push(make([]byte, 1152*4)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	wantTS := []uint32{0, 336, 672, 1008}
	wantLen := []int{1344, 1344, 1344, 576}
	for i := range wantTS {
		_ = ln.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := ln.ReadFrom(buf)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		pkt, err := ParseRTPPacket(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		if int(pkt.SequenceNumber) != i || pkt.Timestamp != wantTS[i] || len(pkt.Payload) != wantLen[i] {
			t.Errorf("packet %d: seq=%d ts=%d len=%d", i, pkt.SequenceNumber, pkt.Timestamp, len(pkt.Payload))
		}
	}
}

func TestLANStreamerTeleport(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("failed to listen UDP: %v", err)
	}
	defer ln.Close()

	streamer, err := NewLANStreamer(LANConfig{
		Mode:           "teleport",
		Target:         ln.LocalAddr().String(),
		SampleRate:     48000,
		Channels:       2,
		BeaconInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewLANStreamer failed: %v", err)
	}

	if err := streamer.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer streamer.Close()

	buf := make([]byte, 4096)
	var (
		gotBeacon bool
		gotFrame  bool
	)

	// Send an audio frame
	pcm := bytes.Repeat([]byte{0xAA, 0xBB, 0xCC, 0xDD}, 240) // 240 samples
	if err := streamer.Push(pcm); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// Read packets until we receive both beacon and audio frame
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (!gotBeacon || !gotFrame) {
		_ = ln.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := ln.ReadFrom(buf)
		if err != nil {
			continue
		}
		data := buf[:n]

		// Check if JSON beacon
		var beacon TeleportBeacon
		if json.Unmarshal(data, &beacon) == nil && beacon.Name == "EngineX OS Master" {
			gotBeacon = true
			if beacon.Type != "audio" {
				t.Errorf("expected type 'audio', got %q", beacon.Type)
			}
			if beacon.SampleRate != 48000 {
				t.Errorf("expected sample_rate 48000, got %d", beacon.SampleRate)
			}
			if beacon.Channels != 2 {
				t.Errorf("expected channels 2, got %d", beacon.Channels)
			}
			if beacon.Format != "s16le" {
				t.Errorf("expected format 's16le', got %q", beacon.Format)
			}
			continue
		}

		// Check if Teleport audio frame
		if frame, err := ParseTeleportFrame(data); err == nil {
			gotFrame = true
			if frame.Magic != TeleportMagicTLPT {
				t.Errorf("expected magic TLPT, got %q", frame.Magic)
			}
			if frame.SampleRate != 48000 {
				t.Errorf("expected sample rate 48000, got %d", frame.SampleRate)
			}
			if frame.Channels != 2 {
				t.Errorf("expected channels 2, got %d", frame.Channels)
			}
			if frame.DataLen != uint32(len(pcm)) {
				t.Errorf("expected data len %d, got %d", len(pcm), frame.DataLen)
			}
			if !bytes.Equal(frame.Payload, pcm) {
				t.Errorf("audio payload mismatch")
			}
		}
	}

	if !gotBeacon {
		t.Errorf("did not receive Teleport beacon")
	}
	if !gotFrame {
		t.Errorf("did not receive Teleport audio frame")
	}
}

func TestLANStreamerValidation(t *testing.T) {
	// Invalid mode
	_, err := NewLANStreamer(LANConfig{Mode: "invalid"})
	if err == nil {
		t.Errorf("expected error for invalid mode")
	}

	// Mode "off" should succeed without target
	s, err := NewLANStreamer(LANConfig{Mode: "off"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unexpected Start error: %v", err)
	}
	if err := s.Push([]byte{1, 2, 3, 4}); err != nil {
		t.Errorf("unexpected Push error: %v", err)
	}
	_ = s.Close()
}
