package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// bulkLibrary is an export of n tracks on mount: three playlists in a folder, one listing a
// track twice, and cue rows on every third track, as a OneLibrary export holds them.
func bulkLibrary(t *testing.T, mount string, n int) *rekordbox.Library {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(mount, "PIONEER"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := &rekordbox.Library{}
	for i := 1; i <= n; i++ {
		rel := fmt.Sprintf("Music/Artist %d/Track %d.mp3", i%7, i)
		if err := os.MkdirAll(filepath.Join(mount, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mount, rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		tr := rekordbox.Track{ID: uint32(i), Path: "/" + rel, Title: fmt.Sprintf("Title %d", i), Artist: fmt.Sprintf("Artist %d", i%7),
			Album: "Album", Genre: "House", Key: "8A", BPM: 120 + float64(i%10), Duration: 300, SampleRate: 44100, Bitrate: 320, Year: 2020, Rating: i % 5, FileSize: 1}
		if i%3 == 0 {
			tr.Cues = []rekordbox.Cue{{HotCue: 1, TimeMS: 1000, Name: "drop", ColorID: 2}, {TimeMS: 500}, {HotCue: 2, TimeMS: 4000, EndMS: 8000, Loop: true, Active: true}}
		}
		l.Tracks = append(l.Tracks, tr)
	}
	all := make([]uint32, 0, n)
	for i := n; i >= 1; i-- {
		all = append(all, uint32(i))
	}
	l.Playlists = []rekordbox.Playlist{
		{ID: 1, Name: "Folder", Folder: true},
		{ID: 2, ParentID: 1, Name: "All", Tracks: all},
		{ID: 3, ParentID: 1, Name: "Twice", Tracks: []uint32{2, 3, 2, 5}},
		{ID: 4, Name: "Top", SortOrder: 1, Tracks: []uint32{1, 4}},
	}
	return l
}

// dump is a library as Engine sees it, with what differs between two builds by design (the
// random UUID and played indicator, and the clock) normalised.
func dump(t *testing.T, dir string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "m.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var uuid string
	if err = db.QueryRow(`SELECT uuid FROM Information`).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, q := range []string{
		`SELECT type,name,tbl_name,sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_autoindex%'`,
		`SELECT * FROM sqlite_sequence`,
		`SELECT id,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch,lastRekordBoxLibraryImportReadCounter FROM Information`,
		`SELECT id,playOrder,length,bpm,year,path,filename,bitrate,bpmAnalyzed,albumArtId,fileBytes,title,artist,album,genre,comment,label,composer,remixer,key,rating,albumArt,timeLastPlayed,isPlayed,fileType,isAnalyzed,isAvailable,isMetadataOfPackedTrackChanged,isPerfomanceDataOfPackedTrackChanged,playedIndicator,isMetadataImported,pdbImportKey,streamingSource,uri,isBeatGridLocked,originDatabaseUuid,originTrackId,streamingFlags,explicitLyrics,albumArtSourceHash FROM Track`,
		`SELECT * FROM PerformanceData`,
		`SELECT id,title,parentListId,isPersisted,nextListId,isExplicitlyExported FROM Playlist`,
		`SELECT * FROM PlaylistEntity`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, _ := rows.Columns()
		var lines []string
		for rows.Next() {
			v := make([]any, len(cols))
			p := make([]any, len(cols))
			for i := range v {
				p[i] = &v[i]
			}
			if err = rows.Scan(p...); err != nil {
				t.Fatal(err)
			}
			line := fmt.Sprintf("%q", v)
			lines = append(lines, strings.ReplaceAll(line, uuid, "UUID"))
		}
		rows.Close()
		sort.Strings(lines) // sqlite_master order is the order objects were created in
		b.WriteString(q + "\n" + strings.Join(lines, "\n") + "\n")
	}
	return b.String()
}

func check(t *testing.T, dir string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "m.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ok string
	if err = db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity_check: %q %v", ok, err)
	}
	var fk int
	if err = db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&fk); err != nil || fk != 0 {
		t.Fatalf("foreign_key_check: %d %v", fk, err)
	}
}

// #301: a first import loaded in bulk is the library an ordinary import writes: Engine's
// schema text, the same rows, and its derived data (PerformanceData rows, origin columns,
// playlist entity links) as its triggers fill them.
func TestIssue301BulkFastImportIsTheOrdinaryLibrary(t *testing.T) {
	mount := t.TempDir()
	l := bulkLibrary(t, mount, 120)
	plain, bulk := t.TempDir(), t.TempDir()
	o := Options{Fast: true, TrustPaths: true, FastCues: true, DatabaseDir: plain}
	r1, err := Sync(context.Background(), mount, l, o)
	if err != nil {
		t.Fatal(err)
	}
	o.Bulk, o.DatabaseDir = true, bulk
	r2, err := Sync(context.Background(), mount, l, o)
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 || r2.Added != 120 {
		t.Fatalf("results differ: %+v %+v", r1, r2)
	}
	check(t, bulk)
	if a, b := dump(t, plain), dump(t, bulk); a != b {
		t.Fatalf("bulk library differs from the ordinary one:\n--- ordinary\n%s\n--- bulk\n%s", a, b)
	}
	// Journal and syncs are back to Engine's defaults for whoever opens it next.
	for _, f := range []string{"m.db-journal", ".rb-bridge-state.db-journal"} {
		if _, err := os.Stat(filepath.Join(bulk, f)); err == nil {
			t.Fatalf("%s left beside the bulk library", f)
		}
	}
	// An ordinary import of the same export then finds nothing to do.
	o.Bulk = false
	r3, err := Sync(context.Background(), mount, l, o)
	if err != nil || r3.Added+r3.Updated != 0 {
		t.Fatalf("ordinary import over the bulk library: %+v %v", r3, err)
	}
	check(t, bulk)
}

// A bulk load needs a private build: the volume's own library is opened by Engine.
func TestIssue301BulkRefusesTheLiveLibrary(t *testing.T) {
	mount := t.TempDir()
	l := bulkLibrary(t, mount, 3)
	if _, err := Sync(context.Background(), mount, l, Options{Fast: true, Bulk: true}); err == nil {
		t.Fatal("bulk import into the volume's own library")
	}
}

// #301: the tracks a Fast import adds get the export database's own cue rows: hot cues,
// the main cue and loops, with no track data or grid, so Engine still analyses the track on
// load. They stay "fast:" and have no BridgeAnalysis, so the next full import replaces them.
func TestIssue301FastCuesFromTheExportDatabase(t *testing.T) {
	mount := t.TempDir()
	l := bulkLibrary(t, mount, 6)
	dir := t.TempDir()
	if _, err := Sync(context.Background(), mount, l, Options{Fast: true, TrustPaths: true, FastCues: true, Bulk: true, DatabaseDir: dir}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`ATTACH DATABASE ? AS bridge`, filepath.Join(dir, stateFile)); err != nil {
		t.Fatal(err)
	}
	for _, tr := range l.Tracks {
		var id int64
		var cues, loops, beats, data []byte
		var active, source sql.NullInt64
		var fingerprint string
		if err = db.QueryRow(`SELECT t.id,p.quickCues,p.loops,p.activeOnLoadLoops,p.beatData,p.trackData,p.thirdPartySourceId,s.fingerprint
			FROM Track t JOIN PerformanceData p ON p.trackId=t.id JOIN BridgeState s ON s.trackId=t.id WHERE t.pdbImportKey=?`, tr.ID).
			Scan(&id, &cues, &loops, &active, &beats, &data, &source, &fingerprint); err != nil {
			t.Fatalf("track %d: %v", tr.ID, err)
		}
		if beats != nil || data != nil {
			t.Fatalf("track %d: a Fast import wrote a grid or track data", tr.ID)
		}
		if !strings.HasPrefix(fingerprint, "fast:") {
			t.Fatalf("track %d: fingerprint %q", tr.ID, fingerprint)
		}
		if len(tr.Cues) == 0 {
			if cues != nil || loops != nil || source.Valid {
				t.Fatalf("track %d without cue rows got cues", tr.ID)
			}
			continue
		}
		want, err := EncodePerformance(tr, &rekordbox.Analysis{Cues: tr.Cues})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(cues, want.QuickCues) || !bytes.Equal(loops, want.Loops) || active.Int64 != int64(want.ActiveLoops) || source.Int64 != 1 {
			t.Fatalf("track %d: cues %x loops %x active %v source %v", tr.ID, cues, loops, active, source)
		}
	}
	var analysed int
	if err = db.QueryRow(`SELECT count(*) FROM bridge.BridgeAnalysis`).Scan(&analysed); err != nil || analysed != 0 {
		t.Fatalf("BridgeAnalysis rows %d %v: they would protect the export cues from the full import", analysed, err)
	}
}

// #301: TrustPaths keeps the export's spelling without a directory lookup; without it a
// Fast import still resolves the file on the volume.
func TestIssue301TrustPathsSkipsTheLookup(t *testing.T) {
	mount := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mount, "music"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "music", "a.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 1, Path: "/Music/A.mp3"}}}
	r, err := compat.NewResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	for trust, want := range map[bool]string{false: "music/a.mp3", true: "Music/A.mp3"} {
		prepared, err := prepareFast(context.Background(), r, l, trust)
		if err != nil || len(prepared) != 1 || prepared[0].rel != want {
			t.Fatalf("trust=%v: %+v %v, want %s", trust, prepared, err, want)
		}
	}
}
