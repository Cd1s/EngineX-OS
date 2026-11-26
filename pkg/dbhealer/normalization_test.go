package dbhealer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func TestHealMountDataOnlyNormalizesMissingNFDPaths(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	write := func(rel string) int64 {
		p := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		const size = 32
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		return size
	}
	cases := []struct {
		id       int
		path     string
		wantPath string
	}{
		{1, "../Music/Cafe\u0301.mp3", "../Music/Café.mp3"},
		{2, "../Music/Café-control.mp3", "../Music/Café-control.mp3"},
		{3, "../Music/Cafe\u0301-nfd.mp3", "../Music/Cafe\u0301-nfd.mp3"},
		{4, "../Music/Beyonce\u0301/한글.mp3", "../Music/Beyoncé/한글.mp3"},
	}
	for _, tc := range cases {
		if tc.id == 3 {
			write(tc.path)
		} else {
			write(tc.wantPath)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`, tc.id, tc.path, filepath.Base(tc.path), 32); err != nil {
			t.Fatal(err)
		}
	}
	collisionPath := "../Music/Collisione\u0301.mp3"
	collisionNFC := "../Music/Collisioné.mp3"
	write(collisionNFC)
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`, 6, collisionPath, filepath.Base(collisionPath), 32); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`, 7, collisionNFC, filepath.Base(collisionNFC), 32); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER collision_guard_probe BEFORE UPDATE OF path ON Track WHEN OLD.id=6 AND NEW.path='../Music/Collisioné.mp3' BEGIN SELECT RAISE(ABORT,'collision guard probe'); END`); err != nil {
		t.Fatal(err)
	}
	duplicateA := "../Music/Doublee\u0301\u0323.mp3"
	duplicateB := "../Music/Doublee\u0323\u0301.mp3"
	duplicateNFC := "../Music/Doubleẹ́.mp3"
	write(duplicateNFC)
	for _, item := range []struct {
		id   int
		path string
	}{{100, duplicateA}, {101, duplicateB}} {
		id, path := item.id, item.path
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`, id, path, filepath.Base(path), 32); err != nil {
			t.Fatal(err)
		}
	}
	bridgePath := "../Music/Bridge\u0301.mp3"
	bridgeNFC := "../Music/Bridgé.mp3"
	write(bridgeNFC)
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`, 102, bridgePath, filepath.Base(bridgePath), 32); err != nil {
		t.Fatal(err)
	}
	streamingPath := "../Music/Cafe\u0301-stream.mp3"
	write("../Music/Café-stream.mp3")
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,streamingSource) VALUES(103,?,?,?)`, streamingPath, filepath.Base(streamingPath), "tidal"); err != nil {
		t.Fatal(err)
	}
	symlinkPath := "../Music/Sym linke\u0301.mp3"
	symlinkNFC := "../Music/Sym linké.mp3"
	write("../Music/symlink-target.mp3")
	if err := os.Symlink("symlink-target.mp3", filepath.Join(root, "Engine Library", filepath.FromSlash(symlinkNFC))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(104,?,?,1)`, symlinkPath, filepath.Base(symlinkPath)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	state, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Exec(`CREATE TABLE BridgeState(path TEXT PRIMARY KEY, trackId INTEGER NOT NULL, fingerprint TEXT NOT NULL); INSERT INTO BridgeState VALUES('../Music/Bridgé.mp3',102,'test')`); err != nil {
		t.Fatal(err)
	}
	state.Close()

	got, err := HealMountDataOnly(root, 0)
	if err != nil {
		t.Fatalf("heal: %+v %v", got, err)
	}
	if got.PathsNormalized != 3 {
		t.Fatalf("paths normalized=%d, want 3: %+v", got.PathsNormalized, got)
	}
	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var path string
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=6`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != collisionPath {
		t.Fatalf("collision row changed to %q", path)
	}
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=102`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != bridgePath {
		t.Fatalf("bridge-managed row changed to %q", path)
	}
	unchanged, normalized := 0, 0
	for id, want := range map[int]string{100: duplicateA, 101: duplicateB} {
		if err := check.QueryRow(`SELECT path FROM Track WHERE id=?`, id).Scan(&path); err != nil {
			t.Fatal(err)
		}
		if path == want {
			unchanged++
		} else if path == duplicateNFC {
			normalized++
		} else {
			t.Fatalf("duplicate collision row %d changed to %q", id, path)
		}
	}
	if unchanged != 1 || normalized != 1 {
		t.Fatalf("duplicate collision result unchanged=%d normalized=%d", unchanged, normalized)
	}
	for _, tc := range cases {
		var path, filename string
		if err := check.QueryRow(`SELECT path,filename FROM Track WHERE id=?`, tc.id).Scan(&path, &filename); err != nil {
			t.Fatal(err)
		}
		if path != tc.wantPath || filename != filepath.Base(tc.wantPath) {
			t.Errorf("track %d path=%q filename=%q, want %q/%q", tc.id, path, filename, tc.wantPath, filepath.Base(tc.wantPath))
		}
	}
	var gotStreamingPath string
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=103`).Scan(&gotStreamingPath); err != nil {
		t.Fatal(err)
	}
	if norm.NFC.String(gotStreamingPath) == gotStreamingPath {
		t.Fatal("streaming path unexpectedly normalized")
	}
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=104`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != symlinkPath {
		t.Fatalf("symlink target was treated as a regular file: %q", path)
	}
}

