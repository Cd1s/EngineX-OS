package stems

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand"
	"testing"
)

func f32s(v []float32) []byte {
	b := make([]byte, 0, len(v)*4)
	for _, f := range v {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(f))
	}
	return b
}

// engineReplies parses what Engine received after the greeting into one
// float32 stream per return channel, checking each reply's size.
func engineReplies(t *testing.T, got []byte, planes, maxBlock int) (streams [][]float32, replies, last int) {
	t.Helper()
	streams = make([][]float32, planes)
	var n int
	var payload []byte
	s := stream{data: func(b []byte) {
		payload = append(payload, b...)
		if len(payload) == n*planes*4 {
			for i := range streams {
				for k := 0; k < n; k++ {
					streams[i] = append(streams[i], math.Float32frombits(binary.LittleEndian.Uint32(payload[(i*n+k)*4:])))
				}
			}
		}
	}}
	if err := s.feed(got, func(f map[string]string) (int64, error) {
		v, err := count(f, "returnFrames", int64(maxBlock))
		n, payload, replies, last = int(v), payload[:0], replies+1, int(v)
		return v * int64(planes) * 4, err
	}); err != nil {
		t.Fatal(err)
	}
	return streams, replies, last
}

// The renderer lags three blocks behind with uneven reply sizes and drains
// after done; Engine must get one immediate reply per block, never more than
// maxBlock frames, and every return channel's frames in order.
func TestPipelineAnswersEngineWithoutWaiting(t *testing.T) {
	var out bytes.Buffer
	p := newPipeline(&out)
	p.unpacked()
	feed := func(s string) {
		if _, err := p.fromEngine([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	render := func(s string) {
		if _, err := p.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	const block, planes = 4, 4 // mono: four stems
	want := make([][]float32, planes)
	next := float32(0)
	reply := func(n int) string { // a planar reply of numbered samples
		var v []float32
		for i := 0; i < planes; i++ {
			for k := 0; k < n; k++ {
				next++
				v = append(v, next)
				want[i] = append(want[i], next)
			}
		}
		return fmt.Sprintf("returnFrames=%d\n\n", n) + string(f32s(v))
	}
	render("session=FFFFFFFFFFFFFFFF\n\n")
	feed("numChannels=1\nsampleRate=44100\nmaxBlockSize=4\n\n")
	render("preparing=ready\n\npreparing=ready\nnumReturnChannels=4\n\n")
	greeting := out.Len()
	sizes := []int{3, 1, 4, 2, 4, 4, 3, 3} // 24 frames over 6 blocks plus drain
	r := 0
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
			render(reply(sizes[r])) // reply to block i-3
			r++
		}
	}
	feed("action=done\n\n")
	for ; r < len(sizes); r++ {
		render(reply(sizes[r]))
	}
	render("returnFrames=0\n\n")
	if err := p.Complete(); err != nil {
		t.Fatal(err)
	}
	got, replies, last := engineReplies(t, out.Bytes()[greeting:], planes, block)
	for i := range want {
		if fmt.Sprint(got[i]) != fmt.Sprint(want[i]) {
			t.Fatalf("plane %d: Engine got %v, want %v", i, got[i], want[i])
		}
	}
	if last != 0 || replies < 7 {
		t.Fatalf("%d replies, last %d", replies, last)
	}
}

// Packed replies: three stems cross as int16 and the deck rebuilds melody
// from its input, so Engine still gets four float32 stems that sum to it.
func TestPipelinePackedRebuildsMelody(t *testing.T) {
	var out bytes.Buffer
	p := newPipeline(&out)
	render := newPackWriter(p) // the renderer's side of the wire
	feed := func(b []byte) {
		if _, err := p.fromEngine(b); err != nil {
			t.Fatal(err)
		}
	}
	const ch, block = 2, 1024
	share := [4]float32{0.3, 0.2, 0.1, 0.4}
	rng := rand.New(rand.NewSource(1))
	var input [][]float32 // per block, planar
	render.Write([]byte("session=FFFFFFFFFFFFFFFF\n\n"))
	feed([]byte(fmt.Sprintf("numChannels=%d\nsampleRate=44100\nmaxBlockSize=%d\n\n", ch, block)))
	render.Write([]byte("preparing=ready\nnumReturnChannels=8\n\n"))
	greeting := out.Len()
	// The renderer's reply to a block: each stem a share of that block's input.
	answer := func(x []float32, n int) {
		var v []float32
		for s := 0; s < 4; s++ {
			for c := 0; c < ch; c++ {
				for k := 0; k < n; k++ {
					v = append(v, x[c*n+k]*share[s])
				}
			}
		}
		if _, err := render.Write([]byte(fmt.Sprintf("returnFrames=%d\n\n", n) + string(f32s(v)))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		x := make([]float32, ch*block)
		for k := range x {
			x[k] = float32(rng.NormFloat64() * 0.5)
		}
		input = append(input, x)
		feed(append([]byte(fmt.Sprintf("action=block\nnumSamples=%d\n\n", block)), f32s(x)...))
		if i >= 2 {
			answer(input[i-2], block)
		}
	}
	feed([]byte("action=done\n\n"))
	answer(input[3], block)
	answer(input[4], block)
	render.Write([]byte("returnFrames=0\n\n"))
	if err := p.Complete(); err != nil {
		t.Fatal(err)
	}
	got, _, last := engineReplies(t, out.Bytes()[greeting:], 4*ch, block)
	if last != 0 {
		t.Fatal("no closing zero")
	}
	for c := 0; c < ch; c++ {
		for i, x := range input {
			for k := 0; k < block; k++ {
				at := i*block + k
				sum := got[c][at] + got[ch+c][at] + got[2*ch+c][at] + got[3*ch+c][at]
				if d := math.Abs(float64(sum - x[c*block+k])); d > 1e-6 {
					t.Fatalf("stems sum off by %g", d)
				}
				if d := math.Abs(float64(got[c][at] - x[c*block+k]*share[0])); d > 1.0/packScale {
					t.Fatalf("vocals off by %g", d)
				}
			}
		}
	}
}

// A zero reply to an early block that arrives after done is not the end.
func TestPipelineWaitsForTheFinalZero(t *testing.T) {
	var out bytes.Buffer
	p := newPipeline(&out)
	p.unpacked()
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

// Packed uploads: Engine's float32 blocks go up as int16 and the renderer's
// unpackWriter restores them, within one quantization step.
func TestPackedUploadRoundTrip(t *testing.T) {
	p := newPipeline(io.Discard)
	var worker bytes.Buffer
	u := newUnpackWriter(&worker)
	send := func(b []byte) int {
		up, err := p.fromEngine(b)
		if err == nil {
			_, err = u.Write(up)
		}
		if err != nil {
			t.Fatal(err)
		}
		return len(up)
	}
	init := "numChannels=2\nsampleRate=44100\nmaxBlockSize=1024\n\n"
	send([]byte(init))
	x := make([]float32, 2*1000)
	for k := range x {
		x[k] = float32(math.Sin(float64(k)) * 0.9)
	}
	head := "action=block\nnumSamples=1000\n\n"
	if n := send(append([]byte(head), f32s(x)...)); n != len(head)+len(x)*2 {
		t.Fatalf("uploaded %d bytes, want %d", n, len(head)+len(x)*2)
	}
	got := worker.Bytes()[len(init)+len(head):]
	if len(got) != len(x)*4 {
		t.Fatalf("worker got %d bytes", len(got))
	}
	for k, want := range x {
		if d := math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(got[k*4:])) - want)); d > 0.5/packScale+1e-9 {
			t.Fatalf("sample %d off by %g", k, d)
		}
	}
}

func TestPipelineRejectsMissingNumChannelsWithoutPanic(t *testing.T) {
	p := newPipeline(io.Discard)
	_, err := p.fromEngine([]byte("maxBlockSize=1\n\naction=block\nnumSamples=0\n\n"))
	if err == nil {
		// Even if fromEngine had accepted it, Write must never panic on empty p.input.
		_, err = p.Write([]byte("numReturnChannels=8\n\nreturnFrames=1\n\n"))
	}
	if err == nil {
		t.Fatal("expected error on missing numChannels, got nil")
	}
}

func TestPipelineRejectsMismatchedNumReturnChannels(t *testing.T) {
	p := newPipeline(io.Discard)
	if _, err := p.fromEngine([]byte("numChannels=2\nsampleRate=44100\nmaxBlockSize=4\n\n")); err != nil {
		t.Fatal(err)
	}
	block := append([]byte("action=block\nnumSamples=1\n\n"), make([]byte, 2*4)...)
	if _, err := p.fromEngine(block); err != nil {
		t.Fatal(err)
	}
	// 12 return channels for 2 input channels previously passed n%4==0 and panicked queue().
	_, err := p.Write(append([]byte("numReturnChannels=12\n\nreturnFrames=1\n\n"), make([]byte, 18)...))
	if err == nil {
		t.Fatal("expected error on numReturnChannels=12 for 2-channel input")
	}
}

func TestPipelineCoalescedBlocksAndBlockPlusDone(t *testing.T) {
	t.Run("coalesced_blocks_each_get_reply", func(t *testing.T) {
		var out bytes.Buffer
		p := newPipeline(&out)
		p.unpacked()
		p.Write([]byte("session=x\n\n"))
		p.fromEngine([]byte("numChannels=1\nsampleRate=44100\nmaxBlockSize=4\n\n"))
		p.Write([]byte("numReturnChannels=4\n\n"))
		greeting := out.Len()

		blk := append([]byte("action=block\nnumSamples=2\n\n"), make([]byte, 2*4)...)
		twoBlocks := append(append([]byte{}, blk...), blk...)
		if _, err := p.fromEngine(twoBlocks); err != nil {
			t.Fatal(err)
		}
		_, replies, _ := engineReplies(t, out.Bytes()[greeting:], 4, 4)
		if replies != 2 {
			t.Fatalf("coalesced 2 blocks got %d replies, want 2", replies)
		}
	})

	t.Run("block_plus_done_no_premature_zero_during_drain", func(t *testing.T) {
		var out bytes.Buffer
		p := newPipeline(&out)
		p.unpacked()
		p.Write([]byte("session=x\n\n"))
		p.fromEngine([]byte("numChannels=1\nsampleRate=44100\nmaxBlockSize=4\n\n"))
		p.Write([]byte("numReturnChannels=4\n\n"))
		greeting := out.Len()

		blk := append([]byte("action=block\nnumSamples=4\n\n"), make([]byte, 4*4)...)
		// Send blocks 1 and 2; renderer answers both with 3 frames each (6 frames queued > maxBlock=4).
		p.fromEngine(blk)
		p.fromEngine(blk)
		p.Write(append([]byte("returnFrames=3\n\n"), bytes.Repeat([]byte{1}, 3*4*4)...))
		p.Write(append([]byte("returnFrames=3\n\n"), bytes.Repeat([]byte{1}, 3*4*4)...))

		// Final block 3 and action=done arrive in the same read while 6 frames are queued.
		if _, err := p.fromEngine(append(append([]byte{}, blk...), []byte("action=done\n\n")...)); err != nil {
			t.Fatal(err)
		}
		// Renderer answers block 3 + drain (6 frames) and closes with returnFrames=0.
		p.Write(append([]byte("returnFrames=6\n\n"), bytes.Repeat([]byte{1}, 6*4*4)...))
		p.Write([]byte("returnFrames=0\n\n"))
		if err := p.Complete(); err != nil {
			t.Fatal(err)
		}

		// Parse individual reply frame counts in order:
		// 3 block replies (0, 0, 4), then non-zero drain replies (2, 4, 2), then ONE closing 0.
		var counts []int
		s := stream{}
		if err := s.feed(out.Bytes()[greeting:], func(f map[string]string) (int64, error) {
			v, err := count(f, "returnFrames", 4)
			counts = append(counts, int(v))
			return v * 4 * 4, err
		}); err != nil {
			t.Fatal(err)
		}
		// After the 3 block replies, the drain loop stops at the FIRST 0; all 12 frames must precede it.
		if len(counts) < 4 {
			t.Fatalf("too few replies: %v", counts)
		}
		seenFrames := counts[0] + counts[1] + counts[2]
		drainZeros := 0
		for _, c := range counts[3:] {
			if c == 0 {
				drainZeros++
				break
			}
			seenFrames += c
		}
		if seenFrames != 12 || counts[len(counts)-1] != 0 {
			t.Fatalf("Engine drain truncated at %d/12 frames (reply counts=%v)", seenFrames, counts)
		}
		totalDrainZeros := 0
		for _, c := range counts[3:] {
			if c == 0 {
				totalDrainZeros++
			}
		}
		if totalDrainZeros != 1 || drainZeros != 1 {
			t.Fatalf("expected exactly 1 zero reply after block replies, got counts=%v", counts)
		}
	})
}

func TestPipelineCompleteReportsRecordedPipelineError(t *testing.T) {
	p := newPipeline(io.Discard)
	if _, err := p.fromEngine([]byte("numChannels=2\nsampleRate=44100\nmaxBlockSize=4\n\naction=block\nnumSamples=0\n\naction=done\n\n")); err != nil {
		t.Fatal(err)
	}
	p.unpacked()
	p.Write([]byte("numReturnChannels=8\n\nreturnFrames=0\n\nreturnFrames=0\n\n"))
	err := p.Complete()
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("cannot unpack")) {
		t.Fatalf("Complete masked p.err: got %v", err)
	}
}
