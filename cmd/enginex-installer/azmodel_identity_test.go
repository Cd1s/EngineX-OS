package main

import (
	"strings"
	"testing"
)

// BH-G2-2: what the hardware reports picks the model; a --bundle/--image filename only fills a missing
// identity and must agree with it.
func TestAZModelHardwareIdentityWinsOverBundleName(t *testing.T) {
	const s1 = "/x/SYSTEMONE-5.3.0.4-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img"
	const live4 = "/x/SCLIVE4-5.3.0.4-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img"
	for _, c := range []struct{ board, bundle, line, want string }{
		{"", live4, "1cc5:2027 inMusic Rane SYSTEM ONE", ""}, // USB name vs filename
		{"RMZ2", live4, "1cc5:2027", ""},                     // LAN board vs filename
		{"RMZ2", "", "1cc5:2027 Denon DJ PRIME 4 G2", ""},    // LAN board vs USB name
		{"jp21", s1, "1cc5:2027", ""},                        // a fastboot deck's board still has to agree
		{"", s1, "1cc5:2027 inMusic Rane SYSTEM ONE", "SYSTEMONE"},
		{"RMZ2", s1, "1cc5:2027 inMusic Rane SYSTEM ONE", "SYSTEMONE"},
		{"", s1, "1cc5:2027", "SYSTEMONE"}, // the filename fills a missing identity
	} {
		m, ok := azModel(&options{Bundle: c.bundle}, c.board, c.line)
		if ok != (c.want != "") || m.Prefix != c.want {
			t.Errorf("azModel(%q, %q, %q) = %v %v, want %q", c.board, c.bundle, c.line, m, ok, c.want)
		}
	}
}

// An SC LIVE 4 package given for a USB device that names itself SYSTEM ONE is refused before
// anything is built or flashed.
func TestUSBUpdateModeRefusesAPackageForAnotherModel(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck")
	b := buildBundle(t, "SCLIVE4", "5.3.0.4")
	o, shim := azOpts(t, b, func(int) string { return "1cc5:2027 inMusic Rane SYSTEM ONE\n" })
	o.Bundle = b.ImagePath
	err := run(ctxT(t), o)
	if err == nil || !strings.Contains(err.Error(), "model mismatch") {
		t.Errorf("err = %v", err)
	}
	if shim.calls != 0 {
		t.Fatalf("flashed an SC LIVE 4 package to a SYSTEM ONE (%d USB writes)", shim.calls)
	}
}
