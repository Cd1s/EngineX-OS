package stems

// The separator behind LocalRender. It works on a short-time Fourier
// transform (a 93 ms Hann window, three quarters overlap) and gives every
// bin of every frame to the stems by what the bin looks like:
//
//   - drums: energy spread across frequency rather than held in time (a
//     median along each axis estimates the two, as in harmonic-percussive
//     separation), and below 150 Hz whatever rises above the held level,
//     which is the kick;
//   - bass: what is held below 190 Hz, weighted towards the centre;
//   - vocals: what is held (judged over a shorter time, so vibrato stays)
//     between 180 Hz and 7 kHz and is common to both channels; it is taken
//     from the mid signal, so sounds panned to one side never get in;
//   - melody: the exact remainder, so the four stems sum to the input.
//
// It is not a neural separator. The constants were tuned for the highest
// signal-to-distortion ratio over the MUSDB18 excerpts.

import (
	"math"
	"math/bits"
)

// rfft transforms a power-of-two number of real samples through a complex
// FFT of half the size.
type rfft struct {
	rev    []int          // bit reversal for the half-size transform
	tw     [][2][]float32 // per stage from size 8 up: cos and -sin
	pr, pi []float32      // e^(-2πik/n) for k up to n/2
	re, im []float32      // work area
}

func newRFFT(n int) *rfft {
	h := n / 2
	f := &rfft{rev: make([]int, h), pr: make([]float32, h+1), pi: make([]float32, h+1), re: make([]float32, h), im: make([]float32, h)}
	shift := 32 - (bits.Len(uint(h)) - 1)
	for i := range f.rev {
		f.rev[i] = int(bits.Reverse32(uint32(i)) >> shift)
	}
	for size := 8; size <= h; size <<= 1 {
		c, s := make([]float32, size/2), make([]float32, size/2)
		for j := range c {
			sin, cos := math.Sincos(2 * math.Pi * float64(j) / float64(size))
			c[j], s[j] = float32(cos), float32(-sin)
		}
		f.tw = append(f.tw, [2][]float32{c, s})
	}
	for k := range f.pr {
		sin, cos := math.Sincos(2 * math.Pi * float64(k) / float64(n))
		f.pr[k], f.pi[k] = float32(cos), float32(-sin)
	}
	return f
}

// fft transforms re, im in place; they hold the input in bit-reversed order.
func (f *rfft) fft() {
	re, im := f.re, f.im
	for i := 0; i+3 < len(re); i += 4 { // sizes 2 and 4
		ar, ai, br, bi := re[i]+re[i+1], im[i]+im[i+1], re[i]-re[i+1], im[i]-im[i+1]
		cr, ci, dr, di := re[i+2]+re[i+3], im[i+2]+im[i+3], re[i+2]-re[i+3], im[i+2]-im[i+3]
		re[i], im[i], re[i+2], im[i+2] = ar+cr, ai+ci, ar-cr, ai-ci
		re[i+1], im[i+1], re[i+3], im[i+3] = br+di, bi-dr, br-di, bi+dr
	}
	s, size := 0, 8
	for ; 2*size <= len(re); s, size = s+2, 4*size { // sizes size and 2*size in one pass
		half := size / 2
		w1r, w1i := f.tw[s][0][:half], f.tw[s][1][:half]
		w2r, w2i := f.tw[s+1][0][:half], f.tw[s+1][1][:half]
		for i := 0; i < len(re); i += 2 * size {
			ar, ai := re[i:i+half], im[i:i+half]
			br, bi := re[i+half:i+size], im[i+half:i+size]
			cr, ci := re[i+size:i+size+half], im[i+size:i+size+half]
			dr, di := re[i+size+half:i+2*size], im[i+size+half:i+2*size]
			for j := range w1r {
				tr, ti := w1r[j]*br[j]-w1i[j]*bi[j], w1r[j]*bi[j]+w1i[j]*br[j]
				ur, ui := w1r[j]*dr[j]-w1i[j]*di[j], w1r[j]*di[j]+w1i[j]*dr[j]
				pr, pi, qr, qi := ar[j]+tr, ai[j]+ti, ar[j]-tr, ai[j]-ti
				xr, xi, yr, yi := cr[j]+ur, ci[j]+ui, cr[j]-ur, ci[j]-ui
				// In the larger size the second pair turns a quarter further: by -i.
				tr, ti = w2r[j]*xr-w2i[j]*xi, w2r[j]*xi+w2i[j]*xr
				ur, ui = w2r[j]*yi+w2i[j]*yr, w2i[j]*yi-w2r[j]*yr
				ar[j], ai[j], cr[j], ci[j] = pr+tr, pi+ti, pr-tr, pi-ti
				br[j], bi[j], dr[j], di[j] = qr+ur, qi+ui, qr-ur, qi-ui
			}
		}
	}
	if size <= len(re) { // an odd number of sizes: the last one alone
		half := size / 2
		wr, wi := f.tw[s][0][:half], f.tw[s][1][:half]
		ar, ai, br, bi := re[:half], im[:half], re[half:], im[half:]
		for j := range wr {
			tr, ti := wr[j]*br[j]-wi[j]*bi[j], wr[j]*bi[j]+wi[j]*br[j]
			br[j], bi[j] = ar[j]-tr, ai[j]-ti
			ar[j], ai[j] = ar[j]+tr, ai[j]+ti
		}
	}
}

