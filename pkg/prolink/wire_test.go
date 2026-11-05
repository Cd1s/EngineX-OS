package prolink

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestCapturedWire(t *testing.T) {
	var data struct {
		Fixtures map[string]struct{ Hex string }
	}
	b, err := os.ReadFile("testdata/captured-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	fixture := func(key string) []byte {
		t.Helper()
		b, err := hex.DecodeString(data.Fixtures[key].Hex)
		if err != nil || len(b) == 0 {
			t.Fatalf("fixture %s: %v", key, err)
		}
		return b
	}
	for _, key := range []string{"0x0", "0x1000", "0x1105", "0x4000", "0x4001", "0x4101", "0x4201", "0x2102", "path"} {
		raw := fixture(key)
		m, err := readDBMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(key, err)
		}
		if !bytes.Equal(m.bytes(), raw) {
			t.Fatalf("captured %s does not roundtrip", key)
		}
	}
	raw := fixture("0x4101")
	s := DBServer{Library: &rekordbox.Library{}}
	items, err := s.query(dbMessage{kind: 0x1000, args: []dbField{dbNumber(0x01010301), dbNumber(0), dbNumber(0xffffff)}})
	if err != nil || !bytes.Equal((dbMessage{0x038001a9, 0x4101, items[0].fields()}).bytes(), raw) {
		t.Fatal("root item differs from captured hardware")
	}
	path := fixture("path")
	m, err := readDBMessage(bytes.NewReader(path))
	if err != nil {
		t.Fatal(err)
	}
	s.Library.Tracks = []rekordbox.Track{{ID: 200, FileSize: 7633531, Path: m.args[3].text(), AnalysisPath: "/not-a-second-item.DAT"}}
	s.tracks = map[uint32]rekordbox.Track{200: s.Library.Tracks[0]}
	items, err = s.query(dbMessage{kind: 0x2102, args: []dbField{dbNumber(0x01080301), dbNumber(200)}})
	if err != nil || len(items) != 1 || !bytes.Equal((dbMessage{m.tx, 0x4101, items[0].fields()}).bytes(), path) {
		t.Fatal("audio path differs from captured hardware")
	}
	keep := fixture("keepalive")
	r := &exportRun{player: keep[0x24], first: true, ip: net.IP(keep[0x2c:0x30]), mac: net.HardwareAddr(keep[0x26:0x2c]), peers: map[string]exportPeer{"peer": {}}}
	got := r.announcement()
	copy(keep[12:32], got[12:32]) // Only the model name differs.
	if !bytes.Equal(got, keep) {
		t.Fatalf("keepalive: got %x want %x", got, keep)
	}
	status := r.statusPacket()
	if status[0x75] != 1 || status[0x6f] != 0 || status[0x89]&0x70 != 0 {
		t.Fatal("export must advertise mounted USB/LINK without playback/master/sync")
	}
	captured := fixture("status")
	d, err := ParsePacket(captured, 50002, net.IPv4(1, 2, 3, 4))
	if err != nil || d.Name != "CDJ-2000nexus" {
		t.Fatal("captured status name offset", d, err)
	}
	if len(status) != len(captured) || len(status) != 0x11c || !bytes.Equal(status[:11], captured[:11]) || status[0x1f] != captured[0x1f] || status[0x20] != captured[0x20] || !bytes.Equal(status[0x22:0x24], captured[0x22:0x24]) {
		t.Fatal("status framing differs from capture")
	}
	r.nfs = &NFSServer{Root: "/media/test"}
	r.lib = &rekordbox.Library{}
	media := r.mediaPacket()
	captured = fixture("media")
	for _, off := range []int{0xa6, 0xae} {
		if binary.BigEndian.Uint16(media[off:]) != 0 {
			t.Fatal("nonempty counts")
		}
	}
	if len(media) != len(captured) || len(media) != 0xc0 || !bytes.Equal(media[:11], captured[:11]) || media[0x1f] != captured[0x1f] || !bytes.Equal(media[0x22:0x24], captured[0x22:0x24]) || media[0x2b] != captured[0x2b] || media[0xaa] != captured[0xaa] {
		t.Fatal("media framing differs from capture")
	}
}

func TestNFSMalformed32BitLengthsAndIndexes(t *testing.T) {
	for _, n := range []uint32{0x7fffffff, 0x80000000, 0xffffffff} {
		r := reader{b: binary.BigEndian.AppendUint32(nil, n)}
		if r.opaque(4096) != nil || r.err == nil {
			t.Fatal("oversized opaque accepted")
		}
	}
	s := &NFSServer{Root: t.TempDir()}
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fh := s.fh("")
	for _, n := range []uint32{0x80000000, 0xffffffff} {
		bad := append([]byte(nil), fh...)
		binary.BigEndian.PutUint32(bad[4:], n)
		if _, ok := s.lookupFH(bad); ok {
			t.Fatal("invalid handle accepted")
		}
		call := &xdr{}
		call.u32(1).u32(0).u32(2).u32(rpcNFS).u32(2).u32(16).u32(0).opaque(nil).u32(0).opaque(nil)
		call.Write(fh)
		call.u32(n).u32(8192)
		if b := s.handle(call.Bytes(), 0); len(b) == 0 {
			t.Fatal("invalid cookie panicked or lost reply")
		}
	}
}

func TestExportPeerWindowRecoveryAndStickyNumber(t *testing.T) {
	now := time.Now()
	r := &exportRun{player: 3, probed: true, mounted: true, peers: map[string]exportPeer{"four": {number: 4, seen: now.Add(-11 * time.Second)}}}
	r.chooseLocked(now)
	if r.player != 3 {
		t.Fatal("expired higher number interrupted active session")
	}
	r.floodUntil = now.Add(10 * time.Second)
	r.chooseLocked(now)
	if r.player != 0 {
		t.Fatal("flood must pause")
	}
	r.chooseLocked(now.Add(11 * time.Second))
	if r.player != 4 {
		t.Fatal("flood did not recover")
	}
}
