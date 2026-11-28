package analyzer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/audioheader"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/hajimehoshi/go-mp3"
	"github.com/jfreymuth/oggvorbis"
	"github.com/mewkiz/flac"
)

const (
	// OverviewSidecarMagic identifies atomic overview peak sidecar files written
	// by stream-sync for OfflineAnalyzer and Engine.
	OverviewSidecarMagic = "EXOV1"
	// OverviewRawSize is the exact uncompressed size of Engine's 1024-bin overviewWaveFormData:
	// >Q(1024) >Q(1024) >d(samplesPerBin) + 1024*3 band bytes + 3 trailer bytes.
	OverviewRawSize          = 24 + OverviewEntries*3 + 3 // 3099
	overviewSidecarHeaderLen = 17
	overviewMaxZLen          = 16384
	overviewFingerprintEdge  = 64 << 10
	overviewFineBuckets      = 8192
	overviewInitialBucketLen = 16

	overviewLowCutHz  = 250.0
	overviewHighCutHz = 2500.0
)

// ErrUnsupportedOverviewFormat reports a valid audio file whose container or
// codec is intentionally left on stock Engine analysis (such as .mp4/AAC).
var ErrUnsupportedOverviewFormat = errors.New("analyzer: overview peaks: unsupported format")

// OverviewPeaks holds the uncompressed 3099-byte and qCompressed overviewWaveFormData
// computed from a track's audio using per-bin peak detection across 3 Butterworth bands.
type OverviewPeaks struct {
	Raw        [OverviewRawSize]byte
	Compressed []byte
	SampleRate int
	Channels   int
	Samples    int64
	Format     string
	Trailer    [3]byte
}

