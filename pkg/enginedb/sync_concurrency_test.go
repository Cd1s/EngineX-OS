package enginedb

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestSyncRollsBackIfEngineOpensDuringResume(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Sync(context.Background(), root, l, Options{Fast: true}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = Sync(context.Background(), root, l, Options{
		Resume: true, Deadline: time.Now().Add(time.Hour),
		Now: func() time.Time {
			calls++
			// The first two clock reads calculate the budget before open();
			// subsequent reads prepare unfinished tracks inside the transaction.
			if calls == 3 {
				holdOpen(t, filepath.Join(root, "Engine Library", "Database2", "m.db"))
			}
			return time.Now()
		},
	})
	if calls < 3 {
		t.Fatal("resume preparation was not exercised")
	}
	if !errors.Is(err, ErrLibraryInUse) {
		t.Fatalf("committed while Engine opened the library during resume: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM Track WHERE isAnalyzed=1`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("resume changes were not rolled back: analyzed=%d %v", n, err)
	}
}

func TestSyncRefusesLibraryOpenedDuringPreparation(t *testing.T) {
	p, root, engine := testlibrary.Create(t)
	l := &rekordbox.Library{Playlists: []rekordbox.Playlist{{ID: 1, Name: "Imported"}}}
	var release func()
	_, err := Sync(context.Background(), root, l, Options{
		Deadline: time.Now().Add(time.Hour),
		Now: func() time.Time {
			if release == nil {
				if _, e := engine.Exec(`INSERT INTO Playlist(id,title) VALUES(99,'Live set')`); e != nil {
					t.Fatal(e)
				}
				release = holdOpen(t, p)
			}
			return time.Now()
		},
	})
	if !errors.Is(err, ErrLibraryInUse) {
		t.Fatalf("wrote library held by Engine: %v", err)
	}
	var n int
	if e := engine.QueryRow(`SELECT count(*) FROM Playlist`).Scan(&n); e != nil || n != 1 {
		t.Fatalf("library changed: %d %v", n, e)
	}
	release()
	if _, err = Sync(context.Background(), root, l, Options{}); err != nil {
		t.Fatalf("retry after Engine closed: %v", err)
	}
}

func TestOpenReservesWriterBeforeReading(t *testing.T) {
	p, _, engine := testlibrary.Create(t)
	if _, err := engine.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=0`); err != nil {
		t.Fatal(err)
	}
	db, tx, err := open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx.Rollback()
	// Each transaction must reserve its write lock before establishing a read
	// snapshot; otherwise a concurrent commit makes its upgrade fail with 517.
	tx, err = db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT count(*) FROM Track`); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Exec(`INSERT INTO Playlist(id,title) VALUES(99,'Concurrent')`); err == nil {
		t.Fatal("another writer committed after the import transaction began")
	}
	if _, err = tx.Exec(`INSERT INTO Playlist(id,title) VALUES(1,'Imported')`); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
