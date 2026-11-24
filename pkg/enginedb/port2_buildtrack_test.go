package enginedb

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestPort2Bogus500BPMUsesGridOrZero(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	first := lib.Tracks[0]
	first.BPM = 500
	got := mustBuild(t, root, first)
	if got.Degraded != "" || got.Facts.BPM != 120 {
		t.Fatalf("500 BPM with a valid grid: %+v", got)
	}

	for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
		testfixture.Write(t, root, "PIONEER/USBANLZ/P002/ANLZ0000"+ext, testfixture.EX2())
	}
	second := lib.Tracks[1]
	second.ID = 2
	second.BPM = 500
	second.AnalysisPath = "/PIONEER/USBANLZ/P002/ANLZ0000.DAT"
	got = mustBuild(t, root, second)
	if got.Degraded != "" || got.Facts.BPM != 0 {
		t.Fatalf("500 BPM without beats: %+v", got)
	}
}

func TestPort2WAVContainerBitsKeepValidSampleCount(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	b := testfixture.WAV(true)
	f := b[20:]
	binary.LittleEndian.PutUint16(f[14:], 32)
	binary.LittleEndian.PutUint16(f[12:], 8)
	binary.LittleEndian.PutUint32(f[8:], 384000)
	testfixture.Write(t, root, "MUSIC/three.wav", b)
	got := mustBuild(t, root, rekordbox.Track{ID: 3, Path: "/MUSIC/three.wav", SampleRate: 48000})
	if got.Degraded != "" || got.Facts.SampleCount != 72000 {
		t.Fatalf("WAV facts: %+v", got.Facts)
	}
}
