package prolink

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
)

// Pioneer Rekordbox DeviceSQL track_row.file_type values (rekordbox_pdb.ksy).
const (
	pdbFileTypeMP3  uint16 = 1
	pdbFileTypeM4A  uint16 = 4
	pdbFileTypeFLAC uint16 = 5
	pdbFileTypeWAV  uint16 = 11
	pdbFileTypeAIFF uint16 = 12
)

func pdbFileTypeForPath(p string) uint16 {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".mp3":
		return pdbFileTypeMP3
	case ".m4a", ".aac", ".mp4":
		return pdbFileTypeM4A
	case ".flac":
		return pdbFileTypeFLAC
	case ".wav":
		return pdbFileTypeWAV
	case ".aif", ".aiff":
		return pdbFileTypeAIFF
	default:
		return pdbFileTypeWAV
	}
}

// ExportTrackSummary holds independently decoded track fields from export.pdb.
type ExportTrackSummary struct {
	ID             uint32
	Path           string
	AnalysisPath   string
	Title          string
	Artist         string
	Key            string
	ArtworkPath    string
	HotCueAutoLoad string
	FileType       uint16
	SampleRate     uint32
	BitDepth       uint16
	Bitrate        uint32
	BPMx100        uint32
	DurationSec    uint16
	FileSize       uint32
}

// ExportCheckReport summarizes a validated Pioneer DeviceSQL export.
type ExportCheckReport struct {
	PageSize       uint32
	TotalPages     uint32
	TablesChecked  int
	Tracks         []ExportTrackSummary
	PlaylistsCount int
	ArtworksCount  int
	ANLZChecked    int
}

func chkDecodeDeviceSQLString(buf []byte, off int) (string, int, error) {
	if off < 0 || off >= len(buf) {
		return "", 0, errors.New("string offset out of bounds")
	}
	kind := buf[off]
	if kind&1 == 1 {
		n := int(kind >> 1)
		if n == 0 {
			return "", 1, nil
		}
		if off+n > len(buf) {
			return "", 0, errors.New("truncated short ASCII string")
		}
		return string(buf[off+1 : off+n]), n, nil
	}
	if off+4 > len(buf) {
		return "", 0, errors.New("truncated long string header")
	}
	n := int(binary.LittleEndian.Uint16(buf[off+1:]))
	if n < 4 || off+n > len(buf) {
		return "", 0, errors.New("invalid long string length")
	}
	body := buf[off+4 : off+n]
	switch kind {
	case 0x40:
		return string(body), n, nil
	case 0x90:
		if len(body)%2 != 0 {
			return "", 0, errors.New("odd UTF-16LE byte length")
		}
		u := make([]uint16, len(body)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(body[2*i:])
		}
		return strings.TrimRight(string(utf16.Decode(u)), "\x00"), n, nil
	default:
		return "", 0, fmt.Errorf("unknown DeviceSQL string tag 0x%02x", kind)
	}
}

