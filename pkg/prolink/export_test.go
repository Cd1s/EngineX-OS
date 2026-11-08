package prolink

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	"golang.org/x/text/unicode/norm"
)

// NFSStatusForTest runs one NFS v2 procedure with a file handle (and READ's
// offset/count) through the server's dispatcher and returns its status.
func NFSStatusForTest(s *NFSServer, proc uint32, fh []byte) uint32 {
	call := &xdr{}
	call.u32(1).u32(0).u32(2).u32(rpcNFS).u32(2).u32(proc).u32(0).opaque(nil).u32(0).opaque(nil)
	call.Write(fh)
	call.u32(0).u32(16).u32(0)
	r := &reader{b: s.handle(call.Bytes(), 0)}
	for i := 0; i < 6; i++ { // xid, REPLY, accepted, verifier, accept_stat
		r.u32()
	}
	return r.u32()
}

func testExportConfig(root string) ExportConfig {
	return ExportConfig{Root: root, IP: net.IPv4(127, 63, 0, 1), MAC: net.HardwareAddr{2, 0, 0, 0, 0, 1}, DBAddr: "127.63.0.1:0", PortmapAddr: "127.63.0.1:0", DiscoveryAddr: "127.63.0.1:0", StatusAddr: "127.63.0.1:0", AnnounceAddr: "127.63.0.1:9", ProbeDuration: 20 * time.Millisecond}
}
func waitNumber(t *testing.T, e *Exporter, want byte, limit time.Duration) {
	t.Helper()
	until := time.Now().Add(limit)
	for time.Now().Before(until) {
		if e.Status().Number == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("number=%d, want %d", e.Status().Number, want)
}
func TestExportDefaultOffIsInert(t *testing.T) {
	// Occupied ports and a nonexistent root would both fail if OFF touched them.
	tcp, err := net.Listen("tcp4", "127.63.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	cfg := testExportConfig(filepath.Join(t.TempDir(), "absent"))
	cfg.DBAddr = tcp.Addr().String()
	cfg.PortmapAddr, cfg.DiscoveryAddr, cfg.StatusAddr = udp.LocalAddr().String(), udp.LocalAddr().String(), udp.LocalAddr().String()
	before := runtime.NumGoroutine()
	e := NewExporter(cfg)
	if err := e.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if e.Status() != (ExportStatus{}) {
		t.Fatalf("OFF has resources: %+v", e.Status())
	}
	if after := runtime.NumGoroutine(); after != before {
		t.Fatalf("OFF started goroutines: %d -> %d", before, after)
	}
	if allocations := testing.AllocsPerRun(100, func() { _ = e.SetEnabled(false) }); allocations != 0 {
		t.Fatalf("OFF allocates: %g", allocations)
	}
	e.Close()
}
func TestExportToggleAndCollision(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	cfg := testExportConfig(root)
	cfg.AnnounceAddr = sink.LocalAddr().String()
	cfg.ProbeDuration = 60 * time.Millisecond
	e := NewExporter(cfg)
	defer e.Close()
	if err := e.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	state := e.Status()
	if state.Number != 0 || state.DBAddress == "" || state.PortmapPort == 0 {
		t.Fatalf("not probing with services ready: %+v", state)
	}
	buf := make([]byte, 2048)
	_ = sink.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, _, err := sink.ReadFromUDP(buf); err == nil {
		t.Fatal("announced before listening for peers")
	}
	waitNumber(t, e, 4, 300*time.Millisecond)
	addr, _ := net.ResolveUDPAddr("udp4", state.DiscoveryAddress)
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	claim := func(number byte) {
		p := make([]byte, 0x36)
		copy(p, magic)
		p[10] = 6
		p[0x24] = number
		copy(p[0x26:], []byte{2, 0, 0, 0, 0, number + 10})
		copy(p[0x2c:], net.IPv4(127, 63, 0, 2).To4())
		if _, err := peer.WriteToUDP(p, addr); err != nil {
			t.Fatal(err)
		}
	}
	// Our own reflected announcement must not evict us.
	self, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer self.Close()
	e.mu.Lock()
	r := e.run
	r.mu.Lock()
	own := r.announcement()
	r.mu.Unlock()
	e.mu.Unlock()
	_, _ = self.WriteToUDP(own, addr)
	// The same announcement looped back through a second interface (Wi-Fi on the same subnet).
	wifi, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 3)})
	if err != nil {
		t.Fatal(err)
	}
	defer wifi.Close()
	_, _ = wifi.WriteToUDP(own, addr)
	time.Sleep(5 * time.Millisecond)
	if e.Status().Number != 4 {
		t.Fatal("yielded to own MAC/IP")
	}
	claim(4)
	waitNumber(t, e, 3, 50*time.Millisecond)
	claim(3)
	waitNumber(t, e, 2, 50*time.Millisecond)
	claim(2)
	waitNumber(t, e, 1, 50*time.Millisecond)
	claim(1)
	waitNumber(t, e, 0, 50*time.Millisecond)
	// Once all numbers are externally owned, drain earlier packets and verify
	// the periodic announcer stays silent too.
	for {
		_ = sink.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		if _, _, err := sink.ReadFromUDP(buf); err != nil {
			break
		}
	}
	_ = sink.SetReadDeadline(time.Now().Add(1600 * time.Millisecond))
	if _, _, err := sink.ReadFromUDP(buf); err == nil {
		t.Fatal("announced with all four numbers occupied")
	}
	// Include an idle TCP connection: OFF must not wait for its read deadline.
	c, err := net.Dial("tcp4", state.DBAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if err := e.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("OFF waited on an idle dbserver client")
	}
	if e.Status() != (ExportStatus{}) {
		t.Fatal("OFF retained resources")
	}
	l, err := net.Listen("tcp4", state.DBAddress)
	if err != nil {
		t.Fatalf("dbserver port retained: %v", err)
	}
	l.Close()
	for _, a := range []string{state.DiscoveryAddress, net.JoinHostPort("127.63.0.1", strconv.Itoa(state.PortmapPort))} {
		a, _ := net.ResolveUDPAddr("udp4", a)
		u, err := net.ListenUDP("udp4", a)
		if err != nil {
			t.Fatal(err)
		}
		u.Close()
	}
	if err := e.SetEnabled(true); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitNumber(t, e, 4, 300*time.Millisecond)
}

