package stems

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
)

const (
	htdSampleRate  = 44100.0
	htdFFTSize     = 4096
	htdHop         = 1024
	htdFreqBins    = htdFFTSize / 2 // 2048
	htdDepth       = 4
	htdChannels    = 48
	htdGrowth      = 2
	htdKernel      = 8
	htdStride      = 4
	htdTLayers     = 5
	htdTHeads      = 8
	htdBottomCh    = 512
	htdTrainLength = 343980 // int(39/5 * 44100) = 7.8 s
)

type htdConv1D struct {
	cin, cout, k, stride, pad, dil int
	w                              []float32 // packed [cout, cin*k] for row-major output, or [cin*k, cout] for sgemm
	b                              []float32 // [cout]
}

type htdConvTr1D struct {
	cin, cout, k, stride int
	w                    []float32 // packed [cin, k*cout]
	b                    []float32 // [cout]
}

type htdConv2D struct {
	cin, cout, kh, kw, sh, sw, ph, pw int
	w                                 []float32 // packed [kh*kw*cin, cout]
	b                                 []float32 // [cout]
}

type htdConvTr2D struct {
	cin, cout, kh, kw, sh, sw int
	w                         []float32 // packed [cin, kh*kw*cout]
	b                         []float32 // [cout]
}

type htdGroupNorm1 struct {
	w []float32
	b []float32
}

type htdLayerNorm struct {
	w []float32
	b []float32
}

type htdDConvSub struct {
	conv1 htdConv1D
	norm1 htdGroupNorm1
	conv2 htdConv1D
	norm2 htdGroupNorm1
	scale []float32
}

type htdDConv struct {
	subs [2]htdDConvSub
}

type htdHEnc struct {
	conv    htdConv2D
	dconv   htdDConv
	rewrite htdConv2D
}

type htdTEnc struct {
	conv    htdConv1D
	dconv   htdDConv
	rewrite htdConv1D
}

type htdHDec struct {
	rewrite htdConv2D
	dconv   htdDConv
	convTr  htdConvTr2D
	last    bool
}

type htdTDec struct {
	rewrite htdConv1D
	dconv   htdDConv
	convTr  htdConvTr1D
	last    bool
}

type htdLinear struct {
	inDim, outDim int
	w             []float32 // packed [inDim, outDim]
	b             []float32 // [outDim]
}

type htdMHA struct {
	dim, heads int
	q, k, v    htdLinear
	out        htdLinear
}

type htdTransLayer struct {
	cross   bool
	norm1   htdLayerNorm
	norm2   htdLayerNorm // for cross: key norm; for self: ffn norm
	norm3   htdLayerNorm // for cross: ffn norm
	attn    htdMHA
	gamma1  []float32
	lin1    htdLinear
	lin2    htdLinear
	gamma2  []float32
	normOut htdGroupNorm1
}

// HTDemucsModel implements Meta's MIT 4-stem Hybrid Transformer Demucs v4 (signature 955717e8).
type HTDemucsModel struct {
	enc       [htdDepth]htdHEnc
	tenc      [htdDepth]htdTEnc
	dec       [htdDepth]htdHDec
	tdec      [htdDepth]htdTDec
	freqEmb   []float32 // [512, 48] pre-scaled by 10.0 * 0.2 = 2.0
	chUp      htdConv1D
	chDown    htdConv1D
	chUpT     htdConv1D
	chDownT   htdConv1D
	normIn    htdLayerNorm
	normInT   htdLayerNorm
	layers    [htdTLayers]htdTransLayer
	layersT   [htdTLayers]htdTransLayer
	fft       *radix2FFT
	window    []float32
	workers   int
	segLength int // defaults to htdTrainLength (343980); smaller allowed in unit tests
}

// LoadHTDemucsSafetensors loads HTDemucs v4 weights from an F16/F32 safetensors file.
func LoadHTDemucsSafetensors(path string) (*HTDemucsModel, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tensors, err := parseSafetensors(raw)
	if err != nil {
		return nil, err
	}
	// Detect optional signature prefix (e.g. "955717e8.")
	pfx := ""
	for k := range tensors {
		if strings.HasSuffix(k, "freq_emb.embedding.weight") {
			pfx = strings.TrimSuffix(k, "freq_emb.embedding.weight")
			break
		}
	}
	return buildHTDemucs(tensors, pfx)
}

func getTensor(t map[string]tensorView, name string, wantLen int) ([]float32, error) {
	v, ok := t[name]
	if !ok {
		return nil, fmt.Errorf("htdemucs: missing tensor %s", name)
	}
	if wantLen >= 0 && len(v.Data) != wantLen {
		return nil, fmt.Errorf("htdemucs: tensor %s len=%d, want %d", name, len(v.Data), wantLen)
	}
	return v.Data, nil
}

// PyTorch Conv1d weight [cout, cin, k] -> pack [cin*k, cout] for sgemm!
func packPTConv1D(t map[string]tensorView, prefix string, cin, cout, k, stride, pad, dil int) (htdConv1D, error) {
	wraw, err := getTensor(t, prefix+".weight", cout*cin*k)
	if err != nil {
		return htdConv1D{}, err
	}
	braw, err := getTensor(t, prefix+".bias", cout)
	if err != nil {
		return htdConv1D{}, err
	}
	packed := make([]float32, cin*k*cout)
	for co := 0; co < cout; co++ {
		for ci := 0; ci < cin; ci++ {
			for ki := 0; ki < k; ki++ {
				packed[(ki*cin+ci)*cout+co] = wraw[(co*cin+ci)*k+ki]
			}
		}
	}
	return htdConv1D{cin: cin, cout: cout, k: k, stride: stride, pad: pad, dil: dil, w: packed, b: slicesCloneF32(braw)}, nil
}

// PyTorch ConvTranspose1d weight [cin, cout, k] -> pack [cin, k*cout] for sgemm!
func packPTConvTr1D(t map[string]tensorView, prefix string, cin, cout, k, stride int) (htdConvTr1D, error) {
	wraw, err := getTensor(t, prefix+".weight", cin*cout*k)
	if err != nil {
		return htdConvTr1D{}, err
	}
	braw, err := getTensor(t, prefix+".bias", cout)
	if err != nil {
		return htdConvTr1D{}, err
	}
	packed := make([]float32, cin*k*cout)
	for ci := 0; ci < cin; ci++ {
		for co := 0; co < cout; co++ {
			for ki := 0; ki < k; ki++ {
				packed[ci*(k*cout)+ki*cout+co] = wraw[(ci*cout+co)*k+ki]
			}
		}
	}
	return htdConvTr1D{cin: cin, cout: cout, k: k, stride: stride, w: packed, b: slicesCloneF32(braw)}, nil
}

// PyTorch Conv2d weight [cout, cin, kh, kw] -> pack [kh*kw*cin, cout] for sgemm!
func packPTConv2D(t map[string]tensorView, prefix string, cin, cout, kh, kw, sh, sw, ph, pw int) (htdConv2D, error) {
	wraw, err := getTensor(t, prefix+".weight", cout*cin*kh*kw)
	if err != nil {
		return htdConv2D{}, err
	}
	braw, err := getTensor(t, prefix+".bias", cout)
	if err != nil {
		return htdConv2D{}, err
	}
	packed := make([]float32, kh*kw*cin*cout)
	for co := 0; co < cout; co++ {
		for ci := 0; ci < cin; ci++ {
			for y := 0; y < kh; y++ {
				for x := 0; x < kw; x++ {
					packed[((y*kw+x)*cin+ci)*cout+co] = wraw[((co*cin+ci)*kh+y)*kw+x]
				}
			}
		}
	}
	return htdConv2D{cin: cin, cout: cout, kh: kh, kw: kw, sh: sh, sw: sw, ph: ph, pw: pw, w: packed, b: slicesCloneF32(braw)}, nil
}

