package enginedb

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// BridgeAnalysis stores only the source ID and the PWV7 (else PWV5) section
// consumed by shadow display. RBA1 is a gzip-wrapped binary record: magic,
// little-endian track ID, four-byte waveform tag, entry width, data length,
// then raw waveform bytes. Other display fields already live in m.db.
// Earlier plain and gzip-JSON records remain readable through UnpackAnalysis.
const compactAnalysisMagic = "RBA1"
const compactAnalysisHeader = 20

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
	if string(prefix[:n]) == compactAnalysisMagic {
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
	if bytes.HasPrefix(b, []byte(compactAnalysisMagic)) {
		return unpackBridgeSource(b, limit)
	}
	return b, nil
}
