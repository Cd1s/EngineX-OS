package bridge

import (
	"path/filepath"
	"testing"
)

func TestStartUpdateRequestsBackgroundBeforeSlowWork(t *testing.T) {
	stage := filepath.Join(t.TempDir(), "Engine Library", "Database2", importStageName)
	var got string
	restore := SetBackgroundStarter(func(mount string) error {
		got = mount
		return nil
	})
	defer restore()
	if err := startUpdate(stage, "stamp"); err != nil {
		t.Fatal(err)
	}
	want := filepath.Dir(filepath.Dir(filepath.Dir(stage)))
	if got != want {
		t.Fatalf("background mount %q, want %q", got, want)
	}
}
