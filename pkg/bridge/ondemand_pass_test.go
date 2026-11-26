package bridge

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestOnDemandMountSkipsPerfPassCreatesDirectArtAndAllowsUnchangedBypass(t *testing.T) {
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, 2)
	fix3WritePlainOneLibrary(t, mount)
	// Give both tracks the same ArtworkPath (deduplication test in build()).
	onePath := filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")
	odb, err := sql.Open("sqlite", onePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := odb.Exec(`
		ALTER TABLE content ADD COLUMN image_path varchar;
		UPDATE content SET image_path='/PIONEER/Artwork/00001/shared.jpg' WHERE content_id IN (1, 2);
	`); err != nil {
		_ = odb.Close()
		t.Fatal(err)
	}
	_ = odb.Close()

	bgStarted := 0
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{
		OnDemand:        true,
		StartBackground: func() error { bgStarted++; return nil },
	})
	if err != nil {
		t.Fatalf("ConvertAtMount(OnDemand): %v", err)
	}
	if res.Added != 2 || res.Background || bgStarted != 0 || !res.Pending.None() {
		t.Fatalf("OnDemand mount result = %+v, bgStarted=%d; want Added=2, Background=false, Pending.None()", res, bgStarted)
	}

	// m.db must stay isAnalyzed=0 on disk (honest for stock Denon decks) and hold 1 deduplicated hash-only AlbumArt row.
	if n := query(t, mount, `SELECT count(*) FROM Track WHERE isAnalyzed=0 AND albumArtId IS NOT NULL`); n[0][0] != "2" {
		t.Fatalf("isAnalyzed=0 with albumArtId count = %s, want 2", n[0][0])
	}
	if n := query(t, mount, `SELECT count(*) FROM AlbumArt WHERE hash IS NOT NULL AND albumArt IS NULL`); n[0][0] != "1" {
		t.Fatalf("deduplicated hash-only AlbumArt rows = %s, want 1", n[0][0])
	}
	if n := query(t, mount, `SELECT count(*) FROM AlbumArt WHERE albumArt IS NOT NULL`); n[0][0] != "0" {
		t.Fatalf("unexpected non-NULL AlbumArt BLOB rows = %s, want 0", n[0][0])
	}

	// .rb-bridge-browse must exist beside .rb-bridge-snapshot with both tracks and the deduplicated cover.
	browseRaw, err := os.ReadFile(filepath.Join(dbDir(mount), rbstate.BrowseIndexName))
	if err != nil {
		t.Fatalf("missing %s: %v", rbstate.BrowseIndexName, err)
	}
	if !strings.HasPrefix(string(browseRaw), "RBB1 ") ||
		!strings.Contains(string(browseRaw), rbstate.DirectArtworkStem("/PIONEER/Artwork/00001/shared.jpg")) {
		t.Fatalf("unexpected browse index:\n%s", browseRaw)
	}

	// Re-insert unchanged stick -> instant stat-only bypass!
	reRes, err := ConvertAtMount(context.Background(), mount, MountOptions{
		OnDemand:        true,
		StartBackground: func() error { bgStarted++; return nil },
	})
	if err != nil || !reRes.Bypassed || reRes.Background || bgStarted != 0 {
		t.Fatalf("unchanged OnDemand re-insert = %+v, err=%v, bgStarted=%d; want Bypassed=true", reRes, err, bgStarted)
	}

	// #321 write-back on a fresh OnDemand stick must read 0 rows (all untouched rows skipped via tr.Synced).
	wb, err := WriteBackCues(context.Background(), mount, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if wb.TracksChecked != 2 || wb.TracksRead != 0 || wb.TracksChanged != 0 {
		t.Fatalf("fresh OnDemand WriteBackCues = %+v, want Checked=2 Read=0 Changed=0", wb)
	}

	// FillInBackground(OnDemand: true) when rows/lists are current must do zero staging and zero publish.
	fillRes, err := FillInBackground(context.Background(), mount, FillOptions{OnDemand: true})
	if err != nil || fillRes.Staged != 0 || fillRes.Publish {
		t.Fatalf("FillInBackground(OnDemand) = %+v, err=%v; want Staged=0 Publish=false", fillRes, err)
	}
}

