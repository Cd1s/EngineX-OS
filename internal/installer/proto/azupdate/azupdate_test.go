package azupdate_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/azupdate"
)

func buildReply(cmd uint32, seq uint32, status uint16, pktLen uint16) []byte {
	buf := make([]byte, azupdate.ReplySize)
	copy(buf[0:4], azupdate.ReplyMagic)
	binary.LittleEndian.PutUint32(buf[4:8], cmd)
	binary.LittleEndian.PutUint64(buf[8:16], 0)
	binary.LittleEndian.PutUint32(buf[16:20], seq)
	binary.LittleEndian.PutUint16(buf[20:22], status)
	binary.LittleEndian.PutUint16(buf[22:24], pktLen)
	return buf
}

type fakeAZDevice struct {
	replies     [][]byte
	writes      [][]byte
	writeCall   int
	readCall    int
	failWriteAt int
	failReadAt  int
	shortAt     int
	writeErr    error
	readErr     error
}

func (d *fakeAZDevice) WriteBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	if ep != 0x01 {
		return 0, errors.New("wrong OUT endpoint")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d.writeCall++
	if d.failWriteAt == d.writeCall {
		return 0, d.writeErr
	}
	if d.shortAt == d.writeCall && len(data) > 0 {
		return len(data) - 1, nil
	}
	d.writes = append(d.writes, append([]byte(nil), data...))
	return len(data), nil
}

func (d *fakeAZDevice) ReadBulk(ctx context.Context, ep uint8, buf []byte) (int, error) {
	if ep != 0x81 {
		return 0, errors.New("wrong IN endpoint")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d.readCall++
	if d.failReadAt == d.readCall {
		return 0, d.readErr
	}
	if len(d.replies) == 0 {
		return 0, io.EOF
	}
	pkt := d.replies[0]
	d.replies = d.replies[1:]
	return copy(buf, pkt), nil
}

func TestHeaderAndReplyMatchSystemOneCapture(t *testing.T) {
	// Real Rane SYSTEM ONE (#116) trace, seq 0x0a, 64-byte payload.
	wantHdr, _ := hex.DecodeString(
		"415a304f04fba55a40000000000000000a00000000002d00" +
			"000000000000000001000000000000006461746100",
	)
	gotHdr, err := azupdate.EncodeWritePartHeader("data", 64, 0x0a)
	if err != nil {
		t.Fatalf("EncodeWritePartHeader: %v", err)
	}
	if !bytes.Equal(gotHdr, wantHdr) {
		t.Fatalf("header mismatch:\ngot:  %x\nwant: %x", gotHdr, wantHdr)
	}

	rawReply, _ := hex.DecodeString("415a304904fba55a00000000000000000a00000005001800")
	r, err := azupdate.ParseReply(rawReply, azupdate.CmdWritePart, 0x0a)
	if err != nil {
		t.Fatalf("ParseReply: %v", err)
	}
	if r.Seq != 0x0a || r.Status != azupdate.StatusReady || r.PacketLen != 24 {
		t.Fatalf("unexpected reply: %+v", r)
	}
}

func TestWriteDataStreams256KiBChunksAndZLP(t *testing.T) {
	data := append(bytes.Repeat([]byte{0x12, 0x34, 0x56, 0x78}, (256*1024)/4*2), []byte("tail")...) // 512 KiB + 4
	dev := &fakeAZDevice{
		replies: [][]byte{
			buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 24),
			buildReply(azupdate.CmdWritePart, 1, azupdate.StatusWritten, 24),
		},
	}
	cl, err := azupdate.New(dev, proto.Endpoints{In: 0x81, Out: 0x01})
	if err != nil {
		t.Fatal(err)
	}
	var progressCount int
	var last proto.Progress
	part, err := cl.WriteData(context.Background(), proto.Image{
		Data: bytes.NewReader(data),
		Size: int64(len(data)),
	}, func(p proto.Progress) {
		progressCount++
		last = p
	})
	if err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if part != "data" {
		t.Fatalf("part = %q, want data", part)
	}
	if progressCount == 0 || last.Sent != int64(len(data)) || last.Total != int64(len(data)) {
		t.Fatalf("unexpected progress: count=%d last=%+v", progressCount, last)
	}
	wantHdr, _ := azupdate.EncodeWritePartHeader("data", uint64(len(data)), 1)
	if !bytes.Equal(dev.writes[0], wantHdr) {
		t.Fatal("first write was not the AZ0O header")
	}
	// Last write MUST be an explicit zero-length packet.
	if len(dev.writes[len(dev.writes)-1]) != 0 {
		t.Fatalf("last write len = %d, want 0 (ZLP)", len(dev.writes[len(dev.writes)-1]))
	}
	// Intermediate writes must be <= 256 KiB and concatenate to exact raw image.
	var streamed bytes.Buffer
	for _, w := range dev.writes[1 : len(dev.writes)-1] {
		if len(w) > azupdate.DefaultChunkSize {
			t.Fatalf("chunk size %d > 256 KiB", len(w))
		}
		streamed.Write(w)
	}
	if !bytes.Equal(streamed.Bytes(), data) {
		t.Fatal("streamed raw payload differs from input")
	}
}

