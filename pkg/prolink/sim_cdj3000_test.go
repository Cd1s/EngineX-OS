package prolink_test

// A software protocol simulator, not firmware emulation or a real-hardware
// capture. Documented CDJ-3000 status is 0x200 bytes, NOT 0x120:
// https://djl-analysis.deepsymmetry.org/djl-analysis/vcdj.html
// Packet builders are independent of the production export encoder.
import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func simPacket(kind byte, size, nameAt int, number byte) []byte {
	b := make([]byte, size)
	copy(b, "Qspt1WmJOL")
	b[10] = kind
	copy(b[nameAt:nameAt+20], "CDJ-3000")
	b[nameAt+20] = 1
	if nameAt == 12 {
		b[0x21] = 2
		binary.BigEndian.PutUint16(b[0x22:], uint16(size))
		b[0x24] = number
	} else {
		b[0x21] = number
		binary.BigEndian.PutUint16(b[0x22:], uint16(size-0x24))
	}
	return b
}
func simKeepalive(number byte) []byte {
	b := simPacket(6, 54, 12, number)
	b[0x25] = 1
	copy(b[0x26:], []byte{2, 0, 0, 0, 0, 2})
	copy(b[0x2c:], net.IPv4(127, 63, 0, 2).To4())
	b[0x30] = 2
	b[0x34] = 1
	return b
}
func simStatus() []byte {
	b := simPacket(0x0a, 0x200, 11, 4)
	b[0x20] = 6
	b[0x24] = 4
	b[0x26] = 1
	b[0x27] = 1
	b[0x28] = 4
	b[0x29] = 3
	b[0x2a] = 1
	binary.BigEndian.PutUint32(b[0x2c:], 1)
	b[0x6a] = 4
	b[0x6b] = 4
	b[0x73] = 4
	b[0x75] = 1
	b[0x7b] = 3
	copy(b[0x7c:], "3.20")
	b[0x89] = 0xf8
	b[0x8b] = 0xfa
	for _, off := range []int{0x8c, 0x98, 0xc0, 0xc4} {
		binary.BigEndian.PutUint32(b[off:], 0x108000)
	} // +3.125%
	binary.BigEndian.PutUint16(b[0x90:], 0x8000)
	binary.BigEndian.PutUint16(b[0x92:], 12000)
	b[0x9f] = 0xff
	binary.BigEndian.PutUint32(b[0xa0:], 2)
	b[0xa6] = 2
	b[0xb7] = 1
	return b
}
func simBeat() []byte {
	b := simPacket(0x28, 0x60, 11, 4)
	for _, off := range []int{0x24, 0x28, 0x2c, 0x30, 0x34, 0x38} {
		binary.BigEndian.PutUint32(b[off:], 500)
	}
	binary.BigEndian.PutUint32(b[0x54:], 0x108000)
	binary.BigEndian.PutUint16(b[0x5a:], 12000)
	b[0x5c] = 2
	return b
}
func simSocket(t *testing.T, ip byte, ports ...int) *net.UDPConn {
	t.Helper()
	port := 0
	if len(ports) > 0 {
		port = ports[0]
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, ip), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func simWait(t *testing.T, e *prolink.Exporter, n byte) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if e.Status().Number == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("player %d, wanted %d", e.Status().Number, n)
}

