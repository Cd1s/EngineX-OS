package webui

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

const shadowAnalysisMagic = "RBXA1" // Same five PerformanceData blobs as the rekordbox bind hook.
const shadowAnalysisMax = 8 << 20

func encodeShadowAnalysis(p enginedb.Performance) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(shadowAnalysisMagic)
	for _, blob := range [][]byte{p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops} {
		if len(blob)+4 > shadowAnalysisMax-b.Len() {
			return nil, errors.New("analysis exceeds size limit")
		}
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(blob)))
		b.Write(n[:])
		b.Write(blob)
	}
	return b.Bytes(), nil
}

func validShadowAnalysis(b []byte) bool {
	if len(b) > shadowAnalysisMax || !bytes.HasPrefix(b, []byte(shadowAnalysisMagic)) {
		return false
	}
	b = b[len(shadowAnalysisMagic):]
	var parts [5][]byte
	for i := range parts {
		if len(b) < 4 {
			return false
		}
		n := binary.LittleEndian.Uint32(b)
		b = b[4:]
		if uint64(n) > uint64(len(b)) {
			return false
		}
		parts[i] = b[:n]
		b = b[n:]
	}
	if len(b) != 0 || len(parts[2]) == 0 {
		return false
	}
	// Qt encodes an empty compressed array as four zero bytes. Loops
	// use the stock uncompressed layout regardless of the cue encoding.
	cues := parts[3]
	if bytes.Equal(cues, []byte{0, 0, 0, 0}) {
		cues = nil
	}
	p, err := enginedb.ParseDisplayPerformance(parts[0], parts[1], parts[2], cues, parts[4])
	return err == nil && len(p.Beats) != 0
}

// Publish only a complete file, before shadowFetch publishes the audio path.
// Older peers ignore format=native and return EXPF; it never becomes a sidecar.
// The result says why there is none ("" when there is one), for the log.
func saveShadowAnalysis(ctx context.Context, local, url string) string {
	out := local + ".analysis"
	_ = os.Remove(out)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "no request"
	}
	resp, err := peerFileClient.Do(req)
	if err != nil {
		return "no answer"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "http " + strconv.Itoa(resp.StatusCode)
	}
	if resp.ContentLength > shadowAnalysisMax {
		return "too large"
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, shadowAnalysisMax+1))
	if err != nil {
		return "cut off"
	}
	if !validShadowAnalysis(b) {
		return "not a native analysis"
	}
	tmp, err := os.CreateTemp(filepath.Dir(local), ".analysis-*")
	if err != nil {
		return "not written"
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if closeErr := tmp.Close(); err != nil || closeErr != nil || os.Rename(tmp.Name(), out) != nil {
		return "not written"
	}
	return ""
}
