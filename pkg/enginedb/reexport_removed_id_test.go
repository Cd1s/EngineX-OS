package enginedb_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

func TestReexportRemovedTrackReusedSourceIDKeepsSurvivingEngineID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		firstID   uint32
		secondID  uint32
		removeIdx int
	}{
		// Re-export removes one track and shifts its numeric source ID onto a surviving track.
		{name: "removed_first_row_order", firstID: 5445, secondID: 5491, removeIdx: 0},
		{name: "surviving_first_row_order", firstID: 5491, secondID: 5445, removeIdx: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			mp3 := testfixture.MP3WithLAMETag()
			testfixture.Write(t, root, "Contents/A/removed.mp3", mp3)
			testfixture.Write(t, root, "Contents/B/surviving.mp3", mp3)
			testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.DAT", testfixture.DAT())
			testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT", testfixture.EXT())
			testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.EX2())

			tracks := []rekordbox.Track{
				{ID: tc.firstID, Title: "First", Path: "/Contents/A/removed.mp3", AnalysisPath: "/PIONEER/USBANLZ/P001/ANLZ0000.DAT", SampleRate: 44100, Duration: 12, BPM: 120},
				{ID: tc.secondID, Title: "Second", Path: "/Contents/B/surviving.mp3", AnalysisPath: "/PIONEER/USBANLZ/P001/ANLZ0000.DAT", SampleRate: 44100, Duration: 12, BPM: 124},
			}
			if tc.removeIdx == 1 {
				tracks[0].Path, tracks[1].Path = "/Contents/B/surviving.mp3", "/Contents/A/removed.mp3"
			}
			lib1 := &rekordbox.Library{
				Tracks: tracks,
				Playlists: []rekordbox.Playlist{
					{ID: 1, Name: "Set", Tracks: []uint32{tracks[0].ID, tracks[1].ID}},
				},
			}
			ctx := context.Background()
			if _, err := enginedb.Sync(ctx, root, lib1, enginedb.Options{}); err != nil {
				t.Fatalf("initial sync: %v", err)
			}

			db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			var survivingID int64
			if err := db.QueryRow(`SELECT id FROM Track WHERE path='../Contents/B/surviving.mp3'`).Scan(&survivingID); err != nil {
				t.Fatalf("find surviving track: %v", err)
			}
			reusedSourceID := tracks[tc.removeIdx].ID

			// Re-export removes Contents/A/removed.mp3 and shifts the removed track's
			// numeric source ID onto the surviving track Contents/B/surviving.mp3.
			lib2 := &rekordbox.Library{
				Tracks: []rekordbox.Track{
					{ID: reusedSourceID, Title: "Surviving", Path: "/Contents/B/surviving.mp3", AnalysisPath: "/PIONEER/USBANLZ/P001/ANLZ0000.DAT", SampleRate: 44100, Duration: 12, BPM: 124},
				},
				Playlists: []rekordbox.Playlist{
					{ID: 1, Name: "Set", Tracks: []uint32{reusedSourceID}},
				},
			}
			if _, err := enginedb.Sync(ctx, root, lib2, enginedb.Options{}); err != nil {
				t.Fatalf("re-export sync failed: %v", err)
			}

			var count int
			if err := db.QueryRow(`SELECT count(*) FROM Track`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("track count = %d (%v), want 1", count, err)
			}
			var gotID int64
			if err := db.QueryRow(`SELECT id FROM Track WHERE path='../Contents/B/surviving.mp3'`).Scan(&gotID); err != nil {
				t.Fatalf("read surviving track after re-export: %v", err)
			}
			if gotID != survivingID {
				t.Fatalf("surviving track Engine ID moved from %d to %d", survivingID, gotID)
			}
		})
	}
}

func TestSyncToleratesNullTrackPathRow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mp3 := testfixture.MP3WithLAMETag()
	testfixture.Write(t, root, "Contents/A/one.mp3", mp3)
	testfixture.Write(t, root, "Contents/B/two.mp3", mp3)
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.DAT", testfixture.DAT())
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT", testfixture.EXT())
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.EX2())

	ctx := context.Background()
	lib1 := &rekordbox.Library{
		Tracks: []rekordbox.Track{
			{ID: 1, Title: "One", Path: "/Contents/A/one.mp3", AnalysisPath: "/PIONEER/USBANLZ/P001/ANLZ0000.DAT", SampleRate: 44100, Duration: 12, BPM: 120},
		},
	}
	if _, err := enginedb.Sync(ctx, root, lib1, enginedb.Options{}); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO Track(path, filename) VALUES(NULL, 'cloud-only')`); err != nil {
		t.Fatalf("insert NULL path track: %v", err)
	}
	lib2 := &rekordbox.Library{
		Tracks: []rekordbox.Track{
			lib1.Tracks[0],
			{ID: 2, Title: "Two", Path: "/Contents/B/two.mp3", AnalysisPath: "/PIONEER/USBANLZ/P001/ANLZ0000.DAT", SampleRate: 44100, Duration: 12, BPM: 124},
		},
	}
	if _, err := enginedb.Sync(ctx, root, lib2, enginedb.Options{}); err != nil {
		t.Fatalf("sync with NULL Track.path failed: %v", err)
	}
}
