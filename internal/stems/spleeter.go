package stems

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	spleeterSampleRate = 44100.0
	spleeterFFTSize    = 4096
	spleeterHop        = 1024
	spleeterBins       = spleeterFFTSize/2 + 1 // 2049
	spleeterTimeFrames = 512                   // ~11.89 s per U-Net window
	spleeterFreqBins   = 1024
	spleeterBatchEps   = 1e-3  // tf.keras.layers.BatchNormalization default epsilon
	spleeterMaskEps    = 1e-10 // EstimatorSpecBuilder.EPSILON
	spleeterWinComp    = 2.0 / 3.0
)

type bnAffine struct {
	scale []float32
	shift []float32
}

type spleeterUNet struct {
	encW  [6][]float32 // [25*Cin, Cout] row-major for sgemm
	encB  [6][]float32 // [Cout]
	encBN [5]bnAffine  // BN 0..4 (BN 5 output is unused in deezer/spleeter unet.py)

	decW  [6][]float32 // [Cin, 25*Cout] row-major for sgemm
	decB  [6][]float32 // [Cout]
	decBN [6]bnAffine  // BN 6..11

	outW []float32 // [16*1, 2] dilated 4x4 conv
	outB []float32 // [2]
}

// SpleeterModel holds the 4 instrument U-Nets (vocals, drums, bass, other)
// from Deezer's MIT Spleeter 4stems model.
type SpleeterModel struct {
	nets    [4]spleeterUNet // 0: vocals, 1: drums, 2: bass, 3: other
	fft     *radix2FFT
	window  []float32
	workers int
}

// LoadSpleeterPath loads Spleeter 4-stem weights from a .safetensors file,
// Deezer's official 4stems.tar.gz archive, or an extracted checkpoint directory.
func LoadSpleeterPath(path string) (*SpleeterModel, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var tensors map[string]tensorView
	switch {
	case info.IsDir():
		if _, err := os.Stat(filepath.Join(path, "spleeter-4stems.safetensors")); err == nil {
			raw, err := os.ReadFile(filepath.Join(path, "spleeter-4stems.safetensors"))
			if err != nil {
				return nil, err
			}
			tensors, err = parseSafetensors(raw)
			if err != nil {
				return nil, err
			}
		} else if _, err := os.Stat(filepath.Join(path, "4stems.tar.gz")); err == nil {
			tensors, err = loadTFBundleTarGz(filepath.Join(path, "4stems.tar.gz"))
			if err != nil {
				return nil, err
			}
		} else if _, err := os.Stat(filepath.Join(path, "4stems", "model.index")); err == nil {
			tensors, err = loadTFBundleDir(filepath.Join(path, "4stems"))
			if err != nil {
				return nil, err
			}
		} else {
			tensors, err = loadTFBundleDir(path)
			if err != nil {
				return nil, err
			}
		}
	case strings.HasSuffix(path, ".safetensors"):
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		tensors, err = parseSafetensors(raw)
		if err != nil {
			return nil, err
		}
	case strings.HasSuffix(path, ".tar.gz") || strings.HasSuffix(path, ".tgz"):
		tensors, err = loadTFBundleTarGz(path)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("spleeter: unsupported model path %q", path)
	}
	return buildSpleeterModel(tensors)
}

func tfIndexedName(base string, idx int) string {
	if idx == 0 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, idx)
}

