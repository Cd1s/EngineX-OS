package prolink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeRekordbox answers like the computer cmd/rbx-probe talked to: a port query, then one
// compact-framed dbserver session. rows maps a menu request to the rows it lists.
type fakeRekordbox struct {
	t     *testing.T
	rows  map[uint16][]dbItem
	none  uint16 // a request answered with "no results"
	blobs map[uint16][]byte
	mu    sync.Mutex
	seen  []dbMessage
	bye   chan struct{}
	query net.Listener
}

func newFakeRekordbox(t *testing.T, rows map[uint16][]dbItem) *fakeRekordbox {
	f := &fakeRekordbox{t: t, rows: rows, bye: make(chan struct{})}
	db, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.query, err = net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); f.query.Close() })
	go func() {
		for {
			c, err := f.query.Accept()
			if err != nil {
				return
			}
			io.ReadFull(c, make([]byte, 19))
			c.Write(binary.BigEndian.AppendUint16(nil, uint16(db.Addr().(*net.TCPAddr).Port)))
			c.Close()
		}
	}()
	go func() {
		for {
			c, err := db.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeRekordbox) port() int { return f.query.Addr().(*net.TCPAddr).Port }

func (f *fakeRekordbox) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	hello := make([]byte, 5)
	io.ReadFull(r, hello)
	c.Write(hello)
	var pending []dbItem
	for {
		m, err := readDBMessage(r)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.seen = append(f.seen, m)
		f.mu.Unlock()
		switch m.kind {
		case 0:
			c.Write(compactWire(m.tx, 0x4000, dbNumber(0), dbNumber(17)))
		case 0x0100:
			close(f.bye)
			return
		case 0x3000:
			start, n := m.args[1].n, m.args[2].n
			c.Write(compactWire(m.tx, 0x4001, dbNumber(0x3000), dbNumber(n)))
			for _, it := range pending[start : start+n] {
				c.Write(compactWire(m.tx, 0x4101, it.fields()...))
			}
			c.Write(compactWire(m.tx, 0x4201, dbNumber(0x3000), dbNumber(0)))
		case 0x2204, 0x2004, 0x2904, 0x2104, 0x2b04, 0x2c04:
			blob := f.blobs[m.kind]
			reply := map[uint16]uint16{0x2204: 0x4602, 0x2004: 0x4402, 0x2904: 0x4a02, 0x2104: 0x4702, 0x2b04: 0x4e02, 0x2c04: 0x4f02}[m.kind]
			status := uint32(0)
			if len(blob) == 0 {
				status = 50 // not available
			}
			c.Write(compactWire(m.tx, reply, dbNumber(uint32(m.kind)), dbNumber(status), dbNumber(uint32(len(blob))), dbField{tag: 0x14, data: blob}, dbNumber(1)))
		default:
			pending = f.rows[m.kind]
			if m.kind == f.none {
				c.Write(compactWire(m.tx, 0x4000, dbNumber(uint32(m.kind)), dbNumber(0xffffffff)))
				continue
			}
			c.Write(compactWire(m.tx, 0x4000, dbNumber(uint32(m.kind)), dbNumber(uint32(len(pending)))))
		}
	}
}

func TestRekordboxLinkBrowsesAndReadsThePath(t *testing.T) {
	var tracks []dbItem
	for i := uint32(1); i <= 150; i++ {
		tracks = append(tracks, dbItem{id: i, kind: 4, label: "Track " + strconv.Itoa(int(i))})
	}
	f := newFakeRekordbox(t, map[uint16][]dbItem{
		0x1004: tracks,
		0x1105: {{id: 7, kind: RekordboxFolder, label: "Sets"}, {id: 9, kind: RekordboxPlaylist, label: "\ufffaWarm up\ufffb"}},
		// What rekordbox lists for a track; the row of kind 0 is the audio path.
		0x2102: {{id: 1, kind: 4}, {id: 392, kind: 0xb}, {id: 5, kind: 0, label: "/Users/dj/Music/A B/ü.mp3"}, {id: 1, kind: 0x2f}},
	})
	l, err := dialRekordbox(context.Background(), net.IPv4(127, 0, 0, 1), f.port(), 0)
	if err != nil {
		t.Fatal(err)
	}
	all, err := l.Tracks()
	if err != nil || len(all) != 150 || all[149].ID != 150 || all[0].Label != "Track 1" {
		t.Fatalf("tracks: %d %v", len(all), err)
	}
	lists, err := l.Playlists(0)
	if err != nil || len(lists) != 2 || lists[0].Kind != RekordboxFolder || lists[1].Label != "Warm up" || lists[1].ID != 9 {
		t.Fatalf("playlists: %+v %v", lists, err)
	}
	path, err := l.Path(5)
	if err != nil || path != "/Users/dj/Music/A B/ü.mp3" {
		t.Fatalf("path: %q %v", path, err)
	}
	l.Close()
	select {
	case <-f.bye:
	case <-time.After(2 * time.Second):
		t.Fatal("no 0x0100 teardown")
	}
	renders, where := 0, uint32(5<<24|1<<16|4<<8|1)
	for _, m := range f.seen {
		if m.kind == 0x3000 {
			renders++
		}
		if m.kind != 0 && m.kind != 0x0100 && m.args[0].n != where {
			t.Fatalf("0x%04x asks %#x, want %#x", m.kind, m.args[0].n, where)
		}
	}
	if renders != 3+1+1 { // 150 tracks in chunks of 64, then the playlists, then the path
		t.Fatalf("%d renders", renders)
	}
	if f.seen[0].kind != 0 || f.seen[0].tx != 0xfffffffe || f.seen[0].args[0].n != 5 {
		t.Fatalf("session setup: %+v", f.seen[0])
	}
}