// CheckPDBBytes validates export.pdb against strict CDJ-3000 / Rekordbox DeviceSQL
// page-chain, string-offset, track-field, and foreign-key invariants (#63).
func CheckPDBBytes(file []byte) (*ExportCheckReport, error) {
	if len(file) < 28 {
		return nil, errors.New("pdbcheck: file shorter than 28-byte header")
	}
	le := binary.LittleEndian
	ps := int(le.Uint32(file[4:]))
	nt := int(le.Uint32(file[8:]))
	nextUnused := le.Uint32(file[12:])
	if ps < 512 || ps > 65536 || len(file)%ps != 0 || nt <= 0 || nt > (ps-28)/16 {
		return nil, fmt.Errorf("pdbcheck: invalid pageSize=%d numTables=%d len=%d", ps, nt, len(file))
	}
	totalPages := uint32(len(file) / ps)
	if nextUnused != totalPages {
		return nil, fmt.Errorf("pdbcheck: nextUnused=%d != totalPages=%d", nextUnused, totalPages)
	}

	type rawRow struct {
		typ  uint32
		data []byte
	}
	var allRows []rawRow
	usedPages := map[uint32]string{0: "file_header"}

	for ti := 0; ti < nt; ti++ {
		th := file[28+ti*16 : 28+(ti+1)*16]
		typ := le.Uint32(th[0:])
		empty := le.Uint32(th[4:])
		first := le.Uint32(th[8:])
		last := le.Uint32(th[12:])

		if first == 0 || first >= totalPages || last == 0 || last >= totalPages {
			return nil, fmt.Errorf("pdbcheck: table %d invalid page bounds first=%d last=%d", typ, first, last)
		}
		if empty == 0 || empty >= nextUnused {
			return nil, fmt.Errorf("pdbcheck: table %d invalid empty_candidate=%d (nextUnused=%d)", typ, empty, nextUnused)
		}
		emptySlice := file[int(empty)*ps : int(empty+1)*ps]
		for _, b := range emptySlice {
			if b != 0 {
				return nil, fmt.Errorf("pdbcheck: table %d empty_candidate page %d is not blank", typ, empty)
			}
		}
		if prev, dup := usedPages[empty]; dup {
			return nil, fmt.Errorf("pdbcheck: empty_candidate page %d aliases %s", empty, prev)
		}
		usedPages[empty] = fmt.Sprintf("table_%d_empty", typ)

		// First page of every table must be an index page (0x64, 0x1fff, 0x1fff).
		idxPage := file[int(first)*ps : int(first+1)*ps]
		if idxPage[27] != 0x64 || le.Uint16(idxPage[0x20:]) != 0x1fff || le.Uint16(idxPage[0x22:]) != 0x1fff {
			return nil, fmt.Errorf("pdbcheck: table %d first page %d is not a valid index page (flags=0x%02x)", typ, first, idxPage[27])
		}

		for n := first; ; {
			if n == 0 || n >= totalPages {
				return nil, fmt.Errorf("pdbcheck: table %d page %d out of range", typ, n)
			}
			if prev, dup := usedPages[n]; dup {
				return nil, fmt.Errorf("pdbcheck: table %d page %d aliases %s", typ, n, prev)
			}
			usedPages[n] = fmt.Sprintf("table_%d_page", typ)

			p := file[int(n)*ps : int(n+1)*ps]
			if le.Uint32(p[4:]) != n || le.Uint32(p[8:]) != typ {
				return nil, fmt.Errorf("pdbcheck: page %d header mismatch (got idx=%d typ=%d, want %d/%d)", n, le.Uint32(p[4:]), le.Uint32(p[8:]), n, typ)
			}
			if p[27]&0x40 == 0 {
				if p[27] != 0x24 {
					return nil, fmt.Errorf("pdbcheck: data page %d unexpected flags 0x%02x", n, p[27])
				}
				count := int(le.Uint32(p[24:]) & 0x1fff)
				indexBytes := (count / 16) * 36
				if rem := count % 16; rem != 0 {
					indexBytes += 4 + 2*rem
				}
				heapEnd := ps - indexBytes
				if heapEnd < 40 {
					return nil, fmt.Errorf("pdbcheck: page %d row index overlaps header", n)
				}
				var offs []int
				for i := 0; i < count; i++ {
					base := ps - (i/16)*36
					live := le.Uint16(p[base-4:])&(1<<uint(i%16)) != 0
					if !live {
						continue
					}
					off := 40 + int(le.Uint16(p[base-6-2*(i%16):]))
					if off < 40 || off >= heapEnd {
						return nil, fmt.Errorf("pdbcheck: page %d row %d offset %d outside heap [40,%d)", n, i, off, heapEnd)
					}
					offs = append(offs, off)
				}
				sort.Ints(offs)
				for i, off := range offs {
					end := heapEnd
					if i+1 < len(offs) {
						end = offs[i+1]
					}
					if end <= off {
						return nil, fmt.Errorf("pdbcheck: page %d overlapping row at %d", n, off)
					}
					allRows = append(allRows, rawRow{typ: typ, data: p[off:end]})
				}
			}
			if n == last {
				nextAfterLast := le.Uint32(p[12:])
				if nextAfterLast != empty {
					return nil, fmt.Errorf("pdbcheck: table %d last page %d links to %d, want empty_candidate %d", typ, last, nextAfterLast, empty)
				}
				break
			}
			n = le.Uint32(p[12:])
		}
	}

	artists := map[uint32]string{}
	keys := map[uint32]string{}
	artworks := map[uint32]string{}

	for _, r := range allRows {
		b := r.data
		switch r.typ {
		case 2:
			if len(b) < 10 {
				return nil, errors.New("pdbcheck: short artist row")
			}
			id := le.Uint32(b[4:])
			if id == 0 {
				return nil, errors.New("pdbcheck: zero artist id")
			}
			if _, dup := artists[id]; dup {
				return nil, fmt.Errorf("pdbcheck: duplicate artist id %d", id)
			}
			off := int(b[9])
			if le.Uint16(b)&4 != 0 {
				if len(b) < 12 {
					return nil, errors.New("pdbcheck: short far artist row")
				}
				off = int(le.Uint16(b[10:]))
			}
			s, _, err := chkDecodeDeviceSQLString(b, off)
			if err != nil {
				return nil, fmt.Errorf("pdbcheck: artist %d string: %w", id, err)
			}
			artists[id] = s
		case 5:
			if len(b) < 9 {
				return nil, errors.New("pdbcheck: short key row")
			}
			id := le.Uint32(b[0:])
			if id == 0 {
				return nil, errors.New("pdbcheck: zero key id")
			}
			if _, dup := keys[id]; dup {
				return nil, fmt.Errorf("pdbcheck: duplicate key id %d", id)
			}
			s, _, err := chkDecodeDeviceSQLString(b, 8)
			if err != nil {
				return nil, fmt.Errorf("pdbcheck: key %d string: %w", id, err)
			}
			keys[id] = s
		case 13:
			if len(b) < 5 {
				return nil, errors.New("pdbcheck: short artwork row")
			}
			id := le.Uint32(b[0:])
			if id == 0 {
				return nil, errors.New("pdbcheck: zero artwork id")
			}
			if _, dup := artworks[id]; dup {
				return nil, fmt.Errorf("pdbcheck: duplicate artwork id %d", id)
			}
			s, _, err := chkDecodeDeviceSQLString(b, 4)
			if err != nil || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("pdbcheck: invalid artwork %d path %q: %v", id, s, err)
			}
			artworks[id] = s
		}
	}

	var tracks []ExportTrackSummary
	trackSet := map[uint32]bool{}
	type plInfo struct {
		parent uint32
		name   string
		folder bool
	}
	playlists := map[uint32]plInfo{}
	siblings := map[string]bool{}
	type plEntry struct {
		pos     uint32
		trackID uint32
	}
	entriesByList := map[uint32][]plEntry{}

	for _, r := range allRows {
		b := r.data
		switch r.typ {
		case 0:
			if len(b) < 136 {
				return nil, errors.New("pdbcheck: short track row (<136 bytes)")
			}
			tid := le.Uint32(b[72:])
			if tid == 0 || trackSet[tid] {
				return nil, fmt.Errorf("pdbcheck: zero or duplicate track id %d", tid)
			}
			trackSet[tid] = true

			var strs [21]string
			prevEnd := 136
			for i := 0; i < 21; i++ {
				off := int(le.Uint16(b[94+2*i:]))
				if off < 136 {
					return nil, fmt.Errorf("pdbcheck: track %d string[%d] offset=%d (<136)", tid, i, off)
				}
				if off < prevEnd {
					return nil, fmt.Errorf("pdbcheck: track %d string[%d] offset=%d overlaps previous string end %d", tid, i, off, prevEnd)
				}
				s, n, err := chkDecodeDeviceSQLString(b, off)
				if err != nil {
					return nil, fmt.Errorf("pdbcheck: track %d string[%d]: %w", tid, i, err)
				}
				strs[i] = s
				prevEnd = off + n
			}

			if strs[7] != "ON" {
				return nil, fmt.Errorf("pdbcheck: track %d autoload_hot_cues (string[7])=%q, want \"ON\"", tid, strs[7])
			}
			if strs[20] == "" || !strings.HasPrefix(strs[20], "/") {
				return nil, fmt.Errorf("pdbcheck: track %d invalid path %q", tid, strs[20])
			}
			ft := le.Uint16(b[90:])
			switch ft {
			case pdbFileTypeMP3, pdbFileTypeM4A, pdbFileTypeFLAC, pdbFileTypeWAV, pdbFileTypeAIFF:
			default:
				return nil, fmt.Errorf("pdbcheck: track %d invalid file_type=%d", tid, ft)
			}

			artistID := le.Uint32(b[68:])
			if artistID != 0 && artists[artistID] == "" {
				return nil, fmt.Errorf("pdbcheck: track %d references missing artist_id %d", tid, artistID)
			}
			keyID := le.Uint32(b[32:])
			if keyID != 0 && keys[keyID] == "" {
				return nil, fmt.Errorf("pdbcheck: track %d references missing key_id %d", tid, keyID)
			}
			artID := le.Uint32(b[28:])
			if artID != 0 && artworks[artID] == "" {
				return nil, fmt.Errorf("pdbcheck: track %d references missing artwork_id %d", tid, artID)
			}

			tracks = append(tracks, ExportTrackSummary{
				ID:             tid,
				Path:           strs[20],
				AnalysisPath:   strs[14],
				Title:          strs[17],
				Artist:         artists[artistID],
				Key:            keys[keyID],
				ArtworkPath:    artworks[artID],
				HotCueAutoLoad: strs[7],
				FileType:       ft,
				SampleRate:     le.Uint32(b[8:]),
				FileSize:       le.Uint32(b[16:]),
				Bitrate:        le.Uint32(b[48:]),
				BPMx100:        le.Uint32(b[56:]),
				DurationSec:    le.Uint16(b[84:]),
				BitDepth:       le.Uint16(b[82:]),
			})
		case 7:
			if len(b) < 21 {
				return nil, errors.New("pdbcheck: short playlist row")
			}
			parent := le.Uint32(b[0:])
			pid := le.Uint32(b[12:])
			folder := le.Uint32(b[16:]) != 0
			name, _, err := chkDecodeDeviceSQLString(b, 20)
			if err != nil {
				return nil, fmt.Errorf("pdbcheck: playlist %d name: %w", pid, err)
			}
			if pid == 0 {
				return nil, errors.New("pdbcheck: zero playlist id")
			}
			if _, dup := playlists[pid]; dup {
				return nil, fmt.Errorf("pdbcheck: duplicate playlist id %d", pid)
			}
			sibKey := fmt.Sprintf("%d\x00%s", parent, name)
			if siblings[sibKey] {
				return nil, fmt.Errorf("pdbcheck: duplicate sibling playlist %q under %d", name, parent)
			}
			siblings[sibKey] = true
			playlists[pid] = plInfo{parent: parent, name: name, folder: folder}
		case 8:
			if len(b) < 12 {
				return nil, errors.New("pdbcheck: short playlist entry row")
			}
			pos, tid, pid := le.Uint32(b[0:]), le.Uint32(b[4:]), le.Uint32(b[8:])
			entriesByList[pid] = append(entriesByList[pid], plEntry{pos: pos, trackID: tid})
		}
	}

	for pid := range playlists {
		seen := map[uint32]bool{}
		for cur := pid; cur != 0; {
			if seen[cur] {
				return nil, fmt.Errorf("pdbcheck: playlist parent cycle at %d", cur)
			}
			seen[cur] = true
			info, ok := playlists[cur]
			if !ok {
				return nil, fmt.Errorf("pdbcheck: playlist %d references missing parent %d", pid, cur)
			}
			cur = info.parent
		}
	}
	for pid, list := range entriesByList {
		pl, ok := playlists[pid]
		if !ok {
			return nil, fmt.Errorf("pdbcheck: playlist entry references missing playlist %d", pid)
		}
		if pl.folder {
			return nil, fmt.Errorf("pdbcheck: folder playlist %d must not contain track entries", pid)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].pos < list[j].pos })
		for i, e := range list {
			if e.pos != uint32(i+1) {
				return nil, fmt.Errorf("pdbcheck: playlist %d non-sequential entry position %d (want %d)", pid, e.pos, i+1)
			}
			if !trackSet[e.trackID] {
				return nil, fmt.Errorf("pdbcheck: playlist %d references missing track %d", pid, e.trackID)
			}
		}
	}

	return &ExportCheckReport{
		PageSize:       uint32(ps),
		TotalPages:     totalPages,
		TablesChecked:  nt,
		Tracks:         tracks,
		PlaylistsCount: len(playlists),
		ArtworksCount:  len(artworks),
	}, nil
}

