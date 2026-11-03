package stems

import (
	"bytes"
	"fmt"
	"testing"
)

// The renderer lags three blocks behind and drains after done; Engine must get
// one immediate reply per block, never more than maxBlock frames, and every
// frame in order.
func TestPipelineAnswersEngineWithoutWaiting(t *testing.T) {
	var out bytes.Buffer
	p := newPipeline(&out)
	feed := func(s string) {
		if err := p.fromEngine([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	render := func(s string) {
		if _, err := p.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	const block, frame = 4, 1 * 4 * 4 // mono: 4 stems × float32
	var sent []byte
	reply := func(n int) string { // renderer reply with numbered frames
		b := make([]byte, n*frame)
		for i := range b {
			b[i] = byte(len(sent) + i)
		}
		sent = append(sent, b...)
		return fmt.Sprintf("returnFrames=%d\n\n", n) + string(b)
	}
	render("session=FFFFFFFFFFFFFFFF\n\n")
	feed("numChannels=1\nsampleRate=44100\nmaxBlockSize=4\n\n")
	render("preparing=ready\n\npreparing=ready\nnumReturnChannels=4\n\n")
	greeting := out.Len()
	for i := 0; i < 6; i++ {
		before := out.Len()
		feed("action=block\nnumSamples=4\n\n")
		if out.Len() != before {
			t.Fatal("replied before the block's payload arrived")
		}
		feed(string(make([]byte, block*4))) // payload in a separate read
		if out.Len() == before {
			t.Fatalf("block %d got no immediate reply", i)
		}
		if i >= 3 {
			render(reply(block)) // reply to block i-3
		}
	}
	feed("action=done\n\n")
	render(reply(block) + reply(block) + reply(block)) // blocks 3-5
	render(reply(0))                                   // the drain's closing zero
	if err := p.Complete(); err != nil {
		t.Fatal(err)
	}
	// Parse what Engine got: every reply within maxBlock, frames in order.
	var got []byte
	s := stream{data: func(b []byte) { got = append(got, b...) }}
	replies, last := 0, -1
	if err := s.feed(out.Bytes()[greeting:], func(f map[string]string) (int64, error) {
		n, err := count(f, "returnFrames", block)
		replies, last = replies+1, int(n)
		return n * frame, err
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sent) || last != 0 || replies < 7 {
		t.Fatalf("Engine got %d of %d bytes in %d replies, last %d", len(got), len(sent), replies, last)
	}
}

// A zero reply to an early block that arrives after done is not the end.
func TestPipelineWaitsForTheFinalZero(t *testing.T) {
	var out bytes.Buffer
	p := newPipeline(&out)
	p.Write([]byte("session=x\n\n"))
	p.fromEngine([]byte("numChannels=1\nsampleRate=44100\nmaxBlockSize=4\n\n"))
	p.Write([]byte("numReturnChannels=4\n\n"))
	p.fromEngine([]byte("action=block\nnumSamples=0\n\naction=done\n\n"))
	p.Write([]byte("returnFrames=0\n\n")) // the block's reply, not the drain's end
	if p.Complete() == nil {
		t.Fatal("treated a block's zero reply as the end of the drain")
	}
	p.Write([]byte("returnFrames=0\n\n"))
	if err := p.Complete(); err != nil {
		t.Fatal(err)
	}
}
