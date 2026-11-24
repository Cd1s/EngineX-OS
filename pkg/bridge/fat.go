package bridge

import (
	"encoding/binary"
	"errors"
	"io"
)

var errUnownedClusters = errors.New("bridge: the file's clusters are not its own in the FAT")

// fatChainOwned reports whether the FAT16/FAT32 volume read through dev still gives a file of
// size bytes, whose first data sector is first (in the volume's sectors), all of its clusters:
// every entry from its first cluster is in use and the chain ends after as many clusters as the
// size needs (#321). A file a pull cut while its entry was ahead of its FAT chain names free
// clusters, and removing it is a file-system error. ok is false when the volume is not one this
// reads (FAT12, an odd boot sector).
func fatChainOwned(dev io.ReaderAt, first, size int64) (owned, ok bool) {
	var bs [512]byte
	if _, err := dev.ReadAt(bs[:], 0); err != nil || bs[510] != 0x55 || bs[511] != 0xaa {
		return false, false
	}
	le := binary.LittleEndian
	bps, spc := int64(le.Uint16(bs[11:])), int64(bs[13])
	rsvd, nfats, rootEnt := int64(le.Uint16(bs[14:])), int64(bs[16]), int64(le.Uint16(bs[17:]))
	total, fatSz := int64(le.Uint16(bs[19:])), int64(le.Uint16(bs[22:]))
	if total == 0 {
		total = int64(le.Uint32(bs[32:]))
	}
	fat32 := fatSz == 0 // as Linux decides it, whatever the cluster count
	if fat32 {
		fatSz = int64(le.Uint32(bs[36:]))
	}
	if bps < 512 || bps > 4096 || bps&(bps-1) != 0 || spc == 0 || spc&(spc-1) != 0 || nfats == 0 || fatSz == 0 {
		return false, false
	}
	rootSecs := (rootEnt*32 + bps - 1) / bps
	data := rsvd + nfats*fatSz + rootSecs
	if total <= data {
		return false, false
	}
	clusters := (total-data)/spc + 2 // one past the last cluster
	width := int64(4)
	if !fat32 {
		if clusters-2 <= 0xff4 {
			return false, false // FAT12: not read
		}
		width = 2
	}
	if size <= 0 || first < data {
		return false, true
	}
	want := (size + spc*bps - 1) / (spc * bps)
	c := (first-data)/spc + 2
	var e [4]byte
	for n := int64(1); ; n++ {
		if c < 2 || c >= clusters || n > want {
			return false, true
		}
		if _, err := dev.ReadAt(e[:width], rsvd*bps+c*width); err != nil {
			return false, false
		}
		next := int64(le.Uint16(e[:]))
		end := next >= 0xfff8
		if width == 4 {
			next = int64(le.Uint32(e[:]) & 0x0fffffff)
			end = next >= 0x0ffffff8
		}
		switch {
		case end:
			return n == want, true
		case next < 2:
			return false, true // free, or reserved
		}
		c = next
	}
}