// Independent client framing: do not use the server's encoder/decoder to test
// its wire compatibility, including UTF-16 lengths and the fixed 12 tag slots.
func dbRequest(tx uint32, kind uint16, args ...uint32) []byte {
	b := []byte{0x11, 0x87, 0x23, 0x49, 0xae, 0x11}
	b = binary.BigEndian.AppendUint32(b, tx)
	b = append(b, 0x10, byte(kind>>8), byte(kind), 0x0f, byte(len(args)), 0x14, 0, 0, 0, 12)
	tags := make([]byte, 12)
	for i := range args {
		tags[i] = 6
	}
	b = append(b, tags...)
	for _, n := range args {
		b = append(b, 0x11)
		b = binary.BigEndian.AppendUint32(b, n)
	}
	return b
}
func dbResponse(t *testing.T, c net.Conn) dbMessage {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	h := make([]byte, 32)
	if _, err := io.ReadFull(c, h); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h[:5], []byte{0x11, 0x87, 0x23, 0x49, 0xae}) || h[5] != 0x11 || h[10] != 0x10 || h[13] != 0x0f || h[15] != 0x14 || binary.BigEndian.Uint32(h[16:]) != 12 || h[14] > 12 {
		t.Fatalf("bad response header: %x", h)
	}
	m := dbMessage{tx: binary.BigEndian.Uint32(h[6:]), kind: binary.BigEndian.Uint16(h[11:])}
	for i := 0; i < int(h[14]); i++ {
		f := make([]byte, 5)
		if _, err := io.ReadFull(c, f); err != nil {
			t.Fatal(err)
		}
		n := binary.BigEndian.Uint32(f[1:])
		arg := dbField{tag: f[0], n: n}
		switch h[20+i] {
		case 6:
			if f[0] != 0x11 {
				t.Fatal("number tag mismatch")
			}
		case 2:
			if f[0] != 0x26 || n == 0 || n > 65536 {
				t.Fatal("invalid string field")
			}
			arg.data = make([]byte, 2*int(n))
			if _, err := io.ReadFull(c, arg.data); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(arg.data[len(arg.data)-2:], []byte{0, 0}) {
				t.Fatal("string missing terminator")
			}
		default:
			t.Fatalf("unexpected argument tag %x", h[20+i])
		}
		m.args = append(m.args, arg)
	}
	return m
}
func fieldText(f dbField) string {
	u := make([]uint16, len(f.data)/2-1)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(f.data[2*i:])
	}
	return string(utf16.Decode(u))
}
func dbConnect(t *testing.T, e *Exporter) net.Conn {
	t.Helper()
	address := e.Status().DBAddress
	q, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.Write(append([]byte{0, 0, 0, 15}, []byte("RemoteDBServer\x00")...))
	if err != nil {
		t.Fatal(err)
	}
	var port [2]byte
	if _, err := io.ReadFull(q, port[:]); err != nil {
		t.Fatal(err)
	}
	q.Close()
	_, p, _ := net.SplitHostPort(address)
	if strconv.Itoa(int(binary.BigEndian.Uint16(port[:]))) != p {
		t.Fatal("wrong port discovery response")
	}
	c, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	// Fragment the initial number field into separate writes.
	for _, b := range []byte{0x11, 0, 0, 0, 1} {
		if _, err := c.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	var hello [5]byte
	if _, err := io.ReadFull(c, hello[:]); err != nil || hello != [5]byte{0x11, 0, 0, 0, 1} {
		t.Fatalf("greeting: %x %v", hello, err)
	}
	_, _ = c.Write(dbRequest(0xfffffffe, 0, 1))
	m := dbResponse(t, c)
	if m.kind != 0x4000 || m.tx != 0xfffffffe || len(m.args) != 2 || m.args[1].n != 4 {
		t.Fatalf("setup: %+v", m)
	}
	return c
}
func TestExportDBServerAndNFS(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	e := NewExporter(testExportConfig(root))
	defer e.Close()
	if err := e.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	waitNumber(t, e, 4, 300*time.Millisecond)
	c := dbConnect(t, e)
	// Browsers interleave main and submenu queries on one connection.
	_, _ = c.Write(append(dbRequest(100, 0x1000, 0x01010301, 0, 0xffffff), dbRequest(101, 0x1004, 0x01020301, 0)...))
	if dbResponse(t, c).kind != 0x4000 || dbResponse(t, c).kind != 0x4000 {
		t.Fatal("menu setup")
	}
	_, _ = c.Write(dbRequest(102, 0x3000, 0x01010301, 0, 2, 0, 2, 0))
	if dbResponse(t, c).kind != 0x4001 || dbResponse(t, c).args[6].n != 0x84 || dbResponse(t, c).args[6].n != 0x83 || dbResponse(t, c).kind != 0x4201 {
		t.Fatal("interleaved submenu replaced root")
	}
	const dmst = uint32(0x01010301)
	tx := uint32(0)
	query := func(kind uint16, args ...uint32) []dbMessage { t.Helper(); return dbMenu(t, c, &tx, kind, args...) }
	items := query(0x1000, 0, 0x00ffffff)
	if len(items) != 2 || items[0].args[6].n != 0x84 || items[1].args[6].n != 0x83 {
		t.Fatal("root menu must offer playlists and tracks")
	}
	items = query(0x1105, 0, 0, 1)
	if len(items) != 2 || items[0].args[1].n != 10 || items[1].args[1].n != 13 {
		t.Fatal("playlist roots")
	}
	items = query(0x1105, 0, 10, 1)
	if len(items) != 1 || items[0].args[1].n != 11 {
		t.Fatal("nested folder")
	}
	items = query(0x1105, 0, 11, 1)
	if len(items) != 1 || fieldText(items[0].args[3]) != "同名" {
		t.Fatal("playlist UTF-16")
	}
	items = query(0x1105, 0, 12, 0)
	if len(items) != 2 || items[0].args[1].n != 1 || items[1].args[1].n != 2 {
		t.Fatal("ordered playlist tracks")
	}
	if items = query(0x1004, 0); len(items) != 5 {
		t.Fatal("track list")
	}
	items = query(0x2102, 1)
	if len(items) != 1 || fieldText(items[0].args[3]) != "/Music/"+testfixture.UnicodeName || fieldText(items[0].args[5]) != "" {
		t.Fatal("track path reply must contain only the audio path")
	}
	nfs := &NFSClient{IP: net.IPv4(127, 63, 0, 1), Portmap: e.Status().PortmapPort, Timeout: 200 * time.Millisecond}
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
	if analysis != "/PIONEER/USBANLZ/P001/ANLZ0000.DAT" {
		t.Fatal("ANLZ path from export.pdb")
	}
	for i, path := range []string{fieldText(items[0].args[3]), analysis} {
		got, _, err := nfs.Fetch(t.Context(), "/C/", path, 1<<26)
		file := filepath.Join(root, filepath.FromSlash(path[1:]))
		if i == 0 {
			file = filepath.Join(root, "MUSIC", norm.NFD.String(testfixture.UnicodeName))
		}
		want, readErr := os.ReadFile(file)
		if err != nil || readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("NFS %s: %v, local %v", path, err, readErr)
		}
	}
	// Unsupported queries return unavailable; the next valid query still works.
	_, _ = c.Write(dbRequest(90, 0xffff, dmst))
	if m := dbResponse(t, c); m.kind != 0x4003 {
		t.Fatal("unsupported query accepted")
	}
	query(0x2002, 1)
	// Oversized argument counts close a malformed connection before allocation.
	bad := dbRequest(100, 0x1000, dmst, 0, 0)
	bad[14] = 255
	_, _ = c.Write(bad)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := c.Read(one[:]); err == nil {
		t.Fatal("malformed header not rejected")
	}
}

