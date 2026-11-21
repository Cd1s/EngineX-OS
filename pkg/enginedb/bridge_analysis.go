package enginedb

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// BridgeAnalysis stores the source ID and, in new rows, where the PWV7 (else PWV5) section
// consumed by shadow display is (RBA2, below). Earlier rows hold the section itself.
// RBA1 is a gzip-wrapped binary record: magic,
// little-endian track ID, four-byte waveform tag, entry width, data length,
// then raw waveform bytes. Other display fields already live in m.db.
// Earlier plain and gzip-JSON records remain readable through UnpackAnalysis.
const compactAnalysisMagic = "RBA1"
const compactAnalysisHeader = 20

// RBA2 keeps the source ID and where the scrolling detail is: the track's own ANLZ files on the
// volume (.2EX for PWV7, then .EXT for PWV5), as volume-relative paths the import resolved. A
// copy of the waveform cost about 100 KB a track; the maintainer's 6000-track stick had a
// 2.7 GB state file, which an update copied whole before its first pass (#253). Not compressed:
// magic, little-endian track ID, then a little-endian uint16 length and the bytes of each path.
const refAnalysisMagic = "RBA2"

// maxDetailFile bounds an ANLZ file read for the display: a PWV7 of 2<<20 entries and the rest.
const maxDetailFile = 16 << 20

func packBridgeRef(id uint32, files []string) []byte {
	b := binary.LittleEndian.AppendUint32([]byte(refAnalysisMagic), id)
	for _, f := range files {
		if len(f) <= 0xffff {
			b = binary.LittleEndian.AppendUint16(b, uint16(len(f)))
			b = append(b, f...)
		}
	}
	return b
}

func unpackBridgeRef(b []byte) (uint32, []string, error) {
	if len(b) < 8 || string(b[:4]) != refAnalysisMagic {
		return 0, nil, fmt.Errorf("enginedb: not a detail reference")
	}
	id := binary.LittleEndian.Uint32(b[4:])
	var files []string
	for rest := b[8:]; len(rest) > 0; {
		if len(rest) < 2 || int(binary.LittleEndian.Uint16(rest)) > len(rest)-2 {
			return 0, nil, fmt.Errorf("enginedb: invalid detail reference")
		}
		n := int(binary.LittleEndian.Uint16(rest))
		files, rest = append(files, string(rest[2:2+n])), rest[2+n:]
	}
	return id, files, nil
}

// detailFiles are the analysis files of a track that can hold the display's detail, in the
// order the display prefers them.
func detailFiles(files []analysisFile) []string {
	var out []string
	for _, ext := range []string{".2EX", ".EXT"} {
		for _, f := range files {
			if strings.EqualFold(path.Ext(f.rel), ext) {
				out = append(out, f.rel)
			}
		}
	}
	return out
}

// BridgeDetail is the scrolling detail a BridgeAnalysis column names or holds, as
// DisplayWaveFromRekordbox reads it: from the track's ANLZ files under root, the volume, for a
// reference (RBA2), else from the column itself. Nil when there is none or it cannot be read.
func BridgeDetail(b []byte, root string, limit int) *rekordbox.Analysis {
	if _, files, err := unpackBridgeRef(b); err == nil {
		a := &rekordbox.Analysis{}
		for _, f := range files {
			data, err := readVolumeFile(root, f, maxDetailFile)
			if err != nil {
				continue
			}
			if one, err := rekordbox.ParseAnalysis(data); err == nil {
				a.Waveforms = append(a.Waveforms, one.Waveforms...)
			}
			// Only the section the display reads, as an RBA1 row held it; a PWV7 found first
			// leaves the .EXT unread.
			if w := displayAnalysisWaveform(a); w != nil {
				return &rekordbox.Analysis{Waveforms: []rekordbox.Waveform{*w}}
			}
		}
		return nil
	}
	b, err := UnpackAnalysis(b, limit)
	if err != nil {
		return nil
	}
	var source struct{ Analysis *rekordbox.Analysis }
	if json.Unmarshal(b, &source) != nil {
		return nil
	}
	return source.Analysis
}

