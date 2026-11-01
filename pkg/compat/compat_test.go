package compat_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/sc5000-custom-os/internal/testfixture"
	"github.com/Cd1s/sc5000-custom-os/pkg/compat"
	"golang.org/x/text/unicode/norm"
)

func TestNormalizeUnicodeAndAmbiguity(t *testing.T) {
	root := t.TempDir()
	name := "éガ한Ａ.wav"
	disk := norm.NFD.String(name)
	testfixture.Write(t, root, "MUSIC/"+disk, []byte("audio"))
	r, e := compat.NewResolver(root)
	if e != nil {
		t.Fatal(e)
	}
	p, e := r.Resolve("/music/" + name)
	if e != nil || p != "MUSIC/"+disk {
		t.Fatalf("%q %v", p, e)
	}
	for _, p := range []string{"../../etc/passwd", "Music/../outside", "/Music/\x00.wav", "C:/Music/a.wav"} {
		if _, e = r.Resolve(p); e == nil {
			t.Errorf("accepted unsafe path %q", p)
		}
	}
	if e = os.Symlink("/etc", filepath.Join(root, "escape")); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Resolve("escape/passwd"); e == nil {
		t.Fatal("followed symlink")
	}
	testfixture.Write(t, root, "AA/File", nil)
	testfixture.Write(t, root, "AA/FILE", nil)
	r, _ = compat.NewResolver(root)
	if _, e = r.Resolve("AA/file"); e == nil {
		t.Fatal("ambiguous case fold accepted")
	}
	if got, e := r.Resolve("AA/FILE"); e != nil || got != "AA/FILE" {
		t.Fatal("exact spelling should win", got, e)
	}
}

func TestWAVExtensibleAndFLACLargeCover(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name   string
		b      []byte
		format string
	}{{"pcm.wav", testfixture.WAV(false), "wav"}, {"extensible.wav", testfixture.WAV(true), "wav"}, {"large-cover.flac", testfixture.FLAC(), "flac"}} {
		t.Run(tc.name, func(t *testing.T) {
			testfixture.Write(t, root, tc.name, tc.b)
			a, e := compat.AudioHeader(filepath.Join(root, tc.name))
			if e != nil {
				t.Fatal(e)
			}
			if a.SampleRate != 48000 || a.BitDepth != 24 || a.Channels != 2 || a.Duration != 2 || a.Samples != 96000 || a.Format != tc.format {
				t.Fatalf("%+v", a)
			}
			after, e := os.ReadFile(filepath.Join(root, tc.name))
			if e != nil || string(after) != string(tc.b) {
				t.Fatal("audio modified", e)
			}
		})
	}
	for _, tc := range []struct {
		name string
		b    []byte
	}{{"broken.wav", testfixture.WAV(true)[:28]}, {"broken.flac", testfixture.FLAC()[:60]}} {
		testfixture.Write(t, root, tc.name, tc.b)
		if _, e := compat.AudioHeader(filepath.Join(root, tc.name)); e == nil {
			t.Errorf("accepted truncated %s", tc.name)
		}
	}
}

func TestFLACHeaderRejectsImplausibleRateAndDuration(t *testing.T) {
	for _, tc := range []struct {
		rate    uint64
		samples uint64
	}{{768001, 48000}, {1, 10_000_001}} {
		data := testfixture.FLAC()
		binary.BigEndian.PutUint64(data[18:], tc.rate<<44|1<<41|15<<36|tc.samples)
		p := filepath.Join(t.TempDir(), "bad.flac")
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := compat.AudioHeader(p); err == nil {
			t.Fatal("accepted implausible FLAC", tc)
		}
	}
}