func TestOnDemandDeferredMountPublishesRowsOnceAndStops(t *testing.T) {
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, 4)

	// No snapshot yet: OnDemand FillInBackground builds .rb-next.db + .rb-bridge-browse.swap + .rb-bridge-snapshot.swap.
	fillRes, err := FillInBackground(context.Background(), mount, FillOptions{OnDemand: true})
	if err != nil || !fillRes.Publish {
		t.Fatalf("FillInBackground(OnDemand deferred) = %+v, err=%v; want Publish=true", fillRes, err)
	}
	dir := dbDir(mount)
	for _, f := range []string{nextDB, rbstate.SwapName, rbstate.BrowseSwapName} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("expected %s before remount swap: %v", f, err)
		}
	}

	bgStarted := 0
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{
		OnDemand:        true,
		Publish:         true,
		StartBackground: func() error { bgStarted++; return nil },
	})
	if err != nil || !res.Pending.None() || res.Background || bgStarted != 0 {
		t.Fatalf("remount swap in OnDemand mode = %+v, err=%v, bgStarted=%d; want Pending.None() and no BG job", res, err, bgStarted)
	}
	for _, gone := range []string{nextDB, rbstate.SwapName, rbstate.BrowseSwapName} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("leftover %s after OnDemand swap: err=%v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, rbstate.BrowseIndexName)); err != nil {
		t.Fatalf("missing promoted %s after OnDemand swap: %v", rbstate.BrowseIndexName, err)
	}
}

