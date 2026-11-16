package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"github.com/Cd1s/EngineX-OS/internal/migration"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/ext4"
)

func rewriteBundleFile(t *testing.T, src, member, replacement string) string {
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
			if replacement == "" {
				continue
			}
			h.Name = strings.TrimSuffix(h.Name, member) + replacement
		}
		if strings.HasSuffix(h.Name, "/SHA256SUMS") {
			var keep []string
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasSuffix(line, "  "+member) {
					if replacement != "" {
						keep = append(keep, strings.TrimSuffix(line, member)+replacement)
					}
				} else {
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
	for _, member := range []string{"bin/enginex-suite", "lib/libengine-ui-hook.so", "boot-activate.sh", "99-enginex.conf"} {
		t.Run(member, func(t *testing.T) {
			fb := newFastboot(t, "jp21", true)
			o := testOpts(t)
			o.USB, o.Bundle = fb.Bin, rewriteBundleFile(t, bundle.Path, member, "")
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

func TestNativeImagePreservesLegacyDirectoryForMigration(t *testing.T) {
	bundle := buildBundle(t, "PRIME4PLUS", "5.3.0.5")
	ctx := ctxT(t)
	backup := filepath.Join(t.TempDir(), "backup.img")
	fs, err := ext4.Create(ctx, backup, ext4.CreateOptions{Size: 64 << 20, Label: "az01-data"})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if err := fs.Apply(ctx, ext4.ChangeSet{
		Directories: []string{"enginex", migration.DataName},
		Files: []ext4.Entry{
			imageBytes(migration.DataName+"/bundle/boot-activate.sh", []byte("stale"), 0o755),
			imageBytes(migration.DataName+"/bundle/lib/libengine-ui-hook.so", []byte("stale"), 0o755),
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
	if err = run(ctx, o); err != nil {
		t.Fatal(err)
	}
	prepared, err := ext4.Open(filepath.Join(fb.Dir, "flashed-data.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if b, err := prepared.ReadFile(migration.DataName+"/bundle/boot-activate.sh", 1<<20); err != nil || string(b) != "stale" {
		t.Fatal("legacy user data changed")
	}
	if b, err := prepared.ReadFile("enginex/bundle/boot-activate.sh", 1<<20); err != nil || len(b) == 0 {
		t.Fatal("new activation missing")
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

// Native image installation must accept a renamed bundle and an explicitly selected older bundle.
func TestRename234NativeImageAcceptsBothSuiteNames(t *testing.T) {
	bundle := buildBundle(t, "SCLIVE4", "5.3.0.5")
	for _, suite := range []string{"bin/enginex-suite", "bin/enginex-suite"} {
		t.Run(suite, func(t *testing.T) {
			path := bundle.Path
			if suite != "bin/enginex-suite" {
				path = rewriteBundleFile(t, path, "bin/enginex-suite", suite)
			}
			b, err := readBundle(ctxT(t), path)
			if err != nil {
				t.Fatal(err)
			}
			fs, err := ext4.Create(ctxT(t), filepath.Join(t.TempDir(), "data.img"), ext4.CreateOptions{Size: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			defer fs.Close()
			changes, err := imageChanges(ctxT(t), testOpts(t), b, fs)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.Apply(ctxT(t), changes); err != nil {
				t.Fatal(err)
			}
			if typ, err := fs.Lstat("enginex/bundle/" + suite); err != nil || typ != ext4.Regular {
				t.Fatalf("suite was not installed: %v %v", typ, err)
			}
		})
	}
}
