package stems

import (
	"archive/tar"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
)

// radix2FFT computes N-point real-to-complex (rfft) and complex-to-real (irfft)
// transforms using an M=N/2 complex Cooley-Tukey radix-2 plan.
type radix2FFT struct {
	n      int
	m      int
	bitrev []int
	cosM   []float64 // cos(-2*pi*k/M) for k in [0, M/2)
	sinM   []float64 // sin(-2*pi*k/M) for k in [0, M/2)
	cosN   []float64 // cos(-2*pi*k/N) for k in [0, M)
	sinN   []float64 // sin(-2*pi*k/N) for k in [0, M)
}

func newRadix2FFT(n int) *radix2FFT {
	if n < 4 || n&(n-1) != 0 {
		panic(fmt.Sprintf("stems: FFT size %d must be a power of two >= 4", n))
	}
	m := n / 2
	lg := bits.TrailingZeros(uint(m))
	rev := make([]int, m)
	for i := 0; i < m; i++ {
		rev[i] = int(bits.Reverse(uint(i)) >> (bits.UintSize - lg))
	}
	cosM := make([]float64, m/2)
	sinM := make([]float64, m/2)
	for k := 0; k < m/2; k++ {
		ang := -2.0 * math.Pi * float64(k) / float64(m)
		cosM[k] = math.Cos(ang)
		sinM[k] = math.Sin(ang)
	}
	cosN := make([]float64, m)
	sinN := make([]float64, m)
	for k := 0; k < m; k++ {
		ang := -2.0 * math.Pi * float64(k) / float64(n)
		cosN[k] = math.Cos(ang)
		sinN[k] = math.Sin(ang)
	}
	return &radix2FFT{n: n, m: m, bitrev: rev, cosM: cosM, sinM: sinM, cosN: cosN, sinN: sinN}
}

func (f *radix2FFT) cfftM(zr, zi []float64) {
	m := f.m
	for i := 0; i < m; i++ {
		j := f.bitrev[i]
		if i < j {
			zr[i], zr[j] = zr[j], zr[i]
			zi[i], zi[j] = zi[j], zi[i]
		}
	}
	for size := 2; size <= m; size <<= 1 {
		half := size >> 1
		step := m / size
		for base := 0; base < m; base += size {
			for k := 0; k < half; k++ {
				wr := f.cosM[k*step]
				wi := f.sinM[k*step]
				i0 := base + k
				i1 := i0 + half
				tr := wr*zr[i1] - wi*zi[i1]
				ti := wr*zi[i1] + wi*zr[i1]
				zr[i1] = zr[i0] - tr
				zi[i1] = zi[i0] - ti
				zr[i0] += tr
				zi[i0] += ti
			}
		}
	}
}

// rfft writes N/2+1 complex bins (outRe, outIm) from N real samples x.
// scratch must have length >= N (used as [M]zr, [M]zi).
func (f *radix2FFT) rfft(x []float32, outRe, outIm []float32, scratch []float64) {
	m := f.m
	zr := scratch[:m]
	zi := scratch[m : 2*m]
	for k := 0; k < m; k++ {
		zr[k] = float64(x[2*k])
		zi[k] = float64(x[2*k+1])
	}
	f.cfftM(zr, zi)
	outRe[0] = float32(zr[0] + zi[0])
	outIm[0] = 0
	outRe[m] = float32(zr[0] - zi[0])
	outIm[m] = 0
	for k := 1; k < m; k++ {
		mk := m - k
		er := 0.5 * (zr[k] + zr[mk])
		ei := 0.5 * (zi[k] - zi[mk])
		or := 0.5 * (zi[k] + zi[mk])
		oi := 0.5 * (zr[mk] - zr[k])
		wr, wi := f.cosN[k], f.sinN[k]
		outRe[k] = float32(er + wr*or - wi*oi)
		outIm[k] = float32(ei + wr*oi + wi*or)
	}
}

