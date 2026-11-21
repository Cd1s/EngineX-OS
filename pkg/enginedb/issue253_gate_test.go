package enginedb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
)

// #253 review 3: the gate that lets a file replace a stored waveform row is the reader that will
// serve the detail. A malformed section of any kind the parser reads, in front of a valid PWV7,
// makes BridgeDetail find nothing; the gate must say the same. A healthy file still passes, and
// the gate reads no waveform body.
func TestIssue253GateIsTheDetailReader(t *testing.T) {
	const base = "PIONEER/USBANLZ/P001/ANLZ0000"
	files := []string{base + ".2EX", base + ".EXT"}
	type stick struct{ ex2, ext []byte }
	cases := map[string]struct {
		s     stick
		serve bool
	}{
		"healthy":           {stick{testfixture.EX2(), testfixture.EXT()}, true},
		"healthy .EXT only": {stick{testfixture.EX2()[:40], testfixture.EXT()}, true},
		"truncated":         {stick{testfixture.EX2()[:len(testfixture.EX2())/2], testfixture.EXT()[:len(testfixture.EXT())/2]}, false},
	}
	for _, k := range testfixture.MalformedANLZKinds {
		bad := testfixture.MalformedANLZSection(k)
		cases["malformed "+k] = struct {
			s     stick
			serve bool
		}{stick{testfixture.WithANLZSectionFirst(testfixture.EX2(), bad), testfixture.WithANLZSectionFirst(testfixture.EXT(), bad)}, false}
		cases["malformed "+k+" in the .2EX only"] = struct {
			s     stick
			serve bool
		}{stick{testfixture.WithANLZSectionFirst(testfixture.EX2(), bad), testfixture.EXT()}, true} // the .EXT's PWV5 serves
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for f, b := range map[string][]byte{files[0]: c.s.ex2, files[1]: c.s.ext} {
				p := filepath.Join(root, filepath.FromSlash(f))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r, err := compat.NewResolver(root)
			if err != nil {
				t.Fatal(err)
			}
			ok, read := DetailServes(r, files)
			served := BridgeDetail(packBridgeRef(1, files), root, 1<<20) != nil
			if ok != served {
				t.Fatalf("the gate says %v, the reader that serves the detail says %v", ok, served)
			}
			if ok != c.serve {
				t.Fatalf("serves=%v, want %v", ok, c.serve)
			}
			if name == "healthy" && read > 128 {
				t.Fatalf("the gate read %d bytes of a healthy .2EX: it read a waveform's body", read)
			}
		})
	}
}
