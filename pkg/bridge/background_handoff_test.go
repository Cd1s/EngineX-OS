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

func TestStartUpdateRequestsBackgroundBeforeEveryStageCut(t *testing.T) {
	type killedAt string
	for _, target := range []string{"update stage marked", "update stage pending"} {
		t.Run(target, func(t *testing.T) {
			stage := filepath.Join(t.TempDir(), "Engine Library", "Database2", importStageName)
			started := false
			restoreStart := SetBackgroundStarter(func(string) error {
				started = true
				return nil
			})
			defer restoreStart()
			restoreCut := SetCutForTest(func(name string) {
				if name == target {
					panic(killedAt(name))
				}
			})
			defer restoreCut()
			func() {
				defer func() {
					if r := recover(); r == nil {
						t.Fatalf("cut %q did not stop the mount path", target)
					}
				}()
				_ = startUpdate(stage, "stamp")
			}()
			if !started {
				t.Fatalf("background worker was not scheduled before cut %q", target)
			}
			if got := readMark(stage, pendingName); got != "stamp" {
				t.Fatalf("pending stamp %q after cut %q", got, target)
			}
		})
	}
}
