package dbhealer

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"golang.org/x/text/unicode/norm"
	"modernc.org/sqlite"
)

func TestHealMountDataOnlyLocalizesOfflineDropboxTracks(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	writeFile := func(rel string, data []byte) {
		t.Helper()
		full := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Local complete Dropbox audio files present on the SD card.
	writeFile("../Music/both.mp3", []byte("audio-1"))
	writeFile("../Music/packtracks.mp3", []byte("audio-2"))
	writeFile("../Music/source-only.mp3", []byte("audio-3"))
	writeFile("../Music/lowercase.mp3", []byte("audio-4"))
	writeFile("../Music/Café-pack.mp3", []byte("audio-5"))
	writeFile("../Music/Café/Café-explicit.mp3", []byte("audio-6"))
	writeFile("../Music/unknown-size.mp3", []byte("audio-unknown-size"))
	exactNFDPath := norm.NFD.String("../Music/Café-exact-nfd.mp3")
	writeFile(exactNFDPath, []byte("audio-8"))
	writeFile("../Music/collision.mp3", []byte("audio-9"))
	writeFile("../Music/Café-collide.mp3", []byte("audio-c"))

	// Negative-control files that must NEVER be localized.
	writeFile("../Music/empty.mp3", nil)
	writeFile("../Music/empty-unknown.mp3", nil)
	writeFile("../Music/truncated.mp3", []byte("x"))
	writeFile("../Music/oversized.mp3", []byte("audio-way-too-long"))
	writeFile("../Music/tidal.mp3", []byte("audio-t"))
	writeFile("../Music/beatport.mp3", []byte("audio-b"))
	writeFile("../Music/mismatched-src.mp3", []byte("audio-m"))
	writeFile("../Music/mismatched-uri.mp3", []byte("audio-u"))
	writeFile("../Music/local-explicit.mp3", []byte("audio-e"))
	if err := os.MkdirAll(filepath.Join(root, "Music", "dir-not-file.mp3"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.mp3")
	if err := os.WriteFile(outside, []byte("secret!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Music", "escape-link.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "Music", "both.mp3"), filepath.Join(root, "Music", "internal-link.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "LinkDir")); err != nil {
		t.Fatal(err)
	}

	nfdPackPath := norm.NFD.String("../Music/Café-pack.mp3")
	nfdExplicitPath := norm.NFD.String("../Music/Café/Café-explicit.mp3")
	nfdCollidePath := norm.NFD.String("../Music/Café-collide.mp3")
	rows := []struct {
		id         int
		path       string
		wantPath   string
		fileBytes  any
		src        any
		uri        any
		flags      int
		lastEdit   int64
		originUUID string
		originID   int
		wantClear  bool
	}{
		// Localizable Dropbox cases.
		{1, "../Music/both.mp3", "../Music/both.mp3", 7, "Dropbox", "streaming://Dropbox/Track/Music/both.mp3", 5, 1700000001, "orig-db", 101, true},
		{2, "../Music/packtracks.mp3", "../Music/packtracks.mp3", 7, "", "streaming://Dropbox/Track/Music/packtracks.mp3", 5, 1700000002, "orig-db", 102, true},
		{3, "../Music/source-only.mp3", "../Music/source-only.mp3", 7, "Dropbox", nil, 5, 1700000003, "orig-db", 103, true},
		{4, "../Music/lowercase.mp3", "../Music/lowercase.mp3", 7, "dropbox", "streaming://dropbox/Track/Music/lowercase.mp3", 5, 1700000004, "orig-db", 104, true},
		{5, nfdPackPath, "../Music/Café-pack.mp3", 7, "", "streaming://Dropbox/Track/Music/Cafe.mp3", 5, 1700000005, "orig-db", 105, true},
		{6, nfdExplicitPath, "../Music/Café/Café-explicit.mp3", 7, "Dropbox", "streaming://Dropbox/Track/Music/Cafe-explicit.mp3", 5, 1700000006, "orig-db", 106, true},
		{7, "../Music/unknown-size.mp3", "../Music/unknown-size.mp3", nil, "Dropbox", "streaming://Dropbox/Track/Music/unknown-size.mp3", 5, 1700000007, "orig-db", 107, true},
		{8, exactNFDPath, exactNFDPath, 7, "Dropbox", "streaming://Dropbox/Track/Music/exact-nfd.mp3", 5, 1700000008, "orig-db", 108, true},
		// Existing local NFC row that collides with row 9.
		{9, "../Music/Café-collide.mp3", "../Music/Café-collide.mp3", 7, nil, nil, 0, 1700000009, "orig-db", 109, false},
		// Must stay untouched.
		{10, "../Music/missing-cloud-only.mp3", "../Music/missing-cloud-only.mp3", 7, "Dropbox", "streaming://Dropbox/Track/Music/missing.mp3", 5, 1700000010, "orig-db", 110, false},
		{11, "../Music/empty.mp3", "../Music/empty.mp3", 7, "Dropbox", "streaming://Dropbox/Track/Music/empty.mp3", 5, 1700000011, "orig-db", 111, false},
		{12, "../Music/dir-not-file.mp3", "../Music/dir-not-file.mp3", 7, "Dropbox", "streaming://Dropbox/Track/Music/dir.mp3", 5, 1700000012, "orig-db", 112, false},
		{13, "../Music/escape-link.mp3", "../Music/escape-link.mp3", 7, "Dropbox", "streaming://Dropbox/Track/Music/escape.mp3", 5, 1700000013, "orig-db", 113, false},
		{14, "../Music/internal-link.mp3", "../Music/internal-link.mp3", 7, "Dropbox", "streaming://Dropbox/Track/Music/internal.mp3", 5, 1700000014, "orig-db", 114, false},
		{15, "../LinkDir/outside.mp3", "../LinkDir/outside.mp3", 7, "Dropbox", "streaming://Dropbox/Track/LinkDir/outside.mp3", 5, 1700000015, "orig-db", 115, false},
		{16, "../../outside.mp3", "../../outside.mp3", 7, "Dropbox", "streaming://Dropbox/Track/outside.mp3", 5, 1700000016, "orig-db", 116, false},
		{17, outside, outside, 7, "Dropbox", "streaming://Dropbox/Track/abs.mp3", 5, 1700000017, "orig-db", 117, false},
		{18, "../Music/tidal.mp3", "../Music/tidal.mp3", 7, "TIDAL", "streaming://TIDAL/Track/9001", 1, 1700000018, "orig-db", 118, false},
		{19, "../Music/beatport.mp3", "../Music/beatport.mp3", 7, "Beatport", "streaming://Beatport/Track/9002", 1, 1700000019, "orig-db", 119, false},
		{20, "../Music/mismatched-src.mp3", "../Music/mismatched-src.mp3", 7, "TIDAL", "streaming://Dropbox/Track/spoof.mp3", 5, 1700000020, "orig-db", 120, false},
		{21, "../Music/mismatched-uri.mp3", "../Music/mismatched-uri.mp3", 7, "Dropbox", "streaming://TIDAL/Track/9003", 5, 1700000021, "orig-db", 121, false},
		{22, "../Music/local-explicit.mp3", "../Music/local-explicit.mp3", 7, nil, nil, 2, 1700000022, "orig-db", 122, false},
		{23, "../Music/truncated.mp3", "../Music/truncated.mp3", 22094, "Dropbox", "streaming://Dropbox/Track/Music/truncated.mp3", 5, 1700000023, "orig-db", 123, false},
		{24, "../Music/oversized.mp3", "../Music/oversized.mp3", 7, "Dropbox", "streaming://Dropbox/Track/Music/oversized.mp3", 5, 1700000024, "orig-db", 124, false},
		{25, "../Music/empty-unknown.mp3", "../Music/empty-unknown.mp3", 0, "Dropbox", "streaming://Dropbox/Track/Music/empty-unknown.mp3", 5, 1700000025, "orig-db", 125, false},
		{26, nfdCollidePath, nfdCollidePath, 7, "Dropbox", "streaming://Dropbox/Track/Music/collide.mp3", 5, 1700000026, "orig-db", 126, false},
	}
	validTrack := rawTrack(1e7)
	validBeat := rawBeats(nil)
	validWave := placeholder(1e7)
	for _, row := range rows {
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,fileBytes,streamingSource,uri,streamingFlags,originDatabaseUuid,originTrackId,isAnalyzed) VALUES(?,?,?,?,?,?,?,?,?,1)`,
			row.id, row.path, filepath.Base(row.path), row.fileBytes, row.src, row.uri, row.flags, row.originUUID, row.originID,
		); err != nil {
			t.Fatalf("insert row %d: %v", row.id, err)
		}
		if _, err := db.Exec(
			`UPDATE PerformanceData SET trackData=?, beatData=?, overviewWaveFormData=? WHERE trackId=?`,
			validTrack, validBeat, validWave, row.id,
		); err != nil {
			t.Fatalf("seed PerformanceData row %d: %v", row.id, err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=? WHERE id=?`, row.lastEdit, row.id); err != nil {
			t.Fatalf("set lastEditTime row %d: %v", row.id, err)
		}
	}
	var schemaBefore string
	if err := db.QueryRow(`SELECT group_concat(type||name||COALESCE(sql,''),char(10)) FROM (SELECT * FROM main.sqlite_master ORDER BY type,name)`).Scan(&schemaBefore); err != nil {
		t.Fatal(err)
	}
	db.Close()

	got, err := HealMountDataOnly(root, 0)
	if err != nil {
		t.Fatalf("HealMountDataOnly: %+v %v", got, err)
	}
	if !got.Healthy || got.DropboxLocalized != 8 || got.PathsNormalized != 2 {
		t.Fatalf("unexpected report: %+v (want healthy, dropbox=8, paths=2)", got)
	}
	// Completed pass must leave no cursor sidecar beside m.db.
	if _, err := os.Stat(dropboxCursorPath(root)); !os.IsNotExist(err) {
		t.Fatalf("cursor file left behind after completed pass: %v", err)
	}

	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()

	var schemaAfter string
	if err := check.QueryRow(`SELECT group_concat(type||name||COALESCE(sql,''),char(10)) FROM (SELECT * FROM main.sqlite_master ORDER BY type,name)`).Scan(&schemaAfter); err != nil {
		t.Fatal(err)
	}
	if schemaAfter != schemaBefore {
		t.Fatalf("data-only heal modified sqlite_master")
	}

	for _, row := range rows {
		var src, uri sql.NullString
		var path, filename string
		var flags int
		var lastEdit int64
		var originUUID string
		var originID int
		if err := check.QueryRow(
			`SELECT path, filename, streamingSource, uri, COALESCE(streamingFlags,0), lastEditTime, originDatabaseUuid, originTrackId FROM Track WHERE id=?`,
			row.id,
		).Scan(&path, &filename, &src, &uri, &flags, &lastEdit, &originUUID, &originID); err != nil {
			t.Fatalf("read row %d: %v", row.id, err)
		}
		if lastEdit != row.lastEdit || originUUID != row.originUUID || originID != row.originID {
			t.Errorf("row %d sync metadata changed: lastEdit=%d (want %d) origin=(%q,%d)", row.id, lastEdit, row.lastEdit, originUUID, originID)
		}
		if path != row.wantPath || filename != filepath.Base(row.wantPath) {
			t.Errorf("row %d path=%q filename=%q, want %q/%q", row.id, path, filename, row.wantPath, filepath.Base(row.wantPath))
		}
		if row.wantClear {
			if src.Valid || uri.Valid || flags != 0 {
				t.Errorf("row %d not localized: src=%+v uri=%+v flags=%d", row.id, src, uri, flags)
			}
		} else {
			wantSrc, _ := row.src.(string)
			wantURI, _ := row.uri.(string)
			if src.String != wantSrc || uri.String != wantURI || flags != row.flags {
				t.Errorf("row %d unexpectedly modified: got src=%q uri=%q flags=%d, want src=%q uri=%q flags=%d",
					row.id, src.String, uri.String, flags, wantSrc, wantURI, row.flags)
			}
		}
	}

	// Idempotent on second mount.
	got2, err := HealMountDataOnly(root, 0)
	if err != nil || !got2.Healthy || got2.DropboxLocalized != 0 {
		t.Fatalf("second pass not idempotent: %+v %v", got2, err)
	}
}

func TestHealMountDropboxCompleteMacAndPartialWAV(t *testing.T) {
	for _, mode := range []string{"complete", "mac", "partial"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			testlibrary.ExportDrive(t, root)
			db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			name := "Track001.wav"
			disk := filepath.Join(root, "Music", name)
			if mode == "mac" {
				if err := os.Rename(disk, filepath.Join(root, "Music", "Café.wav")); err != nil {
					t.Fatal(err)
				}
				name = norm.NFD.String("Café.wav")
			}
			if mode == "partial" {
				if err := os.Truncate(disk, 1); err != nil {
					t.Fatal(err)
				}
			}
			rel := "../Music/" + name
			if _, err := db.Exec(
				`UPDATE Track SET path=?, filename=?, streamingSource='Dropbox', uri='streaming://Dropbox/Track/1', streamingFlags=5 WHERE id=1`,
				rel, filepath.Base(rel),
			); err != nil {
				t.Fatal(err)
			}
			db.Close()

			if _, err := HealMountDataOnly(root, 0); err != nil {
				t.Fatal(err)
			}
			check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			var source, uri, path, filename string
			var flags, listedBytes int64
			if err := check.QueryRow(`SELECT coalesce(streamingSource,''), coalesce(uri,''), streamingFlags, path, filename, fileBytes FROM Track WHERE id=1`).Scan(
				&source, &uri, &flags, &path, &filename, &listedBytes,
			); err != nil {
				t.Fatal(err)
			}
			if mode == "partial" {
				if source != "Dropbox" || uri == "" || flags != 5 {
					t.Fatalf("truncated 1-byte file of %d was localized: source=%q uri=%q flags=%d", listedBytes, source, uri, flags)
				}
			} else {
				if source != "" || uri != "" || flags != 0 || !norm.NFC.IsNormalString(path) || !norm.NFC.IsNormalString(filename) {
					t.Fatalf("mode=%s not cleanly localized: source=%q uri=%q flags=%d path=%+q filename=%+q", mode, source, uri, flags, path, filename)
				}
			}
		})
	}
}

