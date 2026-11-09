package prolink_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

type boundedAnalysisReader struct{ oversizedANLZ bool }

func (boundedAnalysisReader) Lookup(context.Context, string, string) ([]byte, prolink.NFSFile, error) {
	return nil, prolink.NFSFile{Size: uint32(len(testfixture.PDB())), MTime: 1}, nil
}

func (r boundedAnalysisReader) Fetch(_ context.Context, _, path string, _ uint32) ([]byte, prolink.NFSFile, error) {
	if path == "PIONEER/rekordbox/export.pdb" {
		b := testfixture.PDB()
		return b, prolink.NFSFile{Size: uint32(len(b)), MTime: 1}, nil
	}
	// Deliberately violate the reader contract: network size checks also belong
	// at the cache boundary, independent of NFSClient's wire validation.
	if r.oversizedANLZ {
		return []byte("PMAI"), prolink.NFSFile{Size: prolink.DisplayANLZLimit + 1}, nil
	}
	return nil, prolink.NFSFile{}, fmt.Errorf("absent")
}

func TestDisplayCacheByteBudgetAndHostileANLZ(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".fetch-power-loss", "orphan.pdb.stamp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("abandoned"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"old-a.pdb", "old-b.pdb"} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = f.Truncate(prolink.DisplayPDBLimit); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	var c prolink.DisplayAnalysisCache
	if _, _, err := c.Read(t.Context(), boundedAnalysisReader{oversizedANLZ: true}, "192.0.2.1", 3, 1, dir); err == nil {
		t.Fatal("invalid ANLZ length accepted")
	}
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		st, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
	}
	if total > prolink.DisplayCacheLimit {
		t.Fatalf("cache uses %d bytes", total)
	}
	if len(entries) != 3 {
		t.Fatalf("expected old PDB plus new PDB/stamp after eviction, got %d", len(entries))
	}
}

func TestCDJTrackTypeSurvivesNonStatusPackets(t *testing.T) {
	status, err := prolink.ParsePacket(simStatus(), 50002, net.IPv4(127, 63, 0, 2))
	if err != nil || status.TrackType != 1 {
		t.Fatalf("rekordbox status: %+v %v", status, err)
	}
	status.Title, status.Waveform, status.Beats = "fixture", [][3]byte{{1, 2, 3}}, []prolink.BeatGridPoint{{Beat: 1}}
	d := prolink.NewDiscovery()
	d.Observe(status)
	for _, kind := range []byte{6, 0x28, 0x0b} {
		got := d.Observe(prolink.Device{Number: status.Number, Address: status.Address, PacketType: kind})
		if got.TrackType != 1 {
			t.Fatalf("packet %x erased rekordbox source type", kind)
		}
	}
	status.TrackType = 2
	if got := d.Observe(status); got.TrackType != 2 {
		t.Fatal("non-rekordbox source type was not updated")
	}
}
