// Package stagelinq monitors Denon DJ X1800 / X1850 external mixers over the
// StageLinQ LAN protocol (UDP 51337 "airD" discovery + TCP Directory + "smaa"
// StateMap subscriptions) to determine per-channel audible / On-Air state on
// standalone players (#199).
package stagelinq

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const (
	DiscoveryPort = 51337

	ActionHowdy = "DISCOVERER_HOWDY_"
	ActionExit  = "DISCOVERER_EXIT_"

	msgServiceAnnounce uint32 = 0x00000000
	msgReference       uint32 = 0x00000001
	msgServicesRequest uint32 = 0x00000002

	smaaEmit              uint32 = 0x00000000
	smaaSubscribeResponse uint32 = 0x000007d1
	smaaSubscribe         uint32 = 0x000007d2

	// Crossfader assignment constants on X1800 / X1850 mixers.
	AssignThru = 0
	AssignA    = 1
	AssignB    = 2

	FaderAudibleThreshold = 0.05
)

var (
	airDMagic = []byte("airD")
	smaaMagic = []byte("smaa")
)

// DiscoveryPacket represents a StageLinQ UDP 51337 announcement.
type DiscoveryPacket struct {
	Token           [16]byte
	Source          string
	Action          string
	SoftwareName    string
	SoftwareVersion string
	Port            uint16
}

