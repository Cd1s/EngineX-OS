package analyzer

import "math"

// biquad 是 RBJ Audio EQ Cookbook 的二阶 IIR 滤波器（Direct Form I）。
type biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     float64
}

func newLowpass(sr, fc, q float64) *biquad {
	w := 2 * math.Pi * fc / sr
	alpha := math.Sin(w) / (2 * q)
	cos := math.Cos(w)
	a0 := 1 + alpha
	return &biquad{
		b0: (1 - cos) / 2 / a0, b1: (1 - cos) / a0, b2: (1 - cos) / 2 / a0,
		a1: -2 * cos / a0, a2: (1 - alpha) / a0,
	}
}

func newHighpass(sr, fc, q float64) *biquad {
	w := 2 * math.Pi * fc / sr
	alpha := math.Sin(w) / (2 * q)
	cos := math.Cos(w)
	a0 := 1 + alpha
	return &biquad{
		b0: (1 + cos) / 2 / a0, b1: -(1 + cos) / a0, b2: (1 + cos) / 2 / a0,
		a1: -2 * cos / a0, a2: (1 - alpha) / a0,
	}
}

func (f *biquad) process(x float64) float64 {
	y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
	f.x2, f.x1 = f.x1, x
	f.y2, f.y1 = f.y1, y
	return y
}

const butterworthQ = 0.7071067811865476

// decimate 把信号降采样到约 target Hz（整数倍抽取，先做 4 阶低通抗混叠）。
// 返回新信号与新采样率。
func decimate(x []float32, sr, target int, tick func(int) error) ([]float32, int, error) {
	factor := int(math.Round(float64(sr) / float64(target)))
	if factor <= 1 {
		return x, sr, nil
	}
	newSR := sr / factor
	cut := 0.45 * float64(newSR)
	f1, f2 := newLowpass(float64(sr), cut, butterworthQ), newLowpass(float64(sr), cut, butterworthQ)
	out := make([]float32, 0, len(x)/factor+1)
	for i, v := range x {
		y := f2.process(f1.process(float64(v)))
		if i%factor == 0 {
			out = append(out, float32(y))
		}
		if err := tick(i); err != nil {
			return nil, 0, err
		}
	}
	return out, newSR, nil
}
