package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #234: EngineX-Installer of 5.3.0.5 installs an older release over it (5.3.0.5 -> r5), then
// runs the installed release's leave step, which reboots, instead of the older bundle's
// boot-activate.sh: that one never runs in place over 5.3.0.5's units. A newer or the same
// release is still activated in place.
func TestRename234ModeAOlderBundleLeavesInsteadOfActivating(t *testing.T) {
	need(t, "python3", "unshare")
	for _, c := range []struct {
		version, tag string
		older        bool
	}{
		{"5.3.0.4", "v5.3.0.4-r5", true},
		{"5.3.0.6", "", false},
	} {
		b := buildBundle(t, "PRIME4PLUS", c.version, c.tag)
		downloads := 0
		rel := releaseServer(t, b, "", &downloads)
		o := testOpts(t)
		useRelease(o, rel)
		deck := startSSHDeck(t, o.SSHPort, "inmusic,jc11s\x00nxp,imx8mm\x00")
		custom := filepath.Join(deck.Data, "enginex")
		os.MkdirAll(filepath.Join(custom, "bundle"), 0o700)
		os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.5\n"), 0o644)
		os.WriteFile(filepath.Join(custom, "bundle/release-tag"), []byte("v5.3.0.5\n"), 0o644)
		marker := filepath.Join(deck.Data, "left")
		os.WriteFile(filepath.Join(deck.Data, "run-enginex-leave.sh"), []byte("#!/bin/sh\necho \"$*\" > '"+marker+"'\n"), 0o755)
		announceFrom(t, "127.0.0.1", freeUDPFrom(o), airD("prime4plus-lab", "DISCOVERER_HOWDY_", "JC11S", "2.3.4", 1))

		if err := run(ctxT(t), o); err != nil {
			t.Fatalf("%s: run: %v\n%s", c.version, err, o.Out)
		}
		if v, _ := os.ReadFile(filepath.Join(custom, "version.txt")); strings.TrimSpace(string(v)) != c.version {
			t.Fatalf("%s: version.txt = %q", c.version, v)
		}
		cmds := strings.Join(deck.Commands(), "\n")
		activated := strings.Contains(cmds, "/bundle/boot-activate.sh")
		left := false
		for i := 0; i < 100 && c.older && !left; i++ {
			_, err := os.Stat(marker)
			left = err == nil
			time.Sleep(50 * time.Millisecond)
		}
		if c.older && (activated || !left) {
			t.Errorf("older %s: boot-activate.sh ran %v, leave step ran %v\n%s", c.version, activated, left, cmds)
		}
		if !c.older && (!activated || strings.Contains(cmds, "leave")) {
			t.Errorf("newer %s: boot-activate.sh ran %v\n%s", c.version, activated, cmds)
		}
		if c.older && !strings.Contains(o.Out.(*bytes.Buffer).String(), "rebooting") {
			t.Errorf("older %s: the user is not told the deck reboots:\n%s", c.version, o.Out)
		}
	}
}