func TestHealMountDataOnlyNormalizesUnicodePathAndFilename(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	write := func(rel string) {
		path := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name    string
		changed bool
	}{
		{name: "Tiếng Việt", changed: true},
		{name: "한글 노래", changed: true},
		{name: "ガラスの靴", changed: true},
		{name: "Йошкар", changed: true},
		{name: "Ελλάδα", changed: true},
		{name: "أغنية", changed: true},
		{name: "İstanbul", changed: true},
		{name: "Łódź", changed: true},
		{name: "Café", changed: true},
		{name: "中文歌曲", changed: false},
		{name: "เพลงไทย", changed: false},
		{name: "さくら", changed: false},
		{name: "English", changed: false},
	}
	for i, tc := range cases {
		nfdName := norm.NFD.String(tc.name)
		if tc.changed == (nfdName == tc.name) {
			t.Fatalf("case %q has unexpected NFD/NFC relation", tc.name)
		}
		nfcPath := filepath.ToSlash(filepath.Join("..", "Music", tc.name, tc.name+".mp3"))
		nfdPath := filepath.ToSlash(filepath.Join("..", "Music", nfdName, nfdName+".mp3"))
		write(nfcPath)
		storedPath := nfcPath
		if tc.changed {
			storedPath = nfdPath
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,1)`, i+1, storedPath, filepath.Base(storedPath)); err != nil {
			t.Fatal(err)
		}
	}
	limitName := "Limit Café"
	limitNFCPath := filepath.ToSlash(filepath.Join("..", "Music", limitName, limitName+".mp3"))
	limitNFDPath := norm.NFD.String(limitNFCPath)
	write(limitNFDPath)
	limitID := len(cases) + 1
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,1)`, limitID, limitNFCPath, filepath.Base(limitNFCPath)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	got, err := HealMountDataOnly(root, 0)
	if err != nil {
		t.Fatalf("heal: %+v %v", got, err)
	}
	wantNormalized := 0
	for _, tc := range cases {
		if tc.changed {
			wantNormalized++
		}
	}
	if got.PathsNormalized != wantNormalized {
		t.Fatalf("paths normalized=%d, want %d: %+v", got.PathsNormalized, wantNormalized, got)
	}
	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for i, tc := range cases {
		var path, filename string
		if err := check.QueryRow(`SELECT path,filename FROM Track WHERE id=?`, i+1).Scan(&path, &filename); err != nil {
			t.Fatal(err)
		}
		wantPath := filepath.ToSlash(filepath.Join("..", "Music", tc.name, tc.name+".mp3"))
		if path != wantPath || filename != filepath.Base(wantPath) {
			t.Errorf("track %d %q path=%q filename=%q, want %q/%q", i+1, tc.name, path, filename, wantPath, filepath.Base(wantPath))
		}
	}
	var limitPath, limitFilename string
	if err := check.QueryRow(`SELECT path,filename FROM Track WHERE id=?`, limitID).Scan(&limitPath, &limitFilename); err != nil {
		t.Fatal(err)
	}
	if limitPath != limitNFCPath || limitFilename != filepath.Base(limitNFCPath) {
		t.Fatalf("NFC library path was rewritten: %q/%q", limitPath, limitFilename)
	}
}

func TestNormalizeTrackPathPreservesLastEditTime(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	rows := []struct {
		id       int
		valueSQL string
		valueArg any
	}{
		{1, "?", int64(1700000000)},
		{2, "NULL", nil},
		{3, "?", "2024-01-15 10:20:30"},
		{4, "?", ""},
		{5, "X''", nil},
	}
	for _, row := range rows {
		old := filepath.ToSlash(filepath.Join("..", "Music", "Cafe"+string(rune(0x0301))+"-"+strconv.Itoa(row.id)+".mp3"))
		new := filepath.ToSlash(filepath.Join("..", "Music", "Café-"+strconv.Itoa(row.id)+".mp3"))
		path := filepath.Join(root, "Engine Library", filepath.FromSlash(new))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		query := `INSERT INTO Track(id,path,filename,lastEditTime,fileBytes,isAnalyzed) VALUES(?,?,?,` + row.valueSQL + `,1,1)`
		if row.valueSQL == "NULL" {
			if _, err := db.Exec(query, row.id, old, filepath.Base(old)); err != nil {
				t.Fatal(err)
			}
		} else if row.valueSQL == "X''" {
			if _, err := db.Exec(query, row.id, old, filepath.Base(old)); err != nil {
				t.Fatal(err)
			}
		} else if _, err := db.Exec(query, row.id, old, filepath.Base(old), row.valueArg); err != nil {
			t.Fatal(err)
		}
	}
	type storedValue struct{ typ, hex string }
	before := map[int]storedValue{}
	for _, row := range rows {
		var v storedValue
		if err := db.QueryRow(`SELECT typeof(lastEditTime), hex(CAST(lastEditTime AS BLOB)) FROM Track WHERE id=?`, row.id).Scan(&v.typ, &v.hex); err != nil {
			t.Fatal(err)
		}
		before[row.id] = v
	}
	var report Report
	if _, err := normalizeTrackPaths(context.Background(), db, root, &report); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		var after storedValue
		if err := db.QueryRow(`SELECT typeof(lastEditTime), hex(CAST(lastEditTime AS BLOB)) FROM Track WHERE id=?`, row.id).Scan(&after.typ, &after.hex); err != nil {
			t.Fatal(err)
		}
		if after != before[row.id] {
			t.Fatalf("track %d lastEditTime changed from %+v to %+v", row.id, before[row.id], after)
		}
	}
}

