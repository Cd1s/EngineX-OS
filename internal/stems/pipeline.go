package stems

// Engine sends one block and waits for one reply. Relaying that over the
// network put a deck-to-renderer round trip inside every 1024-frame block,
// which held renders below real time. The pipeline answers each block on the
// deck at once with whatever separated frames have already come back (often
// none, which worker.py itself does for its first blocks), while the block
// goes upstream. After done the rest follows in maxBlock pieces, then
// returnFrames=0, as the worker's own drain does.
//
// Replies are planar (each return channel's frames in turn), so frames are
// queued per plane and every reply to Engine is reassembled plane by plane.

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"sync"
)

type pipeline struct {
	mu       sync.Mutex
	out      io.Writer // to Engine
	eng      *Session  // checks what Engine receives
	up, down stream
	maxBlock int
	packed   bool     // replies carry three int16 stems; melody is rebuilt here (assumed until the renderer answers)
	nplanes  int      // numReturnChannels
	planes   [][]byte // queued float32 frames per return channel
	input    [][]byte // with packed: Engine's float32 frames per channel not yet matched
	cur, in  []byte   // the renderer reply, Engine block being received
	want     int      // its payload size
	inWant   int
	upOut    []byte // bytes for the renderer from the current read
	started  bool
	done     bool
	blocks   int  // blocks sent upstream
	awaiting bool // Engine's current block is still arriving
	owed     int  // replies due before the renderer said how wide they are
	replies  int  // renderer replies seen after started
	final    bool
	err      error
}

func newPipeline(out io.Writer) *pipeline {
	p := &pipeline{out: out, eng: &Session{}, packed: true}
	p.down.keep, p.up.keep = true, true
	p.down.data = func(b []byte) {
		if p.cur = append(p.cur, b...); len(p.cur) == p.want {
			p.queue()
		}
	}
	p.up.data = func(b []byte) {
		if !p.packed {
			p.upOut = append(p.upOut, b...)
		} else if p.in = append(p.in, b...); len(p.in) == p.inWant {
			for c, n := 0, len(p.in)/len(p.input); c < len(p.input); c++ {
				p.input[c] = append(p.input[c], p.in[c*n:(c+1)*n]...)
			}
			at, n := len(p.upOut), len(p.in)/2
			p.upOut = slices.Grow(p.upOut, n)[:at+n]
			packSamples(p.upOut[at:], p.in)
		}
	}
	return p
}

// decide records whether the renderer packs. Uploads are packed from the
// start without waiting for its answer (an edge proxy may hold the answer
// until the upload arrives); Engine sends no block before the renderer's
// greeting, so an older renderer is normally known before any block went up.
func (p *pipeline) decide(packed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !packed && p.packed && p.blocks > 0 {
		p.err = fmt.Errorf("stems pipeline: renderer cannot unpack blocks already sent; update sc5000-stemsd")
	}
	if p.packed = packed; !packed {
		p.input = nil
	}
}

func (p *pipeline) unpacked() { p.decide(false) }

// queue appends a complete renderer reply to the planes.
func (p *pipeline) queue() {
	if !p.packed {
		n := len(p.cur) / p.nplanes
		for i := range p.planes {
			p.planes[i] = append(p.planes[i], p.cur[i*n:(i+1)*n]...)
		}
		return
	}
	ch := p.nplanes / 4
	n := len(p.cur) / (3 * ch * 2) // frames
	for i := 0; i < 3*ch; i++ {
		p.planes[i] = slices.Grow(p.planes[i], n*4)
		for k := 0; k < n; k++ {
			p.planes[i] = binary.LittleEndian.AppendUint32(p.planes[i], math.Float32bits(unpackSample(p.cur[(i*n+k)*2:])))
		}
	}
	f32 := func(b []byte, k int) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(b[k*4:])) }
	for c := 0; c < ch; c++ {
		x, base := p.input[c], len(p.planes[c])/4-n
		p.planes[3*ch+c] = slices.Grow(p.planes[3*ch+c], n*4)
		for k := 0; k < n; k++ {
			m := f32(x, k) - f32(p.planes[c], base+k) - f32(p.planes[ch+c], base+k) - f32(p.planes[2*ch+c], base+k)
			p.planes[3*ch+c] = binary.LittleEndian.AppendUint32(p.planes[3*ch+c], math.Float32bits(m))
		}
		p.input[c] = x[n*4:]
	}
}

func (p *pipeline) write(b []byte) {
	if p.err == nil {
		p.eng.FeedOut(b)
		if _, e := p.out.Write(b); e != nil {
			p.err = e
		}
	}
}