// dbMenu sends one menu query plus its render, as a browsing CDJ does, and
// returns the rendered items.
func dbMenu(t *testing.T, c net.Conn, txp *uint32, kind uint16, args ...uint32) []dbMessage {
	t.Helper()
	const dmst = uint32(0x01010301)
	tx := *txp + 1
	*txp = tx + 1
	request := dbRequest(tx, kind, append([]uint32{dmst}, args...)...)
	// Coalesce menu setup and render into one TCP write.
	render := dbRequest(tx+1, 0x3000, dmst, 0, 64, 0, 64, 0)
	if _, err := c.Write(append(request, render...)); err != nil {
		t.Fatal(err)
	}
	available := dbResponse(t, c)
	if available.kind != 0x4000 || available.tx != tx || available.args[0].n != uint32(kind) {
		t.Fatalf("query: %+v", available)
	}
	tx++
	if h := dbResponse(t, c); h.kind != 0x4001 || h.tx != tx || len(h.args) != 2 || h.args[0].n != 1 || h.args[1].n != 0 {
		t.Fatalf("menu header: %+v", h)
	}
	var items []dbMessage
	for range available.args[1].n {
		m := dbResponse(t, c)
		if m.kind != 0x4101 || m.tx != tx || len(m.args) != 12 {
			t.Fatalf("menu item: %+v", m)
		}
		items = append(items, m)
	}
	if f := dbResponse(t, c); f.kind != 0x4201 || f.tx != tx {
		t.Fatalf("menu footer: %+v", f)
	}
	return items
}
func TestExportFailureRollsBackListeners(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	l, err := net.Listen("tcp4", "127.63.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg := testExportConfig(root)
	cfg.DBAddr = l.Addr().String()
	e := NewExporter(cfg)
	defer e.Close()
	if err := e.SetEnabled(true); err == nil {
		t.Fatal("occupied dbserver port accepted")
	}
	if e.Status().Enabled {
		t.Fatal("partial startup published")
	}
	// Reject a symlinked database before binding anything.
	if err := os.Rename(filepath.Join(root, pdbPath), filepath.Join(root, "outside.pdb")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside.pdb"), filepath.Join(root, pdbPath)); err != nil {
		t.Fatal(err)
	}
	if err := NewExporter(testExportConfig(root)).SetEnabled(true); err == nil {
		t.Fatal("symlinked database accepted")
	}
}

func TestExportProbesExistingPeersAndAnswersMedia(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	cfg := testExportConfig(root)
	cfg.ProbeDuration = 80 * time.Millisecond
	e := NewExporter(cfg)
	defer e.Close()
	if err := e.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	addr, _ := net.ResolveUDPAddr("udp4", e.Status().DiscoveryAddress)
	packet := make([]byte, 0x36)
	copy(packet, magic)
	packet[10] = 6
	packet[0x24] = 4
	copy(packet[0x26:], []byte{2, 0, 0, 0, 0, 99})
	_, _ = peer.WriteToUDP(packet, addr)
	waitNumber(t, e, 3, 300*time.Millisecond)
	e.mu.Lock()
	r := e.run
	media := r.status.LocalAddr().(*net.UDPAddr)
	e.mu.Unlock()
	q := make([]byte, 0x30)
	copy(q, magic)
	q[10] = 5
	q[0x2b] = 3
	q[0x2f] = 3
	_, _ = peer.WriteToUDP(q, media)
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2048)
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil || n != 0xc0 || buf[10] != 6 || buf[0x27] != 3 || buf[0x2b] != 3 || buf[0xaa] != 1 || binary.BigEndian.Uint16(buf[0xa6:]) != 5 || binary.BigEndian.Uint64(buf[0xb0:]) == 0 {
		t.Fatalf("media response n=%d err=%v: %x", n, err, buf[:max(n, 0)])
	}
	// A different/stale database must never keep advertising cached track IDs.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, pdbPath), future, future); err != nil {
		t.Fatal(err)
	}
	waitNumber(t, e, 0, 2*time.Second)
	client := &NFSClient{IP: net.IPv4(127, 63, 0, 1), Portmap: e.Status().PortmapPort, Timeout: 100 * time.Millisecond}
	if _, _, err := client.Fetch(t.Context(), "/C/", "/"+pdbPath, 1<<20); err == nil {
		t.Fatal("changed media served through stale export")
	}
}

func TestNFSServerPinsItsRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "usb")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	testfixture.Export(t, root)
	s := &NFSServer{Root: root}
	if err := s.Listen("127.63.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, "MUSIC/two.flac", []byte("replacement mount"))
	c := &NFSClient{IP: net.IPv4(127, 63, 0, 1), Portmap: s.PortmapPort(), Timeout: 100 * time.Millisecond}
	got, _, err := c.Fetch(t.Context(), "/C/", "/Music/two.flac", 1<<26)
	want, _ := os.ReadFile(filepath.Join(root+"-old", "MUSIC", "two.flac"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("root path replacement escaped pinned directory: %v", err)
	}
}

func TestLinkBroadcastSharedWithPassiveListener(t *testing.T) {
	passive, err := listenLinkUDP("0.0.0.0:0", false)
	if err != nil {
		t.Fatal(err)
	}
	defer passive.Close()
	export, err := listenLinkUDP(passive.LocalAddr().String(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer export.Close()
	sender, err := listenLinkUDP("127.63.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	destination := &net.UDPAddr{IP: net.IPv4(127, 255, 255, 255), Port: passive.LocalAddr().(*net.UDPAddr).Port}
	packet := []byte("loopback discovery broadcast")
	send := func() {
		t.Helper()
		if _, err := sender.WriteToUDP(packet, destination); err != nil {
			t.Fatal(err)
		}
	}
	receive := func(c *net.UDPConn) {
		t.Helper()
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		var b [100]byte
		n, _, err := c.ReadFromUDP(b[:])
		if err != nil || !bytes.Equal(b[:n], packet) {
			t.Fatalf("shared broadcast: %q %v", b[:n], err)
		}
	}
	send()
	receive(passive)
	receive(export)
	export.Close()
	send()
	receive(passive)
}

func TestExportSharedStatusKeepsIncomingWaveform(t *testing.T) {
	root, remote := t.TempDir(), t.TempDir()
	testfixture.Export(t, root)
	testfixture.Export(t, remote)
	peerNFS := &NFSServer{Root: remote}
	if err := peerNFS.Listen("127.63.0.2:0"); err != nil {
		t.Fatal(err)
	}
	defer peerNFS.Close()
	observer := NewDiscovery()
	cache := &MediaCache{Dir: t.TempDir(), Portmap: peerNFS.PortmapPort()}
	observer.SetMediaCache(cache)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- observer.Listen(ctx, nil, nil) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("passive listener did not close")
		}
	}()
	cfg := testExportConfig(root)
	cfg.StatusAddr = "127.63.0.1:50002"
	cfg.MediaCache = cache
	updates := make(chan Device, 4)
	cfg.Observe = func(d Device) {
		d = observer.Observe(d)
		select {
		case updates <- d:
		default:
		}
	}
	e := NewExporter(cfg)
	defer e.Close()
	if err := e.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	// The exporter's specific IP socket receives unicast even while passive
	// wildcard discovery is running. It must forward enriched status to the UI.
	status := make([]byte, 0x200)
	copy(status, magic)
	status[10] = 0x0a
	copy(status[11:], "CDJ-3000")
	status[0x21], status[0x28], status[0x29] = 4, 4, 3
	binary.BigEndian.PutUint32(status[0x2c:], 1)
	binary.BigEndian.PutUint32(status[0x8c:], 0x100000)
	to, _ := net.ResolveUDPAddr("udp4", e.Status().StatusAddress)
	if _, err = peer.WriteToUDP(status, to); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-updates:
		if d.Name != "CDJ-3000" || len(d.Waveform) != 4 || len(d.Beats) == 0 || d.Title == "" {
			t.Fatal("export swallowed waveform enrichment", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("export swallowed incoming unicast")
	}
	status[0x21] = 0
	_, _ = peer.WriteToUDP(status, to)
	select {
	case <-updates:
		t.Fatal("invalid player 0 forwarded")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestDBRenderBeyondOnePage(t *testing.T) {
	lib := &rekordbox.Library{}
	for i := 0; i < 130; i++ {
		lib.Tracks = append(lib.Tracks, rekordbox.Track{ID: uint32(i + 1), Title: strconv.Itoa(i)})
	}
	s := &DBServer{Library: lib, Number: func() byte { return 4 }}
	if err := s.Listen("127.63.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := net.Dial("tcp4", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte{0x11, 0, 0, 0, 1})
	hello := make([]byte, 5)
	if _, err = io.ReadFull(c, hello); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write(dbRequest(0xfffffffe, 0, 1))
	if dbResponse(t, c).kind != 0x4000 {
		t.Fatal("setup")
	}
	_, _ = c.Write(dbRequest(1, 0x1004, 0x01010301, 0))
	if m := dbResponse(t, c); m.kind != 0x4000 || m.args[1].n != 130 {
		t.Fatal("large menu availability")
	}
	_, _ = c.Write(dbRequest(2, 0x3000, 0x01010301, 0, 130, 0, 130, 0))
	if dbResponse(t, c).kind != 0x4001 {
		t.Fatal("large render rejected")
	}
	for range 130 {
		if dbResponse(t, c).kind != 0x4101 {
			t.Fatal("missing large menu item")
		}
	}
	if dbResponse(t, c).kind != 0x4201 {
		t.Fatal("large menu footer")
	}
}

func TestExportMediaQueriesAndOffDuringSlowIncomingNFS(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	blackhole, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	cfg := testExportConfig(root)
	cfg.MediaCache = &MediaCache{Dir: t.TempDir(), Portmap: blackhole.LocalAddr().(*net.UDPAddr).Port}
	cfg.Observe = func(Device) {}
	e := NewExporter(cfg)
	defer e.Close()
	if err = e.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	waitNumber(t, e, 4, time.Second)
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	to, _ := net.ResolveUDPAddr("udp4", e.Status().StatusAddress)
	status := make([]byte, 0x200)
	copy(status, magic)
	status[10] = 0x0a
	copy(status[11:], "CDJ-3000")
	status[0x21], status[0x28], status[0x29] = 3, 3, 3
	binary.BigEndian.PutUint32(status[0x2c:], 1)
	_, _ = peer.WriteToUDP(status, to)
	// Wait until the worker is blocked in a real unanswered RPC.
	_ = blackhole.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2048)
	if _, _, err = blackhole.ReadFromUDP(buf); err != nil {
		t.Fatal(err)
	}
	query := make([]byte, 0x30)
	copy(query, magic)
	query[10] = 5
	query[0x2b], query[0x2f] = 4, 3
	start := time.Now()
	_, _ = peer.WriteToUDP(query, to)
	_ = peer.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil || n != 192 || time.Since(start) > 200*time.Millisecond {
		t.Fatal("slow waveform lookup blocked media query", err)
	}
	start = time.Now()
	if err = e.SetEnabled(false); err != nil || time.Since(start) > 200*time.Millisecond {
		t.Fatal("OFF did not cancel incoming NFS promptly", err)
	}
}

// The export follows the players: a peer in an interface's subnet picks that
// interface; our own address and unreachable peers fall back to the first one.
func TestExportIdentityFollowsPeers(t *testing.T) {
	type cand struct {
		ip  net.IP
		net *net.IPNet
	}
	var cands []cand
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || len(iface.HardwareAddr) != 6 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
				cands = append(cands, cand{n.IP.To4(), n})
			}
		}
	}
	if len(cands) == 0 {
		t.Skip("no usable IPv4 interface")
	}
	first, _, _, _, err := exportIdentity(ExportConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		peer := append(net.IP(nil), c.ip...)
		peer[3] ^= 1 // a neighbour on the same subnet
		if !c.net.Contains(peer) {
			continue
		}
		got, _, _, _, err := exportIdentity(ExportConfig{Peers: func() []net.IP { return []net.IP{c.ip, peer} }})
		if err != nil || !c.net.Contains(got) {
			t.Fatalf("peer %v: picked %v, want an address in %v", peer, got, c.net)
		}
	}
	got, _, _, _, _ := exportIdentity(ExportConfig{Peers: func() []net.IP { return []net.IP{net.IPv4(203, 0, 113, 9)} }})
	if !got.Equal(first) {
		t.Fatalf("unreachable peer: picked %v, want the first interface %v", got, first)
	}
}

// #49: the status packets carry the decks this player plays, and master is
// handed over the Pro DJ Link way (0x26 request, 0x27 answer on the beat port).
func TestExportDeckMasterAndHandoff(t *testing.T) {
	r := &exportRun{player: 3}
	if b := r.statusPacket(); string(b[0x7c:0x80]) != "3.30" {
		t.Fatalf("firmware %q", b[0x7c:0x80])
	}
	if b := r.statusPacket(); b[0x89] != 0x84 || binary.BigEndian.Uint16(b[0x92:]) != 0xffff || b[0x9f] != 0xff {
		t.Fatalf("idle: % x", b[0x89:0xa0])
	}
	r.deck = ExportDeck{Playing: true, Master: true, BPM: 126, Pitch: 2}
	r.handoff = 2
	b := r.statusPacket()
	if b[0x89] != 0x84|0x40|0x20 || binary.BigEndian.Uint16(b[0x92:]) != 12600 || binary.BigEndian.Uint32(b[0x8c:]) != uint32(math.Round(0x100000*1.02)) || b[0x9f] != 2 {
		t.Fatalf("master: % x", b[0x89:0xa0])
	}
	if req := r.handoffPacket(0x26, 0x28); len(req) != 0x28 || req[10] != 0x26 || req[0x1f] != 1 || req[0x21] != 3 || binary.BigEndian.Uint16(req[0x22:]) != 8 || req[0x27] != 3 {
		t.Fatalf("request: % x", req)
	}

	cdj, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 2), Port: 50001})
	if err != nil {
		t.Skip("beat port:", err)
	}
	defer cdj.Close()
	r.beat, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	_, r.network, _ = net.ParseCIDR("127.0.0.0/8")
	r.observe = func(Device) {}
	r.wg.Add(1)
	go r.receiveBeats()
	defer func() { r.beat.Close(); r.wg.Wait() }()
	ask := (&exportRun{player: 2}).handoffPacket(0x26, 0x28)
	cdj.WriteToUDP(ask, r.beat.LocalAddr().(*net.UDPAddr))
	cdj.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 64)
	n, _, err := cdj.ReadFromUDP(got)
	if err != nil || n != 0x2c || got[10] != 0x27 || got[0x21] != 3 || got[0x2b] != 1 {
		t.Fatalf("answer: %v % x", err, got[:n])
	}
	r.mu.Lock()
	h := r.handoff
	r.mu.Unlock()
	if h != 2 {
		t.Fatalf("handoff to %d", h)
	}
}

