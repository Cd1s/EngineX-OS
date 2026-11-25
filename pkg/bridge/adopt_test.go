package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func execAll(t *testing.T, db *sql.DB, stmts ...struct {
	q    string
	args []any
}) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s.q, s.args...); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
}

func TestAdoptMigratesFastBaseAndDropsPoisonedSynced(t *testing.T) {
	dir := t.TempDir()
	ids, libraryUUID, _, err := enginedb.BuildLibrary(context.Background(), dir, "", []enginedb.TrackRow{
		{Rel: "Contents/a.mp3", Track: rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", Title: "A"}},
		{Rel: "Contents/b.mp3", Track: rekordbox.Track{ID: 2, Path: "/Contents/b.mp3", Title: "B"}},
		{Rel: "Contents/c.mp3", Track: rekordbox.Track{ID: 3, Path: "/Contents/c.mp3", Title: "C"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Poisoned BridgeSynced (87fac745 / NEW_REG5): track 3 has a deck edit whose current
	// PerformanceData matches the stored BridgeSynced digest, so comparing SyncedDigest alone
	// would wrongly keep it.
	mdb, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	deckCues, deckLoops := []byte("deck-cues"), []byte("deck-loops")
	if _, err = mdb.Exec(`UPDATE PerformanceData SET quickCues=?,loops=?,activeOnLoadLoops=1 WHERE trackId=?`, deckCues, deckLoops, ids[2]); err != nil {
		mdb.Close()
		t.Fatal(err)
	}
	mdb.Close()
	poisonedSynced := enginedb.SyncedDigest(nil, deckCues, deckLoops, 1, "", "", "")

	statePath := filepath.Join(dir, oldStateName)
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE BridgeState(path TEXT,trackId INTEGER,fingerprint TEXT);
		CREATE TABLE BridgeFastBase(trackId INTEGER PRIMARY KEY,record INTEGER,rating INTEGER,quickCues BLOB,loops BLOB,active INTEGER);
		CREATE TABLE BridgeWritten(trackId INTEGER PRIMARY KEY,digest BLOB);
		CREATE TABLE BridgeSynced(trackId INTEGER PRIMARY KEY,lastEditTime INTEGER,digest BLOB);`); err != nil {
		t.Fatal(err)
	}
	execAll(t, db,
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeState VALUES('../Contents/a.mp3',?,'fast:old-record')`, []any{ids[0]}},
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeState VALUES('../Contents/b.mp3',?,'fast:cur-record')`, []any{ids[1]}},
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeState VALUES('../Contents/c.mp3',?,'complete-record')`, []any{ids[2]}},
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeFastBase VALUES(?,?,77,X'0102',X'0304',5)`, []any{ids[0], enginedb.FastBaseRecord("fast:old-record")}},
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeFastBase VALUES(?,?,77,X'0506',X'0708',3)`, []any{ids[1], enginedb.FastBaseRecord("fast:wrong-record")}},
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeWritten VALUES(?,X'11223344556677881122334455667788')`, []any{ids[2]}},
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeSynced VALUES(?,999,?)`, []any{ids[2], poisonedSynced}},
	)
	db.Close()

	s, err := adopt(context.Background(), filepath.Join(dir, "m.db"), dir, libraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Tracks["contents/a.mp3"]
	if a == nil || !a.FastKnown || !bytes.Equal(a.FastCues, []byte{1, 2}) || !bytes.Equal(a.FastLoops, []byte{3, 4}) || a.FastActive != 5 {
		t.Fatalf("matching fast baseline not migrated: %+v", a)
	}
	b := s.Tracks["contents/b.mp3"]
	if b == nil || b.FastKnown || len(b.FastCues) != 0 {
		t.Fatalf("mismatched BridgeFastBase.record was adopted: %+v", b)
	}
	c := s.Tracks["contents/c.mp3"]
	if c == nil || c.Synced != nil || c.Edit != 0 {
		t.Fatalf("poisoned BridgeSynced was adopted: edit=%d digest=%x", c.Edit, c.Synced)
	}
}

func TestAdoptNeverScansPerformanceData(t *testing.T) {
	dir := t.TempDir()
	ids, libraryUUID, _, err := enginedb.BuildLibrary(context.Background(), dir, "", []enginedb.TrackRow{
		{Rel: "Contents/a.mp3", Track: rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", Title: "A"}},
		{Rel: "Contents/b.mp3", Track: rekordbox.Track{ID: 2, Path: "/Contents/b.mp3", Title: "B"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Dropping PerformanceData makes any query against it fail immediately.
	mdb, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mdb.Exec(`DROP TABLE PerformanceData`); err != nil {
		mdb.Close()
		t.Fatal(err)
	}
	mdb.Close()

	statePath := filepath.Join(dir, oldStateName)
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE BridgeState(path TEXT,trackId INTEGER,fingerprint TEXT);
		CREATE TABLE BridgeWritten(trackId INTEGER PRIMARY KEY,digest BLOB);
		CREATE TABLE BridgeSynced(trackId INTEGER PRIMARY KEY,lastEditTime INTEGER,digest BLOB);`); err != nil {
		t.Fatal(err)
	}
	execAll(t, db,
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeState VALUES('../Contents/a.mp3',?,'legacy-complete-no-written')`, []any{ids[0]}},
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeState VALUES('../Contents/b.mp3',?,'legacy-complete-with-written')`, []any{ids[1]}},
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeWritten VALUES(?,X'0102030405060708090a0b0c0d0e0f10')`, []any{ids[1]}},
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeSynced VALUES(?,123,X'deadbeef')`, []any{ids[0]}},
		struct {
			q    string
			args []any
		}{`INSERT INTO BridgeSynced VALUES(?,456,X'cafebabe')`, []any{ids[1]}},
	)
	db.Close()

	s, err := adopt(context.Background(), filepath.Join(dir, "m.db"), dir, libraryUUID)
	if err != nil {
		t.Fatalf("adopt touched PerformanceData: %v", err)
	}
	if len(s.Tracks) != 2 {
		t.Fatalf("adopted %d tracks, want 2", len(s.Tracks))
	}
	for k, tr := range s.Tracks {
		if tr.Synced != nil || tr.Edit != 0 {
			t.Fatalf("%s kept BridgeSynced: edit=%d synced=%x", k, tr.Edit, tr.Synced)
		}
	}
}