func loadBNAffine(tensors map[string]tensorView, idx, channels int) (bnAffine, error) {
	prefix := tfIndexedName("batch_normalization", idx)
	gamma, ok1 := tensors[prefix+"/gamma"]
	beta, ok2 := tensors[prefix+"/beta"]
	mean, ok3 := tensors[prefix+"/moving_mean"]
	variance, ok4 := tensors[prefix+"/moving_variance"]
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return bnAffine{}, fmt.Errorf("spleeter: missing batchnorm tensors for %s", prefix)
	}
	if len(gamma.Data) != channels || len(beta.Data) != channels || len(mean.Data) != channels || len(variance.Data) != channels {
		return bnAffine{}, fmt.Errorf("spleeter: batchnorm %s channel mismatch (want %d)", prefix, channels)
	}
	scale := make([]float32, channels)
	shift := make([]float32, channels)
	for c := 0; c < channels; c++ {
		sc := float64(gamma.Data[c]) / math.Sqrt(float64(variance.Data[c])+spleeterBatchEps)
		scale[c] = float32(sc)
		shift[c] = float32(float64(beta.Data[c]) - float64(mean.Data[c])*sc)
	}
	return bnAffine{scale: scale, shift: shift}, nil
}

func buildSpleeterModel(tensors map[string]tensorView) (*SpleeterModel, error) {
	m := &SpleeterModel{
		fft:     newRadix2FFT(spleeterFFTSize),
		window:  hannPeriodic(spleeterFFTSize),
		workers: defaultWorkers(),
	}
	encIn := [6]int{2, 16, 32, 64, 128, 256}
	encOut := [6]int{16, 32, 64, 128, 256, 512}
	decIn := [6]int{512, 512, 256, 128, 64, 32}
	decOut := [6]int{256, 128, 64, 32, 16, 1}

	for s := 0; s < 4; s++ {
		var u spleeterUNet
		for i := 0; i < 6; i++ {
			cname := tfIndexedName("conv2d", 7*s+i)
			kw, ok1 := tensors[cname+"/kernel"]
			kb, ok2 := tensors[cname+"/bias"]
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("spleeter: missing %s", cname)
			}
			wantW := 25 * encIn[i] * encOut[i]
			if len(kw.Data) != wantW || len(kb.Data) != encOut[i] {
				return nil, fmt.Errorf("spleeter: %s shape mismatch", cname)
			}
			// TF Conv2D kernel is already [5, 5, Cin, Cout] == [25*Cin, Cout] row-major!
			u.encW[i] = slicesCloneF32(kw.Data)
			u.encB[i] = slicesCloneF32(kb.Data)
			if i < 5 {
				bn, err := loadBNAffine(tensors, 12*s+i, encOut[i])
				if err != nil {
					return nil, err
				}
				u.encBN[i] = bn
			}
		}
		for i := 0; i < 6; i++ {
			tname := tfIndexedName("conv2d_transpose", 6*s+i)
			kw, ok1 := tensors[tname+"/kernel"]
			kb, ok2 := tensors[tname+"/bias"]
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("spleeter: missing %s", tname)
			}
			cin, cout := decIn[i], decOut[i]
			if len(kw.Data) != 25*cout*cin || len(kb.Data) != cout {
				return nil, fmt.Errorf("spleeter: %s shape mismatch", tname)
			}
			// TF Conv2DTranspose kernel is [5, 5, Cout, Cin]; pack as [Cin, 25*Cout] for sgemm!
			packed := make([]float32, cin*25*cout)
			for ky := 0; ky < 5; ky++ {
				for kx := 0; kx < 5; kx++ {
					kidx := ky*5 + kx
					for co := 0; co < cout; co++ {
						srcBase := ((ky*5+kx)*cout + co) * cin
						for ci := 0; ci < cin; ci++ {
							packed[ci*(25*cout)+kidx*cout+co] = kw.Data[srcBase+ci]
						}
					}
				}
			}
			u.decW[i] = packed
			u.decB[i] = slicesCloneF32(kb.Data)
			bn, err := loadBNAffine(tensors, 12*s+6+i, cout)
			if err != nil {
				return nil, err
			}
			u.decBN[i] = bn
		}
		oname := tfIndexedName("conv2d", 7*s+6)
		ow, ok1 := tensors[oname+"/kernel"]
		ob, ok2 := tensors[oname+"/bias"]
		if !ok1 || !ok2 || len(ow.Data) != 16*1*2 || len(ob.Data) != 2 {
			return nil, fmt.Errorf("spleeter: missing or invalid output layer %s", oname)
		}
		u.outW = slicesCloneF32(ow.Data)
		u.outB = slicesCloneF32(ob.Data)
		m.nets[s] = u
	}
	return m, nil
}

