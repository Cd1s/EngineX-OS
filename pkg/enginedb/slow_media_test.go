package enginedb

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// r3 review: on a slow stick the stats and MP3 header reads of every track used
// up the whole read budget before Sync read a single track, and Sync then
// deferred them all: no mount of a large MP3 library ever made progress. Each
// import here gets a third of what preparing every track would take.
func TestSlowStickMakesProgressOnEveryImport(t *testing.T) {
	const n = 24
	const slow = 40 * time.Millisecond
	root := t.TempDir()
	mp3, err := os.ReadFile("../../internal/audioheader/testdata/01-lame-cbr-info-44100.mp3")
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.DAT", testfixture.DAT())
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT", testfixture.EXT())
	l := &rekordbox.Library{}
	var ids []uint32
	for i := 1; i <= n; i++ {
		p := fmt.Sprintf("MUSIC/slow/t%02d.mp3", i)
		testfixture.Write(t, root, p, mp3)
		l.Tracks = append(l.Tracks, rekordbox.Track{ID: uint32(i), Path: "/" + p, Title: fmt.Sprintf("T%02d", i), AnalysisPath: "/PIONEER/USBANLZ/P001/ANLZ0000.DAT", SampleRate: 44100, Duration: 10})
		ids = append(ids, uint32(i))
	}
	l.Playlists = []rekordbox.Playlist{{ID: 1, Name: "Slow", Tracks: ids}}
	dir := t.TempDir()
	ctx := context.Background()
	if r, err := Sync(ctx, root, l, Options{Fast: true, DatabaseDir: dir}); err != nil || r.Added != n {
		t.Fatalf("export-only import: %+v %v", r, err)
	}
	oldStat, oldHeader := statFile, audioHeader
	t.Cleanup(func() { statFile, audioHeader = oldStat, oldHeader })
	statFile = func(p string) (os.FileInfo, error) { time.Sleep(slow); return oldStat(p) }
	audioHeader = func(p string) (compat.AudioInfo, error) { time.Sleep(slow); return oldHeader(p) }
	for mount := 1; ; mount++ {
		if mount > n {
			t.Fatalf("not finished after %d imports", n)
		}
		r, err := Sync(ctx, root, l, Options{DatabaseDir: dir, Resume: mount > 1, Deadline: time.Now().Add(n * slow)})
		if err != nil {
			t.Fatal(err)
		}
		if r.Updated == 0 {
			t.Fatalf("import %d made no progress: %+v", mount, r)
		}
		if r.Deferred == 0 {
			t.Logf("finished on import %d", mount)
			break
		}
	}
}
