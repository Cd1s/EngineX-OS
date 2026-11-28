package dbhealer

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

type orderedPlaylistEntry struct {
	trackID int64
	uuid    sql.NullString
	mr      sql.NullInt64
}

func mrVal(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func readOrderedPlaylistEntries(t *testing.T, db *sql.DB, listID int64) []orderedPlaylistEntry {
	t.Helper()
	ids := chainIDs(t, db, "PlaylistEntity", "listId", "nextEntityId", listID)
	out := make([]orderedPlaylistEntry, 0, len(ids))
	for _, id := range ids {
		var e orderedPlaylistEntry
		if err := db.QueryRow("SELECT trackId, databaseUuid, membershipReference FROM PlaylistEntity WHERE id=?", id).Scan(&e.trackID, &e.uuid, &e.mr); err != nil {
			t.Fatalf("read PlaylistEntity %d: %v", id, err)
		}
		out = append(out, e)
	}
	return out
}

func seedS3DuplicateFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := newLibrary(t)
	track(t, db, 1)
	track(t, db, 2)
	track(t, db, 3)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'Club Set',0,0,1)")

	// SQLite UNIQUE(listId, databaseUuid, trackId) permits multiple NULL UUID rows
	// and allows '' alongside the library UUID. Stock Engine's CraylistEntitiesToPack
	// joins on (trackId, databaseUuid), which fails to match NULL across repeated packs
	// and appends duplicate rows on every sync.
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(10,1,1,NULL,20,0)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(20,1,1,NULL,30,1)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(30,1,2,'',40,1)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(40,1,2,'test-library',50,0)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(50,1,3,'remote-library',60,0)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(60,1,3,'test-library',0,0)")
	return db
}

type entityReportCounts struct {
	Duplicates int `json:"playlist_entity_duplicates"`
	Orphans    int `json:"playlist_entity_orphans"`
}

func reportEntityCounts(t *testing.T, r Report) entityReportCounts {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var counts entityReportCounts
	if err := json.Unmarshal(raw, &counts); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	return counts
}

