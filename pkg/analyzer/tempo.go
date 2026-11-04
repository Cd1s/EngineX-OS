package analyzer

import (
	"errors"
	"math"
)

const (
	onsetFFT = 1024
	onsetHop = 128
)

// onsetEnvelope 计算对数压缩谱通量，并减去约 0.5 秒滑动均值后半波整流。
func onsetEnvelope(x []float32, sr int, tick func(int) error) ([]float64, float64, error) {
	s := newSTFT(onsetFFT, onsetHop)
	nf := s.frames(len(x))
	maxBin := min(len(s.mag)-1, int(5000*float64(onsetFFT)/float64(sr)))
	prev := make([]float64, maxBin+1)
	flux := make([]float64, nf)
	for i := 0; i < nf; i++ {
		mag := s.frame(x, i)
		sum := 0.0
		for b := 1; b <= maxBin; b++ {
			v := math.Log1p(1000 * mag[b])
			if d := v - prev[b]; d > 0 && i > 0 {
				sum += d
			}
			prev[b] = v
		}
		flux[i] = sum
		if err := tick(i * onsetHop); err != nil {
			return nil, 0, err
		}
	}
	fps := float64(sr) / onsetHop
	win := int(fps / 2)
	env := make([]float64, nf)
	var acc float64
	for i := 0; i < nf; i++ {
		acc += flux[i]
		if i >= win {
			acc -= flux[i-win]
		}
		mean := acc / float64(min(i+1, win))
		if v := flux[i] - mean; v > 0 {
			env[i] = v
		}
	}
	// 归一化到单位最大值，避免精调时数值依赖音量。
	peak := 0.0
	for _, v := range env {
		peak = math.Max(peak, v)
	}
	if peak > 0 {
		for i := range env {
			env[i] /= peak
		}
	}
	return env, fps, nil
}

// detectTempo 返回 BPM 与第一拍位置（秒）。
//
//  1. 在 [minBPM, maxBPM] 内对 onset 包络做自相关，取峰值（含 2 倍滞后谐波加权）；
//  2. 在粗估 ±2% 内做全曲梳状对齐搜索（速度 × 相位），取 onset 能量和最大者；
//     全曲对齐使精度远高于单帧分辨率（数百拍的累积误差会被放大）。
func detectTempo(x []float32, sr int, minBPM, maxBPM float64, tick func(int) error) (float64, float64, error) {
	env, fps, err := onsetEnvelope(x, sr, tick)
	if err != nil {
		return 0, 0, err
	}
	var energy float64
	for _, v := range env {
		energy += v
	}
	if energy < 1e-9 {
		return 0, 0, errors.New("analyzer: no rhythmic onsets detected")
	}
	minLag := int(math.Floor(60 * fps / maxBPM))
	maxLag := int(math.Ceil(60 * fps / minBPM))
	ac, err := onsetAutocorrelation(env, minLag, maxLag, tick)
	if err != nil {
		return 0, 0, err
	}
	bestLag, bestScore := minLag, -1.0
	for lag := minLag; lag <= maxLag; lag++ {
		score := ac[lag] + 0.5*ac[2*lag]
		if score > bestScore {
			bestLag, bestScore = lag, score
		}
	}
	// 抛物线插值得到亚帧滞后。
	lag := float64(bestLag)
	if bestLag > minLag && bestLag < maxLag {
		a, b, c := ac[bestLag-1], ac[bestLag], ac[bestLag+1]
		if d := a - 2*b + c; d < 0 {
			lag += math.Max(-0.5, math.Min(0.5, 0.5*(a-c)/d))
		}
	}
	coarse := foldBPM(60*fps/lag, minBPM, maxBPM)
	if coarse <= 0 {
		return 0, 0, ErrTooShort
	}

	bpm, phase := coarse, 0.0
	var ok bool
	for _, stage := range []struct{ span, step, phaseStep float64 }{
		{0.02 * coarse, 0.05, 0.5},
		{0.06, 0.002, 0.1},
	} {
		bpm, phase, ok, err = combSearch(env, fps, bpm, stage.span, stage.step, stage.phaseStep, tick)
		if err != nil {
			return 0, 0, err
		}
		if !ok {
			return 0, 0, ErrTooShort
		}
	}
	// The comb search may drift past the window edges; the result stays inside (#35).
	bpm = snapBPM(math.Max(minBPM, math.Min(maxBPM, bpm)))
	period := 60 / bpm
	// 包络帧 i 对应窗口 [i*hop, i*hop+N)，谱通量峰值对应起音在窗口中部附近，
	// 补偿半个窗口长度。
	first := phase/fps + float64(onsetFFT)/2/float64(sr) - float64(onsetHop)/2/float64(sr)
	first = math.Mod(first, period)
	if first < 0 {
		first += period
	}
	return bpm, first, nil
}