// PyTorch ConvTranspose2d weight [cin, cout, kh, kw] -> pack [cin, kh*kw*cout] for sgemm!
func packPTConvTr2D(t map[string]tensorView, prefix string, cin, cout, kh, kw, sh, sw int) (htdConvTr2D, error) {
	wraw, err := getTensor(t, prefix+".weight", cin*cout*kh*kw)
	if err != nil {
		return htdConvTr2D{}, err
	}
	braw, err := getTensor(t, prefix+".bias", cout)
	if err != nil {
		return htdConvTr2D{}, err
	}
	packed := make([]float32, cin*kh*kw*cout)
	for ci := 0; ci < cin; ci++ {
		for co := 0; co < cout; co++ {
			for y := 0; y < kh; y++ {
				for x := 0; x < kw; x++ {
					packed[ci*(kh*kw*cout)+(y*kw+x)*cout+co] = wraw[((ci*cout+co)*kh+y)*kw+x]
				}
			}
		}
	}
	return htdConvTr2D{cin: cin, cout: cout, kh: kh, kw: kw, sh: sh, sw: sw, w: packed, b: slicesCloneF32(braw)}, nil
}

func packPTLinear(wraw, braw []float32, inDim, outDim int) htdLinear {
	packed := make([]float32, inDim*outDim)
	for o := 0; o < outDim; o++ {
		for i := 0; i < inDim; i++ {
			packed[i*outDim+o] = wraw[o*inDim+i]
		}
	}
	return htdLinear{inDim: inDim, outDim: outDim, w: packed, b: slicesCloneF32(braw)}
}

func loadHTDDConv(t map[string]tensorView, prefix string, ch int) (htdDConv, error) {
	var dc htdDConv
	hid := ch / 8
	for d := 0; d < 2; d++ {
		dil := 1 << d
		sp := fmt.Sprintf("%s.dconv.layers.%d", prefix, d)
		c1, err := packPTConv1D(t, sp+".0", ch, hid, 3, 1, dil, dil)
		if err != nil {
			return htdDConv{}, err
		}
		n1w, err := getTensor(t, sp+".1.weight", hid)
		if err != nil {
			return htdDConv{}, err
		}
		n1b, err := getTensor(t, sp+".1.bias", hid)
		if err != nil {
			return htdDConv{}, err
		}
		c2, err := packPTConv1D(t, sp+".3", hid, 2*ch, 1, 1, 0, 1)
		if err != nil {
			return htdDConv{}, err
		}
		n2w, err := getTensor(t, sp+".4.weight", 2*ch)
		if err != nil {
			return htdDConv{}, err
		}
		n2b, err := getTensor(t, sp+".4.bias", 2*ch)
		if err != nil {
			return htdDConv{}, err
		}
		sc, err := getTensor(t, sp+".6.scale", ch)
		if err != nil {
			return htdDConv{}, err
		}
		dc.subs[d] = htdDConvSub{
			conv1: c1,
			norm1: htdGroupNorm1{w: slicesCloneF32(n1w), b: slicesCloneF32(n1b)},
			conv2: c2,
			norm2: htdGroupNorm1{w: slicesCloneF32(n2w), b: slicesCloneF32(n2b)},
			scale: slicesCloneF32(sc),
		}
	}
	return dc, nil
}

func loadHTDTransLayer(t map[string]tensorView, prefix string, cross bool, dim, heads, ffn int) (htdTransLayer, error) {
	var l htdTransLayer
	l.cross = cross
	loadLN := func(sub string) (htdLayerNorm, error) {
		w, err := getTensor(t, prefix+"."+sub+".weight", dim)
		if err != nil {
			return htdLayerNorm{}, err
		}
		b, err := getTensor(t, prefix+"."+sub+".bias", dim)
		if err != nil {
			return htdLayerNorm{}, err
		}
		return htdLayerNorm{w: slicesCloneF32(w), b: slicesCloneF32(b)}, nil
	}
	var err error
	if l.norm1, err = loadLN("norm1"); err != nil {
		return l, err
	}
	if l.norm2, err = loadLN("norm2"); err != nil {
		return l, err
	}
	attnSub := "self_attn"
	if cross {
		attnSub = "cross_attn"
		if l.norm3, err = loadLN("norm3"); err != nil {
			return l, err
		}
	}
	inW, err := getTensor(t, prefix+"."+attnSub+".in_proj_weight", 3*dim*dim)
	if err != nil {
		return l, err
	}
	inB, err := getTensor(t, prefix+"."+attnSub+".in_proj_bias", 3*dim)
	if err != nil {
		return l, err
	}
	outW, err := getTensor(t, prefix+"."+attnSub+".out_proj.weight", dim*dim)
	if err != nil {
		return l, err
	}
	outB, err := getTensor(t, prefix+"."+attnSub+".out_proj.bias", dim)
	if err != nil {
		return l, err
	}
	l.attn = htdMHA{
		dim:   dim,
		heads: heads,
		q:     packPTLinear(inW[:dim*dim], inB[:dim], dim, dim),
		k:     packPTLinear(inW[dim*dim:2*dim*dim], inB[dim:2*dim], dim, dim),
		v:     packPTLinear(inW[2*dim*dim:], inB[2*dim:], dim, dim),
		out:   packPTLinear(outW, outB, dim, dim),
	}
	g1, err := getTensor(t, prefix+".gamma_1.scale", dim)
	if err != nil {
		return l, err
	}
	g2, err := getTensor(t, prefix+".gamma_2.scale", dim)
	if err != nil {
		return l, err
	}
	l.gamma1, l.gamma2 = slicesCloneF32(g1), slicesCloneF32(g2)
	l1w, err := getTensor(t, prefix+".linear1.weight", ffn*dim)
	if err != nil {
		return l, err
	}
	l1b, err := getTensor(t, prefix+".linear1.bias", ffn)
	if err != nil {
		return l, err
	}
	l2w, err := getTensor(t, prefix+".linear2.weight", dim*ffn)
	if err != nil {
		return l, err
	}
	l2b, err := getTensor(t, prefix+".linear2.bias", dim)
	if err != nil {
		return l, err
	}
	l.lin1 = packPTLinear(l1w, l1b, dim, ffn)
	l.lin2 = packPTLinear(l2w, l2b, ffn, dim)
	now, err := getTensor(t, prefix+".norm_out.weight", dim)
	if err != nil {
		return l, err
	}
	nob, err := getTensor(t, prefix+".norm_out.bias", dim)
	if err != nil {
		return l, err
	}
	l.normOut = htdGroupNorm1{w: slicesCloneF32(now), b: slicesCloneF32(nob)}
	return l, nil
}

