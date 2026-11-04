package stems

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestLocalRenderGreetsAndExitsOnEOF(t *testing.T) {
	var out bytes.Buffer
	if err := LocalRender(strings.NewReader(""), &out); err != nil || out.String() != "session=FFFFFFFFFFFFFFFF\n\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
}

// Every block is answered with as many frames; the stems sum to the input; a
// 100 Hz tone lands in bass, not drums.
func TestLocalRenderSplitsAndSumsToInput(t *testing.T) {
	const ch, block, blocks, rate = 2, 1024, 8, 44100
	in := fmt.Sprintf("numChannels=%d\nsampleRate=%d\nmaxBlockSize=%d\n\n", ch, rate, block)
	var x [][]float32
	for b := 0; b < blocks; b++ {
		v := make([]float32, ch*block)
		for c := 0; c < ch; c++ {
			for k := 0; k < block; k++ {
				v[c*block+k] = float32(0.5 * math.Sin(2*math.Pi*100*float64(b*block+k)/rate))
			}
		}
		x = append(x, v)
		in += fmt.Sprintf("action=block\nnumSamples=%d\n\n", block) + string(f32s(v))
	}
	var out bytes.Buffer
	if err := LocalRender(strings.NewReader(in+"action=done\n\n"), &out); err != nil {
		t.Fatal(err)
	}
	head := "session=FFFFFFFFFFFFFFFF\n\npreparing=ready\n\npreparing=ready\nnumReturnChannels=8\n\n"
	if !strings.HasPrefix(out.String(), head) {
		t.Fatalf("greeting %q", out.String()[:min(out.Len(), 90)])
	}
	got, replies, last := engineReplies(t, out.Bytes()[len(head):], 4*ch, block)
	if replies != blocks+1 || last != 0 || len(got[0]) != blocks*block {
		t.Fatalf("%d replies, last %d, %d frames", replies, last, len(got[0]))
	}
	var bass, drums float64
	for c := 0; c < ch; c++ {
		for i := 0; i < blocks*block; i++ {
			want := x[i/block][c*block+i%block]
			if d := math.Abs(float64(got[c][i] + got[ch+c][i] + got[2*ch+c][i] + got[3*ch+c][i] - want)); d > 1e-6 {
				t.Fatalf("stems sum off by %g", d)
			}
			if i >= block { // past the filters' settling
				bass += math.Abs(float64(got[ch+c][i]))
				drums += math.Abs(float64(got[2*ch+c][i]))
			}
		}
	}
	if drums > bass/100 {
		t.Fatalf("100 Hz tone: bass %g, drums %g", bass, drums)
	}
}
