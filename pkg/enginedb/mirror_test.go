package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

// mirrorLive is a stick's library of n tracks in two lists, as a window builds it.
func mirrorLive(t *testing.T, n int) (dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := make([]TrackRow, n)
	for i := range rows {
		rel := fmt.Sprintf("Contents/a/%03d.mp3", i+1)
		rows[i] = TrackRow{Rel: rel, Track: rekordbox.Track{ID: uint32(i + 1), Path: "/" + rel, Title: fmt.Sprintf("T%d", i+1), BPM: 120, Duration: 200}}
	}
	_, _, _, err := BuildLibrary(context.Background(), dir, "", rows, func(b *Batch, ids []int64) error {
		for _, title := range []string{"One", "Two"} {
			id, err := b.InsertList(context.Background(), ListRow{Title: title})
			if err == nil {
				err = b.SetEntries(context.Background(), id, ids[:n/2])
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mirrorExec(t *testing.T, file string, queries ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range queries {
		if _, err = db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// mirrorDump is every row of every table and the schema, the ID counters aside.
func mirrorDump(t *testing.T, file string) string {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out []string
	one := func(q string) (rows []string) {
		r, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer r.Close()
		for r.Next() {
			var s string
			if err = r.Scan(&s); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, s)
		}
		return rows
	}
	out = append(out, one(`SELECT type||' '||name||' '||ifnull(sql,'') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`)...)
	for _, table := range one(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`) {
		cols := one(`SELECT 'quote("'||name||'")' FROM pragma_table_info('` + table + `') ORDER BY cid`)
		for _, row := range one(`SELECT ` + strings.Join(cols, `||'|'||`) + ` FROM "` + table + `" ORDER BY 1`) {
			out = append(out, table+": "+row)
		}
	}
	out = append(out, one(`PRAGMA integrity_check`)...)
	return strings.Join(out, "\n")
}

// The next library is the stick's library row for row, made without a write to it; later it
// takes exactly the rows Engine wrote since, and keeps what the bridge wrote on the others.
func TestMirrorLibraryTakesEveryRowOnceAndThenWhatEngineWrote(t *testing.T) {
	ctx := context.Background()
	dir := mirrorLive(t, 40)
	live, next := filepath.Join(dir, "m.db"), filepath.Join(dir, ".rb-next.db")
	mirrorExec(t, live, `INSERT INTO AlbumArt(hash,albumArt) VALUES(x'01',x'ffd8')`, `UPDATE Track SET albumArtId=1 WHERE id=7`,
		`UPDATE PerformanceData SET overviewWaveFormData=randomblob(40000), beatData=x'0102' WHERE trackId<=40`,
		`UPDATE Track SET lastEditTime=1700000000`) // written long ago
	before, _ := os.ReadFile(live)
	state := LibraryState(live)
	var m Mirror
	paced := 0
	ch, err := MirrorLibrary(ctx, live, next, &m, func(time.Duration) error { paced++; return nil })
	if err != nil || len(ch.Tracks) != 40 || m.State != state || len(m.Tracks) != 40 || paced < 10 || m.Rows != ch.Rows || m.Rows < 80 {
		t.Fatalf("first mirror: %d tracks, state %q want %q, %v", len(ch.Tracks), m.State, state, err)
	}
	if after, _ := os.ReadFile(live); string(after) != string(before) {
		t.Fatal("the stick's library was written")
	}
	if got, want := mirrorDump(t, next), mirrorDump(t, live); got != want {
		t.Fatalf("the next library is not the stick's:\n%s\nwant:\n%s", got, want)
	}
	// With nothing written on the stick since, a second pass writes nothing: no trigger is dropped
	// and made again (each a commit on the stick, in the window that swaps the library in).
	file, _ := os.ReadFile(next)
	if ch, err = MirrorLibrary(ctx, live, next, &m, nil); err != nil || len(ch.Tracks) != 0 || ch.Lists {
		t.Fatalf("a pass with nothing to take: %+v %v", ch, err)
	}
	if again, _ := os.ReadFile(next); !bytes.Equal(again, file) {
		t.Fatal("a pass with nothing to take wrote the next library")
	}
	// What Engine writes at every mount of a stick, the played indicator, is taken without the
	// triggers leaving the file.
	mirrorExec(t, live, `UPDATE Information SET currentPlayedIndiciator=4711`)
	if ch, err = MirrorLibrary(ctx, live, next, &m, nil); err != nil || len(ch.Tracks) != 0 || ch.Lists {
		t.Fatalf("a pass after a mount by Engine: %+v %v", ch, err)
	}
	db, err := sql.Open("sqlite", "file:"+next+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var indicator, triggers, schema int64
	err = db.QueryRow(`SELECT (SELECT currentPlayedIndiciator FROM Information),(SELECT count(*) FROM sqlite_master WHERE type='trigger'),(SELECT schema_version FROM pragma_schema_version)`).Scan(&indicator, &triggers, &schema)
	db.Close()
	if err != nil || indicator != 4711 || triggers < 10 {
		t.Fatalf("the next library after a mount by Engine: indicator %d, %d triggers, %v", indicator, triggers, err)
	}
	mirrorExec(t, live, `UPDATE Information SET currentPlayedIndiciator=4712`)
	if _, err = MirrorLibrary(ctx, live, next, &m, nil); err != nil {
		t.Fatal(err)
	}
	if db, err = sql.Open("sqlite", "file:"+next+"?mode=ro"); err != nil {
		t.Fatal(err)
	}
	var schema2 int64
	err = db.QueryRow(`SELECT schema_version FROM pragma_schema_version`).Scan(&schema2)
	db.Close()
	if err != nil || schema2 != schema {
		t.Fatalf("taking the played indicator changed the next library's schema (%d, then %d): its triggers were dropped and made again; %v", schema, schema2, err)
	}
	// The bridge's rows in the next library get IDs Engine does not give meanwhile.
	mirrorExec(t, next, `INSERT INTO Track(path,filename) VALUES('../Contents/new.mp3','new.mp3')`,
		`UPDATE PerformanceData SET trackData=x'aa' WHERE trackId IN (3,5)`)
	// Engine, meanwhile: a played track, a hot cue (its trigger moves lastEditTime), a prepared
	// track, a removed track, a track and a list of its own, a cover.
	mirrorExec(t, live, `UPDATE Track SET isPlayed=1,timeLastPlayed=1700000000 WHERE id=2`,
		`UPDATE PerformanceData SET quickCues=x'c0ffee' WHERE trackId=3`,
		`INSERT INTO PreparelistEntity(trackId,trackNumber) VALUES(9,1)`,
		`DELETE FROM PlaylistEntity WHERE trackId=11`, `DELETE FROM Track WHERE id=11`,
		`INSERT INTO Track(path,filename) VALUES('../Contents/deck.mp3','deck.mp3')`,
		`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES('Deck Set',0,1,0,1)`,
		`INSERT INTO AlbumArt(hash,albumArt) VALUES(x'02',x'ffd9')`)
	if LibraryState(live) == state {
		t.Fatal("LibraryState did not move with Engine's commits")
	}
	ch, err = MirrorLibrary(ctx, live, next, &m, nil)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(ch.Tracks)
	if fmt.Sprint(ch.Tracks) != "[2 3 11 41]" || !ch.Lists {
		t.Fatalf("taken: tracks %v lists %v, want the played, the cued, the removed and Engine's own, and the lists", ch.Tracks, ch.Lists)
	}
	db, err = sql.Open("sqlite", next)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	err = db.QueryRow(`SELECT (SELECT isPlayed FROM Track WHERE id=2)||' '||(SELECT hex(quickCues)||hex(ifnull(trackData,x'')) FROM PerformanceData WHERE trackId=3)||' '||
		(SELECT hex(trackData) FROM PerformanceData WHERE trackId=5)||' '||(SELECT count(*) FROM Track WHERE id=11)||' '||(SELECT count(*) FROM PlaylistEntity WHERE trackId=11)||' '||
		(SELECT path FROM Track WHERE id=41)||' '||(SELECT count(*) FROM Playlist WHERE title='Deck Set')||' '||(SELECT count(*) FROM PreparelistEntity)||' '||
		(SELECT count(*) FROM AlbumArt)||' '||(SELECT min(id)>1000 FROM Track WHERE filename='new.mp3')||' '||(SELECT count(*) FROM sqlite_master WHERE type='trigger')`).Scan(&got)
	if want := "1 C0FFEE AA 0 0 ../Contents/deck.mp3 1 1 2 1 16"; err != nil || got != want {
		t.Fatalf("next library after Engine's writes: %q, want %q (%v)", got, want, err)
	}
	// Engine's lastEditTime is in whole seconds: a second write of a row in the second it was
	// taken does not move it, and is taken all the same.
	mirrorExec(t, live, `UPDATE PerformanceData SET quickCues=x'c0ffee02' WHERE trackId=3`)
	if ch, err = MirrorLibrary(ctx, live, next, &m, nil); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT hex(quickCues) FROM PerformanceData WHERE trackId=3`).Scan(&got); err != nil || got != "C0FFEE02" {
		t.Fatalf("a row written twice in one second: the next library holds %s (%v)", got, err)
	}
	mirrorExec(t, live, `UPDATE Track SET lastEditTime=1700000000`)
	if _, err = MirrorLibrary(ctx, live, next, &m, nil); err != nil {
		t.Fatal(err)
	}
	if ch, err = MirrorLibrary(ctx, live, next, &m, nil); err != nil || len(ch.Tracks) != 0 || ch.Lists {
		t.Fatalf("a look at an unchanged library takes nothing: %+v, %v", ch, err)
	}
}

// A file Engine gave a row while the bridge gave it one in the next library cannot be both: the
// caller makes the next library again.
func TestMirrorLibraryRefusesARowBothAdded(t *testing.T) {
	ctx := context.Background()
	dir := mirrorLive(t, 4)
	live, next := filepath.Join(dir, "m.db"), filepath.Join(dir, ".rb-next.db")
	var m Mirror
	if _, err := MirrorLibrary(ctx, live, next, &m, nil); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{live, next} {
		mirrorExec(t, file, `INSERT INTO Track(path,filename) VALUES('../Contents/same.mp3','same.mp3')`)
	}
	if _, err := MirrorLibrary(ctx, live, next, &m, nil); !errors.Is(err, ErrMirrorConflict) {
		t.Fatalf("error %v, want ErrMirrorConflict", err)
	}
}

// A new next library has its Track rows together and in order, whatever their waveforms weigh: a
// scan of them (the grid fix at every insert, Engine's own) is then a few reads of a USB 2.0 stick.
// Copied between each other's waveforms they were a read a page, and the insert of an unchanged
// 5,962-track stick took 6.8 s instead of 0.7 s.
func TestMirrorLibraryLaysTrackRowsTogether(t *testing.T) {
	dir := mirrorLive(t, 300)
	live, next := filepath.Join(dir, "m.db"), filepath.Join(dir, ".rb-next.db")
	mirrorExec(t, live, `UPDATE PerformanceData SET overviewWaveFormData=randomblob(20000)`)
	var m Mirror
	if _, err := MirrorLibrary(context.Background(), live, next, &m, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", next)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var pages, span, rows, waves int
	if err = db.QueryRow(`SELECT count(*),max(pageno)-min(pageno)+1,(SELECT count(*) FROM Track),
		(SELECT count(*) FROM PerformanceData WHERE length(overviewWaveFormData)=20000) FROM dbstat WHERE name='Track' AND pagetype='leaf'`).Scan(&pages, &span, &rows, &waves); err != nil {
		t.Fatal(err)
	}
	if rows != 300 || waves != 300 {
		t.Fatalf("the next library has %d tracks, %d with their waveform; want 300 and 300", rows, waves)
	}
	if pages < 4 || span > 2*pages { // between them: the pages of the table's own UNIQUE index, filled with it
		t.Fatalf("the Track table's %d leaf pages lie over %d pages of the file; want them together", pages, span)
	}
}

// Finding 4.2: MirrorLibrary must abort before writing or syncing when pace signals a busy deck,
// and remove any incomplete fresh .rb-next.db so dirty page-cache pages are discarded without fsync.
func TestMirrorLibraryAbortsBeforeSQLAndCleansFreshOnBusyPace(t *testing.T) {
	dir := mirrorLive(t, 40)
	live, next := filepath.Join(dir, "m.db"), filepath.Join(dir, ".rb-next.db")
	errBusy := errors.New("deck active")

	var m Mirror
	_, err := MirrorLibrary(context.Background(), live, next, &m, func(time.Duration) error {
		return errBusy
	})
	if !errors.Is(err, errBusy) {
		t.Fatalf("immediate busy pace: err=%v, want %v", err, errBusy)
	}
	if _, statErr := os.Stat(next); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("fresh .rb-next.db left behind after immediate busy abort: statErr=%v", statErr)
	}

	// Allow two pace checks, then simulate playback starting mid-mirror.
	var m2 Mirror
	calls := 0
	_, err = MirrorLibrary(context.Background(), live, next, &m2, func(time.Duration) error {
		calls++
		if calls > 2 {
			return errBusy
		}
		return nil
	})
	if !errors.Is(err, errBusy) {
		t.Fatalf("mid-mirror busy pace: err=%v, want %v", err, errBusy)
	}
	if _, statErr := os.Stat(next); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial fresh .rb-next.db left behind after mid-mirror busy abort: statErr=%v", statErr)
	}
}

// Finding 8: background fresh-mirror fsync step is raised to 16 MiB so a 4-6 MiB database does not
// execute redundant mid-mirror fsync barriers while still bounding dirty cache on large libraries.
func TestFix3_Finding8_MirrorFlushStep16MiB(t *testing.T) {
	if mirrorFlushBytes < 8<<20 || mirrorFlushBytes > 16<<20 {
		t.Fatalf("mirrorFlushBytes = %d, want in [8 MiB, 16 MiB]", mirrorFlushBytes)
	}
	dir := mirrorLive(t, 220)
	live, next := filepath.Join(dir, "m.db"), filepath.Join(dir, ".rb-next.db")
	// 220 tracks * 20 KiB ~= 4.4 MiB: >2 MiB (old step would fsync >=2 times mid-mirror) and <16 MiB (0 mid-mirror flushes).
	mirrorExec(t, live, `UPDATE PerformanceData SET overviewWaveFormData=randomblob(20480)`)

	midFlushes := 0
	orig := syncMirrorFile
	syncMirrorFile = func(p string) error {
		midFlushes++
		return orig(p)
	}
	defer func() { syncMirrorFile = orig }()

	var m Mirror
	if _, err := MirrorLibrary(context.Background(), live, next, &m, nil); err != nil {
		t.Fatalf("MirrorLibrary: %v", err)
	}
	st, err := os.Stat(next)
	if err != nil || st.Size() < 3<<20 {
		t.Fatalf("unexpected mirrored size: st=%v err=%v", st, err)
	}
	if midFlushes != 0 {
		t.Fatalf("expected 0 mid-mirror fsyncs for %.1f MiB db at %d MiB step, got %d",
			float64(st.Size())/(1<<20), mirrorFlushBytes>>20, midFlushes)
	}
}

func TestMirrorLibraryRemovesTrackReferencesAndKeepsForeignKeysValid(t *testing.T) {
	dir := mirrorLive(t, 2)
	live, next := filepath.Join(dir, "m.db"), filepath.Join(dir, ".rb-next.db")
	var m Mirror
	if _, err := MirrorLibrary(context.Background(), live, next, &m, nil); err != nil {
		t.Fatal(err)
	}
	mirrorExec(t, next, `INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) SELECT listId,1,'bridge',0,1 FROM PlaylistEntity WHERE trackId=2 LIMIT 1`)
	mirrorExec(t, live, `INSERT INTO PreparelistEntity(trackId,trackNumber) VALUES(1,1)`,
		`DELETE FROM PerformanceData WHERE trackId=1`, `DELETE FROM Track WHERE id=1`)
	if _, err := MirrorLibrary(context.Background(), live, next, &m, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", next)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var violations, refs int
	if err = db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT (SELECT count(*) FROM PlaylistEntity WHERE trackId=1)+(SELECT count(*) FROM PreparelistEntity WHERE trackId=1)`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if violations != 0 || refs != 0 {
		t.Fatalf("foreign_key_check=%d references=%d", violations, refs)
	}
}

func TestMirrorLibraryReloadsPlaylistPairTogether(t *testing.T) {
	dir := mirrorLive(t, 2)
	live, next := filepath.Join(dir, "m.db"), filepath.Join(dir, ".rb-next.db")
	var m Mirror
	if _, err := MirrorLibrary(context.Background(), live, next, &m, nil); err != nil {
		t.Fatal(err)
	}
	mirrorExec(t, next, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES('Bridge',0,1,0,1)`,
		`INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES((SELECT id FROM Playlist WHERE title='Bridge'),1,'bridge',0,1)`)
	mirrorExec(t, live, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES('Deck',0,1,0,1)`)
	ch, err := MirrorLibrary(context.Background(), live, next, &m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Lists {
		t.Fatal("playlist pair change was not reported")
	}
	db, err := sql.Open("sqlite", next)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var violations int
	if err = db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign_key_check=%d", violations)
	}
}

func TestMirrorLibraryHandlesNullAndDuplicateTrackReferences(t *testing.T) {
	dir := mirrorLive(t, 3)
	live, next := filepath.Join(dir, "m.db"), filepath.Join(dir, ".rb-next.db")
	var m Mirror
	if _, err := MirrorLibrary(context.Background(), live, next, &m, nil); err != nil {
		t.Fatal(err)
	}
	// On next, create two bridge playlists:
	//  - List A: track 2 (e1) -> track 1 (e3, higher id) -> track 1 (e2, lower id) -> track 3 (e4)
	//    Deleting track 1 on live must unlink both duplicate references in reverse ID order so e1 points to e4.
	//  - List B: track 1 (e5) with nextEntityId IS NULL (must not fail Scan into int64).
	mirrorExec(t, next,
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES(90001,'DupList',0,1,0,1)`,
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES(90002,'NullNextList',0,1,0,1)`,
		`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(90014,90001,3,'bridge',0,0)`,
		`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(90012,90001,1,'bridge-a',90014,0)`,
		`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(90013,90001,1,'bridge-b',90012,0)`,
		`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(90011,90001,2,'bridge',90013,0)`,
		`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(90015,90002,1,'bridge',NULL,0)`,
	)
	mirrorExec(t, live, `DELETE FROM PerformanceData WHERE trackId=1`, `DELETE FROM Track WHERE id=1`)
	ch, err := MirrorLibrary(context.Background(), live, next, &m, nil)
	if err != nil {
		t.Fatalf("MirrorLibrary failed on NULL/duplicate references: %v", err)
	}
	if !ch.Lists {
		t.Fatal("expected ch.Lists=true after removing PlaylistEntity references")
	}
	db, err := sql.Open("sqlite", next)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var violations, refs int
	if err = db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM PlaylistEntity WHERE trackId=1`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if violations != 0 || refs != 0 {
		t.Fatalf("foreign_key_check=%d refs=%d", violations, refs)
	}
	var e1Next int64
	if err = db.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=90011`).Scan(&e1Next); err != nil {
		t.Fatal(err)
	}
	if e1Next != 90014 {
		t.Fatalf("duplicate reverse-ID unlink broke chain: e1.nextEntityId=%d, want 90014", e1Next)
	}
}