func chkDecodeUTF16BE(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", errors.New("odd UTF-16BE byte length")
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return strings.TrimSuffix(string(utf16.Decode(u)), "\x00"), nil
}

// CheckANLZFiles validates a track's (.DAT, .EXT, .2EX) analysis triple against
// Pioneer CDJ / Rekordbox wire invariants (#63).
func CheckANLZFiles(dat, ext, twoEx []byte, wantPath string, isMP3 bool) error {
	be := binary.BigEndian
	type secInfo struct {
		tag  string
		hlen int
		body []byte // full section bytes [0:sn]
	}
	parsePMAI := func(label string, raw []byte) ([]secInfo, error) {
		if len(raw) < 28 || string(raw[:4]) != "PMAI" {
			return nil, fmt.Errorf("anlzcheck (%s): invalid PMAI header", label)
		}
		h := int(be.Uint32(raw[4:]))
		n := int(be.Uint32(raw[8:]))
		if h != 28 || n != len(raw) {
			return nil, fmt.Errorf("anlzcheck (%s): PMAI header=%d total=%d fileLen=%d", label, h, n, len(raw))
		}
		var out []secInfo
		for off := h; off < n; {
			if n-off < 12 {
				return nil, fmt.Errorf("anlzcheck (%s): truncated section at %d", label, off)
			}
			s := raw[off:]
			tag := string(s[:4])
			sh := int(be.Uint32(s[4:]))
			sn := int(be.Uint32(s[8:]))
			if sh < 12 || sn < sh || off+sn > n {
				return nil, fmt.Errorf("anlzcheck (%s): invalid %s section lengths (%d/%d)", label, tag, sh, sn)
			}
			out = append(out, secInfo{tag: tag, hlen: sh, body: s[:sn]})
			off += sn
		}
		return out, nil
	}

	datSecs, err := parsePMAI("DAT", dat)
	if err != nil {
		return err
	}
	extSecs, err := parsePMAI("EXT", ext)
	if err != nil {
		return err
	}
	twoSecs, err := parsePMAI("2EX", twoEx)
	if err != nil {
		return err
	}

	checkPPTH := func(label string, secs []secInfo) error {
		for _, sc := range secs {
			if sc.tag != "PPTH" {
				continue
			}
			if sc.hlen != 16 || len(sc.body) < 16 {
				return fmt.Errorf("anlzcheck (%s): invalid PPTH header len %d", label, sc.hlen)
			}
			byteLen := int(be.Uint32(sc.body[12:16]))
			if 16+byteLen != len(sc.body) {
				return fmt.Errorf("anlzcheck (%s): PPTH string len %d != payload %d", label, byteLen, len(sc.body)-16)
			}
			gotPath, err := chkDecodeUTF16BE(sc.body[16:])
			if err != nil {
				return fmt.Errorf("anlzcheck (%s): PPTH UTF-16BE: %w", label, err)
			}
			if wantPath != "" && gotPath != wantPath {
				return fmt.Errorf("anlzcheck (%s): PPTH=%q != track path %q", label, gotPath, wantPath)
			}
			return nil
		}
		return fmt.Errorf("anlzcheck (%s): missing PPTH section", label)
	}
	if err := checkPPTH("DAT", datSecs); err != nil {
		return err
	}
	if err := checkPPTH("EXT", extSecs); err != nil {
		return err
	}
	if err := checkPPTH("2EX", twoSecs); err != nil {
		return err
	}

	hasPVBR, hasPQTZ := false, false
	for _, sc := range datSecs {
		s := sc.body
		switch sc.tag {
		case "PVBR":
			hasPVBR = true
			if sc.hlen != 16 || len(s) != 1620 {
				return fmt.Errorf("anlzcheck (DAT): invalid PVBR geometry (%d/%d)", sc.hlen, len(s))
			}
		case "PQTZ":
			hasPQTZ = true
			if sc.hlen != 24 || len(s) < 24 || be.Uint32(s[16:20]) != 0x00800000 {
				return errors.New("anlzcheck (DAT): invalid PQTZ header")
			}
			cnt := int(be.Uint32(s[20:24]))
			if 24+cnt*8 != len(s) {
				return fmt.Errorf("anlzcheck (DAT): PQTZ count=%d len=%d", cnt, len(s))
			}
			var prevMS uint32
			for i := 0; i < cnt; i++ {
				p := s[24+i*8:]
				num := be.Uint16(p[0:2])
				bpm100 := be.Uint16(p[2:4])
				tms := be.Uint32(p[4:8])
				if num < 1 || num > 4 || bpm100 == 0 {
					return fmt.Errorf("anlzcheck (DAT): PQTZ beat %d invalid number=%d bpm100=%d", i, num, bpm100)
				}
				if i > 0 && tms <= prevMS {
					return fmt.Errorf("anlzcheck (DAT): non-monotonic PQTZ beat %d (%d <= %d)", i, tms, prevMS)
				}
				prevMS = tms
			}
		case "PCOB":
			if sc.hlen != 24 || len(s) < 24 {
				return errors.New("anlzcheck (DAT): invalid PCOB header")
			}
			kind := be.Uint32(s[12:16])
			memCount := be.Uint16(s[16:18])
			n := int(be.Uint16(s[18:20]))
			if kind > 1 {
				return fmt.Errorf("anlzcheck (DAT): invalid PCOB kind %d", kind)
			}
			if kind == 1 || n == 0 {
				if memCount != 0xffff {
					return fmt.Errorf("anlzcheck (DAT): PCOB kind=%d n=%d memory_count=0x%04x, want 0xffff", kind, n, memCount)
				}
			} else if memCount != uint16(n-1) {
				return fmt.Errorf("anlzcheck (DAT): PCOB memory_count=%d, want %d", memCount, n-1)
			}
			if 24+n*56 != len(s) {
				return fmt.Errorf("anlzcheck (DAT): PCOB entry length mismatch (%d entries in %d bytes)", n, len(s))
			}
			var prevHot, prevTime uint32
			for i := 0; i < n; i++ {
				p := s[24+i*56 : 24+(i+1)*56]
				if string(p[:4]) != "PCPT" || be.Uint32(p[4:8]) != 28 || be.Uint32(p[8:12]) != 56 {
					return errors.New("anlzcheck (DAT): invalid PCPT entry header")
				}
				hot := be.Uint32(p[12:16])
				first := be.Uint16(p[24:26])
				last := be.Uint16(p[26:28])
				typ := p[28]
				tms := be.Uint32(p[32:36])
				endMS := be.Uint32(p[36:40])
				if kind == 1 {
					if hot < 1 || hot > 8 || hot <= prevHot {
						return fmt.Errorf("anlzcheck (DAT): invalid/unsorted hot cue slot %d", hot)
					}
					if first != 0xffff || last != 0xffff {
						return fmt.Errorf("anlzcheck (DAT): hot cue %d must not chain order_first/last (got 0x%04x/0x%04x)", hot, first, last)
					}
					prevHot = hot
				} else {
					if hot != 0 {
						return fmt.Errorf("anlzcheck (DAT): memory cue has non-zero hot_cue %d", hot)
					}
					wantFirst := uint16(i - 1)
					wantLast := uint16(i + 1)
					if i == 0 {
						wantFirst = 0xffff
					}
					if i == n-1 {
						wantLast = 0xffff
					}
					if first != wantFirst || last != wantLast {
						return fmt.Errorf("anlzcheck (DAT): memory cue %d broken chain (%d,%d want %d,%d)", i, first, last, wantFirst, wantLast)
					}
					if i > 0 && tms < prevTime {
						return fmt.Errorf("anlzcheck (DAT): unsorted memory cue %d", i)
					}
					prevTime = tms
				}
				if typ == 1 && endMS != 0xffffffff {
					return fmt.Errorf("anlzcheck (DAT): point cue endMS=0x%x, want 0xffffffff", endMS)
				}
				if typ == 2 && endMS <= tms {
					return fmt.Errorf("anlzcheck (DAT): loop cue endMS=%d <= start=%d", endMS, tms)
				}
			}
		case "PWAV":
			if sc.hlen != 20 || len(s) != 20+400 || be.Uint32(s[12:16]) != 400 {
				return fmt.Errorf("anlzcheck (DAT): invalid PWAV geometry (hlen=%d len=%d)", sc.hlen, len(s))
			}
		case "PWV2":
			if sc.hlen != 20 || len(s) != 20+100 || be.Uint32(s[12:16]) != 100 {
				return fmt.Errorf("anlzcheck (DAT): invalid PWV2 geometry (hlen=%d len=%d)", sc.hlen, len(s))
			}
		}
	}
	if !hasPQTZ {
		return errors.New("anlzcheck (DAT): missing PQTZ section")
	}
	if isMP3 && !hasPVBR {
		return errors.New("anlzcheck (DAT): missing PVBR section for MP3 track")
	}

	hasPQT2 := false
	pwv5Cols := -1
	for _, sc := range extSecs {
		s := sc.body
		switch sc.tag {
		case "PQT2":
			hasPQT2 = true
			if sc.hlen != 56 || len(s) < 56 || !bytes.Equal(s[16:20], []byte{0x01, 0x00, 0x00, 0x02}) {
				return errors.New("anlzcheck (EXT): invalid PQT2 section")
			}
		case "PCO2":
			if sc.hlen != 20 || len(s) < 20 {
				return errors.New("anlzcheck (EXT): invalid PCO2 header")
			}
			kind := be.Uint32(s[12:16])
			n := int(be.Uint16(s[16:18]))
			off := 20
			for i := 0; i < n; i++ {
				if len(s)-off < 48 {
					return errors.New("anlzcheck (EXT): truncated PCP2 entry")
				}
				p := s[off:]
				sz := int(be.Uint32(p[8:12]))
				if string(p[:4]) != "PCP2" || be.Uint32(p[4:8]) != 16 || sz < 48 || off+sz > len(s) {
					return errors.New("anlzcheck (EXT): invalid PCP2 entry bounds")
				}
				hot := be.Uint32(p[12:16])
				if (kind == 1 && (hot < 1 || hot > 8)) || (kind == 0 && hot != 0) {
					return fmt.Errorf("anlzcheck (EXT): PCP2 kind=%d hot=%d mismatch", kind, hot)
				}
				typ := p[16]
				tms := be.Uint32(p[20:24])
				endMS := be.Uint32(p[24:28])
				if typ == 2 && endMS <= tms {
					return fmt.Errorf("anlzcheck (EXT): PCP2 loop endMS=%d <= start=%d", endMS, tms)
				}
				num, den := be.Uint16(p[36:38]), be.Uint16(p[38:40])
				if (num == 0) != (den == 0) {
					return fmt.Errorf("anlzcheck (EXT): PCP2 partial loop fraction %d/%d", num, den)
				}
				lblLen := int(be.Uint32(p[40:44]))
				if lblLen%2 != 0 || 48+lblLen != sz {
					return fmt.Errorf("anlzcheck (EXT): PCP2 label length %d vs size %d", lblLen, sz)
				}
				off += sz
			}
			if off != len(s) {
				return errors.New("anlzcheck (EXT): trailing bytes in PCO2")
			}
		case "PWV5":
			if sc.hlen != 24 || len(s) < 24 || be.Uint32(s[12:16]) != 2 || be.Uint32(s[20:24]) != 0x00960305 {
				return errors.New("anlzcheck (EXT): invalid PWV5 header")
			}
			cols := int(be.Uint32(s[16:20]))
			if 24+cols*2 != len(s) {
				return errors.New("anlzcheck (EXT): PWV5 length mismatch")
			}
			pwv5Cols = cols
		}
	}
	if !hasPQT2 {
		return errors.New("anlzcheck (EXT): missing PQT2 section")
	}

	pwv7Cols := -1
	for _, sc := range twoSecs {
		s := sc.body
		if sc.tag == "PWV7" {
			if sc.hlen != 24 || len(s) < 24 || be.Uint32(s[12:16]) != 3 || be.Uint32(s[20:24]) != 0x00960000 {
				return errors.New("anlzcheck (2EX): invalid PWV7 header")
			}
			cols := int(be.Uint32(s[16:20]))
			if 24+cols*3 != len(s) {
				return errors.New("anlzcheck (2EX): PWV7 length mismatch")
			}
			pwv7Cols = cols
		}
	}
	if (pwv5Cols >= 0 || pwv7Cols >= 0) && pwv5Cols != pwv7Cols {
		return fmt.Errorf("anlzcheck: PWV5 cols (%d) != PWV7 cols (%d)", pwv5Cols, pwv7Cols)
	}
	return nil
}

