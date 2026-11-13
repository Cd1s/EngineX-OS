package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BH-G2-7: both first-generation paths downloaded the latest release even when --image named a
// local image, so an offline install failed and an online one flashed another build.

func localGen1Image(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func offlineRelease(t *testing.T) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("downloaded %s although --image was given", r.URL.Path)
		http.Error(w, "offline", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGen1SSHInstallsTheSelectedImage(t *testing.T) {
	o := testOpts(t)
	o.defaults()
	srv, _ := imageRelease(t, "SC5000") // the latest release is another build
	useRelease(o, srv)
	o.Bundle = localGen1Image(t, "SC5000-5.3.0.1-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img", "AZ01 build A")
	d := startSSHDeck(t, o.SSHPort, "inmusic,jp07\x00")
	if err := modeA(ctxT(t), o, deck{IP: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(d.Data, "az01-update.img"))
	if err != nil || string(got) != "AZ01 build A" {
		t.Fatalf("deck got %q (%v), want the selected image", got, err)
	}
}

func TestGen1FastbootInstallsTheSelectedImageOffline(t *testing.T) {
	o := testOpts(t)
	o.defaults()
	fb := newFastboot(t, "jp07", true)
	o.Fastboot = fb.Bin
	useRelease(o, offlineRelease(t))
	o.Bundle = localGen1Image(t, "SC5000-5.3.0.1-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img", "AZ01 build A")
	seen := filepath.Join(t.TempDir(), "unpacked-from")
	o.Packer = filepath.Join(t.TempDir(), "packer.py")
	if err := os.WriteFile(o.Packer, []byte("import pathlib,sys\n"+
		"pathlib.Path("+pyQuote(seen)+").write_text(sys.argv[sys.argv.index('unpack')+1])\n"+
		"p=pathlib.Path(sys.argv[sys.argv.index('--output')+1]); p.mkdir(); (p/'rootfs.ext2').write_bytes(b'rootfs')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := modeB(ctxT(t), o); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(seen); string(got) != o.Bundle {
		t.Fatalf("unpacked %q, want the selected %q", got, o.Bundle)
	}
	if !strings.Contains(strings.Join(fb.Calls(), "\n"), "flash rootfs ") {
		t.Fatalf("rootfs not flashed: %v", fb.Calls())
	}
}

func TestGen1RefusesASelectedImageForAnotherDeck(t *testing.T) {
	for _, c := range []struct{ name, content, want string }{
		{"PRIME4-5.3.0.1-EngineX-OS-Denon-PRIME-4-JC11-Update.img", "AZ01 prime 4", "not an EngineX image for this deck"},
		{"SC5000-5.3.0.1-EngineX-OS-Data-Bundle.tar.gz", "\x1f\x8b", "not an EngineX image for this deck"},
		{"SC5000-5.3.0.1-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img", "ENGINEX_DATA_BUNDLE_IMG_V1\n", "not a first-generation AZ01"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := testOpts(t)
			o.defaults()
			useRelease(o, offlineRelease(t))
			o.Bundle = localGen1Image(t, c.name, c.content)
			d := startSSHDeck(t, o.SSHPort, "inmusic,jp07\x00")
			err := modeA(ctxT(t), o, deck{IP: "127.0.0.1"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if _, err := os.Stat(filepath.Join(d.Data, "az01-update.img")); err == nil {
				t.Fatal("an image reached the deck")
			}
			if strings.Contains(strings.Join(d.Commands(), "\n"), "az01-update") {
				t.Fatal("az01-update ran")
			}
		})
	}
}

func pyQuote(s string) string {
	var b bytes.Buffer
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\'' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}
