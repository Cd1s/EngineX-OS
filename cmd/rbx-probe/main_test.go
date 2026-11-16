package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRedactorMasksUserHostIPAndPath(t *testing.T) {
	r := &redactor{
		user:  "alice",
		home:  "/Users/alice",
		host:  "alices-macbook",
		ips:   map[string]string{},
		macs:  map[string]string{},
		dirs:  map[string]string{},
		files: map[string]string{},
		allowed: map[string]bool{
			".": true, "..": true,
			"Users": true, "Music": true, "rekordbox": true,
		},
	}

	got := r.path("/Users/alice/Music/rekordbox/SecretFolder/01 Track Name.flac")
	want := "/Users/<USER>/Music/rekordbox/<DIR_1>/<FILE_1.flac>"
	if got != want {
		t.Fatalf("path() = %q, want %q", got, want)
	}

	ipLabel := r.ip(net.IPv4(192, 168, 1, 42))
	if ipLabel != "<LAN_IP_1>" {
		t.Fatalf("ip() = %q, want <LAN_IP_1>", ipLabel)
	}
	if r.ip(net.IPv4(127, 0, 0, 1)) != "127.0.0.1" {
		t.Fatalf("loopback ip must remain 127.0.0.1")
	}

	errTxt := r.text("dial udp4 192.168.1.42:50111 on alices-macbook /Users/alice/x: timeout")
	for _, leak := range []string{"192.168.1.42", "alices-macbook", "alice"} {
		if strings.Contains(errTxt, leak) {
			t.Fatalf("text() leaked %q in %q", leak, errTxt)
		}
	}
}

func TestDecodeDBMsgOmittedEmptyBlobAndShortInts(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(dbMagic)
	buf.WriteByte(0x11)
	_ = binary.Write(&buf, binary.BigEndian, uint32(7))
	buf.WriteByte(0x10)
	_ = binary.Write(&buf, binary.BigEndian, uint16(0x4e02))
	buf.WriteByte(0x0f)
	buf.WriteByte(4)
	buf.Write([]byte{0x14, 0, 0, 0, 12})
	tags := [12]byte{0x06, 0x06, 0x06, 0x03}
	buf.Write(tags[:])
	// arg 0: u16 (0x10), arg 1: u8 (0x0f), arg 2: u32 = 0 (omits arg 3 blob!)
	buf.Write([]byte{0x10, 0x2b, 0x04})
	buf.Write([]byte{0x0f, 0x01})
	buf.Write([]byte{0x11, 0, 0, 0, 0})

	msg, err := decodeDBMsg(&buf)
	if err != nil {
		t.Fatalf("decodeDBMsg failed: %v", err)
	}
	if msg.tx != 7 || msg.kind != 0x4e02 || len(msg.args) != 4 {
		t.Fatalf("unexpected decoded msg: %+v", msg)
	}
	if msg.args[0].num != 0x2b04 || msg.args[1].num != 1 || msg.args[2].num != 0 {
		t.Fatalf("unexpected numeric args: %+v", msg.args)
	}
	if msg.args[3].tag != 0x03 || len(msg.args[3].blob) != 0 {
		t.Fatalf("expected empty omitted blob at arg 3, got %+v", msg.args[3])
	}
}

func TestTagLEAndBuildMediaQuery(t *testing.T) {
	if got := tagLE("DAT"); got != 0x00544144 {
		t.Fatalf("tagLE(DAT) = 0x%08x, want 0x00544144", got)
	}
	if got := tagLE("PQTZ"); got != 0x5a545150 {
		t.Fatalf("tagLE(PQTZ) = 0x%08x, want 0x5a545150", got)
	}

	mq := buildMediaQuery(1, net.IPv4(192, 168, 1, 42), 41, 4)
	if len(mq) != 0x30 || string(mq[:10]) != prolinkMagic || mq[0x0a] != 0x05 {
		t.Fatalf("bad media query header: len=%d", len(mq))
	}
	if mq[0x21] != 1 || mq[0x2b] != 41 || mq[0x2f] != 4 {
		t.Fatalf("unexpected media query fields: as=%d target=%d slot=%d", mq[0x21], mq[0x2b], mq[0x2f])
	}
	if !bytes.Equal(mq[0x24:0x28], []byte{192, 168, 1, 42}) {
		t.Fatalf("unexpected replyIP: %v", mq[0x24:0x28])
	}
}