func writeUTF16BEString(w io.Writer, s string) error {
	u16 := utf16.Encode([]rune(s))
	if err := binary.Write(w, binary.BigEndian, uint32(len(u16)*2)); err != nil {
		return err
	}
	for _, v := range u16 {
		if err := binary.Write(w, binary.BigEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func readUTF16BEString(r io.Reader) (string, error) {
	var byteLen uint32
	if err := binary.Read(r, binary.BigEndian, &byteLen); err != nil {
		return "", err
	}
	if byteLen > 65536 || byteLen%2 != 0 {
		return "", errors.New("stagelinq: invalid UTF-16 string length")
	}
	raw := make([]byte, byteLen)
	if _, err := io.ReadFull(r, raw); err != nil {
		return "", err
	}
	u16 := make([]uint16, byteLen/2)
	for i := range u16 {
		u16[i] = binary.BigEndian.Uint16(raw[i*2:])
	}
	return string(utf16.Decode(u16)), nil
}

// EncodeDiscovery serializes a StageLinQ UDP discovery packet.
func EncodeDiscovery(p DiscoveryPacket) ([]byte, error) {
	var b bytes.Buffer
	b.Write(airDMagic)
	b.Write(p.Token[:])
	for _, s := range []string{p.Source, p.Action, p.SoftwareName, p.SoftwareVersion} {
		if err := writeUTF16BEString(&b, s); err != nil {
			return nil, err
		}
	}
	if err := binary.Write(&b, binary.BigEndian, p.Port); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// DecodeDiscovery parses a StageLinQ UDP discovery packet.
func DecodeDiscovery(raw []byte) (DiscoveryPacket, error) {
	var p DiscoveryPacket
	if len(raw) < 22 || !bytes.Equal(raw[:4], airDMagic) {
		return p, errors.New("stagelinq: not an airD discovery packet")
	}
	r := bytes.NewReader(raw[4:])
	if _, err := io.ReadFull(r, p.Token[:]); err != nil {
		return p, err
	}
	var err error
	if p.Source, err = readUTF16BEString(r); err != nil {
		return p, err
	}
	if p.Action, err = readUTF16BEString(r); err != nil {
		return p, err
	}
	if p.SoftwareName, err = readUTF16BEString(r); err != nil {
		return p, err
	}
	if p.SoftwareVersion, err = readUTF16BEString(r); err != nil {
		return p, err
	}
	if err = binary.Read(r, binary.BigEndian, &p.Port); err != nil {
		return p, err
	}
	return p, nil
}

// IsMixerDevice reports whether a StageLinQ discovery packet identifies an
// external mixer (such as Denon DJ X1800 Prime or X1850 Prime).
func IsMixerDevice(p DiscoveryPacket) bool {
	hay := strings.ToLower(p.Source + " " + p.SoftwareName)
	for _, kw := range []string{"x1800", "x1850", "mixer", "rmx"} {
		if strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

// MixerState holds the latest external mixer fader and crossfader positions.
type MixerState struct {
	Connected   bool       `json:"connected"`
	DeviceName  string     `json:"deviceName,omitempty"`
	Address     string     `json:"address,omitempty"`
	LastSeen    time.Time  `json:"lastSeen,omitempty"`
	Channels    int        `json:"channels"`
	Fader       [4]float64 `json:"fader"`
	Assign      [4]int     `json:"assign"` // 0=Thru, 1=A (left), 2=B (right)
	Crossfader  float64    `json:"crossfader"`
	FaderKnown  [4]bool    `json:"-"`
	XfaderKnown bool       `json:"-"`
}

// ChannelOnAir reports whether mixer channel ch (1..4) is audible on the master bus,
// and whether an active StageLinQ mixer has reported state for that channel.
func (s MixerState) ChannelOnAir(ch int, now time.Time) (onAir bool, known bool) {
	if !s.Connected || ch < 1 || ch > 4 || s.LastSeen.IsZero() || now.Sub(s.LastSeen) > 12*time.Second {
		return false, false
	}
	idx := ch - 1
	if !s.FaderKnown[idx] {
		return false, false
	}
	if s.Fader[idx] <= FaderAudibleThreshold {
		return false, true
	}
	switch s.Assign[idx] {
	case AssignA:
		if s.XfaderKnown && s.Crossfader >= 1.0-FaderAudibleThreshold {
			return false, true
		}
	case AssignB:
		if s.XfaderKnown && s.Crossfader <= FaderAudibleThreshold {
			return false, true
		}
	}
	return true, true
}

// Monitor discovers and subscribes to a StageLinQ mixer on the LAN.
type Monitor struct {
	mu          sync.RWMutex
	token       [16]byte
	state       MixerState
	activeAddr  string
	sessionStop context.CancelFunc
}

func NewMonitor() *Monitor {
	var tok [16]byte
	_, _ = rand.Read(tok[:])
	return &Monitor{
		token: tok,
		state: MixerState{Channels: 4, Crossfader: 0.5},
	}
}

func (m *Monitor) Snapshot() MixerState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

func (m *Monitor) ChannelOnAir(ch int, now time.Time) (bool, bool) {
	return m.Snapshot().ChannelOnAir(ch, now)
}

// ApplyStateMap injects a StateMap key/value update at timestamp at (for deterministic simulation and tests).
func (m *Monitor) ApplyStateMap(deviceName, path string, payload any, at time.Time) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.state.Connected = true
	if deviceName != "" {
		m.state.DeviceName = deviceName
	}
	if !at.IsZero() {
		m.state.LastSeen = at
	} else {
		m.state.LastSeen = time.Now()
	}
	applyMixerStateJSON(&m.state, path, string(raw))
	m.mu.Unlock()
}

// ListenAndServe listens on udpAddr (default ":51337") until ctx is canceled.
func (m *Monitor) ListenAndServe(ctx context.Context, udpAddr string) error {
	if udpAddr == "" {
		udpAddr = fmt.Sprintf(":%d", DiscoveryPort)
	}
	pc, err := net.ListenPacket("udp4", udpAddr)
	if err != nil {
		return err
	}
	stopClose := context.AfterFunc(ctx, func() { _ = pc.Close() })
	defer func() {
		stopClose()
		_ = pc.Close()
		m.stopSession()
	}()

	buf := make([]byte, 4096)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		u, ok := src.(*net.UDPAddr)
		if !ok || u.IP == nil {
			continue
		}
		pkt, err := DecodeDiscovery(buf[:n])
		if err != nil || bytes.Equal(pkt.Token[:], m.token[:]) || !IsMixerDevice(pkt) {
			continue
		}
		m.ObserveDiscovery(ctx, u.IP.String(), pkt)
	}
}

func (m *Monitor) stopSession() {
	m.mu.Lock()
	cancel := m.sessionStop
	m.sessionStop = nil
	m.activeAddr = ""
	m.state.Connected = false
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ObserveDiscovery handles a parsed StageLinQ mixer discovery packet from ip.
func (m *Monitor) ObserveDiscovery(ctx context.Context, ip string, pkt DiscoveryPacket) {
	if pkt.Action == ActionExit {
		m.mu.Lock()
		same := m.activeAddr == ip
		m.mu.Unlock()
		if same {
			m.stopSession()
		}
		return
	}
	if pkt.Action != ActionHowdy || pkt.Port == 0 {
		return
	}
	m.mu.Lock()
	if m.activeAddr != "" {
		if m.activeAddr == ip {
			m.state.LastSeen = time.Now()
		}
		m.mu.Unlock()
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	m.activeAddr = ip
	m.sessionStop = cancel
	m.state.DeviceName = pkt.Source
	m.state.Address = ip
	m.mu.Unlock()

	go m.runSession(sctx, ip, pkt.Port, pkt.Token)
}

func (m *Monitor) runSession(ctx context.Context, ip string, dirPort uint16, targetTok [16]byte) {
	defer func() {
		m.mu.Lock()
		if m.activeAddr == ip {
			m.activeAddr = ""
			m.sessionStop = nil
			m.state.Connected = false
		}
		m.mu.Unlock()
	}()

	dialer := &net.Dialer{Timeout: 4 * time.Second, KeepAlive: 10 * time.Second}
	dirConn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(int(dirPort))))
	if err != nil {
		return
	}
	defer dirConn.Close()
	stopDir := context.AfterFunc(ctx, func() { _ = dirConn.Close() })
	defer stopDir()

	// Request directory services
	var req bytes.Buffer
	_ = binary.Write(&req, binary.BigEndian, msgServicesRequest)
	req.Write(m.token[:])
	if _, err := dirConn.Write(req.Bytes()); err != nil {
		return
	}

	var stateMapPort uint16
	_ = dirConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for stateMapPort == 0 {
		var msgID uint32
		if err := binary.Read(dirConn, binary.BigEndian, &msgID); err != nil {
			return
		}
		switch msgID {
		case msgServiceAnnounce:
			var tok [16]byte
			if _, err := io.ReadFull(dirConn, tok[:]); err != nil {
				return
			}
			svc, err := readUTF16BEString(dirConn)
			if err != nil {
				return
			}
			var port uint16
			if err := binary.Read(dirConn, binary.BigEndian, &port); err != nil {
				return
			}
			if svc == "StateMap" {
				stateMapPort = port
			}
		case msgReference:
			var tok1, tok2 [16]byte
			var ref int64
			if _, err := io.ReadFull(dirConn, tok1[:]); err != nil {
				return
			}
			if _, err := io.ReadFull(dirConn, tok2[:]); err != nil {
				return
			}
			if err := binary.Read(dirConn, binary.BigEndian, &ref); err != nil {
				return
			}
		case msgServicesRequest:
			var tok [16]byte
			if _, err := io.ReadFull(dirConn, tok[:]); err != nil {
				return
			}
		default:
			return
		}
	}
	_ = dirConn.SetReadDeadline(time.Time{})

	// Keep directory connection alive with reference heartbeats
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var hb bytes.Buffer
				_ = binary.Write(&hb, binary.BigEndian, msgReference)
				hb.Write(m.token[:])
				hb.Write(targetTok[:])
				_ = binary.Write(&hb, binary.BigEndian, int64(0))
				if _, err := dirConn.Write(hb.Bytes()); err != nil {
					return
				}
			}
		}
	}()

	smConn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(int(stateMapPort))))
	if err != nil {
		return
	}
	defer smConn.Close()
	stopSM := context.AfterFunc(ctx, func() { _ = smConn.Close() })
	defer stopSM()

	localPort := uint16(0)
	if ta, ok := smConn.LocalAddr().(*net.TCPAddr); ok {
		localPort = uint16(ta.Port)
	}
	var ann bytes.Buffer
	_ = binary.Write(&ann, binary.BigEndian, msgServiceAnnounce)
	ann.Write(m.token[:])
	_ = writeUTF16BEString(&ann, "StateMap")
	_ = binary.Write(&ann, binary.BigEndian, localPort)
	if _, err := smConn.Write(ann.Bytes()); err != nil {
		return
	}

	paths := []string{
		"/Mixer/NumberOfChannels",
		"/Mixer/CrossfaderPosition",
		"/Mixer/CH1faderPosition",
		"/Mixer/CH2faderPosition",
		"/Mixer/CH3faderPosition",
		"/Mixer/CH4faderPosition",
		"/Mixer/ChannelAssignment1",
		"/Mixer/ChannelAssignment2",
		"/Mixer/ChannelAssignment3",
		"/Mixer/ChannelAssignment4",
	}
	for _, p := range paths {
		if err := writeSmaaSubscribe(smConn, p, 0); err != nil {
			return
		}
	}

	m.mu.Lock()
	m.state.Connected = true
	m.state.LastSeen = time.Now()
	m.mu.Unlock()

	for {
		_ = smConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		name, rawJSON, isEmit, err := readSmaaFrame(smConn)
		if err != nil {
			return
		}
		m.mu.Lock()
		m.state.LastSeen = time.Now()
		if isEmit {
			applyMixerStateJSON(&m.state, name, rawJSON)
		}
		m.mu.Unlock()
	}
}

