package enginedb_test

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/Cd1s/sc5000-custom-os/pkg/enginedb"
	"github.com/Cd1s/sc5000-custom-os/pkg/rekordbox"
)

func TestQCompressAndAbsentCues(t *testing.T) {
	raw := []byte("Engine data")
	b, e := enginedb.QCompress(raw)
	if e != nil {
		t.Fatal(e)
	}
	if binary.BigEndian.Uint32(b) != uint32(len(raw)) {
		t.Fatal("missing Qt length prefix")
	}
	got, e := enginedb.QUncompress(b, 100)
	if e != nil || !bytes.Equal(raw, got) {
		t.Fatal(got, e)
	}
	for _, bad := range [][]byte{b[:3], append(append([]byte(nil), b...), 0), append([]byte{0, 0, 0, 1}, b[4:]...)} {
		if _, e = enginedb.QUncompress(bad, 100); e == nil {
			t.Fatal("accepted invalid qCompress")
		}
	}
	if _, e = enginedb.QUncompress(b, 1); e == nil {
		t.Fatal("unbounded decompression")
	}
	p, e := enginedb.EncodePerformance(rekordbox.Track{SampleRate: 44100, Duration: 10}, nil)
	if e != nil {
		t.Fatal(e)
	}
	q, e := enginedb.QUncompress(p.QuickCues, 1000)
	if e != nil {
		t.Fatal(e)
	}
	if len(q) != 129 {
		t.Fatalf("empty eight-slot layout: %d", len(q))
	}
	for i := 0; i < 8; i++ {
		if binary.BigEndian.Uint64(q[9+i*13:]) != 0xbff0000000000000 {
			t.Fatal("cue synthesized")
		}
	}
	if p.Complete {
		t.Fatal("empty analysis marked complete")
	}
}

func TestSmartMatchWheel(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{{"8A", "9A", true}, {"8A", "7A", true}, {"8A", "8B", true}, {"12A", "1A", true}, {"8A", "9B", false}, {"8A", "10A", false}, {"?", "8A", false}, {"Am", "C", true}, {"A minor", "G major", false}} {
		if got := enginedb.CompatibleKeys(tc.a, tc.b); got != tc.want {
			t.Errorf("%s/%s = %v", tc.a, tc.b, got)
		}
	}
	xs := []rekordbox.Track{{ID: 1, BPM: 120, Key: "8A"}, {ID: 2, BPM: 124, Key: "9A"}, {ID: 3, BPM: 124.01, Key: "8A"}, {ID: 4, BPM: 116, Key: "8B"}, {ID: 5, BPM: 120, Key: "bad"}}
	if got := enginedb.SmartMatch(xs, 1); !reflect.DeepEqual(got, []uint32{2, 4}) {
		t.Fatal(got)
	}
}

func TestExactContainerSamplesAndLegacyCueColor(t *testing.T) {
	a := &rekordbox.Analysis{Cues: []rekordbox.Cue{{HotCue: 2, TimeMS: 500}}}
	p, e := enginedb.EncodePerformance(rekordbox.Track{SampleRate: 48000, Duration: 1, SampleCount: 48001}, a)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := enginedb.QUncompress(p.TrackData, 100)
	if e != nil {
		t.Fatal(e)
	}
	if got := binary.BigEndian.Uint64(raw[8:]); got != 48001 {
		t.Fatal("container count lost", got)
	}
	q, e := enginedb.QUncompress(p.QuickCues, 1000)
	if e != nil {
		t.Fatal(e)
	}
	const emptySlot = 13
	slotB := q[8+emptySlot:]
	if !bytes.Equal(slotB[9:13], []byte{255, 0, 255, 0}) {
		t.Fatal("legacy cue is transparent", slotB[:13])
	}
	if !bytes.Equal(q[8+9:8+13], []byte{0, 0, 0, 0}) {
		t.Fatal("empty A was painted")
	}
}
