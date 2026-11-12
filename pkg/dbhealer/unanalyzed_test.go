package dbhealer

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// BH-GC1-12: a track Engine has not analyzed yet has no overview waveform. The mount heal wrote a
// flat 1024-sample placeholder into it (and so bumped the track's lastEditTime through Engine's
// trigger), as if the waveform were damaged. An analyzed track without one is still healed.
func TestHealLeavesTheWaveformOfAnUnanalyzedTrackAlone(t *testing.T) {
	db, path := newLibrary(t)
	mustExec(t, db, "INSERT INTO Track(id,path,isAnalyzed) VALUES(1,'Music/new.mp3',0)")
	track(t, db, 2) // analyzed, but its waveform is missing: damage
	mustExec(t, db, "UPDATE Track SET lastEditTime=111")
	db.Close()
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	blob, _ := os.ReadFile(path)
	if err := os.WriteFile(filepath.Join(dir, "m.db"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := HealMountDataOnly(mount, 1)
	if err != nil {
		t.Fatalf("%+v %v", r, err)
	}
	healed, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer healed.Close()
	var overview []byte
	var edited int64
	if err := healed.QueryRow("SELECT p.overviewWaveFormData,t.lastEditTime FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE p.trackId=1").Scan(&overview, &edited); err != nil {
		t.Fatal(err)
	}
	if len(overview) != 0 || edited != 111 {
		t.Fatalf("unanalyzed track: overview %d bytes, lastEditTime %d; want no waveform and 111", len(overview), edited)
	}
	if err := healed.QueryRow("SELECT overviewWaveFormData FROM PerformanceData WHERE trackId=2").Scan(&overview); err != nil || len(overview) == 0 {
		t.Fatalf("analyzed track without a waveform was not healed (%v)", err)
	}
	if r.Waveforms != 1 {
		t.Fatalf("Waveforms = %d, want 1 (only the analyzed track)", r.Waveforms)
	}
}
