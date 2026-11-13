// Package clubmode is the Minimal Club Mode helper: a reversible pause of a
// fixed optional-service allowlist. No killall, no masking.
package clubmode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

// Services never include engine, rb-bridge, network, audio, Pro DJ Link or the
// WebUI host. stream-sync also hosts control/bridge functionality, so pausing it
// is unsafe. The deemixd unit of 5.3.0.5 and later on both generations, then the 1st-gen /
// 2nd-gen names of earlier releases (#234): a mod runs on any release.
var Services = []string{"enginex-deemixd.service", "sc5000-deemixd.service", "enginex-sc5000-deemixd.service"}

// Systemctl runs systemctl; tests replace it.
var Systemctl = func(args ...string) (stdout, stderr string, code int) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	if err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	return out.String(), errb.String(), code
}

type feature struct {
	path string
}

// New returns the Minimal Club Mode feature.
func New(env modkit.Env) (modkit.Feature, error) {
	return &feature{path: filepath.Join(env.State, "paused.json")}, nil
}

type snapshot struct {
	Active   bool     `json:"active"`
	Services []string `json:"services"`
	Phase    string   `json:"phase,omitempty"`
}

func (f *feature) Status() (any, error) {
	out := map[string]any{"active": false, "services": []string{}}
	if _, err := os.Stat(f.path); err == nil {
		var saved map[string]any
		if err := modkit.LoadJSON(f.path, &saved); err != nil {
			return nil, err
		}
		out = saved
	}
	out["allowlist"] = Services
	out["note"] = "Optional units only. The combined stream-sync process remains active because it also hosts core controls. No CPU-percentage guarantee."
	return out, nil
}

func allowed(unit string) bool {
	for _, s := range Services {
		if s == unit {
			return true
		}
	}
	return false
}

func (f *feature) Action(req map[string]any) (any, error) {
	action, err := modkit.Field(req, "action")
	if err != nil {
		return nil, err
	}
	switch action {
	case "enable":
		if _, err := os.Stat(f.path); err == nil {
			return f.Status()
		}
		active := []string{}
		for _, unit := range Services {
			out, errout, code := Systemctl("is-active", unit)
			status := strings.TrimSpace(out)
			if code == 0 && status == "active" {
				active = append(active, unit)
			} else if status != "inactive" && status != "failed" && status != "unknown" {
				if status == "" {
					status = strings.TrimSpace(errout)
				}
				return nil, fmt.Errorf("cannot establish service state for %s: %s", unit, status)
			}
		}
		// Persist the exact pre-state before the first service is stopped.
		snap := snapshot{Active: true, Services: active, Phase: "pausing"}
		if err := modkit.SaveJSON(f.path, snap); err != nil {
			return nil, err
		}
		stopped := []string{}
		for _, unit := range active {
			if _, _, code := Systemctl("stop", unit); code != 0 || isActive(unit) {
				failed := []string{}
				// The failed stop may still have stopped the unit; restore it too.
				for _, restore := range append(append([]string{}, stopped...), unit) {
					if _, _, c := Systemctl("start", restore); c != 0 {
						failed = append(failed, restore)
					}
				}
				if len(failed) > 0 {
					if err := modkit.SaveJSON(f.path, snapshot{Active: true, Services: failed, Phase: "restore-needed"}); err != nil {
						return nil, err
					}
				} else {
					os.Remove(f.path)
				}
				return nil, fmt.Errorf("could not stop %s; rollback attempted; restore pending: %s", unit, pyList(failed))
			}
			stopped = append(stopped, unit)
		}
		snap.Phase = "paused"
		if err := modkit.SaveJSON(f.path, snap); err != nil {
			return nil, err
		}
	case "disable":
		if _, err := os.Stat(f.path); err != nil {
			return f.Status()
		}
		var saved map[string]any
		if err := modkit.LoadJSON(f.path, &saved); err != nil {
			return nil, err
		}
		list, ok := saved["services"].([]any)
		if !ok {
			return nil, modkit.Bad("invalid saved service list")
		}
		pending := []string{}
		for _, v := range list {
			unit, ok := v.(string)
			if !ok || !allowed(unit) {
				return nil, modkit.Bad("invalid saved service list")
			}
			pending = append(pending, unit)
		}
		failed := []string{}
		for _, unit := range pending {
			if _, _, code := Systemctl("start", unit); code != 0 || !isActive(unit) {
				failed = append(failed, unit)
			}
		}
		if len(failed) > 0 {
			saved["phase"], saved["services"] = "restore-needed", failed
			if err := modkit.SaveJSON(f.path, saved); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("restore still pending: %s", pyList(failed))
		}
		if err := os.Remove(f.path); err != nil {
			return nil, err
		}
	default:
		return nil, modkit.Bad("unknown action")
	}
	return f.Status()
}

// Close persists nothing: restarting the helper must not resume downloads.
func (f *feature) Close() {}

func isActive(unit string) bool {
	_, _, code := Systemctl("is-active", unit)
	return code == 0
}

func pyList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "'" + s + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
