package broadcast

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// publishFrames publishes ms of distinct frames in 24 ms chunks and returns the stream.
func publishFrames(h *hub, start, ms int) []byte {
	stream := testFrames(start, ms*SampleRate/1000)
	for off := 0; off < len(stream); off += 1152 * 4 {
		h.publish(stream[off:min(off+1152*4, len(stream))])
	}
	return stream
}

func nextWithin(l *Listener, d time.Duration) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return l.Next(ctx)
}

// A new listener starts with exactly the most recent ?buffer= ms of audio.
func TestLivePrefillMatchesBuffer(t *testing.T) {
	h := &hub{}
	stream := publishFrames(h, 0, 1000)
	l, cancel := h.subscribeLive(300)
	defer cancel()
	pcm, ok := nextWithin(l, time.Second)
	if !ok || len(pcm) != 300*192 || !bytes.Equal(pcm, stream[len(stream)-300*192:]) {
		t.Fatalf("prefill %d bytes, want the last %d bytes of the stream", len(pcm), 300*192)
	}
	// Asking for more than has been played gives all of it, still whole frames.
	l2, cancel2 := h.subscribeLive(MaxBufferMS)
	defer cancel2()
	if pcm, ok := nextWithin(l2, time.Second); !ok || !bytes.Equal(pcm, stream) {
		t.Fatalf("prefill %d bytes, want the whole %d-byte history", len(pcm), len(stream))
	}
}

// buffer=0 is the wired/lowest-latency mode: no history, only what plays from now on.
func TestLiveBufferZeroSendsNoHistory(t *testing.T) {
	h := &hub{}
	publishFrames(h, 0, 1000)
	l, cancel := h.subscribeLive(0)
	defer cancel()
	if pcm, ok := nextWithin(l, 50*time.Millisecond); ok {
		t.Fatalf("buffer=0 sent %d bytes of history", len(pcm))
	}
	live := testFrames(9000, 1152)
	h.publish(live)
	if pcm, ok := nextWithin(l, time.Second); !ok || !bytes.Equal(pcm, live) {
		t.Fatalf("got %d bytes, want the live chunk", len(pcm))
	}
}

// A stalled listener (Wi-Fi stall) never piles up more than buffer+200 ms: its oldest
// audio is dropped back to buffer ms, it resumes on a frame boundary of the real
// stream, and the following audio continues without a gap.
func TestStalledLiveListenerIsTrimmed(t *testing.T) {
	h := &hub{}
	l, cancel := h.subscribeLive(300)
	defer cancel()
	stream := publishFrames(h, 0, 2000) // 2 s without reading
	pcm, ok := nextWithin(l, time.Second)
	if !ok || len(pcm) < 300*192 || len(pcm) > (300+TrimSlackMS)*192 || len(pcm)%4 != 0 {
		t.Fatalf("backlog after a 2 s stall: %d bytes, want %d..%d whole frames", len(pcm), 300*192, (300+TrimSlackMS)*192)
	}
	if !bytes.Equal(pcm, stream[len(stream)-len(pcm):]) {
		t.Fatal("trimmed backlog is not the newest audio on a frame boundary")
	}
	if l.Dropped() != int64(len(stream)-len(pcm)) {
		t.Fatalf("dropped %d bytes, want %d", l.Dropped(), len(stream)-len(pcm))
	}
	next := testFrames(100000, 1152)
	h.publish(next)
	if got, ok := nextWithin(l, time.Second); !ok || !bytes.Equal(got, next) {
		t.Fatal("live audio after the trim did not continue seamlessly")
	}
}

// closeAll ends every live listener.
func TestLiveListenerEndsWhenStopped(t *testing.T) {
	h := &hub{}
	l, cancel := h.subscribeLive(300)
	defer cancel()
	h.closeAll()
	if _, ok := nextWithin(l, time.Second); ok {
		t.Fatal("listener still open after the broadcaster stopped")
	}
}

// r3 review: a listener that connected while Live Stream was being turned off
// (the handler saw it enabled, then closeAll ran) was registered afterwards and
// stayed parked for good, holding its connection. It ends at once now, until the
// broadcaster starts again.
func TestLiveListenerAfterStopIsNotParked(t *testing.T) {
	h := &hub{}
	h.closeAll()
	l, cancel := h.subscribeLive(300)
	defer cancel()
	start := time.Now()
	if _, ok := nextWithin(l, 2*time.Second); ok || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("a listener subscribed after the stop is parked (%v) or got audio", time.Since(start))
	}
	ch, cancelCh := h.subscribe()
	defer cancelCh()
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("a channel subscriber after the stop received audio")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a channel subscriber after the stop stayed parked")
	}
	h.reopen()
	l2, cancel2 := h.subscribeLive(0)
	defer cancel2()
	live := testFrames(5000, 1152)
	h.publish(live)
	if pcm, ok := nextWithin(l2, time.Second); !ok || !bytes.Equal(pcm, live) {
		t.Fatal("no audio for a listener after the restart")
	}
}

// r3 review: Listener.Dropped was a plain int64 written under the listener's
// lock and read without it. Read it while audio is trimmed (run with -race).
func TestListenerDroppedIsSafeToReadDuringPublish(t *testing.T) {
	h := &hub{}
	l, cancel := h.subscribeLive(0)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		publishFrames(h, 0, 2000)
	}()
	var last int64
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			last = l.Dropped()
		}
	}
	if l.Dropped() == 0 || last < 0 {
		t.Fatal("nothing counted as dropped")
	}
}
