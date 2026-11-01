// Package analyzer 是纯 Go 的轻量级音频分析管线：
// 解码 WAV/FLAC/MP3 → BPM（谱通量 Onset + 自相关 + 全曲梳状精调）→ Beatgrid
// → 调性（Chroma + Krumhansl 模板 → Camelot / Engine key id）
// → 低/中/高三频段能量生成 Engine RGB 波形与 Overview 波形。
//
// 分析器只计算拍线与波形，从不生成 Hot Cue。
package analyzer

import (
	"errors"
	"math"
)

// Ticker 由工作循环频繁调用，用于节流和取消（*resourceguard.Throttle 实现了它）。
type Ticker interface {
	Tick() error
}

// Options 控制分析参数。
type Options struct {
	// MinBPM/MaxBPM 是 BPM 折叠区间 [Min, Max]，宽度应小于一个八度，
	// 以消除倍频/半频歧义。默认 88–175（与 Engine DJ 默认区间一致）。
	MinBPM, MaxBPM float64
	// Ticker 可为 nil。
	Ticker Ticker
}

func (o *Options) defaults() {
	if o.MinBPM <= 0 {
		o.MinBPM = 88
	}
	if o.MaxBPM <= o.MinBPM {
		o.MaxBPM = 175
	}
}

// BeatMarker 对应 Engine beatData 中的一个拍线锚点。
type BeatMarker struct {
	SampleOffset float64 `json:"sampleOffset"`
	BeatNumber   int64   `json:"beatNumber"`
	NumBeats     int32   `json:"numBeats"`
}

// WavePoint 是一个波形点：低/中/高三频段（Engine 中分别渲染为红/绿/蓝）
// 的峰值（Value）与 RMS 不透明度（Opacity），均已归一化到 0–255。
type WavePoint struct {
	LowValue, LowOpacity   uint8
	MidValue, MidOpacity   uint8
	HighValue, HighOpacity uint8
}

// RGB 返回该点用于预览的颜色。
func (p WavePoint) RGB() (r, g, b uint8) { return p.LowValue, p.MidValue, p.HighValue }

// Result 是一次分析的完整结果。
type Result struct {
	SampleRate   int     `json:"sampleRate"`
	TotalSamples int64   `json:"totalSamples"`
	Duration     float64 `json:"duration"`

	BPM float64 `json:"bpm"`
	// FirstBeat 是第一个完整拍的位置（秒）。
	FirstBeat float64      `json:"firstBeat"`
	Beatgrid  []BeatMarker `json:"beatgrid"`

	Key           Key     `json:"key"`
	KeyConfidence float64 `json:"keyConfidence"`

	// Loudness 是 0–1 的平均响度（RMS 相对满幅）。
	Loudness float64 `json:"loudness"`

	Waveform                []WavePoint `json:"-"`
	WaveformSamplesPerEntry float64     `json:"waveformSamplesPerEntry"`
	Overview                []WavePoint `json:"-"`
	OverviewSamplesPerEntry float64     `json:"overviewSamplesPerEntry"`
}

const (
	// analysisRate 是 BPM/调性分析使用的降采样目标率。
	analysisRate = 11025
	// OverviewEntries 是 Engine overview 波形固定点数。
	OverviewEntries = 1024
	// WaveformEntriesPerSecond 是高分辨率 RGB 波形每秒点数。
	WaveformEntriesPerSecond = 105
	tickEvery                = 4096
)

// ErrTooShort 表示音频太短，无法可靠分析。
var ErrTooShort = errors.New("analyzer: audio shorter than 4 seconds")

// Analyze 对解码后的音频执行完整分析。
func Analyze(a *Audio, opts Options) (*Result, error) {
	opts.defaults()
	if opts.MinBPM < 40 || opts.MaxBPM > 300 || opts.MaxBPM <= opts.MinBPM || math.IsNaN(opts.MinBPM) || math.IsNaN(opts.MaxBPM) || math.IsInf(opts.MinBPM, 0) || math.IsInf(opts.MaxBPM, 0) {
		return nil, errors.New("analyzer: invalid BPM range (40..300)")
	}
	if a == nil || a.SampleRate <= 0 || a.Duration() < 4 {
		return nil, ErrTooShort
	}
	tick := tickerFunc(opts.Ticker)
	res := &Result{SampleRate: a.SampleRate, TotalSamples: int64(len(a.Samples)), Duration: a.Duration()}

	if err := computeWaveforms(a, res, tick); err != nil {
		return nil, err
	}

	low, lowSR, err := decimate(a.Samples, a.SampleRate, analysisRate, tick)
	if err != nil {
		return nil, err
	}
	bpm, firstBeat, err := detectTempo(low, lowSR, opts.MinBPM, opts.MaxBPM, tick)
	if err != nil {
		return nil, err
	}
	res.BPM = bpm
	res.FirstBeat = firstBeat
	res.Beatgrid = buildBeatgrid(bpm, firstBeat, a.SampleRate, res.TotalSamples)

	key, conf, err := detectKey(low, lowSR, tick)
	if err != nil {
		return nil, err
	}
	res.Key, res.KeyConfidence = key, conf
	return res, nil
}

// AnalyzeFile 解码并分析文件。
func AnalyzeFile(path string, opts Options) (*Audio, *Result, error) {
	a, err := decodeFile(path, opts.Ticker)
	if err != nil {
		return nil, nil, err
	}
	res, err := Analyze(a, opts)
	return a, res, err
}

