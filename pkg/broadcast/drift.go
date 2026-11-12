package broadcast

import (
	"math"
	"sort"
	"time"
)

// Drift correction for the live feed (r3).
//
// The Engine hook converts the device rate to 48 kHz by its nominal ratio, so the
// feed follows the audio clock: on a real deck it carried about +1400 ppm more
// frames than the system clock asks for, and every receiver's backlog grew by
// about 84 ms a minute. The pacer makes the published stream follow
// CLOCK_MONOTONIC instead: SampleRate frames per second, whatever the audio clock
// does. It is the one place every output (local listeners, Icecast/Shoutcast, LAN)
// gets its PCM from.
//
//   - driftTracker measures the feed against the clock. Reads are only ever late,
//     never early, so it follows the on-time envelope (the per-second maximum of
//     frames received minus frames due). The rate offset is the median slope of
//     those points (Theil-Sen, robust against loaded seconds); a slow pull on the
//     published position keeps the stream on the clock, so the error stays bounded
//     even before the rate is known.
//   - resampler applies the ratio with a 16-tap windowed-sinc interpolator. The
//     ratio only ever moves gradually and the read position is continuous, so a
//     ratio change cannot click. At a ratio of exactly 1 it is a bit-exact delay.
//   - Keep-alive silence is counted on the same clock, so a pause in the feed
//     neither shortens nor lengthens the stream.

const (
	rsTaps   = 16           // interpolator length, input frames
	rsBefore = rsTaps/2 - 1 // taps before the frame the read position is in
	rsPhases = 128          // tabulated fractional positions; coefficients are interpolated between them
	rsBeta   = 4.5          // Kaiser window: flat to ~19 kHz, images below about -50 dB

	driftWindow     = time.Second      // one on-time point per window
	driftPoints     = 48               // points kept for the rate fit
	driftFitMin     = 3                // points before the first fit
	driftBlend      = 15 * time.Second // after a pause the new fit replaces the old estimate over this span
	driftMaxOffset  = 5000e-6          // a larger offset is not a clock: the source is not real time
	driftStep       = 0.010            // s: a point this far off the line restarts the fit (lost or late audio)
	phaseWindows    = 4                // the position error is the maximum over this many windows
	phaseTau        = 20.0             // s: time constant of the pull back onto the clock
	phaseMaxPull    = 2000e-6          // the pull never bends the ratio by more than this
	phaseReanchor   = 0.250            // s: beyond this the clock is re-anchored instead of pulled
	ratioSmooth     = 0.5              // s: the applied ratio approaches its target with this time constant
	maxSilenceBurst = SampleRate / 4   // frames: keep-alive silence never arrives in a larger block
)

// rsCoef[p][0] is the filter for fractional position p/rsPhases, rsCoef[p][1] the
// difference to the next position (for linear interpolation between the two).
var rsCoef = func() *[rsPhases][2][rsTaps]float32 {
	bessel := func(x float64) float64 { // modified Bessel function I0
		sum, term := 1.0, 1.0
		for k := 1; k < 40; k++ {
			term *= (x / 2) / float64(k)
			sum += term * term
		}
		return sum
	}
	phase := func(p int) (h [rsTaps]float64) {
		if p == 0 || p == rsPhases { // on a frame: the frame itself, exactly
			h[rsBefore+p/rsPhases] = 1
			return h
		}
		f, sum := float64(p)/rsPhases, 0.0
		for k := range h {
			d := float64(k-rsBefore) - f // distance of tap k from the read position
			w := bessel(rsBeta*math.Sqrt(math.Max(0, 1-(d/(rsTaps/2))*(d/(rsTaps/2))))) / bessel(rsBeta)
			h[k] = w * math.Sin(math.Pi*d) / (math.Pi * d)
			sum += h[k]
		}
		for k := range h {
			h[k] /= sum // unity gain at DC for every position
		}
		return h
	}
	var tab [rsPhases][2][rsTaps]float32
	for p := range tab {
		a, b := phase(p), phase(p+1)
		for k := range a {
			tab[p][0][k] = float32(a[k])
			tab[p][1][k] = float32(b[k] - a[k])
		}
	}
	return &tab
}()

// resampler reads s16le stereo at a variable ratio (input frames per output frame).
type resampler struct {
	l, r []float32 // the last rsTaps-1 frames already read, then the frames still to read
	pos  float64   // next read position in frames from l[0]
}

// reset forgets the audio seen so far (the feed paused or a new writer started).
func (rs *resampler) reset() {
	rs.l = append(rs.l[:0], make([]float32, rsBefore)...)
	rs.r = append(rs.r[:0], make([]float32, rsBefore)...)
	rs.pos = rsBefore
}

