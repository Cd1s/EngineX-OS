package fastboot_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/fastboot"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/sparse"
)

type fbStep int

const (
	stepIdle fbStep = iota
	stepData
)

type faultRule struct {
	onWriteCall int
	onReadCall  int
	writeErr    error
	shortWrite  bool
	readErr     error
	override    [][]byte
}

// fakeFastbootDevice emulates a stateful U-Boot fastboot USB bulk target,
// including INFO interleaving and multi-piece Android sparse reconstruction.
type fakeFastbootDevice struct {
	vars         map[string]string
	infoBeforeOK map[string][]string
	failOnCmd    map[string]string
	step         fbStep
	expectBytes  int
	staged       bytes.Buffer
	partitions   map[string][]byte
	commands     []string
	writeCalls   int
	readCalls    int
	replies      [][]byte
	fault        faultRule
}

func newFakeFastbootDevice() *fakeFastbootDevice {
	return &fakeFastbootDevice{
		vars:         map[string]string{"product": "jc11s", "max-download-size": "0x20000000"},
		infoBeforeOK: map[string][]string{},
		failOnCmd:    map[string]string{},
		partitions:   map[string][]byte{},
	}
}

func (f *fakeFastbootDevice) WriteBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	if ep != 0x01 {
		return 0, fmt.Errorf("unexpected OUT endpoint 0x%02x", ep)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.writeCalls++
	if f.fault.onWriteCall == f.writeCalls {
		if f.fault.writeErr != nil {
			return 0, f.fault.writeErr
		}
		if f.fault.shortWrite && len(data) > 0 {
			return len(data) - 1, nil
		}
	}

	if f.step == stepData {
		f.staged.Write(data)
		if f.staged.Len() == f.expectBytes {
			f.step = stepIdle
			f.queueInfo("download")
			f.replies = append(f.replies, []byte("OKAY"))
		} else if f.staged.Len() > f.expectBytes {
			f.step = stepIdle
			f.replies = append(f.replies, []byte("FAILtoo many bytes"))
		}
		return len(data), nil
	}

	cmd := string(data)
	f.commands = append(f.commands, cmd)
	if msg, ok := f.failOnCmd[cmd]; ok {
		f.replies = append(f.replies, []byte("FAIL"+msg))
		return len(data), nil
	}
	switch {
	case strings.HasPrefix(cmd, "getvar:"):
		key := strings.TrimPrefix(cmd, "getvar:")
		f.queueInfo(cmd)
		val, ok := f.vars[key]
		if !ok {
			f.replies = append(f.replies, []byte("FAILVariable not implemented"))
		} else {
			f.replies = append(f.replies, []byte("OKAY"+val))
		}
	case cmd == fastboot.Gen1OEMUnlockCommand:
		f.queueInfo(cmd)
		f.replies = append(f.replies, []byte("OKAY"))
	case strings.HasPrefix(cmd, "download:"):
		hexLen := strings.TrimPrefix(cmd, "download:")
		var sz int
		_, _ = fmt.Sscanf(hexLen, "%x", &sz)
		f.expectBytes = sz
		f.staged.Reset()
		f.step = stepData
		f.queueInfo("download")
		f.replies = append(f.replies, []byte(fmt.Sprintf("DATA%08x", sz)))
	case strings.HasPrefix(cmd, "flash:"):
		part := strings.TrimPrefix(cmd, "flash:")
		f.queueInfo(cmd)
		if err := f.applyStaged(part); err != nil {
			f.replies = append(f.replies, []byte("FAIL"+err.Error()))
		} else {
			f.replies = append(f.replies, []byte("OKAY"))
		}
	case cmd == "reboot":
		f.queueInfo(cmd)
		f.replies = append(f.replies, []byte("OKAY"))
	default:
		f.replies = append(f.replies, []byte("FAILunknown command"))
	}
	return len(data), nil
}

func (f *fakeFastbootDevice) queueInfo(key string) {
	for _, msg := range f.infoBeforeOK[key] {
		f.replies = append(f.replies, []byte("INFO"+msg))
	}
}

