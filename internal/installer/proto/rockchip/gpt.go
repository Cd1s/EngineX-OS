package rockchip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"strings"
	"unicode/utf16"
)

const (
	SectorSize   = 512
	GPTSignature = "EFI PART"
	GPTRevision1 = 0x00010000

	minGPTHeaderSize = 92
	minGPTEntrySize  = 128
	maxGPTEntrySize  = 4096
	maxGPTEntries    = 4096
)

var (
	ErrInvalidGPT       = errors.New("rockchip: invalid GPT")
	ErrGPTChecksum      = errors.New("rockchip: GPT CRC32 mismatch")
	ErrPartitionOverlap = errors.New("rockchip: overlapping GPT partitions")
)

// GPTHeader holds the parsed primary GPT header from LBA 1.
type GPTHeader struct {
	Signature                string
	Revision                 uint32
	HeaderSize               uint32
	HeaderCRC32              uint32
	MyLBA                    uint64
	AlternateLBA             uint64
	FirstUsableLBA           uint64
	LastUsableLBA            uint64
	DiskGUID                 [16]byte
	PartitionEntryLBA        uint64
	NumberOfPartitionEntries uint32
	SizeOfPartitionEntry     uint32
	PartitionEntryArrayCRC32 uint32
}

// EntryArrayByteSize returns the total byte size of the GPT partition entry array.
func (h GPTHeader) EntryArrayByteSize() uint64 {
	return uint64(h.NumberOfPartitionEntries) * uint64(h.SizeOfPartitionEntry)
}

// EntryArraySectors returns the number of 512-byte sectors occupied by the entry array.
func (h GPTHeader) EntryArraySectors() uint64 {
	bytes := h.EntryArrayByteSize()
	return (bytes + SectorSize - 1) / SectorSize
}

// Partition describes one non-empty GPT partition entry.
type Partition struct {
	Index      uint32
	Name       string
	TypeGUID   [16]byte
	UniqueGUID [16]byte
	StartLBA   uint64
	EndLBA     uint64 // inclusive
	Attributes uint64
}

// SectorCount returns the number of 512-byte sectors in the partition.
func (p Partition) SectorCount() uint64 {
	if p.EndLBA < p.StartLBA {
		return 0
	}
	return p.EndLBA - p.StartLBA + 1
}

// ByteSize returns the partition size in bytes (clamped against uint64 overflow).
func (p Partition) ByteSize() uint64 {
	cnt := p.SectorCount()
	if cnt > ^uint64(0)/SectorSize {
		return ^uint64(0)
	}
	return cnt * SectorSize
}

// ParseGPTHeader parses and CRC-verifies a primary GPT header from a 512-byte
// LBA 1 sector. If totalSectors > 0, usable and backup LBA bounds are checked
// against totalSectors.
func ParseGPTHeader(sector []byte, totalSectors uint64) (GPTHeader, error) {
	if len(sector) < SectorSize {
		return GPTHeader{}, fmt.Errorf("%w: header sector too short (%d bytes)", ErrInvalidGPT, len(sector))
	}
	sig := string(sector[0:8])
	if sig != GPTSignature {
		return GPTHeader{}, fmt.Errorf("%w: bad signature %q", ErrInvalidGPT, sig)
	}
	h := GPTHeader{
		Signature:                sig,
		Revision:                 binary.LittleEndian.Uint32(sector[8:12]),
		HeaderSize:               binary.LittleEndian.Uint32(sector[12:16]),
		HeaderCRC32:              binary.LittleEndian.Uint32(sector[16:20]),
		MyLBA:                    binary.LittleEndian.Uint64(sector[24:32]),
		AlternateLBA:             binary.LittleEndian.Uint64(sector[32:40]),
		FirstUsableLBA:           binary.LittleEndian.Uint64(sector[40:48]),
		LastUsableLBA:            binary.LittleEndian.Uint64(sector[48:56]),
		PartitionEntryLBA:        binary.LittleEndian.Uint64(sector[72:80]),
		NumberOfPartitionEntries: binary.LittleEndian.Uint32(sector[80:84]),
		SizeOfPartitionEntry:     binary.LittleEndian.Uint32(sector[84:88]),
		PartitionEntryArrayCRC32: binary.LittleEndian.Uint32(sector[88:92]),
	}
	copy(h.DiskGUID[:], sector[56:72])

	if h.Revision != GPTRevision1 {
		return GPTHeader{}, fmt.Errorf("%w: unsupported revision 0x%08x", ErrInvalidGPT, h.Revision)
	}
	if h.HeaderSize < minGPTHeaderSize || h.HeaderSize > SectorSize {
		return GPTHeader{}, fmt.Errorf("%w: invalid header size %d", ErrInvalidGPT, h.HeaderSize)
	}
	var hdrCopy [SectorSize]byte
	copy(hdrCopy[:], sector[:h.HeaderSize])
	clear(hdrCopy[16:20])
	if gotCRC := crc32.ChecksumIEEE(hdrCopy[:h.HeaderSize]); gotCRC != h.HeaderCRC32 {
		return GPTHeader{}, fmt.Errorf("%w: header CRC 0x%08x != computed 0x%08x", ErrGPTChecksum, h.HeaderCRC32, gotCRC)
	}
	if h.MyLBA != 1 {
		return GPTHeader{}, fmt.Errorf("%w: primary MyLBA=%d, want 1", ErrInvalidGPT, h.MyLBA)
	}
	if h.SizeOfPartitionEntry < minGPTEntrySize || h.SizeOfPartitionEntry > maxGPTEntrySize || h.SizeOfPartitionEntry%8 != 0 {
		return GPTHeader{}, fmt.Errorf("%w: invalid entry size %d", ErrInvalidGPT, h.SizeOfPartitionEntry)
	}
	if h.NumberOfPartitionEntries == 0 || h.NumberOfPartitionEntries > maxGPTEntries {
		return GPTHeader{}, fmt.Errorf("%w: invalid entry count %d", ErrInvalidGPT, h.NumberOfPartitionEntries)
	}
	if h.PartitionEntryLBA < 2 {
		return GPTHeader{}, fmt.Errorf("%w: PartitionEntryLBA %d < 2", ErrInvalidGPT, h.PartitionEntryLBA)
	}
	entrySectors := h.EntryArraySectors()
	if h.PartitionEntryLBA+entrySectors > h.FirstUsableLBA {
		return GPTHeader{}, fmt.Errorf("%w: entry array [%d..%d) overlaps FirstUsableLBA %d",
			ErrInvalidGPT, h.PartitionEntryLBA, h.PartitionEntryLBA+entrySectors, h.FirstUsableLBA)
	}
	if h.FirstUsableLBA > h.LastUsableLBA {
		return GPTHeader{}, fmt.Errorf("%w: FirstUsableLBA %d > LastUsableLBA %d", ErrInvalidGPT, h.FirstUsableLBA, h.LastUsableLBA)
	}
	if totalSectors > 0 {
		if h.LastUsableLBA >= totalSectors {
			return GPTHeader{}, fmt.Errorf("%w: LastUsableLBA %d >= disk sectors %d", ErrInvalidGPT, h.LastUsableLBA, totalSectors)
		}
		if h.AlternateLBA >= totalSectors {
			return GPTHeader{}, fmt.Errorf("%w: AlternateLBA %d >= disk sectors %d", ErrInvalidGPT, h.AlternateLBA, totalSectors)
		}
	}
	return h, nil
}

