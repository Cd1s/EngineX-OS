package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #321 review R4 (delta review 3, P1-1): a staged copy is put back only when it is whole. A copy of
// exportLibrary.db cut inside its last page, or by a whole page when its header leaves the page
// count to the file's size, still read; so did an export.pdb cut by the page after its tables'
// chains that its header names. Every 512-byte cut of each must be refused, and the whole file
// taken.
func TestIssue321AStagedCopyCutAnywhereIsNotPutBack(t *testing.T) {
	t.Parallel()
	plain, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary_decrypted.db"))
	if err != nil {
		t.Fatal(err)
	}
	// With a table the export's reader does not read at its end (rekordbox's djmdHistory), and a
	// legacy header (no page count: the file's size gives it), a copy cut by that table's last page
	// still read.
	noCount := issue321WithHistory(t, plain)
	binary.BigEndian.PutUint32(noCount[28:], 0)
	pageSize := int(binary.BigEndian.Uint16(noCount[16:]))
	pdb := testfixture.PDB()
	pages := uint32(len(pdb) / testfixture.PageSize)
	pdb = append(pdb, make([]byte, testfixture.PageSize)...) // a trailing empty candidate page
	binary.LittleEndian.PutUint32(pdb[12:], pages+1)         // the next unused page
	binary.LittleEndian.PutUint32(pdb[28+4:], pages)         // table 0's empty candidate
	for _, tc := range []struct {
		name, dst string
		b         []byte
		cuts      []int
	}{
		{"exportLibrary.db, every 512-byte cut", "exportLibrary.db", plain, nil},
		{"exportLibrary.db without its page count, a whole page cut", "exportLibrary.db", noCount, []int{len(noCount) - pageSize}},
		{"export.pdb with a trailing empty candidate page, every 512-byte cut", "export.pdb", pdb, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, tc.dst)
			tmp := filepath.Join(dir, ".enginex-wb-1.tmp")
			if err := os.WriteFile(tmp, tc.b, 0o644); err != nil {
				t.Fatal(err)
			}
			if !bridge.StagedWholeForTest(tmp, dst) {
				t.Fatalf("the whole %s (%d bytes) is not taken", tc.dst, len(tc.b))
			}
			cuts := tc.cuts
			if cuts == nil {
				for off := 512; off < len(tc.b); off += 512 {
					cuts = append(cuts, off)
				}
			}
			var taken []int
			for _, off := range cuts {
				if err := os.WriteFile(tmp, tc.b[:off], 0o644); err != nil {
					t.Fatal(err)
				}
				if bridge.StagedWholeForTest(tmp, dst) {
					taken = append(taken, off)
				}
			}
			if len(taken) > 0 {
				t.Errorf("%d of %d cut copies of %s would be put back, cut at %v", len(taken), len(cuts), tc.dst, taken)
			}
		})
	}
}

// issue321WithHistory is the SQLite database plain with a table no export reader reads, filled
// over several pages after the others.
func issue321WithHistory(t *testing.T, plain []byte) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	if err := os.WriteFile(path, plain, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE djmdHistory(id INTEGER PRIMARY KEY, note TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err = db.Exec(`INSERT INTO djmdHistory(note) VALUES(?)`, strings.Repeat("h", 100)); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) <= len(plain) {
		t.Fatalf("the table took no page: %d bytes", len(b))
	}
	return b
}