// NewTestSpleeterModel constructs a lightweight deterministic SpleeterModel
// with synthetic weights for fast unit tests without downloading 150 MB weights.
func NewTestSpleeterModel() *SpleeterModel {
	m := &SpleeterModel{
		fft:     newRadix2FFT(spleeterFFTSize),
		window:  hannPeriodic(spleeterFFTSize),
		workers: defaultWorkers(),
	}
	encIn := [6]int{2, 16, 32, 64, 128, 256}
	encOut := [6]int{16, 32, 64, 128, 256, 512}
	decIn := [6]int{512, 512, 256, 128, 64, 32}
	decOut := [6]int{256, 128, 64, 32, 16, 1}
	for s := 0; s < 4; s++ {
		var u spleeterUNet
		for i := 0; i < 6; i++ {
			u.encW[i] = make([]float32, 25*encIn[i]*encOut[i])
			u.encB[i] = make([]float32, encOut[i])
			if i < 5 {
				sc := make([]float32, encOut[i])
				for c := range sc {
					sc[c] = 1
				}
				u.encBN[i] = bnAffine{scale: sc, shift: make([]float32, encOut[i])}
			}
		}
		for i := 0; i < 6; i++ {
			u.decW[i] = make([]float32, decIn[i]*25*decOut[i])
			u.decB[i] = make([]float32, decOut[i])
			sc := make([]float32, decOut[i])
			for c := range sc {
				sc[c] = 1
			}
			u.decBN[i] = bnAffine{scale: sc, shift: make([]float32, decOut[i])}
		}
		u.outW = make([]float32, 16*1*2)
		u.outB = []float32{float32(s) * 0.25, float32(s) * 0.25}
		m.nets[s] = u
	}
	return m
}

func slicesCloneF32(src []float32) []float32 {
	dst := make([]float32, len(src))
	copy(dst, src)
	return dst
}

func addBias(buf []float32, rows int, bias []float32) {
	c := len(bias)
	for r := 0; r < rows; r++ {
		row := buf[r*c : (r+1)*c]
		for j := 0; j < c; j++ {
			row[j] += bias[j]
		}
	}
}

func applyBNAndELU(src []float32, rows int, bn bnAffine) []float32 {
	c := len(bn.scale)
	dst := make([]float32, len(src))
	for r := 0; r < rows; r++ {
		sr := src[r*c : (r+1)*c]
		dr := dst[r*c : (r+1)*c]
		for j := 0; j < c; j++ {
			v := sr[j]*bn.scale[j] + bn.shift[j]
			if v < 0 {
				v = float32(math.Exp(float64(v)) - 1.0)
			}
			dr[j] = v
		}
	}
	return dst
}

func applyELUAndBNInPlace(buf []float32, rows int, bn bnAffine) {
	c := len(bn.scale)
	for r := 0; r < rows; r++ {
		row := buf[r*c : (r+1)*c]
		for j := 0; j < c; j++ {
			v := row[j]
			if v < 0 {
				v = float32(math.Exp(float64(v)) - 1.0)
			}
			row[j] = v*bn.scale[j] + bn.shift[j]
		}
	}
}