// OverviewFileFingerprint hashes the exact file size plus the first and last
// 64 KiB of path so stream-sync and OfflineAnalyzer match the same downloaded
// audio content independently of Engine's internal 40-hex download filename hash.
func OverviewFileFingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("analyzer: overview fingerprint requires a regular file")
	}
	size := uint64(st.Size())
	h := sha256.New()
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], size)
	h.Write(lenBuf[:])

	var buf [overviewFingerprintEdge]byte
	first := int64(size)
	if first > overviewFingerprintEdge {
		first = overviewFingerprintEdge
	}
	if first > 0 {
		if _, err := io.ReadFull(f, buf[:first]); err != nil {
			return "", err
		}
		h.Write(buf[:first])
	}
	if size > overviewFingerprintEdge {
		if _, err := f.Seek(int64(size)-overviewFingerprintEdge, io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.ReadFull(f, buf[:]); err != nil {
			return "", err
		}
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// EncodeOverviewSidecar packs raw (3099 bytes) and compressed (Qt qCompress)
// into a checksummed EXOV1 sidecar payload.
func EncodeOverviewSidecar(raw, compressed []byte) ([]byte, error) {
	if err := validateOverviewRaw(raw); err != nil {
		return nil, err
	}
	if len(compressed) <= 4 || len(compressed) > overviewMaxZLen {
		return nil, errors.New("analyzer: invalid compressed overview length")
	}
	out := make([]byte, overviewSidecarHeaderLen+len(raw)+len(compressed))
	copy(out[0:5], OverviewSidecarMagic)
	binary.LittleEndian.PutUint32(out[5:9], uint32(len(raw)))
	binary.LittleEndian.PutUint32(out[9:13], uint32(len(compressed)))
	copy(out[17:], raw)
	copy(out[17+len(raw):], compressed)
	crc := crc32.ChecksumIEEE(out[17:])
	binary.LittleEndian.PutUint32(out[13:17], crc)
	return out, nil
}

// ParseOverviewSidecar validates EXOV1 framing, CRC-32, raw header, band bounds,
// and trailer maxima, returning slices pointing into data.
func ParseOverviewSidecar(data []byte) (raw, compressed []byte, err error) {
	if len(data) < overviewSidecarHeaderLen+OverviewRawSize+5 || string(data[0:5]) != OverviewSidecarMagic {
		return nil, nil, errors.New("analyzer: invalid overview sidecar header")
	}
	rawLen := binary.LittleEndian.Uint32(data[5:9])
	zLen := binary.LittleEndian.Uint32(data[9:13])
	wantCRC := binary.LittleEndian.Uint32(data[13:17])
	if rawLen != OverviewRawSize || zLen <= 4 || zLen > overviewMaxZLen {
		return nil, nil, errors.New("analyzer: invalid overview sidecar lengths")
	}
	if uint64(overviewSidecarHeaderLen)+uint64(rawLen)+uint64(zLen) != uint64(len(data)) {
		return nil, nil, errors.New("analyzer: truncated overview sidecar")
	}
	payload := data[overviewSidecarHeaderLen:]
	if crc32.ChecksumIEEE(payload) != wantCRC {
		return nil, nil, errors.New("analyzer: overview sidecar checksum mismatch")
	}
	raw = payload[:rawLen]
	compressed = payload[rawLen:]
	if err := validateOverviewRaw(raw); err != nil {
		return nil, nil, err
	}
	if _, err := enginedb.QUncompress(compressed, OverviewRawSize+1); err != nil {
		return nil, nil, fmt.Errorf("analyzer: invalid compressed overview: %w", err)
	}
	return raw, compressed, nil
}

func validateOverviewRaw(raw []byte) error {
	if len(raw) != OverviewRawSize {
		return errors.New("analyzer: raw overview must be 3099 bytes")
	}
	length := binary.BigEndian.Uint64(raw[0:8])
	bins := binary.BigEndian.Uint64(raw[8:16])
	step := math.Float64frombits(binary.BigEndian.Uint64(raw[16:24]))
	if length != OverviewEntries || bins != OverviewEntries || math.IsNaN(step) || math.IsInf(step, 0) || step <= 0 {
		return errors.New("analyzer: invalid raw overview envelope")
	}
	var maxL, maxM, maxH byte
	for i := 0; i < OverviewEntries; i++ {
		off := 24 + i*3
		l, m, h := raw[off], raw[off+1], raw[off+2]
		if l > 127 || m > 127 || h > 127 {
			return errors.New("analyzer: raw overview peak exceeds 127")
		}
		if l > maxL {
			maxL = l
		}
		if m > maxM {
			maxM = m
		}
		if h > maxH {
			maxH = h
		}
	}
	trl := raw[OverviewRawSize-3:]
	if trl[0] != maxL || trl[1] != maxM || trl[2] != maxH {
		return errors.New("analyzer: raw overview trailer does not match band maxima")
	}
	return nil
}

// WriteOverviewSidecar atomically writes <dir>/<fp>.ov, updates <dir>/.ready,
// and bounds <dir> to at most maxEntries .ov files (oldest ModTime evicted first).
func WriteOverviewSidecar(dir, fp string, p *OverviewPeaks, maxEntries int) (string, error) {
	if p == nil || len(fp) != 64 || !isHexLower(fp) {
		return "", errors.New("analyzer: invalid overview sidecar arguments")
	}
	data, err := EncodeOverviewSidecar(p.Raw[:], p.Compressed)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, fp+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil {
		os.Remove(tmpPath)
		return "", werr
	}
	if cerr != nil {
		os.Remove(tmpPath)
		return "", cerr
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	dest := filepath.Join(dir, fp+".ov")
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	mark := filepath.Join(dir, ".ready")
	now := time.Now()
	if os.Chtimes(mark, now, now) != nil {
		_ = os.WriteFile(mark, nil, 0o644)
	}
	if maxEntries > 0 {
		pruneOverviewSidecars(dir, dest, maxEntries)
	}
	return dest, nil
}

func isHexLower(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func pruneOverviewSidecars(dir, keepPath string, maxEntries int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		path string
		mt   time.Time
	}
	var files []item
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".ov") {
			continue
		}
		full := filepath.Join(dir, name)
		if full == keepPath {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, item{path: full, mt: info.ModTime()})
	}
	limit := maxEntries - 1
	if limit < 0 {
		limit = 0
	}
	if len(files) <= limit {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mt.Before(files[j].mt) })
	for i := 0; i < len(files)-limit; i++ {
		_ = os.Remove(files[i].path)
	}
}

type biquad32 struct {
	b0, b1, b2, a1, a2 float32
	z1, z2             float32
}

func makeButterworthLP2(fc, fs float64) biquad32 {
	k := math.Tan(math.Pi * fc / fs)
	k2 := k * k
	n := 1.0 / (1.0 + math.Sqrt2*k + k2)
	return biquad32{
		b0: float32(k2 * n),
		b1: float32(2.0 * k2 * n),
		b2: float32(k2 * n),
		a1: float32(2.0 * (k2 - 1.0) * n),
		a2: float32((1.0 - math.Sqrt2*k + k2) * n),
	}
}

func makeButterworthHP2(fc, fs float64) biquad32 {
	k := math.Tan(math.Pi * fc / fs)
	k2 := k * k
	n := 1.0 / (1.0 + math.Sqrt2*k + k2)
	return biquad32{
		b0: float32(n),
		b1: float32(-2.0 * n),
		b2: float32(n),
		a1: float32(2.0 * (k2 - 1.0) * n),
		a2: float32((1.0 - math.Sqrt2*k + k2) * n),
	}
}

