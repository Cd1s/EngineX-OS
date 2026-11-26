package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

const (
	shadowAnalysisMagic     = "RBXA1" // Same five PerformanceData blobs as the rekordbox bind hook.
	shadowAnalysisMax       = 8 << 20
	shadowPerfMax           = 16 << 20
	rbxAnalysisMax          = 16 << 20
	shadowPerfFormatsHeader = "X-EngineX-Perf-Formats"
	shadowPerfGenHeader     = "X-EngineX-Perf-Gen"
)

// A source still writing a track's analysis answers 503: asked again
// this often, a few times, before Engine loads the copy (only such a track waits).
var (
	perfRetryWait          = 500 * time.Millisecond
	perfRetries            = 10
	perfRequestTimeout     = 5 * time.Second
	shadowPairGenRetries   = 3
	shadowPairGenRetryWait = 25 * time.Millisecond
)

// shadowEncodedPair holds one load's paired native RBXA1 + EXPF v1 payloads and
// precomputed 16-hex generation digest, initialized at most once via sync.Once
// even under concurrent first EXPF and native requests.
type shadowEncodedPair struct {
	once   sync.Once
	native []byte
	expf   []byte
	gen    string
	err    error
}

// shadowAnalysisPair is a validated, generation-matched pair fetched from a peer
// before downloading or linking audio.
type shadowAnalysisPair struct {
	expf   []byte
	native []byte
	gen    string
	noFill string
}

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

// encodePerfPair encodes both the native RBXA1 record and the EXPF v1 payload
// from one set of PerformanceData blobs, uncompressing each blob only once and
// precomputing the 16-hex generation digest once alongside the pair.
func encodePerfPair(bpm float64, p enginedb.Performance) (native, expf []byte, gen string, err error) {
	rawTrack, err := enginedb.QUncompress(p.TrackData, 8<<20)
	if err != nil {
		return nil, nil, "", err
	}
	rawBeats, err := enginedb.QUncompress(p.Beats, 8<<20)
	if err != nil {
		return nil, nil, "", err
	}
	rawOv, err := enginedb.QUncompress(p.Overview, shadowAnalysisMax)
	if err != nil {
		return nil, nil, "", err
	}
	fixedOv := enginedb.OverviewMaxima(rawOv)
	if len(fixedOv) > 0 && (len(rawOv) == 0 || &fixedOv[0] != &rawOv[0]) {
		if comp, cerr := enginedb.QCompress(fixedOv); cerr == nil {
			p.Overview = comp
		}
	}
	fixedTrack := enginedb.TrackDataPeaks(rawTrack, fixedOv)
	if !bytes.Equal(fixedTrack, rawTrack) {
		if comp, cerr := enginedb.QCompress(fixedTrack); cerr == nil {
			p.TrackData = comp
		}
	}
	var nb bytes.Buffer
	nb.WriteString(shadowAnalysisMagic)
	for _, blob := range [][]byte{p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops} {
		if len(blob)+4 > shadowAnalysisMax-nb.Len() {
			return nil, nil, "", errors.New("analysis exceeds size limit")
		}
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(blob)))
		nb.Write(n[:])
		nb.Write(blob)
	}
	if !(bpm > 0 && bpm < 1000) {
		bpm = beatDataBPM(rawBeats)
	}
	if !(bpm > 0 && bpm < 1000) {
		return nil, nil, "", fmt.Errorf("perf: bpm %v", bpm)
	}
	var eb bytes.Buffer
	eb.WriteString("EXPF")
	binary.Write(&eb, binary.LittleEndian, uint32(1))
	binary.Write(&eb, binary.LittleEndian, bpm)
	for _, part := range [][]byte{fixedTrack, rawBeats, fixedOv} {
		binary.Write(&eb, binary.LittleEndian, uint32(len(part)))
		eb.Write(part)
	}
	nat := nb.Bytes()
	sum := sha256.Sum256(nat)
	return nat, eb.Bytes(), fmt.Sprintf("%x", sum)[:16], nil
}

