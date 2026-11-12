package broadcast

import (
	"context"
	"sync"
)

// hub fans captured PCM out to local HTTP listeners (/live.mp3, /live.wav).
type hub struct {
	mu        sync.Mutex
	subs      map[chan []byte]struct{}
	listeners map[*Listener]struct{}
	history   []byte // the most recent MaxBufferMS of published PCM, whole frames
}

// Live listener cushion (/live.wav ?buffer=): on connect a listener first gets the
// most recent BufferMS of audio, so a player starts with a cushion that rides gaps
// in delivery; then live audio. A listener more than BufferMS+TrimSlackMS behind (a
// stalled network) has its oldest queued audio dropped back to BufferMS, so latency
// stays bounded instead of growing with every stall.
//
// DefaultBufferMS is tuned for a wired network (or Wi-Fi without AWDL on the
// receiver), where the network adds almost no jitter: what is left is the deck's own
// scheduling jitter, measured at 60 ms at most between deliveries, so 120 ms is
// twice that. The stream leaves the deck on the system clock (see pacer), so the
// cushion no longer has to absorb drift. Wi-Fi with gaps needs ?buffer=300 or more.
const (
	MaxBufferMS     = 2000
	DefaultBufferMS = 120
	TrimSlackMS     = 200
	bytesPerMS      = SampleRate * 4 / 1000 // 192 bytes: s16le stereo at 48 kHz
)

// msBytes converts milliseconds of audio to whole-frame bytes.
func msBytes(ms int) int { return ms * bytesPerMS }

// Listener is one local live listener with a byte queue the hub can trim.
type Listener struct {
	mu     sync.Mutex
	queue  []byte
	target int // bytes kept after a trim (BufferMS)
	limit  int // a queue above this is trimmed (BufferMS + TrimSlackMS)
	ready  chan struct{}
	done   chan struct{}
	closed bool
	// Dropped counts bytes trimmed from this listener (whole frames).
	Dropped int64
}

// SubscribeLive registers a listener with a bufferMS cushion (clamped to 0..MaxBufferMS).
func (b *Broadcaster) SubscribeLive(bufferMS int) (*Listener, func()) {
	return b.hub.subscribeLive(bufferMS)
}

func (h *hub) subscribeLive(bufferMS int) (*Listener, func()) {
	bufferMS = min(max(bufferMS, 0), MaxBufferMS)
	l := &Listener{target: msBytes(bufferMS), limit: msBytes(bufferMS + TrimSlackMS),
		ready: make(chan struct{}, 1), done: make(chan struct{})}
	h.mu.Lock()
	if h.listeners == nil {
		h.listeners = map[*Listener]struct{}{}
	}
	h.listeners[l] = struct{}{}
	if n := min(l.target, len(h.history)); n > 0 {
		l.queue = append([]byte(nil), h.history[len(h.history)-n:]...)
		l.ready <- struct{}{}
	}
	h.mu.Unlock()
	return l, func() {
		h.mu.Lock()
		if _, ok := h.listeners[l]; ok {
			delete(h.listeners, l)
			l.close()
		}
		h.mu.Unlock()
	}
}

func (l *Listener) close() {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.done)
	}
	l.mu.Unlock()
}

// push appends whole frames and trims a backlog beyond limit back to target.
func (l *Listener) push(pcm []byte) {
	l.mu.Lock()
	l.queue = append(l.queue, pcm...)
	if len(l.queue) > l.limit {
		drop := len(l.queue) - l.target
		drop -= drop % 4 // whole frames only (the queue holds whole frames)
		l.Dropped += int64(drop)
		l.queue = append(l.queue[:0:0], l.queue[drop:]...)
	}
	l.mu.Unlock()
	select {
	case l.ready <- struct{}{}:
	default:
	}
}

// Next waits for queued audio and takes all of it; ok is false once the listener
// is closed (the broadcaster stopped) or ctx ends.
func (l *Listener) Next(ctx context.Context) (pcm []byte, ok bool) {
	for {
		l.mu.Lock()
		if len(l.queue) > 0 {
			pcm, l.queue = l.queue, nil
			l.mu.Unlock()
			return pcm, true
		}
		closed := l.closed
		l.mu.Unlock()
		if closed {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-l.done:
		case <-l.ready:
		}
	}
}

// Subscribe returns a stream of raw 16-bit stereo PCM chunks at SampleRate (48 kHz) and a cancel func.
// The channel is closed when the broadcaster stops; slow listeners drop chunks.
func (b *Broadcaster) Subscribe() (<-chan []byte, func()) {
	return b.hub.subscribe()
}

func (h *hub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 160) // about 1.5 s of ~10 ms chunks, as 64 was with 24 ms ones
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan []byte]struct{}{}
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

func (h *hub) publish(pcm []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.history = append(h.history, pcm...)
	if keep := msBytes(MaxBufferMS); len(h.history) > keep+keep/2 { // amortised trim
		h.history = append(h.history[:0:0], h.history[len(h.history)-keep:]...)
	}
	for l := range h.listeners {
		l.push(pcm)
	}
	for ch := range h.subs {
		select {
		case ch <- pcm:
		default:
		}
	}
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) + len(h.listeners)
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		close(ch)
		delete(h.subs, ch)
	}
	for l := range h.listeners {
		l.close()
		delete(h.listeners, l)
	}
	h.history = nil
}
