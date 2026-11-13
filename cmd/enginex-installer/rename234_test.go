package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #234: EngineX-Installer of 5.3.0.5 hands an older release (5.3.0.5 -> r5) to the installed
// release's leave step, which installs it through a stage (review R1/R2: never the older
// install.sh in place), and then runs the step again to reboot, instead of the older bundle's
// boot-activate.sh: that one never runs in place over 5.3.0.5's units. A newer or the same
// release is still installed and activated in place.
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
		// The stand-in step installs what --install gives it, the way internal/leave ends up.
		os.WriteFile(filepath.Join(deck.Data, "run-enginex-leave.sh"), []byte("#!/bin/sh\necho \"$*\" >> '"+marker+"'\n"+
			"[ \"$1\" != --install ] || sh \"$2/install.sh\"\n"), 0o755)
		announceFrom(t, "127.0.0.1", freeUDPFrom(o), airD("prime4plus-lab", "DISCOVERER_HOWDY_", "JC11S", "2.3.4", 1))

		if err := run(ctxT(t), o); err != nil {
			t.Fatalf("%s: run: %v\n%s", c.version, err, o.Out)
		}
		if v, _ := os.ReadFile(filepath.Join(custom, "version.txt")); strings.TrimSpace(string(v)) != c.version {
			t.Fatalf("%s: version.txt = %q", c.version, v)
		}
		cmds := strings.Join(deck.Commands(), "\n")
		activated := strings.Contains(cmds, "/bundle/boot-activate.sh")
		var steps []string
		for i := 0; i < 100 && c.older && len(steps) < 2; i++ {
			raw, _ := os.ReadFile(marker)
			steps = strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			time.Sleep(50 * time.Millisecond)
		}
		installs := len(steps) == 2 && strings.HasPrefix(steps[0], "--install ") &&
			strings.Contains(steps[0], "/.enginex-leave.incoming/") && strings.HasSuffix(steps[0], " --no-reboot") && steps[1] == ""
		if c.older && (activated || !installs) {
			t.Errorf("older %s: boot-activate.sh ran %v, leave steps %q\n%s", c.version, activated, steps, cmds)
		}
		if !c.older && (!activated || strings.Contains(cmds, "leave")) {
			t.Errorf("newer %s: boot-activate.sh ran %v\n%s", c.version, activated, cmds)
		}
		if c.older && !strings.Contains(o.Out.(*bytes.Buffer).String(), "rebooting") {
			t.Errorf("older %s: the user is not told the deck reboots:\n%s", c.version, o.Out)
		}
	}
}
