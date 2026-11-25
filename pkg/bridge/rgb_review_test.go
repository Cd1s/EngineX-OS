package bridge

import (
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

func TestEmptyLibraryWithClearedSequenceGetsFreshUUID(t *testing.T) {
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
	if _, err = db.Exec(`DELETE FROM PlaylistEntity; DELETE FROM PreparelistEntity; DELETE FROM PerformanceData; DELETE FROM Track; DELETE FROM Playlist; DELETE FROM sqlite_sequence WHERE name='Track'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
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
	if res := mountNow(t, mount, MountOptions{Full: true}); res.Added == 0 {
		t.Fatal("empty library was not rebuilt")
	}
	newSnap := snapshot(t, mount)
	if newSnap.LibraryUUID == old.LibraryUUID {
		t.Fatalf("stale UUID reused: %s", newSnap.LibraryUUID)
	}
}