// CheckExportDir validates root's PIONEER/rekordbox/export.pdb and all generated
// ANLZ / Artwork files on disk (#63).
func CheckExportDir(root string) (*ExportCheckReport, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	pdbPath := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
	st, err := os.Lstat(pdbPath)
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("exportcheck: export.pdb must be a regular file: %w", err)
	}
	pdbBytes, err := os.ReadFile(pdbPath)
	if err != nil {
		return nil, err
	}
	rep, err := CheckPDBBytes(pdbBytes)
	if err != nil {
		return nil, err
	}

	for _, tr := range rep.Tracks {
		// Verify artwork file exists and decodes as JPEG when ArtworkPath is set.
		if tr.ArtworkPath != "" {
			rel, ok := SafeRel(tr.ArtworkPath)
			if !ok {
				return nil, fmt.Errorf("exportcheck: unsafe artwork path %q", tr.ArtworkPath)
			}
			artFull := filepath.Join(root, rel)
			ast, err := os.Lstat(artFull)
			if err != nil || !ast.Mode().IsRegular() {
				return nil, fmt.Errorf("exportcheck: track %d missing artwork %q", tr.ID, tr.ArtworkPath)
			}
			f, err := os.Open(artFull)
			if err != nil {
				return nil, err
			}
			_, jerr := jpeg.DecodeConfig(f)
			f.Close()
			if jerr != nil {
				return nil, fmt.Errorf("exportcheck: track %d invalid JPEG artwork %q: %w", tr.ID, tr.ArtworkPath, jerr)
			}
		}

		// Verify ANLZ triple whenever .DAT is present on disk.
		if tr.AnalysisPath == "" || !strings.HasSuffix(tr.AnalysisPath, ".DAT") {
			return nil, fmt.Errorf("exportcheck: track %d invalid AnalysisPath %q", tr.ID, tr.AnalysisPath)
		}
		relDAT, ok := SafeRel(tr.AnalysisPath)
		if !ok {
			return nil, fmt.Errorf("exportcheck: track %d unsafe AnalysisPath %q", tr.ID, tr.AnalysisPath)
		}
		datFull := filepath.Join(root, relDAT)
		dst, err := os.Lstat(datFull)
		if os.IsNotExist(err) {
			continue // deferred background worker still writing remaining ANLZ files
		}
		if err != nil || !dst.Mode().IsRegular() {
			return nil, fmt.Errorf("exportcheck: track %d non-regular DAT %q", tr.ID, tr.AnalysisPath)
		}
		stem := strings.TrimSuffix(datFull, ".DAT")
		datBytes, err := os.ReadFile(datFull)
		if err != nil {
			return nil, err
		}
		extBytes, err := os.ReadFile(stem + ".EXT")
		if err != nil {
			return nil, fmt.Errorf("exportcheck: track %d missing EXT: %w", tr.ID, err)
		}
		twoBytes, err := os.ReadFile(stem + ".2EX")
		if err != nil {
			return nil, fmt.Errorf("exportcheck: track %d missing 2EX: %w", tr.ID, err)
		}

		// Check MP3 PVBR requirement when the backing audio file exists and is MP3.
		isMP3 := false
		if strings.EqualFold(filepath.Ext(tr.Path), ".mp3") {
			audioFull := StreamFile(tr.Path)
			if audioFull == "" {
				if relAudio, ok := SafeRel(tr.Path); ok {
					audioFull = filepath.Join(root, relAudio)
				}
			}
			if ast, err := os.Stat(audioFull); err == nil && ast.Mode().IsRegular() {
				isMP3 = true
			}
		}
		if err := CheckANLZFiles(datBytes, extBytes, twoBytes, tr.Path, isMP3); err != nil {
			return nil, fmt.Errorf("exportcheck: track %d (%s): %w", tr.ID, tr.Path, err)
		}
		rep.ANLZChecked++
	}
	return rep, nil
}
