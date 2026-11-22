package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #328 stage 1A: a first insert of a large export is listed at the mount from the export alone
// ("fast:" rows, no analysis) and its background update publishes the analysis minutes later.
// What the DJ did on a track in between (hot cues, saved loops, a grid edited or locked, the
// rating) must survive that publish, whether the stick was pulled and inserted again before the
// update copied the library (the edit is in its copy) or stayed mounted (the edit is in the live
// library only, and the publish merges it). What Engine wrote by itself when it analysed a track
// at a load (its own grid, empty cue and loop slots, its default main cue) must not: rekordbox's
// analysis replaces it, exactly as today.

const (
	issue328Tracks = 40
	issue328Rate   = 44100.0
	// Engine's own correction of a marked row it loads with an MP3 decoder (one_offset_rule_test).
	issue328EngineShift = 2292
)

// issue328OneLibrary is a OneLibrary export of the same 40 MP3 tracks as LargeExport, each with
// rekordbox's hot cue A, and B with cues 2, which the fast publish already lists (FastCues).
func issue328OneLibrary(t *testing.T, root string, cues int) {
	t.Helper()
	testfixture.LargeExport(t, root, issue328Tracks)
	if err := os.Remove(filepath.Join(root, "PIONEER/rekordbox/export.pdb")); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT, bpmx100 INTEGER, length INTEGER, samplingRate INTEGER, analysisDataFilePath TEXT, rating INTEGER);
		CREATE TABLE playlist(playlist_id INTEGER PRIMARY KEY, playlist_id_parent INTEGER, sequenceNo INTEGER, name TEXT, attribute INTEGER);
		CREATE TABLE playlist_content(playlist_id INTEGER, content_id INTEGER, sequenceNo INTEGER);
		CREATE TABLE cue(content_id INTEGER, kind INTEGER, inUsec INTEGER, outUsec INTEGER, cueComment TEXT, colorTableIndex INTEGER);
		INSERT INTO playlist VALUES(1, NULL, 1, 'Fast 328', 0);`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= issue328Tracks; i++ {
		if _, err = db.Exec(`INSERT INTO content VALUES(?,?,?,12000,4,44100,'/PIONEER/USBANLZ/P001/ANLZ0000.DAT',0)`, i, fmt.Sprintf("Track %02d", i), "/"+testfixture.LargeTrackPath(i)); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO playlist_content VALUES(1,?,?)`, i, i); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO cue VALUES(?,1,750000,-1,'A rekordbox',1)`, i); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO cue SELECT ?,2,1250000,-1,'B rekordbox',2 WHERE ?>1`, i, cues); err != nil {
			t.Fatal(err)
		}
	}
}

// issue328Slot is a hot cue slot of quickCues, or a saved loop slot of loops.
type issue328Slot struct {
	Label      string
	Start, End float64 // -1: not set
	Color      [4]byte
	Set        [2]byte // loops: isStartSet, isEndSet
}

// issue328Cues parses quickCues: the eight hot cue slots and the 17-byte main cue trailer.
func issue328Cues(t *testing.T, blob []byte) ([]issue328Slot, []byte) {
	t.Helper()
	raw, err := enginedb.QUncompress(blob, 1<<20)
	if err != nil || len(raw) < 8 || binary.BigEndian.Uint64(raw) != 8 {
		t.Fatalf("quickCues %x: %v", blob, err)
	}
	var out []issue328Slot
	off := 8
	for i := 0; i < 8; i++ {
		n := int(raw[off])
		s := issue328Slot{Label: string(raw[off+1 : off+1+n]), End: -1}
		off += 1 + n
		s.Start = math.Float64frombits(binary.BigEndian.Uint64(raw[off:]))
		copy(s.Color[:], raw[off+8:off+12])
		off += 12
		out = append(out, s)
	}
	return out, append([]byte(nil), raw[off:]...)
}