func makeButterworthBP2(f1, f2, fs float64) (biquad32, biquad32) {
	w1 := 2.0 * math.Tan(math.Pi*f1/fs)
	w2 := 2.0 * math.Tan(math.Pi*f2/fs)
	bw := w2 - w1
	w02 := w1 * w2
	p1 := complex(-math.Sqrt2/2, math.Sqrt2/2)
	bp := complex(bw, 0) * p1
	disc := bp*bp - complex(4*w02, 0)
	r := math.Hypot(real(disc), imag(disc))
	th := math.Atan2(imag(disc), real(disc)) / 2
	sq := complex(math.Sqrt(r)*math.Cos(th), math.Sqrt(r)*math.Sin(th))
	sRoots := [2]complex128{(bp + sq) / 2, (bp - sq) / 2}
	var secs [2]biquad32
	for i := 0; i < 2; i++ {
		den := complex(2, 0) - sRoots[i]
		zk := (complex(2, 0) + sRoots[i]) / den
		g := (2.0 * bw) / (real(den)*real(den) + imag(den)*imag(den))
		secs[i] = biquad32{
			b0: float32(g),
			b2: float32(-g),
			a1: float32(-2.0 * real(zk)),
			a2: float32(real(zk)*real(zk) + imag(zk)*imag(zk)),
		}
	}
	return secs[0], secs[1]
}

type peakFilterBank struct {
	sr, ch, bps int
	total       int64
	scale       float32

	lpL, lpR   biquad32
	bp1L, bp2L biquad32
	bp1R, bp2R biquad32
	hpL, hpR   biquad32

	// Direct 1024-bin path when total > 0.
	pkL, pkM, pkH [OverviewEntries]float32
	sampleIdx     int64
	bin           int
	nextBinStart  int64

	// Streaming O(1)-memory adaptive bucket path when total <= 0 (MP3 / Ogg / streamed WAV).
	fineL, fineM, fineH [overviewFineBuckets]float32
	bucketWidth         int64
	bucketIdx           int
	nextBucketStart     int64
}

func newPeakFilterBank(sr, ch, bps int, total int64) (*peakFilterBank, error) {
	if sr < 8000 || sr > 384000 || ch < 1 || ch > 8 {
		return nil, fmt.Errorf("analyzer: overview peaks: invalid format sr=%d ch=%d", sr, ch)
	}
	highCut := overviewHighCutHz
	if maxHz := 0.45 * float64(sr); highCut > maxHz {
		highCut = maxHz
	}
	bp1, bp2 := makeButterworthBP2(overviewLowCutHz, highCut, float64(sr))
	var scale float32
	if bps > 0 {
		scale = float32(1.0 / float64(uint64(1)<<uint(bps-1)))
	}
	b := &peakFilterBank{
		sr:    sr,
		ch:    ch,
		bps:   bps,
		total: total,
		scale: scale,
		lpL:   makeButterworthLP2(overviewLowCutHz, float64(sr)),
		lpR:   makeButterworthLP2(overviewLowCutHz, float64(sr)),
		bp1L:  bp1,
		bp2L:  bp2,
		bp1R:  bp1,
		bp2R:  bp2,
		hpL:   makeButterworthHP2(highCut, float64(sr)),
		hpR:   makeButterworthHP2(highCut, float64(sr)),
	}
	if total > 0 {
		b.nextBinStart = (total + OverviewEntries - 1) / OverviewEntries
		if b.nextBinStart < 1 {
			b.nextBinStart = 1
		}
	} else {
		b.bucketWidth = overviewInitialBucketLen
		b.nextBucketStart = b.bucketWidth
	}
	return b, nil
}