func TestWriteDataFallsBackOnlyOnENOENT(t *testing.T) {
	payload := []byte("hello-ext4")
	dev := &fakeAZDevice{
		replies: [][]byte{
			buildReply(azupdate.CmdWritePart, 1, azupdate.StatusNoEntry, 24),
			buildReply(azupdate.CmdWritePart, 2, azupdate.StatusReady, 24),
			buildReply(azupdate.CmdWritePart, 2, azupdate.StatusWritten, 24),
		},
	}
	cl, _ := azupdate.New(dev, proto.Endpoints{In: 0x81, Out: 0x01})
	part, err := cl.WriteData(context.Background(), proto.Image{Data: bytes.NewReader(payload), Size: int64(len(payload))}, nil)
	if err != nil {
		t.Fatalf("WriteData fallback: %v", err)
	}
	if part != "userdata" {
		t.Fatalf("part = %q, want userdata", part)
	}
	wantHdr2, _ := azupdate.EncodeWritePartHeader("userdata", uint64(len(payload)), 2)
	if !bytes.Equal(dev.writes[1], wantHdr2) {
		t.Fatal("second header was not userdata with seq 2")
	}

	// Status 2 (non-ENOENT refusal) must NOT fall back to userdata.
	devRefuse := &fakeAZDevice{
		replies: [][]byte{
			buildReply(azupdate.CmdWritePart, 1, 2, 24),
			buildReply(azupdate.CmdWritePart, 2, azupdate.StatusReady, 24),
		},
	}
	clRefuse, _ := azupdate.New(devRefuse, proto.Endpoints{In: 0x81, Out: 0x01})
	_, err = clRefuse.WriteData(context.Background(), proto.Image{Data: bytes.NewReader(payload), Size: int64(len(payload))}, nil)
	if !errors.Is(err, azupdate.ErrRefused) || len(devRefuse.writes) != 1 {
		t.Fatalf("expected immediate refusal without userdata retry, err=%v writes=%d", err, len(devRefuse.writes))
	}

	// Both ENOENT -> ErrNoPartition.
	devNone := &fakeAZDevice{
		replies: [][]byte{
			buildReply(azupdate.CmdWritePart, 1, azupdate.StatusNoEntry, 24),
			buildReply(azupdate.CmdWritePart, 2, azupdate.StatusNoEntry, 24),
		},
	}
	clNone, _ := azupdate.New(devNone, proto.Endpoints{In: 0x81, Out: 0x01})
	if _, err := clNone.WriteData(context.Background(), proto.Image{Data: bytes.NewReader(payload), Size: int64(len(payload))}, nil); !errors.Is(err, azupdate.ErrNoPartition) {
		t.Fatalf("expected ErrNoPartition, got %v", err)
	}
}

func TestParseReplyRejectsCorruptSeqCmdLenMagic(t *testing.T) {
	// Short reply.
	if _, err := azupdate.ParseReply([]byte("AZ0I"), azupdate.CmdWritePart, 1); err == nil {
		t.Fatal("expected short reply error")
	}
	// Wrong magic.
	badMagic := buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 24)
	copy(badMagic[:4], "AZ0O")
	if _, err := azupdate.ParseReply(badMagic, azupdate.CmdWritePart, 1); err == nil {
		t.Fatal("expected wrong magic error")
	}
	// Wrong command.
	if _, err := azupdate.ParseReply(buildReply(0x5AA5FB05, 1, azupdate.StatusReady, 24), azupdate.CmdWritePart, 1); err == nil {
		t.Fatal("expected wrong command error")
	}
	// Wrong sequence.
	if _, err := azupdate.ParseReply(buildReply(azupdate.CmdWritePart, 99, azupdate.StatusReady, 24), azupdate.CmdWritePart, 1); err == nil {
		t.Fatal("expected wrong sequence error")
	}
	// Invalid packet_len (< 24 and > len(rx)).
	if _, err := azupdate.ParseReply(buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 10), azupdate.CmdWritePart, 1); err == nil {
		t.Fatal("expected small packet_len error")
	}
	if _, err := azupdate.ParseReply(buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 48), azupdate.CmdWritePart, 1); err == nil {
		t.Fatal("expected oversized packet_len error")
	}
}