func TestHealMountDataOnlyRefusesLibraryInUse(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	path := filepath.Join(root, "Engine Library", "Database2", "m.db")
	db.Close()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.ExtraFiles = []*os.File{f}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if _, err := HealMountDataOnly(root, 0); !errors.Is(err, errLibraryInUse) {
		t.Fatalf("heal while library is open: %v", err)
	}
}

func TestTrackPathRejectsMountRoot(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{".", "..", "../Café/..", "../../outside.mp3", "/tmp/outside.mp3"} {
		if got, ok := trackPath(root, path); ok || got != "" {
			t.Fatalf("trackPath(%q)=%q,%v, want rejection", path, got, ok)
		}
	}
}

func TestHealMountDataOnlyLeavesTracksWhenBridgeStateIsUnreadable(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	for _, item := range []struct {
		id   int
		name string
	}{{1, "Bridge-one"}, {2, "Bridge-two"}} {
		id, name := item.id, item.name
		old := "../Music/" + name + "e\u0301.mp3"
		new := "../Music/" + name + "é.mp3"
		p := filepath.Join(root, "Engine Library", filepath.FromSlash(new))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,1)`, id, old, filepath.Base(old)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	state := filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-state.db")
	if err := os.WriteFile(state, []byte("not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := HealMountDataOnly(root, 0)
	if err != nil || got.PathsNormalized != 0 {
		t.Fatalf("unreadable bridge state changed tracks: %+v %v", got, err)
	}
}

func TestHealMountDataOnlyStopsAtContextDeadline(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	for i := 1; i <= 130; i++ {
		path := filepath.ToSlash(filepath.Join("..", "Music", "Cafe\u0301", "track-"+strconv.Itoa(i)+".mp3"))
		actual := filepath.Join(root, "Engine Library", filepath.FromSlash(filepath.ToSlash(filepath.Join("..", "Music", "Café", "track-"+strconv.Itoa(i)+".mp3"))))
		if err := os.MkdirAll(filepath.Dir(actual), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(actual, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,1)`, i, path, filepath.Base(path)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE Information SET id=99`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'Set',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES(1,1,0,999)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counted := 0
	normalizeTrackPathBatch = func() {
		counted++
		if counted == 1 {
			cancel()
		}
	}
	defer func() { normalizeTrackPathBatch = nil }()
	got, err := HealMountDataOnlyContext(ctx, root, 0)
	if err != nil || !got.Healthy {
		t.Fatalf("budget stop changed heal result: %+v %v", got, err)
	}
	var n int
	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.QueryRow(`SELECT count(*) FROM Track WHERE path LIKE '%Café%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 64 {
		t.Fatalf("first pass committed %d rows, want 64", n)
	}
	var infoID int
	if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
		t.Fatal(err)
	}
	if infoID != 1 {
		t.Fatalf("GL-010 identity was not repaired during soft path stop, Information.id=%d", infoID)
	}
	var nextEntity int
	if err := check.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=1`).Scan(&nextEntity); err != nil {
		t.Fatal(err)
	}
	if nextEntity != 999 {
		t.Fatalf("heavy base heal ran during a soft path stop, nextEntityId=%d", nextEntity)
	}
	got, err = HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy || got.PathsNormalized != 66 {
		t.Fatalf("second pass did not finish remaining rows: %+v %v", got, err)
	}
	if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
		t.Fatal(err)
	}
	if infoID != 1 {
		t.Fatalf("Information.id=%d after completion", infoID)
	}
	if err := check.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=1`).Scan(&nextEntity); err != nil {
		t.Fatal(err)
	}
	if nextEntity != 0 {
		t.Fatalf("fast path completion did not run base heal, nextEntityId=%d", nextEntity)
	}
}

