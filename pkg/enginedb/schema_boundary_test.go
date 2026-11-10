package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
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

func TestWriteEntitiesPreservesEntityChain(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'Set',0,0)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// All tracks are mapped; repeated membership keeps first occurrence order.
	mapping := map[uint32]int64{1: 101, 3: 103}
	tracks := []uint32{1, 3, 1}
	if err := writeEntities(context.Background(), tx, 1, tracks, mapping, "test-uuid"); err != nil {
		t.Fatalf("writeEntities failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`SELECT id, trackId, nextEntityId FROM PlaylistEntity WHERE listId=1 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type ent struct {
		id, trackID, nextEntityID int64
	}
	var ents []ent
	for rows.Next() {
		var e ent
		if err := rows.Scan(&e.id, &e.trackID, &e.nextEntityID); err != nil {
			t.Fatal(err)
		}
		ents = append(ents, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(ents) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(ents))
	}
	if ents[0].trackID != 101 || ents[1].trackID != 103 {
		t.Fatalf("unexpected tracks in playlist: %+v", ents)
	}
	// Entity 1 should chain to Entity 2
	if ents[0].nextEntityID != ents[1].id || ents[1].nextEntityID != 0 {
		t.Fatalf("entity chain broken: %+v", ents)
	}
}