// ParseGPTEntries verifies the partition entry array CRC32 and extracts all
// active partitions, checking usable LBA bounds and overlap.
func ParseGPTEntries(hdr GPTHeader, data []byte) ([]Partition, error) {
	need := hdr.EntryArrayByteSize()
	if uint64(len(data)) < need {
		return nil, fmt.Errorf("%w: entry array truncated (%d < %d bytes)", ErrInvalidGPT, len(data), need)
	}
	arrayBytes := data[:need]
	if gotCRC := crc32.ChecksumIEEE(arrayBytes); gotCRC != hdr.PartitionEntryArrayCRC32 {
		return nil, fmt.Errorf("%w: partition entry array CRC 0x%08x != computed 0x%08x",
			ErrGPTChecksum, hdr.PartitionEntryArrayCRC32, gotCRC)
	}

	var parts []Partition
	stride := int(hdr.SizeOfPartitionEntry)
	for i := uint32(0); i < hdr.NumberOfPartitionEntries; i++ {
		ent := arrayBytes[int(i)*stride : (int(i)+1)*stride]
		if isAllZero(ent[0:16]) {
			continue
		}
		startLBA := binary.LittleEndian.Uint64(ent[32:40])
		endLBA := binary.LittleEndian.Uint64(ent[40:48])
		if startLBA < hdr.FirstUsableLBA || endLBA > hdr.LastUsableLBA || startLBA > endLBA {
			return nil, fmt.Errorf("%w: partition %d LBA range [%d..%d] outside usable [%d..%d]",
				ErrInvalidGPT, i, startLBA, endLBA, hdr.FirstUsableLBA, hdr.LastUsableLBA)
		}
		p := Partition{
			Index:      i,
			Name:       decodeUTF16LEName(ent[56:128]),
			StartLBA:   startLBA,
			EndLBA:     endLBA,
			Attributes: binary.LittleEndian.Uint64(ent[48:56]),
		}
		copy(p.TypeGUID[:], ent[0:16])
		copy(p.UniqueGUID[:], ent[16:32])
		parts = append(parts, p)
	}

	sorted := append([]Partition(nil), parts...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].StartLBA != sorted[j].StartLBA {
			return sorted[i].StartLBA < sorted[j].StartLBA
		}
		return sorted[i].EndLBA < sorted[j].EndLBA
	})
	for i := 1; i < len(sorted); i++ {
		if sorted[i].StartLBA <= sorted[i-1].EndLBA {
			return nil, fmt.Errorf("%w: %q [%d..%d] overlaps %q [%d..%d]",
				ErrPartitionOverlap, sorted[i-1].Name, sorted[i-1].StartLBA, sorted[i-1].EndLBA,
				sorted[i].Name, sorted[i].StartLBA, sorted[i].EndLBA)
		}
	}
	return parts, nil
}

func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func decodeUTF16LEName(raw []byte) string {
	units := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		u := binary.LittleEndian.Uint16(raw[i : i+2])
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return strings.TrimSpace(string(utf16.Decode(units)))
}
