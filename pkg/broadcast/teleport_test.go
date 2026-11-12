package broadcast

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
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

// BH2-A3-6: a LAN target that cannot be used was dropped without a word: Status
// said "streaming" with no error and nothing was ever sent.
func TestLANStreamBadTargetIsReported(t *testing.T) {
	b := New(newTestGuard())
	defer b.Close()
	cfg := Config{Enabled: true, LANMode: "rtp", LANTarget: "studio-pc", InputPath: filepath.Join(t.TempDir(), "none")}
	if err := b.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); !strings.Contains(st.LastError, "LAN stream") || !strings.Contains(st.LastError, "studio-pc") {
		t.Fatalf("a LAN target without a port: lastError=%q, want the reason", st.LastError)
	}
	// With an external server that is not there either, neither stream is up and both say why.
	cfg.ServerURL, cfg.Password = "127.0.0.1:1", "pw"
	if err := b.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(b.Status().LastError, "127.0.0.1:1") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := b.Status(); st.Streaming || !strings.Contains(st.LastError, "127.0.0.1:1") || !strings.Contains(st.LastError, "studio-pc") {
		t.Fatalf("no server and no LAN target: streaming=%v lastError=%q, want not streaming and both reasons", st.Streaming, st.LastError)
	}
	// Switched off, the stale reason goes.
	cfg.ServerURL, cfg.LANMode = "", "off"
	if err := b.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); strings.Contains(st.LastError, "LAN stream") {
		t.Fatalf("LAN streaming off: lastError=%q", st.LastError)
	}
}

// BH2-A3-6: a deck that boots with Live Stream on opens the LAN socket before the
// network is up; that failed with "network is unreachable" and was never tried again.
func TestLANStreamStartsWhenTheNetworkComesUp(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var down atomic.Bool
	down.Store(true)
	realDial, realRetry := dialUDP, lanRetryEvery
	t.Cleanup(func() { dialUDP, lanRetryEvery = realDial, realRetry })
	dialUDP = func(target *net.UDPAddr) (net.Conn, error) {
		if down.Load() {
			return nil, &net.OpError{Op: "dial", Net: "udp", Addr: target, Err: syscall.ENETUNREACH}
		}
		return realDial(target)
	}
	lanRetryEvery = 20 * time.Millisecond

	pcm := filepath.Join(t.TempDir(), "live.pcm")
	if err := os.WriteFile(pcm, bytes.Repeat([]byte{1, 2, 3, 4}, 1152), 0644); err != nil {
		t.Fatal(err)
	}
	b := New(newTestGuard())
	t.Cleanup(b.Close) // before the dialer is put back
	if err := b.Configure(Config{Enabled: true, LANMode: "rtp", LANTarget: ln.LocalAddr().String(), InputPath: pcm}); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); !strings.Contains(st.LastError, "network is unreachable") {
		t.Fatalf("network down: lastError=%q, want the reason", st.LastError)
	}
	buf := make([]byte, 8192)
	_ = ln.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, err := ln.ReadFrom(buf); err == nil {
		t.Fatalf("got %d bytes while the network is down", n)
	}
	down.Store(false)
	_ = ln.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := ln.ReadFrom(buf)
	if err != nil {
		t.Fatalf("the network is up and no audio arrives: %v", err)
	}
	if pkt, err := ParseRTPPacket(buf[:n]); err != nil || len(pkt.Payload) == 0 {
		t.Fatalf("not an RTP audio packet: %v", err)
	}
	if st := b.Status(); strings.Contains(st.LastError, "LAN stream") {
		t.Fatalf("stream is up: lastError=%q", st.LastError)
	}
}

// BH2-A3-10: the sent-bytes figure of the status stayed 0 for a LAN-only stream.
func TestLANStreamCountsSentBytes(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	pcm := filepath.Join(t.TempDir(), "live.pcm")
	if err := os.WriteFile(pcm, bytes.Repeat([]byte{1, 2, 3, 4}, 1152), 0644); err != nil {
		t.Fatal(err)
	}
	b := New(newTestGuard())
	defer b.Close()
	if err := b.Configure(Config{Enabled: true, LANMode: "rtp", LANTarget: ln.LocalAddr().String(), InputPath: pcm}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8192)
	_ = ln.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := ln.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if sent := b.Status().BytesSent; sent < int64(n) {
		t.Fatalf("a %d-byte packet arrived and the status counts %d bytes sent", n, sent)
	}
}

// BH2-A3-4: a Teleport frame was one datagram however much audio it carried: 9 KB
// from one pass of the capture loop, far above the MTU.
func TestLANStreamerTeleportChunksUnderMTU(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// A beacon interval beyond the test: only audio frames arrive after the first beacon.
	streamer, err := NewLANStreamer(LANConfig{Mode: "teleport", Target: ln.LocalAddr().String(), SampleRate: 48000, Channels: 2, BeaconInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := streamer.Start(); err != nil {
		t.Fatal(err)
	}
	defer streamer.Close()

	// Two passes of the capture loop: 2304 frames (9216 B), then 240.
	first := make([]byte, 2304*4)
	for i := range first {
		first[i] = byte(i / 4)
	}
	if err := streamer.Push(first); err != nil {
		t.Fatal(err)
	}
	if err := streamer.Push(make([]byte, 240*4)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 65536)
	var got []byte
	var next uint64
	for frames := 0; next < 2304+240; {
		_ = ln.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := ln.ReadFrom(buf)
		if err != nil {
			t.Fatalf("after %d frames (%d samples): %v", frames, next, err)
		}
		if n > 0 && buf[0] == '{' {
			continue // the beacon
		}
		if n > 1472 {
			t.Fatalf("a %d-byte datagram does not fit a 1500-byte MTU", n)
		}
		f, err := ParseTeleportFrame(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		if f.Timestamp != next || int(f.DataLen) != len(f.Payload) || len(f.Payload)%4 != 0 {
			t.Fatalf("frame %d: timestamp %d (want %d), dataLen %d, payload %d", frames, f.Timestamp, next, f.DataLen, len(f.Payload))
		}
		next += uint64(len(f.Payload) / 4)
		got = append(got, f.Payload...)
		frames++
	}
	if !bytes.Equal(got[:len(first)], first) || len(got) != len(first)+240*4 {
		t.Fatalf("the audio of the frames is not what was pushed (%d bytes)", len(got))
	}
}
