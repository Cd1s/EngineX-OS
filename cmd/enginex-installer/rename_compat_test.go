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

func TestActivationFilesAcceptEitherAlsaName(t *testing.T) {
	bundle := buildBundle(t, "PRIME4PLUS", "5.3.0.5")
	for _, name := range []string{"99-sc5000-custom.conf", "99-enginex.conf"} {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(bundle.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			gz, err := gzip.NewReader(f)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			var archive bytes.Buffer
			out := gzip.NewWriter(&archive)
			tw := tar.NewWriter(out)
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
				h.Name = strings.ReplaceAll(h.Name, "99-sc5000-custom.conf", name)
				if strings.HasSuffix(h.Name, "/SHA256SUMS") {
					data = bytes.ReplaceAll(data, []byte("99-sc5000-custom.conf"), []byte(name))
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
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), filepath.Base(bundle.Path))
			if err := os.WriteFile(path, archive.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			b, err := readBundle(ctxT(t), path)
			if err != nil {
				t.Fatal(err)
			}
			o := testOpts(t)
			image, _, err := buildImage(ctxT(t), o, b, 64<<20, "data.img", "az01-data")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(filepath.Dir(image))
			fs, err := ext4.Open(image)
			if err != nil {
				t.Fatal(err)
			}
			defer fs.Close()
			for _, dir := range []string{"system/var/overlay/lib/alsa/conf.d", "system/var-lib/alsa/conf.d", "enginex/bundle"} {
				if data, err := fs.ReadFile(dir+"/"+name, 1<<20); err != nil || len(data) == 0 {
					t.Fatalf("%s: %v", dir, err)
				}
			}
			if target, err := fs.Readlink("sc5000-custom"); err != nil || target != "enginex" {
				t.Fatalf("compatibility link: %q, %v", target, err)
			}
		})
	}
}
