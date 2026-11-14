// Command ext4inject is a test harness only: it lets the existing shell
// sandboxes (tools/az01-packer/test_sclive4_bootstrap.sh and the JC11S
// first-boot sandbox) exercise package ext4 in place of mke2fs and
// host-install-userdata.py. Its bundle-to-ChangeSet mapping mirrors that
// script for testing; the installer's real mapping belongs to the
// integration (cmd/enginex-installer) and is not this file.
//
//	ext4inject mkfs [-L label] [...ignored mke2fs flags] IMAGE
//	ext4inject inject IMAGE --bundle BUNDLE [--ssh-key KEY.pub] [--no-fsck]
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/Cd1s/EngineX-OS/internal/installer/ext4"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: ext4inject mkfs|inject ...")
	}
	var err error
	switch os.Args[1] {
	case "mkfs":
		err = mkfs(os.Args[2:])
	case "inject":
		err = inject(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "ext4inject:", msg)
	os.Exit(1)
}

func mkfs(args []string) error {
	label := ""
	img := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-L":
			i++
			label = args[i]
		case "-O", "-b", "-t", "-I", "-E":
			i++
		default:
			if !strings.HasPrefix(args[i], "-") {
				img = args[i]
			}
		}
	}
	st, err := os.Stat(img)
	if err != nil {
		return err
	}
	tmp := img + ".mkfs"
	fs, err := ext4.Create(context.Background(), tmp, ext4.CreateOptions{Size: st.Size(), Label: label})
	if err != nil {
		return err
	}
	if err := fs.Close(); err != nil {
		return err
	}
	if err := ext4.VerifyFile(context.Background(), tmp, ext4.ChangeSet{}); err != nil {
		return err
	}
	return os.Rename(tmp, img)
}

type member struct {
	data []byte
	mode int64
}

func readTar(r io.Reader, gz bool) (map[string]member, []string, error) {
	if gz {
		z, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		r = z
	}
	tr := tar.NewReader(io.LimitReader(r, 512<<20))
	out := map[string]member{}
	var dirs []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, dirs, nil
		}
		if err != nil {
			return nil, nil, err
		}
		name := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		switch h.Typeflag {
		case tar.TypeDir:
			dirs = append(dirs, name)
		case tar.TypeReg:
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, nil, err
			}
			out[name] = member{b, h.Mode}
		}
	}
}

