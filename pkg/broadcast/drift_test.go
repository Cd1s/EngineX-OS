package broadcast

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func sineFrames(start, n int, hz, amp float64) []byte {
	out := make([]byte, 0, n*4)
	for i := start; i < start+n; i++ {
		v := uint16(int16(math.Round(amp * math.Sin(2*math.Pi*hz*float64(i)/SampleRate))))
		out = binary.LittleEndian.AppendUint16(out, v)
		out = binary.LittleEndian.AppendUint16(out, v)
	}
	return out
}

func leftSamples(pcm []byte) []int16 {
	out := make([]int16, 0, len(pcm)/4)
	for i := 0; i+3 < len(pcm); i += 4 {
		out = append(out, int16(binary.LittleEndian.Uint16(pcm[i:])))
	}
	return out
}

// At a ratio of exactly 1 the resampler is a pure delay: every frame comes out
// bit for bit, both channels, whatever the chunking.
func TestResamplerUnityIsBitExact(t *testing.T) {
	in := testFrames(-20000, 40000) // crosses zero and both extremes of the pattern
	var rs resampler
	var out []byte
	for off, i := 0, 0; off < len(in); i++ {
		n := min([]int{4, 1920, 12, 4096, 9216}[i%5], len(in)-off)
		out = rs.process(in[off:off+n], 1, out)
		off += n
	}
	if want := len(in) - (rsTaps/2)*4; len(out) != want {
		t.Fatalf("got %d bytes, want %d (all but the %d frames still held)", len(out), want, rsTaps/2)
	}
	for i := range out {
		if out[i] != in[i] {
			t.Fatalf("byte %d differs: got %#x want %#x", i, out[i], in[i])
		}
	}
}

// A sine resampled at the measured deck ratio (and at a slow clock) is the same sine
// at the new positions: the error stays far below the signal, also near the top of
// the audio band, and digital silence stays digital silence.
func TestResamplerSineQuality(t *testing.T) {
	for _, tc := range []struct {
		hz, ratio, minSNR float64
	}{
		{100, 1.0014, 75}, {1000, 1.0014, 58}, {1000, 0.9995, 58},
		{10000, 1.0014, 48}, {10000, 0.9995, 48},
		{16000, 1.0014, 44},
	} {
		const amp = 16000.0
		var rs resampler
		out := leftSamples(rs.process(sineFrames(0, 48000, tc.hz, amp), tc.ratio, nil))
		var sig, noise float64
		for n, v := range out[100:] { // output n+100 reads input position (n+100)*ratio
			want := amp * math.Sin(2*math.Pi*tc.hz*float64(n+100)*tc.ratio/SampleRate)
			sig += want * want
			noise += (float64(v) - want) * (float64(v) - want)
		}
		snr := 10 * math.Log10(sig/noise)
		t.Logf("%.0f Hz at ratio %.4f: SNR %.1f dB", tc.hz, tc.ratio, snr)
		if snr < tc.minSNR {
			t.Errorf("%.0f Hz at ratio %.4f: SNR %.1f dB, want >= %.0f dB", tc.hz, tc.ratio, snr, tc.minSNR)
		}
	}
	var rs resampler
	rs.process(sineFrames(0, 4800, 1000, 16000), 1.0014, nil)
	rs.reset()
	for i, b := range rs.process(make([]byte, 4800*4), 1.0014, nil) {
		if b != 0 {
			t.Fatalf("silence in, byte %d = %#x out", i, b)
		}
	}
}

// simFeed drives a pacer the way the capture loop does, in simulated time. A hook
// writes every 10.1 ms what an audio clock running at rate has produced (a 1 kHz
// sine); the loop reads it with a deck's scheduling delays: mostly under 1 ms, some
// up to 8 ms, about 1% between 20 and 60 ms (the measured maximum).
type simFeed struct {
	t        *testing.T
	p        pacer
	rate     float64 // frames per second of the producer's audio clock
	loaded   bool    // a starved loop: half the reads 3-15 ms late, a tenth 20-60 ms
	produced int64   // frames written by the hook so far
	rng      uint64
	lastOut  int16
	hasOut   bool
	maxStep  float64   // largest sample-to-sample step published
	onTime   []simMark // (time, frames published) after reads that were not delayed
	maxChunk int
}

type simMark struct {
	t       time.Duration
	emitted int64
}

func (s *simFeed) rand() float64 {
	s.rng = s.rng*6364136223846793005 + 1442695040888963407
	return float64(s.rng>>11) / (1 << 53)
}