func spleeterConv2DStride2(in []float32, H, W, Cin, Cout int, weights, bias []float32, workers int) []float32 {
	Hout, Wout := H/2, W/2
	K := 25 * Cin
	out := make([]float32, Hout*Wout*Cout)
	addBias(out, Hout*Wout, bias)
	stripH := min(32, Hout)
	col := make([]float32, stripH*Wout*K)
	for y0 := 0; y0 < Hout; y0 += stripH {
		y1 := min(y0+stripH, Hout)
		rows := (y1 - y0) * Wout
		clear(col[:rows*K])
		idx := 0
		for oy := y0; oy < y1; oy++ {
			for ox := 0; ox < Wout; ox++ {
				dst := col[idx*K : (idx+1)*K]
				p := 0
				for ky := 0; ky < 5; ky++ {
					iy := oy*2 + ky - 1
					if iy < 0 || iy >= H {
						p += 5 * Cin
						continue
					}
					rowOff := iy * W * Cin
					for kx := 0; kx < 5; kx++ {
						ix := ox*2 + kx - 1
						if ix >= 0 && ix < W {
							copy(dst[p:p+Cin], in[rowOff+ix*Cin:rowOff+(ix+1)*Cin])
						}
						p += Cin
					}
				}
				idx++
			}
		}
		sgemmAdd(rows, Cout, K, 1.0, col[:rows*K], weights, out[y0*Wout*Cout:y1*Wout*Cout], workers)
	}
	return out
}

func spleeterConv2DTransposeStride2(in []float32, Hin, Win, Cin, Cout int, weights, bias []float32, workers int) []float32 {
	Hout, Wout := Hin*2, Win*2
	N := 25 * Cout
	out := make([]float32, Hout*Wout*Cout)
	addBias(out, Hout*Wout, bias)
	stripH := min(32, Hin)
	col := make([]float32, stripH*Win*N)
	for y0 := 0; y0 < Hin; y0 += stripH {
		y1 := min(y0+stripH, Hin)
		rows := (y1 - y0) * Win
		clear(col[:rows*N])
		sgemmAdd(rows, N, Cin, 1.0, in[y0*Win*Cin:y1*Win*Cin], weights, col[:rows*N], workers)
		idx := 0
		for iy := y0; iy < y1; iy++ {
			for ix := 0; ix < Win; ix++ {
				src := col[idx*N : (idx+1)*N]
				p := 0
				for ky := 0; ky < 5; ky++ {
					oy := iy*2 + ky - 1
					if oy < 0 || oy >= Hout {
						p += 5 * Cout
						continue
					}
					rowOff := oy * Wout * Cout
					for kx := 0; kx < 5; kx++ {
						ox := ix*2 + kx - 1
						if ox >= 0 && ox < Wout {
							d := out[rowOff+ox*Cout : rowOff+(ox+1)*Cout]
							s := src[p : p+Cout]
							for c := 0; c < Cout; c++ {
								d[c] += s[c]
							}
						}
						p += Cout
					}
				}
				idx++
			}
		}
	}
	return out
}

