package bridge

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func p4HeldReplaced(t *testing.T, path string) bool {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		if link, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && link == path+" (deleted)" {
			return true
		}
	}
	return false
}

func p4CopyTree(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		from, to := filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			p4CopyTree(t, from, to)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(to, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPort4StagedCopiesCutAtAnyOffsetAreRejected(t *testing.T) {
	cases := []struct {
		name string
		src  []byte
		dst  string
	}{
		{name: "PDB", src: testfixture.PDB(), dst: "export.pdb"},
		{name: "ANLZ", src: testfixture.DAT(), dst: "ANLZ0000.DAT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tmp := filepath.Join(dir, ".enginex-wb-1.tmp")
			dst := filepath.Join(dir, tc.dst)
			if err := os.WriteFile(dst, tc.src, 0o644); err != nil {
				t.Fatal(err)
			}
			for off := 1; off < len(tc.src); off++ {
				if err := os.WriteFile(tmp, tc.src[:off], 0o600); err != nil {
					t.Fatal(err)
				}
				if stagedWhole(tmp, dst) {
					t.Fatalf("cut at %d/%d was accepted", off, len(tc.src))
				}
			}
			if err := os.WriteFile(tmp, tc.src, 0o600); err != nil {
				t.Fatal(err)
			}
			if !stagedWhole(tmp, dst) {
				t.Fatal("whole staged copy was rejected")
			}
		})
	}
}

func TestPort4CutMarkCleanupStaysInsideMount(t *testing.T) {
	mount := t.TempDir()
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	inside := filepath.Join(mount, "PIONEER", "USBANLZ", "P001")
	outside := t.TempDir()
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mount, "PIONEER", "sym")); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, ".enginex-wb-keep.tmp")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, writeBackMarkName), []byte("PIONEER/sym\n= .enginex-wb-2.tmp PIONEER/sym/ANLZ0000.DAT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := CleanWriteBackLeftovers(mount); n != 0 {
		t.Fatalf("cleanup count=%d, want 0 for a rejected symlink path", n)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("outside file was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dbDir, writeBackMarkName)); !os.IsNotExist(err) {
		t.Fatalf("invalid mark was not cleaned: %v", err)
	}
}