func entry(p string, mode uint32, b []byte) ext4.Entry {
	return ext4.Entry{Path: p, Mode: mode, Size: int64(len(b)), SHA256: sha256.Sum256(b),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}

func inject(args []string) error {
	var img, bundle, sshKey string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--bundle":
			i++
			bundle = args[i]
		case "--ssh-key":
			i++
			sshKey = args[i]
		case "--no-fsck":
		default:
			img = args[i]
		}
	}
	raw, err := os.ReadFile(bundle)
	if err != nil {
		return err
	}
	if bytes.HasPrefix(raw, []byte("ENGINEX_DATA_BUNDLE")) {
		magic := []byte("ENGINEX_DATA_BUNDLE_IMG_V1\n")
		if len(raw) <= 128 || !bytes.HasPrefix(raw, magic) {
			return errors.New("bad image header")
		}
		want := string(raw[len(magic) : len(magic)+64])
		sum := sha256.Sum256(raw[128:])
		if hex.EncodeToString(sum[:]) != strings.ToLower(want) {
			return errors.New("image payload digest mismatch")
		}
		raw = raw[128:]
	}
	members, _, err := readTar(bytes.NewReader(raw), true)
	if err != nil {
		return err
	}
	top := ""
	for n := range members {
		if i := strings.Index(n, "/"); i > 0 && strings.Contains(n[:i], "EngineX-OS-Data-Bundle") {
			top = n[:i]
		}
	}
	if top == "" {
		return errors.New("no EngineX-OS-Data-Bundle directory")
	}
	get := func(rel string) ([]byte, bool) { m, ok := members[top+"/"+rel]; return m.data, ok }
	sums, ok := get("SHA256SUMS")
	if !ok {
		return errors.New("SHA256SUMS missing")
	}
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		b, ok := get(f[1])
		s := sha256.Sum256(b)
		if !ok || hex.EncodeToString(s[:]) != f[0] {
			return fmt.Errorf("bundle member %s missing or corrupt", f[1])
		}
	}

	src, closer, err := ext4.OpenFileSource(img)
	if err != nil {
		return err
	}
	tmp := img + ".prepared"
	fs, err := ext4.OpenCopy(context.Background(), src, tmp)
	closer.Close()
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	root, links, err := fs.ChooseRuntimeDir()
	if err != nil {
		return err
	}
	version := strings.TrimSpace(string(must(get("version.txt"))))
	cs := ext4.ChangeSet{Links: links, Remove: []string{root + "/gen2-locked"}}
	cs.Directories = []string{root, root + "/bundle", root + "/bundle/bin", root + "/bundle/lib", root + "/ui"}
	alsaRel := "system/var/overlay/lib/alsa/conf.d"
	if b, ok := get("alsa-dir"); ok {
		alsaRel = strings.TrimSpace(string(b))
	}
	alsaDirs := []string{alsaRel}
	for _, d := range []string{"system/var/overlay/lib/alsa/conf.d", "system/var-lib/alsa/conf.d"} {
		if d != alsaRel {
			alsaDirs = append(alsaDirs, d)
		}
	}
	cs.Directories = append(cs.Directories, alsaDirs...)
	names := make([]string, 0, len(members))
	for n := range members {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rel := strings.TrimPrefix(n, top+"/")
		dir, base := path.Split(rel)
		if dir == "bin/" || dir == "lib/" {
			cs.Files = append(cs.Files, entry(root+"/bundle/"+rel, 0o755, members[n].data))
		}
		_ = base
	}
	for _, f := range []struct {
		name string
		mode uint32
	}{{"boot-activate.sh", 0o755}, {"99-sc5000-custom.conf", 0o644}, {"99-enginex.conf", 0o644},
		{"install.sh", 0o755}, {"uninstall.sh", 0o755}, {"version.txt", 0o644}, {"release-tag", 0o644},
		{"model", 0o644}, {"alsa-dir", 0o644}, {"SHA256SUMS", 0o644}} {
		if b, ok := get(f.name); ok {
			cs.Files = append(cs.Files, entry(root+"/bundle/"+f.name, f.mode, b))
		}
	}
	uiTar, ok := get("ui-packs.tar")
	if !ok {
		return errors.New("ui-packs.tar missing")
	}
	ui, uiDirs, err := readTar(bytes.NewReader(uiTar), false)
	if err != nil {
		return err
	}
	for _, d := range uiDirs {
		if d != "" && d != "." {
			cs.Directories = append(cs.Directories, root+"/ui/"+d)
		}
	}
	for n, m := range ui {
		cs.Files = append(cs.Files, entry(root+"/ui/"+n, 0o644, m.data))
	}
	packID := sha256.Sum256(uiTar)
	cs.Files = append(cs.Files,
		entry(root+"/ui/.pack-version", 0o644, []byte(version+"\n")),
		entry(root+"/ui/.pack-id", 0o644, []byte(hex.EncodeToString(packID[:])+"\n")),
		entry(root+"/version.txt", 0o644, []byte(version+"\n")),
		entry(root+"/gen2-unlocked", 0o644, []byte("1\n")))
	if sshKey != "" {
		b, err := os.ReadFile(sshKey)
		if err != nil || len(bytes.TrimSpace(b)) == 0 {
			return fmt.Errorf("SSH public key missing or empty: %s", sshKey)
		}
		cs.Files = append(cs.Files, entry(root+"/authorized_keys", 0o600, b))
	}
	for _, name := range []string{"99-sc5000-custom.conf", "99-enginex.conf"} {
		if b, ok := get(name); ok {
			for _, d := range alsaDirs {
				cs.Files = append(cs.Files, entry(d+"/"+name, 0o644, b))
			}
		}
	}
	if err := fs.Apply(context.Background(), cs); err != nil {
		fs.Close()
		return err
	}
	if err := fs.Close(); err != nil {
		return err
	}
	if err := ext4.VerifyFile(context.Background(), tmp, cs); err != nil {
		return err
	}
	if err := os.Rename(tmp, img); err != nil {
		return err
	}
	fmt.Printf("Successfully injected EngineX OS %s (%d files) into %s (root /%s)\n", version, len(cs.Files), img, root)
	return nil
}

func must(b []byte, ok bool) []byte {
	if !ok {
		fail("bundle lacks version.txt")
	}
	return b
}