// feedInt32 runs the 3-band biquad filter bank and updates either fixed 1024 bins
// (when total > 0) or adaptive fine buckets (when total <= 0).
func (b *peakFilterBank) feedInt32(s0, s1 []int32) {
	bs := len(s0)
	if bs == 0 {
		return
	}
	if b.total <= 0 {
		sc := b.scale
		for i := 0; i < bs; i++ {
			xL := float32(s0[i]) * sc
			var xR float32
			if s1 != nil {
				xR = float32(s1[i]) * sc
			}
			b.pushFloatSample(xL, xR, s1 != nil)
		}
		return
	}

	lb0, lb1, lb2, la1, la2 := b.lpL.b0, b.lpL.b1, b.lpL.b2, b.lpL.a1, b.lpL.a2
	lz1L, lz2L, lz1R, lz2R := b.lpL.z1, b.lpL.z2, b.lpR.z1, b.lpR.z2
	m1b0, m1b2, m1a1, m1a2 := b.bp1L.b0, b.bp1L.b2, b.bp1L.a1, b.bp1L.a2
	m1z1L, m1z2L, m1z1R, m1z2R := b.bp1L.z1, b.bp1L.z2, b.bp1R.z1, b.bp1R.z2
	m2b0, m2b2, m2a1, m2a2 := b.bp2L.b0, b.bp2L.b2, b.bp2L.a1, b.bp2L.a2
	m2z1L, m2z2L, m2z1R, m2z2R := b.bp2L.z1, b.bp2L.z2, b.bp2R.z1, b.bp2R.z2
	hb0, hb1, hb2, ha1, ha2 := b.hpL.b0, b.hpL.b1, b.hpL.b2, b.hpL.a1, b.hpL.a2
	hz1L, hz2L, hz1R, hz2R := b.hpL.z1, b.hpL.z2, b.hpR.z1, b.hpR.z2
	scale := b.scale
	stereo := s1 != nil

	i := 0
	for i < bs {
		rem64 := b.nextBinStart - b.sampleIdx
		if rem64 <= 0 {
			if b.bin < OverviewEntries-1 {
				b.bin++
			}
			if b.bin == OverviewEntries-1 {
				b.nextBinStart = 1 << 62
			} else {
				b.nextBinStart = (int64(b.bin+1)*b.total + OverviewEntries - 1) / OverviewEntries
			}
			continue
		}
		startI := i
		endI := bs
		if rem64 < int64(bs-i) {
			endI = i + int(rem64)
		}
		bin := b.bin
		pL, pM, pH := b.pkL[bin], b.pkM[bin], b.pkH[bin]
		if stereo {
			for ; i < endI; i++ {
				xL := float32(s0[i]) * scale
				xR := float32(s1[i]) * scale

				y := lb0*xL + lz1L
				lz1L = lb1*xL - la1*y + lz2L
				lz2L = lb2*xL - la2*y
				if y < 0 {
					y = -y
				}
				if y > pL {
					pL = y
				}
				y = lb0*xR + lz1R
				lz1R = lb1*xR - la1*y + lz2R
				lz2R = lb2*xR - la2*y
				if y < 0 {
					y = -y
				}
				if y > pL {
					pL = y
				}

				y1 := m1b0*xL + m1z1L
				m1z1L = -m1a1*y1 + m1z2L
				m1z2L = m1b2*xL - m1a2*y1
				y2 := m2b0*y1 + m2z1L
				m2z1L = -m2a1*y2 + m2z2L
				m2z2L = m2b2*y1 - m2a2*y2
				if y2 < 0 {
					y2 = -y2
				}
				if y2 > pM {
					pM = y2
				}
				y1 = m1b0*xR + m1z1R
				m1z1R = -m1a1*y1 + m1z2R
				m1z2R = m1b2*xR - m1a2*y1
				y2 = m2b0*y1 + m2z1R
				m2z1R = -m2a1*y2 + m2z2R
				m2z2R = m2b2*y1 - m2a2*y2
				if y2 < 0 {
					y2 = -y2
				}
				if y2 > pM {
					pM = y2
				}

				yh := hb0*xL + hz1L
				hz1L = hb1*xL - ha1*yh + hz2L
				hz2L = hb2*xL - ha2*yh
				if yh < 0 {
					yh = -yh
				}
				if yh > pH {
					pH = yh
				}
				yh = hb0*xR + hz1R
				hz1R = hb1*xR - ha1*yh + hz2R
				hz2R = hb2*xR - ha2*yh
				if yh < 0 {
					yh = -yh
				}
				if yh > pH {
					pH = yh
				}
			}
		} else {
			for ; i < endI; i++ {
				xL := float32(s0[i]) * scale
				y := lb0*xL + lz1L
				lz1L = lb1*xL - la1*y + lz2L
				lz2L = lb2*xL - la2*y
				if y < 0 {
					y = -y
				}
				if y > pL {
					pL = y
				}

				y1 := m1b0*xL + m1z1L
				m1z1L = -m1a1*y1 + m1z2L
				m1z2L = m1b2*xL - m1a2*y1
				y2 := m2b0*y1 + m2z1L
				m2z1L = -m2a1*y2 + m2z2L
				m2z2L = m2b2*y1 - m2a2*y2
				if y2 < 0 {
					y2 = -y2
				}
				if y2 > pM {
					pM = y2
				}

				yh := hb0*xL + hz1L
				hz1L = hb1*xL - ha1*yh + hz2L
				hz2L = hb2*xL - ha2*yh
				if yh < 0 {
					yh = -yh
				}
				if yh > pH {
					pH = yh
				}
			}
		}
		b.pkL[bin], b.pkM[bin], b.pkH[bin] = pL, pM, pH
		b.sampleIdx += int64(endI - startI)
	}

	b.lpL.z1, b.lpL.z2, b.lpR.z1, b.lpR.z2 = lz1L, lz2L, lz1R, lz2R
	b.bp1L.z1, b.bp1L.z2, b.bp1R.z1, b.bp1R.z2 = m1z1L, m1z2L, m1z1R, m1z2R
	b.bp2L.z1, b.bp2L.z2, b.bp2R.z1, b.bp2R.z2 = m2z1L, m2z2L, m2z1R, m2z2R
	b.hpL.z1, b.hpL.z2, b.hpR.z1, b.hpR.z2 = hz1L, hz2L, hz1R, hz2R
}

