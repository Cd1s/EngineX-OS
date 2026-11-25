package enginedb

import (
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
	if err != nil || len(ch.Tracks) != 40 || m.State != state || len(m.Tracks) != 40 || paced < 10 {
		t.Fatalf("first mirror: %d tracks, state %q want %q, %v", len(ch.Tracks), m.State, state, err)
	}
	if after, _ := os.ReadFile(live); string(after) != string(before) {
		t.Fatal("the stick's library was written")
	}
	if got, want := mirrorDump(t, next), mirrorDump(t, live); got != want {
		t.Fatalf("the next library is not the stick's:\n%s\nwant:\n%s", got, want)
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
	db, err := sql.Open("sqlite", next)
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