func (p *pipeline) queued() int {
	if len(p.planes) == 0 {
		return 0
	}
	return len(p.planes[0]) / 4
}

// reply hands Engine up to n queued frames.
func (p *pipeline) reply(n int) {
	n = min(n, p.queued())
	p.write([]byte("returnFrames=" + strconv.Itoa(n) + "\n\n"))
	for i := range p.planes {
		p.write(p.planes[i][:n*4])
		p.planes[i] = p.planes[i][n*4:]
	}
}

// flush sends every queued frame, in maxBlock pieces.
func (p *pipeline) flush() {
	for p.queued() > 0 {
		p.reply(p.maxBlock)
	}
}

// fromEngine takes bytes Engine sends and returns what goes to the renderer:
// the same bytes, or with packing each block's PCM as int16.
func (p *pipeline) fromEngine(b []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.eng.FeedIn(b)
	p.upOut = nil
	e := p.up.feed(b, func(f map[string]string) (int64, error) {
		p.upOut = append(p.upOut, p.up.raw...)
		if p.maxBlock == 0 {
			n, err := strconv.Atoi(f["maxBlockSize"])
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("stems pipeline: bad init %v", f)
			}
			p.maxBlock = n
			return 0, nil
		}
		switch f["action"] {
		case "block":
			n, err := count(f, "numSamples", int64(p.maxBlock))
			if err != nil {
				return 0, err
			}
			p.started, p.awaiting = true, true
			p.blocks++
			if p.input == nil && p.packed {
				p.input = make([][]byte, p.eng.Channels)
			}
			p.in, p.inWant = p.in[:0], int(n)*p.eng.Channels*4
			return int64(p.inWant), nil
		case "done":
			p.done = true
			if p.owed == 0 {
				p.flush()
			}
		}
		return 0, nil
	})
	if e == nil && p.awaiting && p.up.skip == 0 {
		p.awaiting = false
		if p.nplanes == 0 {
			p.owed++
		} else {
			p.reply(p.maxBlock)
		}
	}
	return p.upOut, cmpErr(e, p.err)
}

// Write takes the renderer's stream. Headers other than replies (its
// greeting) go to Engine as sent; replies feed the queue.
func (p *pipeline) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.down.feed(b, func(f map[string]string) (int64, error) {
		v, ok := f["returnFrames"]
		if !ok {
			p.write(p.down.raw)
			if c, has := f["numReturnChannels"]; has {
				n, err := strconv.Atoi(c)
				if err != nil || n <= 0 || n > 64 || n%4 != 0 {
					return 0, fmt.Errorf("stems pipeline: bad numReturnChannels %q", c)
				}
				p.nplanes, p.planes = n, make([][]byte, n)
				for ; p.owed > 0; p.owed-- {
					p.reply(p.maxBlock)
				}
			}
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || p.nplanes == 0 || !(p.started || p.done) {
			return 0, fmt.Errorf("stems pipeline: unexpected returnFrames %q", v)
		}
		if p.packed && n > 0 && (p.input == nil || n > len(p.input[0])/4) {
			return 0, fmt.Errorf("stems pipeline: %d frames returned beyond the input", n)
		}
		p.replies++
		if n == 0 && p.done && p.replies > p.blocks {
			p.final = true
		}
		p.want, p.cur = n*p.nplanes*4, p.cur[:0]
		if p.packed {
			p.want = n * p.nplanes * 3 / 4 * 2
		}
		return int64(p.want), nil
	})
	if e == nil && p.done {
		p.flush()
		if p.final {
			p.write([]byte("returnFrames=0\n\n"))
			p.done = false // the session is over; nothing more to flush
		}
	}
	return len(b), cmpErr(e, p.err)
}

// Complete reports whether Engine got a whole session.
func (p *pipeline) Complete() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if (p.started || p.done) && !p.final {
		return fmt.Errorf("stems pipeline: renderer ended before its final reply")
	}
	return p.eng.Complete()
}

func cmpErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// engineReader is the upload: Engine's stream as fromEngine rewrites it.
type engineReader struct {
	r       io.Reader
	p       *pipeline
	buf     []byte
	pending []byte
	err     error
}

func (r *engineReader) Read(b []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		n, e := r.r.Read(r.buf)
		if n > 0 {
			up, err := r.p.fromEngine(r.buf[:n])
			if err != nil {
				return 0, err
			}
			r.pending = up
		}
		r.err = e
	}
	n := copy(b, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
