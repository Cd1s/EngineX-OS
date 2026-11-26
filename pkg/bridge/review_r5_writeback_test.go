package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestReviewR5MixedEdits(t *testing.T) {
	for _, coincident := range []bool{false, true} {
		t.Run(fmt.Sprintf("coincident=%t", coincident), func(t *testing.T) {
			env := setupPhantomTestStick(t, nil)
			resolver, e := compat.NewResolver(env.root)
			if e != nil {
				t.Fatal(e)
			}
			one := func() map[uint32]rekordbox.Track {
				lib, err := rekordbox.ReadSQLiteWhole(context.Background(), env.onePath)
				if err != nil {
					t.Fatal(err)
				}
				result := map[uint32]rekordbox.Track{}
				for _, v := range lib.Tracks {
					result[v.ID] = v
				}
				return result
			}
			before := one()
			other := before[2]
			otherRaw := map[string][]byte{}
			for _, ext := range []string{".DAT", ".EXT"} {
				if other.AnalysisPath != "" {
					p := filepath.Join(env.root, other.AnalysisPath)
					p = p[:len(p)-len(filepath.Ext(p))] + ext
					if raw, err := os.ReadFile(p); err == nil {
						otherRaw[p] = raw
					}
				}
			}
			if len(otherRaw) != 2 {
				t.Fatal("missing control track files")
			}
			noncue := func(data []byte) [][]byte {
				var out [][]byte
				for _, v := range dumpANLZSections(t, data) {
					if v.tag != "PCOB" && v.tag != "PCO2" {
						out = append(out, v.raw)
					}
				}
				return out
			}
			baseDAT, baseEXT := noncue(env.origDAT), noncue(env.origEXT)
			slots := map[int]uint32{0: 600}
			if coincident {
				for i := 0; i < 8; i++ {
					slots[i] = 60
				}
			}
			for ix, action := range []string{"add", "move", "delete", "loop"} {
				switch action {
				case "move":
					slots[0] = 750
				case "delete":
					delete(slots, 0)
				}
				setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, slots)
				db, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
				if err != nil {
					t.Fatal(err)
				}
				if action == "loop" {
					perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 650, SampleRate: 44100}, &rekordbox.Analysis{Cues: []rekordbox.Cue{{HotCue: 1, Loop: true, TimeMS: 1000, EndMS: 1500}}, Beats: []rekordbox.Beat{{TimeMS: 0, BPM: 120, Number: 1}}})
					if err != nil {
						_ = db.Close()
						t.Fatal(err)
					}
					if _, err = db.Exec("UPDATE PerformanceData SET loops=? WHERE trackId=1", perf.Loops); err != nil {
						_ = db.Close()
						t.Fatal(err)
					}
				}
				if _, err := db.Exec("UPDATE Track SET comment=? WHERE id=1", action); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				if _, err := db.Exec("UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+? WHERE id=1", (ix+1)*50000); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				_, err = WriteBackCues(context.Background(), env.root, WriteBackOptions{})
				if err != nil {
					t.Fatal(err)
				}
				rows := one()
				if rows[1].Comment != action {
					t.Fatalf("%s: metadata edit missing", action)
				}
				if !reflect.DeepEqual(other, rows[2]) {
					t.Fatalf("%s: changed other database track", action)
				}
				for p, orig := range otherRaw {
					if raw, err := os.ReadFile(p); err != nil || !bytes.Equal(orig, raw) {
						t.Fatalf("%s: changed other analysis file", action)
					}
				}
				dat, _ := os.ReadFile(env.dat1Path)
				ext, _ := os.ReadFile(env.ext1Path)
				if !reflect.DeepEqual(baseDAT, noncue(dat)) || !reflect.DeepEqual(baseEXT, noncue(ext)) {
					t.Fatalf("%s: changed a non-cue analysis section", action)
				}
				for _, target := range []struct {
					name string
					cues []rekordbox.Cue
				}{{"ANLZ", readANLZCues(resolver, env.dat1Rel)}, {"OneLibrary", rows[1].Cues}} {
					got := map[int]uint32{}
					loop := false
					for _, cue := range target.cues {
						if cue.Loop {
							if cue.EndMS > cue.TimeMS {
								loop = true
							}
						} else if cue.HotCue > 0 {
							got[int(cue.HotCue)-1] = cue.TimeMS
						}
					}
					if !reflect.DeepEqual(got, slots) || (action == "loop" && !loop) {
						t.Errorf("%s %s: got pads=%v loop=%t, want pads=%v loop=%t", action, target.name, got, loop, slots, action == "loop")
					}
				}
			}
		})
	}
}

