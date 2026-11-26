package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

func fix3WritePlainOneLibrary(t *testing.T, mount string) {
	t.Helper()
	plainSource := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary_decrypted.db")
	raw, err := os.ReadFile(plainSource)
	if err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")
	if err := os.WriteFile(outPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", outPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		PRAGMA journal_mode=DELETE;
		ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
		ALTER TABLE content ADD COLUMN genre_id INTEGER;
		ALTER TABLE content ADD COLUMN label_id INTEGER;
		CREATE TABLE genre(genre_id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE label(label_id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO genre VALUES (20, 'Old Genre');
		INSERT INTO label VALUES (30, 'Old Label');
		DELETE FROM content;
		DELETE FROM cue;
	`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	paths := []string{"/" + testfixture.LargeTrackPath(1), "/" + testfixture.LargeTrackPath(2)}
	_, err = db.Exec(`INSERT INTO content(content_id,title,bpmx100,length,path,samplingRate,djComment,analysisDataFilePath,genre_id,label_id) VALUES
		(1,'Track 1',12800,4,?,44100,'Old Comment','/PIONEER/USBANLZ/P001/ANLZ0000.DAT',20,30),
		(2,'Track 2',12400,4,?,44100,'','/PIONEER/USBANLZ/P001/ANLZ0000.DAT',20,30)`, paths[0], paths[1])
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func fix3HoldOpen(t *testing.T, path string) func() {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = []*os.File{f}
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	done := false
	release := func() {
		if done {
			return
		}
		done = true
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	t.Cleanup(release)
	return release
}

// Finding 1.1: second write-back pass on the same track (OneLibrary cues + dual PDB/OneLibrary
// metadata) succeeds AND each write-back pass reads the source libraries only once (no second
// full decrypt/scan after file replacement).
func TestR2Proof_Finding1_1_SecondWriteBackOnOneLibrary(t *testing.T) {
	t.Run("onelibrary_two_cue_and_metadata_passes_single_read", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
		mount := filepath.Join(base, "stick")
		testfixture.LargeExport(t, mount, 2)
		fix3WritePlainOneLibrary(t, mount)
		if err := os.Remove(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb")); err != nil {
			t.Fatal(err)
		}
		mountNow(t, mount, MountOptions{})
		stageAll(t, mount)
		mountNow(t, mount, MountOptions{Publish: true})

		key := rbstate.FoldPath(testfixture.LargeTrackPath(1))
		tr := snapshot(t, mount).Tracks[key]
		if tr == nil || tr.EngineID == 0 {
			t.Fatalf("track 1 missing: %+v", tr)
		}
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		var reads atomic.Int64
		oldHook := writeBackReadLibsHook
		writeBackReadLibsHook = func() { reads.Add(1) }
		defer func() { writeBackReadLibsHook = oldHook }()

		// Pass 1: cue 0 (non-palette RGB to exercise palette quantization) + metadata.
		pos1 := 1000.0
		if err := enginedb.SetQuickCue(context.Background(), db, tr.EngineID, 0, "Cue1", "#e83422", &pos1); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET comment='Pass1', genre='House', label='Label1' WHERE id=?`, tr.EngineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=?`, tr.EngineID); err != nil {
			t.Fatal(err)
		}
		reads.Store(0)
		res1, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
		if err != nil || res1.TracksChanged != 1 || !res1.OneLibraryUpdated {
			t.Fatalf("pass 1 failed: res=%+v err=%v", res1, err)
		}
		pass1Reads := reads.Load()

		// Pass 2: cue 1 + updated comment on the exact same track.
		pos2 := 2150.0
		if err := enginedb.SetQuickCue(context.Background(), db, tr.EngineID, 1, "Cue2", "#12cc88", &pos2); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET comment='Pass2' WHERE id=?`, tr.EngineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+200 WHERE id=?`, tr.EngineID); err != nil {
			t.Fatal(err)
		}
		reads.Store(0)
		res2, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
		if err != nil || res2.TracksChanged != 1 || !res2.OneLibraryUpdated {
			t.Fatalf("pass 2 on same OneLibrary track failed: res=%+v err=%v", res2, err)
		}
		if pass1Reads != 1 {
			t.Fatalf("pass 1 called readLibraries %d times, want 1 (must not re-decrypt after write)", pass1Reads)
		}
		if got := reads.Load(); got != 1 {
			t.Fatalf("pass 2 called readLibraries %d times, want 1", got)
		}

		one, err := rekordbox.ReadSQLite(context.Background(), filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db"), nil)
		if err != nil || len(one.Tracks) < 1 {
			t.Fatalf("read OneLibrary: %v", err)
		}
		if one.Tracks[0].Comment != "Pass2" || one.Tracks[0].Genre != "House" || one.Tracks[0].Label != "Label1" {
			t.Fatalf("OneLibrary metadata after pass 2: %+v", one.Tracks[0])
		}
		if len(one.Tracks[0].Cues) < 2 {
			t.Fatalf("OneLibrary cues after pass 2 = %d, want >= 2", len(one.Tracks[0].Cues))
		}
	})

	t.Run("dual_pdb_and_onelibrary_two_metadata_passes", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
		mount := filepath.Join(base, "stick")
		testfixture.LargeExport(t, mount, 2)
		fix3WritePlainOneLibrary(t, mount)
		mountNow(t, mount, MountOptions{})
		stageAll(t, mount)
		mountNow(t, mount, MountOptions{Publish: true})

		key := rbstate.FoldPath(testfixture.LargeTrackPath(1))
		id := snapshot(t, mount).Tracks[key].EngineID
		engineWrites(t, mount, `UPDATE Track SET comment='Dual1' WHERE id=1`, `UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=1`)
		if id != 1 {
			engineWrites(t, mount, fmt.Sprintf(`UPDATE Track SET comment='Dual1' WHERE id=%d`, id), fmt.Sprintf(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=%d`, id))
		}
		res1, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
		if err != nil || res1.TracksChanged != 1 || !res1.OneLibraryUpdated {
			t.Fatalf("dual pass 1: %+v %v", res1, err)
		}
		engineWrites(t, mount, fmt.Sprintf(`UPDATE Track SET comment='Dual2' WHERE id=%d`, id), fmt.Sprintf(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+200 WHERE id=%d`, id))
		res2, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
		if err != nil || res2.TracksChanged != 1 || !res2.OneLibraryUpdated {
			t.Fatalf("dual pass 2: %+v %v", res2, err)
		}
		pdb, err := rekordbox.ReadPDB(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"))
		if err != nil || pdb.Tracks[0].Comment != "Dual2" {
			t.Fatalf("PDB after dual pass 2: err=%v comment=%q", err, pdb.Tracks[0].Comment)
		}
		one, err := rekordbox.ReadSQLite(context.Background(), filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db"), nil)
		if err != nil || one.Tracks[0].Comment != "Dual2" {
			t.Fatalf("OneLibrary after dual pass 2: err=%v comment=%q", err, one.Tracks[0].Comment)
		}
	})
}

// Finding 1.2: TakeDeckRows / carryOver / takeLive must not lock out write-back on unchanged rows,
// must restore old.Row so re-exported metadata is not lost when a track is played during swap,
// must acquire LockVolume, and must preserve nil Written/Synced slices.
func TestR2Proof_Finding1_2_TakeDeckRowsLocksOutWriteBack(t *testing.T) {
	t.Run("small_stick_writeback_not_locked_out", func(t *testing.T) {
		mount := nextStick(t, 12)
		mountNow(t, mount, MountOptions{Publish: true})
		makeNext(t, mount)
		engineWrites(t, mount, `UPDATE Track SET isPlayed=1 WHERE id=1`, `UPDATE Track SET lastEditTime=1763900000 WHERE id=1`)
		if !TakeDeckRows(context.Background(), mount) {
			t.Fatal("TakeDeckRows returned false")
		}
		hookWindowNow(t, mount)
		if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
			t.Fatalf("FillInBackground: %v", err)
		}
		if s := snapshot(t, mount); !s.Pending.None() {
			t.Fatalf("Pending not cleared after FillInBackground: %+v", s.Pending)
		}
		engineWrites(t, mount, `UPDATE Track SET comment='AfterSwap' WHERE id=1`, `UPDATE Track SET lastEditTime=1763900500 WHERE id=1`)
		wb, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
		if err != nil || wb.TracksChanged != 1 {
			t.Fatalf("WriteBackCues locked out after swap: res=%+v err=%v", wb, err)
		}
	})

	t.Run("reexport_row_update_preserved_when_deck_plays_during_swap", func(t *testing.T) {
		mount := nextStick(t, 12)
		pdbPath := filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb")
		raw, err := os.ReadFile(pdbPath)
		if err != nil {
			t.Fatal(err)
		}
		rewritten, err := rekordbox.RewritePDBMetadata(raw, map[uint32]rekordbox.Metadata{
			1: {Comment: "ReexportComment"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pdbPath, rewritten, 0o644); err != nil {
			t.Fatal(err)
		}
		later := time.Now().Add(time.Hour)
		_ = os.Chtimes(pdbPath, later, later)

		makeNext(t, mount)
		engineWrites(t, mount, `UPDATE Track SET isPlayed=1 WHERE id=1`, `UPDATE Track SET lastEditTime=1763900000 WHERE id=1`)
		if !TakeDeckRows(context.Background(), mount) {
			t.Fatal("TakeDeckRows returned false")
		}
		hookWindowNow(t, mount)
		fres, err := FillInBackground(context.Background(), mount, FillOptions{})
		if err != nil {
			t.Fatalf("FillInBackground: %v", err)
		}
		if fres.Publish {
			hookWindowNow(t, mount)
		}
		mountNow(t, mount, MountOptions{Publish: true})
		got := query(t, mount, `SELECT COALESCE(comment,'') FROM Track WHERE id=1`)
		if len(got) == 0 || got[0][0] != "ReexportComment" {
			t.Fatalf("re-exported comment lost after deck played track during swap: got %q, want ReexportComment (Pending=%+v)",
				got[0][0], snapshot(t, mount).Pending)
		}
	})

	t.Run("take_deck_rows_respects_volume_lock_and_propagates_errors", func(t *testing.T) {
		mount := nextStick(t, 12)
		makeNext(t, mount)
		engineWrites(t, mount, `UPDATE Track SET isPlayed=1 WHERE id=1`, `UPDATE Track SET lastEditTime=1763900000 WHERE id=1`)
		unlock, err := rbstate.LockVolume(mount, 0)
		if err != nil {
			t.Fatal(err)
		}
		if TakeDeckRows(context.Background(), mount) {
			unlock()
			t.Fatal("TakeDeckRows ran while LockVolume was held")
		}
		unlock()

		// Corrupt swap snapshot body while keeping valid header + END trailer so ReadHeaderFile succeeds
		// and carryOverChecked fails inside LoadFile; TakeDeckRows must propagate that failure as false.
		swapPath := filepath.Join(dbDir(mount), rbstate.SwapName)
		oldSwap, err := os.ReadFile(swapPath)
		if err != nil {
			t.Fatal(err)
		}
		nl := bytes.IndexByte(oldSwap, '\n')
		trimmed := bytes.TrimSuffix(oldSwap, []byte("\n"))
		lastNL := bytes.LastIndexByte(trimmed, '\n')
		if nl < 0 || lastNL <= nl {
			t.Fatal("swap snapshot missing header/trailer lines")
		}
		corrupt := append(append([]byte(nil), oldSwap[:nl+1]...), []byte("{corrupt\n")...)
		corrupt = append(corrupt, oldSwap[lastNL+1:]...)
		if err := os.WriteFile(swapPath, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := rbstate.ReadHeaderFile(swapPath); err != nil {
			t.Fatalf("corrupted swap header unexpectedly rejected before carryOverChecked: %v", err)
		}
		if TakeDeckRows(context.Background(), mount) {
			t.Fatal("TakeDeckRows returned true when carryOver failed to load swap snapshot")
		}
	})

	t.Run("takeLive_preserves_nil_slices_and_new_refs", func(t *testing.T) {
		key := rbstate.FoldPath("Contents/track.mp3")
		base := rbstate.New("uuid")
		base.Tracks[key] = &rbstate.Track{Key: key, EngineID: 7, Rel: "Contents/track.mp3", Row: "old-row", Rating: 40, Perf: rbstate.PerfDeck, Written: nil, Synced: nil}
		s := rbstate.New("uuid")
		s.Tracks[key] = &rbstate.Track{Key: key, EngineID: 7, Rel: "Contents/track.mp3", Refs: []rbstate.SourceRef{{Kind: rbstate.OneLibrary, ID: 99}}, Row: "new-row", Written: []byte{1, 2}, Synced: []byte{3, 4}}
		takeLive(s, base, enginedb.MirrorChange{Tracks: []int64{7}})
		got := s.Tracks[key]
		if len(got.Refs) != 1 || got.Refs[0].ID != 99 {
			t.Fatalf("new view Refs were replaced: %+v", got.Refs)
		}
		if got.Row != "old-row" || got.Rating != 40 || got.Perf != rbstate.PerfDeck {
			t.Fatalf("base row/deck fields not restored: %+v", got)
		}
		if got.Written != nil || got.Synced != nil {
			t.Fatalf("nil slices became non-nil: Written=%#v Synced=%#v", got.Written, got.Synced)
		}
	})
}

// Finding 1.4: degraded staged track preserves PerfDeck and WriteBackCues; missing artwork does
// not mark a track broken/degraded.
func TestR2Proof_Finding1_4_DegradedClobbersPerfDeckAndSilencesWriteBack(t *testing.T) {
	t.Run("degraded_staged_preserves_perf_deck_and_writeback", func(t *testing.T) {
		mount := nextStick(t, 4)
		if res := mountNow(t, mount, MountOptions{Publish: true}); res.Applied != 4 {
			t.Fatalf("applied %d", res.Applied)
		}
		key := rbstate.FoldPath(testfixture.LargeTrackPath(1))
		s := snapshot(t, mount)
		tr := s.Tracks[key]
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		pos := 1500.0
		if err := enginedb.SetQuickCue(context.Background(), db, tr.EngineID, 0, "DeckCue", "#ff0000", &pos); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+50 WHERE id=?`, tr.EngineID); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		var dbEdit int64
		if err := db.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=?`, tr.EngineID).Scan(&dbEdit); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		_ = db.Close()

		tr.Perf = rbstate.PerfDeck
		tr.Written = nil
		tr.Synced = nil
		tr.Edit = dbEdit
		tr.Built = rbstate.TrackSig{}
		s.Pending.Perf = 1
		if err := s.Save(dbDir(mount)); err != nil {
			t.Fatal(err)
		}
		stage, err := rbstate.OpenStage(s.LibraryUUID)
		if err != nil {
			t.Fatal(err)
		}
		if err := stage.Put(&rbstate.Staged{
			Key:      key,
			EngineID: tr.EngineID,
			Sig:      rbstate.TrackSig{Rules: rbstate.Rules, Row: tr.Row},
			Degraded: "audio: bad header",
		}); err != nil {
			t.Fatal(err)
		}
		if err := stage.Close(); err != nil {
			t.Fatal(err)
		}

		mountNow(t, mount, MountOptions{Publish: true})
		after := snapshot(t, mount).Tracks[key]
		if after.Perf != rbstate.PerfDeck {
			t.Fatalf("degraded staged record clobbered PerfDeck -> %d", after.Perf)
		}
		if after.Synced != nil {
			t.Fatalf("degraded staged record falsely set Synced!=nil, silencing WriteBackCues")
		}
		wb, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
		if err != nil || wb.TracksChanged != 1 {
			t.Fatalf("WriteBackCues dropped deck cue after degraded stage: res=%+v err=%v", wb, err)
		}
	})

	t.Run("missing_artwork_does_not_degrade_track", func(t *testing.T) {
		mount := stick(t)
		r, err := compat.NewResolver(mount)
		if err != nil {
			t.Fatal(err)
		}
		pdb, err := rekordbox.ReadPDB(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"))
		if err != nil {
			t.Fatal(err)
		}
		tr := pdb.Tracks[0]
		tr.ArtworkPath = "/PIONEER/Artwork/00001/missing.jpg"
		src, err := enginedb.StatTrack(context.Background(), r, tr)
		if err != nil {
			t.Fatal(err)
		}
		if src.Broken != nil {
			t.Fatalf("missing artwork set src.Broken=%v", src.Broken)
		}
		rec, err := enginedb.BuildTrack(r, tr, src, enginedb.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if rec.Degraded != "" {
			t.Fatalf("missing artwork produced degraded record: %q", rec.Degraded)
		}
	})
}

// Finding 3.1: swapNext handles zeroed PERSIST journals and stale 0-byte WAL/SHM, refuses active
// WAL/non-zero journals, and NEVER unlinks live sidecars while OpenElsewhere(live) != 0.
func TestR2Proof_Finding3_1_StaleSidecarsOnSwap(t *testing.T) {
	t.Run("zeroed_persist_journal_does_not_block_swap", func(t *testing.T) {
		mount := nextStick(t, 8)
		makeNext(t, mount)
		j := filepath.Join(dbDir(mount), "m.db-journal")
		if err := os.WriteFile(j, make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		hookWindowNow(t, mount)
		if _, err := os.Stat(filepath.Join(dbDir(mount), nextDB)); !os.IsNotExist(err) {
			t.Fatalf("zeroed PERSIST m.db-journal blocked swapNext: %v", err)
		}
		if _, err := os.Stat(j); !os.IsNotExist(err) {
			t.Fatal("zeroed m.db-journal left behind after swap")
		}
	})

	t.Run("stale_wal_and_shm_removed_on_swap", func(t *testing.T) {
		mount := nextStick(t, 8)
		makeNext(t, mount)
		wal := filepath.Join(dbDir(mount), "m.db-wal")
		shm := filepath.Join(dbDir(mount), "m.db-shm")
		if err := os.WriteFile(wal, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shm, make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		hookWindowNow(t, mount)
		if _, err := os.Stat(filepath.Join(dbDir(mount), nextDB)); !os.IsNotExist(err) {
			t.Fatal("swap did not run with 0-byte wal + shm")
		}
		_, walErr := os.Stat(wal)
		_, shmErr := os.Stat(shm)
		if !os.IsNotExist(walErr) || !os.IsNotExist(shmErr) {
			t.Fatalf("stale sidecars left behind after swap: walExists=%v shmExists=%v", walErr == nil, shmErr == nil)
		}
	})

	t.Run("open_elsewhere_never_unlinks_live_wal_or_shm", func(t *testing.T) {
		mount := nextStick(t, 8)
		makeNext(t, mount)
		live := filepath.Join(dbDir(mount), "m.db")
		wal, shm := live+"-wal", live+"-shm"
		if err := os.WriteFile(wal, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shm, make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		release := fix3HoldOpen(t, live)
		defer release()
		if enginedb.OpenElsewhere(live) == 0 {
			t.Skip("OpenElsewhere did not detect child process fd")
		}
		_, _ = ConvertAtMount(context.Background(), mount, MountOptions{Publish: true, Deadline: time.Now().Add(hookWindow)})
		if _, err := os.Stat(filepath.Join(dbDir(mount), nextDB)); err != nil {
			t.Fatalf("swapNext swapped while OpenElsewhere != 0: %v", err)
		}
		if _, err := os.Stat(wal); os.IsNotExist(err) {
			t.Fatal("swapNext unlinked live m.db-wal while Engine held m.db open")
		}
		if _, err := os.Stat(shm); os.IsNotExist(err) {
			t.Fatal("swapNext unlinked live m.db-shm while Engine held m.db open")
		}
	})

	t.Run("active_next_wal_rejects_swap_without_removing_live_sidecars", func(t *testing.T) {
		mount := nextStick(t, 8)
		makeNext(t, mount)
		liveShm := filepath.Join(dbDir(mount), "m.db-shm")
		nextWal := filepath.Join(dbDir(mount), nextDB+"-wal")
		if err := os.WriteFile(liveShm, make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(nextWal, bytes.Repeat([]byte{0x42}, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		hookWindowNow(t, mount)
		if _, err := os.Stat(filepath.Join(dbDir(mount), nextDB)); err != nil {
			t.Fatalf("swapNext swapped despite active .rb-next.db-wal: %v", err)
		}
		if _, err := os.Stat(liveShm); os.IsNotExist(err) {
			t.Fatal("swapNext removed live m.db-shm even though swap was rejected")
		}
	})
}

// Finding 3.2: db7ffc99 in-place batch step-down fits a partial batch when all == updateBatch.
func TestR2Proof_Finding3_2_StepDownFindsFittingPartialBatch(t *testing.T) {
	mount := nextStick(t, 10)
	pdbPath := filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb")
	raw, err := os.ReadFile(pdbPath)
	if err != nil {
		t.Fatal(err)
	}
	updates := map[uint32]rekordbox.Metadata{}
	for i := uint32(1); i <= 10; i++ {
		updates[i] = rekordbox.Metadata{Comment: fmt.Sprintf("U%d", i)}
	}
	rewritten, err := rekordbox.RewritePDBMetadata(raw, updates)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pdbPath, rewritten, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(pdbPath, later, later)

	oldBatch := updateBatch
	oldFree := stickFree
	updateBatch = 10
	stickFree = func(string) (int64, bool) { return 0, true }
	defer func() {
		updateBatch = oldBatch
		stickFree = oldFree
	}()

	cost10 := (work{updates: 10, commits: 2, tracks: 10}).cost()
	cost5 := (work{updates: 5, commits: 1, tracks: 10}).cost()
	budget := (cost10 + cost5) / 2

	r, err := compat.NewResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	files, err := FindSources(r, mount)
	if err != nil {
		t.Fatal(err)
	}
	libs, _, err := ReadSources(context.Background(), r, mount, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := BuildView(libs, rbstate.LayoutFolders, "")
	if err != nil {
		t.Fatal(err)
	}
	lib, err := enginedb.OpenLibrary(context.Background(), dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	snap, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatal(err)
	}
	var res MountResult
	kill := time.Now().Add(spare + budget)
	w := &window{
		ctx: context.Background(), mount: mount, dir: dbDir(mount), view: view,
		st:  &steps{start: time.Now(), last: time.Now(), kill: kill, owed: time.Now().Add(2 * time.Second), optional: time.Now().Add(2 * time.Second)},
		res: &res, lib: lib, snap: snap,
	}
	if w.index, err = lib.TrackIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	idx := make([]int, 10)
	for i := range idx {
		idx[i] = i
	}
	if err := w.updateRows(idx); err != nil {
		t.Fatalf("updateRows: %v", err)
	}
	if res.Updated == 0 || res.Updated >= 10 {
		t.Fatalf("step-down updated %d rows, want partial 1..9 (budget=%s cost5=%s cost10=%s)", res.Updated, budget, cost5, cost10)
	}
}

// Finding 1.3 helper guard (full Finding 1.3 fix lives on agent/5306-rb-enginedb-fix):
// write-back's unflagged main-cue normalizer keeps unflagged main-cue bytes out of the digest.
func TestR2Proof_Finding1_3_AsWrittenDiscardsAndDeletesNewRekordboxCue(t *testing.T) {
	raw := make([]byte, 8+8*13+17)
	binary.BigEndian.PutUint64(raw, 8)
	off := 8 + 8*13
	raw[off+8] = 0
	copy(raw[off+9:], []byte("old-main-cue"))
	other := append([]byte(nil), raw...)
	copy(other[off+9:], []byte("new-main-cue"))
	a, err := enginedb.QCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := enginedb.QCompress(other)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(writtenDigestIgnoringMainCueTail(nil, a, nil), writtenDigestIgnoringMainCueTail(nil, b, nil)) {
		t.Fatal("unflagged main-cue bytes changed the written digest")
	}
}