func issue328EncodeCues(t *testing.T, slots []issue328Slot, trailer []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint64(8))
	for _, s := range slots {
		b.WriteByte(byte(len(s.Label)))
		b.WriteString(s.Label)
		_ = binary.Write(&b, binary.BigEndian, s.Start)
		b.Write(s.Color[:])
	}
	b.Write(trailer)
	out, err := enginedb.QCompress(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// issue328Loops parses a loops blob.
func issue328Loops(t *testing.T, blob []byte) []issue328Slot {
	t.Helper()
	if len(blob) < 8 {
		t.Fatalf("loops %x", blob)
	}
	var out []issue328Slot
	off := 8
	for i := 0; i < int(blob[0]); i++ {
		n := int(blob[off])
		s := issue328Slot{Label: string(blob[off+1 : off+1+n])}
		off += 1 + n
		s.Start = math.Float64frombits(binary.LittleEndian.Uint64(blob[off:]))
		s.End = math.Float64frombits(binary.LittleEndian.Uint64(blob[off+8:]))
		copy(s.Set[:], blob[off+16:off+18])
		copy(s.Color[:], blob[off+18:off+22])
		off += 22
		out = append(out, s)
	}
	return out
}

func issue328EncodeLoops(slots []issue328Slot) []byte {
	var b bytes.Buffer
	b.WriteByte(byte(len(slots)))
	b.Write(make([]byte, 7))
	for _, s := range slots {
		b.WriteByte(byte(len(s.Label)))
		b.WriteString(s.Label)
		_ = binary.Write(&b, binary.LittleEndian, s.Start)
		_ = binary.Write(&b, binary.LittleEndian, s.End)
		b.Write(s.Set[:])
		b.Write(s.Color[:])
	}
	return b.Bytes()
}

// issue328Row is a track's row as the deck reads it.
type issue328Row struct {
	TrackData, Overview, Beats, Cues, Loops []byte
	Active, Mark                            sql.NullInt64
	Rating, Locked                          int64
	BPM                                     sql.NullFloat64
	BPMRounded                              sql.NullInt64
}

func issue328Read(t *testing.T, db *sql.DB, key int) issue328Row {
	t.Helper()
	var r issue328Row
	if err := db.QueryRow(`SELECT p.trackData,p.overviewWaveFormData,p.beatData,p.quickCues,p.loops,p.activeOnLoadLoops,p.thirdPartySourceId,
		COALESCE(t.rating,0),COALESCE(t.isBeatGridLocked,0),t.bpmAnalyzed,t.bpm
		FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, key).Scan(
		&r.TrackData, &r.Overview, &r.Beats, &r.Cues, &r.Loops, &r.Active, &r.Mark, &r.Rating, &r.Locked, &r.BPM, &r.BPMRounded); err != nil {
		t.Fatalf("track %d: %v", key, err)
	}
	return r
}

// issue328Analysed writes what Engine writes when it analyses track key at a deck load: its own
// grid (127 BPM, the default grid and the adjusted one the same), an overview and track data, the
// row's hot cues as Engine holds them (a marked OneLibrary row's moved by Engine's MP3
// correction), empty saved loop slots; the import mark goes and the Track row is analysed.
// TestIssue328EngineOwnRowsOfARealEngine checks the same rule with rows a real Engine wrote.
func issue328Analysed(t *testing.T, db *sql.DB, key int) {
	t.Helper()
	ov := make([]byte, 400)
	for i := range ov {
		ov[i] = byte(i % 31)
	}
	a := &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{TimeMS: 100, BPM: 127, Number: 1}, {TimeMS: 572, BPM: 127, Number: 2}, {TimeMS: 1044, BPM: 127, Number: 3}, {TimeMS: 1516, BPM: 127, Number: 4}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWAV", EntryBytes: 1, Data: ov}},
	}
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, a)
	if err != nil {
		t.Fatal(err)
	}
	cues := p.QuickCues
	var old []byte
	var mark sql.NullInt64
	if err = db.QueryRow(`SELECT p.quickCues,p.thirdPartySourceId FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, key).Scan(&old, &mark); err != nil {
		t.Fatal(err)
	}
	if old != nil { // the fast publish's cues, as Engine loaded them
		slots, trailer := issue328Cues(t, old)
		for i := range slots {
			if slots[i].Start >= 0 && mark.Valid {
				slots[i].Start -= issue328EngineShift
			}
		}
		cues = issue328EncodeCues(t, slots, trailer)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?,thirdPartySourceId=NULL
		WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, p.TrackData, p.Overview, p.Beats, cues, p.Loops, key); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE Track SET isAnalyzed=1,bpmAnalyzed=127 WHERE pdbImportKey=?`, key); err != nil {
		t.Fatal(err)
	}
}

// issue328EditA changes hot cue A of track key's row, the one the fast import listed from a
// OneLibrary export.
func issue328EditA(t *testing.T, db *sql.DB, key int, f func(s *issue328Slot)) {
	t.Helper()
	var blob []byte
	if err := db.QueryRow(`SELECT p.quickCues FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, key).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	slots, trailer := issue328Cues(t, blob)
	if slots[0].Start < 0 {
		t.Fatalf("track %d has no hot cue A on the deck", key)
	}
	f(&slots[0])
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, issue328EncodeCues(t, slots, trailer), key); err != nil {
		t.Fatal(err)
	}
}

