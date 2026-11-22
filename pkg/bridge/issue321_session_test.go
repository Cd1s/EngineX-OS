package bridge_test

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

// A long-lived caller's session: a row Engine rewrote without a change (it rewrites every
// loaded track on unload) is read once, then not again, and the state is not written for it.
func TestIssue321SessionReadsARewrittenRowOnce(t *testing.T) {
	stick, db := issue321Stick(t, 12)
	id := issue321TrackID(t, db, 5)
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=quickCues WHERE trackId=?`, id); err != nil {
		t.Fatal(err)
	}
	issue321Settle()
	state := filepath.Join(stick, "Engine Library", "Database2", ".rb-bridge-state.db")
	st0, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	var s bridge.CueSession
	res, _ := issue321Pass(t, stick, bridge.WriteBackOptions{Session: &s})
	if issue321Read(res) != 1 || res.TracksChanged != 0 {
		t.Fatalf("rewritten row: %+v", res)
	}
	if res, _ = issue321Pass(t, stick, bridge.WriteBackOptions{Session: &s}); issue321Read(res) != 0 {
		t.Fatalf("the session read the same row again: %+v", res)
	}
	if st1, _ := os.Stat(state); st1.Size() != st0.Size() || !st1.ModTime().Equal(st0.ModTime()) {
		t.Fatalf("a pass that wrote nothing to the stick wrote the state")
	}
}

// A stick imported by an earlier build has no record: the first pass reads every row once and
// starts the record, the next reads none.
func TestIssue321EarlierBuildsStickStartsTheRecordOnce(t *testing.T) {
	stick, db := issue321Stick(t, 12)
	state := filepath.Join(stick, "Engine Library", "Database2", ".rb-bridge-state.db")
	sdb, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: state}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sdb.Exec(`DROP TABLE BridgeSynced`); err != nil {
		t.Fatal(err)
	}
	sdb.Close()
	_ = db
	if res, _ := issue321Pass(t, stick, bridge.WriteBackOptions{}); issue321Read(res) != 12 || res.TracksChanged != 0 {
		t.Fatalf("first pass on an earlier build's stick: %+v", res)
	}
	if res, _ := issue321Pass(t, stick, bridge.WriteBackOptions{}); issue321Read(res) != 0 {
		t.Fatalf("second pass: %+v", res)
	}
}

// A row written back and recorded, then edited back to the import's cues (a deleted hot cue):
// the restored cues match the import's old record, not what the stick holds now, so they are
// written too (the BridgeWritten round trip, for BridgeSynced and the session).
func TestIssue321RestoringTheImportsCuesAfterAWriteIsWrittenBack(t *testing.T) {
	stick, db := issue321Stick(t, 6)
	id := issue321TrackID(t, db, 2)
	var orig []byte
	if err := db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=?`, id).Scan(&orig); err != nil {
		t.Fatal(err)
	}
	var s bridge.CueSession
	issue321Edit(t, db, 2, "Drop", 1000)
	issue321Settle()
	if res, _ := issue321Pass(t, stick, bridge.WriteBackOptions{Session: &s}); res.TracksChanged != 1 || !issue321ANLZHas(t, stick, "Drop") {
		t.Fatalf("first write: %+v", res)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, orig, id); err != nil {
		t.Fatal(err)
	}
	issue321Settle()
	for _, sess := range []*bridge.CueSession{&s, nil} {
		res, _ := issue321Pass(t, stick, bridge.WriteBackOptions{Session: sess})
		if issue321ANLZHas(t, stick, "Drop") {
			t.Fatalf("the deleted hot cue is still on the stick (session %v): %+v", sess != nil, res)
		}
	}
}