// runSpleeterUNet predicts [T=512, F=1024, C=2] spectrogram magnitudes for one instrument.
func runSpleeterUNet(u *spleeterUNet, inputMag []float32, workers int) []float32 {
	encOut := [6]int{16, 32, 64, 128, 256, 512}
	decOut := [6]int{256, 128, 64, 32, 16, 1}
	var convs [6][]float32
	cur := inputMag
	curH, curW, curC := spleeterTimeFrames, spleeterFreqBins, 2
	for i := 0; i < 6; i++ {
		c := spleeterConv2DStride2(cur, curH, curW, curC, encOut[i], u.encW[i], u.encB[i], workers)
		convs[i] = c
		curH /= 2
		curW /= 2
		if i < 5 {
			cur = applyBNAndELU(c, curH*curW, u.encBN[i])
		}
		curC = encOut[i]
	}

	upIn := convs[5]
	upC := 512
	for i := 0; i < 6; i++ {
		up := spleeterConv2DTransposeStride2(upIn, curH, curW, upC, decOut[i], u.decW[i], u.decB[i], workers)
		curH *= 2
		curW *= 2
		applyELUAndBNInPlace(up, curH*curW, u.decBN[i])
		if i < 5 {
			skip := convs[4-i]
			sc := encOut[4-i]
			dc := decOut[i]
			px := curH * curW
			merged := make([]float32, px*(sc+dc))
			for p := 0; p < px; p++ {
				copy(merged[p*(sc+dc):p*(sc+dc)+sc], skip[p*sc:(p+1)*sc])
				copy(merged[p*(sc+dc)+sc:p*(sc+dc)+sc+dc], up[p*dc:(p+1)*dc])
			}
			upIn = merged
			upC = sc + dc
		} else {
			upIn = up
			upC = 1
		}
	}

	// up7: Conv2D(4x4, dilation=2, padding="same", Cin=1, Cout=2) + sigmoid * inputMag
	// Effective kernel = 7 -> pad_top = 3, pad_left = 3.
	H, W := spleeterTimeFrames, spleeterFreqBins
	out := make([]float32, H*W*2)
	b0, b1 := u.outB[0], u.outB[1]
	w := u.outW // [4, 4, 1, 2] -> 16 * 2
	for oy := 0; oy < H; oy++ {
		for ox := 0; ox < W; ox++ {
			acc0, acc1 := b0, b1
			p := 0
			for ky := 0; ky < 4; ky++ {
				iy := oy + ky*2 - 3
				if iy < 0 || iy >= H {
					p += 8
					continue
				}
				rowOff := iy * W
				for kx := 0; kx < 4; kx++ {
					ix := ox + kx*2 - 3
					if ix >= 0 && ix < W {
						v := upIn[rowOff+ix]
						acc0 += v * w[p]
						acc1 += v * w[p+1]
					}
					p += 2
				}
			}
			idx := (oy*W + ox) * 2
			sig0 := float32(1.0 / (1.0 + math.Exp(-float64(acc0))))
			sig1 := float32(1.0 / (1.0 + math.Exp(-float64(acc1))))
			out[idx] = sig0 * inputMag[idx]
			out[idx+1] = sig1 * inputMag[idx+1]
		}
	}
	return out
}