func TestHealMountDataOnlyFastPathStillRunsBaseHeal(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	for i := 1; i <= 3; i++ {
		old := "../Music/Cafe\u0301/track-" + strconv.Itoa(i) + ".mp3"
		new := "../Music/Café/track-" + strconv.Itoa(i) + ".mp3"
		actual := filepath.Join(root, "Engine Library", filepath.FromSlash(new))
		if err := os.MkdirAll(filepath.Dir(actual), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(actual, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,1)`, i, old, filepath.Base(old)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE Information SET id=99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	got, err := HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy || got.PathsNormalized != 3 {
		t.Fatalf("fast path/base heal failed: %+v %v", got, err)
	}
	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var infoID int
	if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
		t.Fatal(err)
	}
	if infoID != 1 {
		t.Fatalf("base heal was skipped on a fast path pass, Information.id=%d", infoID)
	}
}

func TestHealMountDataOnlySlowPathDefersBaseHeal(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	for i := 1; i <= 64; i++ {
		old := "../Music/Cafe\u0301/track-" + strconv.Itoa(i) + ".mp3"
		new := "../Music/Café/track-" + strconv.Itoa(i) + ".mp3"
		actual := filepath.Join(root, "Engine Library", filepath.FromSlash(new))
		if err := os.MkdirAll(filepath.Dir(actual), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(actual, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,1)`, i, old, filepath.Base(old)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE Information SET id=99`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'Set',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES(1,1,0,999)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	normalizeTrackPathBatch = func() { time.Sleep(time.Second) }
	defer func() { normalizeTrackPathBatch = nil }()
	got, err := HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy || got.PathsNormalized != 64 {
		t.Fatalf("slow path pass failed: %+v %v", got, err)
	}
	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var infoID int
	if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
		t.Fatal(err)
	}
	if infoID != 1 {
		t.Fatalf("GL-010 identity was not repaired on slow path pass, Information.id=%d", infoID)
	}
	var nextEntity int
	if err := check.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=1`).Scan(&nextEntity); err != nil {
		t.Fatal(err)
	}
	if nextEntity != 999 {
		t.Fatalf("slow path pass unexpectedly ran heavy base heal, nextEntityId=%d", nextEntity)
	}
	normalizeTrackPathBatch = nil
	got, err = HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy {
		t.Fatalf("follow-up fast pass failed: %+v %v", got, err)
	}
	if err := check.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=1`).Scan(&nextEntity); err != nil {
		t.Fatal(err)
	}
	if nextEntity != 0 {
		t.Fatalf("follow-up fast pass did not run base heal, nextEntityId=%d", nextEntity)
	}
}