func buildHTDemucs(t map[string]tensorView, pfx string) (*HTDemucsModel, error) {
	m := &HTDemucsModel{
		fft:       newRadix2FFT(htdFFTSize),
		window:    hannPeriodic(htdFFTSize),
		workers:   defaultWorkers(),
		segLength: htdTrainLength,
	}
	fIn := [4]int{4, 48, 96, 192}
	tIn := [4]int{2, 48, 96, 192}
	chOut := [4]int{48, 96, 192, 384}
	for i := 0; i < 4; i++ {
		ep := fmt.Sprintf("%sencoder.%d", pfx, i)
		c, err := packPTConv2D(t, ep+".conv", fIn[i], chOut[i], 8, 1, 4, 1, 2, 0)
		if err != nil {
			return nil, err
		}
		dc, err := loadHTDDConv(t, ep, chOut[i])
		if err != nil {
			return nil, err
		}
		rw, err := packPTConv2D(t, ep+".rewrite", chOut[i], 2*chOut[i], 1, 1, 1, 1, 0, 0)
		if err != nil {
			return nil, err
		}
		m.enc[i] = htdHEnc{conv: c, dconv: dc, rewrite: rw}

		tep := fmt.Sprintf("%stencoder.%d", pfx, i)
		tc, err := packPTConv1D(t, tep+".conv", tIn[i], chOut[i], 8, 4, 2, 1)
		if err != nil {
			return nil, err
		}
		tdc, err := loadHTDDConv(t, tep, chOut[i])
		if err != nil {
			return nil, err
		}
		trw, err := packPTConv1D(t, tep+".rewrite", chOut[i], 2*chOut[i], 1, 1, 0, 1)
		if err != nil {
			return nil, err
		}
		m.tenc[i] = htdTEnc{conv: tc, dconv: tdc, rewrite: trw}
	}

	dIn := [4]int{384, 192, 96, 48}
	fdOut := [4]int{192, 96, 48, 16}
	tdOut := [4]int{192, 96, 48, 8}
	for i := 0; i < 4; i++ {
		dp := fmt.Sprintf("%sdecoder.%d", pfx, i)
		rw, err := packPTConv2D(t, dp+".rewrite", dIn[i], 2*dIn[i], 3, 3, 1, 1, 1, 1)
		if err != nil {
			return nil, err
		}
		dc, err := loadHTDDConv(t, dp, dIn[i])
		if err != nil {
			return nil, err
		}
		ctr, err := packPTConvTr2D(t, dp+".conv_tr", dIn[i], fdOut[i], 8, 1, 4, 1)
		if err != nil {
			return nil, err
		}
		m.dec[i] = htdHDec{rewrite: rw, dconv: dc, convTr: ctr, last: i == 3}

		tdp := fmt.Sprintf("%stdecoder.%d", pfx, i)
		trw, err := packPTConv1D(t, tdp+".rewrite", dIn[i], 2*dIn[i], 3, 1, 1, 1)
		if err != nil {
			return nil, err
		}
		tdc, err := loadHTDDConv(t, tdp, dIn[i])
		if err != nil {
			return nil, err
		}
		tctr, err := packPTConvTr1D(t, tdp+".conv_tr", dIn[i], tdOut[i], 8, 4)
		if err != nil {
			return nil, err
		}
		m.tdec[i] = htdTDec{rewrite: trw, dconv: tdc, convTr: tctr, last: i == 3}
	}

	fe, err := getTensor(t, pfx+"freq_emb.embedding.weight", 512*48)
	if err != nil {
		return nil, err
	}
	m.freqEmb = make([]float32, len(fe))
	for i, v := range fe {
		m.freqEmb[i] = v * 2.0 // ScaledEmbedding(scale=10) * freq_emb_scale(0.2) = 2.0
	}

	var errUp error
	if m.chUp, errUp = packPTConv1D(t, pfx+"channel_upsampler", 384, 512, 1, 1, 0, 1); errUp != nil {
		return nil, errUp
	}
	if m.chDown, errUp = packPTConv1D(t, pfx+"channel_downsampler", 512, 384, 1, 1, 0, 1); errUp != nil {
		return nil, errUp
	}
	if m.chUpT, errUp = packPTConv1D(t, pfx+"channel_upsampler_t", 384, 512, 1, 1, 0, 1); errUp != nil {
		return nil, errUp
	}
	if m.chDownT, errUp = packPTConv1D(t, pfx+"channel_downsampler_t", 512, 384, 1, 1, 0, 1); errUp != nil {
		return nil, errUp
	}
	niW, err := getTensor(t, pfx+"crosstransformer.norm_in.weight", 512)
	if err != nil {
		return nil, err
	}
	niB, err := getTensor(t, pfx+"crosstransformer.norm_in.bias", 512)
	if err != nil {
		return nil, err
	}
	m.normIn = htdLayerNorm{w: slicesCloneF32(niW), b: slicesCloneF32(niB)}
	nitW, err := getTensor(t, pfx+"crosstransformer.norm_in_t.weight", 512)
	if err != nil {
		return nil, err
	}
	nitB, err := getTensor(t, pfx+"crosstransformer.norm_in_t.bias", 512)
	if err != nil {
		return nil, err
	}
	m.normInT = htdLayerNorm{w: slicesCloneF32(nitW), b: slicesCloneF32(nitB)}

	for i := 0; i < htdTLayers; i++ {
		cross := i%2 == 1
		l, err := loadHTDTransLayer(t, fmt.Sprintf("%scrosstransformer.layers.%d", pfx, i), cross, 512, 8, 2048)
		if err != nil {
			return nil, err
		}
		lt, err := loadHTDTransLayer(t, fmt.Sprintf("%scrosstransformer.layers_t.%d", pfx, i), cross, 512, 8, 2048)
		if err != nil {
			return nil, err
		}
		m.layers[i] = l
		m.layersT[i] = lt
	}
	return m, nil
}

