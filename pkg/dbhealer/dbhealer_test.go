package dbhealer

import (
	"bytes"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
func newLibrary(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	schema, err := os.ReadFile("testdata/engine-5.0.4.sql")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", ""))
	mustExec(t, db, "INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('test-library',3,0,2)")
	mustExec(t, db, "PRAGMA foreign_keys=ON")
	t.Cleanup(func() { db.Close() })
	return db, path
}
func track(t *testing.T, db *sql.DB, id int) {
	t.Helper()
	mustExec(t, db, "INSERT INTO Track(id,path,isAnalyzed) VALUES(?,?,1)", id, fmt.Sprintf("Music/%d.mp3", id))
}
func rawTrack(length int64) []byte {
	b := make([]byte, 44)
	putBEFloat(b[:8], 44100)
	binary.BigEndian.PutUint64(b[8:16], uint64(length))
	return EncodeQCompress(b)
}
func rawBeats(adjusted []marker) []byte {
	raw := make([]byte, 17)
	putBEFloat(raw[:8], 44100)
	putBEFloat(raw[8:16], 1e7)
	raw[16] = 1
	for _, markers := range [][]marker{{{0, 0}, {2205000, 100}}, adjusted} {
		var count [8]byte
		binary.BigEndian.PutUint64(count[:], uint64(len(markers)))
		raw = append(raw, count[:]...)
		for _, m := range markers {
			b := make([]byte, 24)
			putLEFloat(b[:8], m.sample)
			binary.LittleEndian.PutUint64(b[8:16], uint64(int64(m.beat)))
			raw = append(raw, b...)
		}
	}
	return EncodeQCompress(append(raw, 0, 0, 0))
}
func rawLoop(start, end float64) []byte {
	b := []byte{1, 0, 0, 0, 0, 0, 0, 0, 4, 'L', 'o', 'o', 'p'}
	slot := make([]byte, 22)
	putLEFloat(slot[:8], start)
	putLEFloat(slot[8:16], end)
	slot[16] = 1
	slot[17] = 1
	copy(slot[18:], []byte{255, 23, 45, 67})
	return append(b, slot...)
}
func chainIDs(t *testing.T, db *sql.DB, table, groupCol, nextCol string, group int64) []int64 {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("SELECT id,COALESCE(%s,0) FROM %s WHERE COALESCE(%s,0)=?", nextCol, table, groupCol), group)
	if err != nil {
		t.Fatal(err)
	}
	links := map[int64]int64{}
	in := map[int64]bool{}
	for rows.Next() {
		var id, next int64
		if err = rows.Scan(&id, &next); err != nil {
			t.Fatal(err)
		}
		links[id] = next
		if next != 0 {
			in[next] = true
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var heads []int64
	for id := range links {
		if !in[id] {
			heads = append(heads, id)
		}
	}
	if len(heads) != 1 {
		t.Fatalf("heads=%v links=%v", heads, links)
	}
	var out []int64
	id := heads[0]
	for id != 0 {
		next, ok := links[id]
		if !ok {
			t.Fatalf("dangling %d", id)
		}
		out = append(out, id)
		if len(out) > len(links) {
			t.Fatal("cycle")
		}
		id = next
	}
	if len(out) != len(links) {
		t.Fatalf("orphan nodes: %v vs %v", out, links)
	}
	return out
}
func TestEOS01PlaylistEntities(t *testing.T) {
	db, _ := newLibrary(t)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'Set',0,0)")
	for _, n := range []struct{ id, next int }{{10, 30}, {30, 999}, {20, 0}, {40, 40}} {
		mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES(?,1,0,?)", n.id, n.next)
	}
	if err := HealPlaylistEntities(db); err != nil {
		t.Fatal(err)
	}
	got := chainIDs(t, db, "PlaylistEntity", "listId", "nextEntityId", 1)
	if fmt.Sprint(got) != "[10 30 20 40]" {
		t.Fatal(got)
	}
	// The formerly skipped trackId=0 row now splices its predecessor correctly.
	mustExec(t, db, "DELETE FROM PlaylistEntity WHERE id=30")
	if got := chainIDs(t, db, "PlaylistEntity", "listId", "nextEntityId", 1); fmt.Sprint(got) != "[10 20 40]" {
		t.Fatal(got)
	}
	if err := HealPlaylistEntities(db); err != nil {
		t.Fatal(err)
	}
}
func TestEOS01SiblingCyclesParentsAndCache(t *testing.T) {
	db, _ := newLibrary(t)
	// Unique constraint remains enabled throughout repair.
	for _, n := range []struct {
		id, parent, next int
		title            string
	}{{1, 0, 2, "One"}, {2, 0, 1, "Two"}, {3, 0, 999, "Three"}, {4, 1, 0, "Child"}, {5, 6, 0, "Cycle A"}, {6, 5, 0, "Cycle B"}, {7, 999, 0, "One"}} {
		mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(?,?,?,?)", n.id, n.title, n.parent, n.next)
	}
	if err := HealPlaylistTree(db); err != nil {
		t.Fatal(err)
	}
	chainIDs(t, db, "Playlist", "parentListId", "nextListId", 0)
	var path string
	if err := db.QueryRow("SELECT path FROM MaterializedPlaylistPath WHERE id=4").Scan(&path); err != nil || path != "One;Child;" {
		t.Fatalf("%q %v", path, err)
	}
	var count int
	db.QueryRow("SELECT count(*) FROM MaterializedPlaylistPath").Scan(&count)
	if count != 7 {
		t.Fatal(count)
	}
	mustExec(t, db, "UPDATE Playlist SET title='Renamed' WHERE id=1")
	var dirty int
	db.QueryRow("SELECT dirty FROM GuardianCacheState").Scan(&dirty)
	if dirty != 1 {
		t.Fatal("cache not invalidated")
	}
	if err := HealPlaylistTree(db); err != nil {
		t.Fatal(err)
	}
	db.QueryRow("SELECT path FROM MaterializedPlaylistPath WHERE id=4").Scan(&path)
	if path != "Renamed;Child;" {
		t.Fatal(path)
	}
}
func TestEOS03CorruptWaveformsPreserveCuesAndBeatData(t *testing.T) {
	for _, kind := range []string{"truncated", "zero", "wrong-prefix", "bad-bins", "bad-checksum", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			db, _ := newLibrary(t)
			track(t, db, 1)
			cues := EncodeQCompress([]byte("user cue bytes"))
			beat := rawBeats(nil)
			bad := []byte{0, 0, 0, 30, 120}
			switch kind {
			case "zero":
				bad = EncodeQCompress(nil)
			case "wrong-prefix":
				bad = placeholder(1e7)
				binary.BigEndian.PutUint32(bad[:4], 3098)
			case "bad-bins":
				raw, _ := DecodeQCompress(placeholder(1e7))
				raw[7] = 1
				bad = EncodeQCompress(raw)
			case "bad-checksum":
				bad = placeholder(1e7)
				bad[len(bad)-1] ^= 255
			case "trailing":
				bad = append(placeholder(1e7), 0)
			}
			mustExec(t, db, "UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=? WHERE trackId=1", rawTrack(1e7), bad, beat, cues)
			r, err := Heal(db)
			if err != nil {
				t.Fatal(err)
			}
			if r.Waveforms != 1 {
				t.Fatal(r)
			}
			var got, c, b []byte
			db.QueryRow("SELECT overviewWaveFormData,quickCues,beatData FROM PerformanceData WHERE trackId=1").Scan(&got, &c, &b)
			raw, err := DecodeQCompress(got)
			if err != nil || !validOverview(raw) {
				t.Fatalf("invalid replacement %v", err)
			}
			if !bytes.Equal(c, cues) || !bytes.Equal(b, beat) {
				t.Fatal("cue/beat data changed")
			}
			var queue int
			db.QueryRow("SELECT count(*) FROM GuardianReanalysis WHERE field='overviewWaveFormData'").Scan(&queue)
			if queue != 1 {
				t.Fatal(queue)
			}
			r, err = Heal(db)
			if err != nil || r.Waveforms != 0 {
				t.Fatalf("not idempotent: %+v %v", r, err)
			}
		})
	}
}
func TestQCompressRejectsBombAndTrackBeatCorruption(t *testing.T) {
	if _, err := DecodeQCompress([]byte{255, 255, 255, 255, 120}); err == nil {
		t.Fatal("unbounded size accepted")
	}
	db, _ := newLibrary(t)
	track(t, db, 1)
	bad := []byte("invalid track/beat")
	cues := []byte("keep cues")
	loops := rawLoop(100, 200)
	mustExec(t, db, "UPDATE PerformanceData SET trackData=?,beatData=?,quickCues=?,loops=? WHERE trackId=1", bad, bad, cues, loops)
	if _, err := Heal(db); err != nil {
		t.Fatal(err)
	}
	var gotTrack, gotBeat, gotCues, gotLoops []byte
	db.QueryRow("SELECT trackData,beatData,quickCues,loops FROM PerformanceData").Scan(&gotTrack, &gotBeat, &gotCues, &gotLoops)
	if !bytes.Equal(gotTrack, bad) || !bytes.Equal(gotBeat, bad) || !bytes.Equal(gotCues, cues) || !bytes.Equal(gotLoops, loops) {
		t.Fatal("unrelated blobs changed")
	}
	var count int
	db.QueryRow("SELECT count(*) FROM GuardianReanalysis WHERE field IN ('trackData','beatData')").Scan(&count)
	if count != 2 {
		t.Fatal(count)
	}
}
func TestEOS07LoopSampleAlignment(t *testing.T) {
	for _, beats := range []float64{8, 3, 0.5, 1.0 / 128} {
		t.Run(fmt.Sprint(beats), func(t *testing.T) {
			db, _ := newLibrary(t)
			track(t, db, 1)
			markers := []marker{{0, 0}, {22123.456789 * 100, 100}}
			beat := rawBeats(markers)
			start := 22123.456789 * 4
			end := start + beats*22123.456789
			loops := rawLoop(start+0.4, end-0.7)
			mustExec(t, db, "UPDATE PerformanceData SET trackData=?,beatData=?,overviewWaveFormData=?,loops=? WHERE trackId=1", rawTrack(1e7), beat, placeholder(1e7), loops)
			r, err := Heal(db)
			if err != nil {
				t.Fatal(err)
			}
			if r.Loops != 1 {
				t.Fatal(r)
			}
			var got []byte
			db.QueryRow("SELECT loops FROM PerformanceData").Scan(&got)
			if math.Abs(leFloat(got[13:21])-start) > 1e-7 || math.Abs(leFloat(got[21:29])-end) > 1e-7 {
				t.Fatalf("loop samples %v %v", leFloat(got[13:21]), leFloat(got[21:29]))
			}
			if !bytes.Equal(got[:13], loops[:13]) || !bytes.Equal(got[29:], loops[29:]) {
				t.Fatal("loop metadata changed")
			}
			r, err = Heal(db)
			if err != nil || r.Loops != 0 {
				t.Fatalf("not idempotent %+v %v", r, err)
			}
		})
	}
}
func TestLoopVariableTempoAndFreeLoop(t *testing.T) {
	grid, err := parseBeats(rawBeats([]marker{{0, 0}, {176400, 8}, {344400, 16}}))
	if err != nil {
		t.Fatal(err)
	}
	wantStart, wantEnd := grid.sampleAt(6), grid.sampleAt(10)
	got, n, err := alignLoops(rawLoop(wantStart+0.3, wantEnd-0.5), grid)
	if err != nil || n != 1 || math.Abs(leFloat(got[21:29])-wantEnd) > 1e-7 {
		t.Fatalf("%d %v", n, err)
	}
	free := rawLoop(1234, 45678)
	got, n, err = alignLoops(free, grid)
	if err != nil || n != 0 || !bytes.Equal(got, free) {
		t.Fatal("free loop changed")
	}
	if _, _, err = alignLoops(append(rawLoop(0, 22050), 1), grid); err == nil {
		t.Fatal("unknown tail accepted")
	}
}
func TestEOS09ArtworkResizeHashAndOrphan(t *testing.T) {
	db, _ := newLibrary(t)
	track(t, db, 1)
	track(t, db, 2)
	img := image.NewRGBA(image.Rect(0, 0, 1600, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1600; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 42, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	originalHash := fmt.Sprintf("%x", sha1.Sum(buf.Bytes()))
	mustExec(t, db, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,?,?)", originalHash, buf.Bytes())
	mustExec(t, db, "UPDATE Track SET albumArtId=1,albumArtSourceHash=? WHERE id=1", originalHash)
	mustExec(t, db, "PRAGMA foreign_keys=OFF")
	mustExec(t, db, "UPDATE Track SET albumArtId=999 WHERE id=2")
	mustExec(t, db, "PRAGMA foreign_keys=ON")
	r, err := Heal(db)
	if err != nil {
		t.Fatal(err)
	}
	if r.Artwork != 1 || r.OrphanArtwork != 1 {
		t.Fatal(r)
	}
	var blob []byte
	var hash, source string
	db.QueryRow("SELECT albumArt,hash FROM AlbumArt WHERE id=1").Scan(&blob, &hash)
	db.QueryRow("SELECT albumArtSourceHash FROM Track WHERE id=1").Scan(&source)
	thumb, err := jpeg.Decode(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	if thumb.Bounds().Dx() != 512 || thumb.Bounds().Dy() != 256 || len(blob) > 1<<20 {
		t.Fatal(thumb.Bounds())
	}
	if hash != originalHash || source != hash {
		t.Fatal("hashes inconsistent")
	}
	var orphan sql.NullInt64
	db.QueryRow("SELECT albumArtId FROM Track WHERE id=2").Scan(&orphan)
	if orphan.Valid {
		t.Fatal(orphan)
	}
}
func TestHealAtomicRollbackAndUnsupportedSchema(t *testing.T) {
	db, _ := newLibrary(t)
	track(t, db, 1)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'Set',0,0)")
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,nextEntityId) VALUES(1,1,999)")
	mustExec(t, db, "CREATE TRIGGER fail_waveform BEFORE UPDATE OF overviewWaveFormData ON PerformanceData BEGIN SELECT RAISE(ABORT,'injected write failure'); END")
	if _, err := Heal(db); err == nil {
		t.Fatal("expected failure")
	}
	var next int
	db.QueryRow("SELECT nextEntityId FROM PlaylistEntity").Scan(&next)
	if next != 999 {
		t.Fatal("partial repair committed")
	}
	var index int
	db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='index_PlaylistEntity_trackId'").Scan(&index)
	if index != 0 {
		t.Fatal("DDL not rolled back")
	}
	mustExec(t, db, "DROP TRIGGER fail_waveform")
	mustExec(t, db, "UPDATE Information SET schemaVersionMinor=99")
	if _, err := Heal(db); err == nil || !strings.Contains(err.Error(), "unsupported library schema") {
		t.Fatal(err)
	}
}

func TestEOS01NullTailAndPersistedParentCycles(t *testing.T) {
	db, _ := newLibrary(t)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId,isPersisted) VALUES(1,'A',2,NULL,0),(2,'B',1,NULL,0),(3,'C',4,NULL,0),(4,'D',3,NULL,0)")
	// Avoid the stock insert-time ancestry traversal when constructing damage.
	var triggerDDL string
	db.QueryRow("SELECT sql FROM sqlite_master WHERE name='trigger_after_update_isPersistParent'").Scan(&triggerDDL)
	mustExec(t, db, "DROP TRIGGER trigger_after_update_isPersistParent")
	mustExec(t, db, "UPDATE Playlist SET isPersisted=1")
	mustExec(t, db, triggerDDL)
	if err := HealPlaylistTree(db); err != nil {
		t.Fatal(err)
	}
	var nullTails int
	db.QueryRow("SELECT count(*) FROM Playlist WHERE nextListId IS NULL").Scan(&nullTails)
	if nullTails != 0 {
		t.Fatal("NULL tails not normalized")
	}
	mustExec(t, db, "INSERT INTO PlaylistEntity(id,listId,nextEntityId) VALUES(1,1,NULL)")
	if err := HealPlaylistEntities(db); err != nil {
		t.Fatal(err)
	}
	var next sql.NullInt64
	db.QueryRow("SELECT nextEntityId FROM PlaylistEntity").Scan(&next)
	if !next.Valid || next.Int64 != 0 {
		t.Fatal(next)
	}
	var trigger int
	db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='trigger_after_update_isPersistParent'").Scan(&trigger)
	if trigger != 1 {
		t.Fatal("stock trigger not restored")
	}
}