// Fail-before/pass-after test for #321 readFilledStage with standalone filled.jsonl (wr + wrnm,
// NO index.jsonl and NO .bin stage files, as written by the CDJ on-demand load hook).
func TestWriteBackStandaloneFilledJSONLWithWRAndWRNM(t *testing.T) {
	base := t.TempDir()
	stageRoot := filepath.Join(base, "stage")
	t.Setenv(rbstate.StageEnv, stageRoot)
	mount := filepath.Join(base, "stick")
	testfixture.Export(t, mount)

	// Mount in OnDemand mode -> Pending.None(), no .bin stage files anywhere.
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{OnDemand: true})
	if err != nil || !res.Pending.None() {
		t.Fatalf("ConvertAtMount(OnDemand): res=%+v err=%v", res, err)
	}
	snap := snapshot(t, mount)
	var target *rbstate.Track
	for _, tr := range snap.Tracks {
		if strings.HasSuffix(tr.Key, "two.flac") {
			target = tr
			break
		}
	}
	if target == nil {
		t.Fatal("missing target track two.flac")
	}

	// Build PerformanceData blobs as the on-demand load hook would write on Engine's connection.
	perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 60, SampleRate: 44100, SampleCount: 60 * 44100, BPM: 128}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{Number: 1, BPM: 128, TimeMS: 0}, {Number: 2, BPM: 128, TimeMS: 468}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{20, 40, 60, 80, 100, 120}}},
		Cues:      []rekordbox.Cue{{HotCue: 1, TimeMS: 1000, Name: "intro"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wr := enginedb.WrittenDigest(perf.Beats, perf.QuickCues, perf.Loops)
	wrnm := writtenDigestIgnoringMainCueTail(perf.Beats, perf.QuickCues, perf.Loops)

	// Write standalone filled.jsonl (NO index.jsonl, NO .bin!).
	sdir := filepath.Join(stageRoot, snap.LibraryUUID)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	entry, _ := json.Marshal(map[string]any{
		"id":   target.EngineID,
		"k":    target.Key,
		"wr":   base64.StdEncoding.EncodeToString(wr),
		"wrnm": base64.StdEncoding.EncodeToString(wrnm),
	})
	if err := os.WriteFile(filepath.Join(sdir, "filled.jsonl"), append(entry, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// Case 1: Simulate on-demand fill + Engine normalizing the unset main cue tail (last 17 bytes),
	// bumping lastEditTime, WITHOUT any DJ cue edit. Write-back must see wrnm match -> needsCueRead == false!
	normalizedCues := recodeCues(t, perf.QuickCues, func(raw []byte) {
		binary.BigEndian.PutUint64(raw[len(raw)-17:], math.Float64bits(0))
		raw[len(raw)-9] = 0
		binary.BigEndian.PutUint64(raw[len(raw)-8:], math.Float64bits(0))
	})
	db, err := sql.Open("sqlite", filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, beatData=?, quickCues=?, loops=?, activeOnLoadLoops=0, thirdPartySourceId=NULL WHERE trackId=?`,
		perf.TrackData, perf.Beats, normalizedCues, perf.Loops, target.EngineID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET isAnalyzed=1, lastEditTime=1700000100 WHERE id=?`, target.EngineID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	// Verify readFilledStage populated written & writtenNoMain without any .bin or index.jsonl,
	// and needsCueRead returns false for the normalized main-cue tail!
	filled := readFilledStage(snap)
	gotFill, ok := filled[target.Key]
	if !ok || len(gotFill.written) == 0 || len(gotFill.writtenNoMain) == 0 {
		t.Fatalf("readFilledStage did not load standalone filled.jsonl: ok=%v row=%+v", ok, gotFill)
	}
	cand := dbCandidate{
		engineID:            target.EngineID,
		key:                 target.Key,
		track:               target,
		beatData:            perf.Beats,
		trackData:           perf.TrackData,
		quickCues:           normalizedCues,
		loops:               perf.Loops,
		writtenDigest:       enginedb.WrittenDigest(perf.Beats, normalizedCues, perf.Loops),
		rawWrittenDigest:    enginedb.RawWrittenDigest(perf.Beats, normalizedCues, perf.Loops),
		filledWritten:       gotFill.written,
		filledWrittenNoMain: gotFill.writtenNoMain,
	}
	if needsCueRead(cand) {
		t.Fatal("needsCueRead returned true for untouched on-demand fill with normalized main cue tail")
	}

	// Case 2: DJ adds a new hot cue on the deck -> neither wr nor wrnm matches -> needsCueRead == true!
	editedPerf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 60, SampleRate: 44100, SampleCount: 60 * 44100, BPM: 128}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{Number: 1, BPM: 128, TimeMS: 0}, {Number: 2, BPM: 128, TimeMS: 468}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{20, 40, 60, 80, 100, 120}}},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 1000, Name: "intro"},
			{HotCue: 2, TimeMS: 5000, Name: "drop"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cand.quickCues = editedPerf.QuickCues
	cand.writtenDigest = enginedb.WrittenDigest(perf.Beats, editedPerf.QuickCues, perf.Loops)
	cand.rawWrittenDigest = enginedb.RawWrittenDigest(perf.Beats, editedPerf.QuickCues, perf.Loops)
	if !needsCueRead(cand) {
		t.Fatal("needsCueRead returned false after DJ added a new hot cue on an on-demand loaded track")
	}
}

func TestOnDemandUnchangedUpgradeGeneratesMissingBrowseIndexAndWritebackChecksVersionRow(t *testing.T) {
	base := t.TempDir()
	stageRoot := filepath.Join(base, "stage")
	t.Setenv(rbstate.StageEnv, stageRoot)
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, 2)

	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{OnDemand: false, StartBackground: func() error { return nil }}); err != nil {
		t.Fatalf("initial mount: %v", err)
	}
	dir := dbDir(mount)
	browsePath := filepath.Join(dir, rbstate.BrowseIndexName)
	_ = os.Remove(browsePath)

	// Simulate an actual legacy base-format snapshot (ModeFull "", Pending.None, empty browse paths)
	// plus existing artwork and overview files that must survive until replaced.
	snap, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap.Mode = rbstate.ModeFull
	snap.Pending = (rbstate.Header{}).Pending
	for _, c := range snap.Tracks {
		c.AnalysisPath, c.ArtworkPath, c.Duration, c.SampleRate = "", "", 0, 0
	}
	if err := snap.SaveNext(dir); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.Promote(dir); err != nil {
		t.Fatal(err)
	}
	legacyOV := filepath.Join(dir, "OverviewData", snap.LibraryUUID, "999.rgb")
	if err := os.MkdirAll(filepath.Dir(legacyOV), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyOV, []byte("legacy-rgb"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyArt := filepath.Join(filepath.Dir(dir), "Artwork", "legacy.jpg")
	if err := os.MkdirAll(filepath.Dir(legacyArt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyArt, []byte("legacy-jpg"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Upgrading a base-format snapshot in OnDemand mode must NEVER emit a header-only browse stub;
	// it must hydrate real AnalysisPath rows, set ModeOnDemand, and preserve existing assets.
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{OnDemand: true})
	if err != nil || res.Bypassed {
		t.Fatalf("legacy OnDemand upgrade = %+v, err=%v; want hydration (Bypassed=false)", res, err)
	}
	rawBrowse, err := os.ReadFile(browsePath)
	if err != nil || !strings.Contains(string(rawBrowse), "\nT\t") {
		t.Fatalf("upgraded browse index missing hydrated T lines: err=%v\n%s", err, rawBrowse)
	}
	if _, err := os.Stat(legacyOV); err != nil {
		t.Fatalf("existing overview file removed during upgrade: %v", err)
	}
	if _, err := os.Stat(legacyArt); err != nil {
		t.Fatalf("existing artwork file removed during upgrade: %v", err)
	}
	upgradedSnap, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if upgradedSnap.Mode != rbstate.ModeOnDemand || !rbstate.BrowseIndexMatches(dir, upgradedSnap.Header) {
		t.Fatalf("upgraded snapshot Mode=%q / mismatched browse index", upgradedSnap.Mode)
	}

	// Mismatched-tag RBB1 on a current ModeOnDemand snapshot repairs in-window on fast bypass.
	if err := os.WriteFile(browsePath, []byte("RBB1 "+upgradedSnap.LibraryUUID+" stale-tag\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reRes, err := ConvertAtMount(context.Background(), mount, MountOptions{OnDemand: true})
	if err != nil || !reRes.Bypassed {
		t.Fatalf("ModeOnDemand mismatched-browse repair bypass = %+v, err=%v", reRes, err)
	}
	if !rbstate.BrowseIndexMatches(dir, upgradedSnap.Header) {
		t.Fatalf("stale browse tag not repaired on OnDemand bypass")
	}

	// FillInBackground(OnDemand: true) must also hydrate legacy ModeFull/cleared-path snapshots once.
	legacyStick := filepath.Join(base, "legacy-fill-stick")
	testfixture.LargeExport(t, legacyStick, 2)
	if _, err := ConvertAtMount(context.Background(), legacyStick, MountOptions{OnDemand: false, StartBackground: func() error { return nil }}); err != nil {
		t.Fatal(err)
	}
	legacyDir := dbDir(legacyStick)
	lsnap, err := rbstate.Load(legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	lsnap.Mode = rbstate.ModeFull
	lsnap.Pending = (rbstate.Header{}).Pending
	for _, c := range lsnap.Tracks {
		c.AnalysisPath, c.ArtworkPath, c.Duration, c.SampleRate = "", "", 0, 0
	}
	if err := lsnap.SaveNext(legacyDir); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.Promote(legacyDir); err != nil {
		t.Fatal(err)
	}
	if _, err := FillInBackground(context.Background(), legacyStick, FillOptions{OnDemand: true}); err != nil {
		t.Fatalf("FillInBackground(OnDemand legacy hydrate): %v", err)
	}
	lsnapAfter, err := rbstate.Load(legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	if lsnapAfter.Mode != rbstate.ModeOnDemand || !rbstate.BrowseIndexMatches(legacyDir, lsnapAfter.Header) {
		t.Fatalf("FillInBackground did not settle ModeOnDemand/browse tag: mode=%q", lsnapAfter.Mode)
	}
	fillBrowse, err := os.ReadFile(filepath.Join(legacyDir, rbstate.BrowseIndexName))
	if err != nil || !strings.Contains(string(fillBrowse), "\nT\t") {
		t.Fatalf("FillInBackground wrote unhydrated browse index: err=%v\n%s", err, fillBrowse)
	}

	// readFilledStage rejects standalone filled.jsonl entries with mismatched rule version or row hash.
	var tr *rbstate.Track
	for _, c := range upgradedSnap.Tracks {
		tr = c
		break
	}
	tr.Row = "expected-row-hash"
	sdir := filepath.Join(stageRoot, upgradedSnap.LibraryUUID)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	wr := base64.StdEncoding.EncodeToString(make([]byte, 16))
	badVer, _ := json.Marshal(map[string]any{"id": tr.EngineID, "k": tr.Key, "wr": wr, "v": rbstate.Rules + 99})
	badRow, _ := json.Marshal(map[string]any{"id": tr.EngineID, "k": tr.Key, "wr": wr, "v": rbstate.Rules, "w": "stale-row"})
	if err := os.WriteFile(filepath.Join(sdir, "filled.jsonl"), append(append(badVer, '\n'), append(badRow, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFilledStage(upgradedSnap); len(got) != 0 {
		t.Fatalf("readFilledStage accepted stale rule version / row hash: %+v", got)
	}
}

// RD-2 regression test (fails on f65d4fdf): when a mount window's remaining budget cannot finish a
// large playlist rebuild (e.g. 509 lists on UNTITLED), ConvertAtMount leaves the existing library
// usable, triggers StartBackground, FillInBackground(OnDemand: true) builds .rb-next.db + .swap +
// .rb-bridge-browse.swap (Publish=true), and the remount window swaps and publishes all 509 lists.
func TestOnDemandManyListsWindowDefersAndPublishesViaNext(t *testing.T) {
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "untitled")
	testfixture.LargeExport(t, mount, 4)

	// Start with an already-imported earlier library (10 playlists -> now 509 playlists in export).
	writePlaylists := func(count int) {
		encodeStr := func(s string) []byte {
			out := make([]byte, 4+len(s)*2)
			out[0] = 0x90
			binary.LittleEndian.PutUint16(out[1:], uint16(len(out)))
			for i, r := range s {
				binary.LittleEndian.PutUint16(out[4+2*i:], uint16(r))
			}
			return out
		}
		largeTrackPath := func(i int) string {
			return fmt.Sprintf("Contents/Artist%03d/Album%03d/Track%05d.mp3", (i-1)/10, (i-1)/5, i)
		}
		const pageSize = 4096
		types := []uint32{0, 7, 8}
		rows := make([][][]byte, len(types))
		for i := 1; i <= 4; i++ {
			b := make([]byte, 136)
			binary.LittleEndian.PutUint32(b[8:], 44100)
			binary.LittleEndian.PutUint32(b[16:], uint32(len(testfixture.MP3WithLAMETag())))
			binary.LittleEndian.PutUint32(b[48:], 128)
			binary.LittleEndian.PutUint32(b[56:], 12000)
			binary.LittleEndian.PutUint32(b[72:], uint32(i))
			binary.LittleEndian.PutUint16(b[84:], 8)
			for j := 0; j < 21; j++ {
				s := ""
				switch j {
				case 14:
					s = "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"
				case 17:
					s = fmt.Sprintf("Track %05d", i)
				case 20:
					s = "/" + largeTrackPath(i)
				}
				binary.LittleEndian.PutUint16(b[94+2*j:], uint16(len(b)))
				b = append(b, encodeStr(s)...)
			}
			rows[0] = append(rows[0], b)
		}
		for i := 1; i <= count; i++ {
			pl := make([]byte, 20)
			binary.LittleEndian.PutUint32(pl[8:], uint32(i))
			binary.LittleEndian.PutUint32(pl[12:], uint32(i))
			pl = append(pl, encodeStr(fmt.Sprintf("Set %03d", i))...)
			rows[1] = append(rows[1], pl)

			en := make([]byte, 12)
			binary.LittleEndian.PutUint32(en[0:], 1)
			binary.LittleEndian.PutUint32(en[4:], uint32((i-1)%4+1))
			binary.LittleEndian.PutUint32(en[8:], uint32(i))
			rows[2] = append(rows[2], en)
		}
		out := make([]byte, pageSize)
		binary.LittleEndian.PutUint32(out[4:], pageSize)
		binary.LittleEndian.PutUint32(out[8:], uint32(len(types)))
		for ti, typ := range types {
			first := len(out) / pageSize
			left := rows[ti]
			for len(left) > 0 {
				num := len(out) / pageSize
				p := make([]byte, pageSize)
				binary.LittleEndian.PutUint32(p[4:], uint32(num))
				binary.LittleEndian.PutUint32(p[8:], typ)
				p[27] = 0x24
				off, n := 40, 0
				for n < len(left) && off+len(left[n])+((n+16)/16)*36 <= pageSize {
					row := left[n]
					copy(p[off:], row)
					base := pageSize - (n/16)*36
					binary.LittleEndian.PutUint16(p[base-6-2*(n%16):], uint16(off-40))
					binary.LittleEndian.PutUint16(p[base-4:], binary.LittleEndian.Uint16(p[base-4:])|1<<uint(n%16))
					off += len(row)
					n++
				}
				if n == 0 {
					t.Fatalf("row exceeds page")
				}
				binary.LittleEndian.PutUint16(p[24:], uint16(n))
				binary.LittleEndian.PutUint16(p[30:], uint16(off-40))
				left = left[n:]
				if len(left) > 0 {
					binary.LittleEndian.PutUint32(p[12:], uint32(num+1))
				}
				out = append(out, p...)
			}
			h := out[28+16*ti:]
			binary.LittleEndian.PutUint32(h, typ)
			binary.LittleEndian.PutUint32(h[8:], uint32(first))
			binary.LittleEndian.PutUint32(h[12:], uint32(len(out)/pageSize-1))
		}
		binary.LittleEndian.PutUint32(out[12:], uint32(len(out)/pageSize))
		testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", out)
	}
	writePlaylists(10)
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{OnDemand: false}); err != nil {
		t.Fatalf("initial mount: %v", err)
	}

	// Re-export with 509 playlists (mirroring UNTITLED 185 -> 509 lists).
	writePlaylists(509)

	// Give the window spare + 2.5s (enough to stat & parse the 4-track export, NOT enough for 509 lists
	// whose estimated cost with commit margin is ~4.8s). Must defer cleanly and trigger StartBackground.
	bgStarted := 0
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{
		OnDemand:        true,
		Deadline:        time.Now().Add(spare + 2500*time.Millisecond),
		StartBackground: func() error { bgStarted++; return nil },
	})
	if err != nil {
		t.Fatalf("ConvertAtMount timed-out window failed: %v", err)
	}
	if !res.Background || bgStarted != 1 {
		t.Fatalf("expected deferred OnDemand window to trigger StartBackground once, got res=%+v bgStarted=%d", res, bgStarted)
	}
	// Live m.db must remain usable with the old 10 playlists intact.
	if n := query(t, mount, `SELECT count(*) FROM Playlist`)[0][0]; n != "10" {
		t.Fatalf("live m.db Playlist count = %s, want 10 usable old lists", n)
	}

	// Background job builds .rb-next.db + .swap + .browse.swap and asks for remount (Publish=true).
	fillRes, err := FillInBackground(context.Background(), mount, FillOptions{OnDemand: true})
	if err != nil || !fillRes.Publish {
		t.Fatalf("FillInBackground(OnDemand) = %+v, err=%v; want Publish=true", fillRes, err)
	}
	dir := dbDir(mount)
	for _, f := range []string{nextDB, rbstate.SwapName, rbstate.BrowseSwapName} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("expected pending next artifact %s: %v", f, err)
		}
	}

	// Remount window swaps and publishes all 509 playlists cleanly.
	bgAfter := 0
	pubRes, err := ConvertAtMount(context.Background(), mount, MountOptions{
		OnDemand:        true,
		Publish:         true,
		Deadline:        time.Now().Add(hookWindow),
		StartBackground: func() error { bgAfter++; return nil },
	})
	if err != nil {
		t.Fatalf("publish ConvertAtMount: %v", err)
	}
	if !pubRes.Pending.None() || bgAfter != 0 {
		t.Fatalf("published remount result = %+v, bgAfter=%d; want Pending.None() and no BG restart", pubRes, bgAfter)
	}
	if n := query(t, mount, `SELECT count(*) FROM Playlist`)[0][0]; n != "509" {
		t.Fatalf("published Playlist count = %s, want 509", n)
	}
	if snap := snapshot(t, mount); snap.Mode != rbstate.ModeOnDemand || !rbstate.BrowseIndexMatches(dir, snap.Header) {
		t.Fatalf("published snapshot mode=%q or browse index mismatch", snap.Mode)
	}
}
