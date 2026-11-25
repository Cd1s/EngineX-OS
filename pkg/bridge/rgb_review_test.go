package bridge

import (
	"context"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestWriteOverviewRepairsInvalidExistingRGB(t *testing.T) {
	dir := t.TempDir()
	uuid := "review-uuid"
	path := filepath.Join(dir, "OverviewData", uuid, "1.rgb")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt payload that is too large"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 30)
	binary.BigEndian.PutUint64(raw[0:], 1)
	binary.BigEndian.PutUint64(raw[8:], 1)
	body, err := enginedb.QCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	writeOverview(dir, uuid, 1, body)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := enginedb.QUncompress(got, 1<<20)
	if err != nil || !enginedb.ValidRGBOverview(decoded) {
		t.Fatalf("RGB was not repaired: valid=%v err=%v", enginedb.ValidRGBOverview(decoded), err)
	}
}

func TestEmptyLibraryWithStaleRGBGetsFreshUUID(t *testing.T) {
	for _, tc := range []struct {
		name          string
		clearSequence bool
		dropSnapshot  bool
	}{
		{"intact sequence without snapshot", false, true},
		{"intact sequence with snapshot", false, false},
		{"cleared sequence with snapshot", true, false},
		{"cleared sequence without snapshot", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mount := stick(t)
			mountNow(t, mount, MountOptions{})
			stageAll(t, mount)
			if res := mountNow(t, mount, MountOptions{Publish: true}); res.Applied == 0 {
				t.Fatal("initial library was not published")
			}
			old := snapshot(t, mount)
			db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`DELETE FROM PlaylistEntity; DELETE FROM PreparelistEntity; DELETE FROM PerformanceData; DELETE FROM Track; DELETE FROM Playlist`); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if tc.clearSequence {
				if _, err = db.Exec(`DELETE FROM sqlite_sequence WHERE name='Track'`); err != nil {
					db.Close()
					t.Fatal(err)
				}
			}
			db.Close()
			if tc.dropSnapshot {
				_ = os.Remove(filepath.Join(dbDir(mount), ".rb-bridge-snapshot"))
			}
			raw := make([]byte, 30)
			binary.BigEndian.PutUint64(raw[0:], 1)
			binary.BigEndian.PutUint64(raw[8:], 1)
			body, err := enginedb.QCompress(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err = enginedb.WriteRGBOverviewAtomic(dbDir(mount), old.LibraryUUID, 1, body); err != nil {
				t.Fatal(err)
			}
			// liveLibrary must also refuse to reuse the stale UUID or mirror the emptied DB.
			uuid, content, err := liveLibrary(context.Background(), filepath.Join(dbDir(mount), "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			if uuid != "" || content {
				t.Fatalf("liveLibrary(%s): uuid=%q content=%v, want \"\", false", tc.name, uuid, content)
			}
			if res := mountNow(t, mount, MountOptions{Full: true}); res.Added == 0 {
				t.Fatal("empty library was not rebuilt")
			}
			newSnap := snapshot(t, mount)
			if newSnap.LibraryUUID == old.LibraryUUID {
				t.Fatalf("stale UUID reused: %s", newSnap.LibraryUUID)
			}
		})
	}
}