func TestCachedPlaylistPathAndImmediateEditFallback(t *testing.T) {
	db, _ := newLibrary(t)
	mustExec(t, db, "INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'Set',0,0),(2,'Child',1,0)")
	if err := HealPlaylistTree(db); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := db.QueryRow("SELECT path FROM PlaylistPath WHERE id=2").Scan(&path); err != nil || path != "Set;Child;" {
		t.Fatalf("%s %v", path, err)
	}
	mustExec(t, db, "UPDATE Playlist SET title='Edited' WHERE id=1")
	if err := db.QueryRow("SELECT path FROM PlaylistPath WHERE id=2").Scan(&path); err != nil || path != "Edited;Child;" {
		t.Fatalf("stale fallback %s %v", path, err)
	}
	if err := HealPlaylistTree(db); err != nil {
		t.Fatal(err)
	}
	// The expensive stock branch must not be evaluated with a clean cache.
	mustExec(t, db, "DROP VIEW GuardianLivePlaylistPath; CREATE VIEW GuardianLivePlaylistPath AS SELECT id,path,abs(-9223372036854775808) AS position FROM MaterializedPlaylistPath")
	var position int
	if err := db.QueryRow("SELECT position FROM PlaylistPath WHERE id=2").Scan(&position); err != nil {
		t.Fatalf("clean cache evaluated stock branch: %v", err)
	}
}

