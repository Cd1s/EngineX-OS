package joglogo

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestOriginalRestore(t *testing.T) {
	state := filepath.Join(t.TempDir(), "logo-state")
	app, err := newFeature(state)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(state, 0o755)
	os.WriteFile(app.path, []byte("cropped test PNG"), 0o644)
	volume := t.TempDir()
	target := filepath.Join(volume, "logo.png")
	os.WriteFile(target, []byte("original logo"), 0o644)
	app.library = func(any) (string, error) { return volume, nil }
	if _, err := app.Action(map[string]any{"action": "apply", "volumes": []any{"test"}, "sync": false, "enabled": true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "cropped test PNG" {
		t.Fatalf("apply wrote %q", b)
	}
	if _, err := app.Action(map[string]any{"action": "apply", "volumes": []any{"test"}, "sync": false, "enabled": false}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "original logo" {
		t.Fatalf("restore wrote %q", b)
	}
	if _, err := os.Stat(filepath.Join(volume, ".enginex-logo-owned.json")); !os.IsNotExist(err) {
		t.Fatalf("marker left behind")
	}
}

func TestLibraryOnlyBelowMedia(t *testing.T) {
	for _, v := range []any{"/", "/media", "/etc", 5} {
		if _, err := Library(v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
}

func TestCropWithoutFFmpeg(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	state := t.TempDir()
	app, err := newFeature(state)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(state, "wide.png")
	img := image.NewRGBA(image.Rect(0, 0, 1200, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 1200; x++ {
			c := color.RGBA{R: 255, A: 255}
			if x >= 300 && x < 900 {
				c = color.RGBA{G: 255, A: 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err = app.Action(map[string]any{"action": "crop", "source": source}); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(app.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if out.Bounds().Dx() != 600 || out.Bounds().Dy() != 600 || color.RGBAModel.Convert(out.At(0, 300)) != (color.RGBA{G: 255, A: 255}) {
		t.Fatal("wrong centered crop")
	}
	original, _ := os.ReadFile(app.path)
	if err = os.WriteFile(source, []byte("broken PNG"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Action(map[string]any{"action": "crop", "source": source}); err == nil {
		t.Fatal("corrupt image accepted")
	}
	current, _ := os.ReadFile(app.path)
	if string(original) != string(current) {
		t.Fatal("failed crop replaced existing logo")
	}
}