// forward writes the n/2+1 bins of the n samples x to re, im. With z the
// half-size transform of x's even samples plus i times its odd ones, the even
// and odd spectra are (z[k] ± conj z[-k])/2, the odd one divided by i.
func (f *rfft) forward(x, re, im []float32) {
	h := len(f.re)
	for j, r := range f.rev {
		f.re[r], f.im[r] = x[2*j], x[2*j+1]
	}
	f.fft()
	for k := 0; k <= h; k++ {
		a, b := f.re[k&(h-1)], f.im[k&(h-1)]
		c, d := f.re[(h-k)&(h-1)], f.im[(h-k)&(h-1)]
		qr, qi := (b+d)/2, (c-a)/2
		re[k] = (a+c)/2 + f.pr[k]*qr - f.pi[k]*qi
		im[k] = (b-d)/2 + f.pr[k]*qi + f.pi[k]*qr
	}
}

// inverse writes to x the n samples whose bins are re, im, times n/2.
func (f *rfft) inverse(re, im, x []float32) {
	h := len(f.re)
	for k, r := range f.rev {
		dr, di := (re[k]-re[h-k])/2, (im[k]+im[h-k])/2
		qr, qi := dr*f.pr[k]+di*f.pi[k], di*f.pr[k]-dr*f.pi[k]
		// The forward transform of a conjugate is the conjugate of the inverse.
		f.re[r], f.im[r] = (re[k]+re[h-k])/2-qi, -((im[k]-im[h-k])/2 + qr)
	}
	f.fft()
	for j := 0; j < h; j++ {
		x[2*j], x[2*j+1] = f.re[j], -f.im[j]
	}
}

// resort replaces old by v in the ascending slice s. The search is bounded,
// so values that do not compare (NaN) cannot run it off the end.
func resort(s []float32, old, v float32) {
	i := 0
	for i < len(s)-1 && s[i] != old {
		i++
	}
	for ; i < len(s)-1 && s[i+1] < v; i++ {
		s[i] = s[i+1]
	}
	for ; i > 0 && s[i-1] > v; i-- {
		s[i] = s[i-1]
	}
	s[i] = v
}

// timeMedian is, for every bin, the median of the last w frames.
type timeMedian struct {
	w, at        int
	ring, sorted []float32 // bins × w: in arrival order, in ascending order
}

func newTimeMedian(bins, w int) *timeMedian {
	return &timeMedian{w: w, ring: make([]float32, bins*w), sorted: make([]float32, bins*w)}
}

func (m *timeMedian) push(frame, median []float32) {
	for k, v := range frame {
		s := m.sorted[k*m.w : (k+1)*m.w]
		resort(s, m.ring[k*m.w+m.at], v)
		m.ring[k*m.w+m.at] = v
		median[k] = s[m.w/2]
	}
	m.at = (m.at + 1) % m.w
}

// medianAcross writes to median the median of a over the len(s) bins around
// each bin; past either end the edge bin repeats.
func medianAcross(a, median, s []float32) {
	half, last := len(s)/2, len(a)-1
	for i := range s {
		s[i] = a[0]
	}
	for k := 1; k <= half; k++ {
		resort(s, a[0], a[min(k, last)])
	}
	median[0] = s[half]
	for k := 1; k <= last; k++ {
		resort(s, a[max(k-half-1, 0)], a[min(k+half, last)])
		median[k] = s[half]
	}
}