// Separate separates planar [channels][frames] audio at sampleRate into
// EngineX's canonical [4][channels][frames] stems: vocals, bass, drums, melody.
func (m *SpleeterModel) Separate(ctx context.Context, x [][]float32, sampleRate float64) ([][][]float32, error) {
	channels := len(x)
	if channels != 1 && channels != 2 {
		return nil, errors.New("spleeter: channels must be 1 or 2")
	}
	origLen := len(x[0])
	if origLen == 0 {
		return emptyStems(channels), nil
	}

	// Prepare stereo 44.1 kHz input
	targetLen := origLen
	if math.Abs(sampleRate-spleeterSampleRate) > 1e-3 {
		targetLen = max(1, int(math.Round(float64(origLen)*spleeterSampleRate/sampleRate)))
	}
	var stereo [2][]float32
	if math.Abs(sampleRate-spleeterSampleRate) > 1e-3 {
		stereo[0] = resampleChannel(x[0], sampleRate, spleeterSampleRate, targetLen)
		if channels == 2 {
			stereo[1] = resampleChannel(x[1], sampleRate, spleeterSampleRate, targetLen)
		} else {
			stereo[1] = slicesCloneF32(stereo[0])
		}
	} else {
		stereo[0] = slicesCloneF32(x[0])
		if channels == 2 {
			stereo[1] = slicesCloneF32(x[1])
		} else {
			stereo[1] = slicesCloneF32(stereo[0])
		}
	}

	// Forward STFT matching deezer/spleeter _build_stft_feature:
	// prepend 4096 zeros, pad_end=True so ceil((len - 4096)/1024)+1 frames cover all samples.
	leadPad := spleeterFFTSize
	stftInLen := leadPad + targetLen
	nFrames := 1
	if stftInLen > spleeterFFTSize {
		nFrames = (stftInLen-spleeterFFTSize+spleeterHop-1)/spleeterHop + 1
	}
	paddedLen := (nFrames-1)*spleeterHop + spleeterFFTSize
	var padded [2][]float32
	for c := 0; c < 2; c++ {
		padded[c] = make([]float32, paddedLen)
		copy(padded[c][leadPad:leadPad+targetLen], stereo[c])
	}

	// Compute complex STFT [nFrames][2][spleeterBins]
	stftRe := make([]float32, nFrames*2*spleeterBins)
	stftIm := make([]float32, nFrames*2*spleeterBins)
	winBuf := make([]float32, spleeterFFTSize)
	scratch := make([]float64, spleeterFFTSize)
	for f := 0; f < nFrames; f++ {
		off := f * spleeterHop
		for c := 0; c < 2; c++ {
			src := padded[c][off : off+spleeterFFTSize]
			for k := 0; k < spleeterFFTSize; k++ {
				winBuf[k] = src[k] * m.window[k]
			}
			base := (f*2 + c) * spleeterBins
			m.fft.rfft(winBuf, stftRe[base:base+spleeterBins], stftIm[base:base+spleeterBins], scratch)
		}
	}

	// Partition into T=512 windows and predict vocals(0), drums(1), bass(2), other(3)
	nWin := (nFrames + spleeterTimeFrames - 1) / spleeterTimeFrames
	// Only rebuild 3 stems (vocals=0, bass=2, drums=1); melody is exact remainder!
	var stemWaves44k [3][2][]float32
	for i := 0; i < 3; i++ {
		for c := 0; c < 2; c++ {
			stemWaves44k[i][c] = make([]float32, paddedLen)
		}
	}

	// Masks for vocals(net 0), bass(net 2), drums(net 1): [3][nFrames][spleeterFreqBins][2]
	masks := [3][]float32{
		make([]float32, nFrames*spleeterFreqBins*2),
		make([]float32, nFrames*spleeterFreqBins*2),
		make([]float32, nFrames*spleeterFreqBins*2),
	}

	workers := m.workers
	if workers < 1 {
		workers = 1
	}
	for w := 0; w < nWin; w++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f0 := w * spleeterTimeFrames
		cnt := min(spleeterTimeFrames, nFrames-f0)
		winMag := make([]float32, spleeterTimeFrames*spleeterFreqBins*2)
		for t := 0; t < cnt; t++ {
			for b := 0; b < spleeterFreqBins; b++ {
				idxL := ((f0+t)*2+0)*spleeterBins + b
				idxR := ((f0+t)*2+1)*spleeterBins + b
				dst := (t*spleeterFreqBins + b) * 2
				winMag[dst] = float32(math.Hypot(float64(stftRe[idxL]), float64(stftIm[idxL])))
				winMag[dst+1] = float32(math.Hypot(float64(stftRe[idxR]), float64(stftIm[idxR])))
			}
		}

		var preds [4][]float32
		if workers >= 4 {
			subW := max(1, workers/4)
			var wg sync.WaitGroup
			for s := 0; s < 4; s++ {
				wg.Add(1)
				go func(s int) {
					defer wg.Done()
					preds[s] = runSpleeterUNet(&m.nets[s], winMag, subW)
				}(s)
			}
			wg.Wait()
		} else {
			for s := 0; s < 4; s++ {
				preds[s] = runSpleeterUNet(&m.nets[s], winMag, workers)
			}
		}

		// L2 ratio mask: (e_s^2 + eps/4) / (sum_j e_j^2 + eps)
		// Map Spleeter nets -> canonical: 0=vocals(net 0), 1=bass(net 2), 2=drums(net 1)
		netOrder := [3]int{0, 2, 1}
		limit := cnt * spleeterFreqBins * 2
		dstOff := f0 * spleeterFreqBins * 2
		for idx := 0; idx < limit; idx++ {
			p0 := float64(preds[0][idx])
			p1 := float64(preds[1][idx])
			p2 := float64(preds[2][idx])
			p3 := float64(preds[3][idx])
			e0, e1, e2, e3 := p0*p0, p1*p1, p2*p2, p3*p3
			denom := e0 + e1 + e2 + e3 + spleeterMaskEps
			vals := [4]float64{e0, e1, e2, e3}
			for k, netIdx := range netOrder {
				masks[k][dstOff+idx] = float32((vals[netIdx] + spleeterMaskEps*0.25) / denom)
			}
		}
	}

	// Inverse STFT for vocals, bass, drums (mask_extension="zeros" -> bins >= 1024 are 0)
	sRe := make([]float32, spleeterBins)
	sIm := make([]float32, spleeterBins)
	timeFrame := make([]float32, spleeterFFTSize)
	for st := 0; st < 3; st++ {
		for c := 0; c < 2; c++ {
			clear(sRe)
			clear(sIm)
			dstWave := stemWaves44k[st][c]
			for f := 0; f < nFrames; f++ {
				srcBase := (f*2 + c) * spleeterBins
				mBase := f * spleeterFreqBins * 2
				for b := 0; b < spleeterFreqBins; b++ {
					mv := masks[st][mBase+b*2+c]
					sRe[b] = stftRe[srcBase+b] * mv
					sIm[b] = stftIm[srcBase+b] * mv
				}
				m.fft.irfft(sRe, sIm, timeFrame, scratch)
				off := f * spleeterHop
				for k := 0; k < spleeterFFTSize; k++ {
					dstWave[off+k] += timeFrame[k] * m.window[k] * spleeterWinComp
				}
			}
		}
	}

	// Crop [leadPad : leadPad+targetLen], convert back to original channels & sampleRate,
	// and rebuild melody = x - vocals - bass - drums so all 4 stems sum to x!
	out := make([][][]float32, 4)
	for st := 0; st < 4; st++ {
		out[st] = make([][]float32, channels)
	}
	for st := 0; st < 3; st++ {
		if channels == 1 {
			mono44 := make([]float32, targetLen)
			l := stemWaves44k[st][0][leadPad : leadPad+targetLen]
			r := stemWaves44k[st][1][leadPad : leadPad+targetLen]
			for i := 0; i < targetLen; i++ {
				mono44[i] = 0.5 * (l[i] + r[i])
			}
			if math.Abs(sampleRate-spleeterSampleRate) > 1e-3 {
				out[st][0] = resampleChannel(mono44, spleeterSampleRate, sampleRate, origLen)
			} else {
				out[st][0] = mono44
			}
		} else {
			for c := 0; c < 2; c++ {
				cropped := stemWaves44k[st][c][leadPad : leadPad+targetLen]
				if math.Abs(sampleRate-spleeterSampleRate) > 1e-3 {
					out[st][c] = resampleChannel(cropped, spleeterSampleRate, sampleRate, origLen)
				} else {
					cp := make([]float32, origLen)
					copy(cp, cropped)
					out[st][c] = cp
				}
			}
		}
	}
	for c := 0; c < channels; c++ {
		mel := make([]float32, origLen)
		inC := x[c]
		vC, bC, dC := out[0][c], out[1][c], out[2][c]
		for i := 0; i < origLen; i++ {
			mel[i] = inC[i] - vC[i] - bC[i] - dC[i]
		}
		out[3][c] = mel
	}
	return out, nil
}

