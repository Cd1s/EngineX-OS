package bridge_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #321: the watcher's pass is cut at every step of its writes, as a pull leaves the stick (a
// temporary file is also cut in half). At the next mount the stick's export decrypts and passes
// the integrity check, every ANLZ file parses, no temporary file is left, and the next pass
// brings the edit to the stick and then finds nothing to do.

func issue321Copy(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		o, err := os.Create(out)
		if err != nil {
			return err
		}
		defer o.Close()
		_, err = io.Copy(o, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func issue321Temps(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".tmp") && (strings.HasPrefix(d.Name(), ".enginex-") || strings.HasPrefix(d.Name(), ".bak-")) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// issue321Sound is the exportLibrary.db files that decrypted and passed the integrity check: the
// check is a function of the file's bytes, so one that was seen whole is not decrypted again.
var (
	issue321SoundMu sync.Mutex
	issue321Sound   = map[[sha256.Size]byte]bool{}
)

func issue321ExportSound(t *testing.T, stick string) {
	t.Helper()
	p := filepath.Join(stick, "PIONEER", "rekordbox", "exportLibrary.db")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	issue321SoundMu.Lock()
	seen := issue321Sound[sum]
	issue321SoundMu.Unlock()
	if !seen {
		var plain bytes.Buffer
		if err = rekordbox.DecryptSQLCipher4(context.Background(), bytes.NewReader(raw), int64(len(raw)), &plain, []byte(oneLibraryKey)); err != nil {
			t.Fatalf("exportLibrary.db does not decrypt: %v", err)
		}
		f := filepath.Join(t.TempDir(), "plain.db")
		if err = os.WriteFile(f, plain.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: f, RawQuery: "mode=ro"}).String())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var ok string
		if err = db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
			t.Fatalf("exportLibrary.db integrity: %q %v", ok, err)
		}
		issue321SoundMu.Lock()
		issue321Sound[sum] = true
		issue321SoundMu.Unlock()
	}
	if _, err = rekordbox.ReadSQLiteKeys(context.Background(), p, nil, testKeys); err != nil {
		t.Fatalf("exportLibrary.db does not read: %v", err)
	}
}

func issue321ANLZSound(t *testing.T, stick string) {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(filepath.Join(stick, "PIONEER", "USBANLZ"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = rekordbox.ParseAnalysis(b); err != nil {
			t.Fatalf("%s does not parse: %v", p, err)
		}
		n++
		return nil
	})
	if n == 0 {
		t.Fatal("no ANLZ file")
	}
}

