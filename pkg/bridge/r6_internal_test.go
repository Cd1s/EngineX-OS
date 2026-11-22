package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR6ReadMarkCutPrecedesStampWrite(t *testing.T) {
	dir := t.TempDir()
	oldStamp := "v4 old 1 2"
	newStamp := "v4 new 3 4"
	if err := os.WriteFile(filepath.Join(dir, stampName), []byte(oldStamp+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seen := false
	restore := SetCutForTest(func(name string) {
		if name != "read mark written" {
			return
		}
		seen = true
		mark, _ := os.ReadFile(filepath.Join(dir, readName))
		stamp, _ := os.ReadFile(filepath.Join(dir, stampName))
		if !strings.HasPrefix(string(mark), newStamp+"\n") || string(stamp) != oldStamp+"\n" {
			t.Fatalf("cut ordering wrong: mark=%q stamp=%q", mark, stamp)
		}
	})
	defer restore()
	if err := writeImportState(dir, newStamp, []string{"PIONEER/rekordbox/export.pdb"}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("read mark cut point was not reached")
	}
}
