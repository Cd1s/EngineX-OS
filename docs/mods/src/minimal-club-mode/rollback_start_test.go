package clubmode

import (
	"os"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

// A stop that fails rolls the paused services back. A start that reports success while the service
// is still down must count as not restored: the saved state stays, so that Disable can restore it.
func TestRollbackKeepsStateWhenAStartDoesNotTakeEffect(t *testing.T) {
	old := Systemctl
	defer func() { Systemctl = old }()

	live := map[string]bool{Services[0]: true}
	Systemctl = func(args ...string) (string, string, int) {
		unit := args[1]
		switch args[0] {
		case "is-active":
			if live[unit] {
				return "active\n", "", 0
			}
			return "inactive\n", "", 3
		case "stop":
			delete(live, unit) // the service is down, but the stop reports a failure
			return "unit failed during stop\n", "", 1
		case "start":
			return "", "", 0 // reports success; the service stays down
		}
		return "", "", 0
	}

	f, err := New(modkit.Env{State: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	app := f.(*feature)
	if _, err = app.Action(map[string]any{"action": "enable"}); err == nil {
		t.Fatal("enable succeeded although the stop failed")
	}
	if _, err := os.Stat(app.path); os.IsNotExist(err) {
		t.Fatal("the saved state is gone although the service is still down")
	}
	var snap snapshot
	if err := modkit.LoadJSON(app.path, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Phase != "restore-needed" || !reflect.DeepEqual(snap.Services, []string{Services[0]}) {
		t.Fatalf("saved state = %+v, want restore-needed for %s", snap, Services[0])
	}
}
