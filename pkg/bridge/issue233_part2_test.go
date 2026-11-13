package bridge_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const issue233Tracks = 3000

func issue233Mount(t *testing.T, root string, opts enginedb.Options) enginedb.Result {
	t.Helper()
	start := time.Now()
	r, err := bridge.ConvertAtMount(context.Background(), root, opts, false)
	t.Logf("mount: elapsed=%s result=%+v err=%v", time.Since(start), r, err)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func issue233Marks(t *testing.T, root string, pending bool) {
	t.Helper()
	dir := filepath.Join(root, "Engine Library/Database2")
	p, _ := os.ReadFile(filepath.Join(dir, ".rb-bridge-pending"))
	s, _ := os.ReadFile(filepath.Join(dir, ".rb-bridge-stamp"))
	if pending {
		if !strings.HasPrefix(string(p), "v2 ") || bytes.Equal(p, s) {
			t.Fatalf("deferred import marks: pending=%q stamp=%q", p, s)
		}
	} else if len(p) != 0 || !strings.HasPrefix(string(s), "v2 ") {
		t.Fatalf("completed import marks: pending=%q stamp=%q", p, s)
	}
}

func TestIssue233LargeIncrementalExportDefersNewTracks(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 1)
	issue233Mount(t, root, enginedb.Options{Deadline: time.Now().Add(time.Hour)})
	testfixture.LargeExport(t, root, issue233Tracks)
	r := issue233Mount(t, root, enginedb.Options{Deadline: time.Now().Add(-time.Second)})
	if r.Added != issue233Tracks-1 || r.Deferred == 0 {
		t.Fatalf("incremental import: %+v", r)
	}
	issue233Marks(t, root, true)
	db := dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM Track`); n != issue233Tracks {
		t.Fatalf("only %d tracks committed", n)
	}
	r = issue233Mount(t, root, enginedb.Options{Deadline: time.Now().Add(time.Hour)})
	if r.Deferred != 0 {
		t.Fatalf("incremental resume: %+v", r)
	}
	issue233Marks(t, root, false)
}

func TestIssue233ReconcileCanceledLeavesLibraryUntouched(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 2)
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	l := &rekordbox.Library{
		Tracks:    []rekordbox.Track{{ID: 1, Path: "/missing.mp3"}, {ID: 2, Path: "/" + testfixture.LargeTrackPath(2)}},
		Playlists: []rekordbox.Playlist{{Tracks: []uint32{1, 2}}},
	}
	tracks := append([]rekordbox.Track(nil), l.Tracks...)
	playlist := append([]uint32(nil), l.Playlists[0].Tracks...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bridge.ReconcileContext(ctx, r, l); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reconcile: %v", err)
	}
	if !reflect.DeepEqual(l.Tracks, tracks) || !reflect.DeepEqual(l.Playlists[0].Tracks, playlist) {
		t.Fatalf("canceled reconcile changed library: %+v", l)
	}
}