func TestPort4CutPassRecoversAndConverges(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{}); err != nil {
		t.Fatalf("mount window: %v", err)
	}
	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatalf("background job: %v", err)
	}
	if res, err := ConvertAtMount(context.Background(), mount, MountOptions{Publish: true}); err != nil || (!res.Pending.None() && !res.Bypassed) {
		t.Fatalf("publishing window: %+v %v", res.Pending, err)
	}
	db := p4DB(t, mount)
	id := uint32(p4TrackID(t, db))
	if _, err := db.Exec(`UPDATE Track SET genre='cut genre' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+7 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cutDir := t.TempDir()
	var snapshots []string
	restore := SetWriteBackCutForTest(func(step string) {
		dst := filepath.Join(cutDir, string(rune('0'+len(snapshots))))
		p4CopyTree(t, mount, dst)
		snapshots = append(snapshots, step)
	})
	res, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-24", SkipPreparingCheck: true})
	restore()
	if err != nil || res.TracksChanged != 1 || len(snapshots) < 3 {
		t.Fatalf("uncut pass: %+v %v, steps=%q", res, err, snapshots)
	}
	for i, step := range snapshots {
		t.Run(strings.TrimSpace(filepath.Base(step)), func(t *testing.T) {
			cut := filepath.Join(cutDir, string(rune('0'+i)))
			for _, tmp := range p4CutTemps(t, cut) {
				st, err := os.Stat(tmp)
				if err == nil {
					if err := os.Truncate(tmp, st.Size()/2); err != nil {
						t.Fatal(err)
					}
				}
			}
			CleanWriteBackLeftovers(cut)
			if left := p4CutTemps(t, cut); len(left) != 0 {
				t.Fatalf("cut at %q left temporary files: %q", step, left)
			}
			if _, err := rekordbox.ReadPDB(filepath.Join(cut, "PIONEER", "rekordbox", "export.pdb")); err != nil {
				t.Fatalf("cut at %q left unreadable export.pdb: %v", step, err)
			}
			if n := p4ParseANLZ(t, cut); n == 0 {
				t.Fatalf("cut at %q left no readable ANLZ files", step)
			}
			if _, err := ConvertAtMount(context.Background(), cut, MountOptions{}); err != nil {
				t.Fatalf("cut at %q mount window: %v", step, err)
			}
			if _, err := FillInBackground(context.Background(), cut, FillOptions{}); err != nil {
				t.Fatalf("cut at %q background fill: %v", step, err)
			}
			if res, err := ConvertAtMount(context.Background(), cut, MountOptions{Publish: true}); err != nil || (!res.Pending.None() && !res.Bypassed) {
				t.Fatalf("cut at %q publish: %+v %v", step, res.Pending, err)
			}
			pass, err := WriteBackCues(context.Background(), cut, WriteBackOptions{SkipPreparingCheck: true})
			if err != nil || p4PDBGenre(t, cut, id) != "cut genre" {
				t.Fatalf("cut at %q did not retain the metadata edit: %+v %v", step, pass, err)
			}
			if n := p4ParseANLZ(t, cut); n == 0 {
				t.Fatalf("cut at %q left no readable ANLZ files after write-back", step)
			}
			second, err := WriteBackCues(context.Background(), cut, WriteBackOptions{SkipPreparingCheck: true})
			if err != nil || second.TracksChanged != 0 {
				t.Fatalf("cut at %q did not converge: %+v %v", step, second, err)
			}
		})
	}
}

func p4CutTemps(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if (strings.HasPrefix(name, ".enginex-") || strings.HasPrefix(name, ".bak-")) && strings.HasSuffix(name, ".tmp") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func p4PDBGenre(t *testing.T, mount string, id uint32) string {
	t.Helper()
	lib, err := rekordbox.ReadPDB(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, track := range lib.Tracks {
		if track.ID == id {
			return track.Genre
		}
	}
	t.Fatalf("export.pdb has no track %d", id)
	return ""
}

func p4ParseANLZ(t *testing.T, mount string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(filepath.Join(mount, "PIONEER", "USBANLZ"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rekordbox.ParseAnalysis(raw); err != nil {
			t.Fatalf("ANLZ %s: %v", path, err)
		}
		n++
		return nil
	})
	return n
}

func TestPort4ReplacedFilesStayHeldUntilRenameSync(t *testing.T) {
	mount := p4ImportedStick(t)
	p4DeckEdit(t, mount, "held cue", "held genre", "", "")
	seen := map[string]bool{}
	restore := SetWriteBackCutForTest(func(step string) {
		for _, prefix := range []string{"renamed ", "removed "} {
			if dst, ok := strings.CutPrefix(step, prefix); ok && !strings.Contains(dst, ".enginex-backup") {
				seen[filepath.Base(dst)] = p4HeldReplaced(t, dst)
			}
		}
	})
	res, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-24", SkipPreparingCheck: true})
	restore()
	if err != nil || res.FilesUpdated == 0 {
		t.Fatalf("write-back: %+v %v", res, err)
	}
	for _, name := range []string{"ANLZ0000.DAT", "ANLZ0000.EXT", "export.pdb"} {
		if !seen[name] {
			t.Errorf("%s was not held through its synced replacement: %v", name, seen)
		}
	}
	for name := range seen {
		for _, dir := range []string{
			filepath.Join(mount, "PIONEER", "USBANLZ", "P001"),
			filepath.Join(mount, "PIONEER", "rekordbox"),
			filepath.Join(mount, "Engine Library", "Database2"),
		} {
			if p4HeldReplaced(t, filepath.Join(dir, name)) {
				t.Fatalf("%s stayed open after the pass", name)
			}
		}
	}
}

func TestPort4BackupDirectoriesExistBeforeWriteBackMark(t *testing.T) {
	mount := p4ImportedStick(t)
	p4DeckEdit(t, mount, "mark cue", "", "", "")
	want := []string{
		filepath.Join(mount, ".enginex-backup", "2026-11-24", "PIONEER", "USBANLZ", "P001"),
		filepath.Join(mount, ".enginex-backup", "2026-11-24", "PIONEER", "rekordbox"),
	}
	var missing []string
	restore := SetWriteBackCutForTest(func(step string) {
		if step != "temp "+filepath.Join(mount, "Engine Library", "Database2", writeBackMarkName) {
			return
		}
		for _, dir := range want {
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				missing = append(missing, dir)
			}
		}
	})
	res, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-24", SkipPreparingCheck: true})
	restore()
	if err != nil || res.FilesUpdated == 0 {
		t.Fatalf("write-back: %+v %v", res, err)
	}
	if len(missing) != 0 {
		t.Fatalf("backup directories missing when mark was staged: %v", missing)
	}
}