// The DJ's edits, each on its own track, after Engine analysed it at the load. oneLib: on
// rekordbox's hot cue A, which only a OneLibrary export's fast rows have.
var issue328Edits = []struct {
	key    int
	name   string
	oneLib bool
	edit   func(t *testing.T, db *sql.DB, key int)
}{
	{2, "hot cue", false, func(t *testing.T, db *sql.DB, key int) {
		var blob []byte
		if err := db.QueryRow(`SELECT p.quickCues FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, key).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		slots, trailer := issue328Cues(t, blob)
		slots[2] = issue328Slot{Label: "Deck 328", Start: 2 * issue328Rate, End: -1, Color: [4]byte{255, 0x30, 0x5a, 0xff}}
		if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, issue328EncodeCues(t, slots, trailer), key); err != nil {
			t.Fatal(err)
		}
	}},
	{3, "saved loop", false, func(t *testing.T, db *sql.DB, key int) {
		var blob []byte
		if err := db.QueryRow(`SELECT p.loops FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, key).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		slots := issue328Loops(t, blob)
		slots[1] = issue328Slot{Label: "Deck Loop", Start: 2.5 * issue328Rate, End: 3 * issue328Rate, Set: [2]byte{1, 1}, Color: [4]byte{255, 0, 0xe0, 0xff}}
		if _, err := db.Exec(`UPDATE PerformanceData SET loops=?,activeOnLoadLoops=2 WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, issue328EncodeLoops(slots), key); err != nil {
			t.Fatal(err)
		}
	}},
	{4, "grid edit", false, func(t *testing.T, db *sql.DB, key int) {
		var blob []byte
		if err := db.QueryRow(`SELECT p.beatData FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, key).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		raw, err := enginedb.QUncompress(blob, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		// The adjusted grid (the second one) moved 10 ms later: a DJ's grid edit.
		n := int(binary.BigEndian.Uint64(raw[17:]))
		adj := 17 + 8 + 24*n + 8
		for i := 0; i < int(binary.BigEndian.Uint64(raw[adj-8:])); i++ {
			p := raw[adj+24*i:]
			binary.LittleEndian.PutUint64(p, math.Float64bits(math.Float64frombits(binary.LittleEndian.Uint64(p))+441))
		}
		if blob, err = enginedb.QCompress(raw); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`UPDATE PerformanceData SET beatData=? WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, blob, key); err != nil {
			t.Fatal(err)
		}
	}},
	{5, "grid lock", false, func(t *testing.T, db *sql.DB, key int) {
		if _, err := db.Exec(`UPDATE Track SET isBeatGridLocked=1 WHERE pdbImportKey=?`, key); err != nil {
			t.Fatal(err)
		}
	}},
	{6, "rating", false, func(t *testing.T, db *sql.DB, key int) {
		if _, err := db.Exec(`UPDATE Track SET rating=80 WHERE pdbImportKey=?`, key); err != nil {
			t.Fatal(err)
		}
	}},
	{7, "hot cue A moved", true, func(t *testing.T, db *sql.DB, key int) {
		issue328EditA(t, db, key, func(s *issue328Slot) { s.Start = 1.5 * issue328Rate })
	}},
	{8, "hot cue A deleted", true, func(t *testing.T, db *sql.DB, key int) {
		issue328EditA(t, db, key, func(s *issue328Slot) { *s = issue328Slot{Start: -1, End: -1} })
	}},
	{9, "hot cue A renamed", true, func(t *testing.T, db *sql.DB, key int) {
		issue328EditA(t, db, key, func(s *issue328Slot) { s.Label = "Deck 328" })
	}},
	{10, "hot cue A recoloured", true, func(t *testing.T, db *sql.DB, key int) {
		issue328EditA(t, db, key, func(s *issue328Slot) { s.Color = [4]byte{255, 0x30, 0xff, 0x5a} })
	}},
	// The limit of the rule (fastedits.go): with every cue the fast import listed gone, nothing in
	// the row says it listed any; rekordbox's come back, as before #328.
	{11, "every listed hot cue deleted", true, func(t *testing.T, db *sql.DB, key int) {
		var blob []byte
		if err := db.QueryRow(`SELECT p.quickCues FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, key).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		slots, trailer := issue328Cues(t, blob)
		slots[0], slots[1] = issue328Slot{Start: -1, End: -1}, issue328Slot{Start: -1, End: -1}
		if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, issue328EncodeCues(t, slots, trailer), key); err != nil {
			t.Fatal(err)
		}
	}},
}