// process appends the resampled frames for in (whole s16le stereo frames) to out.
// Up to rsTaps/2 input frames stay behind for the next call.
func (rs *resampler) process(in []byte, ratio float64, out []byte) []byte {
	if rs.l == nil {
		rs.reset()
	}
	for i := 0; i+3 < len(in); i += 4 {
		rs.l = append(rs.l, float32(int16(uint16(in[i])|uint16(in[i+1])<<8)))
		rs.r = append(rs.r, float32(int16(uint16(in[i+2])|uint16(in[i+3])<<8)))
	}
	n, pos := len(rs.l), rs.pos
	for {
		i := int(pos)
		if i+rsTaps-rsBefore > n { // the last tap is frame i+rsTaps-rsBefore-1
			break
		}
		pf := (pos - float64(i)) * rsPhases
		p := int(pf)
		a := float32(pf - float64(p))
		c := &rsCoef[p]
		xl := rs.l[i-rsBefore : i-rsBefore+rsTaps : i-rsBefore+rsTaps]
		xr := rs.r[i-rsBefore : i-rsBefore+rsTaps : i-rsBefore+rsTaps]
		var sl, sr float32
		for k := 0; k < rsTaps; k++ {
			h := c[0][k] + a*c[1][k]
			sl += h * xl[k]
			sr += h * xr[k]
		}
		vl, vr := s16(sl), s16(sr)
		out = append(out, byte(vl), byte(vl>>8), byte(vr), byte(vr>>8))
		pos += ratio
	}
	drop := int(pos) - rsBefore // frames no later output can reach
	if drop > n {
		drop = n
	}
	if drop > 0 {
		rs.l = rs.l[:copy(rs.l, rs.l[drop:])]
		rs.r = rs.r[:copy(rs.r, rs.r[drop:])]
		pos -= float64(drop)
	}
	rs.pos = pos
	return out
}

// s16 rounds to the nearest sample and clips (interpolation can overshoot full scale).
func s16(x float32) uint16 {
	switch {
	case x >= 32767:
		return 32767
	case x <= -32768:
		return 0x8000
	case x >= 0:
		return uint16(int16(x + 0.5))
	}
	return uint16(int16(x - 0.5))
}

type driftPoint struct{ t, d float64 } // seconds, frames ahead of the nominal rate

// driftTracker turns what the capture loop sees (frames read and frames published
// against monotonic time) into the resampling ratio.
type driftTracker struct {
	offset   float64 // estimated fractional rate offset of the feed; 0 = on the clock
	locked   bool    // offset comes from a fit of this feed
	prior    float64 // estimate from before the last pause
	hasPrior bool
	ratio    float64 // applied input frames per output frame
	target   float64

	t0       time.Duration // start of the current run of PCM
	in       int64         // input frames in this run
	last     time.Duration // previous observation
	winStart time.Duration
	winD     driftPoint // the most on-time read of this window
	winLead  float64    // the largest lead (frames published ahead of the clock) of this window
	winSet   bool
	pts      []driftPoint
	leads    []float64 // per-window lead maxima, the last phaseWindows
	scratch  []float64
}

func newDriftTracker(offset float64, known bool) driftTracker {
	if !known || math.Abs(offset) > driftMaxOffset {
		offset, known = 0, false
	}
	return driftTracker{offset: offset, prior: offset, hasPrior: known, ratio: 1 + offset, target: 1 + offset}
}

// resume starts a new run of PCM at now: the earlier points are not comparable
// (frames were lost or never played), but the rate estimate and the ratio carry over.
func (t *driftTracker) resume(now time.Duration) {
	if t.ratio == 0 {
		t.ratio, t.target = 1, 1
	}
	if t.locked {
		t.prior, t.hasPrior = t.offset, true
	}
	t.t0, t.in, t.last, t.winStart, t.winSet = now, 0, now, now, false
	t.pts, t.leads = t.pts[:0], t.leads[:0]
	t.target = 1 + t.offset
}