func TestWriteDataFaultInjectionAndOutcomeUnknown(t *testing.T) {
	payload := []byte("0123456789")

	// Pre-ready failure (reading ready reply times out) -> NOT OutcomeUnknown.
	devPre := &fakeAZDevice{failReadAt: 1, readErr: context.DeadlineExceeded}
	clPre, _ := azupdate.New(devPre, proto.Endpoints{In: 0x81, Out: 0x01})
	if _, err := clPre.WriteData(context.Background(), proto.Image{Data: bytes.NewReader(payload), Size: int64(len(payload))}, nil); err == nil || proto.IsOutcomeUnknown(err) {
		t.Fatalf("pre-write read timeout should not be OutcomeUnknown: %v", err)
	}

	// Early EOF in image reader -> OutcomeUnknown (ST_READY already received).
	devEOF := &fakeAZDevice{
		replies: [][]byte{buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 24)},
	}
	clEOF, _ := azupdate.New(devEOF, proto.Endpoints{In: 0x81, Out: 0x01})
	if _, err := clEOF.WriteData(context.Background(), proto.Image{Data: bytes.NewReader([]byte("123")), Size: 10}, nil); !proto.IsOutcomeUnknown(err) {
		t.Fatalf("early image EOF after ST_READY must be OutcomeUnknown, got %v", err)
	}

	// Short bulk write -> OutcomeUnknown.
	devShort := &fakeAZDevice{
		replies: [][]byte{buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 24)},
		shortAt: 2,
	}
	clShort, _ := azupdate.New(devShort, proto.Endpoints{In: 0x81, Out: 0x01})
	if _, err := clShort.WriteData(context.Background(), proto.Image{Data: bytes.NewReader(payload), Size: int64(len(payload))}, nil); !proto.IsOutcomeUnknown(err) {
		t.Fatalf("short USB write must be OutcomeUnknown, got %v", err)
	}

	// ZLP write timeout -> OutcomeUnknown.
	devZLP := &fakeAZDevice{
		replies:     [][]byte{buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 24)},
		failWriteAt: 3, // 1=header, 2=payload, 3=ZLP
		writeErr:    context.DeadlineExceeded,
	}
	clZLP, _ := azupdate.New(devZLP, proto.Endpoints{In: 0x81, Out: 0x01})
	if _, err := clZLP.WriteData(context.Background(), proto.Image{Data: bytes.NewReader(payload), Size: int64(len(payload))}, nil); !proto.IsOutcomeUnknown(err) {
		t.Fatalf("ZLP timeout must be OutcomeUnknown, got %v", err)
	}

	// Final ST_WRITTEN timeout -> OutcomeUnknown.
	devFinalTimeout := &fakeAZDevice{
		replies:    [][]byte{buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 24)},
		failReadAt: 2,
		readErr:    context.DeadlineExceeded,
	}
	clFinalTimeout, _ := azupdate.New(devFinalTimeout, proto.Endpoints{In: 0x81, Out: 0x01})
	if _, err := clFinalTimeout.WriteData(context.Background(), proto.Image{Data: bytes.NewReader(payload), Size: int64(len(payload))}, nil); !proto.IsOutcomeUnknown(err) {
		t.Fatalf("final status timeout must be OutcomeUnknown, got %v", err)
	}

	// Final reply carries wrong seq -> rejected as OutcomeUnknown.
	devWrongSeq := &fakeAZDevice{
		replies: [][]byte{
			buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 24),
			buildReply(azupdate.CmdWritePart, 2, azupdate.StatusWritten, 24),
		},
	}
	clWrongSeq, _ := azupdate.New(devWrongSeq, proto.Endpoints{In: 0x81, Out: 0x01})
	if _, err := clWrongSeq.WriteData(context.Background(), proto.Image{Data: bytes.NewReader(payload), Size: int64(len(payload))}, nil); !proto.IsOutcomeUnknown(err) {
		t.Fatalf("final reply seq mismatch must be OutcomeUnknown, got %v", err)
	}

	// Final reply status != ST_WRITTEN -> ErrRefused.
	devBadStatus := &fakeAZDevice{
		replies: [][]byte{
			buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 24),
			buildReply(azupdate.CmdWritePart, 1, 7, 24),
		},
	}
	clBadStatus, _ := azupdate.New(devBadStatus, proto.Endpoints{In: 0x81, Out: 0x01})
	if _, err := clBadStatus.WriteData(context.Background(), proto.Image{Data: bytes.NewReader(payload), Size: int64(len(payload))}, nil); !errors.Is(err, azupdate.ErrRefused) {
		t.Fatalf("non-written final status must return ErrRefused, got %v", err)
	}
}