func TestHealMountDropboxStopsAtContextDeadline(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	for i := 1; i <= 130; i++ {
		rel := "../Music/Dropbox/track-" + strconv.Itoa(i) + ".mp3"
		full := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,fileBytes,streamingSource,uri,streamingFlags,isAnalyzed) VALUES(?,?,?,5,'Dropbox',?,5,1)`,
			i, rel, filepath.Base(rel), "streaming://Dropbox/Track/track-"+strconv.Itoa(i)+".mp3",
		); err != nil {
			t.Fatal(err)
		}
	}
	// Broken Information.id (GL-010) and broken PlaylistEntity chain (heavy base heal).
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
	batches := 0
	localizeDropboxTrackBatch = func() {
		batches++
		if batches == 1 {
			cancel()
		}
	}
	defer func() { localizeDropboxTrackBatch = nil }()

	got, err := HealMountDataOnlyContext(ctx, root, 0)
	if err != nil || !got.Healthy || got.DropboxLocalized != 64 {
		t.Fatalf("first budgeted pass: %+v %v (want healthy, dropbox=64)", got, err)
	}
	if _, err := os.Stat(dropboxCursorPath(root)); err != nil {
		t.Fatalf("expected resume cursor after interrupted pass: %v", err)
	}

	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	// GL-010 identity repair runs even on the budgeted early-return path.
	var infoID int
	if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
		t.Fatal(err)
	}
	if infoID != 1 {
		t.Fatalf("GL-010 identity not repaired during soft budget stop: Information.id=%d", infoID)
	}
	// Heavy base heal (chain relinking) is still deferred on the soft budget stop.
	var nextEntity int
	if err := check.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=1`).Scan(&nextEntity); err != nil {
		t.Fatal(err)
	}
	if nextEntity != 999 {
		t.Fatalf("heavy base heal ran during soft dropbox budget stop: nextEntityId=%d", nextEntity)
	}
	check.Close()

	// Second pass resumes from cursor, finishes remaining 66 tracks, deletes cursor, and runs base heal.
	localizeDropboxTrackBatch = nil
	got, err = HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy || got.DropboxLocalized != 66 {
		t.Fatalf("second pass: %+v %v (want healthy, dropbox=66)", got, err)
	}
	if _, err := os.Stat(dropboxCursorPath(root)); !os.IsNotExist(err) {
		t.Fatalf("cursor not removed after completion: %v", err)
	}
	check, err = sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=1`).Scan(&nextEntity); err != nil {
		t.Fatal(err)
	}
	if nextEntity != 0 {
		t.Fatalf("base heal did not run after fast completion: nextEntityId=%d", nextEntity)
	}
}

func TestHealMountDropboxSlowPathIdentityAndFullHeal(t *testing.T) {
	// 1. Unlimited full HealMount with slow Dropbox pass must still finish full base heal
	// (including Information.id=1 and chain repair).
	t.Run("full-heal-finishes", func(t *testing.T) {
		root := t.TempDir()
		testlibrary.ExportDrive(t, root)
		db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET path='../Music/Track001.wav', filename='Track001.wav', streamingSource='Dropbox', uri='streaming://Dropbox/Track/1', streamingFlags=5 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Information SET id=99`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES(99,1,0,999)`); err != nil {
			t.Fatal(err)
		}
		db.Close()

		localizeDropboxTrackBatch = func() { time.Sleep(1050 * time.Millisecond) }
		t.Cleanup(func() { localizeDropboxTrackBatch = nil })

		got, err := HealMount(root, 0)
		if err != nil || !got.Healthy || got.DropboxLocalized != 1 {
			t.Fatalf("full heal: %+v %v", got, err)
		}
		check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		var infoID, nextEntity int
		if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
			t.Fatal(err)
		}
		if err := check.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=99`).Scan(&nextEntity); err != nil {
			t.Fatal(err)
		}
		if infoID != 1 || nextEntity != 0 {
			t.Fatalf("full heal skipped base repair: Information.id=%d nextEntityId=%d", infoID, nextEntity)
		}
	})

	// 2. Data-only mount heal with slow Dropbox pass repairs Information.id=1 (GL-010)
	// while deferring heavy base heal.
	t.Run("data-only-preserves-identity-and-defers-heavy-heal", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		for i := 1; i <= 64; i++ {
			rel := "../Music/Slow/track-" + strconv.Itoa(i) + ".mp3"
			full := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("audio"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(
				`INSERT INTO Track(id,path,filename,fileBytes,streamingSource,uri,streamingFlags,isAnalyzed) VALUES(?,?,?,5,'Dropbox',?,5,1)`,
				i, rel, filepath.Base(rel), "streaming://Dropbox/Track/slow-"+strconv.Itoa(i)+".mp3",
			); err != nil {
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

		localizeDropboxTrackBatch = func() { time.Sleep(1050 * time.Millisecond) }
		t.Cleanup(func() { localizeDropboxTrackBatch = nil })

		got, err := HealMountDataOnly(root, 0)
		if err != nil || !got.Healthy || got.DropboxLocalized != 64 {
			t.Fatalf("slow dropbox pass: %+v %v", got, err)
		}
		check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		var infoID, nextEntity int
		if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
			t.Fatal(err)
		}
		if err := check.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=1`).Scan(&nextEntity); err != nil {
			t.Fatal(err)
		}
		if infoID != 1 {
			t.Fatalf("data-only slow path left Information.id=%d, want 1", infoID)
		}
		if nextEntity != 999 {
			t.Fatalf("data-only slow path unexpectedly ran heavy chain heal, nextEntityId=%d", nextEntity)
		}
	})
}

func TestHealMountDropboxResumesPastMissingCloudPrefix(t *testing.T) {
	root := t.TempDir()
	testlibrary.ExportDrive(t, root)
	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (VALUES(9) UNION ALL SELECT i+1 FROM n WHERE i<73)
		INSERT INTO Track(id,path,streamingSource,uri,streamingFlags) SELECT i,printf('../Music/absent%d.wav',i),
			'Dropbox','streaming://Dropbox/Track/'||i,5 FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET path='../Music/Track001.wav' WHERE id=73`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	t.Cleanup(func() { localizeDropboxTrackBatch = nil })
	localizedTotal := 0
	for pass := 1; pass <= 3; pass++ {
		ctx, cancel := context.WithCancel(context.Background())
		localizeDropboxTrackBatch = cancel
		report, err := HealMountDataOnlyContext(ctx, root, 0)
		cancel()
		if err != nil || !report.Healthy {
			t.Fatalf("pass=%d failed: %+v %v", pass, report, err)
		}
		localizedTotal += report.DropboxLocalized
	}
	if localizedTotal != 1 {
		t.Fatalf("expected trailing copied WAV to localize once across repeated passes, got %d", localizedTotal)
	}
	if _, err := os.Stat(dropboxCursorPath(root)); !os.IsNotExist(err) {
		t.Fatalf("cursor not removed after reaching EOF: %v", err)
	}
	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var source, uri string
	var flags int
	if err := check.QueryRow(`SELECT coalesce(streamingSource,''), coalesce(uri,''), coalesce(streamingFlags,0) FROM Track WHERE id=73`).Scan(&source, &uri, &flags); err != nil {
		t.Fatal(err)
	}
	if source != "" || uri != "" || flags != 0 {
		t.Fatalf("trailing copied WAV still tagged: source=%q uri=%q flags=%d", source, uri, flags)
	}
}

func TestDropboxDiscoveryHonorsContextDeadlineAndResumes(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<5000)
		INSERT INTO Track(id,path) SELECT i,printf('../Music/%d.wav',i) FROM n`); err != nil {
		t.Fatal(err)
	}
	// Already-cancelled context must return immediately with zero work.
	cancelledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	start := time.Now()
	rem, err := localizeDropboxTracks(cancelledCtx, db, root, &Report{})
	if err != nil || rem == 0 {
		t.Fatalf("cancelled ctx: rem=%d err=%v", rem, err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("cancelled ctx took %s", elapsed)
	}

	// Tight deadline interrupts paged scan promptly and resumes cleanly to EOF.
	tightCtx, cancelTight := context.WithTimeout(context.Background(), 1*time.Millisecond)
	_, err = localizeDropboxTracks(tightCtx, db, root, &Report{})
	cancelTight()
	if err != nil {
		t.Fatal(err)
	}
	// Subsequent unlimited pass completes and removes any saved cursor.
	rem, err = localizeDropboxTracks(context.Background(), db, root, &Report{})
	if err != nil || rem != 0 {
		t.Fatalf("unlimited follow-up: rem=%d err=%v", rem, err)
	}
	if _, err := os.Stat(dropboxCursorPath(root)); !os.IsNotExist(err) {
		t.Fatalf("cursor left behind after unlimited completion: %v", err)
	}
}

func TestHealMountDropboxRespectsBridgeStateAndReadOnly(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	full := filepath.Join(root, "Music", "track.mp3")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO Track(id,path,filename,fileBytes,streamingSource,uri,streamingFlags,isAnalyzed) VALUES(1,'../Music/track.mp3','track.mp3',5,'Dropbox','streaming://Dropbox/Track/track.mp3',5,1)`,
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Unreadable bridge state skips localizing tracks conservatively.
	state := filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-state.db")
	if err := os.WriteFile(state, []byte("not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := HealMountDataOnly(root, 0)
	if err != nil || got.DropboxLocalized != 0 {
		t.Fatalf("unreadable bridge state modified dropbox track: %+v %v", got, err)
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}

	// Read-only library is inspected without modification or cursor creation.
	dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	if err := os.Chmod(dbPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o644) })
	got, err = HealMountDataOnly(root, 0)
	if err != nil || !got.ReadOnly || got.DropboxLocalized != 0 {
		t.Fatalf("read-only media modified dropbox track: %+v %v", got, err)
	}
	if _, err := os.Stat(dropboxCursorPath(root)); !os.IsNotExist(err) {
		t.Fatalf("read-only media created cursor: %v", err)
	}
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatal(err)
	}

	// Unsupported future schema is left untouched and reported by HealMount.
	udb, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := udb.Exec(`UPDATE Information SET schemaVersionMajor=4`); err != nil {
		udb.Close()
		t.Fatal(err)
	}
	udb.Close()
	got, err = HealMountDataOnly(root, 0)
	if err == nil || got.DropboxLocalized != 0 {
		t.Fatalf("unsupported schema unexpectedly localized: %+v %v", got, err)
	}
	udb, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer udb.Close()
	var src string
	if err := udb.QueryRow(`SELECT COALESCE(streamingSource,'') FROM Track WHERE id=1`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "Dropbox" {
		t.Fatalf("unsupported schema modified Track.streamingSource=%q", src)
	}
	// Restoring schema 3.0.2 and running full HealMount localizes the track too.
	if _, err := udb.Exec(`UPDATE Information SET schemaVersionMajor=3`); err != nil {
		t.Fatal(err)
	}
	udb.Close()
	got, err = HealMount(root, 0)
	if err != nil || !got.Healthy || got.DropboxLocalized != 1 {
		t.Fatalf("full HealMount did not localize dropbox track: %+v %v", got, err)
	}
}

func TestHealMountMixedNFDAndDropboxPreservesIdentityAndFinishes(t *testing.T) {
	for _, mode := range []string{"data-only", "full"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			testlibrary.ExportDrive(t, root)
			music := filepath.Join(root, "Music")
			if err := os.Rename(filepath.Join(music, "Track002.wav"), filepath.Join(music, "Café.wav")); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			nfd := norm.NFD.String("Café.wav")
			if _, err := db.Exec(`UPDATE Track SET path=?,filename=? WHERE id=2`, "../Music/"+nfd, nfd); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE Track SET path='../Music/other1.wav', filename='other1.wav' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE Track SET path='../Music/Track001.wav', filename='Track001.wav', fileBytes=(SELECT fileBytes FROM Track WHERE id=1), streamingSource='Dropbox', uri='streaming://Dropbox/Track/3', streamingFlags=5 WHERE id=3`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE Information SET id=99`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES(99,1,0,999)`); err != nil {
				t.Fatal(err)
			}
			db.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			t.Cleanup(func() { normalizeTrackPathBatch = nil })
			var got Report
			if mode == "full" {
				normalizeTrackPathBatch = func() { time.Sleep(1050 * time.Millisecond) }
				got, err = HealMount(root, 0)
			} else {
				normalizeTrackPathBatch = cancel
				got, err = HealMountDataOnlyContext(ctx, root, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			var infoID, nextEntity int
			var src3 string
			if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
				t.Fatal(err)
			}
			if err := check.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=99`).Scan(&nextEntity); err != nil {
				t.Fatal(err)
			}
			if err := check.QueryRow(`SELECT coalesce(streamingSource,'') FROM Track WHERE id=3`).Scan(&src3); err != nil {
				t.Fatal(err)
			}
			if !got.Healthy || infoID != 1 {
				t.Fatalf("mode=%s healthy=%v Information.id=%d", mode, got.Healthy, infoID)
			}
			if mode == "full" {
				if nextEntity != 0 || src3 != "" {
					t.Fatalf("full heal did not finish: nextEntity=%d src3=%q", nextEntity, src3)
				}
			} else {
				if nextEntity != 999 {
					t.Fatalf("data-only soft stop ran heavy base heal: nextEntity=%d", nextEntity)
				}
				// Next pass must resume after NFD id=2, localize Dropbox id=3, and finish base heal.
				normalizeTrackPathBatch = nil
				check.Close()
				got2, err := HealMountDataOnly(root, 0)
				if err != nil || !got2.Healthy || got2.DropboxLocalized != 1 {
					t.Fatalf("follow-up pass did not localize Dropbox track: %+v %v", got2, err)
				}
				check2, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer check2.Close()
				if err := check2.QueryRow(`SELECT coalesce(streamingSource,'') FROM Track WHERE id=3`).Scan(&src3); err != nil {
					t.Fatal(err)
				}
				if err := check2.QueryRow(`SELECT nextEntityId FROM PlaylistEntity WHERE id=99`).Scan(&nextEntity); err != nil {
					t.Fatal(err)
				}
				if src3 != "" || nextEntity != 0 {
					t.Fatalf("follow-up pass incomplete: src3=%q nextEntity=%d", src3, nextEntity)
				}
			}
		})
	}
}

