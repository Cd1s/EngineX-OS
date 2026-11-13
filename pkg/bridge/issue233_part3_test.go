package bridge_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestIssue233LargeSkeletonAndEngineAnalyzedResume(t *testing.T) {
	p, root, skeleton := testlibrary.Create(t)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("stock skeleton: bytes=%d tracks=%d", st.Size(), scalar[int](t, skeleton, `SELECT count(*) FROM Track`))
	skeleton.Close()
	testfixture.LargeExport(t, root, issue233Tracks)
	if bridge.HasPopulatedLibrary(root) {
		t.Fatal("empty skeleton counts as populated")
	}
	r := issue233Mount(t, root, enginedb.Options{})
	if r.Added != issue233Tracks {
		t.Fatalf("first mount: %+v", r)
	}
	issue233Marks(t, root, true)
	db := dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE isAnalyzed=0`); n != issue233Tracks {
		t.Fatalf("fast import analyzed %d tracks", issue233Tracks-n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData WHERE beatData IS NULL AND quickCues IS NULL AND loops IS NULL`); n != issue233Tracks {
		t.Fatalf("fast import: %d empty performances", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM BridgeState WHERE fingerprint LIKE 'fast:%'`); n != issue233Tracks {
		t.Fatalf("fast import fingerprints: %d", n)
	}
	// Engine analyzes two bridge-owned tracks on load before the next mount,
	// with both an empty and an existing quickCues blob. Ownership must survive.
	for _, cue := range []any{nil, []byte("Engine cue")} {
		id := 1
		if cue != nil {
			id = 2
		}
		if _, err := db.Exec(`UPDATE Track SET isAnalyzed=1 WHERE pdbImportKey=?`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE PerformanceData SET beatData=?,quickCues=? WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, []byte("Engine grid"), cue, id); err != nil {
			t.Fatal(err)
		}
	}
	// A generous explicit budget isolates lifecycle correctness from host load.
	r = issue233Mount(t, root, enginedb.Options{Deadline: time.Now().Add(time.Hour)})
	if r.Updated != issue233Tracks || r.Deferred != 0 {
		t.Fatalf("resume: %+v", r)
	}
	issue233Marks(t, root, false)
	l, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := rekordbox.ReadAnalysis(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.DAT"))
	if err != nil {
		t.Fatal(err)
	}
	ext, err := rekordbox.ReadAnalysis(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}
	a.Merge(ext)
	track := l.Tracks[0]
	info, err := compat.AudioHeader(filepath.Join(root, testfixture.LargeTrackPath(1)))
	if err != nil || info.StartOffsetSamples != 2257 {
		t.Fatalf("LAME fixture: %+v %v", info, err)
	}
	track.StartOffsetSamples = info.StartOffsetSamples
	want, err := enginedb.EncodePerformance(track, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{1, 2, issue233Tracks} {
		var beats, cues, loops []byte
		if err := db.QueryRow(`SELECT beatData,quickCues,loops FROM PerformanceData WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, id).Scan(&beats, &cues, &loops); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(beats, want.Beats) || !bytes.Equal(cues, want.QuickCues) || !bytes.Equal(loops, want.Loops) {
			t.Fatalf("track %d did not replace Engine performance with offset-aligned Rekordbox data", id)
		}
		positions := readTrackPositions(t, db, id)
		if positions.beat0 != -2257 || positions.hotB != 19793 || positions.mainCue != 8768 || positions.loop0Start != 30818 || positions.loop0End != 41843 {
			t.Fatalf("track %d: wrong numerical offset alignment: %+v", id, positions)
		}
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE isAnalyzed=1`); n != issue233Tracks {
		t.Fatalf("analyzed after resume: %d", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PlaylistEntity`); n != issue233Tracks {
		t.Fatalf("playlist lost tracks: %d", n)
	}
	if r := issue233Mount(t, root, enginedb.Options{}); !r.Bypassed {
		t.Fatalf("v2 stamp not bypassed: %+v", r)
	}
}

