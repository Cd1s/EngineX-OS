package bridge_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	bridge "github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/bridgetest"
)

func TestP3SessionReadsRewrittenRowOnce(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 12)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	var id int64
	if err := db.QueryRow(`SELECT id FROM Track ORDER BY id LIMIT 1 OFFSET 4`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=quickCues WHERE trackId=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+7 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	snapshotPath := filepath.Join(mount, "Engine Library", "Database2", ".rb-bridge-snapshot")
	before, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	var session bridge.CueSession
	first, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{SkipPreparingCheck: true, Session: &session})
	if err != nil || first.TracksRead != 1 || first.TracksChanged != 0 {
		t.Fatalf("first rewritten-row pass: %+v %v", first, err)
	}
	second, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{SkipPreparingCheck: true, Session: &session})
	if err != nil || second.TracksRead != 0 || second.TracksChanged != 0 {
		t.Fatalf("session reread the same row: %+v %v", second, err)
	}
	after, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("analysis-only session pass rewrote the snapshot")
	}
}

func TestP3EditBeforePullIsFoundOnNextPass(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 6)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	p3DeckCue(t, db, p3FirstTrackID(t, db), "before pull", 1, 1500)
	_ = db.Close()
	original, err := os.ReadFile(filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}
	pulled := errors.New("stick pulled")
	_, err = bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{SkipPreparingCheck: true, BeforeRename: func(string) error { return pulled }})
	if !errors.Is(err, pulled) {
		t.Fatalf("first pass error=%v, want pull error", err)
	}
	after, err := os.ReadFile(filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT"))
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("pulled pass changed ANLZ: %v", err)
	}
	second, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{SkipPreparingCheck: true})
	if err != nil || second.TracksChanged != 1 || !p3AnalysisHas(t, mount, "before pull") {
		t.Fatalf("next pass did not recover the edit: %+v %v", second, err)
	}
}