func TestIssue321WatcherPassCutAtEveryWriteStep(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := bridge.ConvertAtMount(ctx, stick, enginedb.Options{}, true); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(stick, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	issue321Settle()
	issue321Edit(t, db, 1, "Cut 321", 1000)
	db.Close()
	issue321Settle()

	cuts := t.TempDir()
	var steps []string
	restore := bridge.SetWriteBackCutForTest(func(step string) {
		dst := filepath.Join(cuts, strings.Repeat("0", 3-len(itoa(len(steps))))+itoa(len(steps)))
		issue321Copy(t, stick, dst)
		steps = append(steps, step)
	})
	var s bridge.CueSession
	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{DateDir: "2026-11-22", Session: &s, Keys: testKeys,
		Settled: func() error { return nil }})
	restore()
	if err != nil || res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("uncut pass: %+v %v", res, err)
	}
	// marker, per ANLZ file backup + file, export backup + file, stamp, state.
	if len(steps) < 10 {
		t.Fatalf("only %d write steps: %q", len(steps), steps)
	}
	for i, step := range steps {
		cut := filepath.Join(cuts, strings.Repeat("0", 3-len(itoa(i)))+itoa(i))
		t.Run(itoa(i)+" "+filepath.Base(step), func(t *testing.T) {
			t.Parallel()                                // its own copy of the cut stick; the cut hook was put back above
			for _, tmp := range issue321Temps(t, cut) { // a pull in the middle of writing it
				if st, err := os.Stat(tmp); err == nil {
					_ = os.Truncate(tmp, st.Size()/2)
				}
			}
			bridge.CleanWriteBackLeftovers(cut) // the next mount
			if left := issue321Temps(t, cut); len(left) != 0 {
				t.Fatalf("cut at %q: left after the next mount: %q", step, left)
			}
			issue321ExportSound(t, cut)
			issue321ANLZSound(t, cut)
			res, err := bridge.WriteBackCues(ctx, cut, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true})
			if err != nil || !issue321ANLZHas(t, cut, "Cut 321") {
				t.Fatalf("cut at %q: the next pass did not bring the edit: %+v %v", step, res, err)
			}
			issue321ExportSound(t, cut)
			issue321ANLZSound(t, cut)
			if res, err = bridge.WriteBackCues(ctx, cut, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true}); err != nil || res.TracksChanged != 0 {
				t.Fatalf("cut at %q: the pass after does not converge: %+v %v", step, res, err)
			}
		})
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// issue321PDBGenre is track id's genre in the stick's export.pdb, which must read whole.
func issue321PDBGenre(t *testing.T, stick string, id uint32) string {
	t.Helper()
	lib, err := rekordbox.ReadPDB(filepath.Join(stick, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil {
		t.Fatalf("export.pdb does not read: %v", err)
	}
	if len(lib.Tracks) != 2 {
		t.Fatalf("export.pdb holds %d tracks, want 2", len(lib.Tracks))
	}
	for _, tr := range lib.Tracks {
		if tr.ID == id {
			return tr.Genre
		}
	}
	t.Fatalf("export.pdb has no track %d", id)
	return ""
}

// #321 review R2 (P2-4): a stick with no OneLibrary export carries a deck's metadata edit in
// export.pdb (#314). The pass that writes it is cut at every write step as above: export.pdb reads
// whole with the old or the new genre, and the next pass brings both edits.
func TestIssue321WatcherPassCutAtEveryWriteStepOfExportPDB(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	stick := setupWriteBackStick(t, false)
	if _, err := bridge.ConvertAtMount(ctx, stick, enginedb.Options{}, true); err != nil {
		t.Fatal(err)
	}
	oldGenre := issue321PDBGenre(t, stick, 1)
	const newGenre = "Cut Genre 321"
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(stick, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	issue321Settle()
	issue321Edit(t, db, 1, "Cut 321", 1000)
	if _, err = db.Exec(`UPDATE Track SET genre=? WHERE pdbImportKey=1`, newGenre); err != nil {
		t.Fatal(err)
	}
	db.Close()
	issue321Settle()

	cuts := t.TempDir()
	var steps []string
	restore := bridge.SetWriteBackCutForTest(func(step string) {
		dst := filepath.Join(cuts, strings.Repeat("0", 3-len(itoa(len(steps))))+itoa(len(steps)))
		issue321Copy(t, stick, dst)
		steps = append(steps, step)
	})
	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{DateDir: "2026-11-22", Session: &bridge.CueSession{},
		Keys: testKeys, Settled: func() error { return nil }})
	restore()
	if err != nil || res.TracksChanged != 1 || res.OneLibraryUpdated || issue321PDBGenre(t, stick, 1) != newGenre {
		t.Fatalf("uncut pass: %+v %v", res, err)
	}
	pdbSteps := 0
	for _, step := range steps {
		if strings.HasSuffix(step, "export.pdb") {
			pdbSteps++
		}
	}
	// its backup temp and backup, its temp, replacing and renamed.
	if pdbSteps < 5 {
		t.Fatalf("export.pdb is in %d of the pass's write steps: %q", pdbSteps, steps)
	}
	for i, step := range steps {
		cut := filepath.Join(cuts, strings.Repeat("0", 3-len(itoa(i)))+itoa(i))
		t.Run(itoa(i)+" "+filepath.Base(step), func(t *testing.T) {
			t.Parallel()                                // its own copy of the cut stick; the cut hook was put back above
			for _, tmp := range issue321Temps(t, cut) { // a pull in the middle of writing it
				if st, err := os.Stat(tmp); err == nil {
					_ = os.Truncate(tmp, st.Size()/2)
				}
			}
			bridge.CleanWriteBackLeftovers(cut) // the next mount
			if left := issue321Temps(t, cut); len(left) != 0 {
				t.Fatalf("cut at %q: left after the next mount: %q", step, left)
			}
			if g := issue321PDBGenre(t, cut, 1); g != oldGenre && g != newGenre {
				t.Fatalf("cut at %q: export.pdb's genre is %q, neither the old %q nor the new", step, g, oldGenre)
			}
			issue321ANLZSound(t, cut)
			res, err := bridge.WriteBackCues(ctx, cut, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true})
			if err != nil || !issue321ANLZHas(t, cut, "Cut 321") || issue321PDBGenre(t, cut, 1) != newGenre {
				t.Fatalf("cut at %q: the next pass did not bring the edits: %+v %v", step, res, err)
			}
			issue321ANLZSound(t, cut)
			if res, err = bridge.WriteBackCues(ctx, cut, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true}); err != nil || res.TracksChanged != 0 {
				t.Fatalf("cut at %q: the pass after does not converge: %+v %v", step, res, err)
			}
		})
	}
}

