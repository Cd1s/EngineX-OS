package rekordbox

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func skipTestSection(tag string, h int, payload []byte) []byte {
	b := make([]byte, 12)
	copy(b, tag)
	binary.BigEndian.PutUint32(b[4:], uint32(h))
	binary.BigEndian.PutUint32(b[8:], uint32(12+len(payload)))
	return append(b, payload...)
}

func skipTestFile(sections ...[]byte) []byte {
	b := make([]byte, 28)
	copy(b, "PMAI")
	binary.BigEndian.PutUint32(b[4:], 28)
	for _, s := range sections {
		b = append(b, s...)
	}
	binary.BigEndian.PutUint32(b[8:], uint32(len(b)))
	return b
}

func skipTestWave(width, count int) []byte {
	b := make([]byte, 12+width*count)
	binary.BigEndian.PutUint32(b, uint32(width))
	binary.BigEndian.PutUint32(b[4:], uint32(count))
	for i := 12; i < len(b); i++ {
		b[i] = byte(i * 7)
	}
	return b
}

// ReadAnalysisSkipping leaves out the waveform sections it is told to and reads everything else
// as ReadAnalysis does; it refuses a file exactly when ReadAnalysis does (#253).
func TestReadAnalysisSkippingKeepsTheRestAndRefusesTheSame(t *testing.T) {
	dir := t.TempDir()
	beats := make([]byte, 12+8)
	binary.BigEndian.PutUint32(beats[8:], 1)
	binary.BigEndian.PutUint16(beats[12:], 1)
	binary.BigEndian.PutUint16(beats[14:], 12000)
	good := skipTestFile(skipTestSection("PQTZ", 24, beats), skipTestSection("PWV5", 24, skipTestWave(2, 300)),
		skipTestSection("PWV3", 24, skipTestWave(1, 300)), skipTestSection("PWV4", 24, skipTestWave(6, 20)), skipTestSection("ZZZZ", 12, []byte("kept")))
	badWave := skipTestFile(skipTestSection("PQTZ", 24, beats), skipTestSection("PWV5", 24, skipTestWave(2, 300)[:100]))
	badWave[28+len(skipTestSection("PQTZ", 24, beats))+16+3] = 0xff // count past its section
	badLen := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(badLen[28+8:], 1<<20) // a section past the end
	all := func(string) bool { return true }
	for name, b := range map[string][]byte{"good": good, "bad wave": badWave, "bad length": badLen} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		want, wantErr := ReadAnalysis(p)
		got, err := ReadAnalysisSkipping(p, all)
		if (err == nil) != (wantErr == nil) {
			t.Fatalf("%s: ReadAnalysis %v, skipping %v", name, wantErr, err)
		}
		if err != nil {
			continue
		}
		if len(got.Waveforms) != 0 {
			t.Fatalf("%s: waveforms read: %+v", name, got.Waveforms)
		}
		want.Waveforms = nil
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %+v, want %+v", name, got, want)
		}
		if got, err = ReadAnalysisSkipping(p, func(string) bool { return false }); err != nil {
			t.Fatal(err)
		}
		if full, _ := ReadAnalysis(p); !reflect.DeepEqual(got, full) {
			t.Fatalf("%s: nothing skipped, still different", name)
		}
	}
}
