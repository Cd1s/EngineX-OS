package bridge_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/sc5000-custom-os/internal/testfixture"
	"github.com/Cd1s/sc5000-custom-os/pkg/bridge"
	"github.com/Cd1s/sc5000-custom-os/pkg/enginedb"
	"github.com/Cd1s/sc5000-custom-os/pkg/rekordbox"
	"golang.org/x/text/unicode/norm"
)

func snapshot(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = sha256.Sum256(b)
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func dbOpen(t *testing.T, root string) *sql.DB {
	t.Helper()
	dir := filepath.Join(root, "Engine Library/Database2")
	db, e := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	// One connection keeps the attached bridge state visible to every query.
	db.SetMaxOpenConns(1)
	state := filepath.Join(dir, ".rb-bridge-state.db")
	if _, e = os.Stat(state); e == nil {
		if _, e = db.Exec(`ATTACH DATABASE ? AS bridge`, state); e != nil {
			t.Fatal(e)
		}
	}
	return db
}
func scalar[T any](t *testing.T, db *sql.DB, q string, args ...any) T {
	t.Helper()
	var v T
	if e := db.QueryRow(q, args...).Scan(&v); e != nil {
		t.Fatal(q, e)
	}
	return v
}
func convert(t *testing.T, root string, opts enginedb.Options) enginedb.Result {
	t.Helper()
	r, e := bridge.Convert(context.Background(), root, opts)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func playlistTracks(t *testing.T, db *sql.DB, title, parent string) []int64 {
	t.Helper()
	id := scalar[int64](t, db, `SELECT c.id FROM Playlist c JOIN Playlist p ON p.id=c.parentListId WHERE c.title=? AND p.title=?`, title, parent)
	return entityChain(t, db, id)
}
func entityChain(t *testing.T, db *sql.DB, list int64) []int64 {
	t.Helper()
	rows, e := db.Query(`SELECT id,trackId,nextEntityId FROM PlaylistEntity WHERE listId=?`, list)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	type node struct{ track, next int64 }
	nodes := map[int64]node{}
	targets := map[int64]bool{}
	for rows.Next() {
		var id int64
		var n node
		if e = rows.Scan(&id, &n.track, &n.next); e != nil {
			t.Fatal(e)
		}
		nodes[id] = n
		targets[n.next] = true
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	var head int64
	heads := 0
	for id := range nodes {
		if !targets[id] {
			head = id
			heads++
		}
	}
	if len(nodes) > 0 && heads != 1 {
		t.Fatalf("invalid heads: %d", heads)
	}
	var out []int64
	seen := map[int64]bool{}
	for head != 0 {
		n, ok := nodes[head]
		if !ok || seen[head] {
			t.Fatal("broken/cyclic entity chain")
		}
		seen[head] = true
		out = append(out, n.track)
		head = n.next
	}
	if len(out) != len(nodes) {
		t.Fatal("unreachable playlist members")
	}
	return out
}

func TestEndToEndPDBAnalysisNFDIncrementalSmartMatch(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	before := snapshot(t, filepath.Join(root, "PIONEER"))
	audioBefore := snapshot(t, filepath.Join(root, "MUSIC"))
	opts := enginedb.Options{SmartMatch: true, CurrentTrack: 1}
	r := convert(t, root, opts)
	if r.Added != 5 || r.Updated != 0 || !r.PlaylistsChanged {
		t.Fatalf("first import: %+v", r)
	}
	db := dbOpen(t, root)
	if got := scalar[string](t, db, `PRAGMA integrity_check`); got != "ok" {
		t.Fatal(got)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatal("foreign key errors", n)
	}
	if got := scalar[string](t, db, `SELECT schemaVersionMajor||'.'||schemaVersionMinor||'.'||schemaVersionPatch FROM Information`); got != "3.0.2" {
		t.Fatal(got)
	}
	actualPath := scalar[string](t, db, `SELECT path FROM Track WHERE pdbImportKey=1`)
	wantPath := "../MUSIC/" + norm.NFD.String(testfixture.UnicodeName)
	if actualPath != wantPath {
		t.Fatalf("path bytes %q != %q", actualPath, wantPath)
	}
	if _, e := os.Stat(filepath.Join(root, "Engine Library", actualPath)); e != nil {
		t.Fatal("Engine-relative path cannot open audio", e)
	}
	if got := scalar[int](t, db, `SELECT key FROM Track WHERE pdbImportKey=1`); got != 1 {
		t.Fatalf("Am Engine enum: %d", got)
	}
	if scalar[int](t, db, `SELECT fileBytes FROM Track WHERE pdbImportKey=1`) == 0 {
		t.Fatal("missing size")
	}
	if scalar[int](t, db, `SELECT isAnalyzed FROM Track WHERE pdbImportKey=1`) != 1 {
		t.Fatal("complete analysis not marked")
	}
	if scalar[int](t, db, `SELECT isAnalyzed FROM Track WHERE pdbImportKey=2`) != 0 {
		t.Fatal("missing analysis falsely marked complete")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData`); n != 5 {
		t.Fatalf("factory trigger rows %d", n)
	}
	if got := playlistTracks(t, db, "同名", "Nested"); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatal(got)
	}
	if got := playlistTracks(t, db, "同名", "Other"); !reflect.DeepEqual(got, []int64{3, 4}) {
		t.Fatal(got)
	}
	smartID := scalar[int64](t, db, `SELECT id FROM Playlist WHERE title='[Smart Match]'`)
	if got := entityChain(t, db, smartID); !reflect.DeepEqual(got, []int64{2, 3}) {
		t.Fatalf("Smart Match order/boundary: %v", got)
	}
	if n := scalar[int](t, db, `SELECT nextListId FROM Playlist WHERE id=?`, smartID); n != 0 {
		t.Fatal("Smart Match not at end")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Playlist WHERE nextListId=?`, smartID); n != 1 {
		t.Fatal("Smart Match unreachable")
	}
	decode := func(col string) []byte {
		blob := scalar[[]byte](t, db, `SELECT `+col+` FROM PerformanceData WHERE trackId=1`)
		b, e := enginedb.QUncompress(blob, 1<<20)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	track := decode("trackData")
	if len(track) != 44 || binary.BigEndian.Uint64(track[8:]) != 96000 {
		t.Fatalf("trackData length/samples: %x", track)
	}
	wave := decode("overviewWaveFormData")
	if len(wave) != 3099 || !bytes.Equal(wave[24:27], []byte{10, 20, 30}) {
		t.Fatalf("PWV7 overview: %x", wave[:27])
	}
	beat := decode("beatData")
	if binary.BigEndian.Uint64(beat[17:]) != 4 {
		t.Fatal("beat markers missing")
	}
	q := decode("quickCues")
	if binary.BigEndian.Uint64(q) != 8 {
		t.Fatal("Engine requires exactly eight hot-cue slots")
	}
	reader := bytes.NewReader(q[8:])
	type cue struct {
		name     string
		position float64
		color    [4]byte
	}
	var cues []cue
	for i := 0; i < 8; i++ {
		n, e := reader.ReadByte()
		if e != nil {
			t.Fatal(e)
		}
		name := make([]byte, n)
		if _, e = reader.Read(name); e != nil {
			t.Fatal(e)
		}
		c := cue{name: string(name)}
		if e = binary.Read(reader, binary.BigEndian, &c.position); e != nil {
			t.Fatal(e)
		}
		if _, e = reader.Read(c.color[:]); e != nil {
			t.Fatal(e)
		}
		cues = append(cues, c)
	}
	if cues[0].position != -1 || cues[0].name != "" {
		t.Fatal("Hot Cue A was synthesized", cues[0])
	}
	if cues[1].position != 24000 || cues[1].name != "B 原有" || cues[1].color != [4]byte{255, 12, 34, 56} {
		t.Fatalf("original B changed: %+v", cues[1])
	}
	var main float64
	if e := binary.Read(reader, binary.BigEndian, &main); e != nil {
		t.Fatal(e)
	}
	if cues[7].position != 48000 || main != 12000 {
		t.Fatal("H or main memory cue missing", cues, main)
	}
	var source struct{ Analysis rekordbox.Analysis }
	if e := json.Unmarshal(scalar[[]byte](t, db, `SELECT sourceJSON FROM BridgeAnalysis WHERE trackId=1`), &source); e != nil {
		t.Fatal(e)
	}
	if source.Analysis.Structure == nil || len(source.Analysis.Structure.Phrases) != 1 || len(source.Analysis.Waveforms) != 3 {
		t.Fatal("source metadata lost")
	}
	if len(source.Analysis.Cues) != 4 || source.Analysis.Cues[1].HotCue != 0 || source.Analysis.Cues[1].TimeMS != 250 || source.Analysis.Cues[1].Name != "Mémoire" || source.Analysis.Cues[1].Color != [4]byte{255, 55, 66, 77} {
		t.Fatal("lossless memory cue missing")
	}
	loops := scalar[[]byte](t, db, `SELECT loops FROM PerformanceData WHERE trackId=1`)
	if loops[0] != 8 || !bytes.Contains(loops, []byte("H Loop")) || !bytes.Contains(loops, []byte("Memory Loop")) {
		t.Fatal("loop lost")
	}
	// Byte-for-byte no-op checks include source audio and all Pioneer metadata.
	perfBefore := scalar[[]byte](t, db, `SELECT quickCues FROM PerformanceData WHERE trackId=1`)
	dbBefore := snapshot(t, filepath.Join(root, "Engine Library/Database2"))
	r = convert(t, root, opts)
	if r.Unchanged != 5 || r.Added+r.Updated != 0 || r.PlaylistsChanged {
		t.Fatalf("second import: %+v", r)
	}
	if !reflect.DeepEqual(dbBefore, snapshot(t, filepath.Join(root, "Engine Library/Database2"))) {
		t.Fatal("no-op modified database files")
	}
	if !reflect.DeepEqual(before, snapshot(t, filepath.Join(root, "PIONEER"))) || !reflect.DeepEqual(audioBefore, snapshot(t, filepath.Join(root, "MUSIC"))) {
		t.Fatal("source was modified")
	}
	// File mtime alone invalidates exactly one record, retaining all Engine IDs.
	p := filepath.Join(root, "MUSIC/three.wav")
	stamp := time.Unix(1700000000, 0)
	if e := os.Chtimes(p, stamp, stamp); e != nil {
		t.Fatal(e)
	}
	r = convert(t, root, opts)
	if r.Updated != 1 || r.Unchanged != 4 || r.PlaylistsChanged {
		t.Fatalf("audio delta: %+v", r)
	}
	// ANLZ-only edits invalidate track 1 even if export.pdb and audio did not change.
	anlz := filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT")
	if e := os.Chtimes(anlz, stamp, stamp); e != nil {
		t.Fatal(e)
	}
	r = convert(t, root, opts)
	if r.Updated != 1 || r.Unchanged != 4 {
		t.Fatalf("analysis delta: %+v", r)
	}
	if !bytes.Equal(perfBefore, scalar[[]byte](t, db, `SELECT quickCues FROM PerformanceData WHERE trackId=1`)) {
		t.Fatal("delta changed cues")
	}
	// A PDB metadata edit updates just the affected track and keeps IDs stable.
	pdb := testfixture.PDB()
	idx := bytes.Index(pdb, []byte{0xe0, 0x2e, 0, 0})
	if idx < 0 {
		t.Fatal("BPM fixture offset")
	}
	binary.LittleEndian.PutUint32(pdb[idx:], 12100)
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", pdb)
	r = convert(t, root, opts)
	if r.Updated != 1 || r.Unchanged != 4 {
		t.Fatalf("record hash delta: %+v", r)
	}
	if scalar[int64](t, db, `SELECT id FROM Track WHERE pdbImportKey=1`) != 1 {
		t.Fatal("track ID changed")
	}
}

func TestEngineOnlyBypassAndProtectedSource(t *testing.T) {
	root := t.TempDir()
	testfixture.Write(t, root, "Engine Library/Database2/m.db", []byte("native engine library"))
	before := snapshot(t, root)
	r := convert(t, root, enginedb.Options{})
	if !r.Bypassed || !reflect.DeepEqual(before, snapshot(t, root)) {
		t.Fatal("Engine-only drive was touched")
	}
	for _, name := range []string{"symlink-dir", "symlink-db", "hardlink-db"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			testfixture.Export(t, root)
			pioneer := filepath.Join(root, "PIONEER")
			before := snapshot(t, pioneer)
			if name == "symlink-dir" {
				if e := os.Symlink(pioneer, filepath.Join(root, "Engine Library")); e != nil {
					t.Fatal(e)
				}
			} else {
				dir := filepath.Join(root, "Engine Library/Database2")
				if e := os.MkdirAll(dir, 0755); e != nil {
					t.Fatal(e)
				}
				link := os.Symlink
				if name == "hardlink-db" {
					link = os.Link
				}
				if e := link(filepath.Join(pioneer, "rekordbox/export.pdb"), filepath.Join(dir, "m.db")); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := bridge.Convert(context.Background(), root, enginedb.Options{}); e == nil {
				t.Fatal("unsafe output accepted")
			}
			if !reflect.DeepEqual(before, snapshot(t, pioneer)) {
				t.Fatal("PIONEER changed")
			}
		})
	}
}

func TestFailedUpdateRollsBack(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	before := scalar[string](t, db, `SELECT group_concat(title,'|') FROM Track`)
	// Change the first track, then break the last audio header so the entire
	// transaction (including prior successful track writes) must roll back.
	l, e := bridge.ReadLibrary(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	l.Tracks[0].Title = "must roll back"
	testfixture.Write(t, root, "MUSIC/five.wav", []byte("RIFFbroken"))
	if _, e = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); e == nil {
		t.Fatal("bad audio accepted")
	}
	if got := scalar[string](t, db, `SELECT group_concat(title,'|') FROM Track`); got != before {
		t.Fatalf("partial commit: %s", got)
	}
}

func TestSQLiteIntelligentAndNativeRows(t *testing.T) {
	root := t.TempDir()
	testfixture.Write(t, root, "MUSIC/a.wav", testfixture.WAV(true))
	p := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite", p)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`CREATE TABLE content(id INTEGER,title TEXT,file_path TEXT,analysis_data_path TEXT,bpm INTEGER,key_name TEXT);INSERT INTO content VALUES(1,'Track','/MUSIC/a.wav','',12000,'8A');CREATE TABLE playlist(id INTEGER,parent_id INTEGER,name TEXT,attribute INTEGER,seq INTEGER,smart_list TEXT);INSERT INTO playlist VALUES(10,0,'Folder',1,1,''),(11,10,'Intelligent',4,1,'<rules/>');CREATE TABLE playlist_content(playlist_id INTEGER,content_id INTEGER,track_no INTEGER);INSERT INTO playlist_content VALUES(11,1,1);`)
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	before := snapshot(t, filepath.Join(root, "PIONEER"))
	r := convert(t, root, enginedb.Options{})
	if r.Added != 1 {
		t.Fatal(r)
	}
	engine := dbOpen(t, root)
	if got := playlistTracks(t, engine, "Intelligent", "Folder"); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatal(got)
	}
	var metadata struct{ Playlists []rekordbox.Playlist }
	if e = json.Unmarshal(scalar[[]byte](t, engine, `SELECT value FROM BridgeLibrary WHERE key='source'`), &metadata); e != nil {
		t.Fatal(e)
	}
	if !metadata.Playlists[1].Intelligent || metadata.Playlists[1].Rules != "<rules/>" {
		t.Fatal("smart rules lost")
	}
	if !reflect.DeepEqual(before, snapshot(t, filepath.Join(root, "PIONEER"))) {
		t.Fatal("SQLite source changed")
	}
	// Native playlists coexist; native same-path tracks must not be overwritten.
	if _, e = engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Native',0,1,0)`); e != nil {
		t.Fatal(e)
	}
	r = convert(t, root, enginedb.Options{SmartMatch: true, CurrentTrack: 1})
	if r.Updated != 0 || scalar[int](t, engine, `SELECT count(*) FROM Playlist WHERE title='Native'`) != 1 {
		t.Fatal("native playlist changed")
	}
}

func TestErrorMessages(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	_, e := bridge.Convert(context.Background(), root, enginedb.Options{SmartMatch: true, CurrentTrack: 99})
	if e == nil || !strings.Contains(fmt.Sprint(e), "reference track") {
		t.Fatal(e)
	}
}
