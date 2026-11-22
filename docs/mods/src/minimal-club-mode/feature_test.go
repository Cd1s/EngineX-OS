package clubmode

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

func TestPauseRestoreAndPartialRestore(t *testing.T) {
	live := map[string]bool{Services[0]: true}
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
	if len(st.(map[string]any)["services"].([]any)) != 1 {
		t.Fatalf("snapshot %v", st)
	}
	failStart[Services[0]] = true
	if _, err := app.Action(map[string]any{"action": "disable"}); err == nil || modkit.IsBad(err) {
		t.Fatalf("partial restore must be a 500: %v", err)
	}
	st, _ = app.Status()
	if !reflect.DeepEqual(st.(map[string]any)["services"], []any{Services[0]}) {
		t.Fatalf("pending %v", st.(map[string]any)["services"])
	}
	delete(failStart, Services[0])
	if _, err := app.Action(map[string]any{"action": "disable"}); err != nil {
		t.Fatal(err)
	}
	if !live[Services[0]] {
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

// Unknown legacy units in paused.json are ignored cleanly while enginex-deemixd.service is restored.
func TestDisableRestoresEngineXDeemixdOnly(t *testing.T) {
	old := Systemctl
	defer func() { Systemctl = old }()
	live := map[string]bool{}
	var started []string
	Systemctl = func(args ...string) (string, string, int) {
		unit := args[1]
		if !allowed(unit) {
			t.Fatalf("touched %v", args)
		}
		switch args[0] {
		case "show":
			return "loaded\n", "", 0
		case "is-active":
			if live[unit] {
				return "active\n", "", 0
			}
			return "inactive\n", "", 3
		case "start":
			started = append(started, unit)
			live[unit] = true
		}
		return "", "", 0
	}
	state := t.TempDir()
	f, _ := New(modkit.Env{State: state})
	if err := modkit.SaveJSON(state+"/paused.json", map[string]any{"active": true, "services": []string{"enginex-deemixd.service"}, "phase": "paused"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.(*feature).Action(map[string]any{"action": "disable"}); err != nil {
		t.Fatalf("disable failed: %v", err)
	}
	if !reflect.DeepEqual(started, []string{"enginex-deemixd.service"}) {
		t.Fatalf("started %v, want [enginex-deemixd.service]", started)
	}
	if _, err := os.Stat(state + "/paused.json"); !os.IsNotExist(err) {
		t.Fatalf("the pause is still saved")
	}
}

func TestDisableRestoresLegacySavedUnits(t *testing.T) {
	for _, legacyUnit := range []string{"sc5000-deemixd.service", "enginex-sc5000-deemixd.service"} {
		t.Run(legacyUnit, func(t *testing.T) {
			old := Systemctl
			defer func() { Systemctl = old }()
			live := map[string]bool{}
			var started []string
			Systemctl = func(args ...string) (string, string, int) {
				unit := args[1]
				if !allowed(unit) {
					t.Fatalf("touched %v", args)
				}
				switch args[0] {
				case "show":
					return "loaded\n", "", 0
				case "is-active":
					if live[unit] {
						return "active\n", "", 0
					}
					return "inactive\n", "", 3
				case "start":
					started = append(started, unit)
					live[unit] = true
				}
				return "", "", 0
			}
			state := t.TempDir()
			f, _ := New(modkit.Env{State: state})
			if err := modkit.SaveJSON(state+"/paused.json", map[string]any{
				"active":   true,
				"services": []string{legacyUnit},
				"phase":    "paused",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.(*feature).Action(map[string]any{"action": "disable"}); err != nil {
				t.Fatalf("disable failed for %s: %v", legacyUnit, err)
			}
			if !reflect.DeepEqual(started, []string{"enginex-deemixd.service"}) {
				t.Fatalf("started %v, want [enginex-deemixd.service]", started)
			}
			if !live["enginex-deemixd.service"] {
				t.Fatalf("enginex-deemixd.service not restored")
			}
			if _, err := os.Stat(state + "/paused.json"); !os.IsNotExist(err) {
				t.Fatalf("the pause is still saved")
			}
		})
	}
}

func TestRebootRePausesServicesAndAllowsReEnable(t *testing.T) {
	old := Systemctl
	defer func() { Systemctl = old }()
	live := map[string]bool{Services[0]: true}
	Systemctl = func(args ...string) (string, string, int) {
		unit := args[1]
		switch args[0] {
		case "show":
			return "loaded\n", "", 0
		case "is-active":
			if live[unit] {
				return "active\n", "", 0
			}
			return "inactive\n", "", 3
		case "stop":
			delete(live, unit)
		case "start":
			live[unit] = true
		}
		return "", "", 0
	}

	state := t.TempDir()
	f, err := New(modkit.Env{State: state})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.(*feature).Action(map[string]any{"action": "enable"}); err != nil {
		t.Fatal(err)
	}
	if live[Services[0]] {
		t.Fatalf("service still active after enable")
	}

	// Simulate a normal reboot: systemd starts the service again and the mod helper restarts.
	live[Services[0]] = true
	f2, err := New(modkit.Env{State: state})
	if err != nil {
		t.Fatal(err)
	}
	ff2 := f2.(*feature)
	ff2.mu.Lock() // wait for the start-up re-pause
	ff2.mu.Unlock()
	if live[Services[0]] {
		t.Fatalf("service still active after reboot while club mode is active")
	}
	st, err := f2.Status()
	if err != nil {
		t.Fatal(err)
	}
	if active, _ := st.(map[string]any)["active"].(bool); !active {
		t.Fatalf("expected active=true after reboot, got %v", st)
	}

	// If the service was started externally while paused.json exists, enable re-pauses it.
	live[Services[0]] = true
	if _, err := f2.(*feature).Action(map[string]any{"action": "enable"}); err != nil {
		t.Fatal(err)
	}
	if live[Services[0]] {
		t.Fatalf("service still active after re-enable")
	}
}

func TestNewDoesNotWaitForSystemctlStop(t *testing.T) {
	old := Systemctl
	defer func() { Systemctl = old }()
	entered, release := make(chan struct{}), make(chan struct{})
	stopped := false
	Systemctl = func(args ...string) (string, string, int) {
		switch args[0] {
		case "show":
			return "loaded\n", "", 0
		case "is-active":
			if stopped {
				return "inactive\n", "", 3
			}
			return "active\n", "", 0
		case "stop":
			close(entered)
			<-release // a slow `systemctl stop`
			stopped = true
		}
		return "", "", 0
	}
	state := t.TempDir()
	if err := modkit.SaveJSON(filepath.Join(state, "paused.json"), snapshot{Active: true, Services: []string{Services[0]}, Phase: "paused"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan modkit.Feature, 1)
	go func() {
		f, _ := New(modkit.Env{State: state})
		done <- f
	}()
	var f modkit.Feature
	select {
	case f = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("New waited for systemctl stop: the helper's health check would time out")
	}
	<-entered             // the re-pause is running in the background
	st, err := f.Status() // the health check: must answer while the stop is still running
	if err != nil {
		t.Fatal(err)
	}
	if active, _ := st.(map[string]any)["active"].(bool); !active {
		t.Fatalf("status while re-pausing: %v", st)
	}
	close(release)
	ff := f.(*feature)
	ff.mu.Lock() // wait for the re-pause to finish
	ff.mu.Unlock()
	if !stopped {
		t.Fatal("the saved service was not stopped")
	}
}
