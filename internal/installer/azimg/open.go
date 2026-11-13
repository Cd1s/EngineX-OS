package azimg

import (
	"context"
	"fmt"
	"io"
)

// Image represents an opened and structurally verified EngineX firmware image or bundle.
type Image struct {
	r      io.ReaderAt
	size   int64
	limits Limits
	format Format
	models []Model

	az01 *AZ01Metadata

	// Gen-2 wrapper/bundle metadata
	payloadOffset int64
	payloadSize   int64
	headerPrefix  string
	headerVersion string
	headerSHA256  [32]byte
	hasHeaderSHA  bool
	bundle        *Bundle
}

// Open sniffs, bounds-checks, and verifies a firmware container (AZ01 wire-v1,
// Gen-2 ENGINEX_DATA_BUNDLE_IMG_V1 wrapper, or legacy Gen-2 Data-Bundle.tar.gz).
// Signed vendor containers (AZ0x/AZ04) and FIT images are explicitly rejected.
func Open(ctx context.Context, r io.ReaderAt, size int64, limits Limits) (*Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("%w: nil ReaderAt", ErrInvalidFormat)
	}
	if size < 0 {
		return nil, fmt.Errorf("%w: negative size %d", ErrInvalidFormat, size)
	}
	limits = limits.withDefaults()

	sniffLen := min(size, 128)
	sniff := make([]byte, sniffLen)
	if sniffLen > 0 {
		if _, err := io.ReadFull(io.NewSectionReader(r, 0, sniffLen), sniff); err != nil {
			return nil, fmt.Errorf("%w: reading container header: %v", ErrCorrupt, err)
		}
	}

	if len(sniff) >= 4 {
		switch string(sniff[:4]) {
		case "AZ01":
			return openAZ01(ctx, r, size, limits)
		case "AZ0x":
			return nil, fmt.Errorf("%w: AZ0x/AZ04 signed vendor container cannot be installed or mutated", ErrSignedContainer)
		case "\xd0\x0d\xfe\xed":
			return nil, fmt.Errorf("%w: FDT/FIT image is not an installable container", ErrInvalidFormat)
		}
	}
	if len(sniff) >= len(gen2ImgPrefix) && string(sniff[:len(gen2ImgPrefix)]) == gen2ImgPrefix {
		return openGen2Image(ctx, r, size, limits, sniff)
	}
	if len(sniff) >= 2 && sniff[0] == 0x1f && sniff[1] == 0x8b {
		return openGen2Bundle(ctx, r, size, limits)
	}
	return nil, fmt.Errorf("%w: unrecognized image or bundle container", ErrInvalidFormat)
}

// Format returns the detected container format.
func (im *Image) Format() Format { return im.format }

// Size returns the total input container size in bytes.
func (im *Image) Size() int64 { return im.size }

// Models returns the supported hardware models declared by the container.
func (im *Image) Models() []Model {
	out := make([]Model, len(im.models))
	copy(out, im.models)
	return out
}

// SupportsBoard reports whether the container's verified model metadata includes boardOrPrefix
// (e.g. "jp07", "JP07", "SC5000", "inmusic,jp07", "SCLIVE4", "jp21").
func (im *Image) SupportsBoard(boardOrPrefix string) bool {
	want, ok := LookupModel(boardOrPrefix)
	if !ok {
		return false
	}
	for _, m := range im.models {
		if m.Board == want.Board || m.Prefix == want.Prefix {
			return true
		}
	}
	return false
}

// AZ01 returns the verified AZ01 metadata when Format() == FormatAZ01.
func (im *Image) AZ01() (*AZ01Metadata, bool) {
	if im.format != FormatAZ01 || im.az01 == nil {
		return nil, false
	}
	cp := *im.az01
	cp.Compatible = append([]string(nil), im.az01.Compatible...)
	cp.USBDevices = append([]uint32(nil), im.az01.USBDevices...)
	cp.Partitions = append([]Partition(nil), im.az01.Partitions...)
	return &cp, true
}
