package rekordbox

import (
	"encoding/binary"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestAnalysisSkipsPadsIPAndDisabledCueSlots(t *testing.T) {
	// Build a PCOB hot-cue list containing:
	// - slot 1 (valid Hot Cue A at 500 ms)
	// - slot 0 disabled placeholder (p[28]=0, TimeMS=0xffffffff)
	// - slot 9 (Rekordbox Desktop Pad I at 1500 ms)
	// - slot 16 (Rekordbox Desktop Pad P loop 2000..3000 ms)
	disabledOld := make([]byte, 56)
	copy(disabledOld, "PCPT")
	binary.BigEndian.PutUint32(disabledOld[4:], 28)
	binary.BigEndian.PutUint32(disabledOld[8:], 56)
	disabledOld[28] = 0
	binary.BigEndian.PutUint32(disabledOld[32:], 0xffffffff)
	binary.BigEndian.PutUint32(disabledOld[36:], 0xffffffff)

	mkOld := func(hot, start, end uint32, typ byte) []byte {
		b := make([]byte, 56)
		copy(b, "PCPT")
		binary.BigEndian.PutUint32(b[4:], 28)
		binary.BigEndian.PutUint32(b[8:], 56)
		binary.BigEndian.PutUint32(b[12:], hot)
		b[28] = typ
		binary.BigEndian.PutUint32(b[32:], start)
		binary.BigEndian.PutUint32(b[36:], end)
		return b
	}

	pcobHot := make([]byte, 12)
	binary.BigEndian.PutUint32(pcobHot, 1) // hot cue list
	binary.BigEndian.PutUint16(pcobHot[6:], 4)
	pcobHot = append(pcobHot, disabledOld...)
	pcobHot = append(pcobHot, mkOld(1, 500, 0xffffffff, 1)...)
	pcobHot = append(pcobHot, mkOld(9, 1500, 0xffffffff, 1)...)
	pcobHot = append(pcobHot, mkOld(16, 2000, 3000, 2)...)

	dat := testfixture.PMAI(testfixture.Section("PCOB", 24, pcobHot))
	a, err := ParseAnalysis(dat)
	if err != nil {
		t.Fatalf("ParseAnalysis(DAT): %v", err)
	}

	// Extended PCO2 with disabled slot, valid slot 8, and Desktop Pad J (slot 10) + Pad P (slot 16).
	disabledExt := testfixture.CueExtended(0, 0xffffffff, 0, "", [3]byte{})
	disabledExt[16] = 0
	pco2Hot := make([]byte, 8)
	binary.BigEndian.PutUint32(pco2Hot, 1) // hot cue list
	binary.BigEndian.PutUint16(pco2Hot[4:], 4)
	pco2Hot = append(pco2Hot, disabledExt...)
	pco2Hot = append(pco2Hot, testfixture.CueExtended(8, 800, 1200, "Slot H", [3]byte{10, 20, 30})...)
	pco2Hot = append(pco2Hot, testfixture.CueExtended(10, 2500, 0, "Pad J", [3]byte{40, 50, 60})...)
	pco2Hot = append(pco2Hot, testfixture.CueExtended(16, 3000, 4000, "Pad P", [3]byte{70, 80, 90})...)

	ext, err := ParseAnalysis(testfixture.PMAI(testfixture.Section("PCO2", 20, pco2Hot)))
	if err != nil {
		t.Fatalf("ParseAnalysis(EXT): %v", err)
	}
	a.Merge(ext)

	if len(a.Cues) != 2 {
		t.Fatalf("got %d cues (%+v), want only valid slots 1 and 8", len(a.Cues), a.Cues)
	}
	if a.Cues[0].HotCue != 1 || a.Cues[0].TimeMS != 500 || a.Cues[1].HotCue != 8 || !a.Cues[1].Loop || a.Cues[1].EndMS != 1200 {
		t.Fatalf("unexpected merged cues: %+v", a.Cues)
	}

	// Library.Validate must accept a track populated with these parsed cues.
	lib := &Library{Tracks: []Track{{ID: 1, Path: "/Contents/t.mp3", Cues: a.Cues}}}
	if err := lib.Validate(); err != nil {
		t.Fatalf("Library.Validate failed after parsing ANLZ cues: %v", err)
	}
}