// beatDataBPM derives the track's first-interval BPM from an uncompressed Engine
// beatData blob so EXPF generation succeeds even when a deck posts BPM=0 at load time.
func beatDataBPM(rawBeats []byte) float64 {
	if len(rawBeats) < 33 || rawBeats[16] != 1 {
		return 0
	}
	sr := math.Float64frombits(binary.BigEndian.Uint64(rawBeats[0:8]))
	if math.IsNaN(sr) || math.IsInf(sr, 0) || sr < 8000 || sr > 384000 {
		return 0
	}
	type pair struct {
		ok     bool
		p0, p1 float64
		n0, n1 int64
	}
	var grids [2]pair
	off := 17
	for g := range grids {
		if len(rawBeats)-off < 8 {
			break
		}
		n := binary.BigEndian.Uint64(rawBeats[off : off+8])
		off += 8
		if n > 65536 || n > uint64((len(rawBeats)-off)/24) {
			return 0
		}
		if n >= 2 {
			p0 := math.Float64frombits(binary.LittleEndian.Uint64(rawBeats[off : off+8]))
			n0 := int64(binary.LittleEndian.Uint64(rawBeats[off+8 : off+16]))
			p1 := math.Float64frombits(binary.LittleEndian.Uint64(rawBeats[off+24 : off+32]))
			n1 := int64(binary.LittleEndian.Uint64(rawBeats[off+32 : off+40]))
			if p1 > p0 && n1 > n0 {
				grids[g] = pair{ok: true, p0: p0, p1: p1, n0: n0, n1: n1}
			}
		}
		off += int(n) * 24
	}
	chosen := grids[1]
	if !chosen.ok {
		chosen = grids[0]
	}
	if !chosen.ok {
		return 0
	}
	bpm := 60 * sr * float64(chosen.n1-chosen.n0) / (chosen.p1 - chosen.p0)
	if math.IsNaN(bpm) || math.IsInf(bpm, 0) || !(bpm > 0 && bpm < 1000) {
		return 0
	}
	return bpm
}

// perfFromBlobs turns a track's qCompress'd PerformanceData blobs into EXPF v1,
// what engine_ui_hook.c's OfflineAnalyzer hook replays: "EXPF", u32 1, f64 bpm,
// then raw trackData, beatData and overviewWaveFormData, each u32 length +
// bytes, little-endian. trackData gets 5.3.0's waveform peaks if it lacks them.
func perfFromBlobs(bpm float64, track, beats, overview []byte) ([]byte, error) {
	var raw [3][]byte
	for i, blob := range [][]byte{track, beats, overview} {
		var err error
		if raw[i], err = enginedb.QUncompress(blob, 8<<20); err != nil {
			return nil, err
		}
	}
	if !(bpm > 0 && bpm < 1000) {
		bpm = beatDataBPM(raw[1])
	}
	if !(bpm > 0 && bpm < 1000) {
		return nil, fmt.Errorf("perf: bpm %v", bpm)
	}
	raw[2] = enginedb.OverviewMaxima(raw[2])
	raw[0] = enginedb.TrackDataPeaks(raw[0], raw[2])
	var b bytes.Buffer
	b.WriteString("EXPF")
	binary.Write(&b, binary.LittleEndian, uint32(1))
	binary.Write(&b, binary.LittleEndian, bpm)
	for _, part := range raw {
		binary.Write(&b, binary.LittleEndian, uint32(len(part)))
		b.Write(part)
	}
	return b.Bytes(), nil
}

// withDeckBPM puts the source deck's own OriginalBPM into an EXPF header without
// mutating the cached slice.
func withDeckBPM(perf []byte, bpm float64) []byte {
	if !(bpm > 0 && bpm < 1000) || len(perf) < 16 || string(perf[:4]) != "EXPF" {
		return perf
	}
	if math.Float64frombits(binary.LittleEndian.Uint64(perf[8:16])) == bpm {
		return perf
	}
	out := append([]byte(nil), perf...)
	binary.LittleEndian.PutUint64(out[8:16], math.Float64bits(bpm))
	return out
}

// expfBPM is the BPM an EXPF analysis gives the copy.
func expfBPM(perf []byte) (float64, bool) {
	if len(perf) < 16 || string(perf[:4]) != "EXPF" {
		return 0, false
	}
	bpm := math.Float64frombits(binary.LittleEndian.Uint64(perf[8:16]))
	return bpm, bpm > 0 && bpm < 1000
}

