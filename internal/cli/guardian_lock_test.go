package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// engine-guardian takes the volume's lock once for a mount, before it applies queued edits,
// heals or fixes grids (#336 Go review R13). While another EngineX program holds it, the run
// waits for at most its --timeout (two seconds without one), touches nothing and fails, so the
// hook tries again at the next mount.
func TestGuardianWaitsForTheVolumeLockAndTouchesNothingWithoutIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	mount := mountWithNFDTracks(t, 8)
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	queue := filepath.Join(dbDir, enginedb.PendingFile)
	queued, _ := json.Marshal(enginedb.Pending{TrackMetadata: []enginedb.PendingTrackMetadata{{TrackID: 1, Comment: "queued in the Web Console"}}})
	if err := os.WriteFile(queue, queued, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := rbstate.LockVolume(mount, 0)
	if err != nil {
		t.Fatal(err)
	}
	// With a --timeout the wait is half of it; without one, two seconds.
	for _, mode := range [][]string{{"--data-only", "--timeout", "600ms"}, {"--normalize-reserve", "--fix-mp3-grids"}, {"--fix-mp3-grids"}, {}} {
		var out, errOut bytes.Buffer
		t0 := time.Now()
		code := Guardian(append([]string{"--heal-mount", mount}, mode...), &out, &errOut)
		if took := time.Since(t0); took > 4*time.Second {
			t.Errorf("%v: the run took %s under a held volume lock, want two seconds at most", mode, took)
		}
		var results []guardianResult
		if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 {
			t.Fatalf("%v: output %q: %v", mode, out.String(), err)
		}
		if code != 1 || results[0].Healthy || !strings.Contains(results[0].Error, "locked") {
			t.Errorf("%v under a held volume lock: code=%d healthy=%v error=%q, want a failed run that names the lock", mode, code, results[0].Healthy, results[0].Error)
		}
	}
	if after, rerr := os.ReadFile(filepath.Join(dbDir, "m.db")); rerr != nil || !bytes.Equal(before, after) {
		t.Errorf("a refused run changed m.db (read error %v)", rerr)
	}
	if after, rerr := os.ReadFile(queue); rerr != nil || !bytes.Equal(queued, after) {
		t.Errorf("a refused run changed the queue of pending edits (read error %v)", rerr)
	}
	// --status only reads: it needs no lock and answers under a held one.
	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--heal-mount", mount, "--status"}, &out, &errOut); code != 0 {
		t.Errorf("--status under a held volume lock: code=%d out=%s err=%s", code, &out, &errOut)
	}
	unlock()

	// With the lock free the run does its work, and has given the lock back at its end.
	out.Reset()
	if code := Guardian([]string{"--heal-mount", mount, "--data-only", "--timeout", "10s"}, &out, &errOut); code != 0 {
		t.Fatalf("with the lock free: code=%d out=%s err=%s", code, &out, &errOut)
	}
	if _, err := os.Lstat(queue); err == nil {
		t.Errorf("the queued edit was not applied with the lock free")
	}
	if u, err := rbstate.LockVolume(mount, 0); err != nil {
		t.Errorf("engine-guardian kept the volume's lock: %v", err)
	} else {
		u()
	}
}