func TestReviewR5DeleteAllEightCoincidentPads(t *testing.T) {
	ctx := context.Background()
	env := setupPhantomTestStick(t, nil)
	r, err := compat.NewResolver(env.root)
	if err != nil {
		t.Fatal(err)
	}

	slots := map[int]uint32{}
	for i := 0; i < 8; i++ {
		slots[i] = 60
	}
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, slots)
	db, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+50000 WHERE id=1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	if res, err := WriteBackCues(ctx, env.root, WriteBackOptions{}); err != nil || res.TracksChanged != 1 {
		t.Fatalf("add 8 coincident pads: res=%+v err=%v", res, err)
	}
	if got := readANLZCues(r, env.dat1Rel); len(got) != 8 {
		t.Fatalf("expected 8 cues after add, got %d", len(got))
	}

	// Pass 2: DJ explicitly deletes ALL 8 pads on the deck.
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, nil)
	db, err = sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100000 WHERE id=1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	res2, err := WriteBackCues(ctx, env.root, WriteBackOptions{})
	if err != nil || res2.TracksChanged != 1 || !res2.OneLibraryUpdated {
		t.Fatalf("delete all 8 coincident pads: res=%+v err=%v", res2, err)
	}
	if got := readANLZCues(r, env.dat1Rel); len(got) != 0 {
		t.Fatalf("ANLZ still kept deleted 8 coincident pads: got %d cues", len(got))
	}
	one, err := rekordbox.ReadSQLiteWhole(ctx, env.onePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range one.Tracks {
		if tr.ID == 1 && len(tr.Cues) != 0 {
			t.Fatalf("OneLibrary still kept deleted 8 coincident pads: got %d cues", len(tr.Cues))
		}
	}
}

func TestReviewR5DeckCueEditOnLegacyPhantomTrackWritesDeckCues(t *testing.T) {
	ctx := context.Background()
	env := setupPhantomTestStick(t, nil)
	var cues []rekordbox.Cue
	for pad := uint32(1); pad <= 8; pad++ {
		cues = append(cues, rekordbox.Cue{HotCue: pad, TimeMS: 25})
	}
	dat, ext := makeSwappedPairFromCues(t, env.origDAT, env.origEXT, cues)
	testfixture.Write(t, env.root, env.dat1Rel, dat)
	testfixture.Write(t, env.root, env.ext1Rel, ext)
	insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, cues)
	refreshPhantomSnapStamp(t, env)

	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, nil)
	syncTrackCueBaseline(t, env.dbDir, 1, "contents/fixturea/track01.mp3")

	// DJ sets a real cue on the deck (Pad 1 at 5000 ms) on a track carrying legacy phantom cues.
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, map[int]uint32{0: 5000})
	db, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+999 WHERE id=1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	res, err := WriteBackCues(ctx, env.root, WriteBackOptions{})
	if err != nil || res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("deck cue edit on legacy phantom track failed: res=%+v err=%v", res, err)
	}
	r, err := compat.NewResolver(env.root)
	if err != nil {
		t.Fatal(err)
	}
	gotANLZ := readANLZCues(r, env.dat1Rel)
	if len(gotANLZ) != 1 || gotANLZ[0].HotCue != 1 || gotANLZ[0].TimeMS != 5000 {
		t.Fatalf("ANLZ cues = %+v, want single Pad 1 at 5000ms", gotANLZ)
	}
	one, err := rekordbox.ReadSQLiteWhole(ctx, env.onePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range one.Tracks {
		if tr.ID == 1 {
			if len(tr.Cues) != 1 || tr.Cues[0].HotCue != 1 || tr.Cues[0].TimeMS != 5000 {
				t.Fatalf("OneLibrary cues = %+v, want single Pad 1 at 5000ms", tr.Cues)
			}
			return
		}
	}
	t.Fatal("missing Track 1 in OneLibrary")
}