func (b *peakFilterBank) pushFloatSample(xL, xR float32, stereo bool) {
	y := b.lpL.b0*xL + b.lpL.z1
	b.lpL.z1 = b.lpL.b1*xL - b.lpL.a1*y + b.lpL.z2
	b.lpL.z2 = b.lpL.b2*xL - b.lpL.a2*y
	aL := y
	if aL < 0 {
		aL = -aL
	}

	y1 := b.bp1L.b0*xL + b.bp1L.z1
	b.bp1L.z1 = -b.bp1L.a1*y1 + b.bp1L.z2
	b.bp1L.z2 = b.bp1L.b2*xL - b.bp1L.a2*y1
	y2 := b.bp2L.b0*y1 + b.bp2L.z1
	b.bp2L.z1 = -b.bp2L.a1*y2 + b.bp2L.z2
	b.bp2L.z2 = b.bp2L.b2*y1 - b.bp2L.a2*y2
	aM := y2
	if aM < 0 {
		aM = -aM
	}

	yh := b.hpL.b0*xL + b.hpL.z1
	b.hpL.z1 = b.hpL.b1*xL - b.hpL.a1*yh + b.hpL.z2
	b.hpL.z2 = b.hpL.b2*xL - b.hpL.a2*yh
	aH := yh
	if aH < 0 {
		aH = -aH
	}

	if stereo {
		y = b.lpR.b0*xR + b.lpR.z1
		b.lpR.z1 = b.lpR.b1*xR - b.lpR.a1*y + b.lpR.z2
		b.lpR.z2 = b.lpR.b2*xR - b.lpR.a2*y
		if y < 0 {
			y = -y
		}
		if y > aL {
			aL = y
		}

		y1 = b.bp1R.b0*xR + b.bp1R.z1
		b.bp1R.z1 = -b.bp1R.a1*y1 + b.bp1R.z2
		b.bp1R.z2 = b.bp1R.b2*xR - b.bp1R.a2*y1
		y2 = b.bp2R.b0*y1 + b.bp2R.z1
		b.bp2R.z1 = -b.bp2R.a1*y2 + b.bp2R.z2
		b.bp2R.z2 = b.bp2R.b2*y1 - b.bp2R.a2*y2
		if y2 < 0 {
			y2 = -y2
		}
		if y2 > aM {
			aM = y2
		}

		yh = b.hpR.b0*xR + b.hpR.z1
		b.hpR.z1 = b.hpR.b1*xR - b.hpR.a1*yh + b.hpR.z2
		b.hpR.z2 = b.hpR.b2*xR - b.hpR.a2*yh
		if yh < 0 {
			yh = -yh
		}
		if yh > aH {
			aH = yh
		}
	}

	if b.total > 0 {
		for b.sampleIdx >= b.nextBinStart {
			if b.bin < OverviewEntries-1 {
				b.bin++
			}
			if b.bin == OverviewEntries-1 {
				b.nextBinStart = 1 << 62
			} else {
				b.nextBinStart = (int64(b.bin+1)*b.total + OverviewEntries - 1) / OverviewEntries
			}
		}
		if aL > b.pkL[b.bin] {
			b.pkL[b.bin] = aL
		}
		if aM > b.pkM[b.bin] {
			b.pkM[b.bin] = aM
		}
		if aH > b.pkH[b.bin] {
			b.pkH[b.bin] = aH
		}
		b.sampleIdx++
		return
	}

	for b.sampleIdx >= b.nextBucketStart {
		b.bucketIdx++
		if b.bucketIdx >= overviewFineBuckets {
			half := overviewFineBuckets / 2
			for i := 0; i < half; i++ {
				j := i * 2
				b.fineL[i] = max(b.fineL[j], b.fineL[j+1])
				b.fineM[i] = max(b.fineM[j], b.fineM[j+1])
				b.fineH[i] = max(b.fineH[j], b.fineH[j+1])
			}
			for i := half; i < overviewFineBuckets; i++ {
				b.fineL[i], b.fineM[i], b.fineH[i] = 0, 0, 0
			}
			b.bucketWidth *= 2
			b.bucketIdx = half
		}
		b.nextBucketStart += b.bucketWidth
	}
	if aL > b.fineL[b.bucketIdx] {
		b.fineL[b.bucketIdx] = aL
	}
	if aM > b.fineM[b.bucketIdx] {
		b.fineM[b.bucketIdx] = aM
	}
	if aH > b.fineH[b.bucketIdx] {
		b.fineH[b.bucketIdx] = aH
	}
	b.sampleIdx++
}