// publish checks what one step returned.
func (s *simFeed) publish(out []byte, checkSteps bool) {
	if len(out)%4 != 0 {
		s.t.Fatalf("published %d bytes: not whole frames", len(out))
	}
	s.maxChunk = max(s.maxChunk, len(out)/4)
	if !checkSteps {
		s.hasOut = false
		return
	}
	for _, v := range leftSamples(out) {
		if s.hasOut {
			s.maxStep = math.Max(s.maxStep, math.Abs(float64(v)-float64(s.lastOut)))
		}
		s.lastOut, s.hasOut = v, true
	}
}

// run feeds the pacer from from to to. With producing false the hook writes nothing
// (Engine is restarting) and the loop only makes its 24 ms keep-alive passes.
func (s *simFeed) run(from, to time.Duration, producing, checkSteps bool) {
	const amp = 12000.0
	if !producing {
		for now := from; now < to; now += captureTick {
			s.publish(s.p.step(nil, now), false)
		}
		s.produced = int64(s.rate * to.Seconds()) // the new writer starts at the present
		return
	}
	const period = 10100 * time.Microsecond
	last := from
	for slot := from; slot < to; slot += period {
		delay := 200*time.Microsecond + time.Duration(s.rand()*float64(time.Millisecond))
		switch r := s.rand(); {
		case r < 0.01:
			delay = 20*time.Millisecond + time.Duration(s.rand()*float64(40*time.Millisecond))
		case r < 0.15:
			delay = time.Millisecond + time.Duration(s.rand()*float64(7*time.Millisecond))
		}
		if s.loaded {
			delay = 200*time.Microsecond + time.Duration(s.rand()*float64(3*time.Millisecond))
			switch r := s.rand(); {
			case r < 0.10:
				delay = 20*time.Millisecond + time.Duration(s.rand()*float64(40*time.Millisecond))
			case r < 0.50:
				delay = 3*time.Millisecond + time.Duration(s.rand()*float64(12*time.Millisecond))
			}
		}
		now := slot + delay
		if now <= last {
			continue // still inside the previous delay: this write is read with a later one
		}
		last = now
		// Everything the hook wrote up to its last pass before now.
		written := int64(s.rate * (slot + period*((now-slot)/period)).Seconds())
		got := written > s.produced
		for written > s.produced {
			n := int(min(written-s.produced, 2304)) // one read of the capture loop
			s.publish(s.p.step(sineFrames(int(s.produced), n, 1000, amp), now), checkSteps)
			s.produced += int64(n)
		}
		if got && delay < 500*time.Microsecond {
			s.onTime = append(s.onTime, simMark{now, s.p.emitted})
		}
	}
}

// ppm is the published rate against the clock between the first on-time read after
// from and the last one.
func (s *simFeed) ppm(from time.Duration) float64 {
	var a simMark
	for _, m := range s.onTime {
		if m.t >= from {
			a = m
			break
		}
	}
	b := s.onTime[len(s.onTime)-1]
	return (float64(b.emitted-a.emitted)/(b.t-a.t).Seconds()/SampleRate - 1) * 1e6
}

// leadMS returns the range of the stream's position against the clock, at on-time
// reads from from on: how far the latency of a listener moves.
func (s *simFeed) leadMS(from time.Duration) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, m := range s.onTime {
		if m.t < from {
			continue
		}
		ms := (float64(m.emitted) - SampleRate*m.t.Seconds() - s.p.origin) / SampleRate * 1000
		lo, hi = math.Min(lo, ms), math.Max(hi, ms)
	}
	return lo, hi
}