func TestS3PlaylistEntityDeduplicatesNullAndEmptyUUIDs(t *testing.T) {
	for _, dataOnly := range []bool{false, true} {
		name := "full"
		if dataOnly {
			name = "data-only"
		}
		t.Run(name, func(t *testing.T) {
			db := seedS3DuplicateFixture(t)

			r, err := heal(db, dataOnly)
			if err != nil {
				t.Fatalf("heal(dataOnly=%v): %v", dataOnly, err)
			}
			counts := reportEntityCounts(t, r)
			if counts.Duplicates != 2 || counts.Orphans != 0 {
				t.Fatalf("report duplicates=%d orphans=%d, want 2 and 0", counts.Duplicates, counts.Orphans)
			}

			var count int
			if err := db.QueryRow("SELECT count(*) FROM PlaylistEntity WHERE listId=1").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 4 {
				t.Fatalf("expected 4 deduplicated PlaylistEntity rows, got %d", count)
			}

			var missingUUID int
			if err := db.QueryRow("SELECT count(*) FROM PlaylistEntity WHERE listId=1 AND trackId>0 AND (databaseUuid IS NULL OR databaseUuid='')").Scan(&missingUUID); err != nil {
				t.Fatal(err)
			}
			if missingUUID != 0 {
				t.Fatalf("expected 0 NULL/empty databaseUuid rows for local tracks, got %d", missingUUID)
			}

			// Verify surviving chain order, canonical UUIDs, and direct membership (0).
			got := readOrderedPlaylistEntries(t, db, 1)
			want := []orderedPlaylistEntry{
				{trackID: 1, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
				{trackID: 2, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
				{trackID: 3, uuid: sql.NullString{String: "remote-library", Valid: true}, mr: mrVal(0)},
				{trackID: 3, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ordered entries = %+v, want %+v", got, want)
			}

			// Two complete restaged stock Engine 5.3.0 packs must stay at 4 rows.
			assertStockPackExportSucceeds(t, db, 1, 4)
		})
	}
}

func TestS3InterleavedDuplicatePreservesFirstPositionAndDirectMembership(t *testing.T) {
	for _, dataOnly := range []bool{false, true} {
		name := "full"
		if dataOnly {
			name = "data-only"
		}
		t.Run(name, func(t *testing.T) {
			db, _ := newLibrary(t)
			track(t, db, 1)
			track(t, db, 2)
			track(t, db, 3)
			mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'Set',0,0,1)")
			// A(direct, NULL) -> B -> A(inherited, canonical) -> C must keep A at position 1.
			mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES"+
				"(10,1,1,NULL,20,0),(20,1,2,'test-library',30,0),(30,1,1,'test-library',40,1),(40,1,3,'test-library',0,0)")

			r, err := heal(db, dataOnly)
			if err != nil {
				t.Fatalf("heal(dataOnly=%v): %v", dataOnly, err)
			}
			counts := reportEntityCounts(t, r)
			if counts.Duplicates != 1 || counts.Orphans != 0 {
				t.Fatalf("report duplicates=%d orphans=%d, want 1 and 0", counts.Duplicates, counts.Orphans)
			}

			got := readOrderedPlaylistEntries(t, db, 1)
			want := []orderedPlaylistEntry{
				{trackID: 1, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
				{trackID: 2, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
				{trackID: 3, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ordered entries = %+v, want %+v", got, want)
			}

			assertStockPackExportSucceeds(t, db, 1, 3)
		})
	}
}

func TestS3NullableMembershipMergePreservesDirectAndInherited(t *testing.T) {
	cases := []struct {
		name string
		mrs  []any
		want sql.NullInt64
	}{
		{name: "null_then_direct", mrs: []any{nil, 0}, want: mrVal(0)},
		{name: "inherited_then_null", mrs: []any{1, nil}, want: mrVal(1)},
		{name: "null_then_inherited", mrs: []any{nil, 1}, want: mrVal(1)},
		{name: "null_then_null", mrs: []any{nil, nil}, want: sql.NullInt64{Valid: false}},
		{name: "null_inherited_direct", mrs: []any{nil, 1, 0}, want: mrVal(0)},
	}
	for _, dataOnly := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/dataOnly=%t", tc.name, dataOnly), func(t *testing.T) {
				db, _ := newLibrary(t)
				track(t, db, 1)
				mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'Set',0,0,1)")
				if len(tc.mrs) == 2 {
					mustExec(t, db,
						"INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(10,1,1,NULL,20,?),(20,1,1,'test-library',0,?)",
						tc.mrs[0], tc.mrs[1],
					)
				} else {
					mustExec(t, db,
						"INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(10,1,1,NULL,20,?),(20,1,1,NULL,30,?),(30,1,1,'test-library',0,?)",
						tc.mrs[0], tc.mrs[1], tc.mrs[2],
					)
				}

				r, err := heal(db, dataOnly)
				if err != nil {
					t.Fatalf("heal(dataOnly=%v): %v", dataOnly, err)
				}
				counts := reportEntityCounts(t, r)
				if counts.Duplicates != len(tc.mrs)-1 || counts.Orphans != 0 {
					t.Fatalf("report duplicates=%d orphans=%d, want %d and 0", counts.Duplicates, counts.Orphans, len(tc.mrs)-1)
				}

				got := readOrderedPlaylistEntries(t, db, 1)
				want := []orderedPlaylistEntry{
					{trackID: 1, uuid: sql.NullString{String: "test-library", Valid: true}, mr: tc.want},
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("ordered entries = %+v, want %+v", got, want)
				}
			})
		}
	}
}