// NewTestHTDemucsModel builds a lightweight synthetic HTDemucsModel with a short
// segment length (e.g. 16384 samples) for fast deterministic unit tests.
func NewTestHTDemucsModel(segLength int) *HTDemucsModel {
	if segLength <= 0 {
		segLength = 16384
	}
	ones := func(n int) []float32 {
		s := make([]float32, n)
		for i := range s {
			s[i] = 1
		}
		return s
	}
	mkDC := func(ch int) htdDConv {
		var dc htdDConv
		hid := ch / 8
		for d := 0; d < 2; d++ {
			dil := 1 << d
			dc.subs[d] = htdDConvSub{
				conv1: htdConv1D{cin: ch, cout: hid, k: 3, stride: 1, pad: dil, dil: dil, w: make([]float32, ch*3*hid), b: make([]float32, hid)},
				norm1: htdGroupNorm1{w: ones(hid), b: make([]float32, hid)},
				conv2: htdConv1D{cin: hid, cout: 2 * ch, k: 1, stride: 1, pad: 0, dil: 1, w: make([]float32, hid*2*ch), b: make([]float32, 2*ch)},
				norm2: htdGroupNorm1{w: ones(2 * ch), b: make([]float32, 2*ch)},
				scale: make([]float32, ch),
			}
		}
		return dc
	}
	m := &HTDemucsModel{
		fft:       newRadix2FFT(htdFFTSize),
		window:    hannPeriodic(htdFFTSize),
		workers:   defaultWorkers(),
		segLength: segLength,
		freqEmb:   make([]float32, 512*48),
		chUp:      htdConv1D{cin: 384, cout: 512, k: 1, stride: 1, w: make([]float32, 384*512), b: make([]float32, 512)},
		chDown:    htdConv1D{cin: 512, cout: 384, k: 1, stride: 1, w: make([]float32, 512*384), b: make([]float32, 384)},
		chUpT:     htdConv1D{cin: 384, cout: 512, k: 1, stride: 1, w: make([]float32, 384*512), b: make([]float32, 512)},
		chDownT:   htdConv1D{cin: 512, cout: 384, k: 1, stride: 1, w: make([]float32, 512*384), b: make([]float32, 384)},
		normIn:    htdLayerNorm{w: ones(512), b: make([]float32, 512)},
		normInT:   htdLayerNorm{w: ones(512), b: make([]float32, 512)},
	}
	fIn := [4]int{4, 48, 96, 192}
	tIn := [4]int{2, 48, 96, 192}
	chOut := [4]int{48, 96, 192, 384}
	for i := 0; i < 4; i++ {
		m.enc[i] = htdHEnc{
			conv:    htdConv2D{cin: fIn[i], cout: chOut[i], kh: 8, kw: 1, sh: 4, sw: 1, ph: 2, pw: 0, w: make([]float32, 8*fIn[i]*chOut[i]), b: make([]float32, chOut[i])},
			dconv:   mkDC(chOut[i]),
			rewrite: htdConv2D{cin: chOut[i], cout: 2 * chOut[i], kh: 1, kw: 1, sh: 1, sw: 1, w: make([]float32, chOut[i]*2*chOut[i]), b: make([]float32, 2*chOut[i])},
		}
		m.tenc[i] = htdTEnc{
			conv:    htdConv1D{cin: tIn[i], cout: chOut[i], k: 8, stride: 4, pad: 2, dil: 1, w: make([]float32, 8*tIn[i]*chOut[i]), b: make([]float32, chOut[i])},
			dconv:   mkDC(chOut[i]),
			rewrite: htdConv1D{cin: chOut[i], cout: 2 * chOut[i], k: 1, stride: 1, dil: 1, w: make([]float32, chOut[i]*2*chOut[i]), b: make([]float32, 2*chOut[i])},
		}
	}
	dIn := [4]int{384, 192, 96, 48}
	fdOut := [4]int{192, 96, 48, 16}
	tdOut := [4]int{192, 96, 48, 8}
	for i := 0; i < 4; i++ {
		m.dec[i] = htdHDec{
			rewrite: htdConv2D{cin: dIn[i], cout: 2 * dIn[i], kh: 3, kw: 3, sh: 1, sw: 1, ph: 1, pw: 1, w: make([]float32, 9*dIn[i]*2*dIn[i]), b: make([]float32, 2*dIn[i])},
			dconv:   mkDC(dIn[i]),
			convTr:  htdConvTr2D{cin: dIn[i], cout: fdOut[i], kh: 8, kw: 1, sh: 4, sw: 1, w: make([]float32, dIn[i]*8*fdOut[i]), b: make([]float32, fdOut[i])},
			last:    i == 3,
		}
		m.tdec[i] = htdTDec{
			rewrite: htdConv1D{cin: dIn[i], cout: 2 * dIn[i], k: 3, stride: 1, pad: 1, dil: 1, w: make([]float32, 3*dIn[i]*2*dIn[i]), b: make([]float32, 2*dIn[i])},
			dconv:   mkDC(dIn[i]),
			convTr:  htdConvTr1D{cin: dIn[i], cout: tdOut[i], k: 8, stride: 4, w: make([]float32, dIn[i]*8*tdOut[i]), b: make([]float32, tdOut[i])},
			last:    i == 3,
		}
	}
	mkTL := func(cross bool) htdTransLayer {
		mkL := func(i, o int) htdLinear {
			return htdLinear{inDim: i, outDim: o, w: make([]float32, i*o), b: make([]float32, o)}
		}
		return htdTransLayer{
			cross:   cross,
			norm1:   htdLayerNorm{w: ones(512), b: make([]float32, 512)},
			norm2:   htdLayerNorm{w: ones(512), b: make([]float32, 512)},
			norm3:   htdLayerNorm{w: ones(512), b: make([]float32, 512)},
			attn:    htdMHA{dim: 512, heads: 8, q: mkL(512, 512), k: mkL(512, 512), v: mkL(512, 512), out: mkL(512, 512)},
			gamma1:  make([]float32, 512),
			lin1:    mkL(512, 2048),
			lin2:    mkL(2048, 512),
			gamma2:  make([]float32, 512),
			normOut: htdGroupNorm1{w: ones(512), b: make([]float32, 512)},
		}
	}
	for i := 0; i < htdTLayers; i++ {
		m.layers[i] = mkTL(i%2 == 1)
		m.layersT[i] = mkTL(i%2 == 1)
	}
	return m
}

func geluSlice(x []float32) {
	const invSqrt2 = 0.7071067811865475
	for i, v := range x {
		fv := float64(v)
		x[i] = float32(0.5 * fv * (1.0 + math.Erf(fv*invSqrt2)))
	}
}

func gluLastDim(x []float32, rows, halfC int) []float32 {
	out := make([]float32, rows*halfC)
	for r := 0; r < rows; r++ {
		src := x[r*2*halfC : (r+1)*2*halfC]
		dst := out[r*halfC : (r+1)*halfC]
		for c := 0; c < halfC; c++ {
			sig := float32(1.0 / (1.0 + math.Exp(-float64(src[halfC+c]))))
			dst[c] = src[c] * sig
		}
	}
	return out
}

// All spatial tensors in HTDemucs are stored token-major (channels-last):
// 1D: [B, T, C], 2D: [B, Fr, T, C]! That makes every Conv1D/Conv2D/Linear/GLU/LayerNorm contiguous!

func (c *htdConv1D) forward(x []float32, B, T int, workers int) ([]float32, int) {
	Tout := (T+2*c.pad-c.dil*(c.k-1)-1)/c.stride + 1
	out := make([]float32, B*Tout*c.cout)
	addBias(out, B*Tout, c.b)
	if c.k == 1 && c.stride == 1 && c.pad == 0 {
		sgemmAdd(B*T, c.cout, c.cin, 1.0, x, c.w, out, workers)
		return out, Tout
	}
	K := c.k * c.cin
	stripB := max(1, min(B, (4<<20)/max(1, Tout*K*4)))
	col := make([]float32, stripB*Tout*K)
	for b0 := 0; b0 < B; b0 += stripB {
		b1 := min(b0+stripB, B)
		rows := (b1 - b0) * Tout
		clear(col[:rows*K])
		idx := 0
		for b := b0; b < b1; b++ {
			xb := x[b*T*c.cin : (b+1)*T*c.cin]
			for ot := 0; ot < Tout; ot++ {
				dst := col[idx*K : (idx+1)*K]
				for ki := 0; ki < c.k; ki++ {
					it := ot*c.stride + ki*c.dil - c.pad
					if it >= 0 && it < T {
						copy(dst[ki*c.cin:(ki+1)*c.cin], xb[it*c.cin:(it+1)*c.cin])
					}
				}
				idx++
			}
		}
		sgemmAdd(rows, c.cout, K, 1.0, col[:rows*K], c.w, out[b0*Tout*c.cout:b1*Tout*c.cout], workers)
	}
	return out, Tout
}