// onsetAutocorrelation includes the doubled lags used to score tempo harmonics.
func onsetAutocorrelation(env []float64, minLag, maxLag int, tick func(int) error) ([]float64, error) {
	if minLag < 1 || maxLag < minLag || maxLag > (len(env)-1)/2 {
		return nil, ErrTooShort
	}
	ac := make([]float64, 2*maxLag+1)
	for lag := minLag; lag <= 2*maxLag; lag++ {
		sum := 0.0
		for i := 0; i+lag < len(env); i++ {
			sum += env[i] * env[i+lag]
		}
		ac[lag] = sum / float64(len(env)-lag)
		if err := tick(lag * tickEvery); err != nil {
			return nil, err
		}
	}
	return ac, nil
}

// combSearch 在 center±span 范围内搜索使全曲拍点 onset 能量和最大的速度与相位（帧）。
func combSearch(env []float64, fps, center, span, step, phaseStep float64, tick func(int) error) (bpm, phase float64, ok bool, err error) {
	// A zero BPM produces an infinite phase period, so reject before iterating.
	for _, v := range []float64{fps, center, step, phaseStep} {
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, 0, false, ErrTooShort
		}
	}
	if span < 0 || math.IsNaN(span) || math.IsInf(span, 0) || center-span <= 0 || math.IsInf(center+span, 0) || (center-span)+step == center-span {
		return 0, 0, false, ErrTooShort
	}

	best := -1.0
	it := 0
	for b := center - span; b <= center+span+1e-9; b += step {
		p := 60 * fps / b
		if p <= 0 || math.IsInf(p, 0) || math.IsNaN(p) {
			return 0, 0, false, ErrTooShort
		}
		for ph := 0.0; ph < p; ph += phaseStep {
			sum := 0.0
			for t := ph; t < float64(len(env)-1); t += p {
				i := int(t)
				f := t - float64(i)
				sum += env[i]*(1-f) + env[i+1]*f
			}
			if sum > best {
				best, bpm, phase, ok = sum, b, ph, true
			}
			it++
			if err := tick(it * tickEvery); err != nil {
				return 0, 0, false, err
			}
		}
	}
	return bpm, phase, ok, nil
}

// foldBPM 把 bpm 折叠到 [lo, hi]。
func foldBPM(bpm, lo, hi float64) float64 {
	if bpm <= 0 || math.IsNaN(bpm) || math.IsInf(bpm, 0) || lo <= 0 || hi < lo || math.IsNaN(lo) || math.IsNaN(hi) || math.IsInf(lo, 0) || math.IsInf(hi, 0) {
		return 0
	}
	for bpm < lo {
		bpm *= 2
		if math.IsInf(bpm, 0) {
			return 0
		}
	}
	for bpm > hi {
		bpm /= 2
	}
	// A window narrower than an octave has no octave of some tempos inside it:
	// take the nearer edge instead of returning a value below lo (#35).
	if bpm < lo {
		if 2*bpm-hi < lo-bpm {
			return hi
		}
		return lo
	}
	return bpm
}

// snapBPM 保留两位小数；距整数 0.03 以内时取整（绝大多数电子乐为整数 BPM）。
func snapBPM(bpm float64) float64 {
	if r := math.Round(bpm); math.Abs(bpm-r) < 0.03 {
		return r
	}
	return math.Round(bpm*100) / 100
}
