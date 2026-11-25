package enginedb

import (
	"context"
	"database/sql"
	"testing"
)

func TestDeleteListReparentsNativeChildAfterNullRootTail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := b.InsertList(ctx, ListRow{Title: "Folder"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := b.InsertList(ctx, ListRow{Title: "Native", Parent: folder})
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.InsertList(ctx, ListRow{Title: "Root Native"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.tx.ExecContext(ctx, `UPDATE Playlist SET nextListId=NULL WHERE id=?`, root); err != nil {
		t.Fatal(err)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.DeleteList(ctx, folder, func(id int64) bool { return id == folder }); err != nil {
		t.Fatal(err)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var parent, next int64
	if err = lib.db.QueryRowContext(ctx, `SELECT parentListId,COALESCE(nextListId,0) FROM Playlist WHERE id=?`, child).Scan(&parent, &next); err != nil {
		t.Fatal(err)
	}
	if parent != 0 || next != 0 {
		t.Fatalf("native child was not reparented: parent=%d next=%d", parent, next)
	}
	lib.Close()
}

func TestOpenLibraryMigratesLegacyArtworkHashes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dir+"/m.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES('00112233445566778899aabbccddeeff00112233',x'01')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	var kind string
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt LIMIT 1`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "blob" {
		t.Fatalf("legacy artwork hash type=%q", kind)
	}
}