// issue328Run takes a stick through its first insert: the fast publish at the mount, Engine
// analysing tracks 1 to 11 at their loads and the DJ's edits on 2 to 11 (deck=false: none of
// this), the background update and the mount that publishes it. pulled: the edits are made
// before the update copies the library (the stick was pulled and inserted again meanwhile);
// else after, while it stayed mounted. It returns the published library and, per track, the
// row as the deck had it before the publish.
func issue328Run(t *testing.T, oneLib, pulled, deck bool) (*sql.DB, map[int]issue328Row) {
	t.Helper()
	var play func(t *testing.T, db *sql.DB)
	if deck {
		play = func(t *testing.T, db *sql.DB) {
			for key := 1; key <= 11; key++ {
				issue328Analysed(t, db, key)
			}
			for _, e := range issue328Edits {
				if !e.oneLib || oneLib {
					e.edit(t, db, e.key)
				}
			}
		}
	}
	cues := 0
	if oneLib {
		cues = 2
	}
	return issue328RunWith(t, cues, pulled, play)
}

// issue328RunWith is issue328Run with the export's cues (0: PDB, else OneLibrary with that many
// hot cues) and what the deck does (nil: nothing).
func issue328RunWith(t *testing.T, cues int, pulled bool, deck func(t *testing.T, db *sql.DB)) (*sql.DB, map[int]issue328Row) {
	t.Helper()
	root := t.TempDir()
	if cues > 0 {
		issue328OneLibrary(t, root, cues)
	} else {
		testfixture.LargeExport(t, root, issue328Tracks)
	}
	defer bridge.SetUpdateExportForTest()()
	ctx := context.Background()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || r.Added != issue328Tracks || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	before := map[int]issue328Row{}
	play := func() {
		if deck == nil {
			return
		}
		db := dbOpen(t, root)
		defer db.Close()
		if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeState WHERE fingerprint LIKE 'fast:%'`); n != issue328Tracks {
			t.Fatalf("%d of %d tracks published fast", n, issue328Tracks)
		}
		deck(t, db)
		for key := 1; key <= 12; key++ {
			before[key] = issue328Read(t, db, key)
		}
	}
	if pulled {
		play()
		// Pulled and inserted again: the mount leaves the update to the background.
		if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
			t.Fatalf("mount after the pull: %+v %v", r, err)
		}
	}
	if r, err = bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("background update: %+v %v", r, err)
	}
	if !pulled {
		play()
	}
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("publishing mount: %+v %v", r, err)
	}
	db := dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeState WHERE fingerprint LIKE 'fast:%'`); n != 0 {
		t.Fatalf("%d tracks still fast after the publish", n)
	}
	return db, before
}