func (f *fakeFastbootDevice) applyStaged(part string) error {
	payload := append([]byte(nil), f.staged.Bytes()...)
	isSp, err := sparse.IsSparse(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return err
	}
	if !isSp {
		f.partitions[part] = payload
		return nil
	}
	h, err := sparse.ParseHeader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return err
	}
	needLen := int(h.LogicalSize())
	cur := f.partitions[part]
	if len(cur) < needLen {
		grown := make([]byte, needLen)
		// Pre-fill new area with 0xAA to verify DONT_CARE does not zero skipped ranges.
		for i := len(cur); i < needLen; i++ {
			grown[i] = 0xAA
		}
		copy(grown, cur)
		cur = grown
	}
	if _, err := sparse.Decode(&sliceWriterAt{buf: cur}, bytes.NewReader(payload), int64(len(payload)), sparse.DecodeOptions{ZeroDontCare: false}); err != nil {
		return err
	}
	f.partitions[part] = cur
	return nil
}

func (f *fakeFastbootDevice) ReadBulk(ctx context.Context, ep uint8, buf []byte) (int, error) {
	if ep != 0x81 {
		return 0, fmt.Errorf("unexpected IN endpoint 0x%02x", ep)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.readCalls++
	if f.fault.onReadCall == f.readCalls && f.fault.readErr != nil {
		return 0, f.fault.readErr
	}
	if len(f.fault.override) > 0 {
		pkt := f.fault.override[0]
		f.fault.override = f.fault.override[1:]
		return copy(buf, pkt), nil
	}
	if len(f.replies) == 0 {
		return 0, io.EOF
	}
	pkt := f.replies[0]
	f.replies = f.replies[1:]
	return copy(buf, pkt), nil
}

type sliceWriterAt struct{ buf []byte }

func (s *sliceWriterAt) WriteAt(p []byte, off int64) (int, error) {
	copy(s.buf[off:], p)
	return len(p), nil
}

func TestFastbootInfoInterleavingAndGen1Flow(t *testing.T) {
	dev := newFakeFastbootDevice()
	delete(dev.vars, "product") // Gen-1 SC5000 returns Variable not implemented
	dev.vars["max-download-size"] = "0x100000"
	dev.infoBeforeOK[fastboot.Gen1OEMUnlockCommand] = []string{"unlocking", "done"}
	dev.infoBeforeOK["flash:rootfs"] = []string{"erasing rootfs", "writing rootfs"}

	var infos []string
	cl, err := fastboot.New(dev, proto.Endpoints{In: 0x81, Out: 0x01},
		fastboot.WithOnInfo(func(msg string) { infos = append(infos, msg) }),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Wrong USB fallback refuses.
	if _, err := cl.VerifyProduct(context.Background(), "jp07", "jc11"); !errors.Is(err, fastboot.ErrProductMismatch) {
		t.Fatalf("expected ErrProductMismatch, got %v", err)
	}
	// Matching Gen-1 USB fallback succeeds.
	if code, err := cl.VerifyProduct(context.Background(), "jp07", "jp07"); err != nil || code != "jp07" {
		t.Fatalf("VerifyProduct Gen1 = (%q, %v)", code, err)
	}
	if err := cl.UnlockGen1(context.Background()); err != nil {
		t.Fatalf("UnlockGen1: %v", err)
	}

	rootfs := bytes.Repeat([]byte("ROOTFS-DATA!"), 1024)
	if err := cl.Flash(context.Background(), "rootfs", proto.Image{
		Data: bytes.NewReader(rootfs),
		Size: int64(len(rootfs)),
	}, fastboot.FlashOptions{
		ExpectedBoard:            "jp07",
		AllowMissingProductBoard: "jp07",
	}, nil); err != nil {
		t.Fatalf("Flash rootfs: %v", err)
	}
	if err := cl.Reboot(context.Background()); err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if !bytes.Equal(dev.partitions["rootfs"], rootfs) {
		t.Fatal("flashed rootfs differs from input")
	}
	if len(infos) < 4 {
		t.Fatalf("expected interleaved INFO messages, got %v", infos)
	}
}

func TestFastbootRejectsInvalidHexAndBadVars(t *testing.T) {
	cases := []struct {
		name  string
		reply []byte
		call  func(*fastboot.Client) error
	}{
		{
			name:  "invalid hex in DATA",
			reply: []byte("DATA1234ZZZZ"),
			call: func(c *fastboot.Client) error {
				return c.Download(context.Background(), bytes.NewReader([]byte("abcd")), 4, nil)
			},
		},
		{
			name:  "short hex in DATA",
			reply: []byte("DATA0004"),
			call: func(c *fastboot.Client) error {
				return c.Download(context.Background(), bytes.NewReader([]byte("abcd")), 4, nil)
			},
		},
		{
			name:  "mismatched DATA length",
			reply: []byte("DATA00000010"),
			call: func(c *fastboot.Client) error {
				return c.Download(context.Background(), bytes.NewReader([]byte("abcd")), 4, nil)
			},
		},
		{
			name:  "non-hex max-download-size",
			reply: []byte("OKAY200MB"),
			call: func(c *fastboot.Client) error {
				_, err := c.GetMaxDownloadSize(context.Background())
				return err
			},
		},
		{
			name:  "zero max-download-size",
			reply: []byte("OKAY0x0"),
			call: func(c *fastboot.Client) error {
				_, err := c.GetMaxDownloadSize(context.Background())
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev := newFakeFastbootDevice()
			dev.fault.override = [][]byte{tc.reply}
			cl, _ := fastboot.New(dev, proto.Endpoints{In: 0x81, Out: 0x01})
			err := tc.call(cl)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			// Must only have sent the initial command, never streamed payload bytes.
			if dev.writeCalls != 1 {
				t.Fatalf("writeCalls = %d, want 1 (no payload after bad DATA)", dev.writeCalls)
			}
		})
	}
}

func TestFastbootWrongProductAndMissingMaxDownloadRefuse(t *testing.T) {
	dev := newFakeFastbootDevice()
	dev.vars["product"] = "rk3288-az05-jp20"
	cl, _ := fastboot.New(dev, proto.Endpoints{In: 0x81, Out: 0x01})

	img := proto.Image{Data: bytes.NewReader(make([]byte, 4096)), Size: 4096}
	err := cl.Flash(context.Background(), "data", img, fastboot.FlashOptions{
		ExpectedBoard: "jc11s",
	}, nil)
	if !errors.Is(err, fastboot.ErrProductMismatch) {
		t.Fatalf("expected ErrProductMismatch, got %v", err)
	}
	if len(dev.partitions) != 0 {
		t.Fatal("must not flash on wrong product")
	}

	// Missing max-download-size must never be guessed.
	dev2 := newFakeFastbootDevice()
	delete(dev2.vars, "max-download-size")
	cl2, _ := fastboot.New(dev2, proto.Endpoints{In: 0x81, Out: 0x01})
	err = cl2.Flash(context.Background(), "data", img, fastboot.FlashOptions{ExpectedBoard: "jc11s"}, nil)
	if !errors.Is(err, fastboot.ErrUnsupportedVar) {
		t.Fatalf("expected ErrUnsupportedVar when max-download-size missing, got %v", err)
	}
}

func TestFastbootMultiPieceSparseFlashExceedingDownloadCap(t *testing.T) {
	const blkSize = 4096
	const rawBlocks = 16 // 64 KiB
	raw := make([]byte, rawBlocks*blkSize)
	for b := 0; b < rawBlocks; b++ {
		if b%3 == 0 {
			// Leave zeroed blocks to test FILL(0) overwrites pre-existing flash.
			continue
		}
		for i := 0; i < blkSize; i++ {
			raw[b*blkSize+i] = byte((b*17 + i) ^ 0x3C)
		}
	}

	// Force a tiny download cap (28 + 12 + 12 + 3*4096 = 12340 bytes) so 16 blocks require multiple pieces.
	const capBytes = 28 + 12 + 12 + 3*blkSize
	dev := newFakeFastbootDevice()
	dev.vars["max-download-size"] = fmt.Sprintf("0x%x", capBytes)
	dev.partitions["data"] = bytes.Repeat([]byte{0xFF}, len(raw))

	cl, _ := fastboot.New(dev, proto.Endpoints{In: 0x81, Out: 0x01}, fastboot.WithChunkSize(2048))

	// First check AllowSparse=false refuses without truncating.
	err := cl.Flash(context.Background(), "data", proto.Image{
		Data: bytes.NewReader(raw),
		Size: int64(len(raw)),
	}, fastboot.FlashOptions{
		ExpectedBoard: "jc11s",
		AllowSparse:   false,
	}, nil)
	if !errors.Is(err, fastboot.ErrSparseRequired) {
		t.Fatalf("expected ErrSparseRequired, got %v", err)
	}

	// Now allow sparse splitting and record progress.
	var lastSent, lastTotal int64
	err = cl.Flash(context.Background(), "data", proto.Image{
		Data: bytes.NewReader(raw),
		Size: int64(len(raw)),
	}, fastboot.FlashOptions{
		ExpectedBoard: "jc11s",
		AllowSparse:   true,
	}, func(p proto.Progress) {
		lastSent, lastTotal = p.Sent, p.Total
	})
	if err != nil {
		t.Fatalf("Flash multi-piece sparse: %v", err)
	}
	if lastSent != lastTotal || lastTotal == 0 {
		t.Fatalf("unexpected progress: sent=%d total=%d", lastSent, lastTotal)
	}
	if sha256.Sum256(dev.partitions["data"]) != sha256.Sum256(raw) {
		t.Fatal("reconstructed device partition differs from raw image")
	}
	flashCmds := 0
	for _, c := range dev.commands {
		if c == "flash:data" {
			flashCmds++
		}
	}
	if flashCmds < 3 {
		t.Fatalf("expected >= 3 sparse flash commands, got %d (%v)", flashCmds, dev.commands)
	}
}

func TestFastbootFaultInjectionAndUnknownOutcome(t *testing.T) {
	payload := bytes.Repeat([]byte("ABCD"), 2048)

	// Case 1: timeout reading DATA (before any payload byte sent) -> NOT OutcomeUnknown.
	dev1 := newFakeFastbootDevice()
	dev1.fault = faultRule{onReadCall: 1, readErr: context.DeadlineExceeded}
	cl1, _ := fastboot.New(dev1, proto.Endpoints{In: 0x81, Out: 0x01})
	err1 := cl1.Download(context.Background(), bytes.NewReader(payload), int64(len(payload)), nil)
	if err1 == nil || proto.IsOutcomeUnknown(err1) {
		t.Fatalf("pre-data timeout should not mark outcome unknown: %v", err1)
	}

	// Case 2: short WriteBulk mid-download -> OutcomeUnknown.
	dev2 := newFakeFastbootDevice()
	dev2.fault = faultRule{onWriteCall: 2, shortWrite: true}
	cl2, _ := fastboot.New(dev2, proto.Endpoints{In: 0x81, Out: 0x01}, fastboot.WithChunkSize(4096))
	err2 := cl2.Download(context.Background(), bytes.NewReader(payload), int64(len(payload)), nil)
	if !proto.IsOutcomeUnknown(err2) {
		t.Fatalf("mid-download short write must be OutcomeUnknown, got %v", err2)
	}

	// Case 3: timeout waiting for final OKAY after download -> OutcomeUnknown.
	dev3 := newFakeFastbootDevice()
	dev3.fault = faultRule{onReadCall: 2, readErr: context.DeadlineExceeded}
	cl3, _ := fastboot.New(dev3, proto.Endpoints{In: 0x81, Out: 0x01})
	err3 := cl3.Download(context.Background(), bytes.NewReader(payload), int64(len(payload)), nil)
	if !proto.IsOutcomeUnknown(err3) {
		t.Fatalf("final ack timeout must be OutcomeUnknown, got %v", err3)
	}

	// Case 4: timeout waiting for flash OKAY -> OutcomeUnknown.
	dev4 := newFakeFastbootDevice()
	dev4.fault = faultRule{onReadCall: 1, readErr: context.DeadlineExceeded}
	cl4, _ := fastboot.New(dev4, proto.Endpoints{In: 0x81, Out: 0x01})
	err4 := cl4.FlashPartition(context.Background(), "data")
	if !proto.IsOutcomeUnknown(err4) {
		t.Fatalf("flash ack timeout must be OutcomeUnknown, got %v", err4)
	}

	// Case 5: INFO flood exceeding MaxInfoPackets stops cleanly.
	dev5 := newFakeFastbootDevice()
	dev5.infoBeforeOK["getvar:product"] = []string{"1", "2", "3", "4"}
	cl5, _ := fastboot.New(dev5, proto.Endpoints{In: 0x81, Out: 0x01}, fastboot.WithMaxInfoPackets(2))
	if _, err := cl5.GetVar(context.Background(), "product"); err == nil {
		t.Fatal("expected INFO limit error")
	}

	// Case 6: multi-piece sparse failure on piece 2 -> OutcomeUnknown (piece 1 already flashed).
	const blkSize = 4096
	raw := make([]byte, 8*blkSize)
	for i := range raw {
		raw[i] = byte((i*7 + i/blkSize) ^ 0x5A)
	}
	capBytes := int64(28 + 12 + 12 + 4*blkSize)
	dev6 := newFakeFastbootDevice()
	dev6.vars["max-download-size"] = fmt.Sprintf("0x%x", capBytes)
	// Inject a read failure on piece 2's download command (read #6).
	// Piece 0: getvar product (1), getvar max-download-size (2), dl1 DATA (3), dl1 OKAY (4), flash1 OKAY (5).
	// Piece 1: dl2 DATA (6) fails.
	dev6.fault = faultRule{onReadCall: 6, readErr: io.EOF}
	cl6, _ := fastboot.New(dev6, proto.Endpoints{In: 0x81, Out: 0x01}, fastboot.WithCommandTimeout(time.Second))
	err6 := cl6.Flash(context.Background(), "data", proto.Image{Data: bytes.NewReader(raw), Size: int64(len(raw))}, fastboot.FlashOptions{
		ExpectedBoard: "jc11s",
		AllowSparse:   true,
	}, nil)
	if !proto.IsOutcomeUnknown(err6) {
		t.Fatalf("piece 2 failure after piece 1 flashed must be OutcomeUnknown, got %v", err6)
	}

	// Case 7: write error while sending mutating flash:<part> command -> OutcomeUnknown.
	dev7 := newFakeFastbootDevice()
	dev7.fault = faultRule{onWriteCall: 1, writeErr: context.DeadlineExceeded}
	cl7, _ := fastboot.New(dev7, proto.Endpoints{In: 0x81, Out: 0x01})
	if err := cl7.FlashPartition(context.Background(), "data"); !proto.IsOutcomeUnknown(err) {
		t.Fatalf("mutating flash command write failure must be OutcomeUnknown, got %v", err)
	}

	// Case 8: premature reader failure at sent==0 during DATA phase taints session.
	dev8 := newFakeFastbootDevice()
	cl8, _ := fastboot.New(dev8, proto.Endpoints{In: 0x81, Out: 0x01})
	if err := cl8.Download(context.Background(), bytes.NewReader(nil), 16, nil); err == nil {
		t.Fatal("expected reader EOF error")
	}
	if _, err := cl8.GetVar(context.Background(), "product"); !errors.Is(err, fastboot.ErrSessionDirty) {
		t.Fatalf("expected ErrSessionDirty after aborted DATA phase, got %v", err)
	}

	// Case 9: Gen-2 or unknown board cannot bypass VerifyProduct via allowMissingUSBBoard.
	dev9 := newFakeFastbootDevice()
	delete(dev9.vars, "product")
	cl9, _ := fastboot.New(dev9, proto.Endpoints{In: 0x81, Out: 0x01})
	if _, err := cl9.VerifyProduct(context.Background(), "jc11s", "jc11s"); !errors.Is(err, fastboot.ErrProductMismatch) {
		t.Fatalf("Gen-2 missing product fallback must be rejected, got %v", err)
	}
	if _, err := cl9.VerifyProduct(context.Background(), "notaboard", "notaboard"); !errors.Is(err, fastboot.ErrProductMismatch) {
		t.Fatalf("unknown board must be rejected, got %v", err)
	}
}

func TestParseSizeValue(t *testing.T) {
	good := map[string]int64{
		"0x20000000": 0x20000000,
		"0X1000":     0x1000,
		"536870912":  536870912,
		" 0x400 ":    1024,
	}
	for in, want := range good {
		got, err := fastboot.ParseSizeValue(in)
		if err != nil || got != want {
			t.Errorf("ParseSizeValue(%q) = (%d, %v), want (%d, nil)", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0x", "0xZZ", "-1", "20000000a", "abc"} {
		if _, err := fastboot.ParseSizeValue(bad); err == nil {
			t.Errorf("ParseSizeValue(%q) unexpectedly succeeded", bad)
		}
	}
	_ = hex.EncodeToString
}