// readVolumeFile reads rel under root as the resolver finds it (case, NFC, macOS's hidden
// .PIONEER), refusing traversal and symlinks.
func readVolumeFile(root, rel string, limit int64) ([]byte, error) {
	r, err := compat.NewResolver(root)
	if err != nil {
		return nil, err
	}
	if rel, err = r.Resolve(rel); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(r.FullPath(rel), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, fmt.Errorf("not a regular file within %d bytes: %s", limit, rel)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

// exportDetailFiles are detailFiles as the export names them, before the import resolved them.
func exportDetailFiles(analysisPath string) []string {
	if analysisPath == "" {
		return nil
	}
	base, err := compat.CleanRelative(strings.TrimSuffix(analysisPath, path.Ext(analysisPath)))
	if err != nil {
		return nil
	}
	return []string{base + ".2EX", base + ".EXT"}
}

var errAnalysisTooLarge = errors.New("enginedb: analysis row too large")

var analysisWriters = sync.Pool{New: func() any {
	// BestSpeed is a fixed valid level; keep its workspace across columns/rows.
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return w
}}

func packAnalysis(b []byte) ([]byte, error) { return packAnalysisParts(b) }

func packAnalysisParts(parts ...[]byte) ([]byte, error) {
	var buf bytes.Buffer
	w := analysisWriters.Get().(*gzip.Writer)
	w.Reset(&buf)
	defer func() { w.Reset(io.Discard); analysisWriters.Put(w) }()
	for _, part := range parts {
		if _, err := w.Write(part); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func packBridgeSource(id uint32, a *rekordbox.Analysis) ([]byte, error) {
	var header [compactAnalysisHeader]byte
	copy(header[:4], compactAnalysisMagic)
	binary.LittleEndian.PutUint32(header[4:8], id)
	var data []byte
	if w := displayAnalysisWaveform(a); w != nil {
		copy(header[8:12], w.Tag)
		binary.LittleEndian.PutUint32(header[12:16], w.EntryBytes)
		binary.LittleEndian.PutUint32(header[16:20], uint32(len(w.Data)))
		data = w.Data
	}
	return packAnalysisParts(header[:], data)
}

func unpackBridgeSource(b []byte, limit int) ([]byte, error) {
	if len(b) < compactAnalysisHeader {
		return nil, io.ErrUnexpectedEOF
	}
	var source struct {
		Track    struct{ ID uint32 }
		Analysis *rekordbox.Analysis
	}
	source.Track.ID = binary.LittleEndian.Uint32(b[4:8])
	tag, width, n := string(b[8:12]), binary.LittleEndian.Uint32(b[12:16]), binary.LittleEndian.Uint32(b[16:20])
	if n != uint32(len(b)-compactAnalysisHeader) {
		return nil, fmt.Errorf("enginedb: invalid compact analysis length")
	}
	if n == 0 && tag == "\x00\x00\x00\x00" && width == 0 {
		// An imported track may have no valid scrolling detail.
	} else {
		if (tag != "PWV7" || width != 3) && (tag != "PWV5" || width != 2) || n == 0 || n%width != 0 || n/width > 2<<20 {
			return nil, fmt.Errorf("enginedb: invalid compact analysis waveform")
		}
		// UnpackAnalysis's contract bounds the reconstructed JSON as well as the
		// decompressed binary. Reject base64 expansion before allocating it.
		if (int(n)+2)/3*4 > limit {
			return nil, errAnalysisTooLarge
		}
		source.Analysis = &rekordbox.Analysis{Waveforms: []rekordbox.Waveform{{Tag: tag, EntryBytes: width, Data: b[compactAnalysisHeader:]}}}
	}
	out, err := json.Marshal(source)
	if len(out) > limit {
		return nil, errAnalysisTooLarge
	}
	return out, err
}

// Identity recovery reads only the ID prefix, never the waveform payload.
// Old JSON starts with Track.ID; RBA1 puts it immediately after the magic.
func analysisSourceID(b []byte) (uint32, error) {
	var r io.Reader = bytes.NewReader(b)
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return 0, err
		}
		defer zr.Close()
		r = zr
	}
	var prefix [4]byte
	n, _ := io.ReadFull(r, prefix[:])
	if string(prefix[:n]) == compactAnalysisMagic || string(prefix[:n]) == refAnalysisMagic {
		var id uint32
		err := binary.Read(r, binary.LittleEndian, &id)
		return id, err
	}
	d := json.NewDecoder(io.LimitReader(io.MultiReader(bytes.NewReader(prefix[:n]), r), 4096))
	for _, want := range []any{json.Delim('{'), "Track", json.Delim('{'), "ID"} {
		got, err := d.Token()
		if err != nil || got != want {
			return 0, nil
		}
	}
	var id uint32
	if err := d.Decode(&id); err != nil {
		return 0, nil
	}
	return id, nil
}

// UnpackAnalysis returns a BridgeAnalysis column as JSON, accepting compact
// RBA1 records and legacy plain/gzip JSON. Both decompressed bytes and the
// reconstructed JSON must fit limit.
func UnpackAnalysis(b []byte, limit int) ([]byte, error) {
	if limit < 0 {
		return nil, errAnalysisTooLarge
	}
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		b, err = io.ReadAll(io.LimitReader(r, int64(limit)+1))
		if err != nil {
			return nil, err
		}
	}
	if len(b) > limit {
		return nil, errAnalysisTooLarge
	}
	if id, files, err := unpackBridgeRef(b); err == nil {
		// The detail stays in the ANLZ files: BridgeDetail reads it from there.
		var source struct {
			Track  struct{ ID uint32 }
			Detail []string
		}
		source.Track.ID, source.Detail = id, files
		out, err := json.Marshal(source)
		if len(out) > limit {
			return nil, errAnalysisTooLarge
		}
		return out, err
	}
	if bytes.HasPrefix(b, []byte(compactAnalysisMagic)) {
		return unpackBridgeSource(b, limit)
	}
	return b, nil
}