func TestDecodeWireName(t *testing.T) {
	s, enc := decodeWireName(append(encodeUTF16LE("/"), 0, 0))
	if s != "/" || enc != "utf16le" {
		t.Fatalf("decodeWireName utf16le = (%q, %q)", s, enc)
	}
	s8, enc8 := decodeWireName([]byte("/Users/test\x00"))
	if s8 != "/Users/test" || enc8 != "utf8" {
		t.Fatalf("decodeWireName utf8 = (%q, %q)", s8, enc8)
	}
}

func TestQueryDBPortOnRejectsBannerAndParsesPort(t *testing.T) {
	lnBanner, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lnBanner.Close()
	go func() {
		c, err := lnBanner.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var req [32]byte
		_, _ = c.Read(req[:])
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n"))
	}()

	ip := net.IPv4(127, 0, 0, 1)
	if p, err := queryDBPortOn(ip, lnBanner.Addr().(*net.TCPAddr).Port, time.Second); err == nil {
		t.Fatalf("expected error on non-2-byte banner, got port=%d", p)
	}

	lnPort, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lnPort.Close()
	go func() {
		c, err := lnPort.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var req [19]byte
		_, _ = io.ReadFull(c, req[:])
		_, _ = c.Write([]byte{0x30, 0x39})
	}()

	got, err := queryDBPortOn(ip, lnPort.Addr().(*net.TCPAddr).Port, time.Second)
	if err != nil || got != 12345 {
		t.Fatalf("queryDBPortOn = (%d, %v), want (12345, nil)", got, err)
	}
}

func TestProbeDBFallbackDirectDBServer(t *testing.T) {
	// Closed port
	lnClosed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := lnClosed.Addr().(*net.TCPAddr).Port
	_ = lnClosed.Close()

	// Non-dbserver port
	lnOther, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lnOther.Close()
	go func() {
		for {
			c, err := lnOther.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var buf [16]byte
				_, _ = c.Read(buf[:])
				_, _ = c.Write([]byte("NOPE"))
			}(c)
		}
	}()

	// Direct dbserver port
	lnDB, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lnDB.Close()
	go func() {
		for {
			c, err := lnDB.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var hello [5]byte
				if _, err := io.ReadFull(c, hello[:]); err != nil {
					return
				}
				_, _ = c.Write(hello[:])
				msg, err := decodeDBMsg(c)
				if err != nil || msg.kind != kindSetup {
					return
				}
				_, _ = c.Write(encodeDBReq(msg.tx, kindSetupAck, dbNum(41)))
			}(c)
		}
	}()

	wantPort := lnDB.Addr().(*net.TCPAddr).Port
	got := probeDBFallback(net.IPv4(127, 0, 0, 1), 1, []int{closedPort, lnOther.Addr().(*net.TCPAddr).Port, wantPort}, 500*time.Millisecond, newRedactor())
	if got != wantPort {
		t.Fatalf("probeDBFallback() = %d, want %d", got, wantPort)
	}
}

func TestRPCCallUDPChosenSrcPort(t *testing.T) {
	startMock := func(seen chan<- int) int {
		pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pc.Close() })
		go func() {
			buf := make([]byte, 2048)
			for {
				n, raddr, err := pc.ReadFromUDP(buf)
				if err != nil {
					return
				}
				if n < 4 {
					continue
				}
				select {
				case seen <- raddr.Port:
				default:
				}
				var rep xdrBuf
				rep.Write(buf[:4])              // xid
				rep.u32(1).u32(0).u32(0).u32(0) // REPLY, MSG_ACCEPTED, AUTH_NULL
				rep.u32(0)                      // accept_stat = SUCCESS
				_, _ = pc.WriteToUDP(rep.Bytes(), raddr)
			}
		}()
		return pc.LocalAddr().(*net.UDPAddr).Port
	}

	seen1 := make(chan int, 1)
	seen2 := make(chan int, 1)
	p1 := startMock(seen1)
	p2 := startMock(seen2)

	probeSock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	src := probeSock.LocalAddr().(*net.UDPAddr).Port
	_ = probeSock.Close()

	ip := net.IPv4(127, 0, 0, 1)
	if _, err := rpcCallUDP(ip, p1, src, rpcMount, 1, 0, true, nil, time.Second); err != nil {
		t.Fatalf("rpcCallUDP #1 failed: %v", err)
	}
	if _, err := rpcCallUDP(ip, p2, src, rpcNFS, 2, 0, false, nil, time.Second); err != nil {
		t.Fatalf("rpcCallUDP #2 failed: %v", err)
	}
	if got1, got2 := <-seen1, <-seen2; got1 != src || got2 != src {
		t.Fatalf("observed src ports (%d, %d), want %d", got1, got2, src)
	}
}

