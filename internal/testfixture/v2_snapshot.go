package testfixture

import (
	"database/sql"
	"path/filepath"
	"testing"

	bridgev2 "github.com/Cd1s/EngineX-OS/pkg/bridge/v2"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

// WriteV2SnapshotFromMountedLibrary creates the v2 snapshot that the old fixture import
// represents. It is test-only glue used while the v2 mount writer is being integrated.
func WriteV2SnapshotFromMountedLibrary(t testing.TB, mount string) {
	t.Helper()
	resolver, err := compat.NewResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	files, err := bridgev2.FindSources(resolver, mount)
	if err != nil {
		t.Fatal(err)
	}
	libs, failed, err := bridgev2.ReadSources(t.Context(), resolver, mount, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 0 {
		t.Fatalf("v2 source read failed: %v", failed)
	}
	mDB := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", mDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT t.id,t.path,p.beatData,p.quickCues,p.loops FROM Track t LEFT JOIN PerformanceData p ON p.trackId=t.id WHERE t.path IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	snap := rbstate.New("test-v2")
	snap.Header.Layout = rbstate.LayoutFolders
	snap.Header.Sources = files
	snap.Header.Stamp = rbstate.Stamp(snap.Header.Layout, files, "")
	for rows.Next() {
		var id int64
		var enginePath string
		var beats, cues, loops []byte
		if err := rows.Scan(&id, &enginePath, &beats, &cues, &loops); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		key := rbstate.FoldPath(enginePath)
		t := &rbstate.Track{Key: key, Rel: key, EngineID: id, Written: enginedb.WrittenDigest(beats, cues, loops)}
		for _, src := range libs {
			for _, rt := range src.Lib.Tracks {
				if rbstate.FoldPath(rt.Path) != key {
					continue
				}
				hash := rt.RecordHash
				if hash == "" {
					hash = rekordbox.RecordHash(rt)
				}
				t.Refs = append(t.Refs, rbstate.SourceRef{Kind: src.Kind, Source: src.Source, ID: rt.ID, RecordHash: hash})
				break
			}
		}
		if len(t.Refs) == 0 {
			continue
		}
		snap.Tracks[key] = t
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	dbDir := filepath.Dir(mDB)
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	if len(snap.Tracks) == 0 {
		t.Fatalf("v2 snapshot has no tracks for %s", mount)
	}
}