// The live stream leaves the deck at 48000 frames per second of the system clock
// whatever the audio clock does. The real deck measured +1394 and +1475 ppm (the
// receiver's backlog grew 84 ms a minute); a slow clock must work as well. Over
// three simulated minutes the published rate stays within 20 ppm, the position
// against the clock stays within a few ms (bounded latency), and a 1 kHz sine never
// makes a larger step than a 1 kHz sine can (no click while the ratio moves).
func TestLiveStreamFollowsSystemClock(t *testing.T) {
	for _, ppm := range []float64{1400, -500, 0} {
		s := &simFeed{t: t, rate: SampleRate * (1 + ppm*1e-6), rng: 1, p: pacer{correct: true, tr: newDriftTracker(0, false)}}
		s.run(0, 180*time.Second, true, true)
		if got := s.ppm(30 * time.Second); math.Abs(got) > 20 {
			t.Errorf("%+.0f ppm producer: published rate %+.1f ppm off the clock, want within 20", ppm, got)
		}
		lo, hi := s.leadMS(0)
		t.Logf("%+.0f ppm producer: stream %+.2f ppm off the clock (30-180 s), measured offset %+.1f ppm, position %.2f..%.2f ms, max step %.0f", ppm, s.ppm(30*time.Second), s.p.tr.offset*1e6, lo, hi, s.maxStep)
		if got := s.p.tr.offset * 1e6; math.Abs(got-ppm) > 30 {
			t.Errorf("%+.0f ppm producer: measured %+.1f ppm", ppm, got)
		}
		if lo, hi := s.leadMS(0); lo < -10 || hi > 10 {
			t.Errorf("%+.0f ppm producer: stream position moved %.1f..%.1f ms against the clock, want within 10 ms", ppm, lo, hi)
		}
		if lo, hi := s.leadMS(30 * time.Second); lo < -4 || hi > 4 {
			t.Errorf("%+.0f ppm producer: position after lock %.1f..%.1f ms, want within 4 ms", ppm, lo, hi)
		}
		// A 12000-amplitude 1 kHz sine steps at most 12000*2*pi*1000/48000 = 1571 per frame.
		if limit := 12000 * 2 * math.Pi * 1000 / SampleRate * 1.01; s.maxStep > limit+2 {
			t.Errorf("%+.0f ppm producer: sample step %.0f, a continuous sine stays under %.0f", ppm, s.maxStep, limit)
		}
		// Bounded memory: the resampler keeps one read plus its taps, the tracker its window.
		if cap(s.p.rs.l) > 3*2304 || len(s.p.tr.pts) > driftPoints || len(s.p.tr.leads) > phaseWindows || cap(s.p.tr.scratch) > driftPoints*driftPoints {
			t.Errorf("state grew: resampler %d frames, %d points, %d leads, %d slopes", cap(s.p.rs.l), len(s.p.tr.pts), len(s.p.tr.leads), cap(s.p.tr.scratch))
		}
		if s.maxChunk > 2304+8 {
			t.Errorf("published a %d-frame chunk from 2304-frame reads", s.maxChunk)
		}
	}
}

// A loop that is starved most of the time (the deck runs stream-sync in the idle
// scheduling class next to Engine) still finds the clock: only one read a second
// has to be nearly on time. Bounds are wider than on an idle deck, never unbounded.
func TestLiveStreamFollowsClockOnLoadedDeck(t *testing.T) {
	s := &simFeed{t: t, rate: SampleRate * 1.0014, rng: 3, loaded: true, p: pacer{correct: true, tr: newDriftTracker(0, false)}}
	s.run(0, 180*time.Second, true, true)
	if got := s.ppm(30 * time.Second); math.Abs(got) > 40 {
		t.Errorf("published rate %+.1f ppm off the clock, want within 40", got)
	}
	if lo, hi := s.leadMS(0); lo < -15 || hi > 15 {
		t.Errorf("stream position moved %.1f..%.1f ms against the clock, want within 15 ms", lo, hi)
	}
	if limit := 12000 * 2 * math.Pi * 1000 / SampleRate * 1.01; s.maxStep > limit+2 {
		t.Errorf("sample step %.0f, a continuous sine stays under %.0f", s.maxStep, limit)
	}
	t.Logf("loaded deck: %+.1f ppm, measured offset %+.1f ppm, position %v", s.ppm(30*time.Second), s.p.tr.offset*1e6, func() [2]float64 { lo, hi := s.leadMS(0); return [2]float64{lo, hi} }())
}

// What the old capture loop did (publish the feed as it is): the stream runs at the
// audio clock's rate, +1400 ppm, and a listener's backlog grows without bound.
func TestUncorrectedFeedDrifts(t *testing.T) {
	s := &simFeed{t: t, rate: SampleRate * 1.0014, rng: 1, p: pacer{}}
	s.run(0, 60*time.Second, true, false)
	if got := s.ppm(5 * time.Second); got < 1300 {
		t.Fatalf("passthrough rate %+.0f ppm: the simulated producer is not fast", got)
	}
	if _, hi := s.leadMS(0); hi < 70 {
		t.Fatalf("passthrough backlog after a minute %.0f ms, the deck measured 84 ms a minute", hi)
	}
}

// Engine restarts: the feed stops for three seconds and a new writer starts. The
// stream keeps running on the clock through the pause (silence for exactly the
// missing time), the ratio continues from where it was instead of starting over,
// and the stream is back within 20 ppm afterwards.
func TestProducerGapKeepsClockAndRatio(t *testing.T) {
	s := &simFeed{t: t, rate: SampleRate * 1.0014, rng: 7, p: pacer{correct: true, tr: newDriftTracker(0, false)}}
	s.run(0, 90*time.Second, true, true)
	before := s.p.tr.ratio
	s.run(90*time.Second, 93*time.Second, false, false)
	if got := float64(s.p.emitted) - SampleRate*93 - s.p.origin; math.Abs(got) > 1152 {
		t.Fatalf("after a 3 s pause the stream is %.0f frames off the clock, want within one 24 ms pass", got)
	}
	marks := len(s.onTime)
	s.run(93*time.Second, 94*time.Second, true, false)
	if got := s.p.tr.ratio; math.Abs(got-before) > 50e-6 {
		t.Fatalf("ratio jumped across the pause: %.6f -> %.6f", before, got)
	}
	s.run(94*time.Second, 240*time.Second, true, true)
	if got := s.ppm(120 * time.Second); math.Abs(got) > 20 {
		t.Errorf("after the pause: %+.1f ppm off the clock, want within 20", got)
	}
	s.onTime = s.onTime[marks:]
	if lo, hi := s.leadMS(0); lo < -10 || hi > 10 {
		t.Errorf("after the pause the position moved %.1f..%.1f ms, want within 10 ms", lo, hi)
	}
	if s.p.origin != 0 {
		t.Errorf("the clock was re-anchored by %.0f frames; a pause must not move it", s.p.origin)
	}
	if limit := 12000 * 2 * math.Pi * 1000 / SampleRate * 1.01; s.maxStep > limit+2 {
		t.Errorf("sample step %.0f, a continuous sine stays under %.0f", s.maxStep, limit)
	}
}