// observe records one read of frames input frames at now, after which the stream
// is lead frames ahead of the clock. It returns how far to move the clock when the
// position error is beyond pulling (the source is not real time, or time jumped).
func (t *driftTracker) observe(now time.Duration, frames int, lead float64) (reanchor float64) {
	t.in += int64(frames)
	sec := (now - t.t0).Seconds()
	d := float64(t.in) - SampleRate*sec
	if !t.winSet || d > t.winD.d {
		t.winD = driftPoint{sec, d}
	}
	if !t.winSet || lead > t.winLead {
		t.winLead = lead
	}
	t.winSet = true
	if dt := (now - t.last).Seconds(); dt > 0 {
		t.ratio += (t.target - t.ratio) * math.Min(1, dt/ratioSmooth)
	}
	t.last = now
	if now-t.winStart < driftWindow {
		return 0
	}
	t.winStart, t.winSet = now, false

	// Rate: the median slope of the on-time points.
	if n := len(t.pts); n > 0 {
		predicted := t.pts[n-1].d + t.offset*SampleRate*(t.winD.t-t.pts[n-1].t)
		if math.Abs(t.winD.d-predicted) > driftStep*SampleRate {
			if t.locked {
				t.prior, t.hasPrior = t.offset, true
			}
			t.pts = t.pts[:0]
		}
	}
	if len(t.pts) == driftPoints {
		t.pts = t.pts[:copy(t.pts, t.pts[1:])]
	}
	t.pts = append(t.pts, t.winD)
	if n := len(t.pts); n >= driftFitMin {
		if fit := t.medianSlope() / SampleRate; math.Abs(fit) <= driftMaxOffset {
			// With an earlier estimate (from before a pause, or a seed) a young fit
			// only gradually replaces it; without one the fit is all there is.
			span, w := t.pts[n-1].t-t.pts[0].t, 1.0
			if t.hasPrior {
				w = math.Min(1, span/driftBlend.Seconds())
			}
			t.offset, t.locked = t.prior+(fit-t.prior)*w, true
			if span >= driftBlend.Seconds() {
				t.prior, t.hasPrior = t.offset, true
			}
		}
	}

	// Position: pull the on-time lead back to zero, slowly.
	if len(t.leads) == phaseWindows {
		t.leads = t.leads[:copy(t.leads, t.leads[1:])]
	}
	t.leads = append(t.leads, t.winLead)
	env := t.leads[0]
	for _, v := range t.leads[1:] {
		env = math.Max(env, v)
	}
	errSec := env / SampleRate
	if math.Abs(errSec) > phaseReanchor {
		t.leads = t.leads[:0]
		t.target = 1 + t.offset
		return env
	}
	pull := math.Max(-phaseMaxPull, math.Min(phaseMaxPull, errSec/phaseTau))
	t.target = (1 + t.offset) * (1 + pull)
	return 0
}

func (t *driftTracker) medianSlope() float64 {
	s := t.scratch[:0]
	for i := range t.pts {
		for j := i + 1; j < len(t.pts); j++ {
			if dt := t.pts[j].t - t.pts[i].t; dt > 0 {
				s = append(s, (t.pts[j].d-t.pts[i].d)/dt)
			}
		}
	}
	t.scratch = s
	if len(s) == 0 {
		return 0
	}
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// pacer decides what one pass of the capture loop publishes: the feed resampled
// onto the clock, nothing during a short gap, or keep-alive silence that keeps the
// stream on the clock while nothing plays.
type pacer struct {
	correct bool // resample onto the clock (the live FIFO); false passes PCM through
	base    time.Time
	gate    silenceGate
	rs      resampler
	tr      driftTracker
	emitted int64   // frames published
	origin  float64 // the clock asks for SampleRate*t+origin frames at t
	live    bool    // the last thing published was PCM
}

// step takes what one read returned (nil: nothing) at monotonic time now since the
// capture loop started and returns the bytes to publish (nil: nothing).
func (p *pacer) step(pcm []byte, now time.Duration) []byte {
	due := SampleRate*now.Seconds() + p.origin
	if _, send := p.gate.next(pcm, p.base.Add(now)); !send {
		return nil // a short gap in live PCM: never splice silence into playing audio
	}
	if pcm == nil {
		p.live = false
		n := int64(due) - p.emitted
		if n <= 0 {
			return nil
		}
		if n > maxSilenceBurst { // far behind (time jumped, or the source was slower than real time)
			p.origin -= float64(n - maxSilenceBurst)
			n = maxSilenceBurst
		}
		p.emitted += n
		return make([]byte, n*4)
	}
	if !p.correct {
		p.live = true
		p.emitted += int64(len(pcm) / 4)
		return pcm
	}
	resumed := !p.live
	if resumed {
		p.live = true
		p.rs.reset()
		p.tr.resume(now)
	}
	out := p.rs.process(pcm, p.tr.ratio, make([]byte, 0, len(pcm)+len(pcm)/64+64))
	if resumed {
		// The first PCM after a pause starts on the clock: silence fills what is missing.
		if head := int64(due) - p.emitted - int64(len(out)/4); head > 0 {
			if head > maxSilenceBurst {
				p.origin -= float64(head - maxSilenceBurst)
				head = maxSilenceBurst
			}
			out = append(make([]byte, head*4, int(head)*4+len(out)), out...)
		}
	}
	p.emitted += int64(len(out) / 4)
	lead := float64(p.emitted) - (SampleRate*now.Seconds() + p.origin)
	p.origin += p.tr.observe(now, len(pcm)/4, lead)
	if len(out) == 0 {
		return nil
	}
	return out
}
