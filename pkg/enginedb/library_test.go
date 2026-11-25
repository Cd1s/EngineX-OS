package enginedb

import (
	"bytes"
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
	defer lib.Close()
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := b.InsertList(ctx, ListRow{Title: "Imported"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.InsertList(ctx, ListRow{Title: "Native"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := b.InsertList(ctx, ListRow{Title: "Native", Parent: folder})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE Playlist SET nextListId=NULL WHERE id=?`, root); err != nil {
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
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES(?,?)`, legacyHash, art); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	// Opening alone must not mutate legacy artwork hashes.
	lib, err := OpenLibraryFile(ctx, filepath.Join(dir, "m.db"), mount)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	var kind string
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt LIMIT 1`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "text" {
		t.Fatalf("OpenLibraryFile unexpectedly migrated legacy hash: kind=%q", kind)
	}
	sum := sha1.Sum(art)
	artPath := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum[:])+".jpg")
	if _, err = os.Stat(artPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artwork file existed before migration: err=%v", err)
	}

	if err = lib.MigrateLegacyArtwork(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	var gotHash []byte
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash),hash FROM AlbumArt LIMIT 1`).Scan(&kind, &gotHash); err != nil {
		t.Fatal(err)
	}
	if kind != "blob" || !bytes.Equal(gotHash, sum[:]) {
		t.Fatalf("legacy artwork hash type=%q hash=%x want blob %x", kind, gotHash, sum[:])
	}
	data, err := os.ReadFile(artPath)
	if err != nil || !bytes.Equal(data, art) {
		t.Fatalf("legacy artwork file: err=%v data=%x", err, data)
	}
}

func TestMigrateLegacyArtworkCommitsPartialProgressWithinBudget(t *testing.T) {
	ctx := context.Background()
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	art1 := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 1, 1}
	art2 := []byte{0xff, 0xd8, 0xff, 0xe0, 2, 2, 2}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES('1111111111111111111111111111111111111111',?),('2222222222222222222222222222222222222222',?)`, art1, art2); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	// Advance clock past the 1s budget on the 3rd call (call 1 sets deadline, call 2 checks row 1, call 3 checks row 2).
	base := time.Unix(1700000000, 0)
	calls := 0
	origNow := migrateNow
	migrateNow = func() time.Time {
		calls++
		if calls >= 3 {
			return base.Add(2 * time.Second)
		}
		return base
	}
	t.Cleanup(func() { migrateNow = origNow })

	lib, err := OpenLibraryFile(ctx, filepath.Join(dir, "m.db"), mount)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if err = lib.MigrateLegacyArtwork(ctx, time.Second); err != nil {
		t.Fatalf("MigrateLegacyArtwork failed on budget expiry: %v", err)
	}
	var kind1, kind2 string
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt ORDER BY id LIMIT 1 OFFSET 0`).Scan(&kind1); err != nil {
		t.Fatal(err)
	}
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt ORDER BY id LIMIT 1 OFFSET 1`).Scan(&kind2); err != nil {
		t.Fatal(err)
	}
	if kind1 != "blob" || kind2 != "text" {
		t.Fatalf("partial budget progress not committed: row1=%q row2=%q, want blob/text", kind1, kind2)
	}
	sum1 := sha1.Sum(art1)
	p1 := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum1[:])+".jpg")
	if _, err = os.Stat(p1); err != nil {
		t.Fatalf("first migrated artwork file missing: %v", err)
	}
}

func TestMigrateLegacyArtworkSkipsCorruptCoverAndChecksOpenElsewhere(t *testing.T) {
	ctx := context.Background()
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	badPNG := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0}
	goodJPG := []byte{0xff, 0xd8, 0xff, 0xe0, 9, 8, 7}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',?),('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',?)`, badPNG, goodJPG); err != nil {
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
		t.Fatalf("corrupt PNG aborted migration: %v", err)
	}
	var badKind, goodKind string
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt ORDER BY id LIMIT 1 OFFSET 0`).Scan(&badKind); err != nil {
		t.Fatal(err)
	}
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt ORDER BY id LIMIT 1 OFFSET 1`).Scan(&goodKind); err != nil {
		t.Fatal(err)
	}
	if badKind != "text" || goodKind != "blob" {
		t.Fatalf("corrupt-skip mismatch: bad=%q good=%q, want text/blob", badKind, goodKind)
	}

	// OpenElsewhere before commit must roll back without converting the new TEXT row.
	good2 := []byte{0xff, 0xd8, 0xff, 0xe0, 4, 5, 6}
	if _, err = lib.db.ExecContext(ctx, `INSERT INTO AlbumArt(hash,albumArt) VALUES('cccccccccccccccccccccccccccccccccccccccc',?)`, good2); err != nil {
		t.Fatal(err)
	}
	stop := holdOpen(t, filepath.Join(dir, "m.db"))
	defer stop()
	if err = lib.MigrateLegacyArtwork(ctx, time.Second); !errors.Is(err, ErrLibraryInUse) {
		t.Fatalf("OpenElsewhere migration err=%v, want ErrLibraryInUse", err)
	}
	var thirdKind string
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt ORDER BY id LIMIT 1 OFFSET 2`).Scan(&thirdKind); err != nil {
		t.Fatal(err)
	}
	if thirdKind != "text" {
		t.Fatalf("OpenElsewhere rollback failed: row3=%q, want text", thirdKind)
	}
}

func makeQuickCuesWithMain(t *testing.T, base []byte, main float64, adj byte) []byte {
	t.Helper()
	raw, err := QUncompress(base, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	at, ok := cuePositions(raw)
	if !ok {
		t.Fatal("cue layout not recognized")
	}
	big.PutUint64(raw[at[8]:], math.Float64bits(main))
	raw[at[8]+8] = adj
	big.PutUint64(raw[at[9]:], math.Float64bits(main))
	out, err := QCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readMainCueFromDB(t *testing.T, lib *Library, id int64) (float64, byte) {
	t.Helper()
	var blob []byte
	if err := lib.db.QueryRowContext(context.Background(), `SELECT quickCues FROM PerformanceData WHERE trackId=?`, id).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	raw, err := QUncompress(blob, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	at, ok := cuePositions(raw)
	if !ok {
		t.Fatal("cue layout not recognized")
	}
	return math.Float64frombits(big.Uint64(raw[at[8]:])), raw[at[8]+8]
}

func TestApplyPerformanceAcceptsRawLegacyWrittenDigest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	track := rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", SampleRate: 44100, Duration: 10}
	ids, _, _, err := BuildLibrary(ctx, dir, "", []TrackRow{{Rel: "Contents/a.mp3", Track: track}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	memPerf, err := EncodePerformance(track, &rekordbox.Analysis{Cues: []rekordbox.Cue{{HotCue: 0, TimeMS: 225}}})
	if err != nil {
		t.Fatal(err)
	}
	noMemPerf, err := EncodePerformance(track, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy v1 / 9f2c0770 wrote adj=0 with the memory cue position and stored RawWrittenDigest.
	legacyCues := makeQuickCuesWithMain(t, memPerf.QuickCues, 9920, 0)
	legacyRawDigest := RawWrittenDigest(memPerf.Beats, legacyCues, memPerf.Loops)

	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()

	// Case 1 (NEW_REG1): untouched legacy row (adj=0, main=9920) upgraded to adj=1 -> PerfWritten, not PerfKept.
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`, memPerf.Beats, legacyCues, memPerf.Loops, ids[0]); err != nil {
		t.Fatal(err)
	}
	digest, outcome, err := b.ApplyPerformance(ctx, ids[0], memPerf, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{State: rbstate.PerfApplied, Written: legacyRawDigest}, track)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PerfWritten || digest == nil {
		t.Fatalf("NEW_REG1: untouched legacy row became outcome=%d digestNil=%v, want PerfWritten", outcome, digest == nil)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, adj := readMainCueFromDB(t, lib, ids[0]); adj != 1 {
		t.Fatalf("NEW_REG1: upgraded row adj=%d, want 1", adj)
	}

	// Case 2 (NEW_REG4): untouched pre-fix adj=0 row (main=9920) whose memory cue is deleted -> PerfWritten (rewrites to -1, not PerfSame).
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`, noMemPerf.Beats, legacyCues, noMemPerf.Loops, ids[0]); err != nil {
		t.Fatal(err)
	}
	digest, outcome, err = b.ApplyPerformance(ctx, ids[0], noMemPerf, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{State: rbstate.PerfApplied, Written: legacyRawDigest}, track)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PerfWritten || digest == nil {
		t.Fatalf("NEW_REG4: deleted memory cue on pre-fix row became outcome=%d digestNil=%v, want PerfWritten", outcome, digest == nil)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if main, adj := readMainCueFromDB(t, lib, ids[0]); main != -1 || adj != 0 {
		t.Fatalf("NEW_REG4: stale main cue kept in DB: main=%v adj=%d, want -1, 0", main, adj)
	}

	// Case 3: legacy row (adj=0, main=9920) loaded on deck before upgrade (Engine rewrote to -1105, adj=0),
	// now upgraded to adj=1 -> PerfWritten (restores rekordbox main cue with adj=1).
	loadedLegacyCues := makeQuickCuesWithMain(t, memPerf.QuickCues, -1105, 0)
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`, memPerf.Beats, loadedLegacyCues, memPerf.Loops, ids[0]); err != nil {
		t.Fatal(err)
	}
	mem9920 := memPerf
	mem9920.QuickCues = makeQuickCuesWithMain(t, memPerf.QuickCues, 9920, 1)
	mem9920.Digest = WrittenDigest(mem9920.Beats, mem9920.QuickCues, mem9920.Loops)
	digest, outcome, err = b.ApplyPerformance(ctx, ids[0], mem9920, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{State: rbstate.PerfApplied, Written: legacyRawDigest}, track)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PerfWritten || digest == nil {
		t.Fatalf("loaded legacy row became outcome=%d digestNil=%v, want PerfWritten", outcome, digest == nil)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if main, adj := readMainCueFromDB(t, lib, ids[0]); main != 9920 || adj != 1 {
		t.Fatalf("loaded legacy row did not restore main cue: main=%v adj=%d, want 9920, 1", main, adj)
	}

	// Case 4: real DJ edit on a legacy row (DJ moved main cue -> adj=1) -> must stay PerfKept.
	djEditedCues := makeQuickCuesWithMain(t, memPerf.QuickCues, 4410, 1)
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`, memPerf.Beats, djEditedCues, memPerf.Loops, ids[0]); err != nil {
		t.Fatal(err)
	}
	digest, outcome, err = b.ApplyPerformance(ctx, ids[0], mem9920, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{State: rbstate.PerfApplied, Written: legacyRawDigest}, track)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PerfKept || digest != nil {
		t.Fatalf("DJ edit on legacy row became outcome=%d digest=%x, want PerfKept/nil", outcome, digest)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
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
	stop := holdOpen(t, filepath.Join(dir, "m.db"))
	defer stop()
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

func TestOpenLibraryRepairsGL010Information(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE Information SET id=2, uuid='', currentPlayedIndiciator=0`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	var id, played int64
	var gotUUID string
	if err = lib.db.QueryRowContext(ctx, `SELECT id,uuid,currentPlayedIndiciator FROM Information`).Scan(&id, &gotUUID, &played); err != nil {
		t.Fatal(err)
	}
	if id != 1 || gotUUID == "" || played == 0 {
		t.Fatalf("GL-010 Information repair failed: id=%d uuid=%q played=%d", id, gotUUID, played)
	}
}

func TestLibraryUsesImmediateWriteLock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Hold a RESERVED write lock on a separate connection so a BEGIN IMMEDIATE fails at BeginTx.
	holder, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err = holder.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer holder.ExecContext(ctx, `ROLLBACK`)

	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	shortCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err = lib.db.BeginTx(shortCtx, nil); err == nil {
		t.Fatal("BeginTx succeeded despite active write transaction; _txlock=immediate not active")
	}
}