// validPerf reports whether b is a well-formed EXPF v1 payload accepted by
// engine_ui_hook.c's oa_read_perf AND whose uncompressed trackData, overview,
// and beatData parse cleanly into a supported non-empty Engine beat grid.
func validPerf(b []byte) bool {
	if len(b) < 28 || len(b) > shadowPerfMax || string(b[:4]) != "EXPF" {
		return false
	}
	if binary.LittleEndian.Uint32(b[4:8]) != 1 {
		return false
	}
	bpm := math.Float64frombits(binary.LittleEndian.Uint64(b[8:16]))
	if math.IsNaN(bpm) || math.IsInf(bpm, 0) || !(bpm > 0 && bpm < 1000) {
		return false
	}
	off := 16
	var parts [3][]byte
	for i := range parts {
		if len(b)-off < 4 {
			return false
		}
		n := binary.LittleEndian.Uint32(b[off : off+4])
		off += 4
		if uint64(n) > uint64(len(b)-off) {
			return false
		}
		parts[i] = b[off : off+int(n)]
		off += int(n)
	}
	if off != len(b) || len(parts[0]) < 20 || len(parts[1]) == 0 || len(parts[2]) < 27 {
		return false
	}
	ct, err := enginedb.QCompress(parts[0])
	if err != nil {
		return false
	}
	cb, err := enginedb.QCompress(parts[1])
	if err != nil {
		return false
	}
	co, err := enginedb.QCompress(parts[2])
	if err != nil {
		return false
	}
	dp, err := enginedb.ParseDisplayPerformance(ct, co, cb, nil, nil)
	return err == nil && len(dp.Beats) != 0
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

// rbxPeerPerfPair decodes a rekordbox Link analysis file once into both native
// RBXA1 and EXPF v1 payloads plus its precomputed generation hash.
func rbxPeerPerfPair(file string, bpm float64) (native, expf []byte, gen string, err error) {
	b, err := rbxReadAnalysisFile(file)
	if err != nil {
		return nil, nil, "", err
	}
	p := rbxCachedPerformance(b)
	if !p.Complete {
		return nil, nil, "", errors.New("rekordbox analysis incomplete")
	}
	return encodePerfPair(bpm, p)
}

// rbxReadAnalysisFile reads a regular file directly in rbxAnalysisDir through an
// os.Root (no link, nor a parent's, leads out of it), at most rbxAnalysisMax bytes.
func rbxReadAnalysisFile(file string) ([]byte, error) {
	if filepath.Dir(file) != filepath.Clean(rbxAnalysisDir) {
		return nil, errors.New("rekordbox analysis outside its directory")
	}
	root, err := os.OpenRoot(rbxAnalysisDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := filepath.Base(file)
	if st, err := root.Lstat(name); err != nil {
		return nil, err
	} else if !st.Mode().IsRegular() || st.Size() > rbxAnalysisMax {
		return nil, errors.New("rekordbox analysis is not a bounded regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > rbxAnalysisMax {
		return nil, errors.New("rekordbox analysis is not a bounded regular file")
	}
	return io.ReadAll(io.LimitReader(f, rbxAnalysisMax+1))
}

// savePerf puts the source's validated analysis next to a copy atomically, or
// removes a stale one when perf is nil. Callers must fail publication on error.
func savePerf(local string, perf []byte) error {
	if perf == nil {
		_ = os.Remove(local + ".perf")
		return nil
	}
	if !validPerf(perf) {
		_ = os.Remove(local + ".perf")
		return errors.New("invalid EXPF analysis")
	}
	tmp := local + ".perf.tmp"
	if err := os.WriteFile(tmp, perf, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, local+".perf"); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// saveShadowRGB puts the source's RGB overview next to a copy as <copy>.perf.rgb, which
// engine_ui_hook.c replays as the copy's RGB overview, or removes a stale one.
func saveShadowRGB(ctx context.Context, local, perfURL string) {
	out := local + ".perf.rgb"
	_ = os.Remove(out)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, perfURL+"&format=rgb", nil)
	if err != nil {
		return
	}
	resp, err := peerFileClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode != http.StatusOK || !enginedb.ValidRGBOverview(b) {
		return
	}
	if os.WriteFile(out+".tmp", b, 0o644) != nil || os.Rename(out+".tmp", out) != nil {
		os.Remove(out + ".tmp")
	}
}

func fetchPerf(ctx context.Context, url string) []byte {
	b, _ := fetchPerfPending(ctx, url)
	return b
}

func fetchPerfPending(ctx context.Context, url string) (perf []byte, pending bool) {
	b, _, p := fetchPerfPendingGen(ctx, url)
	return b, p
}

func fetchPerfPendingGen(ctx context.Context, url string) (perf []byte, gen string, pending bool) {
	for try := 0; ; try++ {
		if ctx.Err() != nil {
			return nil, "", true
		}
		b, hdrGen, status, ok := fetchPerfOnce(ctx, url)
		switch {
		case ok && validPerf(b):
			return b, hdrGen, false
		case ok:
			log.Printf("stream-sync: shadow: the source gave invalid analysis (%d bytes)", len(b))
			return nil, "", false
		case status == 0 || status == http.StatusServiceUnavailable || status >= 500:
			pending = true
		default:
			log.Printf("stream-sync: shadow: the source gave no analysis (http %d)", status)
			return nil, "", false
		}
		if try+1 >= perfRetries || ctx.Err() != nil {
			return nil, "", true
		}
		select {
		case <-ctx.Done():
			return nil, "", true
		case <-time.After(perfRetryWait):
		}
	}
}

// fetchPerfOnce: ok only for a complete 200 answer.
func fetchPerfOnce(ctx context.Context, url string) ([]byte, string, int, bool) {
	ctx, cancel := context.WithTimeout(ctx, perfRequestTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := peerFileClient.Do(req)
	if err != nil {
		return nil, "", 0, false
	}
	defer resp.Body.Close()
	hdrGen := strings.TrimSpace(resp.Header.Get(shadowPerfGenHeader))
	b, err := io.ReadAll(io.LimitReader(resp.Body, shadowPerfMax+1))
	if err != nil {
		return nil, "", 0, false
	}
	if resp.StatusCode != 200 {
		return nil, "", resp.StatusCode, false
	}
	return b, hdrGen, resp.StatusCode, true
}

// fetchNativeAnalysisPending fetches a validated native RBXA1 payload before a
// player publishes a shadow copy. Transient failures (503, 5xx, timeouts, broken
// connections) retry up to perfRetries and stay pending if exhausted. An older
// EXPF-only peer is accepted only through explicit capability absence (no
// X-EngineX-Perf-Formats header advertising native AND a valid EXPF response).
func fetchNativeAnalysisPending(ctx context.Context, url string) (b []byte, gen string, olderEXPFOnly bool, pending bool, why string) {
	for try := 0; ; try++ {
		if ctx.Err() != nil {
			return nil, "", false, true, "no answer"
		}
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
			if try+1 >= perfRetries || ctx.Err() != nil {
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
			if try+1 >= perfRetries || ctx.Err() != nil {
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
			if try+1 >= perfRetries || ctx.Err() != nil {
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

// fetchShadowAnalysisPair fetches and validates EXPF + (on player boards) native
// analysis before publishing or refreshing a shadow copy, requiring matching
// generation headers whenever native analysis is returned so mixed generations
// are never installed together. Both format fetches share ctx's deadline.
func fetchShadowAnalysisPair(ctx context.Context, perfURL string) (shadowAnalysisPair, error) {
	for attempt := 0; attempt < shadowPairGenRetries; attempt++ {
		if ctx.Err() != nil {
			return shadowAnalysisPair{}, errPerfPending
		}
		expf, expfGen, pending := fetchPerfPendingGen(ctx, perfURL)
		if pending {
			return shadowAnalysisPair{}, errPerfPending
		}
		if expf == nil {
			return shadowAnalysisPair{}, errAnalysisUnavailable
		}
		pair := shadowAnalysisPair{expf: expf, gen: expfGen, noFill: "not a player"}
		if !shadowIsPlayerBoard() {
			return pair, nil
		}
		native, natGen, olderEXPFOnly, nativePending, noFill := fetchNativeAnalysisPending(ctx, perfURL+"&format=native")
		if nativePending {
			return shadowAnalysisPair{}, errPerfPending
		}
		if native == nil && !olderEXPFOnly {
			return shadowAnalysisPair{noFill: noFill}, errAnalysisUnavailable
		}
		if native != nil && expfGen != natGen {
			// Source switched analysis generation between the EXPF and native requests;
			// retry the pair instead of publishing mismatched sidecars.
			if attempt+1 >= shadowPairGenRetries {
				return shadowAnalysisPair{}, errPerfPending
			}
			select {
			case <-ctx.Done():
				return shadowAnalysisPair{}, errPerfPending
			case <-time.After(shadowPairGenRetryWait):
				continue
			}
		}
		pair.native = native
		pair.noFill = noFill
		if natGen != "" {
			pair.gen = natGen
		}
		return pair, nil
	}
	return shadowAnalysisPair{}, errPerfPending
}

func removeShadowSidecars(local string) {
	_ = os.Remove(local + ".perf")
	_ = os.Remove(local + ".perf.rgb")
	_ = os.Remove(local + ".analysis")
}

// installShadowSidecars writes validated .perf, optional .perf.rgb, and .analysis
// atomically before exposing local audio, cleaning up on any failure.
func installShadowSidecars(ctx context.Context, local, perfURL string, pair shadowAnalysisPair) error {
	if err := savePerf(local, pair.expf); err != nil {
		return fmt.Errorf("save shadow perf: %w", err)
	}
	saveShadowRGB(ctx, local, perfURL)
	if pair.native != nil {
		if err := writeShadowAnalysis(local, pair.native); err != nil {
			removeShadowSidecars(local)
			return fmt.Errorf("save shadow analysis: %w", err)
		}
	} else {
		_ = os.Remove(local + ".analysis")
	}
	return nil
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
