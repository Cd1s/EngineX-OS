package broadcast

import "sync"

// hub fans captured PCM out to local HTTP listeners (/live.mp3, /live.wav).
type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// Subscribe returns a stream of raw 16-bit stereo PCM chunks (44.1kHz or 48kHz) and a cancel func.
// The channel is closed when the broadcaster stops; slow listeners drop chunks.
func (b *Broadcaster) Subscribe() (<-chan []byte, func()) {
	return b.hub.subscribe()
}

func (h *hub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
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
	return len(h.subs)
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		close(ch)
		delete(h.subs, ch)
	}
}
