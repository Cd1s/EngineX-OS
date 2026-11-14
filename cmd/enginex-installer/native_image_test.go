package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/ext4"
)

func dropBundleFile(t *testing.T, src, member string) string {
	t.Helper()
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(h.Name, "/"+member) {
			continue
		}
		if strings.HasSuffix(h.Name, "/SHA256SUMS") {
			var keep []string
			for _, line := range strings.Split(string(data), "\n") {
				if !strings.HasSuffix(line, "  "+member) {
					keep = append(keep, line)
				}
			}
			data = []byte(strings.Join(keep, "\n"))
			h.Size = int64(len(data))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), filepath.Base(src))
	if err := os.WriteFile(dest, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestNativeImageRequiresCompleteBundle(t *testing.T) {
	bundle := buildBundle(t, "SCLIVE4", "5.3.0.5")
	for _, member := range []string{"bin/sc5000-suite", "lib/libengine-ui-hook.so", "boot-activate.sh", "99-sc5000-custom.conf"} {
		t.Run(member, func(t *testing.T) {
			fb := newFastboot(t, "jp21", true)
			o := testOpts(t)
			o.USB, o.Bundle = fb.Bin, dropBundleFile(t, bundle.Path, member)
			err := run(ctxT(t), o)
			if err == nil || !strings.Contains(err.Error(), member) || !strings.HasSuffix(err.Error(), "nothing flashed") {
				t.Fatalf("missing %s: %v", member, err)
			}
			if flashed := fb.Flashed(); len(flashed) != 0 {
				t.Fatalf("flashed incomplete bundle: %v", flashed)
			}
		})
	}
}

func TestNativeImageRefusesConflictingLegacyDirectory(t *testing.T) {
	bundle := buildBundle(t, "PRIME4PLUS", "5.3.0.5")
	ctx := ctxT(t)
	backup := filepath.Join(t.TempDir(), "backup.img")
	fs, err := ext4.Create(ctx, backup, ext4.CreateOptions{Size: 64 << 20, Label: "az01-data"})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if err := fs.Apply(ctx, ext4.ChangeSet{
		Directories: []string{"enginex", "sc5000-custom"},
		Files: []ext4.Entry{
			imageBytes("sc5000-custom/bundle/boot-activate.sh", []byte("stale"), 0o755),
			imageBytes("sc5000-custom/bundle/lib/libengine-ui-hook.so", []byte("stale"), 0o755),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	fb := newFastboot(t, "JC11S", true)
	o := testOpts(t)
	o.USB, o.Bundle, o.Backup = fb.Bin, bundle.Path, backup
	err = run(ctx, o)
	if err == nil || !strings.Contains(err.Error(), "legacy /sc5000-custom") || !strings.HasSuffix(err.Error(), "nothing flashed") {
		t.Fatalf("conflicting directories: %v", err)
	}
	if flashed := fb.Flashed(); len(flashed) != 0 {
		t.Fatalf("flashed with stale legacy hook: %v", flashed)
	}
}

func TestNativeImageRefusesOversizedBackup(t *testing.T) {
	bundle := buildBundle(t, "SCLIVE4", "5.3.0.5")
	backup := filepath.Join(t.TempDir(), "backup.img")
	fs, err := ext4.Create(ctxT(t), backup, ext4.CreateOptions{Size: 128 << 20, Label: "az01-data"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	fb := newFastboot(t, "jp21", true)
	o := testOpts(t)
	o.USB, o.Bundle, o.Backup = fb.Bin, bundle.Path, backup
	err = run(ctxT(t), o)
	if err == nil || !strings.Contains(err.Error(), "backup exceeds the verified data partition") {
		t.Fatalf("oversized backup: %v", err)
	}
	if flashed := fb.Flashed(); len(flashed) != 0 {
		t.Fatalf("flashed oversized backup: %v", flashed)
	}
}

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
}