func writeSmaaSubscribe(w io.Writer, path string, intervalMS uint32) error {
	var body bytes.Buffer
	body.Write(smaaMagic)
	_ = binary.Write(&body, binary.BigEndian, smaaSubscribe)
	if err := writeUTF16BEString(&body, path); err != nil {
		return err
	}
	_ = binary.Write(&body, binary.BigEndian, intervalMS)

	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, uint32(body.Len()))
	frame.Write(body.Bytes())
	_, err := w.Write(frame.Bytes())
	return err
}

// WriteSmaaEmit writes a StageLinQ StateMap emit frame (used in tests and simulators).
func WriteSmaaEmit(w io.Writer, path, jsonText string) error {
	var body bytes.Buffer
	body.Write(smaaMagic)
	_ = binary.Write(&body, binary.BigEndian, smaaEmit)
	if err := writeUTF16BEString(&body, path); err != nil {
		return err
	}
	if err := writeUTF16BEString(&body, jsonText); err != nil {
		return err
	}
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, uint32(body.Len()))
	frame.Write(body.Bytes())
	_, err := w.Write(frame.Bytes())
	return err
}

func readSmaaFrame(r io.Reader) (name, rawJSON string, isEmit bool, err error) {
	var length uint32
	if err = binary.Read(r, binary.BigEndian, &length); err != nil {
		return "", "", false, err
	}
	if length < 8 || length > 1<<20 {
		return "", "", false, errors.New("stagelinq: invalid smaa frame length")
	}
	payload := make([]byte, length)
	if _, err = io.ReadFull(r, payload); err != nil {
		return "", "", false, err
	}
	if !bytes.Equal(payload[:4], smaaMagic) {
		return "", "", false, errors.New("stagelinq: invalid smaa magic")
	}
	msgType := binary.BigEndian.Uint32(payload[4:8])
	br := bytes.NewReader(payload[8:])
	switch msgType {
	case smaaSubscribeResponse:
		name, err = readUTF16BEString(br)
		return name, "", false, err
	case smaaEmit:
		if name, err = readUTF16BEString(br); err != nil {
			return "", "", false, err
		}
		if rawJSON, err = readUTF16BEString(br); err != nil {
			return "", "", false, err
		}
		return name, rawJSON, true, nil
	default:
		return "", "", false, nil
	}
}

