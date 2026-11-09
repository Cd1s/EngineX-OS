package prolink

import (
	"math"
	"testing"
	"time"
)

func TestExportDeckCarried(t *testing.T) {
	t0 := time.Now()
	d := ExportDeck{Playing: true, Master: true, BPM: 120, Pitch: 10, Beat: 3.5, At: t0}
	c := d.Carried(t0.Add(time.Second)) // 132 BPM: 2.2 beats later
	if c.At != t0.Add(time.Second) || math.Abs(c.Beat-math.Mod(3.5+2.2, 4)) > 1e-9 {
		t.Fatalf("carried: %+v", c)
	}
	if b, frac, ok := c.beatAt(c.At.Add(500 * time.Millisecond)); !ok || b != d.mustBeat(t0.Add(1500*time.Millisecond)) || frac < 0 {
		t.Fatalf("beat after carry differs: %d %v %v", b, frac, ok)
	}
	p := ExportDeck{BPM: 120, Beat: 1.25, At: t0}
	if c := p.Carried(t0.Add(time.Hour)); c.Beat != 1.25 {
		t.Fatalf("a paused deck's beat moved: %+v", c)
	}
	if z := (ExportDeck{}).Carried(t0); !z.At.IsZero() {
		t.Fatal("no deck yet: must stay unset")
	}
}

func (d ExportDeck) mustBeat(now time.Time) int {
	pos := d.Beat + now.Sub(d.At).Seconds()*d.BPM*(1+d.Pitch/100)/60
	return int(math.Floor(pos)) % 4
}