func TestReanalysisQueueClearsOnlyAfterActualWaveformReplacement(t *testing.T) {
	db, _ := newLibrary(t)
	track(t, db, 1)
	r, err := Heal(db)
	if err != nil || r.ReanalysisPending != 3 {
		t.Fatalf("%+v %v", r, err)
	}
	mustExec(t, db, "UPDATE PerformanceData SET trackData=?,beatData=? WHERE trackId=1", rawTrack(1e7), rawBeats(nil))
	r, err = Heal(db)
	if err != nil || r.ReanalysisPending != 1 {
		t.Fatalf("placeholder lost its request: %+v %v", r, err)
	}
	raw, _ := DecodeQCompress(placeholder(1e7))
	raw[24] = 42
	mustExec(t, db, "UPDATE PerformanceData SET overviewWaveFormData=? WHERE trackId=1", EncodeQCompress(raw))
	r, err = Heal(db)
	if err != nil || r.ReanalysisPending != 0 {
		t.Fatalf("completed request retained: %+v %v", r, err)
	}
}

func TestEOS02IndexesAndTuning(t *testing.T) {
	db, _ := newLibrary(t)
	if _, err := Heal(db); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index_PlaylistEntity_listId_nextEntityId", "index_PlaylistEntity_trackId"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, p := range []struct {
		name string
		want int
	}{{"synchronous", 1}, {"mmap_size", 134217728}, {"temp_store", 2}} {
		var value int
		if err := db.QueryRow("PRAGMA " + p.name).Scan(&value); err != nil || value != p.want {
			t.Fatalf("%s: %d %v", p.name, value, err)
		}
	}
}

