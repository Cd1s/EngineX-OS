package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

// TestStaging252HiddenExportReadOnlyNoCopy verifies that a CDJ stick (both normal PIONEER
// and hidden .PIONEER exports) can still be read on demand over NFS (MediaCache for live
// display and FetchAnalysis for track load) without copying audio files or creating
// .prolink-cache or Engine Library on any drive.
func TestStaging252HiddenExportReadOnlyNoCopy(t *testing.T) {
	checkNoAudioOrStage := func(t *testing.T, cache string) {
		t.Helper()
		if err := filepath.WalkDir(cache, func(p string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			if ext == ".flac" || ext == ".wav" || ext == ".mp3" || ext == ".aif" || ext == ".aiff" || ext == ".m4a" {
				t.Errorf("MediaCache copied audio file %s", p)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(cache, ".prolink-cache")); !os.IsNotExist(err) {
			t.Fatalf("unexpected .prolink-cache created: %v", err)
		}
		if _, err := os.Stat(filepath.Join(cache, "Engine Library")); !os.IsNotExist(err) {
			t.Fatalf("unexpected Engine Library created: %v", err)
		}
	}

	t.Run("standard_export", func(t *testing.T) {
		source := t.TempDir()
		testfixture.Export(t, source)
		server := &prolink.NFSServer{Root: source}
		if err := server.Listen("127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		defer server.Close()

		cache := t.TempDir()
		d := prolink.NewDiscovery()
		d.SetMediaCache(&prolink.MediaCache{Dir: cache, Portmap: server.PortmapPort()})

		// Seeing the player (PacketType == 6) triggers zero reads or copies.
		d.Observe(prolink.Device{Address: "127.0.0.1", Number: 2, Name: "CDJ-3000", PacketType: 6})
		if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
			t.Fatalf("seen player wrote into media cache: %v %v", entries, err)
		}

		// Loading a track (PacketType == 0x0a) caches ONLY export.pdb and that track's ANLZ.
		obs := d.Observe(prolink.Device{Address: "127.0.0.1", Number: 2, Name: "CDJ-3000", PacketType: 0x0a, SourcePlayer: 2, SourceSlot: 3, TrackID: 1})
		if obs.Title == "" || len(obs.Waveform) == 0 || len(obs.Beats) == 0 {
			t.Fatalf("CDJ track not read for live display: %+v", obs)
		}
		checkNoAudioOrStage(t, cache)

		// On-demand FetchAnalysis reads the track's ANLZ in memory.
		client := &prolink.NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: server.PortmapPort()}
		if _, a, err := prolink.FetchAnalysis(t.Context(), client, 3, 1); err != nil || a == nil || len(a.Beats) == 0 {
			t.Fatalf("FetchAnalysis failed: %v %+v", err, a)
		}
	})

	t.Run("hidden_dot_pioneer_export", func(t *testing.T) {
		source := t.TempDir()
		testfixture.Export(t, source)
		if err := os.Rename(filepath.Join(source, "PIONEER"), filepath.Join(source, ".PIONEER")); err != nil {
			t.Fatal(err)
		}
		server := &prolink.NFSServer{Root: source}
		if err := server.Listen("127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		defer server.Close()

		cache := t.TempDir()
		d := prolink.NewDiscovery()
		d.SetMediaCache(&prolink.MediaCache{Dir: cache, Portmap: server.PortmapPort()})
		d.Observe(prolink.Device{Address: "127.0.0.1", Number: 2, Name: "CDJ-3000", PacketType: 6})
		if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
			t.Fatalf("seen player wrote into media cache: %v %v", entries, err)
		}
		obs := d.Observe(prolink.Device{Address: "127.0.0.1", Number: 2, Name: "CDJ-3000", PacketType: 0x0a, SourcePlayer: 2, SourceSlot: 3, TrackID: 1})
		if obs.Title == "" {
			t.Fatalf("hidden .PIONEER export.pdb title not read: %+v", obs)
		}
		checkNoAudioOrStage(t, cache)
	})
}