// ChunkedSeparator wraps any whole-segment separator (Spleeter or HTDemucs) into
// an overlap-crossfaded StreamSeparator with exact sum-to-input preservation.
type ChunkedSeparator struct {
	channels int
	rate     float64
	size     int
	overlap  int
	separate func(ctx context.Context, x [][]float32, rate float64) ([][][]float32, error)
	fade     []float32
	pending  [][]float32
	tail     [][][]float32
	tailMix  [][]float32
	received int
	returned int
	finished bool
}

func NewChunkedSeparator(channels int, sampleRate, chunkSec, overlapSec float64, separate func(context.Context, [][]float32, float64) ([][][]float32, error)) (*ChunkedSeparator, error) {
	if channels < 1 || channels > 2 || !(sampleRate >= 8000 && sampleRate <= nativeMaxRate) {
		return nil, errors.New("chunked separator: invalid channels or sample rate")
	}
	size := max(4, int(math.Round(chunkSec*sampleRate)))
	overlap := min(int(math.Round(overlapSec*sampleRate)), size/2)
	fade := make([]float32, overlap)
	denom := float64(max(overlap, 1))
	for i := 0; i < overlap; i++ {
		fade[i] = float32((float64(i) + 0.5) / denom)
	}
	return &ChunkedSeparator{
		channels: channels,
		rate:     sampleRate,
		size:     size,
		overlap:  overlap,
		separate: separate,
		fade:     fade,
		pending:  make([][]float32, channels),
	}, nil
}