func TestIssue328DeckEditsOnFastRowsSurviveTheFirstFullImport(t *testing.T) {
	for _, src := range []struct {
		name   string
		oneLib bool
	}{{"PDB", false}, {"OneLibrary", true}} {
		for _, path := range []struct {
			name   string
			pulled bool
		}{{"pulled and inserted again", true}, {"stayed mounted", false}} {
			t.Run(src.name+"/"+path.name, func(t *testing.T) {
				db, before := issue328Run(t, src.oneLib, path.pulled, true)
				ref, _ := issue328Run(t, src.oneLib, path.pulled, false) // no deck: rekordbox's rows
				// Untouched: track 1, analysed by Engine at its load, is rekordbox's to the byte.
				got, want := issue328Read(t, db, 1), issue328Read(t, ref, 1)
				if !bytes.Equal(got.TrackData, want.TrackData) || !bytes.Equal(got.Overview, want.Overview) || !bytes.Equal(got.Beats, want.Beats) ||
					!bytes.Equal(got.Cues, want.Cues) || !bytes.Equal(got.Loops, want.Loops) || got.Active != want.Active || got.Mark != want.Mark ||
					got.Rating != want.Rating || got.BPM != want.BPM || got.BPMRounded != want.BPMRounded || got.Locked != 0 {
					t.Errorf("track 1, analysed by Engine and not touched: not rekordbox's after the publish")
				}
				// Hot cue: the DJ's slot C, rekordbox's other slots.
				got, want = issue328Read(t, db, 2), issue328Read(t, ref, 2)
				gs, _ := issue328Cues(t, got.Cues)
				ws, _ := issue328Cues(t, want.Cues)
				if gs[2].Label != "Deck 328" || gs[2].Start != 2*issue328Rate {
					t.Errorf("hot cue: the DJ's slot C is %+v after the publish", gs[2])
				}
				for i := range ws {
					if i != 2 && gs[i] != ws[i] {
						t.Errorf("hot cue: slot %d is %+v, rekordbox's is %+v", i+1, gs[i], ws[i])
					}
				}
				// Saved loop: the DJ's slot 2 and its active-on-load bit, rekordbox's other slots.
				got, want = issue328Read(t, db, 3), issue328Read(t, ref, 3)
				gl, wl := issue328Loops(t, got.Loops), issue328Loops(t, want.Loops)
				if gl[1].Label != "Deck Loop" || gl[1].Start != 2.5*issue328Rate || got.Active.Int64&2 == 0 {
					t.Errorf("saved loop: the DJ's slot 2 is %+v, active %v after the publish", gl[1], got.Active)
				}
				for i := range wl {
					if i != 1 && gl[i] != wl[i] {
						t.Errorf("saved loop: slot %d is %+v, rekordbox's is %+v", i+1, gl[i], wl[i])
					}
				}
				// Grid edited, grid locked: the DJ's grid and its BPM.
				for _, key := range []int{4, 5} {
					got = issue328Read(t, db, key)
					if !bytes.Equal(got.Beats, before[key].Beats) || got.BPM != before[key].BPM {
						t.Errorf("grid of track %d (%s): rekordbox's after the publish (BPM %v, the DJ's %v)", key, map[int]string{4: "edited", 5: "locked"}[key], got.BPM, before[key].BPM)
					}
				}
				if got = issue328Read(t, db, 5); got.Locked != 1 {
					t.Errorf("grid lock: unlocked after the publish")
				}
				// Rating.
				if got = issue328Read(t, db, 6); got.Rating != 80 {
					t.Errorf("rating: %d after the publish, the DJ's was 80", got.Rating)
				}
				if !src.oneLib {
					return
				}
				// Hot cue A moved, deleted, renamed, recoloured: the DJ's change of slot A, on
				// rekordbox's position where the DJ did not move it; rekordbox's other slots.
				for _, e := range issue328Edits[5:] {
					gs, _ := issue328Cues(t, issue328Read(t, db, e.key).Cues)
					ws, _ := issue328Cues(t, issue328Read(t, ref, e.key).Cues)
					want := ws[0]
					switch e.key {
					case 7:
						want.Start = 1.5 * issue328Rate
					case 8:
						want = issue328Slot{Start: -1, End: -1}
					case 9:
						want.Label = "Deck 328"
					case 10:
						want.Color = [4]byte{255, 0x30, 0xff, 0x5a}
					case 11: // rekordbox's: the limit
					}
					if gs[0] != want {
						t.Errorf("track %d (%s): hot cue A is %+v, want %+v", e.key, e.name, gs[0], want)
					}
					for i := 1; i < 8; i++ {
						if gs[i] != ws[i] {
							t.Errorf("track %d (%s): slot %d is %+v, rekordbox's is %+v", e.key, e.name, i+1, gs[i], ws[i])
						}
					}
				}
			})
		}
	}
}

// issue328EngineRow is a fast row as a real Engine 5.3.0 left it (SC5000 rootfs in the build host's
// sandbox, 2026-11-22): loaded and analysed by itself, or edited at the deck through Engine's own
// controls. Source: the first insert of issue328's PDB export ("pdb"), of its OneLibrary export with
// hot cue A ("onelib") or with A and B ("onelib2"). Hex blobs.
type issue328EngineRow struct {
	Source, What                                string
	Key                                         int
	TrackData, BeatData, QuickCues, Loops       *string
	Active, Mark, Rating, Locked, BPM, Analyzed *int64
	BPMAnalyzed                                 *float64
}

