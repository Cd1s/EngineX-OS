package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func issue321ExportSound(t *testing.T, stick string) {
	t.Helper()
	p := filepath.Join(stick, "PIONEER", "rekordbox", "exportLibrary.db")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err = rekordbox.ReadSQLite(context.Background(), p, nil); err != nil {
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
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := bridge.Convert(ctx, stick, enginedb.Options{}); err != nil {
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
	keys := rekordbox.NewKeyCache()
	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{DateDir: "2026-11-22", Session: &s, Keys: keys,
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
			res, err := bridge.WriteBackCues(ctx, cut, bridge.WriteBackOptions{DateDir: "2026-11-22", SkipPreparingCheck: true})
			if err != nil || !issue321ANLZHas(t, cut, "Cut 321") {
				t.Fatalf("cut at %q: the next pass did not bring the edit: %+v %v", step, res, err)
			}
			issue321ExportSound(t, cut)
			issue321ANLZSound(t, cut)
			if res, err = bridge.WriteBackCues(ctx, cut, bridge.WriteBackOptions{DateDir: "2026-11-22", SkipPreparingCheck: true}); err != nil || res.TracksChanged != 0 {
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
	ctx := context.Background()
	stick := setupWriteBackStick(t, false)
	if _, err := bridge.Convert(ctx, stick, enginedb.Options{}); err != nil {
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
		Keys: rekordbox.NewKeyCache(), Settled: func() error { return nil }})
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
			res, err := bridge.WriteBackCues(ctx, cut, bridge.WriteBackOptions{DateDir: "2026-11-22", SkipPreparingCheck: true})
			if err != nil || !issue321ANLZHas(t, cut, "Cut 321") || issue321PDBGenre(t, cut, 1) != newGenre {
				t.Fatalf("cut at %q: the next pass did not bring the edits: %+v %v", step, res, err)
			}
			issue321ANLZSound(t, cut)
			if res, err = bridge.WriteBackCues(ctx, cut, bridge.WriteBackOptions{DateDir: "2026-11-22", SkipPreparingCheck: true}); err != nil || res.TracksChanged != 0 {
				t.Fatalf("cut at %q: the pass after does not converge: %+v %v", step, res, err)
			}
		})
	}
}