func (cs *ChunkedSeparator) runWindow(ctx context.Context, win [][]float32) ([][][]float32, error) {
	y, err := cs.separate(ctx, win, cs.rate)
	if err != nil {
		return nil, err
	}
	if cs.tail != nil && cs.overlap > 0 {
		o := cs.overlap
		for st := 0; st < 3; st++ {
			for c := 0; c < cs.channels; c++ {
				for i := 0; i < o; i++ {
					f := cs.fade[i]
					y[st][c][i] = cs.tail[st][c][i]*(1.0-f) + y[st][c][i]*f
				}
			}
		}
		for c := 0; c < cs.channels; c++ {
			for i := 0; i < o; i++ {
				y[3][c][i] = win[c][i] - y[0][c][i] - y[1][c][i] - y[2][c][i]
			}
		}
	}
	return y, nil
}

func (cs *ChunkedSeparator) Feed(ctx context.Context, x [][]float32) ([][][]float32, error) {
	if cs.finished {
		return nil, errors.New("chunked separator: PCM after drain")
	}
	for c := 0; c < cs.channels; c++ {
		for _, v := range x[c] {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return nil, errors.New("chunked separator: non-finite PCM")
			}
		}
		cs.pending[c] = append(cs.pending[c], x[c]...)
	}
	cs.received += len(x[0])
	hop := cs.size - cs.overlap
	out := emptyStems(cs.channels)
	for len(cs.pending[0]) >= cs.size {
		win := make([][]float32, cs.channels)
		for c := 0; c < cs.channels; c++ {
			win[c] = cs.pending[c][:cs.size]
		}
		y, err := cs.runWindow(ctx, win)
		if err != nil {
			return nil, err
		}
		cs.tail = make([][][]float32, 4)
		for st := 0; st < 4; st++ {
			cs.tail[st] = make([][]float32, cs.channels)
			for c := 0; c < cs.channels; c++ {
				out[st][c] = append(out[st][c], y[st][c][:hop]...)
				cs.tail[st][c] = slicesCloneF32(y[st][c][hop:])
			}
		}
		for c := 0; c < cs.channels; c++ {
			cs.pending[c] = cs.pending[c][hop:]
		}
	}
	cs.returned += len(out[0][0])
	return out, nil
}

func (cs *ChunkedSeparator) Drain(ctx context.Context) ([][][]float32, error) {
	if cs.finished {
		return emptyStems(cs.channels), nil
	}
	cs.finished = true
	left := len(cs.pending[0])
	out := emptyStems(cs.channels)
	if left > 0 {
		win := make([][]float32, cs.channels)
		for c := 0; c < cs.channels; c++ {
			win[c] = make([]float32, max(left, cs.size))
			copy(win[c], cs.pending[c])
		}
		y, err := cs.runWindow(ctx, win)
		if err != nil {
			return nil, err
		}
		for st := 0; st < 4; st++ {
			for c := 0; c < cs.channels; c++ {
				out[st][c] = append(out[st][c], y[st][c][:left]...)
			}
		}
		// Ensure exact sum to unpadded pending samples
		for c := 0; c < cs.channels; c++ {
			for i := 0; i < left; i++ {
				out[3][c][i] = cs.pending[c][i] - out[0][c][i] - out[1][c][i] - out[2][c][i]
			}
		}
	}
	cs.returned += left
	if cs.returned != cs.received {
		return nil, fmt.Errorf("chunked separator: drain frame mismatch (%d != %d)", cs.returned, cs.received)
	}
	return out, nil
}