func issue328EngineRows(t *testing.T) []issue328EngineRow {
	t.Helper()
	b, err := os.ReadFile("testdata/issue328_engine_rows.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []issue328EngineRow
	if err = json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func issue328Hex(t *testing.T, s *string) []byte {
	t.Helper()
	if s == nil {
		return nil
	}
	b, err := hex.DecodeString(*s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIssue328EngineOwnRowsOfARealEngine: the rule told from rows a real Engine wrote. What Engine
// wrote by itself at a load (its grid, its empty slots and main cue, rekordbox's cues moved by its
// own correction) is rekordbox's after the first full import, byte for byte; what the DJ did with
// Engine's controls stays.
func TestIssue328EngineOwnRowsOfARealEngine(t *testing.T) {
	all := issue328EngineRows(t)
	for _, src := range []struct {
		name string
		cues int
	}{{"pdb", 0}, {"onelib", 1}, {"onelib2", 2}} {
		var rows []issue328EngineRow
		for _, r := range all {
			if r.Source == src.name {
				rows = append(rows, r)
			}
		}
		for _, pulled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/pulled=%v", src.name, pulled), func(t *testing.T) {
				db, _ := issue328RunWith(t, src.cues, pulled, func(t *testing.T, db *sql.DB) {
					for _, r := range rows {
						if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=NULL,beatData=?,quickCues=?,loops=?,activeOnLoadLoops=?,thirdPartySourceId=?
							WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=?)`, issue328Hex(t, r.TrackData), issue328Hex(t, r.BeatData), issue328Hex(t, r.QuickCues),
							issue328Hex(t, r.Loops), r.Active, r.Mark, r.Key); err != nil {
							t.Fatal(err)
						}
						if _, err := db.Exec(`UPDATE Track SET rating=?,isBeatGridLocked=?,bpm=?,bpmAnalyzed=?,isAnalyzed=? WHERE pdbImportKey=?`,
							r.Rating, r.Locked, r.BPM, r.BPMAnalyzed, r.Analyzed, r.Key); err != nil {
							t.Fatal(err)
						}
					}
				})
				ref, _ := issue328RunWith(t, src.cues, pulled, nil)
				for _, r := range rows {
					got, want := issue328Read(t, db, r.Key), issue328Read(t, ref, r.Key)
					gs, gt := issue328Cues(t, got.Cues)
					ws, wt := issue328Cues(t, want.Cues)
					var es []issue328Slot
					var et []byte
					if r.QuickCues != nil {
						es, et = issue328Cues(t, issue328Hex(t, r.QuickCues))
					}
					beats, cues, rating := want.Beats, append([]issue328Slot(nil), ws...), want.Rating
					trailer, bpm, locked := wt, want.BPM, int64(0)
					switch r.What {
					case "own", "loaded twice", "every listed hot cue deleted":
					case "hot cue set":
						cues[2] = es[2]
					case "hot cue moved", "hot cue deleted":
						cues[0] = es[0]
					case "hot cue renamed":
						cues[0].Label = es[0].Label
					case "hot cue recoloured":
						cues[0].Color = es[0].Color
					case "main cue set":
						trailer = append(append([]byte(nil), et[:9]...), wt[9:]...)
					case "grid edited":
						beats, bpm = issue328Hex(t, r.BeatData), sql.NullFloat64{Float64: *r.BPMAnalyzed, Valid: true}
					case "grid locked":
						beats, locked = issue328Hex(t, r.BeatData), 1
					case "rated":
						rating = *r.Rating
					default:
						t.Fatalf("track %d: unknown edit %q", r.Key, r.What)
					}
					for i := range cues {
						if gs[i] != cues[i] {
							t.Errorf("track %d (%s): slot %d is %+v, want %+v", r.Key, r.What, i+1, gs[i], cues[i])
						}
					}
					if !bytes.Equal(gt, trailer) {
						t.Errorf("track %d (%s): main cue %x, want %x", r.Key, r.What, gt, trailer)
					}
					if !bytes.Equal(got.Beats, beats) || got.BPM != bpm || got.Locked != locked || got.Rating != rating {
						t.Errorf("track %d (%s): grid, BPM %v, lock %d, rating %d; want BPM %v, lock %d, rating %d", r.Key, r.What, got.BPM, got.Locked, got.Rating, bpm, locked, rating)
					}
					if !bytes.Equal(got.TrackData, want.TrackData) || !bytes.Equal(got.Overview, want.Overview) || !bytes.Equal(got.Loops, want.Loops) || got.Active != want.Active || got.Mark != want.Mark {
						t.Errorf("track %d (%s): track data, overview or loops are not rekordbox's", r.Key, r.What)
					}
					if r.What == "own" || r.What == "loaded twice" || r.What == "every listed hot cue deleted" {
						if !bytes.Equal(got.Cues, want.Cues) || got.BPMRounded != want.BPMRounded {
							t.Errorf("track %d (%s): not rekordbox's byte for byte", r.Key, r.What)
						}
					}
				}
			})
		}
	}
}