func TestBuildVirtualCDJKeepAlive(t *testing.T) {
	mac := [6]byte{0x02, 0x52, 0x42, 0x58, 0x00, 0x04}
	ip := net.IPv4(192, 168, 1, 50)
	pkt := buildVirtualCDJKeepAlive(4, ip, mac)
	if len(pkt) != 0x36 || string(pkt[:10]) != prolinkMagic || pkt[0x0a] != 0x06 {
		t.Fatalf("bad keepalive header: len=%d type=0x%02x", len(pkt), pkt[0x0a])
	}
	if !strings.HasPrefix(string(pkt[0x0c:0x20]), "CDJ-3000\x00") {
		t.Fatalf("unexpected device name: %q", pkt[0x0c:0x20])
	}
	if pkt[0x20] != 0x01 || pkt[0x21] != 0x03 || binary.BigEndian.Uint16(pkt[0x22:0x24]) != 0x0036 {
		t.Fatalf("unexpected version/len bytes: %x", pkt[0x20:0x24])
	}
	if pkt[0x24] != 4 || pkt[0x25] != 0x01 || !bytes.Equal(pkt[0x26:0x2c], mac[:]) || !bytes.Equal(pkt[0x2c:0x30], ip.To4()) {
		t.Fatalf("unexpected identity bytes: %x", pkt[0x24:0x30])
	}
	if pkt[0x30] != 0x01 || pkt[0x31] != 0x00 || pkt[0x34] != 0x01 || pkt[0x35] != 0x64 {
		t.Fatalf("unexpected tail bytes: %x", pkt[0x30:0x36])
	}
}

func TestDecodeDBMsgCompactTagListAndSetupServerPlayer(t *testing.T) {
	// Exact rekordbox 7 setup-ack framing observed live: argc=2 with 2-byte tag list (not 12).
	var buf bytes.Buffer
	buf.Write(dbMagic)
	buf.WriteByte(0x11)
	_ = binary.Write(&buf, binary.BigEndian, uint32(0xfffffffe))
	buf.WriteByte(0x10)
	_ = binary.Write(&buf, binary.BigEndian, uint16(kindSetupAck))
	buf.WriteByte(0x0f)
	buf.WriteByte(2)
	buf.Write([]byte{0x14, 0, 0, 0, 2})
	buf.Write([]byte{0x06, 0x06})
	buf.Write([]byte{0x11, 0, 0, 0, 0})
	buf.Write([]byte{0x11, 0, 0, 0, 17})

	msg, err := decodeDBMsg(&buf)
	if err != nil {
		t.Fatalf("decodeDBMsg compact setup ack failed: %v", err)
	}
	if msg.tx != 0xfffffffe || msg.kind != kindSetupAck || len(msg.args) != 2 {
		t.Fatalf("unexpected decoded msg: %+v", msg)
	}
	if got := setupServerPlayer(msg); got != 17 {
		t.Fatalf("setupServerPlayer() = %d, want 17", got)
	}

	oneArg := &dbMsg{kind: kindSetupAck, args: []dbField{{tag: 0x06, num: 41}}}
	if got := setupServerPlayer(oneArg); got != 41 {
		t.Fatalf("setupServerPlayer(1-arg) = %d, want 41", got)
	}

	// Reject malformed header where tag list is shorter than argc.
	bad := []byte{
		0x11, 0x87, 0x23, 0x49, 0xae,
		0x11, 0xff, 0xff, 0xff, 0xfe,
		0x10, 0x40, 0x00,
		0x0f, 0x02,
		0x14, 0, 0, 0, 1,
		0x06,
	}
	if _, err := decodeDBMsg(bytes.NewReader(bad)); err == nil {
		t.Fatalf("expected error for tagLen < argc")
	}
}

