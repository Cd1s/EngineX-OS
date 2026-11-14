package enginedb

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
)

// BridgeAnalysis keeps each imported track's Rekordbox source (track, cues,
// beat grid, song structure and every waveform section) and its audio header
// as JSON. Plain, that is about 337 KB a track: a 21,352-track stick needed a
// 7.2 GB .rb-bridge-state.db (#272). Rows are written gzip-compressed, which
// keeps far more tracks under defaultMaxBridgeStateBytes, so fewer of them lose
// their shadow-deck detail; rows written by earlier builds are plain JSON and
// are still read.

var errAnalysisTooLarge = errors.New("enginedb: analysis row too large")

func packAnalysis(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	// BestSpeed: the deck compresses inside the mount-time read budget.
	w, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err = w.Write(b); err != nil {
		return nil, err
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnpackAnalysis returns a BridgeAnalysis column as JSON, whether it was
// written compressed or, by an earlier build, plain. A row longer than limit
// bytes once unpacked is an error.
func UnpackAnalysis(b []byte, limit int) ([]byte, error) {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		if len(b) > limit {
			return nil, errAnalysisTooLarge
		}
		return b, nil
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(out) > limit {
		return nil, errAnalysisTooLarge
	}
	return out, nil
}