func (c *htdConvTr1D) forward(x []float32, T int, workers int) ([]float32, int) {
	Tout := (T-1)*c.stride + c.k
	N := c.k * c.cout
	col := make([]float32, T*N)
	sgemmAdd(T, N, c.cin, 1.0, x, c.w, col, workers)
	out := make([]float32, Tout*c.cout)
	addBias(out, Tout, c.b)
	for it := 0; it < T; it++ {
		src := col[it*N : (it+1)*N]
		for ki := 0; ki < c.k; ki++ {
			ot := it*c.stride + ki
			dst := out[ot*c.cout : (ot+1)*c.cout]
			s := src[ki*c.cout : (ki+1)*c.cout]
			for j := 0; j < c.cout; j++ {
				dst[j] += s[j]
			}
		}
	}
	return out, Tout
}

func (c *htdConv2D) forward(x []float32, H, W int, workers int) ([]float32, int, int) {
	Hout := (H+2*c.ph-c.kh)/c.sh + 1
	Wout := (W+2*c.pw-c.kw)/c.sw + 1
	out := make([]float32, Hout*Wout*c.cout)
	addBias(out, Hout*Wout, c.b)
	if c.kh == 1 && c.kw == 1 && c.sh == 1 && c.sw == 1 && c.ph == 0 && c.pw == 0 {
		sgemmAdd(H*W, c.cout, c.cin, 1.0, x, c.w, out, workers)
		return out, Hout, Wout
	}
	K := c.kh * c.kw * c.cin
	stripH := max(1, min(Hout, 64))
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
				for ky := 0; ky < c.kh; ky++ {
					iy := oy*c.sh + ky - c.ph
					if iy < 0 || iy >= H {
						p += c.kw * c.cin
						continue
					}
					for kx := 0; kx < c.kw; kx++ {
						ix := ox*c.sw + kx - c.pw
						if ix >= 0 && ix < W {
							off := (iy*W + ix) * c.cin
							copy(dst[p:p+c.cin], x[off:off+c.cin])
						}
						p += c.cin
					}
				}
				idx++
			}
		}
		sgemmAdd(rows, c.cout, K, 1.0, col[:rows*K], c.w, out[y0*Wout*c.cout:y1*Wout*c.cout], workers)
	}
	return out, Hout, Wout
}

func (c *htdConvTr2D) forward(x []float32, H, W int, workers int) ([]float32, int, int) {
	Hout := (H-1)*c.sh + c.kh
	Wout := (W-1)*c.sw + c.kw
	N := c.kh * c.kw * c.cout
	out := make([]float32, Hout*Wout*c.cout)
	addBias(out, Hout*Wout, c.b)
	stripH := max(1, min(H, 64))
	col := make([]float32, stripH*W*N)
	for y0 := 0; y0 < H; y0 += stripH {
		y1 := min(y0+stripH, H)
		rows := (y1 - y0) * W
		clear(col[:rows*N])
		sgemmAdd(rows, N, c.cin, 1.0, x[y0*W*c.cin:y1*W*c.cin], c.w, col[:rows*N], workers)
		idx := 0
		for iy := y0; iy < y1; iy++ {
			for ix := 0; ix < W; ix++ {
				src := col[idx*N : (idx+1)*N]
				p := 0
				for ky := 0; ky < c.kh; ky++ {
					oy := iy*c.sh + ky
					for kx := 0; kx < c.kw; kx++ {
						ox := ix*c.sw + kx
						dst := out[(oy*Wout+ox)*c.cout : (oy*Wout+ox+1)*c.cout]
						s := src[p : p+c.cout]
						for j := 0; j < c.cout; j++ {
							dst[j] += s[j]
						}
						p += c.cout
					}
				}
				idx++
			}
		}
	}
	return out, Hout, Wout
}

// GroupNorm(1, C) over [B, T, C]: normalizes across all T*C per batch item b.
func (gn *htdGroupNorm1) applyInPlace(x []float32, B, T int) {
	C := len(gn.w)
	span := T * C
	if span == 0 {
		return
	}
	invN := 1.0 / float64(span)
	for b := 0; b < B; b++ {
		sub := x[b*span : (b+1)*span]
		var sum float64
		for _, v := range sub {
			sum += float64(v)
		}
		mean := sum * invN
		var sq float64
		for _, v := range sub {
			d := float64(v) - mean
			sq += d * d
		}
		invStd := 1.0 / math.Sqrt(sq*invN+1e-5)
		for t := 0; t < T; t++ {
			row := sub[t*C : (t+1)*C]
			for c := 0; c < C; c++ {
				row[c] = float32((float64(row[c])-mean)*invStd)*gn.w[c] + gn.b[c]
			}
		}
	}
}

func (ln *htdLayerNorm) apply(x []float32, tokens int) []float32 {
	C := len(ln.w)
	out := make([]float32, tokens*C)
	invC := 1.0 / float64(C)
	for t := 0; t < tokens; t++ {
		src := x[t*C : (t+1)*C]
		dst := out[t*C : (t+1)*C]
		var sum float64
		for _, v := range src {
			sum += float64(v)
		}
		mean := sum * invC
		var sq float64
		for _, v := range src {
			d := float64(v) - mean
			sq += d * d
		}
		invStd := 1.0 / math.Sqrt(sq*invC+1e-5)
		for c := 0; c < C; c++ {
			dst[c] = float32((float64(src[c])-mean)*invStd)*ln.w[c] + ln.b[c]
		}
	}
	return out
}

func (dc *htdDConv) forward(x []float32, B, T, C, workers int) []float32 {
	cur := slicesCloneF32(x)
	for d := 0; d < 2; d++ {
		sub := &dc.subs[d]
		y, t1 := sub.conv1.forward(cur, B, T, workers)
		sub.norm1.applyInPlace(y, B, t1)
		geluSlice(y)
		y2, t2 := sub.conv2.forward(y, B, t1, workers)
		sub.norm2.applyInPlace(y2, B, t2)
		g := gluLastDim(y2, B*t2, C)
		for r := 0; r < B*T; r++ {
			cr := cur[r*C : (r+1)*C]
			gr := g[r*C : (r+1)*C]
			for c := 0; c < C; c++ {
				cr[c] += gr[c] * sub.scale[c]
			}
		}
	}
	return cur
}

func (lin *htdLinear) forward(x []float32, rows, workers int) []float32 {
	out := make([]float32, rows*lin.outDim)
	addBias(out, rows, lin.b)
	sgemmAdd(rows, lin.outDim, lin.inDim, 1.0, x, lin.w, out, workers)
	return out
}

