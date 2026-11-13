package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/internal/installer/azimg"
	"github.com/Cd1s/EngineX-OS/internal/installer/ext4"
)

type bundleInfo struct {
	Path, Top, Version string
	TarGz              []byte
	bundle             *azimg.Bundle
}

func readBundle(ctx context.Context, path string) (*bundleInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, bundleMaxDown+gen2ImgHeader+1))
	if err != nil {
		return nil, err
	}
	im, err := azimg.Open(ctx, bytes.NewReader(raw), int64(len(raw)), azimg.Limits{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	b, err := im.Bundle(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	payload := raw
	if im.Format() == azimg.FormatGen2Image {
		payload = raw[gen2ImgHeader:]
	}
	return &bundleInfo{Path: path, Top: b.Top(), Version: b.Version(), TarGz: payload, bundle: b}, nil
}

// buildImage prepares a private output; the caller removes its directory after use.
// The supplied backup is only opened read-only, even when it has the output name.
func buildImage(ctx context.Context, o *options, b *bundleInfo, size int64, name, label string) (string, bool, error) {
	if size <= 0 {
		return "", false, errors.New("cannot read the data partition size; nothing flashed")
	}
	dir, err := os.MkdirTemp(o.WorkDir, ".enginex-image-")
	if err != nil {
		return "", false, err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	path := filepath.Join(dir, name)
	fresh := o.Backup == ""
	var fs *ext4.Image
	if fresh {
		fs, err = ext4.Create(ctx, path, ext4.CreateOptions{Size: size, Label: label})
	} else {
		var source ext4.Source
		var closer io.Closer
		source, closer, err = ext4.OpenFileSource(o.Backup)
		if err != nil {
			return "", false, err
		}
		defer closer.Close()
		if source.Size() > size {
			return "", false, errors.New("backup exceeds the verified data partition; nothing flashed")
		}
		fs, err = ext4.OpenCopy(ctx, source, path)
	}
	if err != nil {
		return "", false, err
	}
	defer fs.Close()
	changes, err := imageChanges(ctx, o, b, fs)
	if err != nil {
		return "", false, err
	}
	if err := fs.Apply(ctx, changes); err != nil {
		return "", false, err
	}
	if err := fs.Close(); err != nil {
		return "", false, err
	}
	if err := ext4.VerifyFile(ctx, path, changes); err != nil {
		return "", false, fmt.Errorf("image readback: %w; nothing flashed", err)
	}
	keep = true
	return path, fresh, nil
}

func imageChanges(ctx context.Context, o *options, b *bundleInfo, fs *ext4.Image) (ext4.ChangeSet, error) {
	root, links, err := fs.ChooseRuntimeDir()
	if err != nil {
		return ext4.ChangeSet{}, err
	}
	cs := ext4.ChangeSet{
		Directories: []string{root + "/bundle/bin", root + "/bundle/lib", root + "/ui"},
		Links: links, Remove: []string{root + "/gen2-locked"},
	}
	add := func(e azimg.Entry, path string, mode uint32) {
		cs.Files = append(cs.Files, ext4.Entry{Path: path, Mode: mode, Size: e.Size, SHA256: e.SHA256, Open: e.Open})
	}
	err = b.bundle.Walk(ctx, func(e azimg.Entry) error {
		mode := uint32(0o644)
		switch {
		case strings.HasPrefix(e.Path, "bin/"), strings.HasPrefix(e.Path, "lib/"):
			mode = 0o755
		case e.Path == "boot-activate.sh", e.Path == "install.sh", e.Path == "uninstall.sh":
			mode = 0o755
		case e.Path == "version.txt", e.Path == "release-tag", e.Path == "model", e.Path == "alsa-dir", e.Path == "SHA256SUMS":
		case e.Path == "99-sc5000-custom.conf", e.Path == "99-enginex.conf":
			for _, dir := range []string{"system/var/overlay/lib/alsa/conf.d", "system/var-lib/alsa/conf.d"} {
				add(e, dir+"/"+e.Path, mode)
			}
		default:
			return nil
		}
		add(e, root+"/bundle/"+e.Path, mode)
		return nil
	})
	if err != nil {
		return cs, err
	}
	for _, dir := range b.bundle.UIDirs() {
		cs.Directories = append(cs.Directories, root+"/ui/"+dir)
	}
	if err := b.bundle.WalkUI(ctx, func(e azimg.Entry) error {
		add(e, root+"/ui/"+e.Path, 0o644)
		return nil
	}); err != nil {
		return cs, err
	}
	cs.Files = append(cs.Files,
		imageBytes(root+"/version.txt", []byte(b.Version+"\n"), 0o644),
		imageBytes(root+"/gen2-unlocked", []byte("1\n"), 0o644))
	if b.bundle.HasUIPacks() {
		cs.Files = append(cs.Files,
			imageBytes(root+"/ui/.pack-version", []byte(b.Version+"\n"), 0o644),
			imageBytes(root+"/ui/.pack-id", []byte(b.bundle.PackID()+"\n"), 0o644))
	}
	if key := sshPublicKey(o); key != "" {
		data, err := os.ReadFile(key)
		if err != nil {
			return cs, err
		}
		cs.Files = append(cs.Files, imageBytes(root+"/authorized_keys", data, 0o600))
	}
	return cs, nil
}

func imageBytes(path string, data []byte, mode uint32) ext4.Entry {
	return ext4.Entry{Path: path, Mode: mode, Size: int64(len(data)), SHA256: sha256.Sum256(data),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
}