func TestS3PreservesUnknownSourceNullAndBlankUUIDs(t *testing.T) {
	for _, dataOnly := range []bool{false, true} {
		name := "full"
		if dataOnly {
			name = "data-only"
		}
		t.Run(name, func(t *testing.T) {
			db, _ := newLibrary(t)
			track(t, db, 1) // local track; track 77 is intentionally absent (unknown source)
			mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'UnknownOnly',0,2,1),(2,'Mixed',0,0,1)")
			mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES"+
				"(10,1,77,NULL,20,0),(20,1,77,NULL,30,1),(30,1,77,'',0,0)")
			// Even inside a playlist that also heals a local track, unresolved non-local rows stay untouched.
			mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES"+
				"(40,2,1,NULL,50,0),(50,2,77,NULL,60,0),(60,2,1,'test-library',70,1),(70,2,77,NULL,0,1)")

			r, err := heal(db, dataOnly)
			if err != nil {
				t.Fatalf("heal(dataOnly=%v): %v", dataOnly, err)
			}
			counts := reportEntityCounts(t, r)
			if counts.Duplicates != 1 || counts.Orphans != 0 {
				t.Fatalf("report duplicates=%d orphans=%d, want 1 and 0", counts.Duplicates, counts.Orphans)
			}

			got1 := readOrderedPlaylistEntries(t, db, 1)
			want1 := []orderedPlaylistEntry{
				{trackID: 77, uuid: sql.NullString{Valid: false}, mr: mrVal(0)},
				{trackID: 77, uuid: sql.NullString{Valid: false}, mr: mrVal(1)},
				{trackID: 77, uuid: sql.NullString{String: "", Valid: true}, mr: mrVal(0)},
			}
			if !reflect.DeepEqual(got1, want1) {
				t.Fatalf("list 1 unresolved entries = %+v, want %+v", got1, want1)
			}

			got2 := readOrderedPlaylistEntries(t, db, 2)
			want2 := []orderedPlaylistEntry{
				{trackID: 1, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
				{trackID: 77, uuid: sql.NullString{Valid: false}, mr: mrVal(0)},
				{trackID: 77, uuid: sql.NullString{Valid: false}, mr: mrVal(1)},
			}
			if !reflect.DeepEqual(got2, want2) {
				t.Fatalf("list 2 mixed entries = %+v, want %+v", got2, want2)
			}
		})
	}
}

func TestS3OrphanPlaylistEntitiesRemoved(t *testing.T) {
	db, _ := newLibrary(t)
	track(t, db, 1)
	track(t, db, 2)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'Live',0,0,1)")
	mustExec(t, db, "PRAGMA foreign_keys=OFF")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(10,1,1,'test-library',0,0)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(20,99,2,'test-library',0,0)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(30,NULL,2,'test-library',0,0)")
	mustExec(t, db, "PRAGMA foreign_keys=ON")

	r, err := heal(db, true)
	if err != nil {
		t.Fatal(err)
	}
	counts := reportEntityCounts(t, r)
	if counts.Duplicates != 0 || counts.Orphans != 2 {
		t.Fatalf("JSON counts = %+v, want duplicates=0 orphans=2", counts)
	}

	var orphans int
	if err := db.QueryRow("SELECT count(*) FROM PlaylistEntity WHERE listId IS NULL OR listId NOT IN (SELECT id FROM Playlist)").Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("expected 0 orphan PlaylistEntity rows, got %d", orphans)
	}

	// Re-creating Playlist 99 and inserting track 2 with id 20 must not collide.
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(99,'Recreated',0,0,1)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(20,99,2,'test-library',0,0)")
}

func TestS3AdvancesLaggingSqliteSequence(t *testing.T) {
	db, _ := newLibrary(t)
	track(t, db, 25)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(10,'Main',0,0,1)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(50,10,25,'test-library',0,0)")
	mustExec(t, db, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(5,'h',X'00')")
	mustExec(t, db, "DELETE FROM AlbumArt WHERE id=5")

	// Simulate corrupt/lagging sqlite_sequence entries (below MAX(id)) while AlbumArt stays ahead.
	mustExec(t, db, "UPDATE sqlite_sequence SET seq=10 WHERE name='PlaylistEntity'")
	mustExec(t, db, "UPDATE sqlite_sequence SET seq=2 WHERE name='Playlist'")
	mustExec(t, db, "UPDATE sqlite_sequence SET seq=5 WHERE name='Track'")
	mustExec(t, db, "UPDATE sqlite_sequence SET seq=99 WHERE name='AlbumArt'")

	if _, err := heal(db, true); err != nil {
		t.Fatal(err)
	}

	checkSeq := func(table string, want int64) {
		t.Helper()
		var got int64
		if err := db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name=?", table).Scan(&got); err != nil {
			t.Fatalf("sqlite_sequence[%s]: %v", table, err)
		}
		if got != want {
			t.Fatalf("sqlite_sequence[%s] = %d, want %d", table, got, want)
		}
	}
	checkSeq("PlaylistEntity", 50)
	checkSeq("Playlist", 10)
	checkSeq("Track", 25)
	checkSeq("AlbumArt", 99) // Never lower seq.

	// Explicit seq+1 allocation (as opposed to CraylistEntitiesToPack's MAX(id)-based staging)
	// must advance past MAX(id) so it does not collide with an existing PlaylistEntity.id.
	var nextEntityID int64
	if err := db.QueryRow("SELECT seq+1 FROM sqlite_sequence WHERE name='PlaylistEntity'").Scan(&nextEntityID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "INSERT INTO Track(id,path,isAnalyzed) VALUES(26,'Music/26.mp3',1)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,10,26,'test-library',0,0)", nextEntityID)
}

