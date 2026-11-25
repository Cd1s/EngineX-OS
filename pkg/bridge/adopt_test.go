package bridge

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
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

func setupLegacyArtworkStick(t *testing.T) (mount string, sum [sha1.Size]byte) {
	t.Helper()
	mount = stick(t)
	mountNow(t, mount, MountOptions{})

	dir := dbDir(mount)
	snap := snapshot(t, mount)
	if err := os.Remove(filepath.Join(dir, rbstate.SnapshotName)); err != nil {
		t.Fatal(err)
	}
	stateDB, err := sql.Open("sqlite", filepath.Join(dir, oldStateName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stateDB.Exec(`CREATE TABLE BridgeState(path TEXT,trackId INTEGER,fingerprint TEXT);
		CREATE TABLE BridgePlaylist(sourceId INTEGER PRIMARY KEY,listId INTEGER);`); err != nil {
		stateDB.Close()
		t.Fatal(err)
	}
	for _, tr := range snap.Tracks {
		if _, err = stateDB.Exec(`INSERT INTO BridgeState VALUES(?,?,?)`, "../"+tr.Rel, tr.EngineID, "legacy"); err != nil {
			stateDB.Close()
			t.Fatal(err)
		}
	}
	stateDB.Close()

	jpg := []byte{0xff, 0xd8, 0xff, 0xe0, 0x12, 0x34, 0x56}
	sum = sha1.Sum(jpg)
	mdb, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mdb.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES('1111111111111111111111111111111111111111',?)`, jpg); err != nil {
		mdb.Close()
		t.Fatal(err)
	}
	mdb.Close()
	return mount, sum
}

func assertLegacyArtworkState(t *testing.T, dbPath, mount string, sum [sha1.Size]byte, wantKind string, wantFile bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var kind string
	if err = db.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE length(albumArt)>0 ORDER BY id DESC LIMIT 1`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != wantKind {
		t.Fatalf("AlbumArt hash kind=%q in %s, want %q", kind, filepath.Base(dbPath), wantKind)
	}
	artPath := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum[:])+".jpg")
	_, err = os.Stat(artPath)
	if wantFile && err != nil {
		t.Fatalf("expected artwork file %s to exist: %v", artPath, err)
	}
	if !wantFile && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artwork file %s should not exist yet: err=%v", artPath, err)
	}
}

func TestAdoptDefersLegacyArtworkWhileDeckPlayingOrLoading(t *testing.T) {
	t.Run("PlayingDeck", func(t *testing.T) {
		t.Cleanup(SetPriorityDirForTest(t.TempDir()))
		mount, sum := setupLegacyArtworkStick(t)
		dbPath := filepath.Join(dbDir(mount), "m.db")

		q := NewPriorityQueue()
		now := time.Now()
		if !q.UpdateDeckActivity(true, now) {
			t.Fatal("expected deck activity active=true")
		}

		mountNow(t, mount, MountOptions{})
		// Adoption itself completes while playback only defers legacy artwork writes.
		if _, err := os.Stat(filepath.Join(dbDir(mount), rbstate.SnapshotName)); err != nil {
			t.Fatalf("adoption snapshot missing: %v", err)
		}
		assertLegacyArtworkState(t, dbPath, mount, sum, "text", false)

		// Once playback becomes idle (two spaced idle samples), an idle mount migrates artwork.
		q.UpdateDeckActivity(false, now.Add(time.Second))
		q.UpdateDeckActivity(false, now.Add(2*time.Second))
		mountNow(t, mount, MountOptions{Full: true})
		assertLegacyArtworkState(t, dbPath, mount, sum, "blob", true)
	})

	t.Run("LoadingDeck", func(t *testing.T) {
		t.Cleanup(SetPriorityDirForTest(t.TempDir()))
		mount, sum := setupLegacyArtworkStick(t)
		dbPath := filepath.Join(dbDir(mount), "m.db")

		payload := []byte(fmt.Sprintf(`{"active":false,"loading":true,"updatedAtMs":%d}`, time.Now().UnixMilli()))
		if err := os.WriteFile(deckActivityPath(), payload, 0o600); err != nil {
			t.Fatal(err)
		}

		mountNow(t, mount, MountOptions{})
		assertLegacyArtworkState(t, dbPath, mount, sum, "text", false)
	})
}

