package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func keepalivePacket(name string, number byte, ip net.IP) []byte {
	b := make([]byte, 54)
	copy(b, "Qspt1WmJOL")
	b[10] = 6
	copy(b[12:32], name)
	b[32] = 1
	b[0x21] = 2
	binary.BigEndian.PutUint16(b[0x22:], uint16(len(b)))
	b[0x24] = number
	b[0x25] = 1
	copy(b[0x26:], []byte{2, 0, 0, 0, 0, 12})
	copy(b[0x2c:], ip.To4())
	b[0x30] = 2
	b[0x34] = 1
	b[0x35] = 0x64
	return b
}

// TestRBBridgeProlinkSeenPlayerCopiesNothing verifies that seeing a Pro DJ Link player
// (PacketType == 6) with prolink_share=1 records the device in the snapshot JSON without
// copying any library/tracks to drives, /data, or the tmpfs media cache, and never touches
// any leftover .prolink-cache on disk.
func TestRBBridgeProlinkSeenPlayerCopiesNothing(t *testing.T) {
	cdjUSB := t.TempDir()
	testfixture.Export(t, cdjUSB)
	nfs := &prolink.NFSServer{Root: cdjUSB}
	if err := nfs.Listen("127.63.0.12:0"); err != nil {
		t.Fatal(err)
	}
	defer nfs.Close()

	drive := t.TempDir()
	testfixture.Export(t, drive)
	leftover := filepath.Join(drive, ".prolink-cache", "EngineX Export Beta 1 USB", "Contents", "keep.flac")
	if err := os.MkdirAll(filepath.Dir(leftover), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leftover, []byte("leftover-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	mediaCache := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "prolink.json")
	settings := filepath.Join(t.TempDir(), "settings.conf")
	if err := os.WriteFile(settings, []byte("prolink_share=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- rbBridgeCtx(ctx, []string{
			"--prolink",
			"--bind=127.63.0.11",
			"--state-file=" + stateFile,
			"--media-cache=" + mediaCache,
			"--anlz-root=" + drive,
			"--ui-settings=" + settings,
		}, &bytes.Buffer{})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("rb-bridge --prolink exited with error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("rb-bridge --prolink did not exit after cancel")
		}
	}()

	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 63, 0, 12), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	pkt := keepalivePacket("CDJ-3000", 4, net.IPv4(127, 63, 0, 12))
	dst := &net.UDPAddr{IP: net.IPv4(127, 63, 0, 11), Port: 50000}
	deadline := time.Now().Add(4 * time.Second)
	seen := false
	for time.Now().Before(deadline) {
		_, _ = sender.WriteToUDP(pkt, dst)
		if b, err := os.ReadFile(stateFile); err == nil {
			var snap prolink.Snapshot
			if json.Unmarshal(b, &snap) == nil {
				for _, d := range snap.Devices {
					if d.Name == "CDJ-3000" && d.Number == 4 {
						seen = true
						break
					}
				}
			}
		}
		if seen {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !seen {
		t.Fatal("CDJ-3000 keepalive was not recorded in prolink state snapshot")
	}

	// Seeing a player (PacketType == 6) must write zero files into mediaCache and zero
	// new folders into the drive's .prolink-cache, while leaving existing leftovers intact.
	if entries, err := os.ReadDir(mediaCache); err != nil || len(entries) != 0 {
		t.Fatalf("seen player wrote files to media-cache: entries=%v err=%v", entries, err)
	}
	if entries, err := os.ReadDir(filepath.Join(drive, ".prolink-cache")); err != nil || len(entries) != 1 || entries[0].Name() != "EngineX Export Beta 1 USB" {
		t.Fatalf("drive .prolink-cache changed after seeing CDJ-3000: entries=%v err=%v", entries, err)
	}
	if data, err := os.ReadFile(leftover); err != nil || string(data) != "leftover-data" {
		t.Fatalf("leftover .prolink-cache file modified or deleted: data=%q err=%v", data, err)
	}
}

// TestNoSilentProlinkStagingCodePaths is a fail-before/pass-after guard ensuring no code path
// in internal/cli or pkg/prolink silently copies a Pro DJ Link player's library to any drive
// or /data or deletes leftover .prolink-cache folders.
func TestNoSilentProlinkStagingCodePaths(t *testing.T) {
	checkDir := func(dir string, forbiddenFuncs, forbiddenTypes, forbiddenLiterals []string) {
		t.Helper()
		fset := token.NewFileSet()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			full := filepath.Join(dir, e.Name())
			f, err := parser.ParseFile(fset, full, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", full, err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncDecl:
					for _, bad := range forbiddenFuncs {
						if x.Name.Name == bad {
							t.Errorf("%s defines forbidden silent-copy function %s", full, bad)
						}
					}
				case *ast.TypeSpec:
					for _, bad := range forbiddenTypes {
						if x.Name.Name == bad {
							t.Errorf("%s defines forbidden silent-copy type %s", full, bad)
						}
					}
				case *ast.SelectorExpr:
					if x.Sel.Name == "Stage" || x.Sel.Name == "StagedDir" || (e.Name() == "rbbridge.go" && x.Sel.Name == "observe") {
						t.Errorf("%s references forbidden silent-copy selector .%s", full, x.Sel.Name)
					}
				case *ast.BasicLit:
					for _, bad := range forbiddenLiterals {
						if strings.Contains(x.Value, bad) {
							t.Errorf("%s contains forbidden literal %s", full, bad)
						}
					}
				}
				return true
			})
		}
	}

	checkDir(".", []string{"localExports", "localExportsFrom", "stageName"}, []string{"stager"}, []string{".prolink-cache"})
	checkDir(filepath.Join("..", "..", "pkg", "prolink"), []string{"Stage", "StagedExport", "OwnStagedName"}, nil, []string{".prolink-cache"})
}
