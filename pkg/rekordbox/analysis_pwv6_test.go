package rekordbox

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func buildTestPWV6ANLZ(entries [][3]byte) []byte {
	bodyLen := len(entries) * 3
	secLen := 20 + bodyLen
	totalLen := 28 + secLen
	b := make([]byte, totalLen)
	copy(b[0:4], "PMAI")
	binary.BigEndian.PutUint32(b[4:8], 28)
	binary.BigEndian.PutUint32(b[8:12], uint32(totalLen))
	off := 28
	copy(b[off:off+4], "PWV6")
	binary.BigEndian.PutUint32(b[off+4:off+8], 20)
	binary.BigEndian.PutUint32(b[off+8:off+12], uint32(secLen))
	binary.BigEndian.PutUint32(b[off+12:off+16], 3)
	binary.BigEndian.PutUint32(b[off+16:off+20], uint32(len(entries)))
	pos := off + 20
	for _, e := range entries {
		b[pos], b[pos+1], b[pos+2] = e[0], e[1], e[2]
		pos += 3
	}
	return b
}

func TestParseAnalysisPWV6(t *testing.T) {
	entries := make([][3]byte, 1200)
	for i := range entries {
		entries[i] = [3]byte{byte(i % 64), byte((i * 3) % 64), byte((i * 7) % 64)}
	}
	raw := buildTestPWV6ANLZ(entries)
	a, err := ParseAnalysis(raw)
	if err != nil {
		t.Fatalf("ParseAnalysis PWV6: %v", err)
	}
	if len(a.Waveforms) != 1 || a.Waveforms[0].Tag != "PWV6" || a.Waveforms[0].EntryBytes != 3 || len(a.Waveforms[0].Data) != 3600 {
		t.Fatalf("got waveforms=%+v", a.Waveforms)
	}
	if _, unknown := a.Unknown["PWV6"]; unknown {
		t.Fatal("PWV6 must be parsed into Waveforms, not Unknown")
	}

	p := filepath.Join(t.TempDir(), "ANLZ0000.2EX")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	skipped, err := ReadAnalysisSkipping(p, func(tag string) bool { return tag == "PWV6" })
	if err != nil {
		t.Fatalf("ReadAnalysisSkipping PWV6: %v", err)
	}
	if len(skipped.Waveforms) != 0 {
		t.Fatalf("expected PWV6 skipped, got %+v", skipped.Waveforms)
	}
}
