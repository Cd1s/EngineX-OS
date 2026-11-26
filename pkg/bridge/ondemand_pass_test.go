package bridge

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	// Mark snapshot Pending=none so unchanged fast-path runs on upgrade to OnDemand.
	snap, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap.Pending = (rbstate.Header{}).Pending
	if err := snap.SaveNext(dir); err != nil {
		t.Fatal(err)
	}
	if err := rbstate.Promote(dir); err != nil {
		t.Fatal(err)
	}
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{OnDemand: true})
	if err != nil || !res.Bypassed {
		t.Fatalf("OnDemand upgrade bypass = %+v, err=%v", res, err)
	}
	if _, err := os.Stat(browsePath); err != nil {
		t.Fatalf("OnDemand unchanged bypass did not materialize missing %s: %v", rbstate.BrowseIndexName, err)
	}

	// readFilledStage rejects standalone filled.jsonl entries with mismatched rule version or row hash.
	var tr *rbstate.Track
	for _, c := range snap.Tracks {
		tr = c
		break
	}
	tr.Row = "expected-row-hash"
	sdir := filepath.Join(stageRoot, snap.LibraryUUID)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	wr := base64.StdEncoding.EncodeToString(make([]byte, 16))
	badVer, _ := json.Marshal(map[string]any{"id": tr.EngineID, "k": tr.Key, "wr": wr, "v": rbstate.Rules + 99})
	badRow, _ := json.Marshal(map[string]any{"id": tr.EngineID, "k": tr.Key, "wr": wr, "v": rbstate.Rules, "w": "stale-row"})
	if err := os.WriteFile(filepath.Join(sdir, "filled.jsonl"), append(append(badVer, '\n'), append(badRow, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFilledStage(snap); len(got) != 0 {
		t.Fatalf("readFilledStage accepted stale rule version / row hash: %+v", got)
	}
}