func TestRenderMenuHandlesNoResultsSentinel(t *testing.T) {
	if got := fmtCount(0xffffffff); got != "0xffffffff (none)" {
		t.Fatalf("fmtCount(0xffffffff) = %q", got)
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	wrote := make(chan int, 1)
	go func() {
		var buf [16]byte
		n, _ := c2.Read(buf[:])
		wrote <- n
	}()

	dc := &dbConn{c: c1}
	rows, err := dc.renderMenu(1, 1, 4, 1, dmst(1, 1, 4, 1), 0xffffffff, 32, 200*time.Millisecond)
	if err != nil || len(rows) != 0 {
		t.Fatalf("renderMenu(0xffffffff) = (%v, %v), want empty nil", rows, err)
	}
	_ = c1.Close()
	if n := <-wrote; n != 0 {
		t.Fatalf("renderMenu(0xffffffff) wrote %d bytes to wire, want 0", n)
	}
}

func TestBuildMetadataAndTeardownReqBytes(t *testing.T) {
	if got := dmst(1, menuMain, slotCollection, trackTypeRekordbox); got != 0x01010401 {
		t.Fatalf("dmst(collection rekordbox) = 0x%08x, want 0x01010401", got)
	}

	gotMeta := buildMetadataReq(1, 1, menuMain, slotCollection, trackTypeRekordbox, 0x004f2a9b)
	wantMeta := []byte{
		0x11, 0x87, 0x23, 0x49, 0xae,
		0x11, 0x00, 0x00, 0x00, 0x01,
		0x10, 0x20, 0x02,
		0x0f, 0x02,
		0x14, 0x00, 0x00, 0x00, 0x0c,
		0x06, 0x06, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0x11, 0x01, 0x01, 0x04, 0x01,
		0x11, 0x00, 0x4f, 0x2a, 0x9b,
	}
	if !bytes.Equal(gotMeta, wantMeta) {
		t.Fatalf("buildMetadataReq bytes mismatch:\n got=%x\nwant=%x", gotMeta, wantMeta)
	}
	decMeta, err := decodeDBMsg(bytes.NewReader(gotMeta))
	if err != nil {
		t.Fatalf("decodeDBMsg(meta): %v", err)
	}
	if decMeta.tx != 1 || decMeta.kind != kindMetadata || len(decMeta.args) != 2 ||
		decMeta.args[0].num != 0x01010401 || decMeta.args[1].num != 0x004f2a9b {
		t.Fatalf("unexpected decoded meta request: %+v", decMeta)
	}

	gotTD := buildTeardownReq()
	wantTD := []byte{
		0x11, 0x87, 0x23, 0x49, 0xae,
		0x11, 0xff, 0xff, 0xff, 0xfe,
		0x10, 0x01, 0x00,
		0x0f, 0x00,
		0x14, 0x00, 0x00, 0x00, 0x0c,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}
	if !bytes.Equal(gotTD, wantTD) {
		t.Fatalf("buildTeardownReq bytes mismatch:\n got=%x\nwant=%x", gotTD, wantTD)
	}
	decTD, err := decodeDBMsg(bytes.NewReader(gotTD))
	if err != nil {
		t.Fatalf("decodeDBMsg(teardown): %v", err)
	}
	if decTD.tx != 0xfffffffe || decTD.kind != kindTeardown || len(decTD.args) != 0 {
		t.Fatalf("unexpected decoded teardown: %+v", decTD)
	}
}

func TestProbeDBSessionSequentialAndTeardown(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var mu sync.Mutex
	active := map[byte]int{}
	var sessions, teardowns, concurrentErrs, metadataCalls int
	var errMsgs []string

	dbStr := func(s string) dbField { return dbField{tag: 0x02, str: s} }
	makeItem := func(parent, id uint32, label string, kind, artID, pos uint32) []dbField {
		return []dbField{
			dbNum(parent), dbNum(id),
			dbNum(uint32(len(label)*2 + 2)), dbStr(label),
			dbNum(2), dbStr(""),
			dbNum(kind), dbNum(0), dbNum(artID), dbNum(pos),
		}
	}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				var hello [5]byte
				if _, err := io.ReadFull(c, hello[:]); err != nil {
					_ = c.Close()
					return
				}
				_, _ = c.Write(hello[:])

				var player byte
				var registered, sawTeardown bool
				defer func() {
					mu.Lock()
					if registered {
						active[player]--
						if !sawTeardown {
							errMsgs = append(errMsgs, "session closed without 0x0100 teardown")
						}
					}
					mu.Unlock()
					_ = c.Close()
				}()

				var rendered [][]dbField
				for {
					msg, err := decodeDBMsg(c)
					if err != nil {
						return
					}
					switch msg.kind {
					case kindSetup:
						as := byte(msg.args[0].num)
						if as == 41 {
							_, _ = c.Write(encodeDBReq(msg.tx, kindTeardown))
							return
						}
						mu.Lock()
						if active[as] > 0 {
							concurrentErrs++
							errMsgs = append(errMsgs, "concurrent session for same asking player")
						}
						active[as]++
						sessions++
						mu.Unlock()
						player, registered = as, true
						_, _ = c.Write(encodeDBReq(msg.tx, kindSetupAck, dbNum(0), dbNum(17)))
					case kindTeardown:
						mu.Lock()
						sawTeardown = true
						teardowns++
						mu.Unlock()
						return
					case kindRootMenu:
						rendered = [][]dbField{makeItem(0, 4, "TRACK", 0x0083, 0, 1)}
						_, _ = c.Write(encodeDBReq(msg.tx, kindSetupAck, dbNum(uint32(msg.kind)), dbNum(uint32(len(rendered)))))
					case kindAllTracks:
						slot := byte(msg.args[0].num >> 8)
						if slot == slotCollection {
							rendered = [][]dbField{makeItem(0, 5188251, "Test Track", 0x0004, 5188251, 1)}
						} else {
							rendered = nil
						}
						_, _ = c.Write(encodeDBReq(msg.tx, kindSetupAck, dbNum(uint32(msg.kind)), dbNum(uint32(len(rendered)))))
					case kindPlaylist:
						rendered = nil
						_, _ = c.Write(encodeDBReq(msg.tx, kindSetupAck, dbNum(uint32(msg.kind)), dbNum(0xffffffff)))
					case kindMetadata:
						mu.Lock()
						metadataCalls++
						mu.Unlock()
						if msg.args[0].num != dmst(1, menuMain, slotCollection, trackTypeRekordbox) {
							mu.Lock()
							errMsgs = append(errMsgs, "unexpected 0x2002 dmst")
							mu.Unlock()
							_, _ = c.Write(encodeDBReq(msg.tx, kindUnavailable, dbNum(uint32(msg.kind))))
							continue
						}
						rendered = [][]dbField{makeItem(0, 5188251, "Test Track", 0x0004, 5188251, 0)}
						_, _ = c.Write(encodeDBReq(msg.tx, kindSetupAck, dbNum(uint32(msg.kind)), dbNum(uint32(len(rendered)))))
					case kindTrackInfo:
						rendered = [][]dbField{makeItem(0, 5188251, "/Music/test.flac", 0x0000, 0, 0)}
						_, _ = c.Write(encodeDBReq(msg.tx, kindSetupAck, dbNum(uint32(msg.kind)), dbNum(uint32(len(rendered)))))
					case kindRender:
						_, _ = c.Write(encodeDBReq(msg.tx, kindRenderHdr, dbNum(1), dbNum(0)))
						for _, fields := range rendered {
							_, _ = c.Write(encodeDBReq(msg.tx, kindRenderItem, fields...))
						}
						_, _ = c.Write(encodeDBReq(msg.tx, kindRenderFoot))
					default:
						_, _ = c.Write(encodeDBReq(msg.tx, kindUnavailable, dbNum(uint32(msg.kind))))
					}
				}
			}(c)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	paths := probeDBSession(net.IPv4(127, 0, 0, 1), port, 1, 2, 0, time.Second, false, newRedactor())
	mu.Lock()
	defer mu.Unlock()
	if concurrentErrs > 0 || len(errMsgs) > 0 {
		t.Fatalf("protocol errors: concurrent=%d msgs=%v", concurrentErrs, errMsgs)
	}
	if sessions == 0 || teardowns != sessions {
		t.Fatalf("teardowns=%d sessions=%d, want equal and >0", teardowns, sessions)
	}
	if metadataCalls != 1 {
		t.Fatalf("metadataCalls=%d, want 1", metadataCalls)
	}
	if len(paths) == 0 || paths[0] != "/Music/test.flac" {
		t.Fatalf("paths=%v, want /Music/test.flac", paths)
	}
}

func TestSortSummaryShowsASecondColumn(t *testing.T) {
	got := sortSummary([]menuRow{{kind: 4, label: "abc"}, {kind: 0x704, label: "de", sec: "xyz"}})
	if got != "rows=[kind=0x0004 title_len=3 second_len=0; kind=0x0704 title_len=2 second_len=3]" {
		t.Fatal(got)
	}
	if sortSummary(nil) != "rows=[]" {
		t.Fatal("no rows")
	}
}

func TestSortDetailShowsTempoAndKeyTextOnly(t *testing.T) {
	rows := []menuRow{
		{kind: 0x0704, label: "secret title", sec: "secret artist", secRaw: "secret artist"},
		{kind: 0x0d04, secRaw: "\ufffa128.00\ufffb", parent: 12800, id: 7},
		{kind: 0x0f04, secRaw: "8A", flags: 3},
	}
	got := sortDetail(rows)
	if strings.Contains(got, "secret") || !strings.Contains(got, `second="\ufffa128.00\ufffb" parent=12800`) || !strings.Contains(got, `kind=0x0f04 second="8A" parent=0 flags=3`) {
		t.Fatal(got)
	}
	if sortDetail(rows[:1]) != "" {
		t.Fatal("artist text printed")
	}
}
