package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
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