func TestRekordboxLinkNoResultsAndLinkOff(t *testing.T) {
	f := newFakeRekordbox(t, nil)
	f.none = 0x1004
	l, err := dialRekordbox(context.Background(), net.IPv4(127, 0, 0, 1), f.port(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if all, err := l.Tracks(); err != nil || len(all) != 0 {
		t.Fatalf("no results: %v %v", all, err)
	}
	if _, err := l.Path(5); err == nil {
		t.Fatal("a track with no path rows has a path")
	}

	off, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer off.Close()
	go func() {
		c, _ := off.Accept()
		io.ReadFull(c, make([]byte, 19))
		c.Write([]byte{0xff, 0xff})
		c.Close()
	}()
	if _, err := dialRekordbox(context.Background(), net.IPv4(127, 0, 0, 1), off.Addr().(*net.TCPAddr).Port, 0); err == nil {
		t.Fatal("LINK off accepted")
	}
}

func TestParsePacketRekordboxStatus(t *testing.T) {
	b := make([]byte, 56)
	copy(b, magic)
	b[10] = 0x29
	copy(b[11:], "rekordbox")
	b[0x21] = 17
	d, err := ParsePacket(b, 50002, net.IPv4(192, 168, 1, 9))
	if err != nil || d.Number != 17 || !IsRekordbox(d) || d.PacketType != 0x29 || d.Address != "192.168.1.9" {
		t.Fatalf("rekordbox 0x29: %+v %v", d, err)
	}
	copy(b[11:], "DJM-900NXS2\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	if d, err = ParsePacket(b, 50002, nil); err != nil || d.Number != 17 {
		t.Fatalf("a computer's own number is enough where its name is not rekordbox: %+v %v", d, err)
	}
	b[0x21] = 33
	if _, err := ParsePacket(b, 50002, nil); err == nil {
		t.Fatal("a mixer's 0x29 was not accepted before and must not start to be")
	}
	keep := make([]byte, 54)
	copy(keep, magic)
	keep[10] = 6
	copy(keep[12:], "rekordbox")
	keep[0x24] = 17
	if d, err = ParsePacket(keep, 50000, nil); err != nil || !IsRekordbox(d) || d.Number != 17 {
		t.Fatalf("rekordbox keepalive: %+v %v", d, err)
	}
}

func TestRekordboxLinkReadsAnalysisBlobs(t *testing.T) {
	big := bytes.Repeat([]byte{7}, 200000) // PWV7 of a long track: more than a menu field may hold
	f := newFakeRekordbox(t, nil)
	f.blobs = map[uint16][]byte{0x2204: {1, 2, 3}, 0x2c04: big, 0x2104: nil}
	l, err := dialRekordbox(context.Background(), net.IPv4(127, 0, 0, 1), f.port(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if b, err := l.BeatGrid(5); err != nil || !bytes.Equal(b, []byte{1, 2, 3}) {
		t.Fatalf("beat grid: %v %v", b, err)
	}
	if b, err := l.Analysis(5, "PWV7", "2EX"); err != nil || !bytes.Equal(b, big) {
		t.Fatalf("PWV7: %d bytes %v", len(b), err)
	}
	// Nothing for the track: no data and no error, and the session goes on.
	if b, err := l.Cues(5); err != nil || len(b) != 0 {
		t.Fatalf("no cues: %v %v", b, err)
	}
	if b, err := l.BeatGrid(5); err != nil || len(b) != 3 {
		t.Fatalf("after the empty answer: %v %v", b, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.seen {
		if m.kind == 0x2c04 && (m.args[2].n != 0x37565750 || m.args[3].n != 0x00584532) { // "PWV7", "2EX" little-endian
			t.Fatalf("ANLZ tag request: %+v", m.args)
		}
		if m.kind == 0x2204 && m.args[0].n != 5<<24|8<<16|4<<8|1 {
			t.Fatalf("beat grid asks on menu %#x", m.args[0].n)
		}
	}
}

func TestRekordboxLinkAsksAsTheDecksOwnPlayer(t *testing.T) {
	for player, want := range map[byte]uint32{0: 5, 2: 2, 4: 4} { // a deck with no number asks as 5
		f := newFakeRekordbox(t, map[uint16][]dbItem{0x1004: {{id: 1, kind: 4, label: "One"}}})
		l, err := dialRekordbox(context.Background(), net.IPv4(127, 0, 0, 1), f.port(), player)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.Tracks(); err != nil {
			t.Fatal(err)
		}
		l.Close()
		f.mu.Lock()
		for _, m := range f.seen {
			if m.kind == 0 && m.args[0].n != want || m.kind == 0x1004 && m.args[0].n>>24 != want {
				t.Fatalf("player %d: 0x%04x asks as %#x, want %d", player, m.kind, m.args[0].n, want)
			}
		}
		f.mu.Unlock()
	}
}