// #321 review R3 (P2-1): exFAT renames a file over another in two directory sectors (the target's
// entry removed, the staged copy's rewritten under the target's name), so a pull between them
// left the target missing, and the next mount's cleanup removed the only new copy. At every rename
// of a pass, on a OneLibrary stick and on an export.pdb one, the states such a pull leaves are made:
// the target gone and its staged copy whole; or without its name (its own entries straddled the
// two sectors: the replay on a real exFAT volume found it under a cut name); or cut in half (a copy
// cut while written: the pass never renames before every copy is synced, so this does not happen,
// but the cleanup must not trust it). The next mount puts the target back, the staged copy or else
// the first original kept under .enginex-backup, and leaves no temporary file; the next pass
// brings the edits.
func TestIssue321ARenameCutThatLeftATargetMissingIsPutBack(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	for _, oneLib := range []bool{true, false} {
		name := "export.pdb"
		if oneLib {
			name = "OneLibrary"
		}
		t.Run(name, func(t *testing.T) { issue321MissingTarget(t, oneLib) })
	}
}

func issue321MissingTarget(t *testing.T, oneLib bool) {
	ctx := context.Background()
	stick := setupWriteBackStick(t, oneLib)
	if _, err := bridge.ConvertAtMount(ctx, stick, enginedb.Options{}, true); err != nil {
		t.Fatal(err)
	}
	const cue, genre = "Missing 321", "Missing Genre 321"
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(stick, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	issue321Settle()
	issue321Edit(t, db, 1, cue, 1000)
	if _, err = db.Exec(`UPDATE Track SET genre=? WHERE pdbImportKey=1`, genre); err != nil {
		t.Fatal(err)
	}
	db.Close()
	issue321Settle()
	read := func(t *testing.T, root, rel string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return b
	}

	cuts := t.TempDir()
	type state struct{ dst, staged string } // relative to the stick
	var states []state
	old := map[string][]byte{}
	seen := map[string]bool{}
	staged := map[string]string{}
	restore := bridge.SetWriteBackCutForTest(func(step string) {
		if dst, ok := strings.CutPrefix(step, "temp "); ok { // the copy just staged for dst
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dst), ".enginex-*.tmp"))
			for _, m := range matches {
				if !seen[m] {
					seen[m], staged[dst] = true, m
				}
			}
		}
		dst, ok := strings.CutPrefix(step, "replacing ")
		rel, _ := filepath.Rel(stick, dst)
		if !ok || staged[dst] == "" || !strings.HasPrefix(rel, "PIONEER") {
			return
		}
		cut := filepath.Join(cuts, itoa(len(states)))
		issue321Copy(t, stick, cut)
		st := state{dst: rel}
		st.staged, _ = filepath.Rel(stick, staged[dst])
		// exFAT's first sector of the rename: the target's entry gone, the staged copy's not renamed.
		if err := os.Rename(filepath.Join(cut, rel), filepath.Join(cut, st.staged)); err != nil {
			t.Fatal(err)
		}
		states = append(states, st)
	})
	for _, rel := range []string{"PIONEER/USBANLZ/P001/ANLZ0000.DAT", "PIONEER/USBANLZ/P001/ANLZ0000.EXT", "PIONEER/USBANLZ/P001/ANLZ0000.2EX", "PIONEER/rekordbox/exportLibrary.db", "PIONEER/rekordbox/export.pdb"} {
		if b, err := os.ReadFile(filepath.Join(stick, rel)); err == nil {
			old[filepath.FromSlash(rel)] = b
		}
	}
	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{DateDir: "2026-11-22", Session: &bridge.CueSession{},
		Keys: testKeys, Settled: func() error { return nil }})
	restore()
	if err != nil || res.TracksChanged != 1 || res.OneLibraryUpdated != oneLib {
		t.Fatalf("uncut pass: %+v %v", res, err)
	}
	// The three ANLZ files and the export (exportLibrary.db or export.pdb).
	if len(states) < 3 {
		t.Fatalf("only %d renames: %+v", len(states), states)
	}
	for i, st := range states {
		for _, copyState := range []string{"whole", "without its name", "cut in half", "whole, beside an earlier cut's copy"} {
			t.Run(itoa(i)+" "+filepath.Base(st.dst)+" "+copyState, func(t *testing.T) {
				t.Parallel() // its own copy of the cut stick; the cut hook was put back above
				cut := t.TempDir()
				issue321Copy(t, filepath.Join(cuts, itoa(i)), cut)
				want := read(t, stick, st.dst) // the new copy, else the first original
				tmp := filepath.Join(cut, st.staged)
				switch copyState {
				case "without its name":
					if err := os.Rename(tmp, filepath.Join(filepath.Dir(tmp), ".enginex-wb-")); err != nil {
						t.Fatal(err)
					}
					want = old[st.dst]
				case "whole, beside an earlier cut's copy":
					// Left by an earlier cut pass (one removeFile would not free, or a pass whose
					// put-back failed): of the same kind, named to sort first, newer, and holding the
					// original. The mark names this pass's copy, and only that one is put back.
					earlier := tmp[:strings.LastIndex(tmp, "-")] + "-0.tmp"
					if err := os.WriteFile(earlier, old[st.dst], 0o644); err != nil {
						t.Fatal(err)
					}
					later := time.Now().Add(time.Hour)
					if err := os.Chtimes(earlier, later, later); err != nil {
						t.Fatal(err)
					}
				case "cut in half":
					info, err := os.Stat(tmp)
					if err != nil {
						t.Fatal(err)
					}
					if err = os.Truncate(tmp, info.Size()/2); err != nil {
						t.Fatal(err)
					}
					want = old[st.dst]
				}
				bridge.CleanWriteBackLeftovers(cut) // the next mount
				if got := read(t, cut, st.dst); !bytes.Equal(got, want) {
					t.Fatalf("%s after the next mount: %d bytes, not the %d of the copy it should have put back", st.dst, len(got), len(want))
				}
				if left := issue321Temps(t, cut); len(left) != 0 {
					t.Fatalf("left after the next mount: %q", left)
				}
				issue321ANLZSound(t, cut)
				if oneLib {
					issue321ExportSound(t, cut)
				} else {
					issue321PDBGenre(t, cut, 1)
				}
				res, err := bridge.WriteBackCues(ctx, cut, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true})
				if err != nil || !issue321ANLZHas(t, cut, cue) || (!oneLib && issue321PDBGenre(t, cut, 1) != genre) {
					t.Fatalf("the next pass did not bring the edits: %+v %v", res, err)
				}
				if oneLib {
					issue321ExportSound(t, cut)
				}
				if res, err = bridge.WriteBackCues(ctx, cut, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true}); err != nil || res.TracksChanged != 0 {
					t.Fatalf("the pass after does not converge: %+v %v", res, err)
				}
			})
		}
	}
}

