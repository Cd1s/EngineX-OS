package enginedb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRGBVerifiedCacheRemovesEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.db")
	if err := os.WriteFile(db, []byte("library"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "OverviewData", "uuid")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "1.rgb")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	saveRGBVerifiedCache(dir, db, "uuid", map[int64]rgbVerifiedEntry{1: {Size: st.Size(), ModTime: st.ModTime().UnixNano(), Path: path}})
	removed, handled, err := pruneRGBVerifiedFast(t.Context(), dir, db)
	if err != nil || !handled || removed != 1 {
		t.Fatalf("removed=%d handled=%v err=%v", removed, handled, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty RGB still exists: %v", err)
	}
}
