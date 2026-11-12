package rekordbox

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"
)

// r2 review: the cue import loaded every cue row as a map of all its columns
// (375 MB VmHWM on armhf for 20000 tracks x 8 cues). It must stream only the
// columns it uses, so a wide unused column costs nothing.
func TestOneLibraryCueImportReadsOnlyUsedColumns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "exportLibrary.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT);
CREATE TABLE cue(cue_id INTEGER PRIMARY KEY, content_id INTEGER, kind INTEGER, inUsec INTEGER, outUsec INTEGER, cueComment TEXT, unusedPayload BLOB);
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<200) INSERT INTO content SELECT i, 'T'||i, '/Contents/t'||i||'.mp3' FROM n;
WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<19999) INSERT INTO cue SELECT i+1, i%200+1, (i/200)%9, (i/200)*1000000+500000, -1, 'c'||i, zeroblob(4096) FROM n;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	l, err := ReadSQLite(context.Background(), p, nil)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 40<<20 {
		t.Fatalf("cue import allocated %d MiB for 80 MiB of unused cue payload", got>>20)
	}
	cues := 0
	for _, tr := range l.Tracks {
		cues += len(tr.Cues)
		if tr.ID == 1 && (len(tr.Cues) == 0 || tr.Cues[0].TimeMS != 500) {
			t.Fatalf("track 1 cues: %+v", tr.Cues)
		}
	}
	if cues == 0 {
		t.Fatal("no cues imported")
	}
}