// #321 review R4 (P2-1): when the next insert puts the export back from its first original, a
// later pass must write the deck's edits into it again. The pass compared the deck's cues with the
// ANLZ files only, and those already held them, so a OneLibrary export put back from its original
// kept the original's cues for good (rekordbox showed them, a CDJ reading .EXT the deck's).
func TestIssue321AnExportPutBackFromItsOriginalIsWrittenAgain(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatal(err)
	}
	opts := func() bridge.WriteBackOptions {
		return bridge.WriteBackOptions{DateDir: "2026-11-22", Session: &bridge.CueSession{}, Keys: testKeys, Settled: func() error { return nil }, SkipPreparingCheck: true}
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(stick, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	issue321Settle()
	issue321Edit(t, db, 1, "First 321", 1000)
	issue321Settle()
	if res, err := bridge.WriteBackCues(ctx, stick, opts()); err != nil || res.FilesUpdated == 0 {
		t.Fatalf("first pass: %+v %v", res, err)
	}
	issue321Settle()
	issue321Edit(t, db, 1, "Second 321", 2000)
	issue321Settle()

	// The second pass is pulled while it replaces exportLibrary.db (after the ANLZ files): on exFAT
	// the export's entry is gone, and its staged copy's with it.
	rekordboxDir := filepath.Join("PIONEER", "rekordbox")
	cut := t.TempDir()
	restore := bridge.SetWriteBackCutForTest(func(step string) {
		if step == "replacing "+filepath.Join(stick, rekordboxDir, "exportLibrary.db") {
			issue321Copy(t, stick, cut)
		}
	})
	res, err := bridge.WriteBackCues(ctx, stick, opts())
	restore()
	if err != nil || res.FilesUpdated == 0 {
		t.Fatalf("second pass: %+v %v", res, err)
	}
	if _, err = os.Stat(filepath.Join(cut, "Engine Library")); err != nil {
		t.Fatalf("the pass did not replace exportLibrary.db: %v", err)
	}
	gone, _ := filepath.Glob(filepath.Join(cut, rekordboxDir, ".enginex-*.tmp"))
	for _, f := range append(gone, filepath.Join(cut, rekordboxDir, "exportLibrary.db")) {
		if err = os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}

	// The next insert puts the first original back; the next pass writes the deck's cues into it.
	bridge.CleanWriteBackLeftovers(cut)
	export := filepath.Join(cut, rekordboxDir, "exportLibrary.db")
	if _, err = os.Stat(export); err != nil {
		t.Fatalf("exportLibrary.db not put back: %v", err)
	}
	if res, err = bridge.WriteBackCues(ctx, cut, opts()); err != nil {
		t.Fatalf("the pass after the insert: %+v %v", res, err)
	}
	l, err := rekordbox.ReadSQLiteKeys(ctx, export, nil, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tr := range l.Tracks {
		for _, c := range tr.Cues {
			found = found || c.Name == "Second 321"
		}
	}
	if !found {
		t.Errorf("exportLibrary.db does not have the deck's hot cue after the pass (%+v): rekordbox and the ANLZ files disagree", res)
	}
	if again, err := os.Stat(filepath.Join(cut, "Engine Library", "Database2", ".rb-bridge-writeback-again")); err == nil {
		t.Errorf("the mark that asks for the pass is still there after it: %v", again.Name())
	}
	if res, err = bridge.WriteBackCues(ctx, cut, opts()); err != nil || res.FilesUpdated != 0 {
		t.Errorf("the pass after that: %+v %v, want nothing written", res, err)
	}
}

// #321 review R4 (P2-3): a mark line is a path inside the mount. On a writable HFS+ stick a
// directory can be a symbolic link; a line through one must not lead the clean-up or the put-back
// outside the mount.
func TestIssue321AMarkLineThroughASymbolicLinkStaysInsideTheMount(t *testing.T) {
	t.Parallel()
	stick, outside := t.TempDir(), t.TempDir()
	db2 := filepath.Join(stick, "Engine Library", "Database2")
	for _, d := range []string{db2, filepath.Join(stick, "PIONEER")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(stick, "PIONEER", "sym")); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{".enginex-wb-1.tmp": []byte("someone else's"), ".enginex-wb-2.tmp": testfixture.DAT()}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(outside, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A directory to clean through the link, then a file to put back through it.
	for _, mark := range []string{"PIONEER/sym\n", "= .enginex-wb-2.tmp PIONEER/sym/ANLZ0000.DAT\n"} {
		if err := os.WriteFile(filepath.Join(db2, ".rb-bridge-writeback"), []byte(mark), 0o644); err != nil {
			t.Fatal(err)
		}
		bridge.CleanWriteBackLeftovers(stick)
	}
	for name, b := range files {
		if got, err := os.ReadFile(filepath.Join(outside, name)); err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s outside the mount: %v, changed %v", name, err, !bytes.Equal(got, b))
		}
	}
	if _, err := os.Lstat(filepath.Join(outside, "ANLZ0000.DAT")); err == nil {
		t.Error("a file was put back outside the mount")
	}
	if left, _ := os.ReadDir(outside); len(left) != len(files) {
		var names []string
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Errorf("outside the mount: %s", strings.Join(names, ", "))
	}
}