// A pre-v2 library has non-fast fingerprints but has never read MP3Offset. A
// partial migration must distinguish unread legacy rows from completed rows.
func TestIssue233LargeLegacyOffsetMigrationResumes(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, issue233Tracks)
	issue233Mount(t, root, enginedb.Options{})
	l, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := rekordbox.ReadAnalysis(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.DAT"))
	if err != nil {
		t.Fatal(err)
	}
	ext, err := rekordbox.ReadAnalysis(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT"))
	if err != nil {
		t.Fatal(err)
	}
	a.Merge(ext)
	db := dbOpen(t, root)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	type file struct {
		Path        string
		Size, MTime int64
	}
	for _, tr := range l.Tracks {
		rel := strings.TrimPrefix(tr.Path, "/")
		st, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		var files []file
		for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
			p := "PIONEER/USBANLZ/P001/ANLZ0000" + ext
			st, err := os.Stat(filepath.Join(root, p))
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file{p, st.Size(), st.ModTime().UnixNano()})
		}
		b, _ := json.Marshal(struct {
			Encoding             int
			Record, ResolvedPath string
			Size, MTime          int64
			Analysis             []file
			Artwork              *file
		}{3, tr.RecordHash + ":" + rekordbox.RecordHash(tr), rel, st.Size(), st.ModTime().UnixNano(), files, nil})
		sum := sha256.Sum256(b)
		if _, err := tx.Exec(`UPDATE BridgeState SET fingerprint=? WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, hex.EncodeToString(sum[:]), tr.ID); err != nil {
			t.Fatal(err)
		}
	}
	unshifted, err := enginedb.EncodePerformance(l.Tracks[0], a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE Track SET isAnalyzed=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE PerformanceData SET beatData=?,quickCues=?,loops=?`, unshifted.Beats, unshifted.QuickCues, unshifted.Loops); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "Engine Library/Database2")
	if err := os.Remove(filepath.Join(dir, ".rb-bridge-pending")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-stamp"), []byte("PIONEER/rekordbox/export.pdb 12345 1700000000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	calls := 0
	opts := enginedb.Options{Deadline: start.Add(time.Minute), Now: func() time.Time {
		calls++
		if calls > 40 {
			return start.Add(time.Hour)
		}
		return start
	}}
	r := issue233Mount(t, root, opts)
	if r.Updated == 0 || r.Deferred == 0 {
		t.Fatalf("expected partial migration: %+v", r)
	}
	issue233Marks(t, root, true)
	remaining := r.Deferred
	r = issue233Mount(t, root, enginedb.Options{Deadline: time.Now().Add(time.Hour)})
	if r.Deferred != 0 || r.Updated != remaining {
		t.Fatalf("legacy migration forgot %d deferred offsets: %+v", remaining, r)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM BridgeState WHERE fingerprint LIKE 'fast:%'`); n != 0 {
		t.Fatalf("still deferred: %d", n)
	}
	issue233Marks(t, root, false)
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData WHERE beatData=?`, unshifted.Beats); n != 0 {
		t.Fatalf("%d tracks still have their unshifted legacy grid", n)
	}
	positions := readTrackPositions(t, db, issue233Tracks)
	if positions.beat0 != -2257 || positions.hotB != 19793 || positions.loop0Start != 30818 || positions.loop0End != 41843 {
		t.Fatalf("last legacy track: wrong offset alignment: %+v", positions)
	}
}

// A re-export changes the stamp without making completed v2 offsets legacy.
func TestIssue233CompletedV2LibraryReexportKeepsFinishedTracks(t *testing.T) {
	const n = 600
	root := t.TempDir()
	testfixture.LargeExport(t, root, n)
	issue233Mount(t, root, enginedb.Options{})
	if r := issue233Mount(t, root, enginedb.Options{Deadline: time.Now().Add(time.Hour)}); r.Deferred != 0 {
		t.Fatalf("initial completion: %+v", r)
	}
	issue233Marks(t, root, false)
	db := dbOpen(t, root)
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, []byte("deck-cue"), n); err != nil {
		t.Fatal(err)
	}
	// Only the export and the new audio file change; existing files retain their mtimes.
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.LargePDB(n+1))
	testfixture.Write(t, root, testfixture.LargeTrackPath(n+1), testfixture.MP3WithLAMETag())
	r := issue233Mount(t, root, enginedb.Options{Deadline: time.Now().Add(-time.Second)})
	if r.Added != 1 || r.Deferred != 1 {
		t.Errorf("re-export must defer only the new track: %+v", r)
	}
	if count := scalar[int](t, db, `SELECT count(*) FROM BridgeState WHERE fingerprint LIKE 'fast:%'`); count != 1 {
		t.Errorf("re-export demoted completed v2 tracks: %d pending", count)
	}
	issue233Marks(t, root, true)
	r = issue233Mount(t, root, enginedb.Options{Deadline: time.Now().Add(time.Hour)})
	if r.Updated != 1 || r.Deferred != 0 {
		t.Errorf("re-export resume: %+v", r)
	}
	issue233Marks(t, root, false)
	if got := scalar[[]byte](t, db, `SELECT quickCues FROM PerformanceData WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, n); !bytes.Equal(got, []byte("deck-cue")) {
		t.Fatalf("completed v2 track lost deck cue: %q", got)
	}
}
