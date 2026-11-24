package bridge

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
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

func TestCutStageCanBeFinishedAndPublishedAfterMountKill(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 20)
	if _, err := ConvertAtMount(context.Background(), root, enginedb.Options{}, true); err != nil {
		t.Fatal(err)
	}
	testfixture.LargeExport(t, root, 21)
	defer SetLargeExportForTest()()
	type killed struct{}
	restoreCut := SetCutForTest(func(name string) {
		if name == "update stage marked" {
			panic(killed{})
		}
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("mount path was not stopped at the hand-off cut")
			}
		}()
		_, _ = ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	}()
	restoreCut()
	if _, err := ImportInBackground(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	if result, err := ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !result.PlaylistsChanged {
		t.Fatalf("published result: %+v %v", result, err)
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