func TestHealMountDataOnlyPublicAPIResumesLargeLibraryAcrossDeadlines(t *testing.T) {
	const totalTracks = 20000
	root := t.TempDir()
	testlibrary.ExportDrive(t, root)
	dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET path='../Music/Track001.wav', filename='Track001.wav', streamingSource='Dropbox', uri='streaming://Dropbox/Track/1', streamingFlags=5 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (VALUES(9) UNION ALL SELECT i+1 FROM n WHERE i<` + strconv.Itoa(totalTracks) + `)
		INSERT INTO Track(id,path) SELECT i,printf('../Music/%d.wav',i) FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET path='../Music/Track002.wav', filename='Track002.wav', fileBytes=(SELECT fileBytes FROM Track WHERE id=2),
		streamingSource='Dropbox', uri='streaming://Dropbox/Track/tail', streamingFlags=5 WHERE id=?`, totalTracks); err != nil {
		t.Fatal(err)
	}
	// Calibrate per-pass soft budget to this host (including -race instrumentation)
	// so every run exercises multiple deadline-interrupted passes without flaking.
	openStart := time.Now()
	_ = libraryInUse(root)
	probeDB, err := Open(dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	openDur := time.Since(openStart)
	scanStart := time.Now()
	if _, err := discoverMountTrackCandidates(context.Background(), probeDB, true, 0); err != nil {
		probeDB.Close()
		t.Fatal(err)
	}
	scanDur := time.Since(scanStart)
	probeDB.Close()
	db.Close()

	passBudget := openDur + scanDur/3
	if minBudget := openDur + time.Millisecond; passBudget < minBudget {
		passBudget = minBudget
	}
	maxElapsed := max(passBudget*4, 2*time.Second)

	tagged := func() int {
		t.Helper()
		c, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		var n int
		if err := c.QueryRow(`SELECT count(*) FROM Track WHERE id IN (1,?) AND streamingSource='Dropbox'`, totalTracks).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	cursorSeen := 0
	for pass := 1; pass <= 20 && tagged() > 0; pass++ {
		ctx, cancel := context.WithTimeout(context.Background(), passBudget)
		start := time.Now()
		rep, err := HealMountDataOnlyContext(ctx, root, 0)
		elapsed := time.Since(start)
		cancel()
		if err != nil {
			t.Fatalf("pass=%d unexpected error (must soft-stop, not fail or trigger recovery): %v (elapsed=%s)", pass, err, elapsed)
		}
		if !rep.Healthy {
			t.Fatalf("pass=%d not healthy: %+v", pass, rep)
		}
		if elapsed > maxElapsed {
			t.Fatalf("pass=%d overran %s soft budget to %s (unbounded recovery leaked)", pass, passBudget, elapsed)
		}
		if _, statErr := os.Stat(dropboxCursorPath(root)); statErr == nil {
			cursorSeen++
		} else if !os.IsNotExist(statErr) {
			t.Fatal(statErr)
		}
	}
	if rem := tagged(); rem != 0 {
		t.Fatalf("bounded passes left %d Dropbox tracks unlocalized (budget=%s open=%s scan=%s cursorSeen=%d)", rem, passBudget, openDur, scanDur, cursorSeen)
	}
	if cursorSeen == 0 {
		t.Fatalf("expected at least one deadline-resumed pass with cursor (budget=%s open=%s scan=%s)", passBudget, openDur, scanDur)
	}
	if _, err := os.Stat(dropboxCursorPath(root)); !os.IsNotExist(err) {
		t.Fatalf("cursor not removed after finishing large library: %v", err)
	}
}

func TestHealMountDataOnlyPropagatesWriteRejectionOnCancel(t *testing.T) {
	fnName := fmt.Sprintf("dbhealer_write_reject_%d", time.Now().UnixNano())
	var signal func()
	calls := 0
	if err := sqlite.RegisterScalarFunction(fnName, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		calls++
		if signal != nil {
			signal()
		}
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, cancelDuringWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel_%t", cancelDuringWrite), func(t *testing.T) {
			root := t.TempDir()
			testlibrary.ExportDrive(t, root)
			dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE Track SET path='../Music/Track001.wav', filename='Track001.wav', streamingSource='Dropbox', uri='streaming://Dropbox/Track/1', streamingFlags=5 WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TRIGGER reject_dropbox_update BEFORE UPDATE OF streamingSource ON Track WHEN OLD.id=1 BEGIN SELECT ` + fnName + `(); SELECT RAISE(ABORT,'simulated write rejected'); END`); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			signal = nil
			if cancelDuringWrite {
				signal = cancel
			}
			beforeCalls := calls
			rep, err := HealMountDataOnlyContext(ctx, root, 0)
			var src string
			if qerr := db.QueryRow(`SELECT coalesce(streamingSource,'') FROM Track WHERE id=1`).Scan(&src); qerr != nil {
				t.Fatal(qerr)
			}
			if calls != beforeCalls+1 {
				t.Fatalf("trigger calls=%d, want %d", calls-beforeCalls, 1)
			}
			if src != "Dropbox" {
				t.Fatalf("rejected write altered streamingSource=%q", src)
			}
			if err == nil || !strings.Contains(err.Error(), "simulated write rejected") {
				t.Fatalf("expected write rejection error, got rep=%+v err=%v", rep, err)
			}
			if rep.Healthy {
				t.Fatalf("rejected write reported Healthy=true: %+v", rep)
			}
		})
	}
}

