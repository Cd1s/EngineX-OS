package enginedb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestPlaylistHashIgnoresFastAndDatabaseDir(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	lib, err := rekordbox.ReadPDB(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatalf("ReadPDB: %v", err)
	}

	tmpDB := t.TempDir()
	r1, err := Sync(context.Background(), mount, lib, Options{Fast: true, DatabaseDir: tmpDB})
	if err != nil || !r1.PlaylistsChanged {
		t.Fatalf("initial staged Fast sync: %+v %v", r1, err)
	}
	// Copy staged Database2 into mount/Engine Library/Database2 just like bridge.stagedImport does.
	dstDir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"m.db", ".rb-bridge-state.db"} {
		data, err := os.ReadFile(filepath.Join(tmpDB, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dstDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Second sync in-place with Fast=false, DatabaseDir="" and identical playlists must NOT rebuild playlists.
	r2, err := Sync(context.Background(), mount, lib, Options{Fast: false, DatabaseDir: ""})
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if r2.PlaylistsChanged {
		t.Fatalf("PlaylistsChanged=true on unchanged playlists when Fast/DatabaseDir differed: %+v", r2)
	}
}

func TestPutArtworkRejectsOversizedPNGDimensions(t *testing.T) {
	// 4200x4200 (17.64M pixels > 16M limit) compresses to a tiny paletted PNG
	// yet expands to >70 MB during decode/JPEG encode on 32-bit armhf.
	pal := image.NewPaletted(image.Rect(0, 0, 4200, 4200), color.Palette{color.Black})
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, pal); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(buf.Bytes())
	if err := putArtwork(t.TempDir(), sum[:], buf.Bytes()); err == nil {
		t.Fatal("putArtwork accepted oversized 4200x4200 PNG decompression bomb")
	}
}