func (b *peakFilterBank) finish(format string) (*OverviewPeaks, error) {
	if b.sampleIdx <= 0 {
		return nil, errors.New("analyzer: overview peaks: no audio samples")
	}
	if b.total <= 0 {
		// Resample fine buckets onto 1024 overview bins using exact sample midpoints.
		total := b.sampleIdx
		nBuckets := int((total + b.bucketWidth - 1) / b.bucketWidth)
		if nBuckets > overviewFineBuckets {
			nBuckets = overviewFineBuckets
		}
		for k := 0; k < nBuckets; k++ {
			midSample := int64(k)*b.bucketWidth + b.bucketWidth/2
			if midSample >= total {
				midSample = total - 1
			}
			bin := int((midSample * OverviewEntries) / total)
			if bin >= OverviewEntries {
				bin = OverviewEntries - 1
			}
			if b.fineL[k] > b.pkL[bin] {
				b.pkL[bin] = b.fineL[k]
			}
			if b.fineM[k] > b.pkM[bin] {
				b.pkM[bin] = b.fineM[k]
			}
			if b.fineH[k] > b.pkH[bin] {
				b.pkH[bin] = b.fineH[k]
			}
		}
	}

	out := &OverviewPeaks{
		SampleRate: b.sr,
		Channels:   b.ch,
		Samples:    b.sampleIdx,
		Format:     format,
	}
	binary.BigEndian.PutUint64(out.Raw[0:8], OverviewEntries)
	binary.BigEndian.PutUint64(out.Raw[8:16], OverviewEntries)
	step := float64(b.sampleIdx) / float64(OverviewEntries)
	binary.BigEndian.PutUint64(out.Raw[16:24], math.Float64bits(step))

	var maxL, maxM, maxH byte
	for i := 0; i < OverviewEntries; i++ {
		l := clampPeakByte(b.pkL[i])
		m := clampPeakByte(b.pkM[i])
		h := clampPeakByte(b.pkH[i])
		off := 24 + i*3
		out.Raw[off] = l
		out.Raw[off+1] = m
		out.Raw[off+2] = h
		if l > maxL {
			maxL = l
		}
		if m > maxM {
			maxM = m
		}
		if h > maxH {
			maxH = h
		}
	}
	out.Trailer = [3]byte{maxL, maxM, maxH}
	copy(out.Raw[OverviewRawSize-3:], out.Trailer[:])
	z, err := enginedb.QCompress(out.Raw[:])
	if err != nil {
		return nil, err
	}
	out.Compressed = z
	return out, nil
}

func clampPeakByte(v float32) byte {
	iv := int(math.Round(float64(v) * 127.0))
	if iv < 0 {
		return 0
	}
	if iv > 127 {
		return 127
	}
	return byte(iv)
}