func TestHealMountDataOnlyRejectsMissingOrUnsupportedSchemaUnderShortBudget(t *testing.T) {
	for _, mode := range []string{"future", "missing"} {
		t.Run(mode, func(t *testing.T) {
			_, root, db := testlibrary.Create(t)
			if _, err := db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<4200)
				INSERT INTO Track(id,path) SELECT i,printf('../Music/%d.wav',i) FROM n`); err != nil {
				t.Fatal(err)
			}
			if mode == "future" {
				if _, err := db.Exec(`UPDATE Information SET schemaVersionMajor=4`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.Exec(`DELETE FROM Information`); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 190*time.Millisecond)
			shortRep, shortErr := HealMountDataOnlyContext(ctx, root, 0)
			cancel()
			if shortErr == nil || shortRep.Healthy {
				t.Fatalf("short budget accepted %s schema: rep=%+v err=%v", mode, shortRep, shortErr)
			}

			fullRep, fullErr := HealMountDataOnly(root, 0)
			if fullErr == nil || fullRep.Healthy {
				t.Fatalf("unlimited pass accepted %s schema: rep=%+v err=%v", mode, fullRep, fullErr)
			}
		})
	}
}

func TestHealMountMixedPageInterleavesNFDAndDropboxWithoutStarvation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		dropboxAfter bool
		exactNFDFile bool
	}{
		{name: "dropbox_first_missing_nfd", dropboxAfter: false, exactNFDFile: false},
		{name: "dropbox_first_exact_nfd", dropboxAfter: false, exactNFDFile: true},
		{name: "nfd_first_exact_nfd", dropboxAfter: true, exactNFDFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			testlibrary.ExportDrive(t, root)
			dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			targetID := 1
			if !tc.dropboxAfter {
				if _, err := db.Exec(`UPDATE Track SET path='../Music/Track001.wav', filename='Track001.wav', streamingSource='Dropbox', uri='streaming://Dropbox/Track/1', streamingFlags=5 WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			}
			for id := 9; id <= 73; id++ {
				name := norm.NFD.String(fmt.Sprintf("Café-%d.wav", id))
				if tc.exactNFDFile {
					if err := os.Link(filepath.Join(root, "Music", "Track002.wav"), filepath.Join(root, "Music", name)); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := db.Exec(`INSERT INTO Track(id,path,filename) VALUES(?,?,?)`, id, "../Music/"+name, name); err != nil {
					t.Fatal(err)
				}
			}
			if tc.dropboxAfter {
				targetID = 74
				if _, err := db.Exec(`UPDATE Track SET path='../Music/other1.wav', filename='other1.wav' WHERE id=1`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes,streamingSource,uri,streamingFlags)
					SELECT 74,'../Music/Track001.wav','Track001.wav',fileBytes,'Dropbox','streaming://Dropbox/Track/74',5 FROM Track WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			}

			source := func() string {
				t.Helper()
				var s string
				if err := db.QueryRow(`SELECT coalesce(streamingSource,'') FROM Track WHERE id=?`, targetID).Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			t.Cleanup(func() { normalizeTrackPathBatch = nil })
			for pass := 1; pass <= 4 && source() != ""; pass++ {
				ctx, cancel := context.WithCancel(context.Background())
				normalizeTrackPathBatch = cancel
				rep, err := HealMountDataOnlyContext(ctx, root, 0)
				cancel()
				if err != nil || !rep.Healthy {
					t.Fatalf("pass %d failed: %+v %v", pass, rep, err)
				}
			}
			if got := source(); got != "" {
				t.Fatalf("mixed page starved Dropbox track id=%d across 4 bounded passes (streamingSource=%q)", targetID, got)
			}
		})
	}
}