func TestPlaylistPathCachePreservesVendorPositions(t *testing.T) {
	db, _ := newLibrary(t)
	mustExec(t, db, `INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'A',0,0),(2,'B',0,0),(3,'C',1,0),(4,'D',1,0),(5,'E',3,0),(6,'F',2,0),(7,'Z',0,0),(8,NULL,0,0),(9,'Child',8,0),(10,NULL,9,0)`)
	type entry struct {
		id, position int
		path         sql.NullString
	}
	read := func(view string) []entry {
		t.Helper()
		rows, err := db.Query("SELECT id,path,position FROM " + view + " ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []entry
		for rows.Next() {
			var e entry
			if err = rows.Scan(&e.id, &e.path, &e.position); err != nil {
				t.Fatal(err)
			}
			result = append(result, e)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	expected := read("PlaylistPath")
	if err := HealPlaylistTree(db); err != nil {
		t.Fatal(err)
	}
	if got := read("PlaylistPath"); !reflect.DeepEqual(got, expected) {
		t.Fatalf("cache changed vendor positions: got %v want %v", got, expected)
	}
	mustExec(t, db, `UPDATE Playlist SET title='Edited' WHERE id=1`)
	expected = read("GuardianOriginalPlaylistPath")
	if got := read("PlaylistPath"); !reflect.DeepEqual(got, expected) {
		t.Fatalf("dirty fallback changed vendor positions: got %v want %v", got, expected)
	}
	if err := HealPlaylistTree(db); err != nil {
		t.Fatal(err)
	}
	if got := read("PlaylistPath"); !reflect.DeepEqual(got, expected) {
		t.Fatalf("refreshed cache changed vendor positions: got %v want %v", got, expected)
	}
	// A native edit may temporarily leave a NULL tail before the next healer pass.
	mustExec(t, db, `UPDATE Playlist SET nextListId=NULL WHERE id=8`)
	expected = read("GuardianOriginalPlaylistPath")
	if got := read("PlaylistPath"); !reflect.DeepEqual(got, expected) {
		t.Fatalf("NULL-tail fallback changed vendor positions: got %v want %v", got, expected)
	}
	if err := HealPlaylistTree(db); err != nil {
		t.Fatal(err)
	}
	expected = read("GuardianOriginalPlaylistPath")
	if got := read("PlaylistPath"); !reflect.DeepEqual(got, expected) {
		t.Fatalf("NULL-tail rebuild changed vendor positions: got %v want %v", got, expected)
	}
	// Upgrade a previously installed cache and view even when its dirty flag is clean.
	mustExec(t, db, `DROP VIEW GuardianLivePlaylistPath; CREATE VIEW GuardianLivePlaylistPath AS SELECT id,path,position FROM MaterializedPlaylistPath; UPDATE MaterializedPlaylistPath SET position=position+100`)
	if err := HealPlaylistTree(db); err != nil {
		t.Fatal(err)
	}
	if got := read("PlaylistPath"); !reflect.DeepEqual(got, expected) {
		t.Fatalf("legacy cache was not upgraded: got %v want %v", got, expected)
	}
}

func TestPlaylistPathCacheMatchesVendorAcrossTrees(t *testing.T) {
	for seed := int64(0); seed < 50; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			db, _ := newLibrary(t)
			rng := rand.New(rand.NewSource(seed))
			ids := rng.Perm(24)
			for i, id := range ids {
				parent := 0
				if i > 0 && rng.Intn(3) != 0 {
					parent = ids[rng.Intn(i)] + 1
				}
				var title any = fmt.Sprintf("T%d", id)
				if rng.Intn(7) == 0 {
					title = nil
				}
				mustExec(t, db, `INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(?,?,?,0)`, id+1, title, parent)
			}
			type entry struct {
				id, position int
				path         sql.NullString
			}
			read := func(view string) []entry {
				t.Helper()
				rows, err := db.Query("SELECT id,path,position FROM " + view + " ORDER BY id")
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var result []entry
				for rows.Next() {
					var e entry
					if err = rows.Scan(&e.id, &e.path, &e.position); err != nil {
						t.Fatal(err)
					}
					result = append(result, e)
				}
				if err = rows.Err(); err != nil {
					t.Fatal(err)
				}
				return result
			}
			expected := read("PlaylistPath")
			if err := HealPlaylistTree(db); err != nil {
				t.Fatal(err)
			}
			if got := read("PlaylistPath"); !reflect.DeepEqual(got, expected) {
				t.Fatalf("clean cache differs: got %v want %v", got, expected)
			}
			mustExec(t, db, `UPDATE Playlist SET nextListId=NULL WHERE parentListId=0 AND nextListId=0`)
			expected = read("GuardianOriginalPlaylistPath")
			if got := read("PlaylistPath"); !reflect.DeepEqual(got, expected) {
				t.Fatalf("dirty fallback differs: got %v want %v", got, expected)
			}
			if err := HealPlaylistTree(db); err != nil {
				t.Fatal(err)
			}
			expected = read("GuardianOriginalPlaylistPath")
			if got := read("PlaylistPath"); !reflect.DeepEqual(got, expected) {
				t.Fatalf("refreshed cache differs: got %v want %v", got, expected)
			}
		})
	}
}

// A data-only pass must leave sqlite_master untouched, and Engine's m.db with it.
func TestHealDataOnlyLeavesSchemaUntouched(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	schema, err := os.ReadFile("testdata/engine-5.0.4.sql")
	if err != nil {
		t.Fatal(err)
	}
	ddl := strings.Replace(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", "", 1)
	if _, err = db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('t',3,0,2)`); err != nil {
		t.Fatal(err)
	}
	dump := func() string {
		var s string
		if err := db.QueryRow(`SELECT group_concat(type||name||COALESCE(sql,''),char(10)) FROM (SELECT * FROM main.sqlite_master ORDER BY type,name)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := dump()
	if _, err = heal(db, true); err != nil {
		t.Fatal(err)
	}
	if after := dump(); after != before {
		t.Fatalf("schema changed:\n%s", after)
	}
}
