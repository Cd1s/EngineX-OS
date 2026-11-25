package enginedb

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
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
	root, err := b.InsertList(ctx, ListRow{Title: "Native"})
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
	var parent, next, rootNext int64
	var title string
	if err = lib.db.QueryRowContext(ctx, `SELECT parentListId,COALESCE(nextListId,0),title FROM Playlist WHERE id=?`, child).Scan(&parent, &next, &title); err != nil {
		t.Fatal(err)
	}
	if err = lib.db.QueryRowContext(ctx, `SELECT COALESCE(nextListId,0) FROM Playlist WHERE id=?`, root).Scan(&rootNext); err != nil {
		t.Fatal(err)
	}
	if parent != 0 || next != 0 || rootNext != child || title != "Native (1)" {
		t.Fatalf("native child was not reparented: parent=%d next=%d rootNext=%d title=%q", parent, next, rootNext, title)
	}
	lib.Close()
}

func TestOpenLibraryMigratesLegacyArtworkHashes(t *testing.T) {
	ctx := context.Background()
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	art := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	legacyHash := "00112233445566778899aabbccddeeff00112233"
	db, err := sql.Open("sqlite", dir+"/m.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES(?,?)`, legacyHash, art); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	lib, err := OpenLibraryFile(ctx, filepath.Join(dir, "m.db"), mount)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if err = lib.MigrateLegacyArtwork(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt LIMIT 1`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "blob" {
		t.Fatalf("legacy artwork hash type=%q", kind)
	}
	sum := sha1.Sum(art)
	path := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum[:])+".jpg")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(art) {
		t.Fatalf("legacy artwork file: err=%v data=%x", err, data)
	}
}

func TestApplyPerformanceAcceptsRawLegacyWrittenDigest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	track := rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", SampleRate: 44100, Duration: 10}
	ids, _, _, err := BuildLibrary(ctx, dir, "", []TrackRow{{Rel: "Contents/a.mp3", Track: track}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := EncodePerformance(track, nil)
	if err != nil {
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
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`, p.Beats, p.QuickCues, p.Loops, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	row := append([]byte(nil), p.QuickCues...)
	raw, err := QUncompress(row, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	at, ok := cuePositions(raw)
	if !ok {
		t.Fatal("cue layout not recognized")
	}
	big.PutUint64(raw[at[8]:], math.Float64bits(-1105))
	raw[at[8]+8] = 0
	row, err = QCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, row, ids[0]); err != nil {
		t.Fatal(err)
	}
	prev := PerfPrev{State: rbstate.PerfApplied, Written: RawWrittenDigest(p.Beats, p.QuickCues, p.Loops)}
	_, outcome, err := b.ApplyPerformance(ctx, ids[0], p, rbstate.Facts{Duration: 10, SampleRate: 44100}, prev, track)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == PerfKept {
		t.Fatalf("legacy raw digest became a deck edit: outcome=%d", outcome)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	lib.Close()
}

func TestBatchCommitRollsBackWhenLibraryOpensElsewhere(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE Information SET currentPlayedIndiciator=999`); err != nil {
		t.Fatal(err)
	}
	holdOpen(t, filepath.Join(dir, "m.db"))
	if err = b.Commit(ctx); !errors.Is(err, ErrLibraryInUse) {
		t.Fatalf("commit error=%v, want ErrLibraryInUse", err)
	}
	var indicator int64
	if err = lib.db.QueryRowContext(ctx, `SELECT currentPlayedIndiciator FROM Information`).Scan(&indicator); err != nil {
		t.Fatal(err)
	}
	if indicator == 999 {
		t.Fatal("failed commit left its transaction applied")
	}
}

func TestSetEntriesChainsEntitiesAndClampsFiveHundredBPM(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rows := []TrackRow{{Rel: "Contents/a.mp3", Track: rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", BPM: 500}}, {Rel: "Contents/b.mp3", Track: rekordbox.Track{ID: 2, Path: "/Contents/b.mp3", BPM: 120}}}
	ids, _, _, err := BuildLibrary(ctx, dir, "", rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	var bpm float64
	if err = lib.db.QueryRowContext(ctx, `SELECT bpm FROM Track WHERE id=?`, ids[0]).Scan(&bpm); err != nil {
		t.Fatal(err)
	}
	if bpm != 0 {
		t.Fatalf("500 BPM was not clamped: %v", bpm)
	}
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	list, err := b.InsertList(ctx, ListRow{Title: "Set"})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SetEntries(ctx, list, ids); err != nil {
		t.Fatal(err)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var firstNext, secondNext int64
	if err = lib.db.QueryRowContext(ctx, `SELECT nextEntityId FROM PlaylistEntity WHERE trackId=?`, ids[0]).Scan(&firstNext); err != nil {
		t.Fatal(err)
	}
	if err = lib.db.QueryRowContext(ctx, `SELECT nextEntityId FROM PlaylistEntity WHERE trackId=?`, ids[1]).Scan(&secondNext); err != nil {
		t.Fatal(err)
	}
	if firstNext == 0 || secondNext != 0 {
		t.Fatalf("entity chain: first=%d second=%d", firstNext, secondNext)
	}
}