// ComputeOverviewPeaks decodes path in a single pass with O(1) memory and builds
// Engine's 1024-bin 3-band peak overview (3099-byte raw + qCompressed blobs).
// Containers not supported natively (.mp4/AAC) return ErrUnsupportedOverviewFormat.
func ComputeOverviewPeaks(ctx context.Context, path string, ticker Ticker) (*OverviewPeaks, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(tickReader{f, ticker}, 1<<16)
	if err := audioheader.SkipID3(br); err != nil {
		return nil, err
	}
	head, _ := br.Peek(12)
	switch {
	case len(head) >= 4 && string(head[0:4]) == "fLaC":
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		ov, err := computeFLACPeaksFast(tickReader{f, ticker}, ticker)
		if !errors.Is(err, errFastFLACFallback) {
			return ov, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return computeFLACPeaksSlow(f, ticker)
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return computeWAVPeaks(br, ticker)
	case len(head) >= 4 && string(head[0:4]) == "OggS":
		return computeOggPeaks(br, ticker)
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		return nil, ErrUnsupportedOverviewFormat
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xF6 == 0xF0:
		// ADTS AAC sync word (layer bits == 00)
		return nil, ErrUnsupportedOverviewFormat
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		return computeMP3Peaks(br, ticker)
	case strings.EqualFold(filepath.Ext(path), ".mp4") || strings.EqualFold(filepath.Ext(path), ".m4a") || strings.EqualFold(filepath.Ext(path), ".aac"):
		return nil, ErrUnsupportedOverviewFormat
	default:
		return nil, ErrUnsupportedOverviewFormat
	}
}

func computeFLACPeaksSlow(f *os.File, ticker Ticker) (ov *OverviewPeaks, err error) {
	defer func() {
		if p := recover(); p != nil {
			ov, err = nil, fmt.Errorf("analyzer: flac: malformed stream: %v", p)
		}
	}()
	br := bufio.NewReaderSize(tickReader{f, ticker}, 1<<16)
	if err := audioheader.SkipID3(br); err != nil {
		return nil, err
	}
	st, err := flac.New(br)
	if err != nil {
		return nil, err
	}
	info := st.Info
	bank, err := newPeakFilterBank(int(info.SampleRate), int(info.NChannels), int(info.BitsPerSample), int64(info.NSamples))
	if err != nil {
		return nil, err
	}
	for {
		if ticker != nil {
			if err := ticker.Tick(); err != nil {
				return nil, err
			}
		}
		fr, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if fr.BitsPerSample == 0 {
			fr.BitsPerSample = info.BitsPerSample
		}
		if fr.SampleRate == 0 {
			fr.SampleRate = info.SampleRate
		}
		if fr.BitsPerSample != info.BitsPerSample || fr.SampleRate != info.SampleRate || fr.Channels.Count() != int(info.NChannels) {
			return nil, errors.New("analyzer: flac: frame format differs from STREAMINFO")
		}
		if err := fr.Parse(); err != nil {
			return nil, err
		}
		n := fr.Subframes[0].NSamples
		if info.NChannels == 1 {
			bank.feedInt32(fr.Subframes[0].Samples[:n], nil)
		} else {
			bank.feedInt32(fr.Subframes[0].Samples[:n], fr.Subframes[1].Samples[:n])
		}
	}
	return bank.finish("flac")
}

func computeWAVPeaks(r io.Reader, ticker Ticker) (ov *OverviewPeaks, err error) {
	defer func() {
		if p := recover(); p != nil {
			ov, err = nil, fmt.Errorf("analyzer: wav: malformed stream: %v", p)
		}
	}()
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil, err
	}
	var format, channels, bits, sampleRate int
	var haveFmt bool
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, fmt.Errorf("analyzer: wav: missing data chunk: %w", err)
		}
		id := string(hdr[0:4])
		size := int64(binary.LittleEndian.Uint32(hdr[4:8]))
		switch id {
		case "fmt ":
			if size < 16 || size > 65536 {
				return nil, errors.New("analyzer: wav: fmt chunk too short")
			}
			buf := make([]byte, size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil, err
			}
			format = int(binary.LittleEndian.Uint16(buf[0:2]))
			channels = int(binary.LittleEndian.Uint16(buf[2:4]))
			sampleRate = int(binary.LittleEndian.Uint32(buf[4:8]))
			bits = int(binary.LittleEndian.Uint16(buf[14:16]))
			if format == wavFormatExtensible {
				if size < 40 {
					return nil, errors.New("analyzer: wav: extensible fmt too short")
				}
				format = int(binary.LittleEndian.Uint16(buf[24:26]))
			}
			haveFmt = true
			if size%2 == 1 {
				if _, err := io.CopyN(io.Discard, r, 1); err != nil {
					return nil, err
				}
			}
		case "data":
			if !haveFmt {
				return nil, errors.New("analyzer: wav: data before fmt")
			}
			if channels < 1 || channels > 8 || sampleRate < 8000 || sampleRate > 384000 {
				return nil, errors.New("analyzer: wav: invalid fmt")
			}
			bps := bits / 8
			if bps == 0 || bits%8 != 0 {
				return nil, errors.New("analyzer: wav: invalid bit depth")
			}
			frame := bps * channels
			var total int64
			var src io.Reader = r
			if size > 0 && size != 0xFFFFFFFF {
				src = io.LimitReader(r, size)
				total = size / int64(frame)
			}
			bank, err := newPeakFilterBank(sampleRate, channels, bits, total)
			if err != nil {
				return nil, err
			}
			conv := wavSampleReader(format, bits)
			if conv == nil {
				return nil, fmt.Errorf("analyzer: wav: unsupported format %#x / %d bit", format, bits)
			}
			buf := make([]byte, frame*4096)
			stereo := channels >= 2
			for {
				if ticker != nil {
					if err := ticker.Tick(); err != nil {
						return nil, err
					}
				}
				n, rerr := io.ReadFull(src, buf)
				if n%frame != 0 {
					return nil, errors.New("analyzer: truncated WAV frame")
				}
				for off := 0; off < n; off += frame {
					xL := conv(buf[off:])
					var xR float32
					if stereo {
						xR = conv(buf[off+bps:])
					}
					if math.IsNaN(float64(xL)) || math.IsInf(float64(xL), 0) || math.IsNaN(float64(xR)) || math.IsInf(float64(xR), 0) {
						return nil, errors.New("analyzer: wav: non-finite sample")
					}
					bank.pushFloatSample(xL, xR, stereo)
				}
				if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
					return bank.finish("wav")
				}
				if rerr != nil {
					return nil, rerr
				}
			}
		default:
			if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
				return nil, err
			}
		}
	}
}

