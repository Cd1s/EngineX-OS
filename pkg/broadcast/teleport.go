package broadcast

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Teleport magic constants
const (
	TeleportMagicTLPT = "TLPT"
	TeleportMagicOMTA = "OMTA"
)

// TeleportBeacon is broadcast over UDP to announce audio stream availability to OBS and LAN receivers.
type TeleportBeacon struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Format     string `json:"format"`
	Port       int    `json:"port"`
}

// LANConfig specifies settings for UDP low-latency LAN streaming.
type LANConfig struct {
	Mode           string        `json:"mode"`           // "off", "rtp", "teleport"
	Target         string        `json:"target"`         // e.g. "192.168.1.50:9998" or "239.255.0.1:5004"
	SampleRate     int           `json:"sampleRate"`     // default 48000
	Channels       int           `json:"channels"`       // default 2
	PayloadType    int           `json:"payloadType"`    // default 96 (or 10 for RFC 3551 L16)
	SSRC           uint32        `json:"ssrc"`           // default 0x53433530 ("SC50")
	BeaconPort     int           `json:"beaconPort"`     // default 0 (uses Target's port)
	BeaconInterval time.Duration `json:"beaconInterval"` // default 1s
}

// RTPPacket represents a parsed RFC 3550 RTP packet.
type RTPPacket struct {
	Version        uint8
	PayloadType    uint8
	SequenceNumber uint16
	Timestamp      uint32
	SSRC           uint32
	Payload        []byte
}

// ParseRTPPacket decodes a 12-byte RFC 3550 header and payload.
func ParseRTPPacket(data []byte) (*RTPPacket, error) {
	if len(data) < 12 {
		return nil, errors.New("data too short for RTP packet")
	}
	v := (data[0] >> 6) & 0x03
	pt := data[1] & 0x7F
	seq := binary.BigEndian.Uint16(data[2:4])
	ts := binary.BigEndian.Uint32(data[4:8])
	ssrc := binary.BigEndian.Uint32(data[8:12])
	return &RTPPacket{
		Version:        v,
		PayloadType:    pt,
		SequenceNumber: seq,
		Timestamp:      ts,
		SSRC:           ssrc,
		Payload:        data[12:],
	}, nil
}

// TeleportFrame represents a parsed OBS Teleport / OMT audio frame.
type TeleportFrame struct {
	Magic      string
	SampleRate uint32
	Channels   uint16
	Format     uint16
	Timestamp  uint64
	DataLen    uint32
	Payload    []byte
}

// ParseTeleportFrame decodes a 24-byte Teleport/OMT header and payload.
func ParseTeleportFrame(data []byte) (*TeleportFrame, error) {
	if len(data) < 24 {
		return nil, errors.New("data too short for Teleport frame")
	}
	magic := string(data[0:4])
	if magic != TeleportMagicTLPT && magic != TeleportMagicOMTA {
		return nil, fmt.Errorf("invalid magic: %q", magic)
	}
	sr := binary.BigEndian.Uint32(data[4:8])
	ch := binary.BigEndian.Uint16(data[8:10])
	fmtCode := binary.BigEndian.Uint16(data[10:12])
	ts := binary.BigEndian.Uint64(data[12:20])
	dataLen := binary.BigEndian.Uint32(data[20:24])
	if len(data) < 24+int(dataLen) {
		return nil, errors.New("truncated Teleport payload")
	}
	return &TeleportFrame{
		Magic:      magic,
		SampleRate: sr,
		Channels:   ch,
		Format:     fmtCode,
		Timestamp:  ts,
		DataLen:    dataLen,
		Payload:    data[24 : 24+dataLen],
	}, nil
}

// LANStreamer manages low-latency UDP streaming via RTP L16 or OBS Teleport.
type LANStreamer struct {
	mu        sync.Mutex
	cfg       LANConfig
	conn      net.Conn
	targetUDP *net.UDPAddr
	seq       uint16
	timestamp uint32
	running   bool
	stopCh    chan struct{}
	bytesSent atomic.Int64
	lastTry   time.Time // of opening the socket
	err       string    // why the socket is not open; "" once it is
}

// dialUDP opens the stream's socket; a test stands in for a network that is not up yet.
var dialUDP = func(target *net.UDPAddr) (net.Conn, error) { return net.DialUDP("udp", nil, target) }

// lanRetryEvery is how often Push tries again to open a socket that could not be opened.
var lanRetryEvery = time.Second

// NewLANStreamer creates a new LANStreamer with validated configuration.
func NewLANStreamer(cfg LANConfig) (*LANStreamer, error) {
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode == "" {
		cfg.Mode = "off"
	}
	if cfg.Mode != "off" && cfg.Mode != "rtp" && cfg.Mode != "teleport" {
		return nil, fmt.Errorf("invalid lanMode: %q (must be 'off', 'rtp', or 'teleport')", cfg.Mode)
	}

	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 48000
	}
	if cfg.Channels <= 0 {
		cfg.Channels = 2
	}
	if cfg.PayloadType == 0 {
		cfg.PayloadType = 96
	}
	if cfg.SSRC == 0 {
		cfg.SSRC = 0x53433530 // "SC50"
	}
	if cfg.BeaconInterval <= 0 {
		cfg.BeaconInterval = 1 * time.Second
	}

	var targetUDP *net.UDPAddr
	if cfg.Target != "" && cfg.Mode != "off" {
		var err error
		targetUDP, err = net.ResolveUDPAddr("udp", cfg.Target)
		if err != nil {
			return nil, fmt.Errorf("resolve udp target %q: %w", cfg.Target, err)
		}
	}

	return &LANStreamer{
		cfg:       cfg,
		targetUDP: targetUDP,
	}, nil
}