func TestSimCDJ3000ServerToDenon(t *testing.T) {
	usb := t.TempDir()
	testfixture.Export(t, usb)
	nfs := &prolink.NFSServer{Root: usb}
	if err := nfs.Listen("127.63.0.2:0"); err != nil {
		t.Fatal(err)
	}
	defer nfs.Close()
	discovery := prolink.NewDiscovery()
	cache := t.TempDir()
	discovery.SetMediaCache(&prolink.MediaCache{Dir: cache, Portmap: nfs.PortmapPort()})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	updates := make(chan prolink.Device, 32)
	stopped := make(chan error, 1)
	go func() {
		stopped <- discovery.Listen(ctx, net.IPv4(127, 63, 0, 1), func(d prolink.Device) {
			select {
			case updates <- d:
			default:
			}
		})
	}()
	defer func() {
		cancel()
		select {
		case err := <-stopped:
			if err != nil && err != context.Canceled {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("discovery did not stop")
		}
	}()
	sender := simSocket(t, 2)
	send := func(b []byte, p int) {
		t.Helper()
		if _, err := sender.WriteToUDP(b, &net.UDPAddr{IP: net.IPv4(127, 63, 0, 1), Port: p}); err != nil {
			t.Fatal(err)
		}
	}
	var live prolink.Device
	deadline := time.After(3 * time.Second)
	retry := time.NewTicker(20 * time.Millisecond)
	defer retry.Stop()
	found := false
	for !found {
		select {
		case <-deadline:
			t.Fatal("no UDP-discovered CDJ-3000 with NFS waveform")
		case <-retry.C:
			send(simKeepalive(4), 50000)
			send(simStatus(), 50002)
			send(simBeat(), 50001)
		case d := <-updates:
			if d.PacketType == 0x0a && d.Name == "CDJ-3000" && len(d.Waveform) > 0 && len(d.Beats) > 0 {
				live = d
				found = true
			}
		}
	}
	if live.PlayPositionMS != live.Beats[1].TimeMS {
		t.Fatal("live position does not follow fetched beatgrid")
	}
	if live.Number != 4 || live.SourceSlot != 3 || live.TrackID != 1 || live.BPM != 120 || live.Pitch != 3.125 || live.EffectiveBPM != 123.75 || !live.Master || !live.Playing || !live.Sync || !live.OnAir || live.Beat != 2 || live.Title == "" {
		t.Fatalf("live metadata: %+v", live)
	}
	if len(live.Waveform) != 4 || live.Waveform[0] != [3]byte{30, 10, 20} || live.Waveform[3] != [3]byte{120, 100, 110} {
		t.Fatal("2EX three-band waveform not selected")
	}
	beatSeen := false
	beatDeadline := time.After(time.Second)
	for !beatSeen {
		select {
		case <-beatDeadline:
			t.Fatal("no live beat UDP notification")
		case <-retry.C:
			send(simBeat(), 50001)
		case d := <-updates:
			if d.PacketType == 0x28 && len(d.Beats) > 0 {
				if d.BPM != 120 || d.Pitch != 3.125 || len(d.Beats) == 0 || len(d.Waveform) != 4 || !d.Master || !d.Sync {
					t.Fatal("beat update lost live status", d)
				}
				beatSeen = true
			}
		}
	}
	// The UI's snapshot consumes these exact waveform columns and beat points.
	snap := prolink.BuildSnapshot(discovery.Devices(time.Now()))
	if len(snap.Devices) != 1 || len(snap.Devices[0].Waveform) == 0 {
		t.Fatal("waveform missing from display snapshot")
	}
	for _, ext := range []string{"DAT", "EXT", "2EX"} {
		if _, err := os.Stat(filepath.Join(cache, "127.63.0.2", "4-3/PIONEER/USBANLZ/P001/ANLZ0000."+ext)); err != nil {
			t.Fatal("missing fetched analysis", err)
		}
	}
	// Import the same simulated network drive through Stage -> real rb-bridge.
	denon := t.TempDir() // No local Rekordbox export on the receiving Denon drive.
	client := &prolink.NFSClient{IP: net.IPv4(127, 63, 0, 2), Portmap: nfs.PortmapPort()}
	staged := filepath.Join(denon, prolink.StagedDir, "CDJ-3000")
	if count, err := prolink.Stage(t.Context(), client, "/C/", staged, 1<<26); err != nil || count < 8 {
		t.Fatalf("stage: %d %v", count, err)
	}
	if r, err := bridge.ConvertAtMount(t.Context(), denon, enginedb.Options{}, true); err != nil || r.Added != 5 {
		t.Fatalf("bridge: %+v %v", r, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(denon, "Engine Library/Database2/m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id, n int
	if err = db.QueryRow(`SELECT id FROM Playlist WHERE title='[Pro DJ Link: CDJ-3000]' AND parentListId=0`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM Playlist WHERE parentListId=?`, id).Scan(&n); err != nil || n < 1 {
		t.Fatal("imported playlists missing", err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM Track WHERE path LIKE '%.prolink-cache/CDJ-3000/%'`).Scan(&n); err != nil || n != 5 {
		t.Fatal("staged playable audio missing", n, err)
	}
}

// Independent tagged-number request and response parser for the virtual client.
func simRequest(tx uint32, kind uint16, args ...uint32) []byte {
	b := []byte{0x11, 0x87, 0x23, 0x49, 0xae, 0x11}
	b = binary.BigEndian.AppendUint32(b, tx)
	b = append(b, 0x10, byte(kind>>8), byte(kind), 0x0f, byte(len(args)), 0x14, 0, 0, 0, 12)
	tags := make([]byte, 12)
	for i := range args {
		tags[i] = 6
	}
	b = append(b, tags...)
	for _, arg := range args {
		b = append(b, 0x11)
		b = binary.BigEndian.AppendUint32(b, arg)
	}
	return b
}

type simMessage struct {
	tx   uint32
	kind uint16
	nums []uint32
	text map[int]string
}

func simRead(t *testing.T, c net.Conn) simMessage {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	h := make([]byte, 32)
	if _, err := io.ReadFull(c, h); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h[:5], []byte{0x11, 0x87, 0x23, 0x49, 0xae}) || h[14] > 12 || binary.BigEndian.Uint32(h[16:]) != 12 {
		t.Fatal("invalid db header")
	}
	m := simMessage{tx: binary.BigEndian.Uint32(h[6:]), kind: binary.BigEndian.Uint16(h[11:]), text: make(map[int]string)}
	for i := 0; i < int(h[14]); i++ {
		f := make([]byte, 5)
		if _, err := io.ReadFull(c, f); err != nil {
			t.Fatal(err)
		}
		n := binary.BigEndian.Uint32(f[1:])
		m.nums = append(m.nums, n)
		if h[20+i] == 2 {
			if f[0] != 0x26 || n == 0 || n > 65536 {
				t.Fatal("invalid UTF16")
			}
			raw := make([]byte, 2*n)
			if _, err := io.ReadFull(c, raw); err != nil {
				t.Fatal(err)
			}
			u := make([]uint16, n-1)
			for j := range u {
				u[j] = binary.BigEndian.Uint16(raw[2*j:])
			}
			m.text[i] = string(utf16.Decode(u))
		} else if h[20+i] != 6 || f[0] != 0x11 {
			t.Fatal("unexpected response field")
		}
	}
	return m
}

func TestSimCDJ3000ClientToDenon(t *testing.T) {
	announcements := simSocket(t, 3)
	mediaBroadcast := simSocket(t, 3, 50002)
	peerStatus := simSocket(t, 2, 50002)
	root := t.TempDir()
	testfixture.Export(t, root)
	e := prolink.NewExporter(prolink.ExportConfig{Root: root, IP: net.IPv4(127, 63, 0, 1), MAC: net.HardwareAddr{2, 0, 0, 0, 0, 1}, DBAddr: "127.63.0.1:0", PortmapAddr: "127.63.0.1:0", DiscoveryAddr: "127.63.0.1:0", StatusAddr: "127.63.0.1:0", AnnounceAddr: announcements.LocalAddr().String(), ProbeDuration: 20 * time.Millisecond})
	defer e.Close()
	if e.Status().Enabled {
		t.Fatal("default ON")
	}
	if err := e.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	simWait(t, e, 4)
	peer := simSocket(t, 2)
	readPacket := func(socket *net.UDPConn, size int, kind byte) []byte {
		t.Helper()
		_ = socket.SetReadDeadline(time.Now().Add(time.Second))
		b := make([]byte, 2048)
		n, _, err := socket.ReadFromUDP(b)
		if err != nil || n != size || b[10] != kind {
			t.Fatalf("outgoing packet size/type: %d/%x %v", n, b[10], err)
		}
		return b[:n]
	}
	keep := readPacket(announcements, 54, 6)
	if keep[0x24] != 4 || string(keep[:10]) != "Qspt1WmJOL" {
		t.Fatal("export keepalive")
	}
	media := readPacket(mediaBroadcast, 192, 6)
	if media[0x27] != 4 || media[0x2b] != 3 {
		t.Fatal("export media broadcast")
	}
	claimAddress, _ := net.ResolveUDPAddr("udp4", e.Status().DiscoveryAddress)
	if _, err := peer.WriteToUDP(simKeepalive(6), claimAddress); err != nil {
		t.Fatal(err)
	}
	idle := readPacket(peerStatus, 0x11c, 0x0a)
	if idle[0x21] != 4 || idle[0x6f] != 0 || idle[0x75] != 1 || idle[0x89]&0x70 != 0 {
		t.Fatal("outgoing mounted idle status")
	}
	// Query mounted USB using the real 48-byte media query envelope.
	mediaQuery := simPacket(5, 0x30, 11, 1)
	copy(mediaQuery[0x24:], net.IPv4(127, 63, 0, 2).To4())
	mediaQuery[0x2b] = 4
	mediaQuery[0x2f] = 3
	// Export test address overrides deliberately keep traffic off the LAN.
	status := e.Status()
	address, err := net.ResolveUDPAddr("udp4", status.StatusAddress)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = peer.WriteToUDP(mediaQuery, address); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	raw := make([]byte, 2048)
	n, _, err := peer.ReadFromUDP(raw)
	if err != nil || n != 192 || raw[10] != 6 || raw[0x27] != 4 || raw[0x2b] != 3 || binary.BigEndian.Uint16(raw[0xa6:]) != 5 {
		t.Fatal("media response", n, err)
	}
	q, err := net.Dial("tcp4", status.DBAddress)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = q.Write(append([]byte{0, 0, 0, 15}, []byte("RemoteDBServer\x00")...))
	var port [2]byte
	if _, err = io.ReadFull(q, port[:]); err != nil {
		t.Fatal(err)
	}
	q.Close()
	_, portString, _ := net.SplitHostPort(status.DBAddress)
	if strconv.Itoa(int(binary.BigEndian.Uint16(port[:]))) != portString {
		t.Fatal("wrong dbserver port query reply")
	}
	c, err := net.Dial("tcp4", status.DBAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte{0x11, 0, 0, 0, 1})
	var greeting [5]byte
	if _, err = io.ReadFull(c, greeting[:]); err != nil || greeting != [5]byte{0x11, 0, 0, 0, 1} {
		t.Fatal("greeting", err)
	}
	_, _ = c.Write(simRequest(0xfffffffe, 0, 6))
	if m := simRead(t, c); m.kind != 0x4000 || m.nums[1] != 4 {
		t.Fatal("six-player setup")
	}
	tx := uint32(0)
	query := func(kind uint16, args ...uint32) []simMessage {
		t.Helper()
		tx++
		_, _ = c.Write(simRequest(tx, kind, append([]uint32{0x06010301}, args...)...))
		m := simRead(t, c)
		if m.kind != 0x4000 || m.tx != tx {
			t.Fatalf("query %x: %x", kind, m.kind)
		}
		count := m.nums[1]
		tx++
		_, _ = c.Write(simRequest(tx, 0x3000, 0x06010301, 0, count, 0, count, 0))
		if m = simRead(t, c); m.kind != 0x4001 || m.tx != tx || len(m.nums) != 2 || m.nums[0] != 1 {
			t.Fatal("menu header")
		}
		out := make([]simMessage, 0, count)
		for range count {
			m = simRead(t, c)
			if m.kind != 0x4101 || m.tx != tx {
				t.Fatal("item")
			}
			out = append(out, m)
		}
		if simRead(t, c).kind != 0x4201 {
			t.Fatal("footer")
		}
		return out
	}
	if items := query(0x1000, 0, 0xffffff); len(items) != 2 || items[0].nums[1] != 5 {
		t.Fatal("root")
	}
	if items := query(0x1105, 0, 0, 1); len(items) != 2 {
		t.Fatal("playlist root")
	}
	if items := query(0x1105, 0, 10, 1); len(items) != 1 || items[0].nums[1] != 11 {
		t.Fatal("nested folder")
	}
	if items := query(0x1105, 0, 11, 1); len(items) != 1 || items[0].nums[1] != 12 {
		t.Fatal("nested playlist")
	}
	if items := query(0x1105, 0, 12, 0); len(items) != 2 || items[0].nums[1] != 1 {
		t.Fatal("ordered tracks")
	}
	items := query(0x2102, 1)
	if len(items) != 1 {
		t.Fatal("audio path menu")
	}
	audio := items[0].text[3]
	nfs := &prolink.NFSClient{IP: net.IPv4(127, 63, 0, 1), Portmap: status.PortmapPort}
	pdb, _, err := nfs.Fetch(t.Context(), "/C/", "/PIONEER/rekordbox/export.pdb", 1<<26)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := rekordbox.ParsePDB(bytes.NewReader(pdb), int64(len(pdb)))
	if err != nil {
		t.Fatal(err)
	}
	analysis := ""
	for _, track := range lib.Tracks {
		if track.ID == 1 {
			analysis = track.AnalysisPath
		}
	}
	if len(analysis) < 4 {
		t.Fatal("missing analysis path")
	}
	for _, path := range []string{audio, analysis[:len(analysis)-4] + ".DAT", analysis[:len(analysis)-4] + ".EXT", analysis[:len(analysis)-4] + ".2EX"} {
		b, _, err := nfs.Fetch(t.Context(), "/C/", path, 1<<26)
		if err != nil || len(b) == 0 {
			t.Fatal("NFS file", path, err)
		}
		if path == audio && !bytes.Equal(b, testfixture.WAV(true)) {
			t.Fatal("downloaded audio bytes differ")
		}
		if path != audio {
			if a, err := rekordbox.ParseAnalysis(b); err != nil || a == nil {
				t.Fatal("invalid ANLZ", err)
			}
		}
	}
	claim, _ := net.ResolveUDPAddr("udp4", status.DiscoveryAddress)
	start := time.Now()
	if _, err = peer.WriteToUDP(simKeepalive(4), claim); err != nil {
		t.Fatal(err)
	}
	simWait(t, e, 3)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("collision yield exceeded 50ms")
	}
	if err = e.SetEnabled(false); err != nil || e.Status().Enabled {
		t.Fatal("OFF failed", err)
	}
}

func TestSimCDJ3000StatusLengthTolerance(t *testing.T) {
	// 0x120 is included only as a synthetic parser tolerance regression requested
	// by the caller. It is not a documented CDJ-3000 firmware packet length.
	for _, size := range []int{0xd4, 0x11c, 0x120, 0x124, 0x200} {
		b := simStatus()[:size]
		binary.BigEndian.PutUint16(b[0x22:], uint16(size-0x24))
		d, err := prolink.ParsePacket(b, 50002, net.IPv4(127, 63, 0, 2))
		if err != nil || d.BPM != 120 || d.Pitch != 3.125 || !d.Master || d.Name != "CDJ-3000" || d.PlayPositionMS != 500 {
			t.Fatalf("status length %x: %+v %v", size, d, err)
		}
	}
}

func TestSimPitchExtremes(t *testing.T) {
	for raw, want := range map[uint32]float64{0: -100, 0x100000: 0, 0x200000: 100} {
		b := simStatus()
		binary.BigEndian.PutUint32(b[0x8c:], raw)
		d, err := prolink.ParsePacket(b, 50002, net.IPv4(127, 63, 0, 2))
		if err != nil || d.Pitch != want {
			t.Fatalf("pitch %x: %v %v", raw, d.Pitch, err)
		}
	}
}