// tickerFunc 把 Ticker 包装成「每 tickEvery 次迭代才真正调用一次」的函数。
func tickerFunc(t Ticker) func(int) error {
	if t == nil {
		return func(int) error { return nil }
	}
	return func(i int) error {
		if i%tickEvery != 0 {
			return nil
		}
		return t.Tick()
	}
}

// buildBeatgrid 按 libdjinterop 的规范化方式生成两个锚点：
// 第一个锚点在第 -4 拍（早于曲首），第二个锚点在曲尾之后。
func buildBeatgrid(bpm, firstBeat float64, sr int, total int64) []BeatMarker {
	spb := 60 * float64(sr) / bpm
	first := firstBeat * float64(sr)
	n := int64(math.Ceil((float64(total)-first)/spb)) + 1
	if n < 1 {
		n = 1
	}
	return []BeatMarker{
		{SampleOffset: first - 4*spb, BeatNumber: -4, NumBeats: int32(n + 4)},
		{SampleOffset: first + float64(n)*spb, BeatNumber: n, NumBeats: 0},
	}
}

// computeWaveforms 用 IIR 分频（低 < 200 Hz < 中 < 2.5 kHz < 高）计算每个
// 波形点各频段的峰值与 RMS，再按全曲最大值归一化。
func computeWaveforms(a *Audio, res *Result, tick func(int) error) error {
	sr := float64(a.SampleRate)
	spe := sr / WaveformEntriesPerSecond
	entries := int(math.Ceil(float64(len(a.Samples)) / spe))
	type acc struct{ peak, sq [3]float64 }
	raw := make([]acc, entries)

	lp1, lp2 := newLowpass(sr, 200, butterworthQ), newLowpass(sr, 200, butterworthQ)
	mh1, mh2 := newHighpass(sr, 200, butterworthQ), newHighpass(sr, 200, butterworthQ)
	ml1, ml2 := newLowpass(sr, 2500, butterworthQ), newLowpass(sr, 2500, butterworthQ)
	hp1, hp2 := newHighpass(sr, 2500, butterworthQ), newHighpass(sr, 2500, butterworthQ)

	var totalSq float64
	for i, v := range a.Samples {
		x := float64(v)
		totalSq += x * x
		bands := [3]float64{
			lp2.process(lp1.process(x)),
			ml2.process(ml1.process(mh2.process(mh1.process(x)))),
			hp2.process(hp1.process(x)),
		}
		e := &raw[int(float64(i)/spe)]
		for b, y := range bands {
			if ay := math.Abs(y); ay > e.peak[b] {
				e.peak[b] = ay
			}
			e.sq[b] += y * y
		}
		if err := tick(i); err != nil {
			return err
		}
	}
	res.Loudness = math.Min(1, math.Sqrt(totalSq/float64(len(a.Samples)))*math.Sqrt2)

	// Use a shared scale: independent band normalization would amplify filter
	// leakage into a full-scale colour even for a single-frequency tone.
	n := spe
	var maxPeak, maxRMS [3]float64
	rms := make([][3]float64, entries)
	for i := range raw {
		for b := 0; b < 3; b++ {
			rms[i][b] = math.Sqrt(raw[i].sq[b] / n)
			maxPeak[b] = math.Max(maxPeak[b], raw[i].peak[b])
			maxRMS[b] = math.Max(maxRMS[b], rms[i][b])
		}
	}
	commonPeak := max(maxPeak[0], maxPeak[1], maxPeak[2])
	commonRMS := max(maxRMS[0], maxRMS[1], maxRMS[2])
	for b := range 3 {
		maxPeak[b] = commonPeak
		maxRMS[b] = commonRMS
	}
	scale := func(v, max float64) uint8 {
		if max <= 0 {
			return 0
		}
		return uint8(math.Round(math.Min(1, v/max) * 255))
	}
	res.Waveform = make([]WavePoint, entries)
	for i := range raw {
		p := &res.Waveform[i]
		p.LowValue, p.LowOpacity = scale(raw[i].peak[0], maxPeak[0]), scale(rms[i][0], maxRMS[0])
		p.MidValue, p.MidOpacity = scale(raw[i].peak[1], maxPeak[1]), scale(rms[i][1], maxRMS[1])
		p.HighValue, p.HighOpacity = scale(raw[i].peak[2], maxPeak[2]), scale(rms[i][2], maxRMS[2])
	}
	res.WaveformSamplesPerEntry = spe

	// Overview：固定 1024 点；短音频采用最近的高分辨率点，避免空洞。
	res.OverviewSamplesPerEntry = float64(len(a.Samples)) / OverviewEntries
	res.Overview = make([]WavePoint, OverviewEntries)
	for i := range res.Overview {
		o := &res.Overview[i]
		start := min(len(res.Waveform)-1, int(float64(i)*res.OverviewSamplesPerEntry/spe))
		end := min(len(res.Waveform), max(start+1, int(math.Ceil(float64(i+1)*res.OverviewSamplesPerEntry/spe))))
		for _, p := range res.Waveform[start:end] {
			o.LowValue, o.LowOpacity = max(o.LowValue, p.LowValue), max(o.LowOpacity, p.LowOpacity)
			o.MidValue, o.MidOpacity = max(o.MidValue, p.MidValue), max(o.MidOpacity, p.MidOpacity)
			o.HighValue, o.HighOpacity = max(o.HighValue, p.HighValue), max(o.HighOpacity, p.HighOpacity)
		}
	}
	return nil
}
