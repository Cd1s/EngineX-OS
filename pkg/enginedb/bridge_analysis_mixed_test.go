package enginedb

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestMixedAnalysisRowsKeepSourceIdentityDuringSwap(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Sync(context.Background(), root, l, Options{}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library/Database2/m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`ATTACH DATABASE ? AS bridge`, filepath.Join(root, "Engine Library/Database2", stateFile)); err != nil {
		t.Fatal(err)
	}
	// One old plain row and one newly compressed row, both with Engine's
	// import-wide key: only the saved source ID can distinguish a path swap.
	for _, id := range []int{3, 4} {
		var b []byte
		if err = db.QueryRow(`SELECT sourceJSON FROM BridgeAnalysis WHERE trackId=?`, id).Scan(&b); err != nil {
			t.Fatal(err)
		}
		plain, e := UnpackAnalysis(b, 64<<20)
		if e != nil {
			t.Fatal(e)
		}
		if id == 4 {
			b, err = packAnalysis(plain)
		} else {
			b = plain
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`UPDATE BridgeAnalysis SET sourceJSON=? WHERE trackId=?`, b, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`UPDATE Track SET pdbImportKey=67 WHERE id IN (3,4)`); err != nil {
		t.Fatal(err)
	}
	l.Tracks[2].Path, l.Tracks[3].Path = l.Tracks[3].Path, l.Tracks[2].Path
	r, err := Sync(context.Background(), root, l, Options{})
	if err != nil || r.Added != 0 || r.Updated != 2 {
		t.Fatalf("mixed-row swap: %+v %v", r, err)
	}
	for id, path := range map[int]string{3: "../MUSIC/four.wav", 4: "../MUSIC/three.wav"} {
		var got string
		if err = db.QueryRow(`SELECT path FROM Track WHERE id=? AND pdbImportKey=?`, id, id).Scan(&got); err != nil || got != path {
			t.Fatalf("track %d: path=%q %v", id, got, err)
		}
	}
}
