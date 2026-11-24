package bridge_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	bridge "github.com/Cd1s/EngineX-OS/pkg/bridge/v2"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/v2/bridgetest"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

func TestWriteBackUsesSnapshotPathAndSkipsTheSecondPass(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	bridgetest.Import(t, mount)

	resolver, err := compat.NewResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	files, err := bridge.FindSources(resolver, mount)
	if err != nil {
		t.Fatal(err)
	}
	libs, failed, err := bridge.ReadSources(context.Background(), resolver, mount, files, nil)
	if err != nil || len(failed) != 0 || len(libs) != 1 || len(libs[0].Lib.Tracks) == 0 {
		t.Fatalf("sources: libs=%d failed=%v err=%v", len(libs), failed, err)
	}
	rbTrack := libs[0].Lib.Tracks[0]
	analysisRel, err := resolver.Resolve(rbTrack.AnalysisPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(resolver.FullPath(analysisRel))
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := rekordbox.ParseAnalysis(raw)
	if err != nil {
		t.Fatal(err)
	}
	analysis.Cues = append(analysis.Cues, rekordbox.Cue{HotCue: 3, TimeMS: 1500, Name: "v2 write-back"})
	perf, err := enginedb.EncodePerformance(rbTrack, analysis)
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(mount, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	var engineID int64
	if err := db.QueryRow(`SELECT id FROM Track ORDER BY id LIMIT 1`).Scan(&engineID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,quickCues=?,loops=?,activeOnLoadLoops=?,thirdPartySourceId=NULL WHERE trackId=?`, perf.TrackData, perf.QuickCues, perf.Loops, perf.ActiveLoops, engineID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	first, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{SkipPreparingCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.TracksChanged != 1 || first.FilesUpdated == 0 {
		t.Fatalf("first pass: %+v", first)
	}
	second, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{SkipPreparingCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.TracksRead != 0 || second.TracksChanged != 0 || second.FilesUpdated != 0 || second.PhasesMS["decrypt"] != 0 {
		t.Fatalf("second pass reread or decrypted: %+v", second)
	}
}
