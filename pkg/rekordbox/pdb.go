package rekordbox

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

var le = binary.LittleEndian

// External exports may contain unknown audio metadata. Normalize it only while
// reading; libraries built by our writers must pass strict validation instead.
func normalizeParsedAudioMetadata(t *Track) {
	if math.IsNaN(t.BPM) || math.IsInf(t.BPM, 0) || t.BPM < 0 || t.BPM > 1000 {
		t.BPM = 0
	}
	if math.IsNaN(t.Duration) || math.IsInf(t.Duration, 0) || t.Duration < 0 || t.Duration > 1e7 {
		t.Duration = 0
	}
	if t.SampleRate < 0 || t.SampleRate > 768000 {
		t.SampleRate = 0
	}
}

// ParsePDB reads DeviceSQL pages through ReaderAt, keeping memory proportional to
// metadata rather than the on-disk database (which may contain deleted pages).
func ParsePDB(r io.ReaderAt, size int64) (*Library, error) {
	const maxPDBBytes = 512 << 20
	if size <= 0 || size > maxPDBBytes {
		return nil, fmt.Errorf("rekordbox: invalid pdb size %d", size)
	}
	h := make([]byte, 28)
	if _, err := r.ReadAt(h, 0); err != nil {
		return nil, fmt.Errorf("pdb header: %w", err)
	}
	ps32, nt32 := le.Uint32(h[4:]), le.Uint32(h[8:])
	if ps32 < 512 || ps32 > 65536 || size%int64(ps32) != 0 || nt32 > (ps32-28)/16 {
		return nil, fmt.Errorf("rekordbox: invalid page size/table count")
	}
	ps, nt := int(ps32), int(nt32)
	tables := make([]byte, nt*16)
	if _, err := r.ReadAt(tables, 28); err != nil {
		return nil, err
	}
	trackCapacity := min(max(int(size/400), 16), 4096)
	l := &Library{Artwork: map[uint32]string{}, Keys: map[uint32]string{}}

	type trackRow struct {
		recordHash                         string
		trackID, artistID, composerID      uint32
		artworkID, keyID, genreID          uint32
		albumID, labelID, remixerID        uint32
		sampleRate, fileSize, bitrate      uint32
		bpmRaw                             uint32
		year                               uint16
		bitDepth, duration                 uint16
		rating                             byte
		analysisPath, comment, title, path string
	}
	tracks := make([]trackRow, 0, trackCapacity)
	type entry struct{ index, track uint32 }
	entries := map[uint32][]entry{}
	artists, albums, genres, labels := map[uint32]string{}, map[uint32]string{}, map[uint32]string{}, map[uint32]string{}
	seen := map[uint32]bool{}
	p := make([]byte, ps)

	processRow := func(typ uint32, b []byte) error {
		switch typ {
		case 0:
			if len(b) < 136 {
				return fmt.Errorf("pdb: short track row")
			}
			var ss [21]string
			for _, i := range []int{14, 16, 17, 20} {
				off := int(le.Uint16(b[94+2*i:]))
				s, err := readDeviceSQLString(b, off)
				if err != nil {
					return fmt.Errorf("pdb track string %d: %w", i, err)
				}
				ss[i] = s
			}
			trackID := le.Uint32(b[72:])
			if trackID == 0 || ss[20] == "" {
				return nil
			}
			// Row offsets do not encode sizes and the heap may have deleted holes.
			// Hash through the last referenced string, excluding unrelated padding.
			end := 136
			for i := 0; i < 21; i++ {
				off := int(le.Uint16(b[94+2*i:]))
				if off < 0 || off >= len(b) {
					continue
				}
				var n int
				if b[off]&1 == 1 {
					n = int(b[off] >> 1)
				} else {
					if off+4 > len(b) {
						continue
					}
					n = int(le.Uint16(b[off+1:]))
					if n < 4 {
						continue
					}
				}
				if off+n <= len(b) && off+n > end {
					end = off + n
				}
			}
			hash := sha256.Sum256(b[:end])
			tracks = append(tracks, trackRow{
				recordHash:   hex.EncodeToString(hash[:]),
				trackID:      trackID,
				artistID:     le.Uint32(b[68:]),
				composerID:   le.Uint32(b[12:]),
				artworkID:    le.Uint32(b[28:]),
				keyID:        le.Uint32(b[32:]),
				genreID:      le.Uint32(b[60:]),
				albumID:      le.Uint32(b[64:]),
				remixerID:    le.Uint32(b[44:]),
				labelID:      le.Uint32(b[40:]),
				sampleRate:   le.Uint32(b[8:]),
				fileSize:     le.Uint32(b[16:]),
				bitrate:      le.Uint32(b[48:]),
				bpmRaw:       le.Uint32(b[56:]),
				year:         le.Uint16(b[80:]),
				bitDepth:     le.Uint16(b[82:]),
				duration:     le.Uint16(b[84:]),
				rating:       b[89],
				analysisPath: ss[14],
				comment:      ss[16],
				title:        ss[17],
				path:         ss[20],
			})
		case 1, 4, 5, 13:
			if len(b) < 5 {
				return fmt.Errorf("pdb: short lookup row")
			}
			id := le.Uint32(b)
			off := 4
			if typ == 5 {
				off = 8
			}
			name, err := readDeviceSQLString(b, off)
			if err != nil {
				return fmt.Errorf("pdb lookup %d: %w", typ, err)
			}
			switch typ {
			case 1:
				genres[id] = name
			case 4:
				labels[id] = name
			case 5:
				l.Keys[id] = name
			case 13:
				l.Artwork[id] = name
			}
		case 2:
			if len(b) < 10 {
				return fmt.Errorf("pdb: short artist row")
			}
			id := le.Uint32(b[4:])
			off := int(b[9])
			if le.Uint16(b)&4 != 0 {
				if len(b) < 12 {
					return fmt.Errorf("pdb: short far artist")
				}
				off = int(le.Uint16(b[10:]))
			}
			name, err := readDeviceSQLString(b, off)
			if err != nil {
				return fmt.Errorf("pdb lookup %d: %w", typ, err)
			}
			artists[id] = name
		case 3:
			if len(b) < 22 {
				return fmt.Errorf("pdb: short album row")
			}
			id := le.Uint32(b[12:])
			off := int(b[21])
			if le.Uint16(b)&4 != 0 {
				if len(b) < 24 {
					return fmt.Errorf("pdb: short far album")
				}
				off = int(le.Uint16(b[22:]))
			}
			name, err := readDeviceSQLString(b, off)
			if err != nil {
				return fmt.Errorf("pdb lookup %d: %w", typ, err)
			}
			albums[id] = name
		case 7:
			if len(b) < 21 {
				return fmt.Errorf("pdb: short playlist")
			}
			name, err := readDeviceSQLString(b, 20)
			if err != nil {
				return err
			}
			l.Playlists = append(l.Playlists, Playlist{ID: le.Uint32(b[12:]), ParentID: le.Uint32(b), SortOrder: le.Uint32(b[8:]), Folder: le.Uint32(b[16:]) != 0, Name: name})
		case 8:
			if len(b) < 12 {
				return fmt.Errorf("pdb: short playlist entry")
			}
			id := le.Uint32(b[8:])
			entries[id] = append(entries[id], entry{le.Uint32(b), le.Uint32(b[4:])})
		case 16:
			l.Columns = append(l.Columns, append([]byte(nil), b...))
		}
		return nil
	}

	for ti := 0; ti < nt; ti++ {
		t := tables[ti*16:]
		typ, first, last := le.Uint32(t), le.Uint32(t[8:]), le.Uint32(t[12:])
		for n := first; n != 0; {
			if seen[n] || int64(n) >= size/int64(ps) {
				return nil, fmt.Errorf("pdb: invalid/cyclic page %d", n)
			}
			seen[n] = true
			if _, err := r.ReadAt(p, int64(n)*int64(ps)); err != nil {
				return nil, err
			}
			if le.Uint32(p[4:]) != n || le.Uint32(p[8:]) != typ {
				return nil, fmt.Errorf("pdb: page %d type/index mismatch", n)
			}
			if p[27]&0x40 == 0 {
				count := int(le.Uint32(p[24:]) & 0x1fff)
				indexBytes := (count / 16) * 36
				if rem := count % 16; rem != 0 {
					indexBytes += 4 + 2*rem
				}
				heapEnd := ps - indexBytes
				if heapEnd < 40 {
					return nil, fmt.Errorf("pdb: row index overlaps header")
				}
				offsets := make([]int, 0, count)
				for i := 0; i < count; i++ {
					base := ps - (i/16)*36
					off := 40 + int(le.Uint16(p[base-6-2*(i%16):]))
					if le.Uint16(p[base-4:])&(1<<uint(i%16)) == 0 {
						continue
					}
					if off < 40 || off >= heapEnd {
						return nil, fmt.Errorf("pdb: row outside heap")
					}
					offsets = append(offsets, off)
				}
				sort.Ints(offsets)
				for i, off := range offsets {
					end := heapEnd
					if i+1 < len(offsets) {
						end = offsets[i+1]
					}
					if end <= off {
						return nil, fmt.Errorf("pdb: overlapping row")
					}
					if err := processRow(typ, p[off:end]); err != nil {
						return nil, err
					}
				}
			}
			if n == last {
				break
			}
			n = le.Uint32(p[12:])
			if n == 0 {
				return nil, fmt.Errorf("pdb: page chain ends before last page")
			}
		}
	}

	knownTracks := make(map[uint32]bool, len(tracks))
	l.Tracks = make([]Track, 0, len(tracks))
	for _, row := range tracks {
		var bpm float64
		if row.bpmRaw != 0 && row.bpmRaw != 0x7fffffff && row.bpmRaw != 0xffffffff && row.bpmRaw <= 100000 {
			bpm = float64(row.bpmRaw) / 100
		}
		t := Track{ID: row.trackID, Path: row.path, AnalysisPath: row.analysisPath, Title: row.title, Comment: row.comment, Artist: artists[row.artistID], Album: albums[row.albumID], Genre: genres[row.genreID], Label: labels[row.labelID], Composer: artists[row.composerID], Remixer: artists[row.remixerID], Key: l.Keys[row.keyID], ArtworkPath: l.Artwork[row.artworkID], BPM: bpm, SampleRate: int(row.sampleRate), FileSize: int64(row.fileSize), Bitrate: int(row.bitrate), Year: int(row.year), BitDepth: int(row.bitDepth), Duration: float64(row.duration), Rating: int(row.rating), RecordHash: row.recordHash}
		normalizeParsedAudioMetadata(&t)
		l.Tracks = append(l.Tracks, t)
		knownTracks[t.ID] = true
	}
	for i := range l.Tracks { // include lookup metadata in the incremental fingerprint
		l.Tracks[i].RecordHash += ":" + RecordHash(l.Tracks[i])
	}
	for i := range l.Playlists {
		es := entries[l.Playlists[i].ID]
		delete(entries, l.Playlists[i].ID)
		sort.SliceStable(es, func(i, j int) bool { return es[i].index < es[j].index })
		for _, e := range es {
			if knownTracks[e.track] {
				l.Playlists[i].Tracks = append(l.Playlists[i].Tracks, e.track)
			}
		}
	}
	// Rekordbox leaves entries of deleted playlists behind; ignore them.
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return l, nil
}