func (mha *htdMHA) forward(qIn, kvIn []float32, Tq, Tk, workers int) []float32 {
	D, H := mha.dim, mha.heads
	hd := D / H
	scale := float32(1.0 / math.Sqrt(float64(hd)))
	Q := mha.q.forward(qIn, Tq, workers)
	K := mha.k.forward(kvIn, Tk, workers)
	V := mha.v.forward(kvIn, Tk, workers)
	ctxOut := make([]float32, Tq*D)

	// Per-head scaled dot-product attention: Q_h [Tq, hd] * K_h^T [hd, Tk] -> scores [Tq, Tk] -> * V_h [Tk, hd]
	qHead := make([]float32, Tq*hd)
	kHeadT := make([]float32, hd*Tk)
	vHead := make([]float32, Tk*hd)
	scores := make([]float32, Tq*Tk)
	attV := make([]float32, Tq*hd)
	for h := 0; h < H; h++ {
		coff := h * hd
		for t := 0; t < Tq; t++ {
			copy(qHead[t*hd:(t+1)*hd], Q[t*D+coff:t*D+coff+hd])
		}
		for t := 0; t < Tk; t++ {
			ks := K[t*D+coff : t*D+coff+hd]
			for j := 0; j < hd; j++ {
				kHeadT[j*Tk+t] = ks[j]
			}
			copy(vHead[t*hd:(t+1)*hd], V[t*D+coff:t*D+coff+hd])
		}
		clear(scores)
		sgemmAdd(Tq, Tk, hd, scale, qHead, kHeadT, scores, workers)
		for t := 0; t < Tq; t++ {
			row := scores[t*Tk : (t+1)*Tk]
			mx := row[0]
			for _, s := range row[1:] {
				if s > mx {
					mx = s
				}
			}
			var sum float64
			for j, s := range row {
				ev := math.Exp(float64(s - mx))
				row[j] = float32(ev)
				sum += ev
			}
			inv := float32(1.0 / sum)
			for j := range row {
				row[j] *= inv
			}
		}
		clear(attV)
		sgemmAdd(Tq, hd, Tk, 1.0, scores, vHead, attV, workers)
		for t := 0; t < Tq; t++ {
			copy(ctxOut[t*D+coff:t*D+coff+hd], attV[t*hd:(t+1)*hd])
		}
	}
	return mha.out.forward(ctxOut, Tq, workers)
}

func (tl *htdTransLayer) forward(q, k []float32, Tq, Tk, workers int) []float32 {
	D := len(tl.gamma1)
	var x []float32
	if !tl.cross {
		nq := tl.norm1.apply(q, Tq)
		ao := tl.attn.forward(nq, nq, Tq, Tq, workers)
		x = make([]float32, Tq*D)
		for t := 0; t < Tq; t++ {
			for c := 0; c < D; c++ {
				x[t*D+c] = q[t*D+c] + tl.gamma1[c]*ao[t*D+c]
			}
		}
		nx := tl.norm2.apply(x, Tq)
		ff := tl.lin1.forward(nx, Tq, workers)
		geluSlice(ff)
		ff2 := tl.lin2.forward(ff, Tq, workers)
		for t := 0; t < Tq; t++ {
			for c := 0; c < D; c++ {
				x[t*D+c] += tl.gamma2[c] * ff2[t*D+c]
			}
		}
	} else {
		nq := tl.norm1.apply(q, Tq)
		nk := tl.norm2.apply(k, Tk)
		ao := tl.attn.forward(nq, nk, Tq, Tk, workers)
		x = make([]float32, Tq*D)
		for t := 0; t < Tq; t++ {
			for c := 0; c < D; c++ {
				x[t*D+c] = q[t*D+c] + tl.gamma1[c]*ao[t*D+c]
			}
		}
		nx := tl.norm3.apply(x, Tq)
		ff := tl.lin1.forward(nx, Tq, workers)
		geluSlice(ff)
		ff2 := tl.lin2.forward(ff, Tq, workers)
		for t := 0; t < Tq; t++ {
			for c := 0; c < D; c++ {
				x[t*D+c] += tl.gamma2[c] * ff2[t*D+c]
			}
		}
	}
	tl.normOut.applyInPlace(x, 1, Tq)
	return x
}

func addSin1DEmbedding(x []float32, T, D int) {
	half := D / 2
	for t := 0; t < T; t++ {
		row := x[t*D : (t+1)*D]
		for i := 0; i < half; i++ {
			denom := math.Pow(10000.0, float64(i)/float64(half-1))
			phase := float64(t) / denom
			row[i] += float32(math.Cos(phase))
			row[half+i] += float32(math.Sin(phase))
		}
	}
}

// addSin2DEmbedding adds create_2d_sin_embedding(D, Fr, T1) onto x laid out as [T1, Fr, D].
func addSin2DEmbedding(x []float32, Fr, T1, D int) {
	half := D / 2
	div := make([]float64, half/2)
	for k := 0; k < half/2; k++ {
		div[k] = math.Exp(float64(2*k) * -(math.Log(10000.0) / float64(half)))
	}
	for t := 0; t < T1; t++ {
		for f := 0; f < Fr; f++ {
			dst := x[(t*Fr+f)*D : (t*Fr+f+1)*D]
			for k := 0; k < half/2; k++ {
				pw := float64(t) * div[k]
				dst[2*k] += float32(math.Sin(pw))
				dst[2*k+1] += float32(math.Cos(pw))
				ph := float64(f) * div[k]
				dst[half+2*k] += float32(math.Sin(ph))
				dst[half+2*k+1] += float32(math.Cos(ph))
			}
		}
	}
}

// htdSpec computes HTDemucs _spec(samples) -> [le, 2048] complex (re, im).
func (m *HTDemucsModel) htdSpec(samples []float32) (re, im []float32, le int) {
	hl := htdHop
	le = (len(samples) + hl - 1) / hl
	specPad := hl / 2 * 3 // 1536
	rightExtra := le*hl - len(samples)
	sp := reflectPad1D(samples, specPad, specPad+rightExtra)
	padded := reflectPad1D(sp, htdFFTSize/2, htdFFTSize/2)

	re = make([]float32, le*htdFreqBins)
	im = make([]float32, le*htdFreqBins)
	winBuf := make([]float32, htdFFTSize)
	fRe := make([]float32, htdFreqBins+1)
	fIm := make([]float32, htdFreqBins+1)
	scratch := make([]float64, htdFFTSize)
	norm := float32(1.0 / math.Sqrt(float64(htdFFTSize)))
	for f := 0; f < le; f++ {
		start := (f + 2) * hl
		src := padded[start : start+htdFFTSize]
		for k := 0; k < htdFFTSize; k++ {
			winBuf[k] = src[k] * m.window[k]
		}
		m.fft.rfft(winBuf, fRe, fIm, scratch)
		dstOff := f * htdFreqBins
		for b := 0; b < htdFreqBins; b++ {
			re[dstOff+b] = fRe[b] * norm
			im[dstOff+b] = fIm[b] * norm
		}
	}
	return re, im, le
}

// htdISpec computes HTDemucs _ispec(re, im [le, 2048], length) -> [length] float32.
func (m *HTDemucsModel) htdISpec(re, im []float32, le, length int) []float32 {
	hl := htdHop
	specPad := hl / 2 * 3 // 1536
	nFrames := le + 4
	paddedLen := (nFrames-1)*hl + htdFFTSize
	ola := make([]float32, paddedLen)
	wsum := make([]float32, paddedLen)
	fRe := make([]float32, htdFreqBins+1)
	fIm := make([]float32, htdFreqBins+1)
	timeBuf := make([]float32, htdFFTSize)
	scratch := make([]float64, htdFFTSize)
	scale := float32(math.Sqrt(float64(htdFFTSize)))

	for f := 0; f < nFrames; f++ {
		clear(fRe)
		clear(fIm)
		if f >= 2 && f < 2+le {
			srcOff := (f - 2) * htdFreqBins
			for b := 0; b < htdFreqBins; b++ {
				fRe[b] = re[srcOff+b] * scale
				fIm[b] = im[srcOff+b] * scale
			}
		}
		m.fft.irfft(fRe, fIm, timeBuf, scratch)
		off := f * hl
		for k := 0; k < htdFFTSize; k++ {
			w := m.window[k]
			ola[off+k] += timeBuf[k] * w
			wsum[off+k] += w * w
		}
	}
	for i := range ola {
		if wsum[i] > 1e-11 {
			ola[i] /= wsum[i]
		}
	}
	centerTrim := htdFFTSize / 2
	start := centerTrim + specPad
	out := make([]float32, length)
	if start < len(ola) {
		copy(out, ola[start:min(len(ola), start+length)])
	}
	return out
}

