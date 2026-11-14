package webui

// Settings copies carry an explicit subset, never a uiStatus response or the
// original settings file. New settings remain private until added here.
import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

var copyUIBools = []string{
	"engineLighting", "autoAnalyze", "masterLimiter", "autoGain",
	"fourDeckWaveform", "pairedView", "prolinkWaveform", "upfaderEcho",
	"browserDualColumn", "tracklistWaveform", "cueCountdown", "swapLoopOrder",
	"stemsEqMode", "beatJumpPads",
}

var copyUIEnums = map[string][]string{
	"language": uiLanguages, "waveform": uiWaveforms, "shadowMode": uiShadowModes,
	"quality":        uiQualities,
	"streamSortRole": uiStreamSortRoles, "streamSortOrder": uiStreamSortOrders,
	"pioneerFx": uiPioneerFxModes, "fxTimeMode": uiFxTimeModes,
}

var copyUINumbers = map[string]func(float64) bool{
	"playhead":      validPlayhead,
	"bpmDecimals":   func(n float64) bool { return n == 1 || n == 2 },
	"syncGuardPct":  func(n float64) bool { return n == float64(int(n)) && slices.Contains(uiSyncGuardPcts, int(n)) },
	"ledBrightness": func(n float64) bool { return n == float64(int(n)) && n >= 20 && n <= 100 },
	"uiScale":       func(n float64) bool { return n == float64(int(n)) && slices.Contains(uiUIScales, int(n)) },
	"fxTimeMs":      func(n float64) bool { return n == float64(int(n)) && n >= 10 && n <= 4000 },
}

func copyUIAllowed(key string) bool {
	return slices.Contains(copyUIBools, key) || copyUIEnums[key] != nil || copyUINumbers[key] != nil
}

func copyUIExport(v uiSettings) map[string]json.RawMessage {
	b, _ := json.Marshal(v)
	var all map[string]json.RawMessage
	_ = json.Unmarshal(b, &all)
	for key := range all {
		if !copyUIAllowed(key) {
			delete(all, key)
		}
	}
	return all
}

func copyUISupported(key, board string) bool {
	c := boardCapabilities(board)
	switch key {
	case "upfaderEcho", "pioneerFx", "fxTimeMode", "fxTimeMs":
		return c.Mixer
	case "stemsEqMode":
		return c.StemsEq
	case "beatJumpPads":
		return c.BeatJumpPads
	case "pairedView":
		return playerBoards[board]
	case "fourDeckWaveform", "shadowMode":
		return c.FourDeck
	}
	return true
}

// Validate the untrusted payload before merging. The enum/range rules are the
// same ones used by uiUpdate; destination-only fields never enter the merge.
func copyUIMerge(current uiSettings, values map[string]json.RawMessage, board string) (uiSettings, []string, error) {
	b, _ := json.Marshal(current)
	var merged map[string]json.RawMessage
	_ = json.Unmarshal(b, &merged)
	var skipped []string
	for key, raw := range values {
		if !copyUIAllowed(key) {
			return current, nil, fmt.Errorf("setting is not transferable: %s", key)
		}
		valid := false
		if slices.Contains(copyUIBools, key) {
			var v any
			if json.Unmarshal(raw, &v) == nil {
				_, valid = v.(bool)
			}
		} else if enums := copyUIEnums[key]; enums != nil {
			var v string
			valid = json.Unmarshal(raw, &v) == nil && slices.Contains(enums, v)
		} else {
			var v any
			if json.Unmarshal(raw, &v) == nil {
				if n, ok := v.(float64); ok {
					valid = copyUINumbers[key](n)
				}
			}
		}
		if !valid {
			return current, nil, fmt.Errorf("invalid setting: %s", key)
		}
		if !copyUISupported(key, board) {
			skipped = append(skipped, key)
			continue
		}
		merged[key] = raw
	}
	b, _ = json.Marshal(merged)
	var result uiSettings
	if err := json.Unmarshal(b, &result); err != nil {
		return current, nil, err
	}
	if !result.FourDeckWaveform {
		result.PairedView = false
	}
	sort.Strings(skipped)
	return result, skipped, nil
}

// Snapshots are local-only. Preserve bytes, permissions and absence rather than
// reserializing a parsed configuration when rolling back or undoing a copy.
type copyFileSnapshot struct {
	Present bool        `json:"present"`
	Mode    os.FileMode `json:"mode"`
	Bytes   []byte      `json:"bytes"`
}

func snapshotCopyFile(path string) (copyFileSnapshot, error) {
	i, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return copyFileSnapshot{}, nil
	}
	if err != nil {
		return copyFileSnapshot{}, err
	}
	if !i.Mode().IsRegular() || i.Size() > 1<<20 {
		return copyFileSnapshot{}, errors.New("settings snapshot requires a regular file under 1 MiB")
	}
	b, err := os.ReadFile(path)
	return copyFileSnapshot{Present: true, Mode: i.Mode().Perm(), Bytes: b}, err
}

func restoreCopyFile(path string, snap copyFileSnapshot) error {
	if !snap.Present {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".settings-copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(snap.Mode); err == nil {
		_, err = f.Write(snap.Bytes)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
