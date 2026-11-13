package main

import (
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/ext4"
)

func TestNativeImageWithoutHostTools(t *testing.T) {
	bundle := buildBundle(t, "PRIME4PLUS", "5.3.0.5")
	b, err := readBundle(ctxT(t), bundle.Path)
	if err != nil {
		t.Fatal(err)
	}
	o := testOpts(t)
	o.defaults()
	o.Key = filepath.Join(t.TempDir(), "no-host-key")
	t.Setenv("PATH", t.TempDir())
	img, fresh, err := buildImage(ctxT(t), o, b, 64<<20, "data.img", "az01-data")
	if err != nil || !fresh {
		t.Fatalf("native fresh image: fresh=%t err=%v", fresh, err)
	}
	fs, err := ext4.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile("enginex/gen2-unlocked", 2)
	fs.Close()
	if err != nil || string(got) != "1\n" {
		t.Fatalf("unlock marker: %q, %v", got, err)
	}
	before := sha(t, img)
	o.Backup = img // Even the usual output basename must remain untouched.
	if _, fresh, err := buildImage(ctxT(t), o, b, 64<<20, "data.img", "az01-data"); err != nil || fresh {
		t.Fatalf("native backup image: fresh=%t err=%v", fresh, err)
	}
	if sha(t, img) != before {
		t.Fatal("original backup changed")
	}
	if files, _ := filepath.Glob(filepath.Join(o.WorkDir, "*.tmp")); len(files) != 0 {
		t.Fatalf("unfinished images: %v", files)
	}
}
