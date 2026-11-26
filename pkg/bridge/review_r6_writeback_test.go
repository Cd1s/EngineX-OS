package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func setupReviewR6Stick(t *testing.T, pads []rekordbox.Cue, onDemand bool) (root, dat1Rel, ext1Rel, onePath string) {
	t.Helper()
	root = t.TempDir()
	dat1, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_compact.dat"))
	if err != nil {
		t.Fatal(err)
	}
	ext1, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_compact.ext"))
	if err != nil {
		t.Fatal(err)
	}
	dat2, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_mem_full.dat"))
	if err != nil {
		t.Fatal(err)
	}
	ext2, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_mem_full.ext"))
	if err != nil {
		t.Fatal(err)
	}

	dat1Rel = "PIONEER/USBANLZ/P001/ANLZ0000.DAT"
	ext1Rel = "PIONEER/USBANLZ/P001/ANLZ0000.EXT"
	dat2Rel := "PIONEER/USBANLZ/P002/ANLZ0000.DAT"
	ext2Rel := "PIONEER/USBANLZ/P002/ANLZ0000.EXT"
	testfixture.Write(t, root, dat1Rel, dat1)
	testfixture.Write(t, root, ext1Rel, ext1)
	testfixture.Write(t, root, dat2Rel, dat2)
	testfixture.Write(t, root, ext2Rel, ext2)

	mp3 := makeLAMEFixtureMP3(25000)
	testfixture.Write(t, root, "Contents/FixtureA/Track01.mp3", mp3)
	testfixture.Write(t, root, "Contents/FixtureB/Track02.mp3", mp3)

	onePath = filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db")
	if err := os.MkdirAll(filepath.Dir(onePath), 0o755); err != nil {
		t.Fatal(err)
	}
	oneDB, err := sql.Open("sqlite", onePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = oneDB.Exec(`
		PRAGMA journal_mode=DELETE;
		CREATE TABLE genre(genre_id integer primary key, name varchar);
		CREATE TABLE label(label_id integer primary key, name varchar);
		CREATE TABLE content(
			content_id integer primary key,
			title varchar,
			bpmx100 integer,
			length integer,
			path varchar,
			samplingRate integer,
			genre_id integer,
			label_id integer,
			djComment varchar,
			analysisDataFilePath varchar,
			hasModified integer,
			cueUpdateCount integer,
			analysisDataUpdateCount integer
		);
		CREATE TABLE cue(
			cue_id integer primary key,
			content_id integer,
			kind integer,
			colorTableIndex integer,
			cueComment varchar,
			isActiveLoop integer,
			beatLoopNumerator integer,
			beatLoopDenominator integer,
			inUsec integer,
			outUsec integer,
			in150FramePerSec integer,
			out150FramePerSec integer
		);
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, genre_id, label_id, djComment, analysisDataFilePath, hasModified, cueUpdateCount, analysisDataUpdateCount)
		VALUES
			(1, 'Fixture Track 1', 12800, 650, '/Contents/FixtureA/Track01.mp3', 44100, NULL, NULL, '', '/PIONEER/USBANLZ/P001/ANLZ0000.DAT', 0, NULL, NULL),
			(2, 'Fixture Track 2', 12400, 650, '/Contents/FixtureB/Track02.mp3', 44100, NULL, NULL, '', '/PIONEER/USBANLZ/P002/ANLZ0000.DAT', 0, NULL, NULL);
	`)
	_ = oneDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	insertOneLibCues(t, onePath, 1, 1, sql.NullInt64{Valid: true, Int64: 1}, sql.NullInt64{}, pads)
	for _, f := range []struct {
		p    string
		data []byte
		ext  bool
	}{{dat1Rel, dat1, false}, {ext1Rel, ext1, true}} {
		out, _, err := rekordbox.RewriteANLZCues(f.data, pads, f.ext, false)
		if err != nil {
			t.Fatal(err)
		}
		testfixture.Write(t, root, f.p, out)
	}

	ctx := context.Background()
	if onDemand {
		if _, err := ConvertAtMount(ctx, root, MountOptions{OnDemand: true}); err != nil {
			t.Fatalf("ConvertAtMount(OnDemand): %v", err)
		}
	} else {
		if _, err := ConvertAtMount(ctx, root, MountOptions{}); err != nil {
			t.Fatalf("ConvertAtMount: %v", err)
		}
		if _, err := FillInBackground(ctx, root, FillOptions{}); err != nil {
			t.Fatalf("FillInBackground: %v", err)
		}
		if _, err := ConvertAtMount(ctx, root, MountOptions{Publish: true}); err != nil {
			t.Fatalf("ConvertAtMount(Publish): %v", err)
		}
	}
	return root, dat1Rel, ext1Rel, onePath
}

func readOneLibTrack1(t *testing.T, onePath string) rekordbox.Track {
	t.Helper()
	lib, err := rekordbox.ReadSQLiteWhole(context.Background(), onePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range lib.Tracks {
		if tr.ID == 1 {
			return tr
		}
	}
	t.Fatal("missing Track 1 in exportLibrary.db")
	return rekordbox.Track{}
}

func refreshOnDemandSnapExport(t *testing.T, root, dbDir string) {
	t.Helper()
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	sources, stamp, err := exportNow(root, snap.Header.Layout)
	if err != nil {
		t.Fatal(err)
	}
	snap.Header.Sources, snap.Header.Stamp = sources, stamp
	libs, _, err := ReadSources(context.Background(), r, root, sources, newKeyCache())
	if err != nil {
		t.Fatal(err)
	}
	for _, lib := range libs {
		if lib.Lib == nil {
			continue
		}
		for _, libTr := range lib.Lib.Tracks {
			if st := snap.Tracks[rbstate.FoldPath(libTr.Path)]; st != nil {
				for i := range st.Refs {
					if st.Refs[i].Kind == lib.Kind && st.Refs[i].ID == libTr.ID {
						st.Refs[i].RecordHash = libTr.RecordHash
					}
				}
			}
		}
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
}

func TestReviewR6UnloadedMetadata(t *testing.T) {
	ctx := context.Background()
	for _, count := range []int{1, 8} {
		for _, shape := range []string{"absent", "null", "import", "validTrackNullCues", "validTrackImportCues"} {
			for _, field := range []string{"comment", "label", "genre"} {
				t.Run(fmt.Sprintf("%d/%s/%s", count, shape, field), func(t *testing.T) {
					t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
					pads := make([]rekordbox.Cue, 0, count)
					for i := 1; i <= count; i++ {
						pos := uint32(800)
						if count == 8 {
							pos = 25
						}
						pads = append(pads, rekordbox.Cue{HotCue: uint32(i), Name: fmt.Sprint("original ", i), TimeMS: pos})
					}
					root, dat, ext, onePath := setupReviewR6Stick(t, pads, true)
					readFile := func(rel string) []byte {
						raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
						if err != nil {
							t.Fatal(err)
						}
						return raw
					}
					beforeD, beforeE := readFile(dat), readFile(ext)
					before := readOneLibTrack1(t, onePath)
					if len(before.Cues) != count {
						t.Fatalf("invalid exported cue fixture: got %d, want %d", len(before.Cues), count)
					}

					dir := filepath.Join(root, "Engine Library", "Database2")
					snap, err := rbstate.Load(dir)
					if err != nil {
						t.Fatal(err)
					}
					row := snap.Tracks["contents/fixturea/track01.mp3"]
					if row == nil {
						t.Fatal("missing snapshot track")
					}
					row.Synced = nil
					if err := snap.Save(dir); err != nil {
						t.Fatal(err)
					}

					db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					execSQL := func(q string, args ...any) {
						if _, err := db.Exec(q, args...); err != nil {
							t.Fatal(err)
						}
					}
					var savedQC, savedLoops []byte
					if shape == "validTrackNullCues" {
						if err := db.QueryRow("SELECT quickCues, loops FROM PerformanceData WHERE trackId=1").Scan(&savedQC, &savedLoops); err != nil {
							t.Fatal(err)
						}
					}
					switch shape {
					case "absent":
						execSQL("DELETE FROM PerformanceData WHERE trackId=1")
					case "null":
						execSQL("UPDATE PerformanceData SET quickCues=NULL, loops=NULL WHERE trackId=1")
					case "validTrackNullCues":
						execSQL("UPDATE PerformanceData SET trackData=?, quickCues=NULL, loops=NULL WHERE trackId=1", buildTestTrackDataBlob(44100, 650*44100))
					case "validTrackImportCues":
						execSQL("UPDATE PerformanceData SET trackData=? WHERE trackId=1", buildTestTrackDataBlob(44100, 650*44100))
					}
					switch field {
					case "comment":
						execSQL("UPDATE Track SET comment='DJ comment only' WHERE id=1")
					case "label":
						execSQL("UPDATE Track SET label='DJ label only' WHERE id=1")
					case "genre":
						execSQL("UPDATE Track SET genre='Techno' WHERE id=1")
					}
					execSQL("UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+9000 WHERE id=1")

					res, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true})
					if err != nil {
						t.Fatal(err)
					}
					after := readOneLibTrack1(t, onePath)
					switch field {
					case "comment":
						if after.Comment != "DJ comment only" {
							t.Errorf("comment edit missing: got %q", after.Comment)
						}
					case "label":
						if after.Label != "DJ label only" {
							t.Errorf("label edit missing: got %q", after.Label)
						}
					case "genre":
						if after.Genre != "Techno" {
							t.Errorf("genre edit missing: got %q", after.Genre)
						}
					}
					if !bytes.Equal(beforeD, readFile(dat)) || !bytes.Equal(beforeE, readFile(ext)) || !reflect.DeepEqual(before.Cues, after.Cues) {
						t.Errorf("%s-only write changed cues: OneLibrary %d -> %d, DAT equal=%v EXT equal=%v, changed=%d files=%d",
							field, len(before.Cues), len(after.Cues), bytes.Equal(beforeD, readFile(dat)), bytes.Equal(beforeE, readFile(ext)), res.TracksChanged, res.FilesUpdated)
					}

					// If NULL cue blobs are later populated with original import cues without a written baseline
					// (no deck cue edit), a subsequent metadata edit must still leave export cues and ANLZ untouched.
					if shape == "validTrackNullCues" && field == "comment" {
						execSQL("UPDATE PerformanceData SET quickCues=?, loops=? WHERE trackId=1", savedQC, savedLoops)
						execSQL("UPDATE Track SET comment='DJ follow-up comment', lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=1")
						if _, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true}); err != nil {
							t.Fatal(err)
						}
						after2 := readOneLibTrack1(t, onePath)
						if after2.Comment != "DJ follow-up comment" {
							t.Errorf("follow-up comment missing: got %q", after2.Comment)
						}
						if !bytes.Equal(beforeD, readFile(dat)) || !bytes.Equal(beforeE, readFile(ext)) || !reflect.DeepEqual(before.Cues, after2.Cues) {
							t.Errorf("follow-up comment after delayed cue blob restore changed cues: OneLibrary %d -> %d", len(before.Cues), len(after2.Cues))
						}
					}
				})
			}
		}
	}
}