func TestS3NullSqliteSequenceInitializesAndDoesNotAbortRepairs(t *testing.T) {
	db, _ := newLibrary(t)
	track(t, db, 1)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'Set',0,0,1)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(10,1,1,'test-library',999,0)")
	mustExec(t, db, "UPDATE sqlite_sequence SET seq=NULL WHERE name='PlaylistEntity'")
	mustExec(t, db, "UPDATE sqlite_sequence SET seq=77 WHERE name='Track'")

	if _, err := heal(db, true); err != nil {
		t.Fatalf("heal with NULL sqlite_sequence.seq failed: %v", err)
	}

	var next, peSeq, trackSeq int64
	if err := db.QueryRow("SELECT nextEntityId FROM PlaylistEntity WHERE id=10").Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != 0 {
		t.Fatalf("dangling nextEntityId = %d, want 0", next)
	}
	if err := db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='PlaylistEntity'").Scan(&peSeq); err != nil {
		t.Fatal(err)
	}
	if peSeq != 10 {
		t.Fatalf("PlaylistEntity seq = %d, want 10", peSeq)
	}
	if err := db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='Track'").Scan(&trackSeq); err != nil {
		t.Fatal(err)
	}
	if trackSeq != 77 {
		t.Fatalf("Track seq = %d, want preserved 77", trackSeq)
	}
}

func TestS3UnrelatedSqliteSequenceDoesNotAbortRepairs(t *testing.T) {
	db, _ := newLibrary(t)
	track(t, db, 1)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'Set',0,0,1)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(10,1,1,'test-library',999,0)")
	mustExec(t, db, "INSERT INTO Pack(id,packId,changeLogId) VALUES(1,'fixture',1)")
	mustExec(t, db, "UPDATE sqlite_sequence SET seq='unset' WHERE name='Pack'")

	if _, err := heal(db, true); err != nil {
		t.Fatalf("heal aborted on unrelated Pack.seq: %v", err)
	}

	var next int64
	var packSeq string
	if err := db.QueryRow("SELECT nextEntityId, (SELECT seq FROM sqlite_sequence WHERE name='Pack') FROM PlaylistEntity WHERE id=10").Scan(&next, &packSeq); err != nil {
		t.Fatal(err)
	}
	if next != 0 || packSeq != "unset" {
		t.Fatalf("next=%d Pack.seq=%q, want 0 and \"unset\"", next, packSeq)
	}
}

func TestS3ConflictingSqliteSequenceRowsRaisedToHighWaterMark(t *testing.T) {
	for _, tc := range []struct {
		name     string
		firstSeq any
	}{
		{name: "lower_sibling", firstSeq: 9},
		{name: "null_sibling", firstSeq: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := newLibrary(t)
			track(t, db, 1)
			track(t, db, 2)
			mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'Set',0,0,1)")
			mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(10,1,1,'test-library',11,0),(11,1,2,'test-library',0,0)")
			mustExec(t, db, "UPDATE sqlite_sequence SET seq=? WHERE name='PlaylistEntity'", tc.firstSeq)
			mustExec(t, db, "INSERT INTO sqlite_sequence(name,seq) VALUES('PlaylistEntity',80)")

			if _, err := heal(db, true); err != nil {
				t.Fatal(err)
			}

			var low, high int64
			if err := db.QueryRow("SELECT MIN(seq), MAX(seq) FROM sqlite_sequence WHERE name='PlaylistEntity'").Scan(&low, &high); err != nil {
				t.Fatal(err)
			}
			if low != 80 || high != 80 {
				t.Fatalf("sqlite_sequence[PlaylistEntity] min=%d max=%d, want 80 and 80", low, high)
			}
			mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES((SELECT seq+1 FROM sqlite_sequence WHERE name='PlaylistEntity' ORDER BY rowid LIMIT 1),1,3,'test-library',0,0)")
		})
	}
}

