package bridge_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func qfU16(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for _, r := range s {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

func TestQuickUpdateFoldsSecondaryCaseSpelling(t *testing.T) {
	root := t.TempDir()
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.LargePDB(4))
	for i := 1; i <= 4; i++ {
		testfixture.Write(t, root, testfixture.LargeTrackPath(i), testfixture.MP3WithLAMETag())
	}
	pdb := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(pdb, when, when); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true); err != nil || res.Added != 4 {
		t.Fatalf("initial union: %+v %v", res, err)
	}
	data := testfixture.LargePDB(5)
	oldPath := "/" + testfixture.LargeTrackPath(5)
	newPath := strings.Replace(oldPath, "/Album", "/album", 1)
	old := qfU16(oldPath)
	new := qfU16(newPath)
	if bytes.Count(data, old) != 1 {
		t.Fatalf("new export path count %d", bytes.Count(data, old))
	}
	data = bytes.Replace(data, old, new, 1)
	if err := os.WriteFile(pdb, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdb, when.Add(time.Minute), when.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	restore := bridge.SetUpdateExportForTest()
	defer restore()
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 0 {
		t.Fatalf("quick update added %d rows for a secondary case spelling", res.Added)
	}
}