func TestHealMountURIOnlyNFDDropboxSingleUnitNoStarvation(t *testing.T) {
	setup := func(t *testing.T, onDiskNFC bool) (string, *sql.DB, string) {
		t.Helper()
		root := t.TempDir()
		testlibrary.ExportDrive(t, root)
		dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		nfdName := norm.NFD.String("Café.wav")
		diskName := nfdName
		if onDiskNFC {
			diskName = norm.NFC.String("Café.wav")
		}
		music := filepath.Join(root, "Music")
		audio := filepath.Join(music, diskName)
		if err := os.Rename(filepath.Join(music, "Track001.wav"), audio); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET path=?, filename=?, streamingSource=NULL, uri='streaming://Dropbox/Track/1', streamingFlags=5 WHERE id=1`, "../Music/"+nfdName, nfdName); err != nil {
			t.Fatal(err)
		}
		return root, db, audio
	}

	isTagged := func(t *testing.T, db *sql.DB) bool {
		t.Helper()
		var uri string
		var flags int
		if err := db.QueryRow(`SELECT coalesce(uri,''), coalesce(streamingFlags,0) FROM Track WHERE id=1`).Scan(&uri, &flags); err != nil {
			t.Fatal(err)
		}
		return uri != "" || flags != 0
	}

	t.Run("exact_nfd_skips_nfd_batch_and_localizes_in_one_pass", func(t *testing.T) {
		root, db, _ := setup(t, false)
		t.Cleanup(func() { normalizeTrackPathBatch = nil })
		nfdBatches := 0
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		normalizeTrackPathBatch = func() { nfdBatches++; cancel() }
		rep, err := HealMountDataOnlyContext(ctx, root, 0)
		if err != nil || !rep.Healthy {
			t.Fatalf("mount heal failed: %+v %v", rep, err)
		}
		if nfdBatches != 0 || rep.DropboxLocalized != 1 || isTagged(t, db) {
			t.Fatalf("expected single-unit Dropbox localization without NFD batch: nfdBatches=%d rep=%+v tagged=%v", nfdBatches, rep, isTagged(t, db))
		}
	})

	t.Run("exact_nfd_in_large_directory_does_not_read_dir", func(t *testing.T) {
		root, db, audio := setup(t, false)
		dir := filepath.Dir(audio)
		for i := 0; i < 4000; i++ {
			if err := os.Link(audio, filepath.Join(dir, fmt.Sprintf("Music-%05d.wav", i))); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { normalizeTrackPathBatch = nil })
		nfdBatches := 0
		normalizeTrackPathBatch = func() { nfdBatches++ }
		rep, err := HealMountDataOnly(root, 0)
		if err != nil || !rep.Healthy {
			t.Fatalf("large-dir mount heal failed: %+v %v", rep, err)
		}
		if nfdBatches != 0 || rep.DropboxLocalized != 1 || isTagged(t, db) {
			t.Fatalf("expected direct lstat localization without NFD directory scan: nfdBatches=%d rep=%+v tagged=%v", nfdBatches, rep, isTagged(t, db))
		}
	})

	t.Run("nfc_on_disk_normalizes_and_localizes_atomically", func(t *testing.T) {
		root, db, _ := setup(t, true)
		rep, err := HealMountDataOnly(root, 0)
		if err != nil || !rep.Healthy || rep.DropboxLocalized != 1 || rep.PathsNormalized != 1 {
			t.Fatalf("expected atomic normalize+localize: %+v %v", rep, err)
		}
		var path string
		if err := db.QueryRow(`SELECT path FROM Track WHERE id=1`).Scan(&path); err != nil {
			t.Fatal(err)
		}
		if !norm.NFC.IsNormalString(path) || isTagged(t, db) {
			t.Fatalf("track not normalized+localized: path=%+q tagged=%v", path, isTagged(t, db))
		}
	})
}

func TestHealMountDataOnlyPreservesWriteRejectionWhenRecoveryCanceled(t *testing.T) {
	suffix := time.Now().UnixNano()
	rejectFn := fmt.Sprintf("dbhealer_write_reject_rec_%d", suffix)
	cancelFn := fmt.Sprintf("dbhealer_cancel_rec_%d", suffix)
	var cancelCtx func()
	if err := sqlite.RegisterScalarFunction(rejectFn, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.RegisterScalarFunction(cancelFn, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if cancelCtx != nil {
			cancelCtx()
		}
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	testlibrary.ExportDrive(t, root)
	dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE Track SET path='../Music/Track001.wav', filename='Track001.wav', streamingSource='Dropbox', uri='streaming://Dropbox/Track/1', streamingFlags=5 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO AlbumArt(id,hash,albumArt) VALUES(99, X'0001', X'')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_dropbox_update BEFORE UPDATE OF streamingSource ON Track WHEN OLD.id=1 BEGIN SELECT ` + rejectFn + `(); SELECT RAISE(ABORT,'simulated write rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER cancel_during_recovery BEFORE UPDATE OF albumArt ON AlbumArt WHEN OLD.id=99 BEGIN SELECT ` + cancelFn + `(); END`); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelCtx = cancel

	rep, err := HealMountDataOnlyContext(ctx, root, 0)
	if err == nil || !strings.Contains(err.Error(), "simulated write rejected") {
		t.Fatalf("expected original write rejection error, got rep=%+v err=%v", rep, err)
	}
	if rep.Healthy {
		t.Fatalf("rejected write with canceled recovery reported Healthy=true: %+v", rep)
	}
}

