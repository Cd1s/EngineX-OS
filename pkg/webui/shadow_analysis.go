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
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

const shadowAnalysisMagic = "RBXA1" // Same five PerformanceData blobs as the rekordbox bind hook.
const shadowAnalysisMax = 8 << 20
const shadowPerfFormatsHeader = "X-EngineX-Perf-Formats"
const shadowPerfGenHeader = "X-EngineX-Perf-Gen"

func peerAdvertisesFormat(header, want string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), want) {
			return true
		}
	}
	return false
}

func encodeShadowAnalysis(p enginedb.Performance) ([]byte, error) {
	// Ensure native shadow blobs carry non-zero overview band maxima and 5.3.0
	// trackData peaks (matching EXPF perfFromBlobs), without altering already
	// complete blobs.
	var rawOv []byte
	if len(p.Overview) > 4 {
		if raw, err := enginedb.QUncompress(p.Overview, shadowAnalysisMax); err == nil {
			rawOv = enginedb.OverviewMaxima(raw)
			if len(rawOv) > 0 && (len(raw) == 0 || &rawOv[0] != &raw[0]) {
				if fixed, err := enginedb.QCompress(rawOv); err == nil {
					p.Overview = fixed
				}
			}
		}
	}
	if len(p.TrackData) > 4 {
		if raw, err := enginedb.QUncompress(p.TrackData, 4096); err == nil {
			fixedTrack := enginedb.TrackDataPeaks(raw, rawOv)
			if !bytes.Equal(fixedTrack, raw) {
				if fixed, err := enginedb.QCompress(fixedTrack); err == nil {
					p.TrackData = fixed
				}
			}
		}
	}
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

// fetchNativeAnalysisPending fetches a validated native RBXA1 payload before a
// player publishes a shadow copy. Transient failures (503, 5xx, timeouts, broken
// connections) retry up to perfRetries and stay pending if exhausted. An older
// EXPF-only peer is accepted only through explicit capability absence (no
// X-EngineX-Perf-Formats header advertising native AND a valid EXPF response).
func fetchNativeAnalysisPending(ctx context.Context, url string) (b []byte, gen string, olderEXPFOnly bool, pending bool, why string) {
	for try := 0; ; try++ {
		reqCtx, cancel := context.WithTimeout(ctx, perfRequestTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			return nil, "", false, false, "no request"
		}
		resp, err := peerFileClient.Do(req)
		if err != nil {
			cancel()
			why = "no answer"
			if try+1 >= perfRetries {
				return nil, "", false, true, why
			}
			select {
			case <-ctx.Done():
				return nil, "", false, true, "no answer"
			case <-time.After(perfRetryWait):
				continue
			}
		}
		formats := resp.Header.Get(shadowPerfFormatsHeader)
		perfGen := strings.TrimSpace(resp.Header.Get(shadowPerfGenHeader))
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode >= 500 {
			code := resp.StatusCode
			resp.Body.Close()
			cancel()
			why = "http " + strconv.Itoa(code)
			if try+1 >= perfRetries {
				return nil, "", false, true, why
			}
			select {
			case <-ctx.Done():
				return nil, "", false, true, "no answer"
			case <-time.After(perfRetryWait):
				continue
			}
		}
		if resp.StatusCode != http.StatusOK {
			code := resp.StatusCode
			resp.Body.Close()
			cancel()
			return nil, "", false, false, "http " + strconv.Itoa(code)
		}
		if resp.ContentLength > shadowAnalysisMax {
			resp.Body.Close()
			cancel()
			return nil, "", false, false, "too large"
		}
		b, err = io.ReadAll(io.LimitReader(resp.Body, shadowAnalysisMax+1))
		resp.Body.Close()
		cancel()
		if err != nil {
			why = "cut off"
			if try+1 >= perfRetries {
				return nil, "", false, true, why
			}
			select {
			case <-ctx.Done():
				return nil, "", false, true, "no answer"
			case <-time.After(perfRetryWait):
				continue
			}
		}
		if validShadowAnalysis(b) {
			return b, perfGen, false, false, ""
		}
		if validPerf(b) && formats == "" {
			return nil, perfGen, true, false, "not a native analysis"
		}
		return nil, "", false, false, "not a native analysis"
	}
}

func writeShadowAnalysis(local string, b []byte) error {
	out := local + ".analysis"
	if !validShadowAnalysis(b) {
		_ = os.Remove(out)
		return errors.New("not a native analysis")
	}
	tmp, err := os.CreateTemp(filepath.Dir(local), ".analysis-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), out)
}

// Publish only a complete file, before shadowFetch publishes the audio path.
// Older peers ignore format=native and return EXPF; it never becomes a sidecar.
// The result says why there is none ("" when there is one), for the log.
func saveShadowAnalysis(ctx context.Context, local, url string) string {
	out := local + ".analysis"
	_ = os.Remove(out)
	b, _, _, _, why := fetchNativeAnalysisPending(ctx, url)
	if why != "" {
		return why
	}
	if err := writeShadowAnalysis(local, b); err != nil {
		return "not written"
	}
	return ""
}