// Start begins network transport and periodic Teleport beacons if enabled.
func (s *LANStreamer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}
	s.running = true
	if s.cfg.Mode == "off" || s.targetUDP == nil {
		return nil
	}
	s.stopCh = make(chan struct{})
	if s.cfg.Mode == "teleport" {
		go s.beaconLoop(s.stopCh)
	}
	return s.connectLocked()
}

// connectLocked opens the socket. While the network is down (a deck that boots with
// Live Stream on, before its address arrives) this fails with "network is
// unreachable"; the streamer stays started and Push tries again. BH2-A3-6: Start
// gave up for good and the stream stayed dead until the settings were saved again.
func (s *LANStreamer) connectLocked() error {
	s.lastTry = time.Now()
	conn, err := dialUDP(s.targetUDP)
	if err != nil {
		err = fmt.Errorf("dial udp %s: %w", s.targetUDP.String(), err)
		s.err = err.Error()
		return err
	}
	s.conn, s.err = conn, ""
	return nil
}

// Connected reports whether the stream has a socket to send on.
func (s *LANStreamer) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// Err is why the stream is not sending ("" while it is, or when it is off).
func (s *LANStreamer) Err() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// beaconLoop periodically broadcasts the discovery beacon.
func (s *LANStreamer) beaconLoop(stopCh <-chan struct{}) {
	ticker := time.NewTicker(s.cfg.BeaconInterval)
	defer ticker.Stop()

	// Initial immediate beacon
	_ = s.SendBeacon()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			_ = s.SendBeacon()
		}
	}
}

// SendBeacon broadcasts or transmits a single JSON discovery beacon.
func (s *LANStreamer) SendBeacon() error {
	s.mu.Lock()
	conn := s.conn
	port := 0
	if s.targetUDP != nil {
		port = s.targetUDP.Port
	}
	if s.cfg.BeaconPort > 0 {
		port = s.cfg.BeaconPort
	}
	sampleRate := s.cfg.SampleRate
	channels := s.cfg.Channels
	s.mu.Unlock()

	if conn == nil {
		return nil
	}

	b := TeleportBeacon{
		Name:       "EngineX OS Master",
		Type:       "audio",
		SampleRate: sampleRate,
		Channels:   channels,
		Format:     "s16le",
		Port:       port,
	}

	data, err := json.Marshal(b)
	if err != nil {
		return err
	}

	_, err = conn.Write(data)
	return err
}

// Push encapsulates and transmits one chunk of PCM data (48kHz, 2ch, S16LE).
func (s *LANStreamer) Push(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running || len(pcm) == 0 {
		return nil
	}
	if s.conn == nil {
		if s.cfg.Mode == "off" || s.targetUDP == nil || time.Since(s.lastTry) < lanRetryEvery {
			return nil
		}
		if err := s.connectLocked(); err != nil {
			return err
		}
	}

	// Calculate sample count (stereo 16-bit = 4 bytes per sample frame)
	sampleBytes := s.cfg.Channels * 2
	if sampleBytes == 0 {
		sampleBytes = 4
	}

	// One datagram per chunk below the 1500-byte MTU, so Wi-Fi never sees IP
	// fragments. BH2-A3-4: only RTP did this. A Teleport frame carried whatever
	// the capture loop handed over, 9 KB at a time and up to 48 KB after a
	// pause; such a datagram leaves in up to 33 fragments and is lost whole
	// when one of them is.
	chunk := (1344 / sampleBytes) * sampleBytes
	var firstErr error
	for off := 0; off < len(pcm); off += chunk {
		part := pcm[off:min(off+chunk, len(pcm))]
		var packet []byte
		switch s.cfg.Mode {
		case "rtp":
			// RFC 3550 12-byte RTP header
			packet = make([]byte, 12+len(part))
			packet[0] = 0x80 // V=2, P=0, X=0, CC=0
			packet[1] = byte(s.cfg.PayloadType & 0x7F)
			binary.BigEndian.PutUint16(packet[2:4], s.seq)
			binary.BigEndian.PutUint32(packet[4:8], s.timestamp)
			binary.BigEndian.PutUint32(packet[8:12], s.cfg.SSRC)
			copy(packet[12:], part)
			s.seq++
		case "teleport":
			// 24-byte Teleport/OMT header:
			// Magic (4B) + SampleRate (4B) + Channels (2B) + Format (2B) + Timestamp (8B) + DataLen (4B)
			packet = make([]byte, 24+len(part))
			copy(packet[0:4], TeleportMagicTLPT)
			binary.BigEndian.PutUint32(packet[4:8], uint32(s.cfg.SampleRate))
			binary.BigEndian.PutUint16(packet[8:10], uint16(s.cfg.Channels))
			binary.BigEndian.PutUint16(packet[10:12], 1) // 1 = s16le
			binary.BigEndian.PutUint64(packet[12:20], uint64(s.timestamp))
			binary.BigEndian.PutUint32(packet[20:24], uint32(len(part)))
			copy(packet[24:], part)
		default:
			return nil
		}
		s.timestamp += uint32(len(part) / sampleBytes)
		n, err := s.conn.Write(packet)
		if n > 0 {
			s.bytesSent.Add(int64(n))
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// BytesSent returns total network bytes transmitted.
func (s *LANStreamer) BytesSent() int64 {
	return s.bytesSent.Load()
}

// Close shuts down transport and stops background beacon routines.
func (s *LANStreamer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}
	s.running = false

	if s.stopCh != nil {
		close(s.stopCh)
		s.stopCh = nil
	}
	if s.conn != nil {
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}