func parseJSONFloat(raw string) (float64, bool) {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return 0, false
	}
	for _, k := range []string{"value", "unnormalized", "state"} {
		switch v := m[k].(type) {
		case float64:
			return v, true
		case bool:
			if v {
				return 1, true
			}
			return 0, true
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func parseAssignValue(raw string) (int, bool) {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return 0, false
	}
	if s, ok := m["string"].(string); ok {
		switch strings.ToUpper(strings.TrimSpace(s)) {
		case "A", "LEFT":
			return AssignA, true
		case "B", "RIGHT":
			return AssignB, true
		case "THRU", "OFF", "CENTER", "":
			return AssignThru, true
		}
	}
	if v, ok := parseJSONFloat(raw); ok {
		iv := int(v)
		if iv >= 0 && iv <= 2 {
			return iv, true
		}
	}
	return 0, false
}

func applyMixerStateJSON(st *MixerState, name, rawJSON string) {
	switch name {
	case "/Mixer/NumberOfChannels":
		if v, ok := parseJSONFloat(rawJSON); ok && v >= 2 && v <= 4 {
			st.Channels = int(v)
		}
	case "/Mixer/CrossfaderPosition":
		if v, ok := parseJSONFloat(rawJSON); ok {
			st.Crossfader = v
			st.XfaderKnown = true
		}
	case "/Mixer/CH1faderPosition", "/Mixer/CH2faderPosition", "/Mixer/CH3faderPosition", "/Mixer/CH4faderPosition":
		idx := int(name[len("/Mixer/CH")] - '1')
		if idx >= 0 && idx < 4 {
			if v, ok := parseJSONFloat(rawJSON); ok {
				st.Fader[idx] = v
				st.FaderKnown[idx] = true
			}
		}
	case "/Mixer/ChannelAssignment1", "/Mixer/ChannelAssignment2", "/Mixer/ChannelAssignment3", "/Mixer/ChannelAssignment4":
		idx := int(name[len(name)-1] - '1')
		if idx >= 0 && idx < 4 {
			if a, ok := parseAssignValue(rawJSON); ok {
				st.Assign[idx] = a
			}
		}
	}
}
