package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClockTrackerAdvancesPeriodicallyAndOnForwardStepNeverBackwards(t *testing.T) {
	dir := t.TempDir()
	clockFile := filepath.Join(dir, "systemd", "timesync", "clock")
	tr := newClockTracker(clockFile)

	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	// 1. Missing file at startup -> created with base mtime.
	updated, err := tr.step(base, 0)
	if err != nil || !updated {
		t.Fatalf("initial step: updated=%v err=%v", updated, err)
	}
	st, err := os.Stat(clockFile)
	if err != nil {
		t.Fatalf("stat clock: %v", err)
	}
	if !st.ModTime().Equal(base) {
		t.Fatalf("initial mtime = %v, want %v", st.ModTime(), base)
	}

	// 2. Normal 2-minute advance (< 10 min, no forward jump) -> does NOT touch disk.
	updated, err = tr.step(base.Add(2*time.Minute), 2*time.Minute)
	if err != nil || updated {
		t.Fatalf("sub-interval step: updated=%v err=%v", updated, err)
	}
	st, _ = os.Stat(clockFile)
	if !st.ModTime().Equal(base) {
		t.Fatalf("mtime changed before 10m interval: %v", st.ModTime())
	}

	// 3. 10-minute periodic boundary -> advances mtime to base + 10m.
	t10 := base.Add(10 * time.Minute)
	updated, err = tr.step(t10, 10*time.Minute)
	if err != nil || !updated {
		t.Fatalf("10m periodic step: updated=%v err=%v", updated, err)
	}
	st, _ = os.Stat(clockFile)
	if !st.ModTime().Equal(t10) {
		t.Fatalf("10m mtime = %v, want %v", st.ModTime(), t10)
	}

	// 4. Manual SetTime / NTP forward step (+45m wall while monotonic advanced 2s) -> updates immediately.
	tStep := t10.Add(45 * time.Minute)
	updated, err = tr.step(tStep, 10*time.Minute+2*time.Second)
	if err != nil || !updated {
		t.Fatalf("forward clock step: updated=%v err=%v", updated, err)
	}
	st, _ = os.Stat(clockFile)
	if !st.ModTime().Equal(tStep) {
		t.Fatalf("forward step mtime = %v, want %v", st.ModTime(), tStep)
	}

	// 5. Backward wall-clock step + 10m elapsed -> NEVER moves mtime backwards.
	tBack := tStep.Add(-30 * time.Minute)
	updated, err = tr.step(tBack, 21*time.Minute)
	if err != nil || updated {
		t.Fatalf("backward step should not update: updated=%v err=%v", updated, err)
	}
	st, _ = os.Stat(clockFile)
	if !st.ModTime().Equal(tStep) {
		t.Fatalf("mtime moved backwards to %v, want %v", st.ModTime(), tStep)
	}

	// 6. Uninitialized pre-2025 RTC (e.g. 1970) -> ignored completely.
	other := filepath.Join(dir, "unseeded", "clock")
	updated, err = advanceClockFile(other, time.Unix(5, 0))
	if err != nil || updated {
		t.Fatalf("1970 clock should be ignored: updated=%v err=%v", updated, err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("1970 clock created file unexpectedly: %v", err)
	}
}

func TestAdvanceClockFileRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	oldTime := time.Date(2026, 10, 30, 11, 41, 29, 0, time.UTC)
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(victim, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "clock")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if updated, err := advanceClockFile(link, oldTime.Add(time.Hour)); err == nil || updated {
		t.Fatalf("expected symlink rejection, got updated=%v err=%v", updated, err)
	}
	st, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(oldTime) {
		t.Fatalf("victim mtime changed through symlink: %v", st.ModTime())
	}
}
