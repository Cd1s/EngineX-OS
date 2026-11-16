package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Explicit older bundles are refused before their installer or activation can run.
func TestRename234ModeARefusesOlderBundle(t *testing.T) {
	need(t, "python3", "unshare")
	for _, version := range []string{"5.3.0.4", "5.3.0.6"} {
		t.Run(version, func(t *testing.T) {
			b := buildBundle(t, "PRIME4PLUS", version, "")
			o := testOpts(t)
			o.Bundle = b.Path
			deck := startSSHDeck(t, o.SSHPort, "inmusic,jc11s\x00nxp,imx8mm\x00")
			custom := filepath.Join(deck.Data, "enginex")
			os.MkdirAll(filepath.Join(custom, "bundle"), 0o700)
			os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.5\n"), 0o644)
			announceFrom(t, "127.0.0.1", freeUDPFrom(o), airD("prime4plus-lab", "DISCOVERER_HOWDY_", "JC11S", "2.3.4", 1))
			err := run(ctxT(t), o)
			wantVersion := version
			if version == "5.3.0.4" {
				wantVersion = "5.3.0.5"
				if err == nil || !strings.Contains(err.Error(), "downgrade") {
					t.Fatalf("downgrade: %v", err)
				}
			} else if err != nil {
				t.Fatalf("upgrade: %v\n%s", err, o.Out)
			}
			if v, _ := os.ReadFile(filepath.Join(custom, "version.txt")); strings.TrimSpace(string(v)) != wantVersion {
				t.Fatalf("version.txt = %q, want %s", v, wantVersion)
			}
			cmds := strings.Join(deck.Commands(), "\n")
			if strings.Contains(cmds, "/bundle/boot-activate.sh") != (version != "5.3.0.4") {
				t.Fatalf("unexpected activation:\n%s", cmds)
			}
			if version == "5.3.0.4" && strings.Contains(cmds, "tar -xzf") {
				t.Fatalf("downgrade installer was streamed:\n%s", cmds)
			}
		})
	}
}
