package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// writeIDCopies sets the two copies of the gateway ID ceiling ("" removes one).
func writeIDCopies(t *testing.T, stateDir, one, two string) {
	t.Helper()
	for i, content := range []string{one, two} {
		file := filepath.Join(stateDir, []string{"gateway-ids.json", "gateway-ids-2.json"}[i])
		if err := os.RemoveAll(file); err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// writeGatewayFile rewrites the gateway file with counter next and the rows given.
func writeGatewayFile(t *testing.T, stateDir string, next int64, rows []savedRef) {
	t.Helper()
	if err := providers.SaveJSON(filepath.Join(stateDir, "gateway-tracks.json"), savedTracks{Next: next, Tracks: rows}); err != nil {
		t.Fatal(err)
	}
}

// savedRows are the rows of the gateway file as saved.
func savedRows(t *testing.T, stateDir string) []savedRef {
	t.Helper()
	var v savedTracks
	raw, err := os.ReadFile(filepath.Join(stateDir, "gateway-tracks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.Tracks
}

// idRig is a deck whose server one's track got an ID above 5 (so a plausible lower
// ceiling exists) that Engine's library holds, as it keeps a played track. It is
// stopped: the case sets up the state and starts it.
func idRig(t *testing.T) (*nasSwitchRig, string, int64) {
	t.Helper()
	rig := newNASSwitchRig(t)
	lib := floorLibrary(t)
	rig.stop()
	writeIDCopies(t, rig.stateDir, `{"ceiling":5}`, `{"ceiling":5}`)
	rig.start()
	rig.saveNAS(rig.oneURL)
	old := rig.browse()
	rig.plays("server one's row", old, rig.one)
	if old < 3 {
		t.Fatalf("server one's row got ID %d, want one above the copies' 5", old)
	}
	libraryRows(t, lib, old)
	rig.stop()
	return rig, lib, old
}

// #335 round 12 (round-11 re-check, item 1): when the gateway file cannot give a safe
// bound (it is unreadable, a copy is missing or not valid, the copies disagree), a
// surviving ceiling copy alone is not trusted. On 900f8055, with the gateway file
// unreadable and one copy of a plausible but too low ceiling (the reviewer's
// plausible-low-copy), the other copy was repaired from it without looking at Engine's
// library, and server one's ID, still in the library, was handed to server two: it
// played server two's recording. Now the bound is max(every valid copy, the file's IDs,
// every ID still referenced) before any repair or new ID.
func TestNASIDBoundReconcilesWithTheIDsInUse(t *testing.T) {
	for _, c := range []struct {
		name  string
		state func(t *testing.T, rig *nasSwitchRig, old int64)
	}{
		{"plausible-low-copy", func(t *testing.T, rig *nasSwitchRig, old int64) {
			rig.unreadable()
			writeIDCopies(t, rig.stateDir, fmt.Sprintf(`{"ceiling":%d}`, old-1), "")
		}},
		{"copies disagree", func(t *testing.T, rig *nasSwitchRig, old int64) {
			writeGatewayFile(t, rig.stateDir, old-2, nil) // its rows evicted, an old counter
			writeIDCopies(t, rig.stateDir, fmt.Sprintf(`{"ceiling":%d}`, old-1), fmt.Sprintf(`{"ceiling":%d}`, old-2))
		}},
		{"a copy not valid", func(t *testing.T, rig *nasSwitchRig, old int64) {
			writeGatewayFile(t, rig.stateDir, old-1, nil)
			writeIDCopies(t, rig.stateDir, "{", fmt.Sprintf(`{"ceiling":%d}`, old-1))
		}},
		{"a copy missing", func(t *testing.T, rig *nasSwitchRig, old int64) {
			writeGatewayFile(t, rig.stateDir, old-1, nil)
			writeIDCopies(t, rig.stateDir, "", fmt.Sprintf(`{"ceiling":%d}`, old-1))
		}},
		{"gateway file unreadable, equal copies", func(t *testing.T, rig *nasSwitchRig, old int64) {
			rig.unreadable()
			writeIDCopies(t, rig.stateDir, fmt.Sprintf(`{"ceiling":%d}`, old-1), fmt.Sprintf(`{"ceiling":%d}`, old-1))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig, _, old := idRig(t)
			c.state(t, rig, old)
			rig.start()
			rig.saveNAS(rig.twoURL)
			ids := rig.browseIDs()
			if a := rig.stream(old); a.code == http.StatusOK && bytes.Equal(a.body, rig.two) {
				t.Fatalf("server one's ID %d, still in Engine's library, plays server two's recording (rows %v)", old, ids)
			}
			if len(ids) != 1 || ids[0] <= old {
				t.Fatalf("server two's rows %v, want one above Engine's row %d", ids, old)
			}
			rig.plays("server two's row", ids[0], rig.two)
			if a, b := idCopies(t, rig.stateDir); a == 0 || a != b || a < ids[0] {
				t.Fatalf("copies of the ceiling %d and %d, want both equal and at least %d", a, b, ids[0])
			}
		})
	}
}

// #335 round 12 (round-11 re-check, item 1, the converse): a value above maxTrackID is
// never handed out, so it bounds nothing. On 900f8055 an out-of-range counter in the
// gateway file made both good copies "below the gateway file", the counter jumped past
// maxTrackID and no row got an ID, on every writable start. The same for an out-of-range
// ID in Engine's library with every bound of ours lost. Now such a value is ignored
// (logged) and rows come back on the next start.
func TestNASIDOutOfRangeValuesNeverStopRows(t *testing.T) {
	check := func(t *testing.T, rig *nasSwitchRig, old int64, url, step string) int64 {
		t.Helper()
		rig.saveNAS(url)
		ids := rig.browseIDs()
		if len(ids) != 1 || ids[0] <= old || ids[0] > maxTrackID {
			t.Fatalf("%s: rows %v, want one above %d (and below 2^31)", step, ids, old)
		}
		return ids[0]
	}
	t.Run("gateway file counter", func(t *testing.T) {
		rig, _, old := idRig(t)
		writeGatewayFile(t, rig.stateDir, 1<<32, savedRows(t, rig.stateDir))
		rig.start()
		id := check(t, rig, old, rig.twoURL, "first start")
		rig.plays("server two's row", id, rig.two)
		rig.stop()
		rig.start()
		check(t, rig, id, rig.oneURL, "second start")
	})
	t.Run("gateway file row", func(t *testing.T) {
		rig, _, old := idRig(t)
		rows := savedRows(t, rig.stateDir)
		rows = append(rows, savedRef{ID: 1 << 32, Track: providers.Track{ID: "far", Provider: "spotify", Title: "far"}})
		writeGatewayFile(t, rig.stateDir, old, rows)
		rig.start()
		check(t, rig, old, rig.twoURL, "start")
	})
	t.Run("library row with every bound lost", func(t *testing.T) {
		rig, lib, old := idRig(t)
		removeIDCopies(t, rig.stateDir)
		rig.unreadable()
		libraryRows(t, lib, old, 1<<32+1)
		rig.start()
		check(t, rig, old, rig.twoURL, "start")
	})
}