// #49: as master, the beat phase comes from Engine's BeatNumber (0..4).
func TestExportDeckBeat(t *testing.T) {
	at := time.Now()
	d := ExportDeck{Master: true, Playing: true, BPM: 120, Beat: 2.75, At: at} // 500 ms a beat
	if beat, frac, ok := d.beatAt(at.Add(250 * time.Millisecond)); !ok || beat != 3 || math.Abs(frac-0.25) > 1e-6 {
		t.Fatalf("beat %d %v %v", beat, frac, ok)
	}
	if beat, frac, ok := d.beatAt(at.Add(750 * time.Millisecond)); !ok || beat != 0 || math.Abs(frac-0.25) > 1e-6 {
		t.Fatalf("wrap: beat %d %v %v", beat, frac, ok)
	}
	if _, _, ok := (ExportDeck{Playing: true, BPM: 120, At: at}).beatAt(at); ok {
		t.Fatal("not master, yet beats")
	}
	r := &exportRun{player: 4}
	b := r.beatPacket(ExportDeck{BPM: 120, Pitch: 0}, 1, 0) // beat 2 of the bar just hit
	if len(b) != 0x60 || b[10] != 0x28 || b[0x21] != 4 || b[0x5f] != 4 || b[0x5c] != 2 || binary.BigEndian.Uint16(b[0x5a:]) != 12000 || b[0x3c] != 0xff {
		t.Fatalf("% x", b)
	}
	for i, want := range []uint32{500, 1000, 1500, 2000, 3500, 4000} {
		if got := binary.BigEndian.Uint32(b[0x24+4*i:]); got != want {
			t.Fatalf("timing %d: %d, want %d", i, got, want)
		}
	}
	r.deck = d
	r.deck.At = time.Now()
	if st := r.statusPacket(); st[0xa6] != 3 {
		t.Fatalf("status beat in bar %d", st[0xa6])
	}
}