func meanStdSample(x []float32) (float32, float32) {
	if len(x) == 0 {
		return 0, 1
	}
	var sum float64
	for _, v := range x {
		sum += float64(v)
	}
	mean := sum / float64(len(x))
	if len(x) == 1 {
		return float32(mean), 0
	}
	var sq float64
	for _, v := range x {
		d := float64(v) - mean
		sq += d * d
	}
	std := math.Sqrt(sq / float64(len(x)-1))
	return float32(mean), float32(std)
}

// separateSegment runs HTDemucs on a single stereo segment of length <= m.segLength at 44.1 kHz.
// Returns [4][2][nSamples] in HTDemucs order: drums(0), bass(1), other(2), vocals(3).
func (m *HTDemucsModel) separateSegment(ctx context.Context, left, right []float32) ([4][2][]float32, error) {
	var empty [4][2][]float32
	nSamples := len(left)
	segLen := max(nSamples, m.segLength)
	mixL := make([]float32, segLen)
	mixR := make([]float32, segLen)
	copy(mixL, left)
	copy(mixR, right)

	reL, imL, le := m.htdSpec(mixL)
	reR, imR, _ := m.htdSpec(mixR)

	// Build freq [Fr=2048, T=le, C=4] (CaC: reL, imL, reR, imR)
	Fr, T := htdFreqBins, le
	freq := make([]float32, Fr*T*4)
	for f := 0; f < Fr; f++ {
		for t := 0; t < T; t++ {
			src := t*Fr + f
			dst := (f*T + t) * 4
			freq[dst+0] = reL[src]
			freq[dst+1] = imL[src]
			freq[dst+2] = reR[src]
			freq[dst+3] = imR[src]
		}
	}
	fMean, fStd := meanStdSample(freq)
	invFStd := 1.0 / (1e-5 + fStd)
	for i, v := range freq {
		freq[i] = (v - fMean) * invFStd
	}

	// Build time [T=segLen, C=2]
	tArr := make([]float32, segLen*2)
	for t := 0; t < segLen; t++ {
		tArr[t*2+0] = mixL[t]
		tArr[t*2+1] = mixR[t]
	}
	tMean, tStd := meanStdSample(tArr)
	invTStd := 1.0 / (1e-5 + tStd)
	for i, v := range tArr {
		tArr[i] = (v - tMean) * invTStd
	}

	workers := max(1, m.workers)
	var fSkips [4][]float32
	var fShapes [4][3]int // [Fr, T, C]
	curF, curFr, curFT, curFC := freq, Fr, T, 4
	for i := 0; i < 4; i++ {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		enc := &m.enc[i]
		y, nFr, nT := enc.conv.forward(curF, curFr, curFT, workers)
		geluSlice(y)
		// DConv runs with B = nFr, T = nT, C = chOut — already contiguous in [Fr, T, C]!
		y = enc.dconv.forward(y, nFr, nT, enc.conv.cout, workers)
		rw, _, _ := enc.rewrite.forward(y, nFr, nT, workers)
		curF = gluLastDim(rw, nFr*nT, enc.conv.cout)
		curFr, curFT, curFC = nFr, nT, enc.conv.cout
		if i == 0 {
			for f := 0; f < curFr; f++ {
				emb := m.freqEmb[f*curFC : (f+1)*curFC]
				for t := 0; t < curFT; t++ {
					dst := curF[(f*curFT+t)*curFC : (f*curFT+t+1)*curFC]
					for c := 0; c < curFC; c++ {
						dst[c] += emb[c]
					}
				}
			}
		}
		fSkips[i] = slicesCloneF32(curF)
		fShapes[i] = [3]int{curFr, curFT, curFC}
	}

	var tSkips [4][]float32
	var tLens [4]int
	var tSkipLens [4]int
	curT, curTLen, curTC := tArr, segLen, 2
	for i := 0; i < 4; i++ {
		tLens[i] = curTLen
		if curTLen%htdStride != 0 {
			pad := htdStride - (curTLen % htdStride)
			padded := make([]float32, (curTLen+pad)*curTC)
			copy(padded, curT)
			curT = padded
			curTLen += pad
		}
		enc := &m.tenc[i]
		y, nL := enc.conv.forward(curT, 1, curTLen, workers)
		geluSlice(y)
		y = enc.dconv.forward(y, 1, nL, enc.conv.cout, workers)
		rw, _ := enc.rewrite.forward(y, 1, nL, workers)
		curT = gluLastDim(rw, nL, enc.conv.cout)
		curTLen, curTC = nL, enc.conv.cout
		tSkips[i] = slicesCloneF32(curT)
		tSkipLens[i] = curTLen
	}

	// Channel upsample [curFr, curFT, 384] -> [curFr, curFT, 512] and permute to [curFT, curFr, 512]
	upF, _ := m.chUp.forward(curF, 1, curFr*curFT, workers)
	tokF := make([]float32, curFT*curFr*512)
	for f := 0; f < curFr; f++ {
		for t := 0; t < curFT; t++ {
			copy(tokF[(t*curFr+f)*512:(t*curFr+f+1)*512], upF[(f*curFT+t)*512:(f*curFT+t+1)*512])
		}
	}
	tokF = m.normIn.apply(tokF, curFT*curFr)
	addSin2DEmbedding(tokF, curFr, curFT, 512)

	tokT, _ := m.chUpT.forward(curT, 1, curTLen, workers)
	tokT = m.normInT.apply(tokT, curTLen)
	addSin1DEmbedding(tokT, curTLen, 512)

	nTokF := curFT * curFr
	for i := 0; i < htdTLayers; i++ {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		if !m.layers[i].cross {
			tokF = m.layers[i].forward(tokF, tokF, nTokF, nTokF, workers)
			tokT = m.layersT[i].forward(tokT, tokT, curTLen, curTLen, workers)
		} else {
			oldF := tokF
			tokF = m.layers[i].forward(oldF, tokT, nTokF, curTLen, workers)
			tokT = m.layersT[i].forward(tokT, oldF, curTLen, nTokF, workers)
		}
	}

	// Unpermute [curFT, curFr, 512] -> [curFr, curFT, 512] and channel downsample -> 384
	unpermF := make([]float32, curFr*curFT*512)
	for t := 0; t < curFT; t++ {
		for f := 0; f < curFr; f++ {
			copy(unpermF[(f*curFT+t)*512:(f*curFT+t+1)*512], tokF[(t*curFr+f)*512:(t*curFr+f+1)*512])
		}
	}
	curF, _ = m.chDown.forward(unpermF, 1, curFr*curFT, workers)
	curT, _ = m.chDownT.forward(tokT, 1, curTLen, workers)

	// Decode frequency branch
	for i := 0; i < 4; i++ {
		dec := &m.dec[i]
		skip := fSkips[3-i]
		for k := range curF {
			curF[k] += skip[k]
		}
		rw, _, _ := dec.rewrite.forward(curF, curFr, curFT, workers)
		y := gluLastDim(rw, curFr*curFT, dec.rewrite.cin)
		y = dec.dconv.forward(y, curFr, curFT, dec.rewrite.cin, workers)
		z, zFr, zT := dec.convTr.forward(y, curFr, curFT, workers)
		// Trim pad=2 top and bottom along freq: [2 : zFr-2]
		trimFr := zFr - 4
		curF = z[2*zT*dec.convTr.cout : (2+trimFr)*zT*dec.convTr.cout]
		curFr, curFT, curFC = trimFr, zT, dec.convTr.cout
		if !dec.last {
			geluSlice(curF)
		}
	}

	// Decode time branch
	for i := 0; i < 4; i++ {
		dec := &m.tdec[i]
		skip := tSkips[3-i]
		curTLen = tSkipLens[3-i]
		for k := range curT {
			curT[k] += skip[k]
		}
		rw, _ := dec.rewrite.forward(curT, 1, curTLen, workers)
		y := gluLastDim(rw, curTLen, dec.rewrite.cin)
		y = dec.dconv.forward(y, 1, curTLen, dec.rewrite.cin, workers)
		z, _ := dec.convTr.forward(y, curTLen, workers)
		targetL := tLens[3-i]
		curT = z[2*dec.convTr.cout : (2+targetL)*dec.convTr.cout]
		curTLen, curTC = targetL, dec.convTr.cout
		if !dec.last {
			geluSlice(curT)
		}
	}

	// Denormalize & reconstruct 4 stems (drums=0, bass=1, other=2, vocals=3)
	fScale := fStd
	for i, v := range curF {
		curF[i] = v*fScale + fMean
	}
	tScale := tStd
	for i, v := range curT {
		curT[i] = v*tScale + tMean
	}

	var out [4][2][]float32
	sRe := make([]float32, T*Fr)
	sIm := make([]float32, T*Fr)
	for s := 0; s < 4; s++ {
		for c := 0; c < 2; c++ {
			chIdx := s*4 + c*2
			for f := 0; f < Fr; f++ {
				for t := 0; t < T; t++ {
					base := (f*T+t)*16 + chIdx
					sRe[t*Fr+f] = curF[base]
					sIm[t*Fr+f] = curF[base+1]
				}
			}
			fwav := m.htdISpec(sRe, sIm, T, segLen)
			wav := make([]float32, nSamples)
			tCh := s*2 + c
			for t := 0; t < nSamples; t++ {
				wav[t] = fwav[t] + curT[t*8+tCh]
			}
			out[s][c] = wav
		}
	}
	return out, nil
}