// spectrum is one analysed frame.
type spectrum struct {
	re, im [][]float32 // per channel
	mag    []float32   // root mean square over the channels
}

type separator struct {
	ch, n, hop int
	f          *rfft
	win, syn   []float32 // analysis window; synthesis window with the scaling folded in
	// Per bin: the share below the bass crossover, in the vocal band above
	// it, and in the kick band.
	low, voice, kick []float32

	in     [][]float32 // per channel: input not yet returned, after n-hop frames of silence
	frames int         // frames analysed
	ring   []spectrum  // the newest look+1 of them
	// The medians over time are centred on the frame being separated: look
	// and near are the frames each needs after it.
	look, near  int
	long, short *timeMedian
	// Per bin, for the frame being separated: its medians over the long
	// time, over the short time and across frequency, then its masks.
	held, heldNear, across, drums, bass []float32
	scratch                             []float32 // medianAcross's window

	frame      []float32      // n samples
	spec       [][2][]float32 // spectra to synthesise: vocals, then bass and drums per channel
	ola        [][]float32    // their overlap-add, n samples each
	skip, owed int            // leading frames to drop; frames received and not yet queued
	out        [][]float32    // queued output per stem and channel
}

func newSeparator(ch int, rate float64) *separator {
	n := 1 << int(math.Round(math.Log2(rate*0.093)))
	hop, bins := n/4, n/2+1
	perSecond := rate / float64(hop)
	// "Held" means present for most of 0.4 s; for the voice, of 0.15 s.
	long, near := int(0.4*perSecond)|1, int(0.15*perSecond)|1
	s := &separator{ch: ch, n: n, hop: hop, f: newRFFT(n), win: make([]float32, n), syn: make([]float32, n),
		low: make([]float32, bins), voice: make([]float32, bins), kick: make([]float32, bins),
		in: make([][]float32, ch), ring: make([]spectrum, long/2+1), look: long / 2, near: near / 2,
		long: newTimeMedian(bins, long), short: newTimeMedian(bins, near),
		held: make([]float32, bins), heldNear: make([]float32, bins), across: make([]float32, bins),
		drums: make([]float32, bins), bass: make([]float32, bins),
		scratch: make([]float32, int(330*float64(n)/rate)|1), frame: make([]float32, n),
		spec: make([][2][]float32, 1+2*ch), ola: make([][]float32, 1+2*ch), skip: n - hop, out: make([][]float32, 4*ch)}
	for i := range s.win {
		s.win[i] = float32(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n)))
		s.syn[i] = s.win[i] / (1.5 * float32(n/2)) // squared Hann windows a quarter apart sum to 1.5
	}
	for k := range s.low {
		f := max(float64(k)*rate/float64(n), 1)
		s.low[k] = float32(1 / (1 + math.Pow(f/190, 8)))
		s.voice[k] = (1 - s.low[k]) * float32(1/((1+math.Pow(180/f, 6))*(1+math.Pow(f/7000, 6))))
		s.kick[k] = float32(1 / (1 + math.Pow(f/150, 8)))
	}
	for c := range s.in {
		s.in[c] = make([]float32, n-hop)
	}
	for i := range s.ring {
		s.ring[i] = spectrum{re: make([][]float32, ch), im: make([][]float32, ch), mag: make([]float32, bins)}
		for c := 0; c < ch; c++ {
			s.ring[i].re[c], s.ring[i].im[c] = make([]float32, bins), make([]float32, bins)
		}
	}
	for i := range s.spec {
		s.spec[i], s.ola[i] = [2][]float32{make([]float32, bins), make([]float32, bins)}, make([]float32, n)
	}
	return s
}

// run analyses every frame the input now covers. A frame is separated once
// look frames after it are known, and each separated frame queues hop frames.
func (s *separator) run() {
	for {
		at := min(s.frames, s.look) * s.hop // the next frame's place in s.in
		if len(s.in[0]) < at+s.n {
			return
		}
		sp := &s.ring[s.frames%len(s.ring)]
		for c, x := range s.in {
			for i, w := range s.win {
				s.frame[i] = w * x[at+i]
			}
			s.f.forward(s.frame, sp.re[c], sp.im[c])
		}
		clear(sp.mag)
		for c := range s.in {
			re, im := sp.re[c], sp.im[c]
			for k := range sp.mag {
				sp.mag[k] += re[k]*re[k] + im[k]*im[k]
			}
		}
		for k, p := range sp.mag {
			sp.mag[k] = sqrt32(p / float32(s.ch))
		}
		s.long.push(sp.mag, s.held)
		if lag := s.look - s.near; s.frames >= lag {
			s.short.push(s.ring[(s.frames-lag)%len(s.ring)].mag, s.heldNear)
		}
		if s.frames >= s.look {
			s.separate(&s.ring[(s.frames-s.look)%len(s.ring)])
		}
		s.frames++
	}
}

