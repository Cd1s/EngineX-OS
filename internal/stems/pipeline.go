package stems

// Engine sends one block and waits for one reply. Relaying that over the
// network put a deck-to-renderer round trip inside every 1024-frame block,
// which held renders below real time. The pipeline answers each block on the
// deck at once with whatever separated frames have already come back (often
// none, which worker.py itself does for its first blocks), while the block
// goes upstream. After done the rest follows in maxBlock pieces, then
// returnFrames=0, as the worker's own drain does.

import (
	"fmt"
	"io"
	"strconv"
	"sync"
)

type pipeline struct {
	mu       sync.Mutex
	out      io.Writer // to Engine
	eng      *Session  // checks what Engine receives
	up, down stream
	maxBlock int
	frame    int // bytes per returned frame
	started  bool
	done     bool
	blocks   int  // blocks sent upstream
	awaiting bool // Engine's current block is still arriving
	owed     int  // replies due before the renderer said how wide they are
	replies  int  // renderer replies seen after started
	queued   bool
	queue    []byte
	final    bool
	err      error
}

func newPipeline(out io.Writer) *pipeline {
	p := &pipeline{out: out, eng: &Session{}}
	p.down.keep = true
	p.down.data = func(b []byte) {
		if p.queued {
			p.queue = append(p.queue, b...)
		}
	}
	return p
}

func (p *pipeline) write(b []byte) {
	if p.err == nil {
		p.eng.FeedOut(b)
		if _, e := p.out.Write(b); e != nil {
			p.err = e
		}
	}
}

// reply hands Engine up to n queued frames.
func (p *pipeline) reply(n int) {
	if p.frame > 0 {
		n = min(n, len(p.queue)/p.frame)
	} else {
		n = 0
	}
	b := n * p.frame
	p.write([]byte("returnFrames=" + strconv.Itoa(n) + "\n\n"))
	p.write(p.queue[:b])
	p.queue = p.queue[b:]
}

// flush sends every complete frame queued, in maxBlock pieces.
func (p *pipeline) flush() {
	for p.frame > 0 && len(p.queue) >= p.frame {
		p.reply(p.maxBlock)
	}
}

// fromEngine observes bytes Engine sends; they go upstream unchanged.
func (p *pipeline) fromEngine(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.eng.FeedIn(b)
	e := p.up.feed(b, func(f map[string]string) (int64, error) {
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
			return n * int64(p.eng.Channels) * 4, nil
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
		if p.frame == 0 {
			p.owed++
		} else {
			p.reply(p.maxBlock)
		}
	}
	return cmpErr(e, p.err)
}

// Write takes the renderer's stream. Headers other than replies (its
// greeting) go to Engine as sent; replies feed the queue.
func (p *pipeline) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.down.feed(b, func(f map[string]string) (int64, error) {
		v, ok := f["returnFrames"]
		p.queued = ok
		if !ok {
			p.write(p.down.raw)
			if c, has := f["numReturnChannels"]; has {
				n, err := strconv.Atoi(c)
				if err != nil || n <= 0 || n > 64 {
					return 0, fmt.Errorf("stems pipeline: bad numReturnChannels %q", c)
				}
				p.frame = n * 4
				for ; p.owed > 0; p.owed-- {
					p.reply(p.maxBlock)
				}
			}
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || p.frame == 0 || !(p.started || p.done) {
			return 0, fmt.Errorf("stems pipeline: unexpected returnFrames %q", v)
		}
		p.replies++
		if n == 0 && p.done && p.replies > p.blocks {
			p.final = true
		}
		return int64(n * p.frame), nil
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

type engineReader struct {
	r io.Reader
	p *pipeline
}

func (r engineReader) Read(b []byte) (int, error) {
	n, e := r.r.Read(b)
	if n > 0 {
		if err := r.p.fromEngine(b[:n]); err != nil {
			return 0, err
		}
	}
	return n, e
}