// Separate separates planar [channels][frames] audio at sampleRate into
// EngineX's canonical [4][channels][frames] stems: vocals, bass, drums, melody.
func (m *HTDemucsModel) Separate(ctx context.Context, x [][]float32, sampleRate float64) ([][][]float32, error) {
	channels := len(x)
	if channels != 1 && channels != 2 {
		return nil, errors.New("htdemucs: channels must be 1 or 2")
	}
	origLen := len(x[0])
	if origLen == 0 {
		return emptyStems(channels), nil
	}
	targetLen := origLen
	if math.Abs(sampleRate-htdSampleRate) > 1e-3 {
		targetLen = max(1, int(math.Round(float64(origLen)*htdSampleRate/sampleRate)))
	}
	var left, right []float32
	if math.Abs(sampleRate-htdSampleRate) > 1e-3 {
		left = resampleChannel(x[0], sampleRate, htdSampleRate, targetLen)
		if channels == 2 {
			right = resampleChannel(x[1], sampleRate, htdSampleRate, targetLen)
		} else {
			right = slicesCloneF32(left)
		}
	} else {
		left = slicesCloneF32(x[0])
		if channels == 2 {
			right = slicesCloneF32(x[1])
		} else {
			right = slicesCloneF32(left)
		}
	}

	// Overlap-add chunks of segLength with 25% overlap using Demucs linear triangular weight
	var htdOut [4][2][]float32
	if targetLen <= m.segLength {
		segOut, err := m.separateSegment(ctx, left, right)
		if err != nil {
			return nil, err
		}
		htdOut = segOut
	} else {
		seg := m.segLength
		stride := seg * 3 / 4
		weight := make([]float32, seg)
		half := seg / 2
		for i := 0; i < half; i++ {
			weight[i] = float32(i + 1)
		}
		for i := half; i < seg; i++ {
			weight[i] = float32(seg - i)
		}
		mx := weight[half-1]
		for i := range weight {
			weight[i] /= mx
		}
		wsum := make([]float32, targetLen)
		for s := 0; s < 4; s++ {
			htdOut[s][0] = make([]float32, targetLen)
			htdOut[s][1] = make([]float32, targetLen)
		}
		for start := 0; start < targetLen; start += stride {
			end := min(start+seg, targetLen)
			clen := end - start
			segOut, err := m.separateSegment(ctx, left[start:end], right[start:end])
			if err != nil {
				return nil, err
			}
			for i := 0; i < clen; i++ {
				w := weight[i]
				wsum[start+i] += w
				for s := 0; s < 4; s++ {
					htdOut[s][0][start+i] += segOut[s][0][i] * w
					htdOut[s][1][start+i] += segOut[s][1][i] * w
				}
			}
			if end == targetLen {
				break
			}
		}
		for i := 0; i < targetLen; i++ {
			inv := float32(1.0)
			if wsum[i] > 1e-12 {
				inv = 1.0 / wsum[i]
			}
			for s := 0; s < 4; s++ {
				htdOut[s][0][i] *= inv
				htdOut[s][1][i] *= inv
			}
		}
	}

	// Map HTDemucs order [0=drums, 1=bass, 2=other, 3=vocals] -> canonical [0=vocals, 1=bass, 2=drums]
	// and rebuild melody = x - vocals - bass - drums!
	srcMap := [3]int{3, 1, 0}
	out := make([][][]float32, 4)
	for st := 0; st < 4; st++ {
		out[st] = make([][]float32, channels)
	}
	for dstIdx, srcIdx := range srcMap {
		if channels == 1 {
			mono := make([]float32, targetLen)
			for i := 0; i < targetLen; i++ {
				mono[i] = 0.5 * (htdOut[srcIdx][0][i] + htdOut[srcIdx][1][i])
			}
			if math.Abs(sampleRate-htdSampleRate) > 1e-3 {
				out[dstIdx][0] = resampleChannel(mono, htdSampleRate, sampleRate, origLen)
			} else {
				out[dstIdx][0] = mono
			}
		} else {
			for c := 0; c < 2; c++ {
				if math.Abs(sampleRate-htdSampleRate) > 1e-3 {
					out[dstIdx][c] = resampleChannel(htdOut[srcIdx][c], htdSampleRate, sampleRate, origLen)
				} else {
					out[dstIdx][c] = htdOut[srcIdx][c]
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
