package dbhealer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"golang.org/x/text/unicode/norm"
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

	// Local Dropbox audio files present on the SD card.
	writeFile("../Music/both.mp3", []byte("audio-1"))
	writeFile("../Music/packtracks.mp3", []byte("audio-2"))
	writeFile("../Music/source-only.mp3", []byte("audio-3"))
	writeFile("../Music/lowercase.mp3", []byte("audio-4"))
	writeFile("../Music/Café-pack.mp3", []byte("audio-5"))

	// Negative-control files that must NEVER be localized.
	writeFile("../Music/empty.mp3", nil)
	writeFile("../Music/tidal.mp3", []byte("audio-tidal"))
	writeFile("../Music/beatport.mp3", []byte("audio-beatport"))
	writeFile("../Music/mismatched-src.mp3", []byte("audio-mismatch1"))
	writeFile("../Music/mismatched-uri.mp3", []byte("audio-mismatch2"))
	writeFile("../Music/local-explicit.mp3", []byte("audio-explicit"))
	if err := os.MkdirAll(filepath.Join(root, "Music", "dir-not-file.mp3"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.mp3")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
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
	rows := []struct {
		id         int
		path       string
		src        any
		uri        any
		flags      int
		lastEdit   int64
		originUUID string
		originID   int
		wantClear  bool
	}{
		// Localizable Dropbox cases.
		{1, "../Music/both.mp3", "Dropbox", "streaming://Dropbox/Track/Music/both.mp3", 5, 1700000001, "orig-db", 101, true},
		{2, "../Music/packtracks.mp3", "", "streaming://Dropbox/Track/Music/packtracks.mp3", 5, 1700000002, "orig-db", 102, true},
		{3, "../Music/source-only.mp3", "Dropbox", nil, 5, 1700000003, "orig-db", 103, true},
		{4, "../Music/lowercase.mp3", "dropbox", "streaming://dropbox/Track/Music/lowercase.mp3", 5, 1700000004, "orig-db", 104, true},
		{5, nfdPackPath, "", "streaming://Dropbox/Track/Music/Cafe.mp3", 5, 1700000005, "orig-db", 105, true},
		// Must stay untouched.
		{10, "../Music/missing-cloud-only.mp3", "Dropbox", "streaming://Dropbox/Track/Music/missing.mp3", 5, 1700000010, "orig-db", 110, false},
		{11, "../Music/empty.mp3", "Dropbox", "streaming://Dropbox/Track/Music/empty.mp3", 5, 1700000011, "orig-db", 111, false},
		{12, "../Music/dir-not-file.mp3", "Dropbox", "streaming://Dropbox/Track/Music/dir.mp3", 5, 1700000012, "orig-db", 112, false},
		{13, "../Music/escape-link.mp3", "Dropbox", "streaming://Dropbox/Track/Music/escape.mp3", 5, 1700000013, "orig-db", 113, false},
		{14, "../Music/internal-link.mp3", "Dropbox", "streaming://Dropbox/Track/Music/internal.mp3", 5, 1700000014, "orig-db", 114, false},
		{15, "../LinkDir/outside.mp3", "Dropbox", "streaming://Dropbox/Track/LinkDir/outside.mp3", 5, 1700000015, "orig-db", 115, false},
		{16, "../../outside.mp3", "Dropbox", "streaming://Dropbox/Track/outside.mp3", 5, 1700000016, "orig-db", 116, false},
		{17, outside, "Dropbox", "streaming://Dropbox/Track/abs.mp3", 5, 1700000017, "orig-db", 117, false},
		{18, "../Music/tidal.mp3", "TIDAL", "streaming://TIDAL/Track/9001", 1, 1700000018, "orig-db", 118, false},
		{19, "../Music/beatport.mp3", "Beatport", "streaming://Beatport/Track/9002", 1, 1700000019, "orig-db", 119, false},
		{20, "../Music/mismatched-src.mp3", "TIDAL", "streaming://Dropbox/Track/spoof.mp3", 5, 1700000020, "orig-db", 120, false},
		{21, "../Music/mismatched-uri.mp3", "Dropbox", "streaming://TIDAL/Track/9003", 5, 1700000021, "orig-db", 121, false},
		{22, "../Music/local-explicit.mp3", nil, nil, 2, 1700000022, "orig-db", 122, false},
	}
	validTrack := rawTrack(1e7)
	validBeat := rawBeats(nil)
	validWave := placeholder(1e7)
	for _, row := range rows {
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,fileBytes,streamingSource,uri,streamingFlags,originDatabaseUuid,originTrackId,isAnalyzed) VALUES(?,?,?,7,?,?,?,?,?,1)`,
			row.id, row.path, filepath.Base(row.path), row.src, row.uri, row.flags, row.originUUID, row.originID,
		); err != nil {
			t.Fatalf("insert row %d: %v", row.id, err)
		}
		if _, err := db.Exec(
			`UPDATE PerformanceData SET trackData=?, beatData=?, overviewWaveFormData=? WHERE trackId=?`,
			validTrack, validBeat, validWave, row.id,
		); err != nil {
			t.Fatalf("seed PerformanceData row %d: %v", row.id, err)
		}
		// Set lastEditTime after PerformanceData seed so trigger_PerformanceData_after_update_Track_timestamp does not overwrite it.
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
	if !got.Healthy || got.DropboxLocalized != 5 || got.PathsNormalized != 1 {
		t.Fatalf("unexpected report: %+v (want healthy, dropbox=5, paths=1)", got)
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
		var flags int
		var lastEdit int64
		var originUUID string
		var originID int
		if err := check.QueryRow(
			`SELECT streamingSource, uri, COALESCE(streamingFlags,0), lastEditTime, originDatabaseUuid, originTrackId FROM Track WHERE id=?`,
			row.id,
		).Scan(&src, &uri, &flags, &lastEdit, &originUUID, &originID); err != nil {
			t.Fatalf("read row %d: %v", row.id, err)
		}
		if lastEdit != row.lastEdit || originUUID != row.originUUID || originID != row.originID {
			t.Errorf("row %d sync metadata changed: lastEdit=%d origin=(%q,%d)", row.id, lastEdit, originUUID, originID)
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
	if _, err := db.Exec(`UPDATE Information SET id=99`); err != nil {
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

	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var infoID int
	if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
		t.Fatal(err)
	}
	if infoID != 99 {
		t.Fatalf("base heal ran during soft dropbox budget stop: Information.id=%d", infoID)
	}

	// Second pass finishes remaining 66 tracks and runs base heal.
	localizeDropboxTrackBatch = nil
	got, err = HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy || got.DropboxLocalized != 66 {
		t.Fatalf("second pass: %+v %v (want healthy, dropbox=66)", got, err)
	}
	if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
		t.Fatal(err)
	}
	if infoID != 1 {
		t.Fatalf("base heal did not run after fast completion: Information.id=%d", infoID)
	}
}

func TestHealMountDropboxSlowPathDefersBaseHeal(t *testing.T) {
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
	db.Close()

	localizeDropboxTrackBatch = func() { time.Sleep(time.Second) }
	defer func() { localizeDropboxTrackBatch = nil }()

	got, err := HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy || got.DropboxLocalized != 64 {
		t.Fatalf("slow dropbox pass: %+v %v", got, err)
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
	if infoID != 99 {
		t.Fatalf("slow dropbox pass ran base heal, Information.id=%d", infoID)
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

	// Read-only library is inspected without modification.
	dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	if err := os.Chmod(dbPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o644) })
	got, err = HealMountDataOnly(root, 0)
	if err != nil || !got.ReadOnly || got.DropboxLocalized != 0 {
		t.Fatalf("read-only media modified dropbox track: %+v %v", got, err)
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
