package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestAACGridMigratesAnUnchangedV2Export(t *testing.T) {
	root := t.TempDir()
	rel := testfixture.AACExport(t, root)
	audio := filepath.Join(root, rel)
	media := testfixture.AACWithPriming()
	// The old bridge did not use the AAC header. Keep the file's size and
	// mtime unchanged so the upgrade must be detected by the stamp.
	testfixture.Write(t, root, rel, make([]byte, len(media)))
	st, err := os.Stat(audio)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "Engine Library", "Database2")
	stamp := filepath.Join(dir, ".rb-bridge-stamp")
	v, err := os.ReadFile(stamp)
	if err != nil || len(v) < 2 {
		t.Fatalf("initial stamp: %q, %v", v, err)
	}
	copy(v, "v2")
	if err = os.WriteFile(stamp, v, 0o644); err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, rel, media)
	if err = os.Chtimes(audio, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Bypassed {
		t.Fatalf("v2 upgrade skipped: %+v, %v", res, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := readTrackPositions(t, db, 1); got.beat0 != -2048 {
		t.Fatalf("first beat %v samples, want -2048", got.beat0)
	}
	v, err = os.ReadFile(stamp)
	if err != nil || !bytes.HasPrefix(v, []byte("v3 ")) {
		t.Fatalf("upgraded stamp: %q, %v", v, err)
	}
}
