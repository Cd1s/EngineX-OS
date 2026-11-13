package testfixture_test

import (
	"bytes"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/audioheader"
	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestLargePDBPageChains(t *testing.T) {
	for _, n := range []int{1, 16, 17, 3000} {
		b := testfixture.LargePDB(n)
		l, err := rekordbox.ParsePDB(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		if len(l.Tracks) != n || len(l.Playlists) != 1 || len(l.Playlists[0].Tracks) != n || l.Playlists[0].Tracks[n-1] != uint32(n) {
			t.Fatalf("%d-track fixture lost records or playlist order", n)
		}
		if l.Tracks[n-1].Path != "/"+testfixture.LargeTrackPath(n) {
			t.Fatalf("last path: %q", l.Tracks[n-1].Path)
		}
	}
}

func TestMP3WithLAMETag(t *testing.T) {
	b := testfixture.MP3WithLAMETag()
	info, err := audioheader.ReadMP3(bytes.NewReader(b), int64(len(b)))
	if err != nil || info.SampleRate != 44100 || info.OffsetClass != "lame:2257" {
		t.Fatalf("header fixture: %+v %v", info, err)
	}
}