// irfft reconstructs N real samples in out from N/2+1 complex bins (inRe, inIm).
// Includes standard 1/N inverse scaling so irfft(rfft(x)) == x.
func (f *radix2FFT) irfft(inRe, inIm []float32, out []float32, scratch []float64) {
	m := f.m
	zr := scratch[:m]
	zi := scratch[m : 2*m]
	zr[0] = 0.5 * (float64(inRe[0]) + float64(inRe[m]))
	zi[0] = -0.5 * (float64(inRe[0]) - float64(inRe[m])) // negated for conj-FFT trick
	for k := 1; k < m; k++ {
		mk := m - k
		xkr, xki := float64(inRe[k]), float64(inIm[k])
		xmr, xmi := float64(inRe[mk]), float64(inIm[mk])
		er := 0.5 * (xkr + xmr)
		ei := 0.5 * (xki - xmi)
		tr := 0.5 * (xkr - xmr)
		ti := 0.5 * (xki + xmi)
		// odd = conj(tw) * (tr + i*ti) where tw = cosN[k] + i*sinN[k]
		wr, wi := f.cosN[k], f.sinN[k]
		or := wr*tr + wi*ti
		oi := wr*ti - wi*tr
		// Z[k] = even + i*odd = (er - oi) + i*(ei + or); negate imag for inverse via forward cfftM
		zr[k] = er - oi
		zi[k] = -(ei + or)
	}
	f.cfftM(zr, zi)
	invM := 1.0 / float64(m)
	for k := 0; k < m; k++ {
		out[2*k] = float32(zr[k] * invM)
		out[2*k+1] = float32(-zi[k] * invM)
	}
}

func hannPeriodic(n int) []float32 {
	w := make([]float32, n)
	for i := 0; i < n; i++ {
		w[i] = float32(0.5 - 0.5*math.Cos(2.0*math.Pi*float64(i)/float64(n)))
	}
	return w
}

func hannSymmetric(n int) []float32 {
	w := make([]float32, n)
	if n <= 1 {
		for i := range w {
			w[i] = 1
		}
		return w
	}
	for i := 0; i < n; i++ {
		w[i] = float32(0.5 - 0.5*math.Cos(2.0*math.Pi*float64(i)/float64(n-1)))
	}
	return w
}

// reflectPad1D matches Meta Demucs pad1d(..., mode="reflect"), zero-padding
// tiny inputs first when len(x) <= max(padLeft, padRight).
func reflectPad1D(x []float32, padLeft, padRight int) []float32 {
	if len(x) == 0 {
		return make([]float32, padLeft+padRight)
	}
	src := x
	pl, pr := padLeft, padRight
	maxPad := max(pl, pr)
	if len(src) <= maxPad {
		extra := maxPad - len(src) + 1
		extraR := min(pr, extra)
		extraL := extra - extraR
		tmp := make([]float32, extraL+len(src)+extraR)
		copy(tmp[extraL:], src)
		src = tmp
		pl -= extraL
		pr -= extraR
	}
	n := len(src)
	out := make([]float32, pl+n+pr)
	copy(out[pl:pl+n], src)
	for i := 0; i < pl; i++ {
		idx := pl - i
		if idx >= n {
			idx = n - 1
		}
		out[i] = src[idx]
	}
	for i := 0; i < pr; i++ {
		idx := n - 2 - i
		if idx < 0 {
			idx = 0
		}
		out[pl+n+i] = src[idx]
	}
	return out
}

func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	half := 0.5 * x
	for k := 1; k <= 24; k++ {
		term *= (half * half) / float64(k*k)
		sum += term
		if term < 1e-12*sum {
			break
		}
	}
	return sum
}

// resampleChannel resamples x from inRate to outRate producing exact outLen samples
// using an anti-aliased Kaiser-windowed sinc filter.
func resampleChannel(x []float32, inRate, outRate float64, outLen int) []float32 {
	if outLen <= 0 {
		return nil
	}
	if len(x) == 0 {
		return make([]float32, outLen)
	}
	if math.Abs(inRate-outRate) < 1e-6 && len(x) == outLen {
		cp := make([]float32, len(x))
		copy(cp, x)
		return cp
	}
	out := make([]float32, outLen)
	ratio := outRate / inRate
	cutoff := math.Min(1.0, ratio) * 0.94
	const halfZeros = 16
	const beta = 6.0
	invI0 := 1.0 / besselI0(beta)
	support := float64(halfZeros) / cutoff
	inLen := len(x)
	for i := 0; i < outLen; i++ {
		center := float64(i) / ratio
		j0 := int(math.Ceil(center - support))
		j1 := int(math.Floor(center + support))
		var acc, wsum float64
		for j := j0; j <= j1; j++ {
			d := center - float64(j)
			t := d / support
			if t <= -1.0 || t >= 1.0 {
				continue
			}
			win := besselI0(beta*math.Sqrt(1.0-t*t)) * invI0
			arg := math.Pi * d * cutoff
			sinc := cutoff
			if math.Abs(arg) > 1e-9 {
				sinc = cutoff * math.Sin(arg) / arg
			}
			w := win * sinc
			wsum += w
			if j >= 0 && j < inLen {
				acc += float64(x[j]) * w
			}
		}
		if wsum > 1e-12 {
			out[i] = float32(acc / wsum)
		}
	}
	return out
}