// A CDJ that loaded from this deck's export reports the export's track id.
func TestExporterTrackPath(t *testing.T) {
	e := &Exporter{run: &exportRun{nfs: &NFSServer{Root: "/media/KDATA"}, lib: &rekordbox.Library{Tracks: []rekordbox.Track{
		{ID: 7, Path: "/Contents/a b.flac"}, {ID: 8, Path: "/../etc/passwd"}}}}}
	for id, want := range map[uint32]string{7: "/media/KDATA/Contents/a b.flac", 8: "", 9: ""} {
		if got := e.TrackPath(id); got != want {
			t.Errorf("track %d: %q, want %q", id, got, want)
		}
	}
	if got := (&Exporter{}).TrackPath(7); got != "" {
		t.Errorf("not exporting: %q", got)
	}
}

// #100: a deck without a USB library shares its EngineX OS streaming downloads:
// a CDJ browses the "EngineX Streaming" folder over dbserver, then reads the
// grid and the audio of /EngineX_Stream/<name> over NFS.
func TestExportStreamingWithoutUSB(t *testing.T) {
	base := t.TempDir()
	stream, sd := filepath.Join(base, "Stream"), filepath.Join(base, "sd", "Stream")
	db := filepath.Join(base, "AIR Music Technology", "Engine", "m.db")
	saved := []any{StreamDirs, InternalEngineDB, StreamExportRoot}
	StreamDirs, InternalEngineDB, StreamExportRoot = []string{stream, sd}, db, filepath.Join(base, "prolink-export")
	t.Cleanup(func() {
		StreamDirs, InternalEngineDB, StreamExportRoot = saved[0].([]string), saved[1].(string), saved[2].(string)
	})
	cloud := testaudio.WAV(testaudio.Song(3))
	for path, body := range map[string][]byte{
		filepath.Join(stream, "Artist A - Cloud Song.wav"): cloud,
		filepath.Join(stream, ".sources.json"):             []byte(`{"soundcloud:42":"Artist A - Cloud Song.wav"}`),
		filepath.Join(stream, "half.mp3.part"):             []byte("partial download"),
		filepath.Join(sd, "Artist A - Cloud Song.wav"):     []byte("older copy, shadowed"),
		filepath.Join(sd, "Netease Track.mp3"):             bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x64}, 1000),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil || os.WriteFile(path, body, 0o644) != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(stream, "escape.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	engine, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Exec(`CREATE TABLE Track (id INTEGER PRIMARY KEY, title TEXT, artist TEXT, bpm INTEGER, bpmAnalyzed REAL, length INTEGER, path TEXT, filename TEXT);
		INSERT INTO Track VALUES(1, '网易云曲目', '云音乐', 126, 126.5, 200, '../../../../sc5000-custom/Stream/Netease Track.mp3', 'Netease Track.mp3');`)
	engine.Close()
	if err != nil {
		t.Fatal(err)
	}

	cfg := testExportConfig("")
	if roots := MountedExports(); len(roots) == 0 || roots[0] != StreamExportRoot {
		// This host mounts something under /media; export the fallback root by name.
		if len(roots) == 0 || !strings.HasPrefix(roots[0], "/media/") {
			t.Fatalf("no-USB fallback root: %v", roots)
		}
		if err := os.MkdirAll(StreamExportRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.Root = StreamExportRoot
	}
	e := NewExporter(cfg)
	defer e.Close()
	if err := e.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if st := e.Status(); st.Root != StreamExportRoot {
		t.Fatalf("export root %q", st.Root)
	}
	waitNumber(t, e, 4, 300*time.Millisecond)
	c := dbConnect(t, e)
	tx := uint32(0)
	query := func(kind uint16, args ...uint32) []dbMessage { t.Helper(); return dbMenu(t, c, &tx, kind, args...) }
	items := query(0x1105, 0, 0, 1)
	if len(items) != 1 || fieldText(items[0].args[3]) != StreamFolder || items[0].args[6].n != 1 {
		t.Fatalf("root playlists: %d", len(items))
	}
	var names []string
	ids := map[string]uint32{}
	for _, it := range query(0x1105, 0, items[0].args[1].n, 1) {
		names = append(names, fieldText(it.args[3]))
		ids[fieldText(it.args[3])] = it.args[1].n
	}
	if strings.Join(names, "|") != "All Streaming|Downloaded Stream|SoundCloud" {
		t.Fatalf("streaming lists %q", names)
	}
	cloudID := query(0x1105, 0, ids["SoundCloud"], 0)[0].args[1].n
	if all := query(0x1105, 0, ids["All Streaming"], 0); len(all) != 2 {
		t.Fatalf("all streaming: %d tracks", len(all))
	}
	path := fieldText(query(0x2102, cloudID)[0].args[3])
	if path != "/EngineX_Stream/Artist A - Cloud Song.wav" {
		t.Fatalf("track path %q", path)
	}

	nfs := &NFSClient{IP: net.IPv4(127, 63, 0, 1), Portmap: e.Status().PortmapPort, Timeout: 200 * time.Millisecond}
	if got, _, err := nfs.Fetch(t.Context(), "/C/", path, 1<<26); err != nil || !bytes.Equal(got, cloud) {
		t.Fatalf("NFS audio: %v (%d bytes)", err, len(got))
	}
	for _, bad := range []string{"/EngineX_Stream/escape.mp3", "/EngineX_Stream/../../../etc/passwd", "/EngineX_Stream/half.mp3.part/x"} {
		if _, _, err := nfs.Fetch(t.Context(), "/C/", bad, 1<<20); err == nil {
			t.Fatalf("NFS served %s", bad)
		}
	}
	pdb, _, err := nfs.Fetch(t.Context(), "/C/", "/PIONEER/rekordbox/export.pdb", 1<<26)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := rekordbox.ParsePDB(bytes.NewReader(pdb), int64(len(pdb)))
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]rekordbox.Track{}
	for _, tr := range lib.Tracks {
		byTitle[tr.Title] = tr
	}
	if n := byTitle["网易云曲目"]; n.Artist != "云音乐" || n.BPM != 126.5 || n.Duration != 200 || n.Path != "/EngineX_Stream/Netease Track.mp3" {
		t.Fatalf("Engine metadata: %+v", n)
	}
	cl := byTitle["Cloud Song"]
	if cl.Artist != "Artist A" || cl.Duration != 3 || cl.SampleRate != testaudio.Rate || cl.FileSize != int64(len(cloud)) || cl.Comment != "SoundCloud" {
		t.Fatalf("file metadata: %+v", cl)
	}
	anlz, _, err := nfs.Fetch(t.Context(), "/C/", cl.AnalysisPath, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	a, err := rekordbox.ParseAnalysis(anlz)
	if err != nil {
		t.Fatal(err)
	}
	if _, beats, _ := ExtractANLZ(a); len(beats) < 4 {
		t.Fatalf("grid: %d beats", len(beats))
	}
	if got := e.TrackPath(cloudID); got != filepath.Join(stream, "Artist A - Cloud Song.wav") {
		t.Fatalf("TrackPath %q", got)
	}
	e.Close()

	// An evicted download leaves the export; the others keep their ids.
	pdbPath := filepath.Join(StreamExportRoot, "PIONEER", "rekordbox", "export.pdb")
	before, _ := os.Stat(pdbPath)
	if err := SyncEngineToPioneerExport(StreamExportRoot); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(pdbPath); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("unchanged downloads rebuilt export.pdb")
	}
	if err := os.Remove(filepath.Join(sd, "Netease Track.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := SyncEngineToPioneerExport(StreamExportRoot); err != nil {
		t.Fatal(err)
	}
	lib, err = rekordbox.ReadPDB(pdbPath)
	if err != nil {
		t.Fatal(err)
	}
	var lists []string
	for _, p := range lib.Playlists {
		lists = append(lists, p.Name)
	}
	if len(lib.Tracks) != 1 || lib.Tracks[0].ID != cl.ID || strings.Join(lists, "|") != "EngineX Streaming|All Streaming|SoundCloud" {
		t.Fatalf("after eviction: %d tracks, lists %q", len(lib.Tracks), lists)
	}
}
