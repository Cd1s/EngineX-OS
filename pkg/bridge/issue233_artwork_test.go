package bridge_test

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// Stamp bypass must not materialize all cover BLOBs again. The files already
// exist; loading the 128 MiB payload serves no purpose on this mount.
func TestIssue233StampBypassDoesNotReloadMigratedArtwork(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 1)
	issue233Mount(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	dir := filepath.Join(root, "Engine Library/Artwork")
	data := make([]byte, 128<<10)
	copy(data, []byte{0xff, 0xd8, 0xff})
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 1; i <= 1024; i++ {
		sum := make([]byte, 20)
		binary.BigEndian.PutUint64(sum, uint64(i))
		if _, err := tx.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES(?,?)`, sum, data); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, base64.RawURLEncoding.EncodeToString(sum)+".jpg"), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r := issue233Mount(t, root, enginedb.Options{})
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("1024 migrated covers: allocated=%d payload=%d", allocated, len(data)*1024)
	if !r.Bypassed {
		t.Fatal("unchanged export was not bypassed")
	}
	if allocated > 32<<20 {
		t.Fatalf("stamp bypass allocated %d bytes for already migrated covers", allocated)
	}
}