// float16ToFloat32 converts an IEEE-754 binary16 value to float32.
func float16ToFloat32(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32((h >> 10) & 0x1f)
	mant := uint32(h & 0x03ff)
	switch exp {
	case 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		for (mant & 0x0400) == 0 {
			mant <<= 1
			exp--
		}
		exp++
		mant &= 0x03ff
		return math.Float32frombits(sign | ((exp + (127 - 15)) << 23) | (mant << 13))
	case 0x1f:
		return math.Float32frombits(sign | 0x7f800000 | (mant << 13))
	default:
		return math.Float32frombits(sign | ((exp + (127 - 15)) << 23) | (mant << 13))
	}
}

type tensorView struct {
	Shape []int
	Data  []float32
}

// parseSafetensors decodes F16 and F32 tensors from a safetensors byte buffer.
func parseSafetensors(raw []byte) (map[string]tensorView, error) {
	if len(raw) < 8 {
		return nil, errors.New("safetensors: truncated header length")
	}
	hdrLen := binary.LittleEndian.Uint64(raw[:8])
	if hdrLen > 64<<20 || uint64(len(raw)-8) < hdrLen {
		return nil, errors.New("safetensors: invalid header length")
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw[8:8+hdrLen], &meta); err != nil {
		return nil, fmt.Errorf("safetensors: header json: %w", err)
	}
	body := raw[8+hdrLen:]
	out := make(map[string]tensorView, len(meta))
	for name, rm := range meta {
		if name == "__metadata__" {
			continue
		}
		var entry struct {
			DType   string    `json:"dtype"`
			Shape   []int     `json:"shape"`
			Offsets [2]uint64 `json:"data_offsets"`
		}
		if err := json.Unmarshal(rm, &entry); err != nil {
			return nil, fmt.Errorf("safetensors: tensor %s: %w", name, err)
		}
		if entry.Offsets[1] < entry.Offsets[0] || entry.Offsets[1] > uint64(len(body)) {
			return nil, fmt.Errorf("safetensors: tensor %s offsets out of bounds", name)
		}
		count := 1
		for _, d := range entry.Shape {
			if d < 0 {
				return nil, fmt.Errorf("safetensors: tensor %s negative dim", name)
			}
			count *= d
		}
		slice := body[entry.Offsets[0]:entry.Offsets[1]]
		vals := make([]float32, count)
		switch entry.DType {
		case "F32":
			if len(slice) != count*4 {
				return nil, fmt.Errorf("safetensors: tensor %s byte count mismatch", name)
			}
			for i := 0; i < count; i++ {
				vals[i] = math.Float32frombits(binary.LittleEndian.Uint32(slice[i*4:]))
			}
		case "F16":
			if len(slice) != count*2 {
				return nil, fmt.Errorf("safetensors: tensor %s byte count mismatch", name)
			}
			for i := 0; i < count; i++ {
				vals[i] = float16ToFloat32(binary.LittleEndian.Uint16(slice[i*2:]))
			}
		default:
			return nil, fmt.Errorf("safetensors: unsupported dtype %q for %s", entry.DType, name)
		}
		out[name] = tensorView{Shape: entry.Shape, Data: vals}
	}
	return out, nil
}