func TestReviewR5MetadataKeepsExportOnlyPad(t *testing.T) {
	env := setupPhantomTestStick(t, []rekordbox.Cue{{HotCue: 3, TimeMS: 800, Name: "Export pad"}})
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, nil)
	insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Valid: true, Int64: 1}, sql.NullInt64{}, []rekordbox.Cue{{HotCue: 3, TimeMS: 800, Name: "Export pad"}})
	refreshPhantomSnapStamp(t, env)
	syncTrackCueBaseline(t, env.dbDir, 1, "contents/fixturea/track01.mp3")

	db, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE Track SET comment='Metadata only' WHERE id=1"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+900 WHERE id=1"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{SkipPreparingCheck: true})
	if err != nil || res.TracksChanged != 1 {
		t.Fatalf("write-back: %+v %v", res, err)
	}
	one, err := rekordbox.ReadSQLiteWhole(context.Background(), env.onePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range one.Tracks {
		if v.ID == 1 && (len(v.Cues) != 1 || v.Comment != "Metadata only") {
			t.Errorf("export has %d cues and comment %q", len(v.Cues), v.Comment)
		}
	}
	dat, _ := os.ReadFile(env.dat1Path)
	ext, _ := os.ReadFile(env.ext1Path)
	if !bytes.Equal(env.origDAT, dat) || !bytes.Equal(env.origEXT, ext) {
		t.Error("metadata-only update rewrote ANLZ with a clean deck cue baseline")
	}
}

func TestReviewR5WriterKeepsWALWriteAccess(t *testing.T) {
	root := t.TempDir()
	seedRealRekordboxOneLibraryStick(t, root)
	path := filepath.Join(root, "Engine Library", "Database2", "m.db")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	for _, s := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=0",
		"PRAGMA wal_autocheckpoint=0",
		"PRAGMA wal_checkpoint(TRUNCATE)",
		"UPDATE Track SET comment='committed' WHERE id=1",
		"UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+1000 WHERE id=1",
		"BEGIN IMMEDIATE",
		"UPDATE Track SET comment='uncommitted' WHERE id=1",
	} {
		if _, err = writer.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) []byte {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	mainBefore, walBefore := read(path), read(path+"-wal")
	if len(walBefore) == 0 {
		t.Fatal("expected a live WAL")
	}
	released := false
	quiet := func() error {
		if !released {
			if !bytes.Equal(mainBefore, read(path)) || !bytes.Equal(walBefore, read(path+"-wal")) {
				t.Error("watcher changed database or WAL bytes")
			}
			if _, e := writer.Exec("COMMIT"); e != nil {
				t.Fatalf("writer commit without waiting failed: %v", e)
			}
			released = true
		}
		return nil
	}
	_, err = WriteBackCues(context.Background(), root, WriteBackOptions{Settled: quiet, SkipPreparingCheck: true})
	if err != nil || !released {
		t.Fatalf("watcher with active writer: %v commit=%v", err, released)
	}
	lib, e := rekordbox.ReadSQLiteWhole(context.Background(), filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db"))
	if e != nil {
		t.Fatal(e)
	}
	for _, tr := range lib.Tracks {
		if tr.ID == 1 {
			if tr.Comment != "committed" {
				t.Fatalf("watcher saw %q", tr.Comment)
			}
			return
		}
	}
	t.Fatal("missing fixture track")
}
