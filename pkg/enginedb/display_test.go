package enginedb

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func displayFixture(t *testing.T) Performance {
	t.Helper()
	p, err := EncodePerformance(rekordbox.Track{SampleRate:44100, Duration:4}, &rekordbox.Analysis{
		Beats: []rekordbox.Beat{{TimeMS:0, BPM:120, Number:1}, {TimeMS:500, BPM:120, Number:2}, {TimeMS:1000, BPM:120, Number:3}},
		Waveforms: []rekordbox.Waveform{{Tag:"PWV7", EntryBytes:3, Data:[]byte{20,40,60, 80,100,120}}},
		Cues: []rekordbox.Cue{{HotCue:1, TimeMS:500, Name:"drop"}, {TimeMS:1000}, {HotCue:2, TimeMS:1500, EndMS:2500, Loop:true, Name:"loop"}},
	})
	if err != nil { t.Fatal(err) }
	return p
}

func TestParseDisplayPerformance(t *testing.T) {
	p := displayFixture(t)
	d, err := ParseDisplayPerformance(p.TrackData,p.Overview,p.Beats,p.QuickCues,p.Loops)
	if err != nil { t.Fatal(err) }
	if d.SampleRate != 44100 || d.DurationMS != 4000 || len(d.Overview.Bands) != 3072 || len(d.Beats) != 3 || d.Beats[1].MS != 500 || d.Beats[1].Number != 1 {
		t.Fatalf("track/wave/grid: %+v", d)
	}
	if len(d.Cues) != 3 || d.Cues[0].MS != 500 || d.Cues[0].N != "drop" || d.Cues[2].I != 0 || d.Cues[2].MS != 1000 || len(d.Loops) != 1 || d.Loops[0].StartMS != 1500 || d.Loops[0].EndMS != 2500 {
		t.Fatalf("cues/loops: %+v %+v", d.Cues, d.Loops)
	}
}

func TestDisplayAdjustedSparseGrid(t *testing.T) {
	p := displayFixture(t)
	var b bytes.Buffer
	put(&b,big,float64(44100)); put(&b,big,float64(176400)); b.WriteByte(1)
	for _, shift := range []float64{0,11025} {
		put(&b,big,uint64(2))
		for _, m := range []struct{p float64;n int64;span uint32}{{-22050+shift,-1,8},{154350+shift,7,0}} {
			put(&b,little,m.p); put(&b,little,m.n); put(&b,little,m.span); put(&b,little,uint32(0))
		}
	}
	q, _ := QCompress(b.Bytes())
	d, err := ParseDisplayPerformance(p.TrackData,p.Overview,q,nil,nil)
	if err != nil { t.Fatal(err) }
	if len(d.Beats) != 8 || d.Beats[0].MS != 250 || d.Beats[0].Number != 0 || d.Beats[7].MS != 3750 {
		t.Fatalf("adjusted sparse grid was replaced: %+v",d.Beats)
	}
}

func TestDisplayMalformedPerformance(t *testing.T) {
	p := displayFixture(t)
	unpack := func(b []byte) []byte { r,e:=QUncompress(b,8<<20); if e!=nil {t.Fatal(e)}; return r }
	pack := func(b []byte) []byte { q,e:=QCompress(b);if e!=nil {t.Fatal(e)}; return q }
	badTrack := unpack(p.TrackData); binary.BigEndian.PutUint64(badTrack,math.Float64bits(math.NaN()))
	badWave := unpack(p.Overview); binary.BigEndian.PutUint64(badWave[8:],math.MaxUint64)
	badGrid := unpack(p.Beats); binary.BigEndian.PutUint64(badGrid[17:],math.MaxUint64)
	badCue := unpack(p.QuickCues); badCue[8]=255
	badLoops := bytes.Clone(p.Loops); badLoops[0]=255
	for _, tc := range []struct{name string;track,wave,beats,cues,loops []byte}{
		{"track",pack(badTrack),p.Overview,p.Beats,p.QuickCues,p.Loops},
		{"wave",p.TrackData,pack(badWave),p.Beats,p.QuickCues,p.Loops},
		{"grid",p.TrackData,p.Overview,pack(badGrid),p.QuickCues,p.Loops},
		{"cues",p.TrackData,p.Overview,p.Beats,pack(badCue),p.Loops},
		{"loops",p.TrackData,p.Overview,p.Beats,p.QuickCues,badLoops},
		{"trailing",p.TrackData,append(bytes.Clone(p.Overview),1),p.Beats,p.QuickCues,p.Loops},
	} {
		t.Run(tc.name,func(t *testing.T){ if _,e:=ParseDisplayPerformance(tc.track,tc.wave,tc.beats,tc.cues,tc.loops);e==nil {t.Fatal("malformed input accepted")} })
	}
}