// parseTFBundle reads float32 tensors from a TensorFlow v1 TensorBundle
// (model.index SSTable + model.data-00000-of-00001).
func parseTFBundle(indexBytes []byte, readData func(offset, size int64) ([]byte, error)) (map[string]tensorView, error) {
	const sstableMagic = uint64(0xdb4775248b80fb57)
	if len(indexBytes) < 48 {
		return nil, errors.New("tfbundle: index too small")
	}
	if binary.LittleEndian.Uint64(indexBytes[len(indexBytes)-8:]) != sstableMagic {
		return nil, errors.New("tfbundle: invalid sstable magic")
	}
	readUvarint := func(b []byte, p int) (uint64, int, error) {
		if p >= len(b) {
			return 0, p, errors.New("tfbundle: truncated varint")
		}
		v, n := binary.Uvarint(b[p:])
		if n <= 0 {
			return 0, p, errors.New("tfbundle: invalid varint")
		}
		return v, p + n, nil
	}
	pos := len(indexBytes) - 48
	_, pos, err := readUvarint(indexBytes, pos) // metaindex offset
	if err != nil {
		return nil, err
	}
	_, pos, err = readUvarint(indexBytes, pos) // metaindex size
	if err != nil {
		return nil, err
	}
	idxOff, pos, err := readUvarint(indexBytes, pos)
	if err != nil {
		return nil, err
	}
	idxSz, _, err := readUvarint(indexBytes, pos)
	if err != nil {
		return nil, err
	}
	readBlock := func(off, sz uint64) ([]byte, error) {
		if off+sz+1 > uint64(len(indexBytes)) {
			return nil, errors.New("tfbundle: block out of range")
		}
		if indexBytes[off+sz] != 0 {
			return nil, fmt.Errorf("tfbundle: compressed sstable block unsupported (type=%d)", indexBytes[off+sz])
		}
		return indexBytes[off : off+sz], nil
	}
	iterBlock := func(blk []byte, fn func(key, val []byte) error) error {
		if len(blk) < 4 {
			return errors.New("tfbundle: short block")
		}
		restarts := int(binary.LittleEndian.Uint32(blk[len(blk)-4:]))
		if 4*(1+restarts) > len(blk) {
			return errors.New("tfbundle: corrupt block restarts")
		}
		limit := len(blk) - 4*(1+restarts)
		var prev []byte
		p := 0
		for p < limit {
			shared, np, err := readUvarint(blk, p)
			if err != nil {
				return err
			}
			nonShared, np, err := readUvarint(blk, np)
			if err != nil {
				return err
			}
			vlen, np, err := readUvarint(blk, np)
			if err != nil {
				return err
			}
			p = np
			if int(shared) > len(prev) || p+int(nonShared)+int(vlen) > limit {
				return errors.New("tfbundle: corrupt block entry")
			}
			k := make([]byte, int(shared)+int(nonShared))
			copy(k, prev[:shared])
			copy(k[shared:], blk[p:p+int(nonShared)])
			p += int(nonShared)
			v := blk[p : p+int(vlen)]
			p += int(vlen)
			prev = k
			if err := fn(k, v); err != nil {
				return err
			}
		}
		return nil
	}
	idxBlk, err := readBlock(idxOff, idxSz)
	if err != nil {
		return nil, err
	}
	out := map[string]tensorView{}
	err = iterBlock(idxBlk, func(_, handle []byte) error {
		doff, hp, err := readUvarint(handle, 0)
		if err != nil {
			return err
		}
		dsz, _, err := readUvarint(handle, hp)
		if err != nil {
			return err
		}
		dblk, err := readBlock(doff, dsz)
		if err != nil {
			return err
		}
		return iterBlock(dblk, func(k, val []byte) error {
			if len(k) == 0 {
				return nil // BundleHeaderProto under empty key
			}
			name := string(k)
			var dtype uint64
			var off, sz int64
			var dims []int
			p := 0
			for p < len(val) {
				tag, np, err := readUvarint(val, p)
				if err != nil {
					return err
				}
				p = np
				field, wtype := tag>>3, tag&7
				switch wtype {
				case 0:
					v, np, err := readUvarint(val, p)
					if err != nil {
						return err
					}
					p = np
					switch field {
					case 1:
						dtype = v
					case 4:
						off = int64(v)
					case 5:
						sz = int64(v)
					}
				case 2:
					ln, np, err := readUvarint(val, p)
					if err != nil || np+int(ln) > len(val) {
						return errors.New("tfbundle: bad submessage")
					}
					sub := val[np : np+int(ln)]
					p = np + int(ln)
					if field == 2 {
						sp := 0
						for sp < len(sub) {
							stag, snp, err := readUvarint(sub, sp)
							if err != nil {
								return err
							}
							sp = snp
							if stag>>3 == 2 && stag&7 == 2 {
								dln, snp, err := readUvarint(sub, sp)
								if err != nil || snp+int(dln) > len(sub) {
									return errors.New("tfbundle: bad dim")
								}
								dsub := sub[snp : snp+int(dln)]
								sp = snp + int(dln)
								dp := 0
								for dp < len(dsub) {
									dtag, dnp, err := readUvarint(dsub, dp)
									if err != nil {
										return err
									}
									dp = dnp
									if dtag>>3 == 1 && dtag&7 == 0 {
										dv, dnp, err := readUvarint(dsub, dp)
										if err != nil {
											return err
										}
										dp = dnp
										dims = append(dims, int(int64(dv)))
									} else if dtag&7 == 0 {
										_, dp, err = readUvarint(dsub, dp)
										if err != nil {
											return err
										}
									} else if dtag&7 == 2 {
										l2, dnp, err := readUvarint(dsub, dp)
										if err != nil || dnp+int(l2) > len(dsub) {
											return errors.New("tfbundle: skip dim")
										}
										dp = dnp + int(l2)
									}
								}
							} else if stag&7 == 0 {
								_, sp, err = readUvarint(sub, sp)
								if err != nil {
									return err
								}
							}
						}
					}
				case 5:
					p += 4
				case 1:
					p += 8
				default:
					return fmt.Errorf("tfbundle: unsupported wire type %d", wtype)
				}
			}
			if dtype != 1 { // DT_FLOAT = 1; skip global_step (DT_INT64)
				return nil
			}
			if strings.Contains(name, "Adam") || strings.Contains(name, "_power") {
				return nil
			}
			raw, err := readData(off, sz)
			if err != nil {
				return err
			}
			if int64(len(raw)) != sz || sz%4 != 0 {
				return fmt.Errorf("tfbundle: tensor %s short read", name)
			}
			vals := make([]float32, sz/4)
			for i := range vals {
				vals[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
			}
			out[name] = tensorView{Shape: dims, Data: vals}
			return nil
		})
	})
	return out, err
}

func loadTFBundleDir(dir string) (map[string]tensorView, error) {
	idx, err := os.ReadFile(filepath.Join(dir, "model.index"))
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "model.data-00000-of-00001"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseTFBundle(idx, func(offset, size int64) ([]byte, error) {
		if offset < 0 || size < 0 || size > 256<<20 {
			return nil, errors.New("tfbundle: invalid slice")
		}
		buf := make([]byte, size)
		_, err := f.ReadAt(buf, offset)
		return buf, err
	})
}

func loadTFBundleTarGz(path string) (map[string]tensorView, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var idxBytes, dataBytes []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		base := filepath.Base(hdr.Name)
		if strings.HasPrefix(base, "._") {
			continue
		}
		switch base {
		case "model.index":
			if hdr.Size < 0 || hdr.Size > 16<<20 {
				return nil, errors.New("tfbundle: oversized model.index")
			}
			idxBytes, err = io.ReadAll(io.LimitReader(tr, hdr.Size))
			if err != nil {
				return nil, err
			}
		case "model.data-00000-of-00001":
			if hdr.Size < 0 || hdr.Size > 512<<20 {
				return nil, errors.New("tfbundle: oversized model.data")
			}
			dataBytes, err = io.ReadAll(io.LimitReader(tr, hdr.Size))
			if err != nil {
				return nil, err
			}
		}
	}
	if len(idxBytes) == 0 || len(dataBytes) == 0 {
		return nil, errors.New("tfbundle: archive missing model.index or model.data-00000-of-00001")
	}
	return parseTFBundle(idxBytes, func(offset, size int64) ([]byte, error) {
		if offset < 0 || size < 0 || offset+size > int64(len(dataBytes)) {
			return nil, errors.New("tfbundle: slice out of bounds")
		}
		return dataBytes[offset : offset+size], nil
	})
}