func TestAdoptLegacyArtworkWindowSpareAndNextPacing(t *testing.T) {
	t.Run("DefersNearThreeSecondSpare", func(t *testing.T) {
		t.Cleanup(SetPriorityDirForTest(t.TempDir()))
		mount, sum := setupLegacyArtworkStick(t)
		dbPath := filepath.Join(dbDir(mount), "m.db")

		// Deadline 4s leaves owed=250ms (< 500ms minimum window margin) right near the 3s spare rule.
		mountNow(t, mount, MountOptions{Deadline: time.Now().Add(4 * time.Second)})
		assertLegacyArtworkState(t, dbPath, mount, sum, "text", false)
	})

	t.Run("NextWindowMigratesWithPacingAndDeckCheck", func(t *testing.T) {
		t.Cleanup(SetPriorityDirForTest(t.TempDir()))
		mount, sum := setupLegacyArtworkStick(t)
		dbPath := filepath.Join(dbDir(mount), "m.db")

		// Adopt while a deck plays so m.db still holds the unmigrated TEXT AlbumArt row.
		q := NewPriorityQueue()
		now := time.Now()
		q.UpdateDeckActivity(true, now)
		mountNow(t, mount, MountOptions{})
		assertLegacyArtworkState(t, dbPath, mount, sum, "text", false)
		q.UpdateDeckActivity(false, now.Add(time.Second))
		q.UpdateDeckActivity(false, now.Add(2*time.Second))

		r, err := newResolver(mount)
		if err != nil {
			t.Fatal(err)
		}
		s := snapshot(t, mount)
		v, err := readJobView(context.Background(), r, mount, s.Layout, "")
		if err != nil {
			t.Fatal(err)
		}
		nb := &nextBuild{
			ctx:   context.Background(),
			mount: mount,
			dir:   dbDir(mount),
			view:  v.view,
			live:  s,
		}
		if err = nb.build(false, true); err != nil {
			t.Fatal(err)
		}
		assertLegacyArtworkState(t, filepath.Join(dbDir(mount), nextDB), mount, sum, "blob", true)

		// Also verify window.migrateLegacyArtwork wires w.rest as batch Pace when migrating an adopted stick.
		mount2, sum2 := setupLegacyArtworkStick(t)
		q2 := NewPriorityQueue()
		now2 := time.Now()
		q2.UpdateDeckActivity(true, now2)
		mountNow(t, mount2, MountOptions{})
		assertLegacyArtworkState(t, filepath.Join(dbDir(mount2), "m.db"), mount2, sum2, "text", false)
		q2.UpdateDeckActivity(false, now2.Add(time.Second))
		q2.UpdateDeckActivity(false, now2.Add(2*time.Second))

		r2, err := newResolver(mount2)
		if err != nil {
			t.Fatal(err)
		}
		s2 := snapshot(t, mount2)
		if !s2.LegacyArtwork {
			t.Fatal("adopted snapshot missing LegacyArtwork=true")
		}
		v2, err := readJobView(context.Background(), r2, mount2, s2.Layout, "")
		if err != nil {
			t.Fatal(err)
		}
		paceCalls := 0
		w := &window{
			ctx:   context.Background(),
			mount: mount2,
			dir:   dbDir(mount2),
			view:  v2.view,
			st:    newSteps(MountOptions{Full: true}),
			o:     MountOptions{Full: true},
			res:   &MountResult{},
			rest:  func(time.Duration) error { paceCalls++; return nil },
		}
		if err = w.run(); err != nil {
			t.Fatal(err)
		}
		if paceCalls == 0 {
			t.Fatal("window artwork migration did not invoke w.rest pacing")
		}
		assertLegacyArtworkState(t, filepath.Join(dbDir(mount2), "m.db"), mount2, sum2, "blob", true)
		if after := snapshot(t, mount2); after.LegacyArtwork {
			t.Fatal("snapshot kept LegacyArtwork=true after all covers migrated")
		}
	})
}
