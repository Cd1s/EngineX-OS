package analyzer

import (
	"math"
	"math/cmplx"
)

// fft 是一个原地、迭代的 radix-2 复数 FFT，len(x) 必须是 2 的幂。
type fft struct {
	n       int
	twiddle []complex128
	rev     []int
}

func newFFT(n int) *fft {
	if n&(n-1) != 0 || n < 2 {
		panic("analyzer: fft size must be a power of two")
	}
	f := &fft{n: n, twiddle: make([]complex128, n/2), rev: make([]int, n)}
	for i := range f.twiddle {
		f.twiddle[i] = cmplx.Rect(1, -2*math.Pi*float64(i)/float64(n))
	}
	bits := 0
	for 1<<bits < n {
		bits++
	}
	for i := range f.rev {
		r := 0
		for b := 0; b < bits; b++ {
			if i&(1<<b) != 0 {
				r |= 1 << (bits - 1 - b)
			}
		}
		f.rev[i] = r
	}
	return f
}

func (f *fft) transform(x []complex128) {
	for i, r := range f.rev {
		if i < r {
			x[i], x[r] = x[r], x[i]
		}
	}
	for size := 2; size <= f.n; size <<= 1 {
		half := size / 2
		step := f.n / size
		for start := 0; start < f.n; start += size {
			for k := 0; k < half; k++ {
				t := f.twiddle[k*step] * x[start+k+half]
				x[start+k+half] = x[start+k] - t
				x[start+k] += t
			}
		}
	}
}

// stft 对实信号做短时傅里叶变换，每帧回调一次幅度谱（长度 n/2+1）。
// 帧 i 覆盖样本 [i*hop, i*hop+n)，越界部分补零。
type stft struct {
	f      *fft
	window []float64
	buf    []complex128
	mag    []float64
	hop    int
}

func newSTFT(n, hop int) *stft {
	s := &stft{f: newFFT(n), window: make([]float64, n), buf: make([]complex128, n), mag: make([]float64, n/2+1), hop: hop}
	for i := range s.window {
		s.window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	return s
}

func (s *stft) frames(nsamples int) int {
	if nsamples <= 0 {
		return 0
	}
	return (nsamples + s.hop - 1) / s.hop
}

// frame 计算第 i 帧并返回内部复用的幅度谱切片。
func (s *stft) frame(x []float32, i int) []float64 {
	n := len(s.window)
	off := i * s.hop
	for j := 0; j < n; j++ {
		v := 0.0
		if k := off + j; k < len(x) {
			v = float64(x[k])
		}
		s.buf[j] = complex(v*s.window[j], 0)
	}
	s.f.transform(s.buf)
	for j := range s.mag {
		s.mag[j] = cmplx.Abs(s.buf[j])
	}
	return s.mag
}
