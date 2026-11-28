package enginedb

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every place that opens a SQLite database for writing is on this list, with what makes it
// follow the rule of #336 Go review R13 (hold.go), or why it is no volume's library. A new one
// fails the test until its author has decided which it is: an edit of a volume's library goes
// through HoldForEdit and OpenLibraryRW, a replacement through HoldLibrary.
var writableOpens = map[string]string{
	"pkg/enginedb/hold.go":          "the rule itself: OpenLibraryRW refuses a context without the volume's lock; HoldLibrary is the hold of a replacement",
	"pkg/enginedb/library.go":       "the bridge's library: a window's, under ConvertAtMount's volume lock, or the job's own next library",
	"pkg/enginedb/mirror.go":        "the job's own next library (.rb-next.db), a window's only under the volume lock",
	"pkg/dbhealer/db.go":            "the healer's opener: for writing only inside healMountContext, which holds the volume's lock; CheckCopy opens the hook's stage copy",
	"pkg/dbhealer/reserve.go":       "the reserve rebuild: the volume's lock and its own exclusive hold; the other open is the rebuilt copy",
	"pkg/analyzer/library.go":       "the stream download import: its caller, pkg/webui/jobs.go, takes the volume's lock around each import",
	"pkg/rekordbox/sqlite_write.go": "the plain copy of a rekordbox export on /data, not an Engine library; written by a cue write-back pass, which holds the volume's lock",
}

// lockedWriters edit a volume's library in place. Each takes the volume's lock itself and
// opens the library through OpenLibraryRW; none has a database open of its own.
var lockedWriters = []string{
	"pkg/enginedb/pending.go", "pkg/enginedb/relocate.go", "pkg/enginedb/mp3grid.go",
	"pkg/webui/hotcues.go", "pkg/webui/track_metadata.go",
}

func TestEveryWritableDatabaseOpenIsAccountedFor(t *testing.T) {
	root := filepath.Join("..", "..")
	readOnly := []string{"mode=ro", "immutable", ":memory:", "query_only", "roURI("}
	found := map[string][]int{}
	for _, top := range []string{"pkg", "internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				strings.HasPrefix(filepath.Base(filepath.Dir(path)), "test") { // internal/test*: fixtures
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			lines := strings.Split(string(b), "\n")
			for i, line := range lines {
				if !strings.Contains(line, "sql.Open(") || strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				// The data source name is on the line or built just above it.
				near := strings.Join(lines[max(0, i-24):i+1], "\n")
				ro := false
				for _, mark := range readOnly {
					ro = ro || strings.Contains(near, mark)
				}
				// An opener that can do both counts as writing.
				if !ro || strings.Contains(near, "mode=rw") || strings.Contains(near, `"mode", "rw"`) {
					rel, _ := filepath.Rel(root, path)
					found[filepath.ToSlash(rel)] = append(found[filepath.ToSlash(rel)], i+1)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var files []string
	for f := range found {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		if _, ok := writableOpens[f]; !ok {
			t.Errorf("%s opens a database for writing (line %v) and is not on the list: if it is a volume's library, edit it through HoldForEdit and OpenLibraryRW", f, found[f])
		}
	}
	for f := range writableOpens {
		if len(found[f]) == 0 {
			t.Errorf("%s is on the list and opens no database for writing any more: take it off", f)
		}
	}
	for _, f := range lockedWriters {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if !strings.Contains(src, "OpenLibraryRW(") || !strings.Contains(src, "HoldForEdit(") {
			t.Errorf("%s edits a volume's library: it must take the lock with HoldForEdit and open with OpenLibraryRW", f)
		}
		if len(found[f]) != 0 {
			t.Errorf("%s opens a database for writing on its own (line %v) besides OpenLibraryRW", f, found[f])
		}
	}
}