// A capture loop that restarts (Live Stream reconfigured) starts from the offset
// measured before, not from 1.0, and does not accept a value that is not a clock.
func TestDriftTrackerSeed(t *testing.T) {
	if tr := newDriftTracker(1400e-6, true); tr.ratio != 1+1400e-6 || !tr.hasPrior {
		t.Fatalf("seeded tracker starts at ratio %.6f", tr.ratio)
	}
	if tr := newDriftTracker(0.4, true); tr.ratio != 1 || tr.hasPrior {
		t.Fatalf("an offset of 40%% was accepted as a clock: ratio %.6f", tr.ratio)
	}
}

// Keep-alive silence is counted on the clock: late or missed passes do not shorten
// the stream (the old loop sent one 24 ms block per pass it managed to make), and a
// long stop never produces a burst of more than a quarter second.
func TestKeepAliveSilenceFollowsClock(t *testing.T) {
	var p pacer
	var now time.Duration
	total := 0
	for i := 0; now < 10*time.Second; i++ {
		now += captureTick
		if i%50 == 49 {
			now += 70 * time.Millisecond // a starved pass
		}
		out := p.step(nil, now)
		for _, b := range out {
			if b != 0 {
				t.Fatal("keep-alive silence is not silent")
			}
		}
		total += len(out) / 4
	}
	if want := int(SampleRate * now.Seconds()); total < want-1 || total > want {
		t.Fatalf("published %d frames of silence in %v, want %d", total, now, want)
	}
	now += 30 * time.Second // suspended, or the loop was stopped by a debugger
	if out := p.step(nil, now); len(out)/4 > maxSilenceBurst {
		t.Fatalf("a %d-frame burst of silence after a long stop", len(out)/4)
	}
	if out := p.step(nil, now+captureTick); len(out)/4 != 1152 {
		t.Fatalf("after the stop: %d frames in one pass, want 1152", len(out)/4)
	}
}

// A source that is not a real-time clock (a writer at 40% speed, as in
// TestShortFeedGapIsNotFilledWithSilence) is never taken for a drifting clock: no
// lock, and the ratio stays within the small pull, so the audio is not bent audibly.
func TestSlowSourceNeverLocks(t *testing.T) {
	p := pacer{correct: true, tr: newDriftTracker(0, false)}
	in := testFrames(0, 1152)
	frames := 0
	for i := 0; i < 400; i++ { // 24 ms of audio every 60 ms, for 24 s
		out := p.step(in, time.Duration(i)*60*time.Millisecond)
		if len(out)%4 != 0 {
			t.Fatalf("published %d bytes: not whole frames", len(out))
		}
		frames += len(out) / 4
		if p.tr.locked || math.Abs(p.tr.ratio-1) > phaseMaxPull*1.001 {
			t.Fatalf("pass %d: ratio %.6f locked=%v for a source at 40%% of real time", i, p.tr.ratio, p.tr.locked)
		}
	}
	read := 400.0 * 1152
	if lo, hi := int(read*(1-phaseMaxPull))-rsTaps, int(read*(1+phaseMaxPull))+1; frames < lo || frames > hi {
		t.Fatalf("published %d frames for %d read, want %d..%d", frames, 400*1152, lo, hi)
	}
}

func BenchmarkResampler(b *testing.B) {
	in := sineFrames(0, 480, 1000, 12000) // one 10 ms write of the hook
	var rs resampler
	out := make([]byte, 0, 2048)
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out = rs.process(in, 1.0014, out[:0])
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/480, "ns/frame")
}

func BenchmarkPacerStep(b *testing.B) {
	in := sineFrames(0, 480, 1000, 12000)
	p := pacer{correct: true, tr: newDriftTracker(1400e-6, true)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.step(in, time.Duration(i)*10*time.Millisecond)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/480, "ns/frame")
}
