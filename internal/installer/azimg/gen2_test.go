package azimg_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/azimg"
)

func TestGen2OpenTarGzVerifiesOnFullRead(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		read func(io.Reader, int64) error
	}{
		{"ReadFull", func(r io.Reader, n int64) error { _, err := io.ReadFull(r, make([]byte, n)); return err }},
		{"CopyN", func(r io.Reader, n int64) error { _, err := io.CopyN(io.Discard, r, n); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildTestBundleTarGz(t, bundleBuildOpts{gzipStored: true})
			r := &mutatingReaderAt{b: wrapGen2Image(raw, "SCLIVE4", "5.3.0.4")}
			im, err := azimg.Open(ctx, r, int64(len(r.b)), azimg.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			b, err := im.Bundle(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rc, err := b.OpenTarGz(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.read(rc, b.CompressedSize()); err != nil {
				t.Fatalf("honest payload: %v", err)
			}
			_ = rc.Close()
			pos := bytes.Index(r.b, []byte("suite-bin-PAYLOAD"))
			if pos < 0 {
				t.Fatal("stored payload not found")
			}
			r.b[pos] ^= 0x20
			rc, err = b.OpenTarGz(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			if err := tc.read(rc, b.CompressedSize()); !errors.Is(err, azimg.ErrDigestMismatch) {
				t.Fatalf("tampered payload full read: %v; want ErrDigestMismatch", err)
			}
		})
	}
}

func TestGen2DuplicateManifestListedPaths(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, first, second string }{
		{"case-fold", "Readme.txt", "readme.txt"},
		{"nfc-fold", "caf\u00e9.txt", "cafe\u0301.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both names receive correct SHA256SUMS entries. Removing duplicate
			// detection must not be masked by an unrelated unlisted-file error.
			opts := bundleBuildOpts{extra: map[string][]byte{tc.first: []byte("same payload")}}
			control := buildTestBundleTarGz(t, opts)
			if _, err := azimg.Open(ctx, bytes.NewReader(control), int64(len(control)), azimg.Limits{}); err != nil {
				t.Fatalf("single-entry control: %v", err)
			}
			opts.extra[tc.second] = []byte("same payload")
			raw := buildTestBundleTarGz(t, opts)
			_, err := azimg.Open(ctx, bytes.NewReader(raw), int64(len(raw)), azimg.Limits{})
			if !errors.Is(err, azimg.ErrCorrupt) || !strings.Contains(err.Error(), "duplicate archive path") {
				t.Fatalf("err=%v; want duplicate archive path", err)
			}
		})
	}
}