func TestFinalS3SequenceAfterLastOrphan(t *testing.T) {
	for _, dataOnly := range []bool{false, true} {
		for _, keepRow := range []bool{false, true} {
			t.Run(fmt.Sprintf("dataOnly=%t/keepRow=%t", dataOnly, keepRow), func(t *testing.T) {
				db, _ := newLibrary(t)
				track(t, db, 1)
				track(t, db, 2)
				mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'Set',0,0)")
				const insert = "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES "
				mustExec(t, db, insert+"(10,1,1,'test-library',0,0)")
				mustExec(t, db, "DELETE FROM PlaylistEntity WHERE id=10")
				if keepRow {
					mustExec(t, db, insert+"(12,1,2,'test-library',0,0)")
				}
				mustExec(t, db, "PRAGMA foreign_keys=OFF")
				mustExec(t, db, insert+"(80,99,1,'test-library',0,0)")
				mustExec(t, db, "PRAGMA foreign_keys=ON")
				mustExec(t, db, "UPDATE sqlite_sequence SET seq=9 WHERE name='PlaylistEntity'")
				mustExec(t, db, "INSERT INTO sqlite_sequence(name,seq) VALUES('PlaylistEntity',80)")

				if _, err := heal(db, dataOnly); err != nil {
					t.Fatal(err)
				}
				var remaining, low, high, next int
				if err := db.QueryRow("SELECT (SELECT count(*) FROM PlaylistEntity),min(seq),max(seq) FROM sqlite_sequence WHERE name='PlaylistEntity'").Scan(&remaining, &low, &high); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow("INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(1,1,'test-library',0,0) RETURNING id").Scan(&next); err != nil {
					t.Fatal(err)
				}
				if keepRow != (remaining == 1) || low != 80 || high != 80 || next != 81 {
					t.Fatalf("remaining=%d counters=%d/%d next automatic ID=%d", remaining, low, high, next)
				}
			})
		}
	}
}

func TestS3EmptyTableSequenceFastPathAndNullCounter(t *testing.T) {
	db, _ := newLibrary(t)
	track(t, db, 1)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'Set',0,0)")
	// PreparelistEntity is empty and has no sqlite_sequence row: must stay absent.
	// AlbumArt is empty with a NULL + high-water sibling: must reconcile to 42.
	mustExec(t, db, "INSERT INTO sqlite_sequence(name,seq) VALUES('AlbumArt',NULL),('AlbumArt',42)")

	if _, err := heal(db, true); err != nil {
		t.Fatal(err)
	}
	var prepSeqCount int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_sequence WHERE name='PreparelistEntity'").Scan(&prepSeqCount); err != nil {
		t.Fatal(err)
	}
	if prepSeqCount != 0 {
		t.Fatalf("healthy empty PreparelistEntity gained %d sqlite_sequence row(s), want 0", prepSeqCount)
	}
	var low, high int64
	if err := db.QueryRow("SELECT MIN(seq), MAX(seq) FROM sqlite_sequence WHERE name='AlbumArt'").Scan(&low, &high); err != nil {
		t.Fatal(err)
	}
	if low != 42 || high != 42 {
		t.Fatalf("empty AlbumArt sqlite_sequence min=%d max=%d, want 42 and 42", low, high)
	}
}

func TestS3HealMountRespectsVolumeLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	mount := filepath.Join(t.TempDir(), "s3-stick")
	dbPath := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("testdata/engine-5.0.4.sql")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	mustExec(t, db, strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", ""))
	mustExec(t, db, "INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('s3-lock-uuid',3,0,2)")
	mustExec(t, db, "INSERT INTO Track(id,path,isAnalyzed) VALUES(1,'Music/1.mp3',1)")
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'Set',0,0,1)")
	mustExec(t, db, "PRAGMA foreign_keys=OFF")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(10,1,1,NULL,20,0),(20,1,1,'s3-lock-uuid',0,1),(80,99,1,'s3-lock-uuid',0,0)")
	mustExec(t, db, "PRAGMA foreign_keys=ON")
	mustExec(t, db, "UPDATE sqlite_sequence SET seq=9 WHERE name='PlaylistEntity'")
	mustExec(t, db, "INSERT INTO sqlite_sequence(name,seq) VALUES('PlaylistEntity',80)")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	prevWait := HealLockWait
	HealLockWait = 150 * time.Millisecond
	defer func() { HealLockWait = prevWait }()

	unlock, err := rbstate.LockVolume(mount, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = HealMountDataOnly(mount, 0)
	unlock()
	if !errors.Is(err, rbstate.ErrLocked) {
		t.Fatalf("S3 heal under held volume lock: %v, want rbstate.ErrLocked", err)
	}
	if after, rerr := os.ReadFile(dbPath); rerr != nil || !bytes.Equal(before, after) {
		t.Fatalf("refused S3 heal modified m.db (read err=%v)", rerr)
	}

	r, err := HealMountDataOnly(mount, 0)
	if err != nil {
		t.Fatalf("S3 heal after lock release: %v", err)
	}
	counts := reportEntityCounts(t, r)
	if counts.Duplicates != 1 || counts.Orphans != 1 {
		t.Fatalf("healed report counts = %+v, want duplicates=1 orphans=1", counts)
	}
}

func TestS3MixedSourceConservationAndKnownLocalRepeatedExport(t *testing.T) {
	t.Run("known_local_repeated_export_stable", func(t *testing.T) {
		db, _ := newLibrary(t)
		track(t, db, 1)
		track(t, db, 2)
		mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'LocalSet',0,0,1)")
		mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES"+
			"(10,1,1,NULL,20,0),(20,1,2,'test-library',30,0),(30,1,1,'test-library',0,1)")
		mustExec(t, db, "PRAGMA foreign_keys=OFF")
		mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(50,99,2,'test-library',0,0)")
		mustExec(t, db, "PRAGMA foreign_keys=ON")
		mustExec(t, db, "UPDATE sqlite_sequence SET seq=1 WHERE name='PlaylistEntity'")

		r, err := heal(db, true)
		if err != nil {
			t.Fatal(err)
		}
		counts := reportEntityCounts(t, r)
		if counts.Duplicates != 1 || counts.Orphans != 1 {
			t.Fatalf("report duplicates=%d orphans=%d, want 1 and 1", counts.Duplicates, counts.Orphans)
		}
		got := readOrderedPlaylistEntries(t, db, 1)
		want := []orderedPlaylistEntry{
			{trackID: 1, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
			{trackID: 2, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ordered entries = %+v, want %+v", got, want)
		}
		assertStockPackExportSucceeds(t, db, 1, 2)
	})

	t.Run("unresolved_null_conserved_still_grows_on_stock_repeat_pack", func(t *testing.T) {
		db, _ := newLibrary(t)
		track(t, db, 1)
		track(t, db, 2)
		// Track 77 is intentionally absent from Track (unresolved external source).
		mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'MixedSet',0,0,1)")
		mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES"+
			"(10,1,1,NULL,20,0),(20,1,2,'test-library',30,0),(30,1,1,'test-library',40,1),(40,1,77,NULL,0,0)")

		r, err := heal(db, true)
		if err != nil {
			t.Fatal(err)
		}
		counts := reportEntityCounts(t, r)
		if counts.Duplicates != 1 || counts.Orphans != 0 {
			t.Fatalf("report duplicates=%d orphans=%d, want 1 and 0", counts.Duplicates, counts.Orphans)
		}
		got := readOrderedPlaylistEntries(t, db, 1)
		want := []orderedPlaylistEntry{
			{trackID: 1, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
			{trackID: 2, uuid: sql.NullString{String: "test-library", Valid: true}, mr: mrVal(0)},
			{trackID: 77, uuid: sql.NullString{Valid: false}, mr: mrVal(0)},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mixed ordered entries = %+v, want %+v", got, want)
		}
		// Because the unresolved NULL-UUID row is intentionally conserved, stock
		// CraylistEntitiesToPack's (trackId, databaseUuid) = (...) join cannot match
		// NULL = NULL on pass 2 and still grows by that one unresolved row (3 -> 4).
		sizes := runTwoStockPackExports(t, db, 1)
		if sizes != [2]int{3, 4} {
			t.Fatalf("mixed target row counts after two restaged packs = %v, want [3 4]", sizes)
		}
	})
}