func TestReviewR6NullCuesUnknownEvenWithBaseline(t *testing.T) {
	ctx := context.Background()
	cues := []rekordbox.Cue{
		{HotCue: 1, Name: "Cue A", TimeMS: 800, Color: [4]byte{255, 255, 0, 0}, ColorCode: rekordbox.NearestCueColorCode([4]byte{255, 255, 0, 0})},
		{HotCue: 2, Name: "Loop B", TimeMS: 2000, EndMS: 4000, Loop: true, Color: [4]byte{255, 0, 255, 0}, ColorCode: rekordbox.NearestCueColorCode([4]byte{255, 0, 255, 0})},
		{HotCue: 0, TimeMS: 6000, EndMS: 8000, Loop: true},
	}

	for _, mode := range []string{"bothNull", "quickCuesNullOnly", "loopsNullOnly"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
			root, dat, ext, onePath := setupReviewR6Stick(t, cues, false)
			// Establish a clean Synced baseline via an untouched pass.
			if res, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true}); err != nil || res.TracksChanged != 0 {
				t.Fatalf("initial baseline settle: res=%+v err=%v", res, err)
			}

			readFile := func(rel string) []byte {
				raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			beforeD, beforeE := readFile(dat), readFile(ext)
			before := readOneLibTrack1(t, onePath)
			if len(before.Cues) != len(cues) {
				t.Fatalf("OneLibrary cues = %d, want %d", len(before.Cues), len(cues))
			}

			dbDir := filepath.Join(root, "Engine Library", "Database2")
			db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			switch mode {
			case "bothNull":
				_, err = db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=NULL, loops=NULL WHERE trackId=1`, buildTestTrackDataBlob(44100, 650*44100))
			case "quickCuesNullOnly":
				_, err = db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=NULL WHERE trackId=1`, buildTestTrackDataBlob(44100, 650*44100))
			case "loopsNullOnly":
				_, err = db.Exec(`UPDATE PerformanceData SET trackData=?, loops=NULL WHERE trackId=1`, buildTestTrackDataBlob(44100, 650*44100))
			}
			if err != nil {
				t.Fatal(err)
			}
			wantComment := "Comment with " + mode
			if _, err := db.Exec(`UPDATE Track SET comment=? WHERE id=1`, wantComment); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+900 WHERE id=1`); err != nil {
				t.Fatal(err)
			}

			res, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true})
			if err != nil {
				t.Fatal(err)
			}
			after := readOneLibTrack1(t, onePath)
			if after.Comment != wantComment {
				t.Errorf("comment = %q, want %q", after.Comment, wantComment)
			}
			if !bytes.Equal(beforeD, readFile(dat)) || !bytes.Equal(beforeE, readFile(ext)) || !reflect.DeepEqual(before.Cues, after.Cues) {
				t.Errorf("%s erased export cues: OneLibrary %d -> %d, DAT equal=%v EXT equal=%v, changed=%d files=%d",
					mode, len(before.Cues), len(after.Cues), bytes.Equal(beforeD, readFile(dat)), bytes.Equal(beforeE, readFile(ext)), res.TracksChanged, res.FilesUpdated)
			}
		})
	}
}

func TestReviewR6StaleImportNoBaselineKeepsNewerExport(t *testing.T) {
	ctx := context.Background()
	for _, fastKnown := range []bool{false, true} {
		t.Run(fmt.Sprintf("fastKnown=%v", fastKnown), func(t *testing.T) {
			t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
			oldCues := []rekordbox.Cue{{HotCue: 1, Name: "Old Cue", TimeMS: 1000}}
			root, dat, ext, onePath := setupReviewR6Stick(t, oldCues, false)

			// Switch to an empty stage root so old filled stage does not act as a baseline for the new export.
			t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "new-stage"))

			// Simulate a newer rekordbox export on the stick while m.db still holds the older import.
			newCues := []rekordbox.Cue{
				{HotCue: 1, Name: "New Export 1", TimeMS: 2500},
				{HotCue: 2, Name: "New Export 2", TimeMS: 5000},
				{HotCue: 0, TimeMS: 7000, EndMS: 9000, Loop: true},
			}
			for _, f := range []struct {
				p   string
				ext bool
			}{{dat, false}, {ext, true}} {
				raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.p)))
				if err != nil {
					t.Fatal(err)
				}
				upd, _, err := rekordbox.RewriteANLZCues(raw, newCues, f.ext, false)
				if err != nil {
					t.Fatal(err)
				}
				testfixture.Write(t, root, f.p, upd)
			}
			insertOneLibCues(t, onePath, 1, 1, sql.NullInt64{Valid: true, Int64: 2}, sql.NullInt64{}, newCues)

			dbDir := filepath.Join(root, "Engine Library", "Database2")
			snap, err := rbstate.Load(dbDir)
			if err != nil {
				t.Fatal(err)
			}
			tr := snap.Tracks["contents/fixturea/track01.mp3"]
			tr.Synced = nil
			tr.Written = nil
			tr.Perf = rbstate.PerfNone
			tr.FastKnown = fastKnown
			if fastKnown {
				base := enginedb.FastBaseForTrack(rekordbox.Track{Cues: newCues, Duration: 650, SampleRate: 44100})
				tr.FastCues, tr.FastLoops, tr.FastActive = base.Cues, base.Loops, base.Active
			} else {
				tr.FastCues, tr.FastLoops, tr.FastActive = nil, nil, 0
			}
			if err := snap.Save(dbDir); err != nil {
				t.Fatal(err)
			}
			refreshOnDemandSnapExport(t, root, dbDir)

			readFile := func(rel string) []byte {
				raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			beforeD, beforeE := readFile(dat), readFile(ext)
			before := readOneLibTrack1(t, onePath)
			if len(before.Cues) != len(newCues) {
				t.Fatalf("new OneLibrary fixture cues = %d, want %d", len(before.Cues), len(newCues))
			}

			db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE Track SET comment='DJ comment over stale import' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+900 WHERE id=1`); err != nil {
				t.Fatal(err)
			}

			res, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true})
			if err != nil {
				t.Fatal(err)
			}
			after := readOneLibTrack1(t, onePath)
			if after.Comment != "DJ comment over stale import" {
				t.Errorf("comment missing on stale-import pass: got %q", after.Comment)
			}
			if !bytes.Equal(beforeD, readFile(dat)) || !bytes.Equal(beforeE, readFile(ext)) || !reflect.DeepEqual(before.Cues, after.Cues) {
				t.Errorf("stale import overwrote newer export cues: OneLibrary %d -> %d, DAT equal=%v EXT equal=%v, changed=%d files=%d",
					len(before.Cues), len(after.Cues), bytes.Equal(beforeD, readFile(dat)), bytes.Equal(beforeE, readFile(ext)), res.TracksChanged, res.FilesUpdated)
			}

			// Subsequent metadata-only edit without changing deck cues must continue to preserve the newer export.
			if _, err := db.Exec(`UPDATE Track SET comment='Second DJ comment' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+1500 WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			if _, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true}); err != nil {
				t.Fatal(err)
			}
			after2 := readOneLibTrack1(t, onePath)
			if after2.Comment != "Second DJ comment" {
				t.Errorf("second comment missing: got %q", after2.Comment)
			}
			if !bytes.Equal(beforeD, readFile(dat)) || !bytes.Equal(beforeE, readFile(ext)) || !reflect.DeepEqual(before.Cues, after2.Cues) {
				t.Errorf("second pass overwrote newer export cues: OneLibrary %d -> %d", len(before.Cues), len(after2.Cues))
			}
		})
	}
}

func TestReviewR6NoBaselineGridNotWritten(t *testing.T) {
	ctx := context.Background()
	pads := []rekordbox.Cue{{HotCue: 1, Name: "Pad 1", TimeMS: 800}}
	const sr = 44100.0
	const step = 60.0 * sr / 125.0 // 125.00 BPM != export BPM
	defGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{{pos: 0, idx: 0, numBeats: 16}, {pos: 16 * 20671.875, idx: 16, numBeats: 0}}
	adjGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{{pos: 5000.25, idx: 0, numBeats: 20}, {pos: 5000.25 + 20*step, idx: 20, numBeats: 0}}

	for _, tc := range []struct {
		name     string
		onDemand bool
	}{
		{name: "noSyncedOrWritten", onDemand: false},
		{name: "onDemandSyncedWithoutGridBaseline", onDemand: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
			root, dat, ext, onePath := setupReviewR6Stick(t, pads, tc.onDemand)

			// Switch to an empty stage root so staged perf does not act as a grid baseline.
			t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "clean-stage"))
			dbDir := filepath.Join(root, "Engine Library", "Database2")
			if !tc.onDemand {
				snap, err := rbstate.Load(dbDir)
				if err != nil {
					t.Fatal(err)
				}
				tr := snap.Tracks["contents/fixturea/track01.mp3"]
				tr.Synced = nil
				tr.Written = nil
				tr.Perf = rbstate.PerfNone
				if err := snap.Save(dbDir); err != nil {
					t.Fatal(err)
				}
			}

			readFile := func(rel string) []byte {
				raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			beforeD, beforeE := readFile(dat), readFile(ext)
			before := readOneLibTrack1(t, onePath)

			db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE PerformanceData SET beatData=? WHERE trackId=1`, buildEngineBeatDataBlob(sr, 20*sr, defGrid, adjGrid, true)); err != nil {
				t.Fatal(err)
			}
			wantComment := "Grid comment " + tc.name
			if _, err := db.Exec(`UPDATE Track SET comment=? WHERE id=1`, wantComment); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+500 WHERE id=1`); err != nil {
				t.Fatal(err)
			}

			res, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true})
			if err != nil {
				t.Fatal(err)
			}
			after := readOneLibTrack1(t, onePath)
			if after.Comment != wantComment {
				t.Errorf("comment missing when no grid baseline: got %q, want %q", after.Comment, wantComment)
			}
			if math.Abs(after.BPM-before.BPM) > 0.01 {
				t.Errorf("OneLibrary BPM changed without baseline: got %.2f, want %.2f", after.BPM, before.BPM)
			}
			if !bytes.Equal(beforeD, readFile(dat)) || !bytes.Equal(beforeE, readFile(ext)) || !reflect.DeepEqual(before.Cues, after.Cues) {
				t.Errorf("beat grid or cues rewritten without baseline: DAT equal=%v EXT equal=%v, changed=%d files=%d",
					bytes.Equal(beforeD, readFile(dat)), bytes.Equal(beforeE, readFile(ext)), res.TracksChanged, res.FilesUpdated)
			}
		})
	}
}
