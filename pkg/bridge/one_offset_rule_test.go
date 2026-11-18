package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/audioheader"
	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// One offset rule: a rekordbox grid, cue or loop is placed on Engine's timeline by one
// per-file offset, the same for a Pro DJ Link fill and a USB conversion, and no row
// carries Engine's import mark (thirdPartySourceId). Engine 5.3.0 moves a marked row by
// its own constant when a decoder is attached at the load (an MP3 by 2292 samples) and
// clears the mark without moving it otherwise, so a marked row ends in one of two places.
// A row Engine has saved keeps what it was converted with; a later conversion has to
// recognise that shape, or the row never receives a corrected rule. The shapes below are
// rows of a real 6013-track stick read on 2026-11-18.

const (
	ruleBeatMS = 50  // first beat of makeOffsetANLZ's grid
	ruleHotBMS = 550 // its hot cue B
	ruleRate   = 44100
	ruleANLZ   = "PIONEER/USBANLZ/P001/ANLZ0000.DAT"
	ruleMP3    = "MUSIC/info-lame.mp3"
)

// ruleStick is an export with one Info + LAME MP3 (encoder delay 576).
func ruleStick(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	exportPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	if err := os.MkdirAll(filepath.Dir(exportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	exp, err := sql.Open("sqlite", exportPath)
	if err != nil {
		t.Fatal(err)
	}
	copyMP3Fixture(t, root, ruleMP3, "01-lame-cbr-info-44100.mp3")
	testfixture.Write(t, root, ruleANLZ, makeOffsetANLZ(ruleBeatMS))
	if _, err = exp.Exec(`
		CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT, bpmx100 INTEGER, length INTEGER, samplingRate INTEGER, analysisDataFilePath TEXT);
		CREATE TABLE playlist(playlist_id INTEGER PRIMARY KEY, playlist_id_parent INTEGER, sequenceNo INTEGER, name TEXT, attribute INTEGER);
		CREATE TABLE playlist_content(playlist_id INTEGER, content_id INTEGER, sequenceNo INTEGER);
		INSERT INTO playlist VALUES(1, NULL, 1, 'OneRule', 0);
		INSERT INTO content VALUES(1, 'info-lame', '/` + ruleMP3 + `', 12000, 8, 44100, '/` + ruleANLZ + `');
		INSERT INTO playlist_content VALUES(1, 1, 1);
	`); err != nil {
		t.Fatal(err)
	}
	if err = exp.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

// ruleOffset is what the Pro DJ Link fill subtracts for the same file.
func ruleOffset(t *testing.T, root string) float64 {
	t.Helper()
	info, err := audioheader.OpenMP3(filepath.Join(root, ruleMP3))
	if err != nil {
		t.Fatal(err)
	}
	return float64(info.RekordboxOffsetSamples)
}

func ruleConvert(t *testing.T, root string, opts enginedb.Options) enginedb.Result {
	t.Helper()
	res, err := bridge.ConvertAtMount(context.Background(), root, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ruleOlderBuild makes the next mount see a library an older build converted.
func ruleOlderBuild(t *testing.T, root, version string) {
	t.Helper()
	stamp := filepath.Join(root, "Engine Library/Database2/.rb-bridge-stamp")
	b, err := os.ReadFile(stamp)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := bytes.Cut(b, []byte(" "))
	if !ok {
		t.Fatalf("stamp %q has no version", b)
	}
	if err = os.WriteFile(stamp, append([]byte(version+" "), rest...), 0o644); err != nil {
		t.Fatal(err)
	}
	// An older build's record of each track is not this build's (another encoding number).
	state, err := sql.Open("sqlite", filepath.Join(root, "Engine Library/Database2/.rb-bridge-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if _, err = state.Exec(`UPDATE BridgeState SET fingerprint='an older build'; DELETE FROM BridgeWritten`); err != nil {
		t.Fatal(err)
	}
}

type ruleRow struct {
	grid, adjusted, hotB float64
	marked               bool
}

func ruleBlobs(t *testing.T, db *sql.DB) (id int64, beats, cues []byte) {
	t.Helper()
	var beatBlob, cueBlob []byte
	if err := db.QueryRow(`SELECT t.id, p.beatData, p.quickCues FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=1`).Scan(&id, &beatBlob, &cueBlob); err != nil {
		t.Fatal(err)
	}
	beats, err := enginedb.QUncompress(beatBlob, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if cues, err = enginedb.QUncompress(cueBlob, 1<<16); err != nil {
		t.Fatal(err)
	}
	return id, beats, cues
}

// ruleHotB is where hot cue B's position starts in a quick-cue blob.
func ruleHotB(cues []byte) int {
	return 8 + 1 + int(cues[8]) + 12 + 1 + int(cues[8+1+int(cues[8])+12])
}

// ruleAdjusted is where the adjusted grid's first position starts in a beat blob.
func ruleAdjusted(beats []byte) int {
	return 25 + 24*int(binary.BigEndian.Uint64(beats[17:])) + 8
}

func readRuleRow(t *testing.T, root string) ruleRow {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	_, beats, cues := ruleBlobs(t, db)
	f := func(b []byte, order binary.ByteOrder) float64 { return math.Float64frombits(order.Uint64(b)) }
	return ruleRow{
		grid:     f(beats[25:], binary.LittleEndian),
		adjusted: f(beats[ruleAdjusted(beats):], binary.LittleEndian),
		hotB:     f(cues[ruleHotB(cues):], binary.BigEndian),
		marked:   scalar[int](t, db, `SELECT count(*) FROM PerformanceData WHERE thirdPartySourceId IS NOT NULL`) != 0,
	}
}

// rowAsConverted puts the row back to what a conversion with the given offset wrote:
// grid, cues and loops, and the import mark. loaded: Engine has loaded and saved it since
// (the mark is cleared); edit then changes the blobs the way a DJ at the deck would.
func rowAsConverted(t *testing.T, root string, offset int, loaded bool, edit func(beats, cues []byte)) {
	t.Helper()
	anlz, err := rekordbox.ReadAnalysis(filepath.Join(root, ruleANLZ))
	if err != nil {
		t.Fatal(err)
	}
	old, err := enginedb.EncodePerformance(rekordbox.Track{SampleRate: ruleRate, Duration: 8, StartOffsetSamples: offset}, anlz)
	if err != nil {
		t.Fatal(err)
	}
	db := dbOpen(t, root)
	defer db.Close()
	id, _, _ := ruleBlobs(t, db)
	beats, err := enginedb.QUncompress(old.Beats, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	cues, err := enginedb.QUncompress(old.QuickCues, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(beats, cues)
	}
	beatBlob, err := enginedb.QCompress(beats)
	if err != nil {
		t.Fatal(err)
	}
	cueBlob, err := enginedb.QCompress(cues)
	if err != nil {
		t.Fatal(err)
	}
	var mark any = 1
	if loaded {
		mark = nil
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET beatData=?, quickCues=?, loops=?, thirdPartySourceId=? WHERE trackId=?`, beatBlob, cueBlob, old.Loops, mark, id); err != nil {
		t.Fatal(err)
	}
}

func lockGrid(t *testing.T, root string) {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	if _, err := db.Exec(`UPDATE Track SET isBeatGridLocked=1`); err != nil {
		t.Fatal(err)
	}
}

func TestOneOffsetRuleUSBRowMatchesLink(t *testing.T) {
	root := ruleStick(t)
	ruleConvert(t, root, enginedb.Options{})
	off := ruleOffset(t, root)
	got := readRuleRow(t, root)
	wantGrid, wantHotB := ruleBeatMS*ruleRate/1000-off, ruleHotBMS*ruleRate/1000-off
	if got.grid != wantGrid || got.adjusted != wantGrid || got.hotB != wantHotB {
		t.Fatalf("USB row grid %v / %v, hot cue B %v; the Link fill puts the same file at grid %v, hot cue B %v (offset %v)", got.grid, got.adjusted, got.hotB, wantGrid, wantHotB, off)
	}
}

// A library an older build completed with the frame-plus-delay offset, its rows never
// loaded: the next mount reads it again once.
func TestOneOffsetRuleCompletedLibraryIsReadAgainOnce(t *testing.T) {
	root := ruleStick(t)
	ruleConvert(t, root, enginedb.Options{})
	off := ruleOffset(t, root)
	rowAsConverted(t, root, 2257, false, nil)
	ruleOlderBuild(t, root, "v3")
	if res := ruleConvert(t, root, enginedb.Options{}); res.Bypassed {
		t.Fatalf("the mount after the older build was bypassed: %+v", res)
	}
	if got, want := readRuleRow(t, root), ruleBeatMS*ruleRate/1000-off; got.grid != want || got.adjusted != want {
		t.Fatalf("never-loaded row of the older build: grid %v / %v, want %v", got.grid, got.adjusted, want)
	}
	if res := ruleConvert(t, root, enginedb.Options{}); !res.Bypassed {
		t.Fatalf("the unchanged export was read a second time: %+v", res)
	}
}

// The mark on what rb-bridge writes. A deck with the hook can clear it before Engine reads
// the row; a stock player, Engine DJ on a computer, a linked deck reading the stick over
// the network and a read-only stick cannot.
func TestOneOffsetRuleConvertedRowHasNoMark(t *testing.T) {
	root := ruleStick(t)
	ruleConvert(t, root, enginedb.Options{})
	if readRuleRow(t, root).marked {
		t.Fatal("the converted row carries thirdPartySourceId: Engine adds its own correction to it when a decoder is attached at the load")
	}
}

// A fast import writes the export's own cues before any file is read: it does not know the
// file's offset, so the cues are at rekordbox's raw position and carry the import mark, the
// one case where Engine's own correction is the only one. The full import replaces both.
func TestOneOffsetRuleFastImportCuesAreRawAndMarked(t *testing.T) {
	root := ruleStick(t)
	l, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	l.Tracks[0].Cues = []rekordbox.Cue{{HotCue: 2, TimeMS: ruleHotBMS, Name: "B"}}
	if _, err = enginedb.Sync(context.Background(), root, l, enginedb.Options{Fast: true, FastCues: true}); err != nil {
		t.Fatal(err)
	}
	db := dbOpen(t, root)
	var blob []byte
	var mark sql.NullInt64
	if err = db.QueryRow(`SELECT quickCues, thirdPartySourceId FROM PerformanceData WHERE quickCues IS NOT NULL`).Scan(&blob, &mark); err != nil {
		t.Fatal(err)
	}
	db.Close()
	cues, err := enginedb.QUncompress(blob, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	if got := math.Float64frombits(binary.BigEndian.Uint64(cues[ruleHotB(cues):])); mark.Int64 != 1 || got != ruleHotBMS*ruleRate/1000 {
		t.Fatalf("fast import: hot cue B at %v with mark %v, want the raw position %v with the mark", got, mark, ruleHotBMS*ruleRate/1000)
	}
	ruleConvert(t, root, enginedb.Options{})
	if got, off := readRuleRow(t, root), ruleOffset(t, root); got.marked || got.hotB != ruleHotBMS*ruleRate/1000-off {
		t.Fatalf("full import after it: %+v, want hot cue B at %v without the mark", got, ruleHotBMS*ruleRate/1000-off)
	}
}

// A row Engine saved under an older rule, never edited at the deck: raw (converted before
// the MP3 offset existed, loaded without Engine's own correction), raw - 2292 (the same,
// loaded with it) and raw - 2257 (converted with the frame-plus-delay offset).
func TestOneOffsetRuleFrozenRowsAreConvertedAgainOnce(t *testing.T) {
	for _, old := range []int{0, 2292, 2257} {
		root := ruleStick(t)
		ruleConvert(t, root, enginedb.Options{})
		off := ruleOffset(t, root)
		wantGrid, wantHotB := ruleBeatMS*ruleRate/1000-off, ruleHotBMS*ruleRate/1000-off
		rowAsConverted(t, root, old, true, nil)
		ruleOlderBuild(t, root, "v3")
		ruleConvert(t, root, enginedb.Options{})
		if got := readRuleRow(t, root); got.grid != wantGrid || got.adjusted != wantGrid || got.hotB != wantHotB {
			t.Fatalf("row frozen at raw - %d: got %+v, want grid %v and hot cue B %v", old, got, wantGrid, wantHotB)
		}
		db := dbOpen(t, root)
		if got, want := readTrackPositions(t, db, 1).loop0Start, 1050*ruleRate/1000-off; got != want {
			t.Fatalf("row frozen at raw - %d: loop starts at %v, want %v", old, got, want)
		}
		db.Close()
		// Once: the DJ's later edit of the corrected row survives the mounts after it.
		rowAsConverted(t, root, int(off), true, func(beats, cues []byte) {
			binary.BigEndian.PutUint64(cues[ruleHotB(cues):], math.Float64bits(30000.5))
		})
		ruleConvert(t, root, enginedb.Options{})
		if got := readRuleRow(t, root); got.grid != wantGrid || got.hotB != 30000.5 {
			t.Fatalf("row frozen at raw - %d, after its correction and a deck edit: got %+v, want grid %v and hot cue B 30000.5", old, got, wantGrid)
		}
	}
}

func TestOneOffsetRuleDeckEditsStay(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		lock                       bool
		edit                       func(beats, cues []byte)
		wantGrid, wantAdj, wantHot func(off float64) float64 // wantHot -1: either place is defensible
	}{
		{name: "adjusted grid differs from the default grid",
			edit: func(beats, cues []byte) {
				binary.LittleEndian.PutUint64(beats[ruleAdjusted(beats):], math.Float64bits(ruleBeatMS*ruleRate/1000-2257+441))
			},
			wantGrid: func(float64) float64 { return ruleBeatMS*ruleRate/1000 - 2257 },
			wantAdj:  func(float64) float64 { return ruleBeatMS*ruleRate/1000 - 2257 + 441 },
			wantHot:  func(float64) float64 { return -1 }},
		{name: "grid fits no older rule",
			edit: func(beats, cues []byte) {
				for _, at := range []int{25, ruleAdjusted(beats)} {
					binary.LittleEndian.PutUint64(beats[at:], math.Float64bits(ruleBeatMS*ruleRate/1000-333.25))
				}
			},
			wantGrid: func(float64) float64 { return ruleBeatMS*ruleRate/1000 - 333.25 },
			wantAdj:  func(float64) float64 { return ruleBeatMS*ruleRate/1000 - 333.25 },
			wantHot:  func(float64) float64 { return -1 }},
		{name: "beat grid locked at the deck", lock: true,
			wantGrid: func(float64) float64 { return ruleBeatMS*ruleRate/1000 - 2257 },
			wantAdj:  func(float64) float64 { return ruleBeatMS*ruleRate/1000 - 2257 },
			wantHot:  func(float64) float64 { return -1 }},
		{name: "a cue the DJ moved, in a row whose frozen grid is corrected",
			edit: func(beats, cues []byte) {
				binary.BigEndian.PutUint64(cues[ruleHotB(cues):], math.Float64bits(30000.5))
			},
			wantGrid: func(off float64) float64 { return ruleBeatMS*ruleRate/1000 - off },
			wantAdj:  func(off float64) float64 { return ruleBeatMS*ruleRate/1000 - off },
			wantHot:  func(float64) float64 { return 30000.5 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := ruleStick(t)
			ruleConvert(t, root, enginedb.Options{})
			off := ruleOffset(t, root)
			rowAsConverted(t, root, 2257, true, tc.edit)
			if tc.lock {
				lockGrid(t, root)
			}
			ruleOlderBuild(t, root, "v3")
			ruleConvert(t, root, enginedb.Options{})
			got := readRuleRow(t, root)
			if got.grid != tc.wantGrid(off) || got.adjusted != tc.wantAdj(off) || (tc.wantHot(off) >= 0 && got.hotB != tc.wantHot(off)) {
				t.Fatalf("got %+v, want grid %v, adjusted grid %v, hot cue B %v", got, tc.wantGrid(off), tc.wantAdj(off), tc.wantHot(off))
			}
		})
	}
}

// The M4A of the 2026-11-18 report: converted before the AAC priming was read (grid at the
// raw rekordbox position), loaded on a deck, then mounted by a build that reads it.
func TestOneOffsetRuleFrozenAACRowGetsThePriming(t *testing.T) {
	root := t.TempDir()
	rel := testfixture.AACExport(t, root)
	audio := filepath.Join(root, rel)
	media := testfixture.AACWithPriming()
	// The old bridge did not use the AAC header; size and mtime stay as they were.
	testfixture.Write(t, root, rel, make([]byte, len(media)))
	st, err := os.Stat(audio)
	if err != nil {
		t.Fatal(err)
	}
	ruleConvert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	if got := readTrackPositions(t, db, 1); got.beat0 != 0 {
		t.Fatalf("fixture: first beat %v before the priming is known, want 0", got.beat0)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET thirdPartySourceId=NULL`); err != nil { // Engine loaded and saved it
		t.Fatal(err)
	}
	db.Close()
	ruleOlderBuild(t, root, "v2")
	testfixture.Write(t, root, rel, media)
	if err = os.Chtimes(audio, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	ruleConvert(t, root, enginedb.Options{})
	db = dbOpen(t, root)
	defer db.Close()
	if got := readTrackPositions(t, db, 1); got.beat0 != -2048 {
		t.Fatalf("first beat %v samples after the mount, want -2048 (2048 samples of priming)", got.beat0)
	}
}

// The maintainer's stick is large: its update is built after the mount on a snapshot and
// replaces the live library at a later mount (#301), carrying what the deck wrote meanwhile.
// It carried the whole live row, so a row Engine had loaded came back at the older import's
// position whatever the build had made of it.
func TestOneOffsetRuleStagedUpdateCorrectsARowEngineHasLoaded(t *testing.T) {
	root := ruleStick(t)
	ruleConvert(t, root, enginedb.Options{})
	off := ruleOffset(t, root)
	rowAsConverted(t, root, 2257, true, nil)
	ruleOlderBuild(t, root, "v3")
	defer bridge.SetLargeExportForTest()()
	ctx := context.Background()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("mount that starts the update: %+v %v", r, err)
	}
	if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("update: %+v %v", r, err)
	}
	// While the update ran, the DJ moved hot cue B on the deck.
	rowAsConverted(t, root, 2257, true, func(beats, cues []byte) {
		binary.BigEndian.PutUint64(cues[ruleHotB(cues):], math.Float64bits(30000.5))
	})
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("mount publishing the update: %+v %v", r, err)
	}
	want := ruleBeatMS*ruleRate/1000 - off
	if got := readRuleRow(t, root); got.grid != want || got.adjusted != want || got.hotB != 30000.5 || got.marked {
		t.Fatalf("published row: %+v, want grid %v, the deck's hot cue B at 30000.5 and no mark", got, want)
	}
	db := dbOpen(t, root)
	defer db.Close()
	if got, want := readTrackPositions(t, db, 1).loop0Start, 1050*ruleRate/1000-off; got != want {
		t.Fatalf("published row: loop starts at %v, want %v", got, want)
	}
}