func wavSampleReader(format, bits int) func([]byte) float32 {
	switch {
	case format == wavFormatPCM && bits == 8:
		return func(b []byte) float32 { return (float32(b[0]) - 128) / 128 }
	case format == wavFormatPCM && bits == 16:
		return func(b []byte) float32 { return float32(int16(binary.LittleEndian.Uint16(b))) / 32768 }
	case format == wavFormatPCM && bits == 24:
		return func(b []byte) float32 {
			v := int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24) >> 8
			return float32(float64(v) / 8388608)
		}
	case format == wavFormatPCM && bits == 32:
		return func(b []byte) float32 { return float32(float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648) }
	case format == wavFormatFloat && bits == 32:
		return func(b []byte) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(b)) }
	case format == wavFormatFloat && bits == 64:
		return func(b []byte) float32 { return float32(math.Float64frombits(binary.LittleEndian.Uint64(b))) }
	default:
		return nil
	}
}

func computeMP3Peaks(br *bufio.Reader, ticker Ticker) (ov *OverviewPeaks, err error) {
	defer func() {
		if p := recover(); p != nil {
			ov, err = nil, fmt.Errorf("analyzer: mp3: malformed stream: %v", p)
		}
	}()
	startTrim := audioheader.PeekMP3StartOffset(br)
	dec, err := mp3.NewDecoder(br)
	if err != nil {
		return nil, fmt.Errorf("analyzer: mp3: %w", err)
	}
	bank, err := newPeakFilterBank(dec.SampleRate(), 2, 16, 0)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 4*4096)
	trimmed := 0
	for {
		if ticker != nil {
			if err := ticker.Tick(); err != nil {
				return nil, err
			}
		}
		n, rerr := io.ReadFull(dec, buf)
		n -= n % 4
		for off := 0; off < n; off += 4 {
			if trimmed < startTrim {
				trimmed++
				continue
			}
			l := float32(int16(binary.LittleEndian.Uint16(buf[off:]))) / 32768
			r := float32(int16(binary.LittleEndian.Uint16(buf[off+2:]))) / 32768
			bank.pushFloatSample(l, r, true)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return bank.finish("mp3")
		}
		if rerr != nil {
			return nil, fmt.Errorf("analyzer: mp3: %w", rerr)
		}
	}
}

func computeOggPeaks(r io.Reader, ticker Ticker) (ov *OverviewPeaks, err error) {
	defer func() {
		if recover() != nil {
			ov, err = nil, errors.New("analyzer: ogg: malformed Vorbis stream")
		}
	}()
	dec, err := oggvorbis.NewReader(tickReader{r, ticker})
	if err != nil {
		return nil, fmt.Errorf("analyzer: ogg: %w", err)
	}
	sr, ch := dec.SampleRate(), dec.Channels()
	bank, err := newPeakFilterBank(sr, ch, 16, 0)
	if err != nil {
		return nil, err
	}
	buf := make([]float32, 4096*ch)
	stereo := ch >= 2
	for {
		if ticker != nil {
			if err := ticker.Tick(); err != nil {
				return nil, err
			}
		}
		n, rerr := dec.Read(buf)
		for i := 0; i+ch <= n; i += ch {
			xL := buf[i]
			var xR float32
			if stereo {
				xR = buf[i+1]
			}
			if math.IsNaN(float64(xL)) || math.IsInf(float64(xL), 0) || math.IsNaN(float64(xR)) || math.IsInf(float64(xR), 0) {
				return nil, errors.New("analyzer: ogg: non-finite sample")
			}
			bank.pushFloatSample(xL, xR, stereo)
		}
		if rerr == io.EOF {
			if dec.Length() <= 0 {
				return nil, errors.New("analyzer: ogg: missing end of stream")
			}
			return bank.finish("ogg")
		}
		if rerr != nil {
			return nil, fmt.Errorf("analyzer: ogg: %w", rerr)
		}
	}
}