func TestS3RepeatedPackExportPreventsNullUUIDGrowth(t *testing.T) {
	// Control: without healing, two complete restaged CraylistEntitiesToPack passes
	// against the same populated destination grow from 6 to 8 rows because NULL never matches NULL.
	unhealed := seedS3DuplicateFixture(t)
	unhealedSizes := runTwoStockPackExports(t, unhealed, 1)
	if unhealedSizes != [2]int{6, 8} {
		t.Fatalf("unhealed target row counts after two restaged packs = %v, want [6 8]", unhealedSizes)
	}

	// Healed: local NULL/empty UUID duplicates are resolved before export, so two
	// complete restaged packs stay stable at 4 rows.
	healed := seedS3DuplicateFixture(t)
	if _, err := heal(healed, true); err != nil {
		t.Fatal(err)
	}
	healedSizes := runTwoStockPackExports(t, healed, 1)
	if healedSizes != [2]int{4, 4} {
		t.Fatalf("healed target row counts after two restaged packs = %v, want [4 4]", healedSizes)
	}
}

func TestS3CrossListAndNegativeNextEntityIdExportSucceeds(t *testing.T) {
	db, _ := newLibrary(t)
	track(t, db, 1)
	track(t, db, 2)
	track(t, db, 3)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'One',0,2,1),(2,'Two',0,0,1)")
	// Row 10 points across lists to row 30 (in list 2); row 20 carries a negative nextEntityId
	// left by an interrupted stock pack/export transaction.
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(10,1,1,'test-library',30,0)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(20,1,2,'test-library',-11,0)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(30,2,3,'test-library',0,0)")

	if _, err := heal(db, true); err != nil {
		t.Fatal(err)
	}

	// Stock Engine PlaylistOrder CTE (which joins without listId filter on recursion)
	// must only traverse list 1's two tracks, never pulling list 2's row 30.
	rows, err := db.Query(`
WITH PlaylistOrder AS (
	SELECT id AS entityId, nextEntityId, 1 AS reverseOrder, trackId
	FROM PlaylistEntity WHERE nextEntityId = 0 AND listId = 1
	UNION ALL
	SELECT c.id, c.nextEntityId, l.reverseOrder + 1, c.trackId
	FROM PlaylistEntity c
	JOIN PlaylistOrder l ON c.nextEntityId = l.entityId
)
SELECT entityId FROM PlaylistOrder ORDER BY reverseOrder DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if fmt.Sprint(got) != "[10 20]" {
		t.Fatalf("PlaylistOrder list 1 = %v, want [10 20]", got)
	}

	assertStockPackExportSucceeds(t, db, 1, 2)
	assertStockPackExportSucceeds(t, db, 2, 1)
}

// runTwoStockPackExports replays two full restaged Engine 5.3.0 CraylistEntitiesToPack
// passes from src into the same target database and returns target row counts after each pass.
func runTwoStockPackExports(t *testing.T, src *sql.DB, listID int64) [2]int {
	t.Helper()
	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := sql.Open("sqlite", dstPath)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("testdata/engine-5.0.4.sql")
	if err != nil {
		dst.Close()
		t.Fatal(err)
	}
	mustExec(t, dst, strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", ""))
	mustExec(t, dst, "INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('target-library',3,0,2)")
	mustExec(t, dst, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'Exported',0,0,1)")
	dst.Close()

	mustExec(t, src, fmt.Sprintf("ATTACH DATABASE '%s' AS dst", strings.ReplaceAll(dstPath, "'", "''")))
	defer mustExec(t, src, "DETACH DATABASE dst")

	mustExec(t, src, "CREATE TEMP TABLE IF NOT EXISTS CraylistsToPack(id INTEGER PRIMARY KEY, sourceId INTEGER)")
	mustExec(t, src, "DELETE FROM CraylistsToPack")
	mustExec(t, src, "INSERT INTO CraylistsToPack(id, sourceId) VALUES(1, ?)", listID)

	mustExec(t, src, `
CREATE TEMP TABLE IF NOT EXISTS CraylistEntitiesToPack (
	id INTEGER,
	listId INTEGER,
	trackId INTEGER,
	databaseUuid TEXT,
	nextEntityId INTEGER,
	membershipReference INTEGER
)`)

	// Restage against the populated destination on every pass, matching repeated stock syncs.
	var sizes [2]int
	for pass := 0; pass < 2; pass++ {
		mustExec(t, src, "DELETE FROM CraylistEntitiesToPack")
		mustExec(t, src, `
WITH targetEntities AS (
	SELECT
		craylistEntities.id AS id,
		CraylistsToPack.id AS newListId,
		sourceEntities.trackId,
		sourceEntities.databaseUuid,
		sourceEntities.id AS sourceId,
		sourceEntities.nextEntityId AS sourceNextId,
		sourceEntities.membershipReference AS sourceMR,
		craylistEntities.membershipReference AS targetMR
	FROM (
		SELECT * FROM main.PlaylistEntity
		WHERE listId IN (SELECT sourceId FROM CraylistsToPack)
	) AS sourceEntities
	LEFT JOIN CraylistsToPack
		ON sourceEntities.listId = CraylistsToPack.sourceId
	LEFT JOIN dst.PlaylistEntity craylistEntities
		ON craylistEntities.listId = newListId
		AND (sourceEntities.trackId, sourceEntities.databaseUuid)
			= (craylistEntities.trackId, craylistEntities.databaseUuid)
),
targetEntitiesWithPos AS (
	SELECT *, 1 AS pos FROM targetEntities WHERE sourceNextId = 0
	UNION ALL
	SELECT targetEntities.*, pos+1 AS pos
	FROM targetEntities
	INNER JOIN targetEntitiesWithPos
		ON targetEntitiesWithPos.sourceId = targetEntities.sourceNextId
),
newEntities AS (
	SELECT
		IFNULL(id, newId + COALESCE((SELECT max(id) FROM dst.PlaylistEntity), 0)) AS id,
		newListId, trackId, databaseUuid, sourceMR, targetMR, pos
	FROM (
		SELECT
			row_number() OVER (PARTITION BY targetEntitiesWithPos.id) AS newId,
			targetEntitiesWithPos.*
		FROM targetEntitiesWithPos
	)
)
INSERT INTO CraylistEntitiesToPack(id, listId, trackId, databaseUuid, nextEntityId, membershipReference)
SELECT
	id, newListId AS listId, trackId, databaseUuid,
	LEAD(id, 1, 0) OVER (PARTITION BY newListId ORDER BY pos DESC) AS nextEntityId,
	CASE
		WHEN targetMR IS NULL THEN sourceMR
		WHEN targetMR = 0 THEN 0
		ELSE sourceMR
	END AS membershipReference
FROM newEntities`)

		if _, err := src.Exec(`
INSERT INTO dst.PlaylistEntity (id, listId, trackId, databaseUuid, nextEntityId, membershipReference)
SELECT id, listId, trackId, databaseUuid, nextEntityId, membershipReference
FROM CraylistEntitiesToPack
WHERE TRUE
ON CONFLICT(id) DO UPDATE SET
	listId = excluded.listId,
	trackId = excluded.trackId,
	databaseUuid = excluded.databaseUuid,
	nextEntityId = excluded.nextEntityId,
	membershipReference = excluded.membershipReference`); err != nil {
			t.Fatalf("stock export pass %d failed: %v", pass+1, err)
		}

		if err := src.QueryRow("SELECT count(*) FROM dst.PlaylistEntity WHERE listId=1").Scan(&sizes[pass]); err != nil {
			t.Fatal(err)
		}
	}
	return sizes
}

// assertStockPackExportSucceeds replays two full restaged Engine 5.3.0
// CraylistEntitiesToPack exports from src into the same target database and
// verifies both passes produce exactly wantRows rows without repeated-pack growth.
func assertStockPackExportSucceeds(t *testing.T, src *sql.DB, listID int64, wantRows int) {
	t.Helper()
	sizes := runTwoStockPackExports(t, src, listID)
	if sizes[0] != wantRows || sizes[1] != wantRows {
		t.Fatalf("stock restaged exports produced %v rows, want [%d %d]", sizes, wantRows, wantRows)
	}
}