// CheckPDBExtent reports an error when the export.pdb at path names a page past its end: the
// header's next unused page, or a table's empty candidate page (#321 review R4). ParsePDB follows
// only each table's chain of pages, so a copy cut by the whole pages after them still reads.
func CheckPDBExtent(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := make([]byte, 28)
	if _, err := f.ReadAt(h, 0); err != nil {
		return fmt.Errorf("pdb header: %w", err)
	}
	ps, nt, next := int64(le.Uint32(h[4:])), int64(le.Uint32(h[8:])), int64(le.Uint32(h[12:]))
	if ps < 512 || ps > 65536 || st.Size()%ps != 0 || nt > (ps-28)/16 {
		return fmt.Errorf("rekordbox: invalid page size/table count")
	}
	pages := st.Size() / ps
	if next > pages {
		return fmt.Errorf("pdb: next unused page %d past the file's %d pages", next, pages)
	}
	tables := make([]byte, nt*16)
	if _, err := f.ReadAt(tables, 28); err != nil {
		return err
	}
	for i := int64(0); i < nt; i++ {
		if c := int64(le.Uint32(tables[i*16+4:])); c != 0 && c < next && c >= pages {
			return fmt.Errorf("pdb: table %d's empty candidate page %d past the file's %d pages", i, c, pages)
		}
	}
	return nil
}

func ReadPDB(path string) (*Library, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := f.Stat()
	if err != nil {
		return nil, err
	}
	l, err := ParsePDB(f, s.Size())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}
