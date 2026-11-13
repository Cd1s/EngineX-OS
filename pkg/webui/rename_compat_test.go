package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A release after 5.3.0.4 keeps version.txt and release-tag in /etc/enginex. This
// release is the one that updates to it and the one users roll back to: it reads
// the new directory first and /etc/sc5000 when the new one is not there.
func TestVersionAndReleaseTagAreReadFromEitherImageDir(t *testing.T) {
	defer func(d, i string) { newImageDir, imageReleaseTagFile = d, i }(newImageDir, imageReleaseTagFile)
	dir := t.TempDir()
	custom := filepath.Join(dir, "enginex")
	os.MkdirAll(filepath.Join(custom, "bundle"), 0o700)
	os.WriteFile(filepath.Join(custom, "version.txt"), []byte("5.3.0.4\n"), 0o644)
	newImageDir = filepath.Join(dir, "etc-enginex")
	imageReleaseTagFile = filepath.Join(dir, "etc-sc5000", releaseTagName)
	os.MkdirAll(filepath.Dir(imageReleaseTagFile), 0o755)
	s := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: dir, VersionFile: filepath.Join(custom, "version.txt")}, nil)

	os.WriteFile(imageReleaseTagFile, []byte("v5.3.0.4-r3\n"), 0o644)
	if got := s.installedRelease(); got != "v5.3.0.4-r3" {
		t.Fatalf("only /etc/sc5000 has a tag: release %q, want v5.3.0.4-r3", got)
	}
	os.MkdirAll(newImageDir, 0o755)
	os.WriteFile(filepath.Join(newImageDir, releaseTagName), []byte("v5.3.0.4-r4\n"), 0o644)
	if got := s.installedRelease(); got != "v5.3.0.4-r4" {
		t.Fatalf("/etc/enginex has the newer tag: release %q, want v5.3.0.4-r4", got)
	}
	os.Remove(imageReleaseTagFile)
	if got := s.installedRelease(); got != "v5.3.0.4-r4" {
		t.Fatalf("only /etc/enginex has a tag: release %q, want v5.3.0.4-r4", got)
	}

	// The image's version: with no bundle version, /etc/enginex/version.txt is the answer.
	img := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: dir}, nil)
	os.WriteFile(filepath.Join(newImageDir, "version.txt"), []byte("9.9.9.9\n"), 0o644)
	if got := img.installedVersion(); got != "9.9.9.9" {
		t.Fatalf("installedVersion %q, want the version in /etc/enginex", got)
	}
}

// The online update of a second-generation deck runs the install.sh of the bundle it
// downloaded. A later release's install.sh reads ENGINEX_CUSTOM and ENGINEX_ALSA_OVERLAY
// and carries enginex-named files; it must install from this release's updater.
func TestUpdateInstallsABundleOfALaterReleaseWithEnginexNames(t *testing.T) {
	install := `#!/bin/sh
set -eu
cd "$(dirname "$0")"
: "${ENGINEX_CUSTOM:?this updater did not say where to install}"
: "${ENGINEX_ALSA_OVERLAY:?this updater did not say where the ALSA file goes}"
sha256sum -c SHA256SUMS >/dev/null
mkdir -p "$ENGINEX_CUSTOM/bundle/bin" "$ENGINEX_ALSA_OVERLAY"
cp bin/enginex-suite "$ENGINEX_CUSTOM/bundle/bin/"
cp boot-activate.sh version.txt "$ENGINEX_CUSTOM/bundle/"
cp 99-enginex.conf "$ENGINEX_ALSA_OVERLAY/"
cp version.txt "$ENGINEX_CUSTOM/version.txt"
`
	files := map[string][]byte{
		"bin/enginex-suite": []byte("\x7fELF"), "lib/libengine-ui-hook.so": []byte("\x7fELF"),
		"version.txt": []byte("5.3.0.5\n"), "boot-activate.sh": []byte("#!/bin/sh\n"),
		"99-enginex.conf": []byte("x\n"), "install.sh": []byte(install), "uninstall.sh": []byte("#!/bin/sh\n"),
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var sums strings.Builder
	for _, n := range names {
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(files[n]), n)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	top := "SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle"
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, d := range []string{"/", "/bin/", "/lib/"} {
		tw.WriteHeader(&tar.Header{Name: top + d, Typeflag: tar.TypeDir, Mode: 0o755})
	}
	for n, c := range files {
		tw.WriteHeader(&tar.Header{Name: top + "/" + n, Mode: 0o755, Size: int64(len(c))})
		tw.Write(c)
	}
	tw.Close()
	gz.Close()

	name := top + ".tar.gz"
	s, custom := bundleDeck(t, "inmusic,jp21", func(base string) string {
		return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, name, base+"/dl/"+name)
	}, b.Bytes())
	getUpdate(s, "")
	getUpdate(s, "?apply=1")
	if v := waitUpdate(t, s); v.Error != "" || v.Progress != 1 {
		t.Fatalf("a bundle with enginex names did not install: %+v", v)
	}
	for _, path := range []string{"bundle/bin/enginex-suite", "alsa-conf.d/99-enginex.conf"} {
		if _, err := os.Stat(filepath.Join(custom, path)); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.5\n" {
		t.Errorf("version.txt = %q", b)
	}
}
