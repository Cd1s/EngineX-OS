package azimg

import (
	"errors"
	"io"
)

// Format identifies a supported EngineX installation container format.
type Format string

const (
	// FormatAZ01 is a first-generation unsigned AZ01 wire-v1 firmware image.
	FormatAZ01 Format = "az01"
	// FormatGen2Image is a second-generation 128-byte ENGINEX_DATA_BUNDLE_IMG_V1
	// wrapper followed by a verified Data-Bundle.tar.gz payload.
	FormatGen2Image Format = "gen2-image"
	// FormatGen2Bundle is a legacy second-generation Data-Bundle.tar.gz archive.
	FormatGen2Bundle Format = "gen2-bundle"
)

const (
	// DefaultMaxAZ01Size is the maximum accepted AZ01 container size (2 GiB).
	DefaultMaxAZ01Size int64 = 2 << 30
	// DefaultMaxRootfsSize is the maximum decoded AZ01 rootfs output (2 GiB).
	DefaultMaxRootfsSize int64 = 2 << 30
	// DefaultMaxXZDictSize is the maximum allowed XZ/LZMA2 dictionary size (256 MiB).
	DefaultMaxXZDictSize uint32 = 256 << 20
	// DefaultMaxXZMemory is the maximum allowed XZ decoder memory budget (256 MiB).
	DefaultMaxXZMemory uint64 = 256 << 20
	// DefaultMaxCompressedSize is the maximum Gen-2 compressed tar.gz payload size (64 MiB).
	DefaultMaxCompressedSize int64 = 64 << 20
	// DefaultMaxExpandedSize is the maximum total Gen-2 expanded payload size across
	// both the bundle archive and any nested UI archive (256 MiB).
	DefaultMaxExpandedSize int64 = 256 << 20
	// DefaultMaxMemberSize is the maximum single archive member size (256 MiB).
	DefaultMaxMemberSize int64 = 256 << 20
	// DefaultMaxArchiveEntries is the maximum total member count across outer and nested archives.
	DefaultMaxArchiveEntries = 16384
	// DefaultMaxManifestSize is the maximum size of SHA256SUMS inside a Gen-2 bundle (1 MiB).
	DefaultMaxManifestSize int64 = 1 << 20
)

var (
	// ErrSignedContainer is returned for vendor-signed AZ0x/AZ04 images, which are
	// never accepted for general mutation or treated as authenticated.
	ErrSignedContainer = errors.New("signed vendor container is not supported")
	// ErrInvalidFormat is returned when input does not match a recognized container format.
	ErrInvalidFormat = errors.New("invalid or unrecognized image format")
	// ErrCorrupt is returned when headers, tables, padding, streams, or trailers are malformed.
	ErrCorrupt = errors.New("corrupt container or archive")
	// ErrDigestMismatch is returned when a SHA-1 or SHA-256 check fails.
	ErrDigestMismatch = errors.New("digest mismatch")
	// ErrLimitExceeded is returned when an input exceeds hostile-input size/count/memory bounds.
	ErrLimitExceeded = errors.New("size or resource limit exceeded")
	// ErrUnsafePath is returned when an archive or host path violates safety rules.
	ErrUnsafePath = errors.New("unsafe or non-canonical path")
	// ErrModelMismatch is returned when model compatibility or metadata is missing or inconsistent.
	ErrModelMismatch = errors.New("model mismatch or unsupported model")
	// ErrUnsupported is returned when an operation does not apply to the opened format.
	ErrUnsupported = errors.New("unsupported operation for image format")
)

// Limits bounds container parsing, decompression, and archive validation against hostile inputs.
// Zero-valued fields are populated from DefaultLimits.
type Limits struct {
	MaxAZ01Size       int64
	MaxRootfsSize     int64
	MaxXZDictSize     uint32
	MaxXZMemory       uint64
	MaxCompressedSize int64
	MaxExpandedSize   int64
	MaxMemberSize     int64
	MaxArchiveEntries int
	MaxManifestSize   int64
}

// DefaultLimits returns the production safety limits matching az01_packer.py and #255.
func DefaultLimits() Limits {
	return Limits{
		MaxAZ01Size:       DefaultMaxAZ01Size,
		MaxRootfsSize:     DefaultMaxRootfsSize,
		MaxXZDictSize:     DefaultMaxXZDictSize,
		MaxXZMemory:       DefaultMaxXZMemory,
		MaxCompressedSize: DefaultMaxCompressedSize,
		MaxExpandedSize:   DefaultMaxExpandedSize,
		MaxMemberSize:     DefaultMaxMemberSize,
		MaxArchiveEntries: DefaultMaxArchiveEntries,
		MaxManifestSize:   DefaultMaxManifestSize,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxAZ01Size <= 0 {
		l.MaxAZ01Size = d.MaxAZ01Size
	}
	if l.MaxRootfsSize <= 0 {
		l.MaxRootfsSize = d.MaxRootfsSize
	}
	if l.MaxXZDictSize == 0 {
		l.MaxXZDictSize = d.MaxXZDictSize
	}
	if l.MaxXZMemory == 0 {
		l.MaxXZMemory = d.MaxXZMemory
	}
	if l.MaxCompressedSize <= 0 {
		l.MaxCompressedSize = d.MaxCompressedSize
	}
	if l.MaxExpandedSize <= 0 {
		l.MaxExpandedSize = d.MaxExpandedSize
	}
	if l.MaxMemberSize <= 0 {
		l.MaxMemberSize = d.MaxMemberSize
	}
	if l.MaxArchiveEntries <= 0 {
		l.MaxArchiveEntries = d.MaxArchiveEntries
	}
	if l.MaxManifestSize <= 0 {
		l.MaxManifestSize = d.MaxManifestSize
	}
	return l
}

// Model identifies an EngineX-supported hardware model.
type Model struct {
	Compatible string // Device-tree compatible string (e.g. "inmusic,jp07"), empty for Gen-2 bundles.
	Board      string // Lowercase board code (e.g. "jp07", "jp21", "rmz2").
	Code       string // Uppercase board code (e.g. "JP07", "JP21", "RMZ2").
	Prefix     string // Release asset prefix (e.g. "SC5000", "SCLIVE4", "SYSTEMONE").
	Name       string // Short display name (matches az01_packer.py for Gen-1, e.g. "SC5000").
	FullName   string // Full product name (e.g. "Denon DJ SC5000").
	Gen2       bool
	AZ04       bool
}

// Partition describes one verified AZ01 partition record.
type Partition struct {
	Name         string
	Compression  string
	EntryOffset  int64
	Offset       int64
	Size         int64
	DigestOffset int64
	SHA1         [20]byte
}

// AZ01Metadata holds verified header and partition metadata from an AZ01 wire-v1 container.
type AZ01Metadata struct {
	WireVersion uint32
	Build       string
	Compatible  []string
	USBDevices  []uint32
	Description string
	TableOffset int64
	EOFOffset   int64
	Aligned     bool
	Partitions  []Partition
}

// Entry represents a verified, canonically-named regular file entry inside a Gen-2 bundle
// or its nested UI archive.
type Entry struct {
	Path   string // Canonical image/archive-relative POSIX path (no leading slash).
	Mode   uint32 // Unix permission bits (e.g. 0755, 0644).
	Size   int64
	SHA256 [32]byte
	Open   func() (io.ReadCloser, error)
}
