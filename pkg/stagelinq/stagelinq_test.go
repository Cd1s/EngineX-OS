package stagelinq

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestDiscoveryEncodeDecodeAndMixerFilter(t *testing.T) {
	pkt := DiscoveryPacket{
		Token:           [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Source:          "X1850 PRIME",
		Action:          ActionHowdy,
		SoftwareName:    "StageLinQ Mixer",
		SoftwareVersion: "1.6.0",
		Port:            54321,
	}
	raw, err := EncodeDiscovery(pkt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeDiscovery(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != pkt {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, pkt)
	}
	if !IsMixerDevice(got) {
		t.Fatal("expected X1850 PRIME to match IsMixerDevice")
	}
	player := DiscoveryPacket{Source: "SC6000 PRIME", Action: ActionHowdy, SoftwareName: "JP13"}
	if IsMixerDevice(player) {
		t.Fatal("SC6000 player must not match IsMixerDevice")
	}
}

func TestStageLinQMonitorEndToEndX1850StateMap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Mock StateMap server
	smLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer smLn.Close()
	smPort := uint16(smLn.Addr().(*net.TCPAddr).Port)

	emitCh := make(chan [2]string, 16)
	go func() {
		conn, err := smLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read client's initial serviceAnnouncement ("StateMap", localPort)
		var msgID uint32
		if binary.Read(conn, binary.BigEndian, &msgID) != nil || msgID != msgServiceAnnounce {
			return
		}
		var tok [16]byte
		_, _ = io.ReadFull(conn, tok[:])
		_, _ = readUTF16BEString(conn)
		var cport uint16
		_ = binary.Read(conn, binary.BigEndian, &cport)

		// Drain 10 subscribe frames
		for i := 0; i < 10; i++ {
			var ln uint32
			if binary.Read(conn, binary.BigEndian, &ln) != nil {
				return
			}
			_, _ = io.ReadFull(conn, make([]byte, ln))
		}
		for {
			select {
			case <-ctx.Done():
				return
			case pair := <-emitCh:
				if WriteSmaaEmit(conn, pair[0], pair[1]) != nil {
					return
				}
			}
		}
	}()

	// Mock Directory server
	dirLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dirLn.Close()
	dirPort := uint16(dirLn.Addr().(*net.TCPAddr).Port)
	mixerTok := [16]byte{9, 9, 9, 9}

	go func() {
		conn, err := dirLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var msgID uint32
		if binary.Read(conn, binary.BigEndian, &msgID) != nil || msgID != msgServicesRequest {
			return
		}
		var ctok [16]byte
		_, _ = io.ReadFull(conn, ctok[:])

		// Reply with StateMap service port
		_ = binary.Write(conn, binary.BigEndian, msgServiceAnnounce)
		_, _ = conn.Write(mixerTok[:])
		_ = writeUTF16BEString(conn, "StateMap")
		_ = binary.Write(conn, binary.BigEndian, smPort)
		<-ctx.Done()
	}()

	mon := NewMonitor()
	mon.ObserveDiscovery(ctx, "127.0.0.1", DiscoveryPacket{
		Token:        mixerTok,
		Source:       "Denon DJ X1850",
		Action:       ActionHowdy,
		SoftwareName: "X1850",
		Port:         dirPort,
	})

	emitCh <- [2]string{"/Mixer/CH1faderPosition", `{"type":1,"value":0.92}`}
	emitCh <- [2]string{"/Mixer/CH2faderPosition", `{"type":1,"value":0.0}`}
	emitCh <- [2]string{"/Mixer/ChannelAssignment1", `{"string":"A"}`}
	emitCh <- [2]string{"/Mixer/ChannelAssignment3", `{"string":"B"}`}
	emitCh <- [2]string{"/Mixer/CH3faderPosition", `{"type":1,"value":0.85}`}
	emitCh <- [2]string{"/Mixer/CrossfaderPosition", `{"type":1,"value":0.0}`} // hard Left (Side A): cuts Side B (CH3)!

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		on1, k1 := mon.ChannelOnAir(1, time.Now())
		on2, k2 := mon.ChannelOnAir(2, time.Now())
		on3, k3 := mon.ChannelOnAir(3, time.Now())
		if k1 && k2 && k3 && on1 && !on2 && !on3 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	now := time.Now()
	if on1, k1 := mon.ChannelOnAir(1, now); !k1 || !on1 {
		t.Fatalf("CH1 expected on-air, got on=%v known=%v", on1, k1)
	}
	if on2, k2 := mon.ChannelOnAir(2, now); !k2 || on2 {
		t.Fatalf("CH2 (fader 0) expected off-air, got on=%v known=%v", on2, k2)
	}
	if on3, k3 := mon.ChannelOnAir(3, now); !k3 || on3 {
		t.Fatalf("CH3 (assigned B while crossfader=0.0 A) expected cut, got on=%v known=%v", on3, k3)
	}

	// Move crossfader to center -> CH3 becomes audible!
	emitCh <- [2]string{"/Mixer/CrossfaderPosition", `{"type":1,"value":0.5}`}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if on3, _ := mon.ChannelOnAir(3, time.Now()); on3 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if on3, _ := mon.ChannelOnAir(3, time.Now()); !on3 {
		t.Fatal("CH3 expected on-air after crossfader moved to center")
	}

	// DISCOVERER_EXIT_ disconnects cleanly
	mon.ObserveDiscovery(ctx, "127.0.0.1", DiscoveryPacket{
		Token:  mixerTok,
		Source: "Denon DJ X1850",
		Action: ActionExit,
	})
	if _, known := mon.ChannelOnAir(1, time.Now()); known {
		t.Fatal("expected mixer state unknown after DISCOVERER_EXIT_")
	}
}
