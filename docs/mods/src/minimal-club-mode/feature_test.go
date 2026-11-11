package clubmode

import (
	"os"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

func TestPauseRestoreAndPartialRestore(t *testing.T) {
	live := map[string]bool{Services[0]: true, Services[1]: true}
	failStart := map[string]bool{}
	var calls [][2]string
	old := Systemctl
	defer func() { Systemctl = old }()
	Systemctl = func(args ...string) (string, string, int) {
		action, unit := args[0], args[1]
		calls = append(calls, [2]string{action, unit})
		switch action {
		case "is-active":
			if live[unit] {
				return "active\n", "", 0
			}
			return "inactive\n", "", 3
		case "stop":
			delete(live, unit)
		case "start":
			if failStart[unit] {
				return "", "", 1
			}
			live[unit] = true
		}
		return "", "", 0
	}
	f, _ := New(modkit.Env{State: t.TempDir()})
	app := f.(*feature)
	if _, err := app.Action(map[string]any{"action": "enable"}); err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("services still live: %v", live)
	}
	st, _ := app.Status()
	if len(st.(map[string]any)["services"].([]any)) != 2 {
		t.Fatalf("snapshot %v", st)
	}
	failStart[Services[1]] = true
	if _, err := app.Action(map[string]any{"action": "disable"}); err == nil || modkit.IsBad(err) {
		t.Fatalf("partial restore must be a 500: %v", err)
	}
	st, _ = app.Status()
	if !reflect.DeepEqual(st.(map[string]any)["services"], []any{Services[1]}) {
		t.Fatalf("pending %v", st.(map[string]any)["services"])
	}
	delete(failStart, Services[1])
	if _, err := app.Action(map[string]any{"action": "disable"}); err != nil {
		t.Fatal(err)
	}
	if !live[Services[0]] || !live[Services[1]] {
		t.Fatalf("not restored: %v", live)
	}
	if _, err := os.Stat(app.path); !os.IsNotExist(err) {
		t.Fatalf("paused.json left")
	}
	for _, c := range calls {
		if !allowed(c[1]) {
			t.Fatalf("touched %s", c[1])
		}
	}
}
