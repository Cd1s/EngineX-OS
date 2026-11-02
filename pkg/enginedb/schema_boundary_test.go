package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/Cd1s/sc5000-custom-os/pkg/dbhealer"
	"github.com/Cd1s/sc5000-custom-os/pkg/rekordbox"
)

func TestBridgeSchemaHealerAndDeletionInterop(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(q, args...); e != nil {
			t.Fatal(q, e)
		}
	}
	exec(schema)
	exec(`INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('test',3,0,2)`)
	exec(`INSERT INTO Track(id,path,length) VALUES(1,'../short.wav',1024)`)
	p, e := EncodePerformance(rekordbox.Track{}, &rekordbox.Analysis{Waveforms: []rekordbox.Waveform{{Tag: "PWAV", EntryBytes: 1, Data: []byte{31}}}})
	if e != nil {
		t.Fatal(e)
	}
	exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=? WHERE trackId=1`, p.TrackData, p.Overview)
	// The chain-repairing delete trigger is the healer's, not Engine's stock DDL.
	if _, e = dbhealer.Heal(db); e != nil {
		t.Fatal(e)
	}
	// Deleting an external membership (trackId=0) must repair its predecessor.
	exec(`INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'Test',0,0)`)
	exec(`INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES(1,1,1,2),(2,1,0,3),(3,1,1,0)`)
	exec(`DELETE FROM PlaylistEntity WHERE id=2`)
	var next int
	if e = db.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=1`).Scan(&next); e != nil || next != 3 {
		t.Fatal("external membership chain broken", next, e)
	}
	exec(`UPDATE PlaylistEntity SET nextEntityId=3 WHERE id=3`)
	exec(`DELETE FROM PlaylistEntity WHERE id=3`)
	if e = db.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=1`).Scan(&next); e != nil || next != 0 {
		t.Fatal("deleted self-loop retained", next, e)
	}
	report, e := dbhealer.Heal(db)
	if e != nil {
		t.Fatal(e)
	}
	var waveform []byte
	if e = db.QueryRow(`SELECT overviewWaveFormData FROM PerformanceData WHERE trackId=1`).Scan(&waveform); e != nil {
		t.Fatal(e)
	}
	if report.Waveforms != 0 || !bytes.Equal(waveform, p.Overview) {
		t.Fatalf("healer rejected short analysis: %+v", report)
	}
	if e = db.PingContext(context.Background()); e != nil {
		t.Fatal(e)
	}
}