func TestHealMountDataOnlyAlreadyCanceledValidatesSchemaAndRepairsIdentity(t *testing.T) {
	t.Run("future_schema_rejected", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET schemaVersionMajor=4`); err != nil {
			t.Fatal(err)
		}
		db.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rep, err := HealMountDataOnlyContext(ctx, root, 0)
		if err == nil || rep.Healthy {
			t.Fatalf("already-canceled context accepted schema 4: rep=%+v err=%v", rep, err)
		}
	})

	t.Run("bad_identity_repaired_without_track_pass", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		full := filepath.Join(root, "Music", "track.mp3")
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Information SET id=99`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes,streamingSource,uri,streamingFlags,isAnalyzed) VALUES(1,'../Music/track.mp3','track.mp3',5,'Dropbox','streaming://Dropbox/Track/1',5,1)`); err != nil {
			t.Fatal(err)
		}
		db.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rep, err := HealMountDataOnlyContext(ctx, root, 0)
		if err != nil || !rep.Healthy {
			t.Fatalf("already-canceled context failed identity check: rep=%+v err=%v", rep, err)
		}
		if rep.DropboxLocalized != 0 {
			t.Fatalf("already-canceled context ran Dropbox track pass: %+v", rep)
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
			t.Fatalf("Information.id=%d, want 1 under already-canceled context", infoID)
		}
	})
}

func TestHealMountDataOnlyDoesNotOverrunDeadlineWhenLockedAfterBatch(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	full := filepath.Join(root, "Music", "track1.wav")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes,streamingSource,uri,streamingFlags,isAnalyzed) VALUES(1,'../Music/track1.wav','track1.wav',5,'Dropbox','streaming://Dropbox/Track/1',5,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (VALUES(2) UNION ALL SELECT i+1 FROM n WHERE i<=1025)
		INSERT INTO Track(id,path,isAnalyzed) SELECT i,printf('../Music/%d.wav',i),1 FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Information SET id=99`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	locker, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	locker.SetMaxOpenConns(1)
	defer locker.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()

	relDone := make(chan struct{})
	relOnce := false
	localizeDropboxTrackBatch = func() {
		if relOnce {
			return
		}
		relOnce = true
		if _, lerr := locker.Exec("BEGIN EXCLUSIVE"); lerr != nil {
			close(relDone)
			t.Errorf("BEGIN EXCLUSIVE: %v", lerr)
			return
		}
		go func() {
			defer close(relDone)
			wait := time.Until(deadline) + 180*time.Millisecond
			if wait > 0 {
				time.Sleep(wait)
			}
			_, _ = locker.Exec("ROLLBACK")
		}()
	}
	t.Cleanup(func() {
		localizeDropboxTrackBatch = nil
		if relOnce {
			<-relDone
		}
	})

	rep, err := HealMountDataOnlyContext(ctx, root, 0)
	overrun := time.Since(deadline)
	if err != nil || !rep.Healthy {
		t.Fatalf("locked post-batch mount heal: rep=%+v err=%v", rep, err)
	}
	if overrun >= 120*time.Millisecond {
		t.Fatalf("mount heal overran deadline by %v while deferred identity waited on lock", overrun)
	}

	check, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var infoID int
	if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
		t.Fatal(err)
	}
	if infoID != 1 {
		t.Fatalf("Information.id=%d, want 1", infoID)
	}
}