func sqrt32(x float32) float32 { return float32(math.Sqrt(float64(x))) }

// separate masks one frame, whose medians are in held and heldNear, and
// queues the hop frames it completes.
func (s *separator) separate(sp *spectrum) {
	const tiny = 1e-20
	medianAcross(sp.mag, s.across, s.scratch)
	vr, vi := s.spec[0][0], s.spec[0][1]
	lr, li, rr, ri := sp.re[0], sp.im[0], sp.re[s.ch-1], sp.im[s.ch-1]
	for k, a := range sp.mag {
		h, p2 := s.held[k], s.across[k]*s.across[k]
		// Percussive: spread across frequency at 1.4 times what is held in
		// time or more. In the kick band: what rises above the held level.
		drums := p2 / (p2 + 1.96*h*h + tiny)
		if w := s.kick[k]; w > 1e-4 {
			rise := max(a-h, 0)
			drums += w * (rise*rise/(rise*rise+h*h+tiny) - drums)
		}
		// Tonal enough for a voice: held, over the short time, at half the spread.
		near := s.heldNear[k]
		tonal := 1 - p2/(p2+4*near*near+tiny)
		// One channel has no centre to find: it gives 0.7 of what is tonal to the vocals.
		mr, mi, centre, bass := lr[k], li[k], float32(0.7), (1-drums)*s.low[k]
		if s.ch == 2 {
			mr, mi = (mr+rr[k])/2, (mi+ri[k])/2
			sr, si := lr[k]-mr, li[k]-mi
			mid, side := sqrt32(mr*mr+mi*mi), sqrt32(sr*sr+si*si)
			centre = max(mid-side, 0) / (mid + side + 1e-10)
			bass *= sqrt32(centre)
		}
		v := tonal * s.voice[k] * centre
		vr[k], vi[k], s.drums[k], s.bass[k] = v*mr, v*mi, drums, bass
	}
	for c := 0; c < s.ch; c++ {
		re, im := sp.re[c], sp.im[c]
		br, bi, dr, di := s.spec[1+c][0], s.spec[1+c][1], s.spec[1+s.ch+c][0], s.spec[1+s.ch+c][1]
		for k, bass := range s.bass {
			br[k], bi[k] = bass*re[k], bass*im[k]
			dr[k], di[k] = s.drums[k]*(re[k]-vr[k]), s.drums[k]*(im[k]-vi[k])
		}
	}
	for i, part := range s.spec {
		s.f.inverse(part[0], part[1], s.frame)
		for j, w := range s.syn {
			s.ola[i][j] += w * s.frame[j]
		}
	}
	take := s.hop
	if s.skip > 0 {
		s.skip -= s.hop
		take = 0
	}
	take = min(take, s.owed)
	s.owed -= take
	for c := 0; c < s.ch; c++ {
		v, b, d := s.ola[0][:take], s.ola[1+c][:take], s.ola[1+s.ch+c][:take]
		x := s.in[c][:take]
		s.out[c] = append(s.out[c], v...)
		s.out[s.ch+c] = append(s.out[s.ch+c], b...)
		s.out[2*s.ch+c] = append(s.out[2*s.ch+c], d...)
		for i := range x {
			s.out[3*s.ch+c] = append(s.out[3*s.ch+c], x[i]-v[i]-b[i]-d[i])
		}
		s.in[c] = s.in[c][s.hop:]
	}
	for _, o := range s.ola {
		copy(o, o[s.hop:])
		clear(o[s.n-s.hop:])
	}
}

// drain separates what is still owed, as if silence followed the input.
func (s *separator) drain() {
	silence := make([]float32, s.hop)
	for s.owed > 0 {
		for c := range s.in {
			s.in[c] = append(s.in[c], silence...)
		}
		s.run()
	}
}
