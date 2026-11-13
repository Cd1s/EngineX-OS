package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"

	"github.com/ulikunitz/xz"
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
	data := []byte(content)
	if strings.HasPrefix(content, "AZ01 ") {
		data = gen1Container(t, strings.SplitN(name, "-", 2)[0], data)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
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
	want, _ := os.ReadFile(o.Bundle)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("deck got %q (%v), want the selected image", got, err)
	}
}

func TestGen1FastbootInstallsTheSelectedImageOffline(t *testing.T) {
	o := testOpts(t)
	o.defaults()
	fb := newFastboot(t, "jp07", true)
	o.USB = fb.Bin
	useRelease(o, offlineRelease(t))
	o.Bundle = localGen1Image(t, "SC5000-5.3.0.1-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img", "AZ01 build A")
	if err := run(ctxT(t), o); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(fb.Dir, "flashed-rootfs.img")); string(got) != "AZ01 build A" {
		t.Fatalf("flashed %q, want the selected rootfs", got)
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

func TestGen1RefusesWrongContainer(t *testing.T) {
	gen2 := buildBundle(t, "SCLIVE4", "5.3.0.5")
	gen2Bytes, err := os.ReadFile(gen2.ImagePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"ssh", "usb"} {
		for _, tc := range []struct {
			name string
			data []byte
		}{
			{"model", gen1Container(t, "SC6000", []byte("rootfs"))},
			{"format", gen2Bytes},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				o := testOpts(t)
				o.defaults()
				useRelease(o, oneFileRelease(t, "SC5000-5.3.0.5-EngineX-OS-Test-Update.img", tc.data, signFault{}, nil, ""))
				if route == "ssh" {
					d := startSSHDeck(t, o.SSHPort, "inmusic,jp07\x00")
					err = modeA(ctxT(t), o, deck{IP: "127.0.0.1"})
					if strings.Contains(strings.Join(d.Commands(), "\n"), "az01-update --file") {
						t.Fatal("sent an incompatible image")
					}
				} else {
					fb := newFastboot(t, "jp07", true)
					o.USB = fb.Bin
					err = run(ctxT(t), o)
					for _, call := range fb.Calls() {
						if strings.HasPrefix(call, "oem ") || strings.HasPrefix(call, "flash ") || call == "reboot" {
							t.Fatalf("mutated a deck for an incompatible container: %s", call)
						}
					}
				}
				if err == nil || !strings.Contains(err.Error(), "AZ01 image does not support this deck") {
					t.Fatalf("%s: %v", tc.name, err)
				}
			})
		}
	}
}

// gen1Container is an AZ01 image with a real XZ rootfs and partition digests.
func gen1Container(t *testing.T, prefix string, rootfs []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	xw, err := xz.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xw.Write(rootfs); err != nil {
		t.Fatal(err)
	}
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
	str := func(out *bytes.Buffer, text string) {
		binary.Write(out, binary.LittleEndian, uint32(len(text)))
		out.WriteString(text)
		out.WriteByte(0)
		out.Write(make([]byte, (4-(len(text)+1)%4)%4))
	}
	board := ""
	for code, m := range gen1Boards {
		if m.Prefix == prefix {
			board = code
		}
	}
	if board == "" {
		t.Fatalf("unknown fixture prefix %s", prefix)
	}
	var header bytes.Buffer
	str(&header, "test-build")
	binary.Write(&header, binary.LittleEndian, uint32(1))
	str(&header, "inmusic,"+board)
	binary.Write(&header, binary.LittleEndian, uint32(1))
	binary.Write(&header, binary.LittleEndian, uint32(0x15e4d006))
	str(&header, "native installer fixture")
	var out bytes.Buffer
	out.WriteString("AZ01")
	binary.Write(&out, binary.LittleEndian, uint32(1))
	binary.Write(&out, binary.LittleEndian, uint32(12+header.Len()))
	out.Write(header.Bytes())
	for _, part := range []struct {
		tag, name, compression string
		data                   []byte
	}{
		{"PARTP", "splash", "none", []byte("S")},
		{"PARTX", "recoverysplash", "none", []byte("R")},
		{"PARTL", "rootfs", "xz", compressed.Bytes()},
	} {
		out.WriteString(part.tag)
		out.Write([]byte{0, 0, 0})
		binary.Write(&out, binary.LittleEndian, uint64(len(part.data)))
		str(&out, part.name)
		str(&out, part.compression)
		binary.Write(&out, binary.LittleEndian, uint32(1))
		str(&out, "sha1")
		binary.Write(&out, binary.LittleEndian, uint32(20))
		sum := sha1.Sum(part.data)
		out.Write(sum[:])
		out.Write(part.data)
	}
	out.Write([]byte{'E', 'O', 'F', 0})
	binary.Write(&out, binary.LittleEndian, uint32(16))
	binary.Write(&out, binary.LittleEndian, uint64(0))
	return out.Bytes()
}
