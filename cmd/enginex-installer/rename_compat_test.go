package main

import "testing"

// A release after 5.3.0.4 names the ALSA file that starts EngineX at boot
// 99-enginex.conf. This installer still has to accept such a bundle for the
// PRIME 4+ data image: one ALSA file, under either name, at its own path.
func TestActivationFilesAcceptEitherAlsaName(t *testing.T) {
	want := activationFiles("/enginex")
	for _, name := range []string{"99-sc5000-custom.conf", "99-enginex.conf"} {
		paths := want[name]
		if len(paths) != 1 || paths[0] != "/system/var/overlay/lib/alsa/conf.d/"+name {
			t.Errorf("%s is checked at %v", name, paths)
		}
	}
	if len(want["boot-activate.sh"]) != 2 || len(want["lib/libengine-ui-hook.so"]) != 2 {
		t.Errorf("activation files: %v", want)
	}
}