// #321 review R4 (N-1): a pull while a first pass made its backup directories on FAT32 left one
// whose cluster the FAT did not give it; reading it later (the next insert's clean-up, the next
// pass's backup lookup) turned vfat read-only. They are now made before the pass's mark, each on
// the stick before the entry that names it (mkdirOrdered; the order itself is measured by the
// replay on a real FAT32 volume).
func TestIssue321BackupDirectoriesAreMadeBeforeTheMark(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := bridge.ConvertAtMount(ctx, stick, enginedb.Options{}, true); err != nil {
		t.Fatal(err)
	}
	injectDeckCueEdit(t, stick)
	dirs := []string{filepath.Join(".enginex-backup", "2026-11-22", "PIONEER", "USBANLZ", "P001"), filepath.Join(".enginex-backup", "2026-11-22", "PIONEER", "rekordbox")}
	var atMark []string
	restore := bridge.SetWriteBackCutForTest(func(step string) {
		if step != "temp "+filepath.Join(stick, "Engine Library", "Database2", ".rb-bridge-writeback") {
			return
		}
		for _, d := range dirs {
			if st, err := os.Stat(filepath.Join(stick, d)); err != nil || !st.IsDir() {
				atMark = append(atMark, d)
			}
		}
		atMark = append(atMark, "") // the mark was written
	})
	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true})
	restore()
	if err != nil || res.FilesUpdated == 0 {
		t.Fatalf("pass: %+v %v", res, err)
	}
	if len(atMark) != 1 {
		t.Fatalf("when the pass wrote its mark, these backup directories were not made yet: %q (or no mark: %d)", atMark, len(atMark))
	}
}
